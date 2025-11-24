package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/events"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
	pb "github.com/FA25SE050-RogueLearn/RogueLearn.Event/protos"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrEventNotFound          = errors.New("Event not found.")
	ErrEventNotStarted        = errors.New("Event hasn't started yet.")
	ErrPlayerNotInGuild       = errors.New("Player is not in any Guild.")
	ErrGuildNotAssignedToRoom = errors.New("Guild is not assigned to this room.")
	ErrEventEnded             = errors.New("Event has ended.")
	ErrUserNotInSelectedList  = errors.New("User is not in the selected list.")
	ErrInvalidRoom            = errors.New("Room is invalid")
	ErrRoomIsFullOfConnection = errors.New("Room is full - too many connections")
	ErrUserAlreadyInRoom      = errors.New("User is already in the room")
)

// SSE Event Handler for room's leaderboard
// Send the Room events to connected players
func (hr *HandlerRepo) JoinRoomHandler(w http.ResponseWriter, r *http.Request) {
	authToken := r.URL.Query().Get("auth_token")
	if authToken == "" {
		hr.unauthorized(w, r)
		return
	}

	userClaims, err := hr.jwtParser.GetUserClaimsFromToken(authToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	userIDStr := userClaims.GetUserID()
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		hr.badRequest(w, r, err)
		return
	}

	hr.logger.Info("player join requested",
		"user_id", userID)

	eventID, roomID, err := getRequestEventIDAndRoomID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	event, err := hr.queries.GetEventByID(r.Context(), toPgtypeUUID(eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		hr.logger.Debug("Event not found")
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get event")
		hr.serverError(w, r, err)
		return
	}

	// event start time and end time validation
	if time.Now().UTC().Before(event.StartedDate.Time.UTC()) {
		hr.logger.Debug("Event hasn't started yet")
		hr.badRequest(w, r, ErrEventNotStarted)
		return
	}

	// event end time validation
	if time.Now().UTC().After(event.EndDate.Time.UTC()) {
		hr.logger.Debug("Event has ended")
		hr.badRequest(w, r, ErrEventEnded)
		return
	}

	// check if player has permission to join this room
	// First, get the user's guild via gRPC
	// Use a separate context with timeout for gRPC calls (not the SSE connection context)
	grpcCtx, grpcCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer grpcCancel()

	userProfile, err := hr.userClient.GetUserProfileByAuthId(grpcCtx, &pb.GetUserProfileByAuthIdRequest{
		AuthUserId: userID.String(),
	})
	if err != nil {
		hr.logger.Error("failed to get user profile", "user_id", userID, "error", err)
		hr.serverError(w, r, err)
		return
	}

	guild, err := hr.userClient.GetMyGuild(grpcCtx, &pb.GetMyGuildRequest{
		AuthUserId: userProfile.AuthUserId,
	})
	if err != nil {
		hr.logger.Error("failed to get user guild", "user_id", userID, "error", err)
		hr.serverError(w, r, err)
		return
	}

	if guild == nil {
		hr.logger.Warn("user not in a guild", "user_id", userID)
		hr.badRequest(w, r, ErrPlayerNotInGuild)
		return
	}

	guildID, err := uuid.Parse(guild.Id)
	if err != nil {
		hr.logger.Error("invalid guild_id format", "guild_id", guild.Id, "error", err)
		http.Error(w, "invalid guild ID", http.StatusInternalServerError)
		return
	}

	// Validate that the user's guild is assigned to this room
	_, err = hr.queries.ValidateGuildRoomAssignment(r.Context(), store.ValidateGuildRoomAssignmentParams{
		EventID: toPgtypeUUID(eventID),
		GuildID: toPgtypeUUID(guildID),
		RoomID:  toPgtypeUUID(roomID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		hr.logger.Warn("guild not assigned to room",
			"user_id", userID,
			"guild_id", guildID,
			"room_id", roomID,
			"event_id", eventID)
		hr.badRequest(w, r, ErrGuildNotAssignedToRoom)
		return
	} else if err != nil {
		hr.logger.Error("failed to validate guild room assignment", "error", err)
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("guild room assignment validated",
		"user_id", userID,
		"guild_id", guildID,
		"room_id", roomID,
		"event_id", eventID)

	// Validate that the user is in the selected members list for this event
	_, err = hr.queries.CheckIfUserSelectedForEvent(r.Context(), store.CheckIfUserSelectedForEventParams{
		EventID: toPgtypeUUID(eventID),
		GuildID: toPgtypeUUID(guildID),
		UserID:  toPgtypeUUID(userID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		hr.logger.Warn("user not selected to compete in this event",
			"user_id", userID,
			"guild_id", guildID,
			"event_id", eventID)
		hr.badRequest(w, r, ErrUserNotInSelectedList)
		return
	} else if err != nil {
		hr.logger.Error("failed to validate user selection for event", "error", err)
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("user selection validated - player is authorized to join",
		"user_id", userID,
		"guild_id", guildID,
		"event_id", eventID)

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
		hr.serverError(w, r, ErrInvalidRoom)
		return
	}

	// listen for incoming SseEvents
	listen := make(chan events.SseEvent, 50) // Buffered channel to prevent blocking during burst events

	// Properly lock when modifying listeners
	roomHub.Mu.Lock()
	if roomHub.Listerners == nil {
		roomHub.Listerners = make(map[uuid.UUID]chan<- events.SseEvent)
	} else { // room hub listener is not nill
		if _, ok := roomHub.Listerners[userID]; ok {
			roomHub.Mu.Unlock()
			hr.logger.Debug("User already in room",
				"user_id", userIDStr,
				"room_id", roomID.String())
			hr.conflict(w, r, ErrUserAlreadyInRoom)
			return
		}
	}

	// Check connection limit to prevent DoS attacks
	const maxConnectionsPerRoom = 100
	if len(roomHub.Listerners) >= maxConnectionsPerRoom {
		roomHub.Mu.Unlock()
		hr.logger.Warn("room connection limit reached",
			"room_id", roomID,
			"current_connections", len(roomHub.Listerners),
			"max_connections", maxConnectionsPerRoom)
		hr.badRequest(w, r, ErrRoomIsFullOfConnection)
		return
	}

	roomHub.Listerners[userID] = listen
	roomHub.Mu.Unlock()

	defer hr.logger.Info("SSE connection closed", "userID", userID, "room_id", roomID)
	defer close(listen)
	defer func() {
		roomHub.Mu.Lock()
		delete(roomHub.Listerners, userID)
		roomHub.Mu.Unlock()
		go func() {
			roomHub.Events <- events.PlayerLeft{PlayerID: userID, RoomID: roomID}
		}()
	}()

	hr.logger.Info("SSE connection established", "userID", userID, "room_id", roomID)

	// player joined event - time remaining will be sent after successful join
	roomHub.Events <- events.PlayerJoined{
		PlayerID:      userID,
		RoomID:        roomID,
		EventID:       eventID,
		PlayerName:    userProfile.Username,
		PlayerGuildID: guildID,
		EventEndTime:  event.EndDate.Time,
	}

	for {
		select {
		case <-r.Context().Done():
			hr.logger.Info("SSE client disconnected", "userID", userID, "room_id", roomID)
			return
		case event, ok := <-listen:
			if !ok {
				hr.logger.Info("SSE client disconnected", "userID", userID, "room_id", roomID)
				return
			}

			hr.logger.Info("Sending event to player's client", "userID", userID, "event", event, "room_id", roomID)
			data, err := json.Marshal(event)
			if err != nil {
				hr.logger.Error("failed to marshal SSE event", "error", err, "userID", userID)
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

// formatDuration formats a duration as HH:MM:SS
func formatDuration(d time.Duration) string {
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60

	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}
