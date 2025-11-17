package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
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
func (e *EventHub) ScheduleEventExpiry(eventID uuid.UUID, endDate time.Time) {
	duration := time.Until(endDate)

	if duration <= 0 {
		e.logger.Warn("Event end date is in the past, not scheduling timer",
			"event_id", eventID,
			"end_date", endDate,
			"now", time.Now())
		return
	}

	e.logger.Info("Scheduling event expiry timer",
		"event_id", eventID,
		"end_date", endDate,
		"duration", duration)

	// Schedule one-time timer
	time.AfterFunc(duration, func() {
		e.logger.Info("Event expiry timer fired", "event_id", eventID)
		e.completeEvent(eventID)
	})
}

// completeEvent atomically marks an event as completed and broadcasts the expiry message.
// This should only be called by the timer (ScheduleEventExpiry).
//
// Flow:
// 1. Atomically update event status: active → completed (only one instance succeeds)
// 2. Publish EventExpired to RabbitMQ fanout exchange
// 3. All instances receive the message and broadcast to their connected players
func (e *EventHub) completeEvent(eventID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	e.logger.Info("Attempting to complete event", "event_id", eventID)

	// Start a transaction to ensure atomicity
	tx, err := e.db.Begin(ctx)
	if err != nil {
		e.logger.Error("Failed to start transaction for event completion",
			"event_id", eventID,
			"error", err)
		return
	}
	defer tx.Rollback(ctx)

	qtx := e.queries.WithTx(tx)

	// Atomic update: only succeeds if event is 'active'
	// This prevents duplicate completion if multiple instances fire simultaneously
	rowsAffected, err := qtx.UpdateEventStatusToCompleted(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Error("Failed to execute update event status to completed",
			"event_id", eventID,
			"error", err)
		return
	}

	// Check if we actually updated the row (compare-and-swap success)
	if rowsAffected == 0 {
		// Another instance already completed this event
		e.logger.Info("Event already completed by another instance, skipping",
			"event_id", eventID)
		return
	}

	e.logger.Info("Successfully marked event as completed in database",
		"event_id", eventID,
		"rows_affected", rowsAffected)

	// Update all room player states for this event
	// present -> completed, disconnected -> left
	playerRowsAffected, err := qtx.UpdateRoomPlayerStatesOnEventComplete(ctx, toPgtypeUUID(eventID))
	if err != nil {
		e.logger.Error("Failed to update room player states on event complete",
			"event_id", eventID,
			"error", err)
		return
	}

	e.logger.Info("Successfully updated room player states",
		"event_id", eventID,
		"players_updated", playerRowsAffected)

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		e.logger.Error("Failed to commit event completion transaction",
			"event_id", eventID,
			"error", err)
		return
	}

	// Create EventExpired message
	eventExpired := events.EventExpired{
		EventID:     eventID,
		CompletedAt: time.Now(),
		Message:     "Event has ended. Thank you for participating!",
	}

	sseEvent := events.SseEvent{
		EventType: events.EVENT_EXPIRED,
		Data:      eventExpired,
	}

	// Publish to RabbitMQ so ALL instances receive the message
	payload, err := json.Marshal(sseEvent)
	if err != nil {
		e.logger.Error("Failed to marshal EventExpired for RabbitMQ",
			"event_id", eventID,
			"error", err)
		return
	}

	routingKey := fmt.Sprintf("event.%s", eventID)
	err = e.rabbitClient.Publish(ctx, rabbitmq.SseEventsExchange, routingKey, payload)
	if err != nil {
		e.logger.Error("Failed to publish EventExpired to RabbitMQ",
			"event_id", eventID,
			"error", err)
		return
	}

	e.logger.Info("Successfully published EventExpired to RabbitMQ",
		"event_id", eventID,
		"routing_key", routingKey)
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

	// Schedule event expiry timer if event is active (lazy-loading recovery)
	// This ensures that if a room is lazy-loaded on a new instance,
	// the expiry timer is still scheduled correctly
	event, err := h.queries.GetEventByID(ctx, pgtype.UUID{Bytes: eventID, Valid: true})
	if err == nil && event.Status == "active" {
		h.logger.Info("Lazy-loaded room for active event, scheduling expiry timer",
			"room_id", roomID,
			"event_id", eventID,
			"end_date", event.EndDate.Time)
		h.ScheduleEventExpiry(eventID, event.EndDate.Time)
	}

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

// combineCodeWithTemplate combined the userCode and templateFunction at placeHolder
func combineCodeWithTemplate(templateCode, userCode, placeHolder string) string {
	finalCode := strings.Replace(templateCode, placeHolder, userCode, 1)
	return finalCode
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
	// Double-check time validation to handle clock skew between instances
	if time.Now().After(eventDetails.EndDate.Time) {
		r.logger.Warn("Rejecting solution result - event has expired",
			"event_id", r.EventID,
			"end_date", eventDetails.EndDate.Time,
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

		return fmt.Errorf("event has expired at %s (now: %s)", eventDetails.EndDate.Time, time.Now())
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
func (r *RoomHub) addPlayerToRoom(ctx context.Context, roomID, playerID uuid.UUID, playerName string) error {
	createParams := store.CreateRoomPlayerParams{
		RoomID:   toPgtypeUUID(roomID),
		UserID:   toPgtypeUUID(playerID),
		Username: playerName,
		State:    store.RoomPlayerStatePresent,
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

	eventDetails, err := r.queries.GetEventByID(ctx, toPgtypeUUID(event.EventID))
	if err != nil {
		r.logger.Error("Failed to get event details", "event_id", event.EventID, "error", err)
		return err
	}

	if eventDetails.EndDate.Time.Before(time.Now()) {
		sseEvent := events.SseEvent{
			EventType: events.EVENT_ENDED,
		}

		r.dispatchEventToPlayer(sseEvent, event.PlayerID)
		return err
	}

	p, err := r.queries.GetRoomPlayer(ctx, store.GetRoomPlayerParams{
		RoomID: toPgtypeUUID(event.RoomID),
		UserID: toPgtypeUUID(event.PlayerID),
	})
	if err == pgx.ErrNoRows {
		r.logger.Info("Player not found in room", "room_id", event.RoomID, "player_id", event.PlayerID)
		userProfile, err := r.userClient.GetUserProfileByAuthId(ctx, &pb.GetUserProfileByAuthIdRequest{
			AuthUserId: event.PlayerID.String(),
		})
		if err != nil {
			r.logger.Error("Failed to get player username", "player_id", event.PlayerID, "error", err)
			return err
		}

		r.logger.Info("retrieved user profile successfully", "player_id", event.PlayerID, "username", userProfile.Username)
		if err := r.addPlayerToRoom(ctx, event.RoomID, event.PlayerID, userProfile.Username); err != nil {
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
	remaining := time.Until(eventDetails.EndDate.Time)
	if remaining > 0 {
		initialTime := map[string]any{
			"event_id":       event.EventID.String(),
			"end_date":       eventDetails.EndDate.Time.Format(time.RFC3339),
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

	return nil
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
