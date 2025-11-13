package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/events"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// SSE Event Handler for room's leaderboard
// Send the Room events to connected players
func (hr *HandlerRepo) JoinRoomHandler(w http.ResponseWriter, r *http.Request) {
	// we will get the playerID through request
	eventID, roomID, err := getRequestEventIDAndRoomID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	hr.logger.Info("joined to the room",
		"event_id", eventID,
		"room_id", roomID)

	// get from the passed context in production stage, we will get from query in dev stage
	connectedPlayerIDStr := r.URL.Query().Get("connected_player_id")
	connectedPlayerID, err := uuid.Parse(connectedPlayerIDStr)
	if err != nil {
		hr.logger.Error("failed to parse connectedPlayerID",
			"err", err)
		hr.badRequest(w, r, err)
		return
	}

	hr.logger.Info("player join requested",
		"connected_player_id", connectedPlayerID)

	// Set http headers required for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")

	// Get or create the room manager for the requested room.
	// This supports lazy-loading: if the room exists in DB but not in memory,
	// it will be loaded automatically. This is crucial for multi-instance deployments.
	roomHub := hr.eventHub.GetOrCreateRoomHub(r.Context(), roomID)
	if roomHub == nil {
		http.Error(w, "room not found or not active", http.StatusNotFound)
		return
	}

	// listen for incoming SseEvents
	listen := make(chan events.SseEvent) // Add buffer to prevent blocking

	// Properly lock when modifying listeners
	roomHub.Mu.Lock()
	if roomHub.Listerners == nil {
		roomHub.Listerners = make(map[uuid.UUID]chan<- events.SseEvent)
	}
	roomHub.Listerners[connectedPlayerID] = listen
	roomHub.Mu.Unlock()

	defer hr.logger.Info("SSE connection closed", "connected_player_id", connectedPlayerID, "room_id", roomID)
	defer close(listen)
	defer func() {
		roomHub.Mu.Lock()
		delete(roomHub.Listerners, connectedPlayerID)
		roomHub.Mu.Unlock()
		go func() {
			roomHub.Events <- events.PlayerLeft{PlayerID: connectedPlayerID, RoomID: roomID}
		}()
	}()

	hr.logger.Info("SSE connection established", "connected_player_id", connectedPlayerID, "room_id", roomID)

	// player joined event
	roomHub.Events <- events.PlayerJoined{PlayerID: connectedPlayerID, RoomID: roomID}

	for {
		select {
		case <-r.Context().Done():
			hr.logger.Info("SSE client disconnected", "connected_player_id", connectedPlayerID, "room_id", roomID)
			// player left event
			return
		case event, ok := <-listen:
			if !ok {
				hr.logger.Info("SSE client disconnected", "connected_player_id", connectedPlayerID, "room_id", roomID)
				return
			}

			hr.logger.Info("Sending event to player's client", "connected_player_id", connectedPlayerID, "event", event, "room_id", roomID)
			data, err := json.Marshal(event)
			if err != nil {
				hr.logger.Error("failed to marshal SSE event", "error", err, "connected_player_id", connectedPlayerID)
				return // Client is likely gone, so exit
			}

			if event.EventType != "" {
				fmt.Fprintf(w, "event: %s\n", event.EventType)
			}

			fmt.Fprintf(w, "data: %s\n\n", string(data))

			w.(http.Flusher).Flush()
		}
	}
}

// SSE Event Handler for event's leaderboards
// Send the Event's changes of events to connected users
// SpectateEventHandler handles SSE connections for the event-wide guild leaderboard.
func (hr *HandlerRepo) SpectateEventHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Create a unique ID for this listener and a channel for events
	clientID := uuid.New()
	listen := make(chan events.SseEvent, 5)

	// Add listener to the main EventHub
	hr.eventHub.EventListenersMu.Lock()
	if hr.eventHub.EventListeners[eventID] == nil {
		hr.eventHub.EventListeners[eventID] = make(map[uuid.UUID]chan<- events.SseEvent)
	}

	hr.eventHub.EventListeners[eventID][clientID] = listen
	hr.eventHub.EventListenersMu.Unlock()

	// Defer cleanup to remove the listener when the connection closes
	defer func() {
		hr.eventHub.EventListenersMu.Lock()
		if eventListeners, ok := hr.eventHub.EventListeners[eventID]; ok {
			delete(eventListeners, clientID)
			if len(eventListeners) == 0 {
				delete(hr.eventHub.EventListeners, eventID)
			}
		}
		hr.eventHub.EventListenersMu.Unlock()
		close(listen)
		hr.logger.Info("Guild leaderboard listener disconnected", "event_id", eventID, "client_id", clientID)
	}()

	hr.logger.Info("Guild leaderboard listener connected", "event_id", eventID, "client_id", clientID)

	// --- Send initial leaderboard state ---
	// So the user sees data immediately upon connecting
	initialEntries, err := hr.queries.GetGuildLeaderboardByEvent(r.Context(), toPgtypeUUID(eventID))
	if err == nil {
		initialEvent := events.SseEvent{
			EventType: events.GUILD_LEADERBOARD_UPDATED,
			Data:      initialEntries,
		}

		data, _ := json.Marshal(initialEvent)
		fmt.Fprintf(w, "event: %s\n", initialEvent.EventType)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		w.(http.Flusher).Flush()
	}
	// -----------------------------------

	// Listen for context cancellation or incoming events
	for {
		select {
		case <-r.Context().Done():
			return // Client disconnected
		case event := <-listen:
			data, err := json.Marshal(event)
			if err != nil {
				hr.logger.Error("failed to marshal guild SSE event", "error", err)
				return
			}
			fmt.Fprintf(w, "event: %s\n", event.EventType)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			w.(http.Flusher).Flush()
		}
	}
}

// getRequestPlayerIdAndRoomId extract player_id and room_id from query params
func getRequestEventIDAndRoomID(r *http.Request) (eventID, roomID uuid.UUID, err error) {
	eventIDStr := chi.URLParam(r, "event_id")
	roomIDStr := chi.URLParam(r, "room_id")
	eventIDUID, err := uuid.Parse(eventIDStr)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, err
	}

	roomIDUID, err := uuid.Parse(roomIDStr)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, err
	}

	return eventIDUID, roomIDUID, nil
}

// getRequestPlayerIdAndRoomId extract player_id and room_id from query params
func getRequestPlayerIDAndEventID(r *http.Request) (playerID, eventID uuid.UUID, err error) {
	playerIDStr := r.URL.Query().Get("player_id")
	eventIDStr := r.URL.Query().Get("event_id")
	playerIDUID, err := uuid.Parse(playerIDStr)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, err
	}

	eventIDUID, err := uuid.Parse(eventIDStr)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, err
	}

	return playerIDUID, eventIDUID, nil
}
