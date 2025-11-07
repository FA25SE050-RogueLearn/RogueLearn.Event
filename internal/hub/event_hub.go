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
	Mu            sync.RWMutex // Protects Listerners map
	leaderboardMu sync.Mutex   // Protects leaderboard calculation

	guildUpdateChan chan<- uuid.UUID

	rabbitClient   *rabbitmq.RabbitMQClient
	executorClient *executor.Client
}

func NewEventHub(queries *store.Queries, logger *slog.Logger, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client) *EventHub {
	e := EventHub{
		logger:          logger,
		queries:         queries,
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

func newRoomHub(eventID, roomId uuid.UUID, queries *store.Queries, logger *slog.Logger, guildUpdateChan chan<- uuid.UUID, rabbitClient *rabbitmq.RabbitMQClient, executorClient *executor.Client) *RoomHub {
	return &RoomHub{
		RoomID:          roomId,
		EventID:         eventID, // Set the eventID
		Events:          make(chan any, 10),
		Listerners:      make(map[uuid.UUID]chan<- events.SseEvent),
		logger:          logger,
		queries:         queries,
		Mu:              sync.RWMutex{},
		leaderboardMu:   sync.Mutex{},
		guildUpdateChan: guildUpdateChan, // Set the notification channel
		rabbitClient:    rabbitClient,
		executorClient:  executorClient,
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
			if room := e.GetRoomById(entityID); room != nil {
				room.dispatchEvent(sseEvent)
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

func (h *EventHub) GetRoomById(roomID uuid.UUID) *RoomHub {
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	return h.Rooms[roomID]
}

func (e *EventHub) CreateRoom(eventID, roomID uuid.UUID, queries *store.Queries) *RoomHub {
	r := newRoomHub(eventID, roomID, queries, e.logger, e.GuildUpdateChan, e.rabbitClient, e.executorClient)
	e.Mu.Lock()
	e.Rooms[roomID] = r
	e.Mu.Unlock()
	go r.Start() // Start RoomHub
	return r
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

// calculateLeaderboard recalculates and updates player ranks in a single, atomic, and concurrency-safe operation.
func (r *RoomHub) calculateLeaderboard(ctx context.Context) error {
	// Lock to prevent concurrent calculations for the same room, which could cause deadlocks or race conditions.
	r.leaderboardMu.Lock()
	defer r.leaderboardMu.Unlock()

	r.logger.Info("Starting leaderboard calculation for room", "room_id", r.RoomID)

	err := r.queries.CalculateRoomLeaderboard(ctx, toPgtypeUUID(r.RoomID))
	if err != nil {
		r.logger.Error("Failed to update player ranks via single query", "room_id", r.RoomID, "error", err)
		return err
	}

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
