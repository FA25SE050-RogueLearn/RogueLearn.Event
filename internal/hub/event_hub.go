package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/executor"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/rabbitmq"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/client/user"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/events"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
	pb "github.com/FA25SE050-RogueLearn/RogueLearn.Event/protos"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultQueryTimeoutSecond = 10 * time.Second

	AchievementCodeBattleTop1        = "code_battle_top_1"
	AchievementCodeBattleTop2        = "code_battle_top_2"
	AchievementCodeBattleTop3        = "code_battle_top_3"
	AchievementCodeBattleParticipant = "code_battle_participant"
)

var (
	AchievementCodeBattle map[int]string = map[int]string{
		1: AchievementCodeBattleTop1,
		2: AchievementCodeBattleTop2,
		3: AchievementCodeBattleTop3,
	}

	ErrPlayerNotInGuild = errors.New("player not in guild")
)

// event-based
// each room will have a room manager, acting as a broadcaster for room-related events to all connected clients
// events is a single queue that received events from multiple sources and process it, then send to all listeners
// listeners are all the clients connected to the room, represented by their client IDs

// EventHub struct holds all the RoomHub (channel) of each room
type EventHub struct {
	logger        *slog.Logger
	queries       *store.Queries
	db            *pgxpool.Pool             // Database pool for transactions
	Rooms         map[uuid.UUID]*RoomHub    // roomID -> roomManager
	EventRooms    map[uuid.UUID][]uuid.UUID // eventID -> []roomID
	Mu            sync.RWMutex
	leaderboardMu sync.Mutex // Protects leaderboard calculation

	GuildUpdateChan  chan uuid.UUID
	EventListeners   map[uuid.UUID]map[uuid.UUID]chan<- events.SseEvent // eventID -> map[listenerID] channel
	EventListenersMu sync.RWMutex                                       // A dedicated mutex for the event

	rabbitClient   *rabbitmq.RabbitMQClient
	executorClient *executor.Client
	userClient     *user.Client
}

type RoomHub struct {
	RoomID        uuid.UUID
	EventID       uuid.UUID
	Events        chan any                             // Events channel is what happened in the room
	Listerners    map[uuid.UUID]chan<- events.SseEvent // Players connected to this RoomHub
	logger        *slog.Logger
	queries       *store.Queries
	db            *pgxpool.Pool // Database pool for transactions
	Mu            sync.RWMutex  // Protects Listerners map
	leaderboardMu sync.Mutex    // Protects leaderboard calculation

	guildUpdateChan chan<- uuid.UUID

	rabbitClient   *rabbitmq.RabbitMQClient
	executorClient *executor.Client
	userClient     *user.Client

	// Track last activity for cleanup purposes
	lastActivity time.Time
	activityMu   sync.RWMutex
}

func NewEventHub(db *pgxpool.Pool, queries *store.Queries, logger *slog.Logger, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client, userClient *user.Client) *EventHub {
	e := EventHub{
		logger:          logger,
		queries:         queries,
		db:              db,
		Rooms:           make(map[uuid.UUID]*RoomHub),
		EventRooms:      make(map[uuid.UUID][]uuid.UUID), // NEW!
		GuildUpdateChan: make(chan uuid.UUID, 100),       // Buffered channel
		EventListeners:  make(map[uuid.UUID]map[uuid.UUID]chan<- events.SseEvent),
		rabbitClient:    rabbitClient,
		executorClient:  executorClient,
		userClient:      userClient,
	}

	go e.listenForAMQPEvents() // Start the central RabbitMQ listener

	// Recover timers for active events on startup
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		e.recoverActiveEvents(ctx)
	}()

	// CRITICAL: Start background job to detect and complete expired events
	// This provides defense-in-depth against timer loss due to crashes/restarts
	// Runs every 60 seconds to catch events that should have expired but didn't
	go e.startExpiredEventMonitor()

	go e.Start()

	for _, r := range e.Rooms {
		go r.Start()
	}
	// --------- remove on production ---------

	return &e
}

// ScheduleEventExpiry schedules a one-time timer to complete the event when it expires.
// Uses time.AfterFunc to fire exactly at the event's end_date.
// When the timer fires, it calls completeEvent which:
// - Atomically updates the event status to 'completed' in the database
// - Publishes EventExpired to RabbitMQ so all instances receive it
//
// P1-3: Implements grace period for in-flight submissions
// - Timer fires at end_date
// - Waits GRACE_PERIOD (10 seconds) for in-flight submissions to complete
// - Then marks event as completed
func (e *EventHub) ScheduleEventExpiry(eventID uuid.UUID, endDate time.Time) {
	duration := time.Until(endDate)

	if duration <= 0 {
		e.logger.Warn("Event end date is in the past, not scheduling timer",
			"event_id", eventID,
			"end_date", endDate,
			"now", time.Now().UTC())
		return
	}

	e.logger.Info("Scheduling event expiry timer",
		"event_id", eventID,
		"end_date", endDate,
		"duration", duration)

	// Schedule one-time timer
	time.AfterFunc(duration, func() {
		e.logger.Info("Event expiry timer fired", "event_id", eventID)
		e.completeEventWithGracePeriod(eventID)
	})
}

// completeEventWithGracePeriod implements a grace period before completing the event.
// This gives in-flight submissions time to finish processing.
//
// P1-3: Grace period prevents unfair rejection of submissions that were:
// - Submitted before end_date
// - Still being judged when timer fired
// - Would otherwise be rejected despite being submitted on time
//
// Grace period: 10 seconds (configurable)
func (e *EventHub) completeEventWithGracePeriod(eventID uuid.UUID) {
	const gracePeriod = 10 * time.Second

	e.logger.Info("Event expired - starting grace period for in-flight submissions",
		"event_id", eventID,
		"grace_period", gracePeriod)

	// Wait for grace period to allow in-flight submissions to complete
	// During this time:
	// - New submissions are rejected (event.end_date has passed)
	// - In-flight submissions continue processing
	// - Judge responses are still accepted and scored
	time.Sleep(gracePeriod)

	e.logger.Info("Grace period complete - now completing event",
		"event_id", eventID,
		"grace_period", gracePeriod)

	// Now complete the event (with all P0 fixes)
	e.completeEvent(eventID)
}

// completeEvent atomically marks an event as completed and broadcasts the expiry message.
// This should only be called by the timer (ScheduleEventExpiry).
//
// Flow:
// 1. Atomically update event status: active → completed (only one instance succeeds)
// 2. Update all player states (present → completed)
// 3. Commit transaction (all-or-nothing)
// 4. Publish EventExpired to RabbitMQ with retry (fanout to all instances)
// 5. All instances receive the message and broadcast to their connected players
//
// Returns:
//   - true if event was successfully completed by this instance
//   - false if event was already completed or if completion failed
func (e *EventHub) completeEvent(eventID uuid.UUID) bool {
	// Use longer timeout for transaction + retries
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	e.logger.Info("Attempting to complete event", "event_id", eventID)

	// CRITICAL: All database operations in single transaction (atomicity)
	// If any step fails, entire operation rolls back - no partial state
	success := e.completeEventTransaction(ctx, eventID)
	if !success {
		return false
	}

	// Transaction committed successfully - now notify other instances
	// Use retry mechanism for RabbitMQ publish (P0-2)
	e.publishEventExpiredWithRetry(ctx, eventID)

	return true
}

// completeEventTransaction handles the database transaction for event completion.
// Returns true only if the transaction was successfully committed.
// This ensures atomicity: either ALL changes happen or NONE happen.
func (e *EventHub) completeEventTransaction(ctx context.Context, eventID uuid.UUID) bool {
	// Start a transaction to ensure atomicity
	tx, err := e.db.Begin(ctx)
	if err != nil {
		e.logger.Error("Failed to start transaction for event completion",
			"event_id", eventID,
			"error", err,
			"action", "will retry on next monitor cycle")
		return false
	}

	// CRITICAL: Always rollback on function exit
	// If commit succeeds, rollback does nothing
	// If commit fails or we return early, rollback prevents partial state
	defer func() {
		if err := tx.Rollback(ctx); err != nil {
			// Rollback only fails if transaction was already committed or connection lost
			// This is expected after successful commit, so only log at debug level
			e.logger.Debug("Transaction rollback after completion",
				"event_id", eventID,
				"error", err)
		}
	}()

	qtx := e.queries.WithTx(tx)

	// Step 1: Atomic update event status (active → completed)
	// Only succeeds if event is still 'active' (compare-and-swap)
	// This prevents duplicate completion if multiple instances fire simultaneously
	rowsAffected, err := qtx.UpdateEventStatusToCompleted(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Error("Failed to execute update event status to completed",
			"event_id", eventID,
			"error", err,
			"action", "transaction will rollback")
		return false
	}

	// Check if we actually updated the row (compare-and-swap success)
	if rowsAffected == 0 {
		// Another instance already completed this event - this is normal
		e.logger.Info("Event already completed by another instance, skipping",
			"event_id", eventID)
		return false
	}

	e.logger.Info("Successfully marked event as completed in database",
		"event_id", eventID,
		"rows_affected", rowsAffected)

	// Step 2: Update all room player states for this event
	// present -> completed, disconnected -> left
	// IMPORTANT: This must be in same transaction as status update
	playerRowsAffected, err := qtx.UpdateRoomPlayerStatesOnEventComplete(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Error("Failed to update room player states on event complete",
			"event_id", eventID,
			"error", err,
			"action", "transaction will rollback - event stays active")
		// Transaction will rollback - event status NOT updated
		// Monitor will retry on next cycle
		return false
	}

	e.logger.Info("Successfully updated room player states within transaction",
		"event_id", eventID,
		"players_updated", playerRowsAffected)

	// Step 3: P0-1 - Capture final leaderboard snapshot (permanent historical record)
	// This must be in the same transaction to ensure atomicity with status update
	// CRITICAL: This is the only way to prove who won after event ends
	err = qtx.CaptureFinalRoomLeaderboards(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Error("Failed to capture final room leaderboards",
			"event_id", eventID,
			"error", err,
			"action", "transaction will rollback - event stays active")
		// Transaction will rollback - we need leaderboard snapshot
		// This is critical data - without it we can't determine winners
		return false
	}

	e.logger.Info("Successfully captured final room leaderboards snapshot",
		"event_id", eventID)

	// Step 4: Capture final guild leaderboard snapshot
	// This determines which guild won the event
	err = qtx.CaptureFinalGuildLeaderboard(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Warn("Failed to capture final guild leaderboard",
			"event_id", eventID,
			"error", err,
			"action", "continuing - guild leaderboard is optional")
		// NOTE: We continue here because guild leaderboard is less critical
		// Individual player rankings are the primary record
		// Guild rankings can be recalculated from player data if needed
	} else {
		e.logger.Info("Successfully captured final guild leaderboard snapshot",
			"event_id", eventID)
	}

	// TODO: Grant achievements to top 3 and all user of event
	winningMembers, err := qtx.GetTop3PlayersByEvent(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Warn("Failed to get top 3 players by event",
			"event_id", eventID,
			"error", err,
			"action", "continuing - top 3 players are optional")
		return false
	} else {
		e.logger.Info("Successfully retrieved top 3 players by event",
			"event_id", eventID,
			"winning_members", winningMembers)
	}
	// granting using grpc
	// TODO: Refactor later
	for i, member := range winningMembers {
		resp, err := e.userClient.GrantAchievements(ctx,
			&pb.GrantAchievementsRequest{
				UserAchievements: []*pb.UserAchievementGrant{
					&pb.UserAchievementGrant{
						UserId:         member.UserID.String(),
						AchievementKey: AchievementCodeBattle[i],
					},
				},
			})
		if err != nil {
			e.logger.Error("Failed to grant achievement to top 3 players",
				"event_id", eventID,
				"error", err,
				"action", "continuing - top 3 players are optional")
			return false
		}
		e.logger.Debug("Achievement granted.", *resp)
	}

	// Step 5: Commit the transaction (all-or-nothing)
	// If this fails, everything above is rolled back
	if err := tx.Commit(ctx); err != nil {
		e.logger.Error("CRITICAL: Failed to commit event completion transaction",
			"event_id", eventID,
			"error", err,
			"action", "transaction rolled back - event stays active - will retry")
		// Transaction rolled back - event still 'active'
		// Monitor will retry on next cycle
		return false
	}

	e.logger.Info("Successfully committed event completion transaction",
		"event_id", eventID,
		"players_updated", playerRowsAffected,
		"final_leaderboard_captured", true)

	return true
}

// buildEventExpiredMessage creates an enhanced EventExpired message with winners and statistics.
// P2-1: Provides rich information to players about event results
func (e *EventHub) buildEventExpiredMessage(ctx context.Context, eventID uuid.UUID) events.EventExpired {
	// Base message
	eventExpired := events.EventExpired{
		EventID:     eventID,
		CompletedAt: time.Now().UTC(),
		Message:     "Event has ended. Thank you for participating!",
		TopPlayers:  []events.TopPlayer{},
		TopGuilds:   []events.TopGuild{},
	}

	// Fetch top 3 players
	topPlayers, err := e.queries.GetTop3PlayersByEvent(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Warn("Failed to fetch top players for event expiry message",
			"event_id", eventID,
			"error", err)
	} else {
		for _, player := range topPlayers {
			eventExpired.TopPlayers = append(eventExpired.TopPlayers, events.TopPlayer{
				Rank:     int(player.Rank.Int32), // pgtype.Int4
				UserID:   uuid.UUID(player.UserID.Bytes),
				Username: player.Username,
				Score:    int(player.Score),
				RoomID:   uuid.UUID(player.RoomID.Bytes),
			})
		}
		e.logger.Info("Added top players to event expiry message",
			"event_id", eventID,
			"count", len(eventExpired.TopPlayers))
	}

	// Fetch top 3 guilds
	topGuilds, err := e.queries.GetTop3GuildsByEvent(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Warn("Failed to fetch top guilds for event expiry message",
			"event_id", eventID,
			"error", err)
	} else {
		for _, guild := range topGuilds {
			// Extract guild name with type assertion (interface{} -> string)
			guildName := ""
			if guild.GuildName != nil {
				guildName, _ = guild.GuildName.(string)
			}

			eventExpired.TopGuilds = append(eventExpired.TopGuilds, events.TopGuild{
				Rank:       int(guild.Rank), // already int64
				GuildID:    uuid.UUID(guild.GuildID.Bytes),
				GuildName:  guildName,
				TotalScore: int(guild.TotalScore),
			})
		}
		e.logger.Info("Added top guilds to event expiry message",
			"event_id", eventID,
			"count", len(eventExpired.TopGuilds))
	}

	// Enhance message with winner information
	if len(eventExpired.TopPlayers) > 0 {
		winner := eventExpired.TopPlayers[0]
		eventExpired.Message = fmt.Sprintf(
			"Event has ended! Congratulations to %s for winning with %d points! Thank you all for participating!",
			winner.Username,
			winner.Score,
		)
	}

	return eventExpired
}

// publishEventExpiredWithRetry publishes EventExpired message to RabbitMQ with exponential backoff retry.
// This is a critical operation - if it fails, other instances won't be notified that event ended.
// P0-2: Retry mechanism for RabbitMQ publish failures
// P2-1: Enhanced with winners and statistics
func (e *EventHub) publishEventExpiredWithRetry(ctx context.Context, eventID uuid.UUID) {
	// P2-1: Build enhanced EventExpired message with winners and stats
	eventExpired := e.buildEventExpiredMessage(ctx, eventID)

	sseEvent := events.SseEvent{
		EventType: events.EVENT_EXPIRED,
		Data:      eventExpired,
	}

	// Marshal payload once (outside retry loop)
	payload, err := json.Marshal(sseEvent)
	if err != nil {
		e.logger.Error("CRITICAL: Failed to marshal EventExpired for RabbitMQ",
			"event_id", eventID,
			"error", err,
			"impact", "other instances will not be notified - monitor will handle")
		// This is a code bug - JSON marshaling should never fail
		// Continue anyway - the monitor will catch orphaned instances
		return
	}

	routingKey := fmt.Sprintf("event.%s", eventID)

	// Retry configuration
	const maxRetries = 5
	const initialBackoff = 100 * time.Millisecond
	const maxBackoff = 5 * time.Second

	backoff := initialBackoff
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Check if context cancelled
		if ctx.Err() != nil {
			e.logger.Error("Context cancelled during RabbitMQ publish retry",
				"event_id", eventID,
				"attempt", attempt,
				"error", ctx.Err())
			return
		}

		// Attempt to publish
		err = e.rabbitClient.Publish(ctx, rabbitmq.SseEventsExchange, routingKey, payload)
		if err == nil {
			// Success!
			e.logger.Info("Successfully published EventExpired to RabbitMQ",
				"event_id", eventID,
				"routing_key", routingKey,
				"attempts", attempt)
			return
		}

		// Failed - log and retry
		lastErr = err
		e.logger.Warn("Failed to publish EventExpired to RabbitMQ, will retry",
			"event_id", eventID,
			"attempt", attempt,
			"max_retries", maxRetries,
			"error", err,
			"next_retry_in", backoff)

		// Wait before retry (with exponential backoff)
		select {
		case <-ctx.Done():
			e.logger.Error("Context cancelled during retry backoff",
				"event_id", eventID)
			return
		case <-time.After(backoff):
			// Continue to next retry
		}

		// Exponential backoff: double the wait time, cap at maxBackoff
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	// All retries exhausted - log critical error
	e.logger.Error("CRITICAL: Failed to publish EventExpired after all retries",
		"event_id", eventID,
		"attempts", maxRetries,
		"last_error", lastErr,
		"routing_key", routingKey,
		"impact", "some instances may not receive notification",
		"mitigation", "expired event monitor will detect and notify")

	// NOTE: We don't panic or return error here because:
	// 1. Event is already marked as completed in database (source of truth)
	// 2. Our expired event monitor will detect instances that missed the message
	// 3. Submissions will be rejected due to database validation
	// This is degraded operation but not a catastrophic failure
}

// recoverActiveEvents queries for all active events on startup and re-schedules their expiry timers.
// This ensures that if the service restarts, events continue to expire correctly.
//
// Called by: NewEventHub() in a goroutine with 30-second timeout
func (e *EventHub) recoverActiveEvents(ctx context.Context) {
	e.logger.Info("Recovering active events and scheduling expiry timers")

	// Query all active events from database
	activeEvents, err := e.queries.GetActiveEvents(ctx)
	if err != nil {
		e.logger.Error("Failed to query active events for recovery", "error", err)
		return
	}

	if len(activeEvents) == 0 {
		e.logger.Info("No active events to recover")
		return
	}

	e.logger.Info("Found active events to recover", "count", len(activeEvents))

	// Schedule expiry timer for each active event
	for _, event := range activeEvents {
		eventID := uuid.UUID(event.ID.Bytes)
		endDate := event.EndDate.Time

		e.logger.Info("Recovering event",
			"event_id", eventID,
			"title", event.Title,
			"end_date", endDate)

		// Schedule the timer
		e.ScheduleEventExpiry(eventID, endDate)
	}

	e.logger.Info("Event recovery complete", "recovered_count", len(activeEvents))
}

// startExpiredEventMonitor runs a background goroutine that periodically checks
// for events stuck in 'active' status past their end_date and completes them.
//
// This is a critical defense-in-depth mechanism that protects against:
// 1. Timer loss due to service crashes/restarts between status transition and timer scheduling
// 2. Failed timer scheduling (e.g., if duration <= 0)
// 3. Clock skew between multiple instances
// 4. Database replication lag issues
//
// Design decisions:
// - Runs every 60 seconds (configurable via ticker interval)
// - Uses database as source of truth (not in-memory state)
// - Each instance runs its own monitor (completeEvent is idempotent)
// - First run happens immediately on startup (catch any existing issues)
// - 30-second timeout per check cycle (prevents blocking)
func (e *EventHub) startExpiredEventMonitor() {
	const checkInterval = 60 * time.Second
	const checkTimeout = 30 * time.Second

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	e.logger.Info("Expired event monitor started",
		"check_interval", checkInterval,
		"timeout_per_check", checkTimeout)

	// Run immediately on startup (don't wait for first tick)
	// This catches events that expired while service was down
	e.checkAndCompleteExpiredEvents(checkTimeout)

	// Then run periodically
	for range ticker.C {
		e.checkAndCompleteExpiredEvents(checkTimeout)
	}
}

// checkAndCompleteExpiredEvents performs a single check cycle:
// 1. Query database for events WHERE status='active' AND end_date < NOW()
// 2. Attempt to complete each expired event
// 3. Verify completion and log results
//
// This function is safe to run concurrently across multiple instances because
// completeEvent() uses atomic compare-and-swap (only one instance succeeds).
func (e *EventHub) checkAndCompleteExpiredEvents(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Query expired events using the new SQL query
	expiredEvents, err := e.queries.GetExpiredActiveEvents(ctx)
	if err != nil {
		e.logger.Error("Failed to query expired active events",
			"error", err,
			"timeout", timeout)
		return
	}

	// Normal case: no expired events found
	if len(expiredEvents) == 0 {
		e.logger.Debug("Expired event check complete - no issues found")
		return
	}

	// ALERT: Found events that should have been completed but are still active
	// This indicates a timer was lost or failed to schedule
	eventIDs := make([]string, len(expiredEvents))
	for i, evt := range expiredEvents {
		eventIDs[i] = uuid.UUID(evt.ID.Bytes).String()
	}

	e.logger.Warn("CRITICAL: Found expired events still marked as active",
		"count", len(expiredEvents),
		"event_ids", eventIDs,
		"action", "completing them now")

	// Track metrics for observability
	successCount := 0
	failureCount := 0
	alreadyCompletedCount := 0

	// Process each expired event
	for _, event := range expiredEvents {
		eventID := uuid.UUID(event.ID.Bytes)
		timeOverdue := time.Since(event.EndDate.Time)

		e.logger.Warn("Attempting to complete overdue event",
			"event_id", eventID,
			"title", event.Title,
			"end_date", event.EndDate.Time,
			"overdue_duration", timeOverdue)

		// Call completeEvent() - same function used by timers
		// This ensures:
		// - Atomic status update (only succeeds if status='active')
		// - Player states updated (present->completed)
		// - RabbitMQ notification sent to all instances
		e.completeEvent(eventID)

		// Verify completion succeeded by checking status
		verifyCtx, verifyCancel := context.WithTimeout(ctx, 5*time.Second)
		updatedEvent, verifyErr := e.queries.GetEventByID(verifyCtx, event.ID)
		verifyCancel()

		if verifyErr != nil {
			e.logger.Error("Failed to verify event completion status",
				"event_id", eventID,
				"error", verifyErr)
			failureCount++
			continue
		}

		// Check if completion succeeded
		switch updatedEvent.Status {
		case store.EventStatusCompleted:
			successCount++
			e.logger.Info("Successfully completed overdue event",
				"event_id", eventID,
				"title", event.Title,
				"was_overdue_by", timeOverdue,
				"status_after", updatedEvent.Status)

		case store.EventStatusActive:
			// Still active after completeEvent() call
			// This could mean another instance completed it simultaneously
			// or there's a deeper issue
			failureCount++
			e.logger.Error("Event still active after completion attempt",
				"event_id", eventID,
				"title", event.Title,
				"status", updatedEvent.Status,
				"possible_cause", "concurrent completion failed or database issue")

		default:
			// Event is in some other status (cancelled, etc.)
			alreadyCompletedCount++
			e.logger.Info("Event was already in final status",
				"event_id", eventID,
				"status", updatedEvent.Status)
		}
	}

	// Log summary metrics for monitoring/alerting
	e.logger.Info("Expired event check cycle complete",
		"total_expired_found", len(expiredEvents),
		"successfully_completed", successCount,
		"failed_to_complete", failureCount,
		"already_completed", alreadyCompletedCount,
		"check_duration", time.Since(time.Now().Add(-timeout)))

	// If any failures occurred, log an additional error for alert visibility
	if failureCount > 0 {
		e.logger.Error("ALERT: Some expired events failed to complete",
			"failure_count", failureCount,
			"action_required", "investigate database health and event states")
	}
}

func newRoomHub(eventID, roomId uuid.UUID, db *pgxpool.Pool, queries *store.Queries, logger *slog.Logger, guildUpdateChan chan<- uuid.UUID, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client, userClient *user.Client) *RoomHub {
	return &RoomHub{
		RoomID:          roomId,
		EventID:         eventID, // Set the eventID
		Events:          make(chan any, 10),
		Listerners:      make(map[uuid.UUID]chan<- events.SseEvent),
		logger:          logger,
		queries:         queries,
		db:              db,
		Mu:              sync.RWMutex{},
		leaderboardMu:   sync.Mutex{},
		guildUpdateChan: guildUpdateChan, // Set the notification channel
		rabbitClient:    rabbitClient,
		executorClient:  executorClient,
		userClient:      userClient,
		lastActivity:    time.Now(), // Initialize with current time
		activityMu:      sync.RWMutex{},
	}
}

// listenForAMQPEvents is the central goroutine that receives all messages from RabbitMQ
// and dispatches them to the correct local listeners.
func (e *EventHub) listenForAMQPEvents() {
	msgs, err := e.rabbitClient.Consume()
	if err != nil {
		e.logger.Error("Failed to start consuming from RabbitMQ", "error", err)
		// Implement retry logic here if needed
		return
	}

	e.logger.Info("Listening for messages from RabbitMQ fanout exchange")

	for msg := range msgs {
		e.logger.Info("Received message from RabbitMQ", "routing_key", msg.RoutingKey, "payload_size", len(msg.Body))

		var sseEvent events.SseEvent
		if err := json.Unmarshal(msg.Body, &sseEvent); err != nil {
			e.logger.Error("Failed to unmarshal AMQP message", "error", err)
			continue
		}

		// Use the routing key to determine where to dispatch the event
		parts := strings.Split(msg.RoutingKey, ".")
		if len(parts) != 2 {
			continue
		}
		entityType, entityIDStr := parts[0], parts[1]
		entityID, err := uuid.Parse(entityIDStr)
		if err != nil {
			e.logger.Error("Failed to parse entity ID from routing key", "error", err, "routing_key", msg.RoutingKey)
			continue
		}

		switch entityType {
		case "room":
			// Use GetOrCreateRoomHub to support lazy-loading across instances
			// If room doesn't exist in DB, this will return nil and we'll skip the event
			ctx := context.Background()
			if room := e.GetOrCreateRoomHub(ctx, entityID); room != nil {
				room.dispatchEvent(sseEvent)
			} else {
				e.logger.Warn("Received RabbitMQ event for non-existent room, skipping",
					"room_id", entityID,
					"event_type", sseEvent.EventType)
			}
		case "event":
			// Handle event-level messages (including EventExpired)
			e.handleEventMessage(entityID, sseEvent)
		}
	}
}

// ... (GetRoomById and dispatchEventToEvent remain largely the same)

// publishEvent is a helper to publish any SseEvent to RabbitMQ.
func (r *RoomHub) publishEvent(ctx context.Context, routingKey string, sseEvent events.SseEvent) {
	payload, err := json.Marshal(sseEvent)
	if err != nil {
		r.logger.Error("Failed to marshal event for publishing", "error", err, "routing_key", routingKey)
		return
	}

	// TODO: Understand this better and maybe refactor
	err = r.rabbitClient.Publish(ctx, rabbitmq.SseEventsExchange, routingKey, payload)
	if err != nil {
		r.logger.Error("Failed to publish event to RabbitMQ", "error", err, "routing_key", routingKey)
	}
}

// GetRoomById returns a RoomHub if it exists in memory, nil otherwise.
// This method does NOT query the database or create new RoomHubs.
// For lazy-loading behavior, use GetOrCreateRoomHub instead.
func (h *EventHub) GetRoomById(roomID uuid.UUID) *RoomHub {
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	return h.Rooms[roomID]
}

// GetOrCreateRoomHub attempts to get a RoomHub from memory first.
// If not found in memory, it queries the database to verify the room exists,
// then creates and initializes a new RoomHub for it.
// This enables lazy-loading of rooms across multiple instances.
//
// Returns:
//   - *RoomHub if room exists (either in memory or in database)
//   - nil if room doesn't exist in database
//   - error is logged internally but doesn't prevent the method from returning
func (h *EventHub) GetOrCreateRoomHub(ctx context.Context, roomID uuid.UUID) *RoomHub {
	// Fast path: Check if room already exists in memory
	h.Mu.RLock()
	roomHub, exists := h.Rooms[roomID]
	h.Mu.RUnlock()

	if exists {
		h.logger.Info("Room found in memory", "room_id", roomID)
		return roomHub
	}

	// Slow path: Room not in memory, try to load from database
	h.logger.Info("Room not in memory, attempting to load from database", "room_id", roomID)

	// Query database to verify room exists and get its details
	roomRecord, err := h.queries.GetRoomByID(ctx, pgtype.UUID{Bytes: roomID, Valid: true})
	if err != nil {
		// Room doesn't exist in database
		h.logger.Warn("Room not found in database", "room_id", roomID, "error", err)
		return nil
	}

	// Room exists in database, now we need to create the RoomHub
	// Use write lock to prevent race conditions during creation
	h.Mu.Lock()
	defer h.Mu.Unlock()

	// Double-check: another goroutine might have created it while we were waiting for the lock
	if existingRoom, ok := h.Rooms[roomID]; ok {
		h.logger.Info("Room was created by another goroutine while waiting", "room_id", roomID)
		return existingRoom
	}

	// Extract event ID from the room record
	eventID := uuid.UUID(roomRecord.EventID.Bytes)

	// Create and initialize the new RoomHub
	h.logger.Info("Creating new RoomHub from database record",
		"room_id", roomID,
		"event_id", eventID,
		"room_name", roomRecord.Name)

	newRoomHub := newRoomHub(
		eventID,
		roomID,
		h.db,
		h.queries,
		h.logger,
		h.GuildUpdateChan,
		h.rabbitClient,
		h.executorClient,
		h.userClient,
	)

	// Add to in-memory map
	h.Rooms[roomID] = newRoomHub

	// NEW: Register in EventRooms map
	if _, exists := h.EventRooms[eventID]; !exists {
		h.EventRooms[eventID] = []uuid.UUID{}
	}

	// Check if roomID already in slice (shouldn't happen, but safety check)
	alreadyRegistered := false
	for _, rid := range h.EventRooms[eventID] {
		if rid == roomID {
			alreadyRegistered = true
			break
		}
	}

	if !alreadyRegistered {
		h.EventRooms[eventID] = append(h.EventRooms[eventID], roomID)
	}

	// Start the RoomHub's event processing goroutine
	go newRoomHub.Start()

	h.logger.Info("Successfully created and started RoomHub", "room_id", roomID)

	return newRoomHub
}

func (e *EventHub) CreateRoom(eventID, roomID uuid.UUID, queries *store.Queries) *RoomHub {
	r := newRoomHub(eventID, roomID, e.db, queries, e.logger, e.GuildUpdateChan, e.rabbitClient, e.executorClient, e.userClient)
	e.Mu.Lock()
	e.Rooms[roomID] = r

	// NEW: Register room in EventRooms map
	if _, exists := e.EventRooms[eventID]; !exists {
		e.EventRooms[eventID] = []uuid.UUID{}
	}
	e.EventRooms[eventID] = append(e.EventRooms[eventID], roomID)

	e.Mu.Unlock()

	e.logger.Info("Room created and registered to event",
		"room_id", roomID,
		"event_id", eventID,
		"total_rooms_in_event", len(e.EventRooms[eventID]))

	go r.Start() // Start RoomHub
	return r
}

// updateActivity marks the room as recently active
func (r *RoomHub) updateActivity() {
	r.activityMu.Lock()
	r.lastActivity = time.Now()
	r.activityMu.Unlock()
}

// getLastActivity returns the time of last activity
func (r *RoomHub) getLastActivity() time.Time {
	r.activityMu.RLock()
	defer r.activityMu.RUnlock()
	return r.lastActivity
}

// isInactive returns true if the room has no listeners and hasn't been active
// for the specified duration
func (r *RoomHub) isInactive(inactivityThreshold time.Duration) bool {
	r.Mu.RLock()
	hasListeners := len(r.Listerners) > 0
	r.Mu.RUnlock()

	if hasListeners {
		return false // Room is active if it has listeners
	}

	lastActivity := r.getLastActivity()
	return time.Since(lastActivity) > inactivityThreshold
}

// StartInactiveRoomCleanup starts a background goroutine that periodically
// removes inactive rooms from memory to prevent memory leaks.
// Rooms are considered inactive if they have no listeners and haven't
// processed any events for the specified duration.
func (e *EventHub) StartInactiveRoomCleanup(ctx context.Context, checkInterval, inactivityThreshold time.Duration) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	e.logger.Info("Starting inactive room cleanup routine",
		"check_interval", checkInterval,
		"inactivity_threshold", inactivityThreshold)

	for {
		select {
		case <-ctx.Done():
			e.logger.Info("Stopping inactive room cleanup routine")
			return
		case <-ticker.C:
			e.cleanupInactiveRooms(inactivityThreshold)
		}
	}
}

// cleanupInactiveRooms removes rooms that have been inactive for too long
func (e *EventHub) cleanupInactiveRooms(inactivityThreshold time.Duration) {
	e.Mu.Lock()
	defer e.Mu.Unlock()

	roomsToRemove := []uuid.UUID{}

	// Identify inactive rooms
	for roomID, roomHub := range e.Rooms {
		if roomHub.isInactive(inactivityThreshold) {
			roomsToRemove = append(roomsToRemove, roomID)
		}
	}

	// Remove inactive rooms
	for _, roomID := range roomsToRemove {
		e.logger.Info("Removing inactive room from memory",
			"room_id", roomID,
			"last_activity", e.Rooms[roomID].getLastActivity())

		// Close the room's event channel to stop its goroutine
		close(e.Rooms[roomID].Events)

		// Remove from map
		delete(e.Rooms, roomID)
	}

	if len(roomsToRemove) > 0 {
		e.logger.Info("Cleaned up inactive rooms",
			"count", len(roomsToRemove),
			"remaining_rooms", len(e.Rooms))
	}
}

func (e *EventHub) Start() {
	for eventID := range e.GuildUpdateChan {
		e.logger.Info("Received guild leaderboard update notification", "event_id", eventID)

		ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeoutSecond)

		// TODO: You will need a new SQLc query to recalculate the total score for each guild
		// based on the sum of their players' scores across all rooms in the event.
		// For now, we assume you have a way to get the updated leaderboard.

		// Fetch the latest guild leaderboard data from the database
		guildEntries, err := e.queries.GetGuildLeaderboardByEvent(ctx, toPgtypeUUID(eventID))
		if err != nil {
			e.logger.Error("failed to get guild leaderboard for event", "event_id", eventID, "error", err)
			cancel()
			continue
		}

		// Create the SSE event payload
		sseEvent := events.SseEvent{
			EventType: events.GUILD_LEADERBOARD_UPDATED,
			Data:      guildEntries, // Assuming your frontend can handle this structure
		}

		// Broadcast the new leaderboard to all subscribed clients for this event
		e.dispatchEventToEvent(eventID, sseEvent)
		cancel()
	}
}

// handleEventMessage handles event-level messages received from RabbitMQ
func (e *EventHub) handleEventMessage(eventID uuid.UUID, sseEvent events.SseEvent) {
	switch sseEvent.EventType {
	case events.EVENT_EXPIRED:
		// EventExpired message received from RabbitMQ
		// This is broadcast by the ONE instance that successfully updated the DB
		// ALL instances receive it and broadcast to their connected players
		e.logger.Info("Received EventExpired from RabbitMQ", "event_id", eventID)

		// Broadcast to all rooms in this event
		e.Mu.RLock()
		roomIDs, exists := e.EventRooms[eventID]
		e.Mu.RUnlock()

		if !exists || len(roomIDs) == 0 {
			e.logger.Warn("No rooms found for event", "event_id", eventID)
		} else {
			e.logger.Info("Broadcasting EventExpired to rooms",
				"event_id", eventID,
				"room_count", len(roomIDs))

			affectedRooms := 0
			for _, roomID := range roomIDs {
				e.Mu.RLock()
				roomHub, exists := e.Rooms[roomID]
				e.Mu.RUnlock()

				if !exists {
					e.logger.Warn("Room not found in memory",
						"room_id", roomID,
						"event_id", eventID)
					continue
				}

				// Non-blocking send to room's event channel
				select {
				case roomHub.Listerners[roomID] <- sseEvent:
					affectedRooms++
				default:
					e.logger.Warn("Room event channel full, skipping",
						"room_id", roomID)
				}
			}

			e.logger.Info("EventExpired broadcast complete",
				"event_id", eventID,
				"rooms_notified", affectedRooms)
		}

		// Also broadcast to event-level spectators
		e.dispatchEventToEvent(eventID, sseEvent)

	default:
		// Other event-level messages (guild leaderboard, etc.)
		e.dispatchEventToEvent(eventID, sseEvent)
	}
}

// dispatchEventToEvent sends an SSE event to all listeners for a specific event.
func (e *EventHub) dispatchEventToEvent(eventID uuid.UUID, sseEvent events.SseEvent) {
	e.EventListenersMu.RLock()
	defer e.EventListenersMu.RUnlock()

	listeners, ok := e.EventListeners[eventID]
	if !ok {
		e.logger.Info("No event listeners found for guild update", "event_id", eventID)
		return
	}

	e.logger.Info("Dispatching guild leaderboard update", "event_id", eventID, "listeners_count", len(listeners))
	for clientID, listener := range listeners {
		go func(l chan<- events.SseEvent, cID uuid.UUID) {
			select {
			case l <- sseEvent:
				// Sent successfully
			default:
				e.logger.Warn("Failed to send guild update to client, channel full or closed", "client_id", cID)
			}
		}(listener, clientID)
	}
}

// Start will start to listen and serve events to players connected to the room
func (r *RoomHub) Start() {
	for event := range r.Events {
		// Mark room as active whenever we process an event
		r.updateActivity()

		switch e := event.(type) {
		case events.SolutionSubmitted:
			if err := r.processSolutionSubmitted(e); err != nil {
				r.logger.Error("failed to process solution submitted event", "error", err)
			}

		case events.SolutionResult:
			if err := r.processSolutionResult(e); err != nil {
				r.logger.Error("failed to process correct solution result event", "error", err)
			}

		case events.PlayerJoined:
			if err := r.processPlayerJoined(e); err != nil {
				r.logger.Error("failed to process player joined event", "error", err)
			}
		case events.PlayerLeft:
			if err := r.processPlayerLeft(e); err != nil {
				r.logger.Error("failed to process player left event", "error", err)
			}
		case events.RoomDeleted:
			if err := r.processRoomDeleted(e); err != nil {
				r.logger.Error("failed to process room deleted event", "error", err)
			}

		case events.EventExpired:
			if err := r.processEventExpired(e); err != nil {
				r.logger.Error("failed to process event expired event", "error", err)
			}
		}
	}
}

func (r *RoomHub) dispatchEvent(e events.SseEvent) {
	// Safely copy listeners to avoid race conditions
	r.Mu.RLock()
	if r.Listerners == nil {
		r.Mu.RUnlock()
		r.logger.Warn("no listeners map found")
		return
	}

	listeners := make(map[uuid.UUID]chan<- events.SseEvent)
	for pid, listener := range r.Listerners {
		listeners[pid] = listener
	}
	r.Mu.RUnlock()

	r.logger.Info("Hit dispatchEvent()",
		"Number of Listeners", len(listeners),
		"Event", e)

	for playerId, listener := range listeners {
		// Capture the listener variable properly
		go func(l chan<- events.SseEvent, pid uuid.UUID) {
			defer func() {
				if a := recover(); a != nil {
					r.logger.Error("panic while dispatching event", "error", r, "player_id", pid, "event", e)
				}
			}()

			r.logger.Info("dispatching to", "player_id", pid)
			select {
			case l <- e:
				// Successfully sent
				r.logger.Info("event sent to", "player_id", pid)
			default:
				// Channel is full or closed, log but don't block
				r.logger.Warn("failed to send event to listener - channel full or closed", "player_id", pid)
			}
		}(listener, playerId)
	}
}

// This only works in the same instance the request received
func (r *RoomHub) dispatchEventToPlayer(e events.SseEvent, playerID uuid.UUID) {
	r.Mu.RLock()
	if r.Listerners == nil {
		r.Mu.RUnlock()
		r.logger.Warn("no listeners map found")
		return
	}

	// find the target listener
	var listener chan<- events.SseEvent
	for pid, l := range r.Listerners {
		if pid == playerID {
			listener = l
		}
	}

	if listener == nil {
		r.logger.Error("listener not found", "player_id", playerID)
		r.Mu.RUnlock()
		return
	}

	r.Mu.RUnlock()

	// Capture the listener variable properly
	go func(l chan<- events.SseEvent, pid uuid.UUID) {
		defer func() {
			if a := recover(); a != nil {
				r.logger.Error("panic while dispatching event", "error", r, "player_id", pid, "event", e)
			}
		}()

		r.logger.Info("dispatching to", "player_id", pid)
		select {
		case l <- e:
			// Successfully sent
			r.logger.Info("event sent to", "player_id", pid)
		default:
			// Channel is full or closed, log but don't block
			r.logger.Warn("failed to send event to listener - channel full or closed", "player_id", pid)
		}
	}(listener, playerID)
}

// TODO: Rewrite processSolutionSubmitted and processSolutionResult
func (r *RoomHub) processSolutionSubmitted(event events.SolutionSubmitted) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeoutSecond)
	defer cancel()

	lang, err := r.queries.GetLanguageByName(ctx, event.Language)
	if err != nil {
		r.logger.Error("error", "lang", event.Language)
		return err
	}

	submission, err := r.queries.CreateSubmission(ctx, store.CreateSubmissionParams{
		UserID:        toPgtypeUUID(event.PlayerID),
		CodeProblemID: toPgtypeUUID(event.ProblemID),
		LanguageID:    lang.ID,
		RoomID:        toPgtypeUUID(event.RoomID),
		CodeSubmitted: event.Code,
		Status:        store.SubmissionStatusPending,
	})

	if err != nil {
		r.logger.Error("failed to create submission", "err", err)
		return err
	}

	event.SubmissionID = submission.ID.Bytes

	problem, err := r.queries.GetCodeProblemLanguageDetail(ctx, store.GetCodeProblemLanguageDetailParams{
		CodeProblemID: toPgtypeUUID(event.ProblemID),
		LanguageID:    lang.ID,
	})
	if err != nil {
		r.logger.Error("Error", "question", err)
		return err
	}

	testCases, err := r.queries.GetTestCasesByProblem(ctx, problem.CodeProblemID)
	if err != nil {
		return err
	}

	result, err := r.executorClient.ExecuteCode(ctx, &pb.ExecuteRequest{
		Language:     lang.Name,
		Code:         event.Code,
		DriverCode:   problem.DriverCode,
		CompileCmd:   lang.CompileCmd,
		RunCmd:       lang.RunCmd,
		TempFileDir:  lang.TempFileDir.String,
		TempFileName: lang.TempFileName.String,
		TestCases:    storeTestCasesToPbTestCases(testCases),
	})
	if err != nil {
		return err
	}

	eventCodeProblem, err := r.queries.GetEventCodeProblem(ctx, store.GetEventCodeProblemParams{
		EventID:       toPgtypeUUID(event.EventID),
		CodeProblemID: toPgtypeUUID(event.ProblemID),
	})
	if err != nil {
		return err
	}

	solutionResult := r.generateSolutionResult(event, result, eventCodeProblem.Score)
	r.Events <- solutionResult

	return nil
}

// generateSolutionResult creates a SolutionResult from the execution response
func (r *RoomHub) generateSolutionResult(solutionSubmitted events.SolutionSubmitted, result *pb.ExecuteResponse, score int32) events.SolutionResult {
	solutionResult := events.SolutionResult{
		SolutionSubmitted: solutionSubmitted,
		Score:             score,
		Status:            events.Accepted,
		Message:           "Solution is correct!",
		ExecutionTimeMs:   result.ExecutionTimeMs,
	}

	// If execution was not successful, determine the error type and set appropriate status
	if !result.Success {
		solutionResult.Score = 0

		switch result.ErrorType {
		case "COMPILE_ERROR":
			solutionResult.Message = fmt.Sprintf("Compilation error: %v", result.Message)
			solutionResult.Status = events.CompilationError

		case "RUNTIME_ERROR":
			solutionResult.Message = fmt.Sprintf("Runtime error: %v", result.Message)
			solutionResult.Status = events.RuntimeError

		case "WRONG_ANSWER":
			solutionResult.Message = fmt.Sprintf("Wrong answer: %v", result.Message)
			solutionResult.Status = events.WrongAnswer

		default:
			solutionResult.Message = fmt.Sprintf("Execution failed: %v", result.Message)
			solutionResult.Status = events.RuntimeError
		}
	}

	return solutionResult
}

func (r *RoomHub) processSolutionResult(event events.SolutionResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	roomRoutingKey := fmt.Sprintf("room.%s", r.RoomID)

	r.logger.Info("processing solution result...", "submission_id", event.SolutionSubmitted.SubmissionID)

	// CRITICAL FIX: Validate event is still active before processing solution
	// This prevents race condition where timer completes event while submission is being processed
	// See: Race Condition Timeline in docs - submission can be in-flight when event expires
	eventDetails, err := r.queries.GetEventByID(ctx, toPgtypeUUID(r.EventID))
	if err != nil {
		r.logger.Error("Failed to get event details during solution processing",
			"event_id", r.EventID,
			"submission_id", event.SolutionSubmitted.SubmissionID,
			"error", err)
		return fmt.Errorf("failed to get event: %w", err)
	}

	// Check 1: Event status must be 'active'
	// If status changed to 'completed', reject this submission even if solution is correct
	if eventDetails.Status != store.EventStatusActive {
		r.logger.Warn("Rejecting solution result - event is not active",
			"event_id", r.EventID,
			"event_status", eventDetails.Status,
			"submission_id", event.SolutionSubmitted.SubmissionID,
			"player_id", event.SolutionSubmitted.PlayerID,
			"was_correct", event.Status == events.Accepted)

		// Update submission status to indicate it was rejected due to event completion
		// We still record the submission but don't award points
		_, updateErr := r.queries.UpdateSubmission(ctx, store.UpdateSubmissionParams{
			ID:     toPgtypeUUID(event.SolutionSubmitted.SubmissionID),
			Status: store.SubmissionStatusPending, // Mark as pending since it wasn't scored
		})
		if updateErr != nil {
			r.logger.Error("Failed to update submission status for rejected late submission",
				"submission_id", event.SolutionSubmitted.SubmissionID,
				"error", updateErr)
		}

		return fmt.Errorf("event is not active (status: %s)", eventDetails.Status)
	}

	// Check 2: Current time must be before end_date
	// NOTE: This check has a grace period built in:
	// - Timer fires at end_date
	// - Waits 10 seconds (grace period) before marking event as 'completed'
	// - During grace period: status='active' but time > end_date
	// - In-flight submissions pass Check 1 (status) but may fail Check 2 (time)
	// - This is intentional: we want to allow submissions that were:
	//   * Accepted by HTTP handler before end_date
	//   * Still being judged when timer fired
	//   * Complete during grace period
	// - If submission completes >10 seconds after end_date, it's rejected here
	//
	// P1-3: Grace period prevents unfair rejection while limiting how late we accept results
	if time.Now().UTC().After(eventDetails.EndDate.Time.UTC().Add(10 * time.Second)) {
		r.logger.Warn("Rejecting solution result - grace period expired",
			"event_id", r.EventID,
			"end_date", eventDetails.EndDate.Time,
			"grace_period", 10*time.Second,
			"current_time", time.Now(),
			"submission_id", event.SolutionSubmitted.SubmissionID,
			"player_id", event.SolutionSubmitted.PlayerID,
			"time_past_deadline", time.Since(eventDetails.EndDate.Time))

		// Update submission status
		_, updateErr := r.queries.UpdateSubmission(ctx, store.UpdateSubmissionParams{
			ID:     toPgtypeUUID(event.SolutionSubmitted.SubmissionID),
			Status: store.SubmissionStatusPending,
		})
		if updateErr != nil {
			r.logger.Error("Failed to update submission status for expired submission",
				"submission_id", event.SolutionSubmitted.SubmissionID,
				"error", updateErr)
		}

		return fmt.Errorf("event has expired at %s (now: %s)", eventDetails.EndDate.Time.UTC(), time.Now().UTC())
	}

	if event.Status != events.Accepted {
		r.logger.Info("solution failed", "event", event)

		// Map JudgeStatus to SubmissionStatus
		var submissionStatus store.SubmissionStatus
		switch event.Status {
		case events.CompilationError:
			submissionStatus = store.SubmissionStatusPending // or create a compilation_error status if available
		case events.RuntimeError:
			submissionStatus = store.SubmissionStatusPending // or create a runtime_error status if available
		case events.WrongAnswer:
			submissionStatus = store.SubmissionStatusWrongAnswer
		default:
			submissionStatus = store.SubmissionStatusPending
		}

		// Update submission status in database
		_, err := r.queries.UpdateSubmission(ctx, store.UpdateSubmissionParams{
			ID:     toPgtypeUUID(event.SolutionSubmitted.SubmissionID),
			Status: submissionStatus,
		})
		if err != nil {
			r.logger.Error("Failed to update submission status for failed solution",
				"submission_id", event.SolutionSubmitted.SubmissionID,
				"error", err)
			// Continue despite error - don't block the event flow
		}

		sseEvent := events.SseEvent{
			EventType: events.WRONG_SOLUTION_SUBMITTED,
			Data:      event, // Send the full result object instead of formatted string
		}

		r.publishEvent(ctx, roomRoutingKey, sseEvent)

		return nil
	}

	// CRITICAL FIX: Wrap score update and leaderboard calculation in a single transaction
	// This prevents race conditions when multiple instances process submissions simultaneously
	r.logger.Info("Starting atomic score update and leaderboard calculation",
		"room_id", r.RoomID,
		"player_id", event.SolutionSubmitted.PlayerID,
		"score", event.Score)

	// Begin transaction
	tx, err := r.db.Begin(ctx)
	if err != nil {
		r.logger.Error("Failed to begin transaction for score update",
			"room_id", r.RoomID,
			"error", err)
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Create query runner that executes within this transaction
	qtx := r.queries.WithTx(tx)

	// CRITICAL: Perform duplicate check INSIDE transaction to prevent race condition
	// This ensures atomicity - if two submissions arrive simultaneously, only one will succeed
	// The check in HTTP handler is a first-pass filter, but this is the authoritative check
	existingSolution, err := qtx.CheckIfProblemAlreadySolved(ctx, store.CheckIfProblemAlreadySolvedParams{
		UserID:        toPgtypeUUID(event.SolutionSubmitted.PlayerID),
		CodeProblemID: toPgtypeUUID(event.SolutionSubmitted.ProblemID),
		RoomID:        toPgtypeUUID(event.SolutionSubmitted.RoomID),
	})

	if err == nil {
		// Problem already solved within transaction - another submission beat us
		r.logger.Warn("Duplicate solution detected during transaction - rejecting",
			"player_id", event.SolutionSubmitted.PlayerID,
			"problem_id", event.SolutionSubmitted.ProblemID,
			"room_id", r.RoomID,
			"original_submission_id", existingSolution.ID,
			"duplicate_submission_id", event.SolutionSubmitted.SubmissionID,
			"solved_at", existingSolution.SubmittedAt)

		// Update the current submission to mark it as duplicate (don't award points)
		_, updateErr := qtx.UpdateSubmission(ctx, store.UpdateSubmissionParams{
			ID:     toPgtypeUUID(event.SolutionSubmitted.SubmissionID),
			Status: store.SubmissionStatusPending, // Mark as pending to indicate it wasn't scored
		})
		if updateErr != nil {
			r.logger.Error("Failed to update duplicate submission status",
				"submission_id", event.SolutionSubmitted.SubmissionID,
				"error", updateErr)
		}

		// Rollback transaction (no points awarded)
		tx.Rollback(ctx)
		return fmt.Errorf("problem already solved by player in this room")
	}
	// If error is pgx.ErrNoRows, that's expected - problem not solved yet, continue

	// Update player score within transaction
	err = qtx.AddRoomPlayerScore(ctx, store.AddRoomPlayerScoreParams{
		PointsToAdd: int32(event.Score),
		UserID:      toPgtypeUUID(event.SolutionSubmitted.PlayerID),
		RoomID:      toPgtypeUUID(event.SolutionSubmitted.RoomID),
	})
	if err != nil {
		r.logger.Error("Failed to add score within transaction",
			"room_id", r.RoomID,
			"error", err)
		return fmt.Errorf("failed to add score: %w", err)
	}

	// Calculate leaderboard within SAME transaction
	// The CalculateRoomLeaderboard query uses SELECT FOR UPDATE to lock rows
	err = qtx.CalculateRoomLeaderboard(ctx, toPgtypeUUID(r.RoomID))
	if err != nil {
		r.logger.Error("Failed to calculate leaderboard within transaction",
			"room_id", r.RoomID,
			"error", err)
		return fmt.Errorf("failed to calculate leaderboard: %w", err)
	}

	// Update submission status to 'accepted' within the transaction
	// This ensures atomicity: if leaderboard calculation fails, submission isn't marked as accepted
	//
	executionTimeMs := parseExecutionTime(event.ExecutionTimeMs)
	if executionTimeMs == (pgtype.Int4{}) && event.ExecutionTimeMs != "" {
		r.logger.Warn("failed to parse execution time", "value", event.ExecutionTimeMs)
	}
	_, err = qtx.UpdateSubmission(ctx, store.UpdateSubmissionParams{
		ID:              toPgtypeUUID(event.SolutionSubmitted.SubmissionID),
		Status:          store.SubmissionStatusAccepted,
		ExecutionTimeMs: pgtype.Int4{Int32: 0, Valid: false}, // TODO: Get actual execution time from result
	})
	if err != nil {
		r.logger.Error("Failed to update submission status to accepted",
			"submission_id", event.SolutionSubmitted.SubmissionID,
			"error", err)
		return fmt.Errorf("failed to update submission status: %w", err)
	}

	// Commit the transaction atomically
	// This commits: score update + leaderboard calculation + submission status update
	err = tx.Commit(ctx)
	if err != nil {
		r.logger.Error("Failed to commit score and leaderboard transaction",
			"room_id", r.RoomID,
			"error", err)
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	r.logger.Info("Successfully committed atomic score update and leaderboard calculation",
		"room_id", r.RoomID,
		"player_id", event.SolutionSubmitted.PlayerID)

	roomEntries, err := r.getRoomLeaderboardEntries(ctx)
	if err != nil {
		return err
	}

	// Publish correct solution event with full result details
	correctSolutionEvent := events.SseEvent{
		EventType: events.CORRECT_SOLUTION_SUBMITTED,
		Data:      event, // Send full result with score, message, etc.
	}
	r.publishEvent(ctx, roomRoutingKey, correctSolutionEvent)

	// Publish updated room leaderboard
	roomLeaderboardEvent := events.SseEvent{EventType: events.LEADERBOARD_UPDATED, Data: roomEntries}
	r.publishEvent(ctx, roomRoutingKey, roomLeaderboardEvent)

	// Publish updated guild leaderboard
	guildEntries, err := r.queries.GetGuildLeaderboardByEvent(ctx, toPgtypeUUID(r.EventID))
	if err == nil {
		eventRoutingKey := fmt.Sprintf("event.%s", r.EventID)
		guildLeaderboardEvent := events.SseEvent{EventType: events.GUILD_LEADERBOARD_UPDATED, Data: guildEntries}
		r.publishEvent(ctx, eventRoutingKey, guildLeaderboardEvent)
	} else {
		r.logger.Error("failed to get guild leaderboard for event", "error", err, "event_id", r.EventID)
	}

	r.logger.Info("Solution processed successfully",
		"submission_id", event.SolutionSubmitted.SubmissionID,
		"player_id", event.SolutionSubmitted.PlayerID,
		"score", event.Score)

	return nil
}

func parseExecutionTime(execTimeStr string) pgtype.Int4 {
	if execTimeStr == "" {
		return pgtype.Int4{}
	}

	// Remove "ms" suffix and parse the numeric value
	timeStr := strings.TrimSuffix(execTimeStr, "ms")
	timeValue, err := strconv.ParseInt(timeStr, 10, 32)
	if err != nil {
		// Return null pgtype.Int4 if parsing fails
		return pgtype.Int4{}
	}

	return pgtype.Int4{Int32: int32(timeValue), Valid: true}
}

// Helper function to convert pgtype.Int4 to string with "ms" suffix
func formatExecutionTime(execTime pgtype.Int4) string {
	if !execTime.Valid {
		return ""
	}
	return strconv.FormatInt(int64(execTime.Int32), 10) + "ms"
}

// Helper method to check if player is in room
func (r *RoomHub) playerInRoom(ctx context.Context, roomID, playerID uuid.UUID) bool {
	player, err := r.queries.GetRoomPlayer(ctx, store.GetRoomPlayerParams{
		RoomID: toPgtypeUUID(roomID),
		UserID: toPgtypeUUID(playerID),
	})

	if err != nil {
		r.logger.Error("failed to get player", "err", err)
		return false
	}

	if err == sql.ErrNoRows {
		r.logger.Error("no player found", "err", sql.ErrNoRows)
		return false
	}

	r.logger.Info("player found", "player", player)

	return true
}

// Helper method to add player to room
func (r *RoomHub) addPlayerToRoom(ctx context.Context, guildID, roomID, playerID uuid.UUID, playerName string) error {
	createParams := store.CreateRoomPlayerParams{
		RoomID:   toPgtypeUUID(roomID),
		UserID:   toPgtypeUUID(playerID),
		Username: playerName,
		State:    store.RoomPlayerStatePresent,
		GuildID:  toPgtypeUUID(guildID),
	}

	_, err := r.queries.CreateRoomPlayer(ctx, createParams)
	return err
}

// Helper method to remove player from room
func (r *RoomHub) removePlayerFromRoom(ctx context.Context, roomID, playerID uuid.UUID) error {
	return r.queries.DeleteRoomPlayer(ctx, store.DeleteRoomPlayerParams{
		RoomID: toPgtypeUUID(roomID),
		UserID: toPgtypeUUID(playerID),
	})
}

type RoomLeaderboardEntry struct {
	PlayerName string `json:"player_name"`
	Score      int32  `json:"score"`
	Place      int32  `json:"place"`
}

// calculateLeaderboard recalculates and updates player ranks in a transaction with row-level locking.
// This prevents race conditions across multiple instances by using SELECT FOR UPDATE in the SQL query.
//
// How it works:
// 1. Starts a database transaction
// 2. SELECT FOR UPDATE locks the rows being read
// 3. Calculates new rankings
// 4. Updates the rankings
// 5. Commits the transaction (releases locks)
//
// If Instance A and Instance B both try to calculate simultaneously:
// - Instance A acquires locks first
// - Instance B waits until Instance A commits
// - Instance B then calculates with the updated data
// This ensures leaderboard consistency across all instances.
func (r *RoomHub) calculateLeaderboard(ctx context.Context) error {
	// Lock to prevent concurrent calculations on the same instance
	// (reduces unnecessary database contention from this instance)
	r.leaderboardMu.Lock()
	defer r.leaderboardMu.Unlock()

	r.logger.Info("Starting leaderboard calculation for room", "room_id", r.RoomID)

	// Begin a transaction - this is required for SELECT FOR UPDATE to work
	tx, err := r.db.Begin(ctx)
	if err != nil {
		r.logger.Error("Failed to begin transaction for leaderboard calculation",
			"room_id", r.RoomID,
			"error", err)
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	// Ensure rollback if something goes wrong
	defer tx.Rollback(ctx)

	// Create a query runner that executes within this transaction
	qtx := r.queries.WithTx(tx)

	// Execute the leaderboard calculation within the transaction
	// The SQL query includes "FOR UPDATE" which locks the rows until we commit
	err = qtx.CalculateRoomLeaderboard(ctx, toPgtypeUUID(r.RoomID))
	if err != nil {
		r.logger.Error("Failed to calculate leaderboard within transaction",
			"room_id", r.RoomID,
			"error", err)
		return fmt.Errorf("failed to calculate leaderboard: %w", err)
	}

	// Commit the transaction - this releases the row locks
	err = tx.Commit(ctx)
	if err != nil {
		r.logger.Error("Failed to commit leaderboard transaction",
			"room_id", r.RoomID,
			"error", err)
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	r.logger.Info("Successfully calculated and committed leaderboard",
		"room_id", r.RoomID)

	return nil
}

func (r *RoomHub) getRoomLeaderboardEntries(ctx context.Context) ([]RoomLeaderboardEntry, error) {
	var entries []RoomLeaderboardEntry
	// get room players
	roomPlayers, err := r.queries.GetRoomPlayers(ctx, toPgtypeUUID(r.RoomID))
	if err != nil {
		r.logger.Error("Failed to get room players", "room_id", r.RoomID, "error", err)
		return nil, err
	}

	for _, rp := range roomPlayers {
		entries = append(entries, RoomLeaderboardEntry{
			PlayerName: rp.Username,
			Score:      rp.Score,
			Place:      rp.Place.Int32,
		})
	}

	return entries, nil
}

// Process the player joined event
func (r *RoomHub) processPlayerJoined(event events.PlayerJoined) error {
	ctx := context.Background()

	p, err := r.queries.GetRoomPlayer(ctx, store.GetRoomPlayerParams{
		RoomID: toPgtypeUUID(event.RoomID),
		UserID: toPgtypeUUID(event.PlayerID),
	})
	if err == pgx.ErrNoRows {
		r.logger.Info("Player not found in room", "room_id", event.RoomID, "player_id", event.PlayerID)
		s := rand.NewPCG(uint64(time.Now().UnixNano()), 0)
		rand := rand.New(s)
		fmt.Println(rand.IntN(100))

		if err := r.addPlayerToRoom(ctx, event.PlayerGuildID, event.RoomID, event.PlayerID, event.PlayerName); err != nil {
			r.logger.Error("Failed to add player to room", "room_id", event.RoomID, "player_id", event.PlayerID, "error", err)
			return err
		}
	}

	if p.State != store.RoomPlayerStatePresent {
		if _, err := r.queries.UpdateRoomPlayerState(ctx, store.UpdateRoomPlayerStateParams{
			RoomID: toPgtypeUUID(event.RoomID),
			UserID: toPgtypeUUID(event.PlayerID),
			State:  store.RoomPlayerStatePresent,
		}); err != nil {
			r.logger.Error("Failed to update room player state", "room_id", event.RoomID, "player_id", event.PlayerID, "error", err)
			return err
		}
	}

	r.logger.Info("player is not in room, adding to room...",
		"playerID", event.PlayerID,
		"room", event.RoomID)

	// Recalculate leaderboard after a player joins
	if err := r.calculateLeaderboard(ctx); err != nil {
		r.logger.Error("failed to calculate leaderboard after player joined", "error", err)
	}

	roomRoutingKey := fmt.Sprintf("room.%s", r.RoomID)

	r.logger.Info("player joined", "event", event)

	playerJoinedEvent := events.SseEvent{EventType: events.PLAYER_JOINED, Data: event}
	r.publishEvent(ctx, roomRoutingKey, playerJoinedEvent)

	entries, err := r.getRoomLeaderboardEntries(ctx)
	if err != nil {
		return err
	}

	leaderboardUpdatedEvent := events.SseEvent{
		EventType: events.LEADERBOARD_UPDATED,
		Data:      entries,
	}

	r.publishEvent(ctx, roomRoutingKey, leaderboardUpdatedEvent)

	// Send time remaining to the player who just joined
	// This is only sent on successful join
	remaining := event.EventEndTime.UTC().Sub(time.Now().UTC())
	if remaining > 0 {
		initialTime := map[string]any{
			"event_id":       event.EventID.String(),
			"end_date":       event.EventEndTime.Format(time.RFC3339),
			"seconds_left":   int64(remaining.Seconds()),
			"formatted_time": formatDuration(remaining),
			"server_time":    time.Now().Format(time.RFC3339),
		}

		// Send time remaining directly to the player who joined
		timeRemainingEvent := events.SseEvent{
			EventType: "initial_time",
			Data:      initialTime,
		}

		r.dispatchEventToPlayer(timeRemainingEvent, event.PlayerID)

		r.logger.Info("Sent time remaining to player after successful join",
			"player_id", event.PlayerID,
			"event_id", event.EventID,
			"seconds_left", int64(remaining.Seconds()))
	}

	return nil
}

func (r *RoomHub) processPlayerLeft(event events.PlayerLeft) error {
	ctx := context.Background()

	// Process the player left event
	data := fmt.Sprintf("playerId:%d,roomId:%d\n\n", event.PlayerID, r.RoomID)

	// change status to disconnect instead of remove
	_, err := r.queries.UpdateRoomPlayerState(ctx, store.UpdateRoomPlayerStateParams{
		RoomID: toPgtypeUUID(event.RoomID),
		UserID: toPgtypeUUID(event.PlayerID),
		State:  store.RoomPlayerStateDisconnected,
	})
	if err != nil {
		r.logger.Error("failed to update player state",
			"error", err,
			"player_id", event.PlayerID,
			"room_id", event.RoomID,
			"state", store.RoomPlayerStateDisconnected,
		)
	}

	// Recalculate leaderboard after a player leaves
	err = r.calculateLeaderboard(ctx)
	if err != nil {
		r.logger.Error("failed to calculate leaderboard after player left", "error", err)
	}

	playerLeft := events.SseEvent{
		EventType: events.PLAYER_LEFT,
		Data:      data,
	}

	go r.dispatchEvent(playerLeft)
	r.logger.Info("player left", "event", event)

	entries, err := r.getRoomLeaderboardEntries(ctx)
	if err != nil {
		r.logger.Error("failed to get room leaderboard entries", "error", err)
	}

	leaderboardUpdated := events.SseEvent{
		EventType: events.LEADERBOARD_UPDATED,
		Data:      entries,
	}

	go r.dispatchEvent(leaderboardUpdated)

	return nil
}

// TODO: Complete this
func (r *RoomHub) processRoomDeleted(event events.RoomDeleted) error {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	ctx := context.Background()

	// Process the room deleted event
	data := fmt.Sprintf("roomId:%d\n\n", r.RoomID)

	sseEvent := events.SseEvent{
		EventType: events.ROOM_DELETED,
		Data:      data,
	}

	err := r.queries.DeleteRoom(ctx, toPgtypeUUID(event.RoomID))
	if err != nil {
		r.logger.Error("failed to delete room from database", "error", err)
	}

	r.logger.Info("room deleted", "roomID", event.RoomID)

	go r.dispatchEvent(sseEvent)

	return nil
}

// processEventExpired handles event expiry notification to all players in the room.
// This is called when an event expires and needs to notify all connected players.
// Note: This function only dispatches locally to players on this instance.
// The EventExpired message is published to RabbitMQ by completeEvent() in EventHub,
// ensuring all instances receive and process it.
//
// P1-1: After notifying players, schedules room cleanup to free resources
func (r *RoomHub) processEventExpired(event events.EventExpired) error {
	r.logger.Info("Event expired, broadcasting to room players",
		"event_id", event.EventID,
		"room_id", r.RoomID)

	sseEvent := events.SseEvent{
		EventType: events.EVENT_EXPIRED,
		Data:      event,
	}

	// Broadcast to all connected players in this room on this instance
	r.Mu.RLock()
	playersNotified := 0
	for playerID, ch := range r.Listerners {
		select {
		case ch <- sseEvent:
			r.logger.Info("Sent event expired to player", "player_id", playerID)
			playersNotified++
		default:
			r.logger.Warn("Player channel full, could not send event expired",
				"player_id", playerID)
		}
	}
	r.Mu.RUnlock()

	r.logger.Info("Event expired broadcast complete",
		"room_id", r.RoomID,
		"players_notified", playersNotified)

	// P1-1: Schedule cleanup of this room after delay
	// Give players time to view final results before closing connections
	r.scheduleRoomCleanup(5 * time.Minute)

	return nil
}

// scheduleRoomCleanup schedules this room to be cleaned up after a delay.
// P1-1: This prevents memory leaks from completed event rooms.
//
// Flow:
// 1. Wait for delay (default 5 minutes) to give players time to view results
// 2. Close all SSE connections gracefully
// 3. Stop processing events
// 4. Mark room as ready for removal from EventHub
//
// The room will be fully removed by EventHub's inactive room cleanup routine.
func (r *RoomHub) scheduleRoomCleanup(delay time.Duration) {
	r.logger.Info("Scheduling room cleanup",
		"room_id", r.RoomID,
		"cleanup_delay", delay)

	// Schedule cleanup in background
	time.AfterFunc(delay, func() {
		r.logger.Info("Starting room cleanup",
			"room_id", r.RoomID)

		// Step 1: Close all player connections gracefully
		r.Mu.Lock()
		connectionsClosed := 0
		for playerID, ch := range r.Listerners {
			// Send final goodbye message before closing
			goodbyeEvent := events.SseEvent{
				EventType: events.EVENT_EXPIRED,
				Data: map[string]string{
					"message": "Event session ended. Connection will close.",
				},
			}

			select {
			case ch <- goodbyeEvent:
				r.logger.Info("Sent goodbye to player before cleanup",
					"player_id", playerID,
					"room_id", r.RoomID)
			default:
				r.logger.Warn("Could not send goodbye - channel full or closed",
					"player_id", playerID)
			}

			// Close the channel to disconnect the player
			close(ch)
			connectionsClosed++
		}

		// Clear the listeners map
		r.Listerners = make(map[uuid.UUID]chan<- events.SseEvent)
		r.Mu.Unlock()

		r.logger.Info("Closed all player connections",
			"room_id", r.RoomID,
			"connections_closed", connectionsClosed)

		// Step 2: Stop processing events by closing the Events channel
		// This will cause the RoomHub.Start() goroutine to exit
		close(r.Events)

		r.logger.Info("Room cleanup complete - ready for removal",
			"room_id", r.RoomID,
			"note", "will be removed by EventHub cleanup routine")
	})
}

func toPgtypeUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: id,
		Valid: true,
	}
}

// storeTestCaseToPbTestCase converts a store.TestCase to pb.TestCase
func storeTestCaseToPbTestCase(tc store.TestCase) *pb.TestCase {
	return &pb.TestCase{
		Input:          tc.Input,
		ExpectedOutput: tc.ExpectedOutput,
	}
}

// storeTestCasesToPbTestCases converts a slice of store.TestCase to a slice of pb.TestCase
func storeTestCasesToPbTestCases(testCases []store.TestCase) []*pb.TestCase {
	pbTestCases := make([]*pb.TestCase, len(testCases))
	for i, tc := range testCases {
		pbTestCases[i] = storeTestCaseToPbTestCase(tc)
	}
	return pbTestCases
}

// formatDuration formats a duration as HH:MM:SS
func formatDuration(d time.Duration) string {
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60

	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}
