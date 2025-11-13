package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/client/executor"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/client/rabbitmq"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/events"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/store"
	pb "github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/protos"
	"github.com/google/uuid"
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
	db            *pgxpool.Pool // Database pool for transactions
	Rooms         map[uuid.UUID]*RoomHub // roomID -> roomManager
	Mu            sync.RWMutex
	leaderboardMu sync.Mutex // Protects leaderboard calculation

	GuildUpdateChan  chan uuid.UUID
	EventListeners   map[uuid.UUID]map[uuid.UUID]chan<- events.SseEvent // eventID -> map[listenerID] channel
	EventListenersMu sync.RWMutex                                       // A dedicated mutex for the event

	rabbitClient   *rabbitmq.RabbitMQClient
	executorClient *executor.Client
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

	// Track last activity for cleanup purposes
	lastActivity time.Time
	activityMu   sync.RWMutex
}

func NewEventHub(db *pgxpool.Pool, queries *store.Queries, logger *slog.Logger, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client) *EventHub {
	e := EventHub{
		logger:          logger,
		queries:         queries,
		db:              db,
		Rooms:           make(map[uuid.UUID]*RoomHub),
		GuildUpdateChan: make(chan uuid.UUID, 100), // Buffered channel
		EventListeners:  make(map[uuid.UUID]map[uuid.UUID]chan<- events.SseEvent),
		rabbitClient:    rabbitClient,
		executorClient:  executorClient,
	}

	go e.listenForAMQPEvents() // Start the central RabbitMQ listener

	supabaseEventID, _ := uuid.Parse("e88e5761-e0fa-409d-baad-057edad1496a")
	e.EventListeners[supabaseEventID] = make(map[uuid.UUID]chan<- events.SseEvent)

	// --------- remove on production ---------
	beginnerArea, err := uuid.Parse("4d5e6f7a-8b9c-0d1e-2f3a-4b5c6d7e8f90")
	if err != nil {
		panic(err)
	}
	advancedLobby, err := uuid.Parse("5e6f7a8b-9c0d-1e2f-3a4b-5c6d7e8f90a1")
	if err != nil {
		panic(err)
	}

	// remove on production
	e.CreateRoom(supabaseEventID, beginnerArea, queries)
	e.CreateRoom(supabaseEventID, advancedLobby, queries)

	go e.Start()

	for _, r := range e.Rooms {
		go r.Start()
	}
	// --------- remove on production ---------

	return &e
}

func newRoomHub(eventID, roomId uuid.UUID, db *pgxpool.Pool, queries *store.Queries, logger *slog.Logger, guildUpdateChan chan<- uuid.UUID, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client) *RoomHub {
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
			e.dispatchEventToEvent(entityID, sseEvent)
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
	)

	// Add to in-memory map
	h.Rooms[roomID] = newRoomHub

	// Start the RoomHub's event processing goroutine
	go newRoomHub.Start()

	h.logger.Info("Successfully created and started RoomHub", "room_id", roomID)

	return newRoomHub
}

func (e *EventHub) CreateRoom(eventID, roomID uuid.UUID, queries *store.Queries) *RoomHub {
	r := newRoomHub(eventID, roomID, e.db, queries, e.logger, e.GuildUpdateChan, e.rabbitClient, e.executorClient)
	e.Mu.Lock()
	e.Rooms[roomID] = r
	e.Mu.Unlock()
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

	// TODO: Send grpc request to executor service
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

	solutionResult := generateSolutionResult(event, result)
	r.Events <- solutionResult

	return nil
}

// generateSolutionResult creates a SolutionResult from the execution response
func generateSolutionResult(solutionSubmitted events.SolutionSubmitted, result *pb.ExecuteResponse) events.SolutionResult {
	solutionResult := events.SolutionResult{
		SolutionSubmitted: solutionSubmitted,
		Score:             50, // TODO: Calculate actual score based on test cases passed
		Status:            events.Accepted,
		Message:           "Solution is correct!",
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

	if event.Status != events.Accepted {
		r.logger.Info("solution failed", "event", event)
		sseEvent := events.SseEvent{
			EventType: events.WRONG_SOLUTION_SUBMITTED,
			Data:      fmt.Sprintf("status:%v,message:%v", event.Status, event.Message),
		}

		r.publishEvent(ctx, roomRoutingKey, sseEvent)

		// _, err := r.queries.UpdateSubmissionStatus(ctx, store.UpdateSubmissionStatusParams{
		// 	ID:     toPgtypeUUID(event.SolutionSubmitted.SubmissionID),
		// 	Status: store.SubmissionStatusWrongAnswer,
		// })

		// if err != nil {
		// 	r.logger.Error("failed to update submission status to wrong", "error", err,
		// 		"submission_id", event.SolutionSubmitted.SubmissionID)
		// 	return err
		// }

		// go r.dispatchEventToPlayer(sseEvent, event.SolutionSubmitted.PlayerID)

		return nil
	}

	err := r.queries.AddRoomPlayerScore(ctx, store.AddRoomPlayerScoreParams{
		PointsToAdd: int32(event.Score),
		UserID:      toPgtypeUUID(event.SolutionSubmitted.PlayerID),
		RoomID:      toPgtypeUUID(event.SolutionSubmitted.RoomID),
	})

	if err != nil {
		r.logger.Error("failed to add score",
			"err", err)
		return err
	}

	// Recalculate leaderboard after score update
	if err := r.calculateLeaderboard(ctx); err != nil {
		r.logger.Error("failed to calculate leaderboard after solution result", "error", err)
		// non-fatal, but should be monitored
	}

	roomEntries, err := r.getRoomLeaderboardEntries(ctx)
	if err != nil {
		return err
	}

	roomLeaderboardEvent := events.SseEvent{EventType: events.LEADERBOARD_UPDATED, Data: roomEntries}
	r.publishEvent(ctx, roomRoutingKey, roomLeaderboardEvent)

	guildEntries, err := r.queries.GetGuildLeaderboardByEvent(ctx, toPgtypeUUID(r.EventID))
	if err == nil {
		eventRoutingKey := fmt.Sprintf("event.%s", r.EventID)
		guildLeaderboardEvent := events.SseEvent{EventType: events.GUILD_LEADERBOARD_UPDATED, Data: guildEntries}
		r.publishEvent(ctx, eventRoutingKey, guildLeaderboardEvent)
	} else {
		r.logger.Error("failed to get guild leaderboard for event", "error", err, "event_id", r.EventID)
	}

	// ... update submission status in DB
	return nil
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

func (r *RoomHub) processPlayerJoined(event events.PlayerJoined) error {
	// Process the player joined event
	ctx := context.Background()
	// playerID is passed by the event
	// playerID is parsed from the jwt token
	if ok := r.playerInRoom(ctx, event.RoomID, event.PlayerID); !ok {
		r.logger.Info("player is not in room, adding to room...",
			"playerID", event.PlayerID,
			"room", event.RoomID)

		playerName := "grpc_called"

		err := r.addPlayerToRoom(ctx, event.RoomID, event.PlayerID, playerName)
		if err != nil {
			r.logger.Error("failed to add player to room", "error", err)
			return err
		}
	}

	// Recalculate leaderboard after a player joins
	if err := r.calculateLeaderboard(ctx); err != nil {
		r.logger.Error("failed to calculate leaderboard after player joined", "error", err)
	}

	roomRoutingKey := fmt.Sprintf("room.%s", r.RoomID)

	r.logger.Info("player joined", "event", event)

	playerJoinedEvent := events.SseEvent{EventType: events.PLAYER_JOINED, Data: event}
	r.publishEvent(ctx, roomRoutingKey, playerJoinedEvent)

	// data := fmt.Sprintf("playerId:%d,roomId:%d\n\n", event.PlayerID, r.RoomID)

	// playerJoined := events.SseEvent{
	// 	EventType: events.PLAYER_JOINED,
	// 	Data:      data,
	// }

	// go r.dispatchEvent(playerJoined)

	entries, err := r.getRoomLeaderboardEntries(ctx)
	if err != nil {
		return err
	}

	leaderboardUpdatedEvent := events.SseEvent{
		EventType: events.LEADERBOARD_UPDATED,
		Data:      entries,
	}

	r.publishEvent(ctx, roomRoutingKey, leaderboardUpdatedEvent)

	// go r.dispatchEvent(leaderboardUpdated)

	return nil
}

func (r *RoomHub) processPlayerLeft(event events.PlayerLeft) error {
	ctx := context.Background()

	// Process the player left event
	data := fmt.Sprintf("playerId:%d,roomId:%d\n\n", event.PlayerID, r.RoomID)

	err := r.removePlayerFromRoom(ctx, event.RoomID, event.PlayerID)
	if err != nil {
		r.logger.Error("failed to remove player from room", "error", err)
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
