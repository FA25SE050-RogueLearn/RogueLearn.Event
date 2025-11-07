package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/request"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/response"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (hr *HandlerRepo) GetEventsHandler(w http.ResponseWriter, r *http.Request) {
	// For now, no pagination.
	// In the future, we can add helper functions to parse query params for pagination.
	params := store.GetEventsParams{
		Limit:  10,
		Offset: 0,
	}

	events, err := hr.queries.GetEvents(r.Context(), params)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    events,
		Success: true,
		Msg:     "Events retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

type EventCreationRequest struct {
	RequesterGuildID  string               `json:"requester_guild_id"`
	EventType         string               `json:"event_type"`
	Title             string               `json:"title"`
	Description       string               `json:"description"`
	ProposedStartDate time.Time            `json:"proposed_start_date"`
	ProposedEndDate   time.Time            `json:"proposed_end_date"`
	Participation     ParticipationDetails `json:"participation"`
	RoomConfiguration RoomConfiguration    `json:"room_configuration"`
	EventSpecifics    *EventSpecifics      `json:"event_specifics,omitempty"`
	Notes             string               `json:"notes"`
}

// ParticipationDetails defines the rules for who and how many can join the event.
type ParticipationDetails struct {
	MaxGuilds          int `json:"max_guilds"`
	MaxPlayersPerGuild int `json:"max_players_per_guild"`
}

// RoomConfiguration specifies how rooms should be set up for the event.
type RoomConfiguration struct {
	NumberOfRooms    int    `json:"number_of_rooms"`
	GuildsPerRoom    int    `json:"guilds_per_room"`
	RoomNamingPrefix string `json:"room_naming_prefix"`
}

// EventSpecifics contains details that vary depending on the event type.
type EventSpecifics struct {
	CodeBattle *CodeBattleDetails `json:"code_battle,omitempty"`
}

// CodeBattleDetails holds the specific requirements for a "code_battle" event.
type CodeBattleDetails struct {
	Topics      []uuid.UUID              `json:"topics"` // problem tag id
	Distrbution []DifficultyDistribution `json:"distribution"`
}

type DifficultyDistribution struct {
	NumberOfProblems int `json:"number_of_problems"`
	Difficulty       int `json:"difficulty"` // 1 -> 3
	Score            int `json:"score"`      // score of each problem
}

type ProcessRequestPayload struct {
	Action          string `json:"action"` // "approve" or "decline"
	RejectionReason string `json:"rejection_reason,omitempty"`
}

// CreateEventHandler handles the submission of a new event creation request.
// It no longer creates an event directly.
func (hr *HandlerRepo) CreateEventHandler(w http.ResponseWriter, r *http.Request) {
	var req EventCreationRequest
	err := request.DecodeJSON(w, r, &req)
	if err != nil {
		hr.badRequest(w, r, err)
		return
	}

	// --- Basic Validation ---
	if req.Title == "" {
		hr.badRequest(w, r, errors.New("event title cannot be empty"))
		return
	}
	if req.ProposedStartDate.After(req.ProposedEndDate) {
		hr.badRequest(w, r, errors.New("start date must be before end date"))
		return
	}
	requesterGuildID, err := uuid.Parse(req.RequesterGuildID)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid requester guild ID format"))
		return
	}

	// Marshal JSONB fields
	participationDetailsJSON, err := json.Marshal(req.Participation)
	if err != nil {
		hr.serverError(w, r, fmt.Errorf("failed to marshal participation details: %w", err))
		return
	}

	roomConfigJSON, err := json.Marshal(req.RoomConfiguration)
	if err != nil {
		hr.serverError(w, r, fmt.Errorf("failed to marshal room configuration: %w", err))
		return
	}

	var eventSpecificsJSON []byte
	if req.EventSpecifics != nil {
		eventSpecificsJSON, err = json.Marshal(req.EventSpecifics)
		if err != nil {
			hr.serverError(w, r, fmt.Errorf("failed to marshal event specifics: %w", err))
			return
		}
	}

	// --- Database Insertion ---
	params := store.CreateEventRequestParams{
		RequesterGuildID:     toPgtypeUUID(requesterGuildID),
		EventType:            store.EventType(req.EventType),
		Title:                req.Title,
		Description:          req.Description,
		ProposedStartDate:    pgtype.Timestamptz{Time: req.ProposedStartDate, Valid: true},
		ProposedEndDate:      pgtype.Timestamptz{Time: req.ProposedEndDate, Valid: true},
		Notes:                pgtype.Text{String: req.Notes, Valid: req.Notes != ""},
		ParticipationDetails: participationDetailsJSON,
		RoomConfiguration:    roomConfigJSON,
		EventSpecifics:       eventSpecificsJSON,
	}

	eventRequest, err := hr.queries.CreateEventRequest(r.Context(), params)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("event creation request submitted", "request_id", eventRequest.ID.Bytes, "title", eventRequest.Title)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusAccepted,
		Data:    eventRequest,
		Success: true,
		Msg:     "Event creation request submitted successfully. It is now pending approval.",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

// GetMyEventRequestsHandler fetches a list of event requests submitted by a specific guild.
func (hr *HandlerRepo) GetMyEventRequestsHandler(w http.ResponseWriter, r *http.Request) {
	// In a real application, this would come from JWT claims or session.
	// For this example, we'll use a query parameter.
	guildIDStr := r.URL.Query().Get("guild_id")
	if guildIDStr == "" {
		hr.badRequest(w, r, errors.New("guild_id query parameter is required"))
		return
	}

	guildID, err := uuid.Parse(guildIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid guild_id format"))
		return
	}

	params := store.ListEventRequestsByGuildParams{
		RequesterGuildID: toPgtypeUUID(guildID),
		Limit:            20,
		Offset:           0,
	}

	requests, err := hr.queries.ListEventRequestsByGuild(r.Context(), params)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	responseDTOs := make([]EventRequestResponse, 0, len(requests))
	for _, req := range requests {
		dto, err := toEventRequestResponse(req)
		if err != nil {
			// Log the error but don't fail the whole request
			hr.logger.Error("failed to convert event request to DTO", "error", err, "request_id", req.ID.Bytes)
			continue
		}
		responseDTOs = append(responseDTOs, dto)
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    responseDTOs,
		Success: true,
		Msg:     "Your event requests retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

func (hr *HandlerRepo) GetEventRoomsHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	pgEventID := pgtype.UUID{Bytes: eventID, Valid: true}

	rooms, err := hr.queries.GetRoomsByEvent(r.Context(), pgEventID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hr.notFound(w, r)
		} else {
			hr.serverError(w, r, err)
		}
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    rooms,
		Success: true,
		Msg:     "Rooms for the event retrieved successfully",
	})

	if err != nil {
		hr.serverError(w, r, err)
	}
}

type GuildEventRegisterRequest struct {
	GuildID uuid.UUID
	EventID uuid.UUID
}

// RegisterGuildToEventHandler will add the requested Guild to the Event Participant and
// create a record in guild_leaderboard_entries
func (hr *HandlerRepo) RegisterGuildToEventHandler(w http.ResponseWriter, r *http.Request) {
	guildIDStr := chi.URLParam(r, "guild_id")
	guildID, err := uuid.Parse(guildIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid guild ID format"))
		return
	}

	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	// room_id will be known later
	_, err = hr.queries.CreateEventGuildParticipant(r.Context(), store.CreateEventGuildParticipantParams{
		EventID: toPgtypeUUID(eventID),
		GuildID: toPgtypeUUID(guildID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hr.notFound(w, r)
		} else {
			hr.serverError(w, r, err)
		}
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    nil,
		Success: true,
		Msg:     "Guild registered to event successfully",
	})

	if err != nil {
		hr.serverError(w, r, err)
	}
}

// EventRequestResponse is a DTO for sending event request data to the frontend.
type EventRequestResponse struct {
	ID                   uuid.UUID            `json:"id"`
	Status               string               `json:"status"`
	RequesterGuildID     uuid.UUID            `json:"requester_guild_id"`
	Title                string               `json:"title"`
	Description          string               `json:"description"`
	ProposedStartDate    time.Time            `json:"proposed_start_date"`
	ProposedEndDate      time.Time            `json:"proposed_end_date"`
	ParticipationDetails ParticipationDetails `json:"participation_details"`
	RoomConfiguration    RoomConfiguration    `json:"room_configuration"`
	EventSpecifics       *EventSpecifics      `json:"event_specifics,omitempty"`
	RejectionReason      string               `json:"rejection_reason"`
	ApprovedEventID      uuid.UUID            `json:"approved_event_id"`
	Notes                string               `json:"notes"`
}

// GetEventRequestsHandler fetches a list of event requests, optionally filtered by status.
// This is intended for admin use.
func (hr *HandlerRepo) GetEventRequestsHandler(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	// Basic pagination
	params := store.ListEventRequestsParams{
		Limit:  20,
		Offset: 0,
	}

	var requests []store.EventRequest
	var err error

	if status != "" {
		requests, err = hr.queries.ListEventRequestsByStatus(r.Context(), store.ListEventRequestsByStatusParams{
			Status: store.EventRequestStatus(status),
			Limit:  params.Limit,
			Offset: params.Offset,
		})
	} else {
		requests, err = hr.queries.ListEventRequests(r.Context(), params)
	}

	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	responseDTOs := make([]EventRequestResponse, 0, len(requests))
	for _, req := range requests {
		dto, err := toEventRequestResponse(req)
		if err != nil {
			hr.badRequest(w, r, ErrInvalidRequest)
			return
		}
		responseDTOs = append(responseDTOs, dto)
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    responseDTOs,
		Success: true,
		Msg:     "Event requests retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

// ProcessEventRequestHandler allows an admin to approve or reject an event request.
func (hr *HandlerRepo) ProcessEventRequestHandler(w http.ResponseWriter, r *http.Request) {
	requestIDStr := chi.URLParam(r, "request_id")
	requestID, err := uuid.Parse(requestIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid request ID format"))
		return
	}

	var payload ProcessRequestPayload
	if err := request.DecodeJSON(w, r, &payload); err != nil {
		hr.badRequest(w, r, err)
		return
	}

	// TODO: Get admin ID from auth context. Using a placeholder for now.
	adminID := uuid.New()

	// 1. Fetch the original request
	eventRequest, err := hr.queries.GetEventRequestByID(r.Context(), toPgtypeUUID(requestID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hr.notFound(w, r)
		} else {
			hr.serverError(w, r, err)
		}
		return
	}

	if eventRequest.Status != store.EventRequestStatusPending {
		hr.badRequest(w, r, fmt.Errorf("request has already been processed with status: %s", eventRequest.Status))
		return
	}

	switch payload.Action {
	case "approve":
		err = hr.approveEventRequest(r.Context(), eventRequest, adminID)
	case "decline":
		if payload.RejectionReason == "" {
			hr.badRequest(w, r, errors.New("rejection reason is required when declining a request"))
			return
		}
		err = hr.declineEventRequest(r.Context(), eventRequest, adminID, payload.RejectionReason)
	default:
		hr.badRequest(w, r, errors.New("invalid action: must be 'approve' or 'decline'"))
		return
	}

	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Success: true,
		Msg:     fmt.Sprintf("Event request successfully %sed", payload.Action),
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

// approve an event will create the event and schedule delayed processing for room creation and guild assignment
func (hr *HandlerRepo) approveEventRequest(ctx context.Context, req store.EventRequest, adminID uuid.UUID) error {
	var roomConfig RoomConfiguration
	if err := json.Unmarshal(req.RoomConfiguration, &roomConfig); err != nil {
		return fmt.Errorf("failed to unmarshal room configuration: %w", err)
	}

	var participationDetails ParticipationDetails
	if err := json.Unmarshal(req.ParticipationDetails, &participationDetails); err != nil {
		return fmt.Errorf("failed to unmarshal participation details: %w", err)
	}

	// Use a transaction to ensure atomicity
	tx, err := hr.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := hr.queries.WithTx(tx)

	// Create the actual event
	event, err := qtx.CreateEvent(ctx, store.CreateEventParams{
		Title:              req.Title,
		Description:        req.Description,
		Type:               req.EventType,
		StartedDate:        req.ProposedStartDate,
		EndDate:            req.ProposedEndDate,
		MaxGuilds:          pgtype.Int4{Int32: int32(participationDetails.MaxGuilds), Valid: true},
		MaxPlayersPerGuild: pgtype.Int4{Int32: int32(participationDetails.MaxPlayersPerGuild), Valid: true},
		NumberOfRooms:      pgtype.Int4{Int32: int32(roomConfig.NumberOfRooms), Valid: true},
		GuildsPerRoom:      pgtype.Int4{Int32: int32(roomConfig.GuildsPerRoom), Valid: true},
		RoomNamingPrefix:   pgtype.Text{String: roomConfig.RoomNamingPrefix, Valid: true},
		OriginalRequestID:  req.ID,
	})
	if err != nil {
		return fmt.Errorf("failed to create event: %w", err)
	}

	// Create rooms for the event (rooms are created now, but guilds will be assigned later)
	for i := 0; i < int(roomConfig.NumberOfRooms); i++ {
		roomName := fmt.Sprintf("%s %d", roomConfig.RoomNamingPrefix, i+1)
		_, err := qtx.CreateRoom(ctx, store.CreateRoomParams{
			EventID:     event.ID,
			Name:        roomName,
			Description: fmt.Sprintf("Room %d for event %s", i+1, event.Title),
		})
		if err != nil {
			return fmt.Errorf("failed to create room: %w", err)
		}
	}

	// Register the requester's guild
	_, err = qtx.CreateEventGuildParticipant(ctx, store.CreateEventGuildParticipantParams{
		EventID: event.ID,
		GuildID: req.RequesterGuildID,
		RoomID:  pgtype.UUID{Valid: false}, // Room assignment happens at event start time
	})
	if err != nil {
		return fmt.Errorf("failed to add requester guild to event: %w", err)
	}

	// Update the request status
	_, err = qtx.UpdateEventRequestStatus(ctx, store.UpdateEventRequestStatusParams{
		ID:                 req.ID,
		Status:             store.EventRequestStatusApproved,
		ProcessedByAdminID: toPgtypeUUID(adminID),
		ApprovedEventID:    event.ID,
		RejectionReason:    pgtype.Text{Valid: false},
	})
	if err != nil {
		return fmt.Errorf("failed to update event request status: %w", err)
	}

	// Commit the transaction
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit event approval: %w", err)
	}

	// Schedule delayed processing for room assignments and event initialization
	// This happens at the proposed start time
	if hr.scheduler != nil {
		err = hr.scheduler.ScheduleEventCreation(ctx, req.ID.Bytes, req.ProposedStartDate.Time)
		if err != nil {
			hr.logger.Error("failed to schedule event processing",
				"event_request_id", req.ID.Bytes,
				"error", err)
			// Don't fail the approval if scheduling fails - log it for manual intervention
			return fmt.Errorf("event approved but failed to schedule processing: %w", err)
		}
		hr.logger.Info("scheduled event processing",
			"event_id", event.ID.Bytes,
			"scheduled_for", req.ProposedStartDate.Time)
	}

	return nil
}

func (hr *HandlerRepo) declineEventRequest(ctx context.Context, req store.EventRequest, adminID uuid.UUID, reason string) error {
	_, err := hr.queries.UpdateEventRequestStatus(ctx, store.UpdateEventRequestStatusParams{
		ID:                 req.ID,
		Status:             store.EventRequestStatusRejected,
		ProcessedByAdminID: toPgtypeUUID(adminID),
		RejectionReason:    pgtype.Text{String: reason, Valid: true},
		ApprovedEventID:    pgtype.UUID{Valid: false},
	})
	return err
}

func toEventRequestResponse(req store.EventRequest) (EventRequestResponse, error) {
	var participation ParticipationDetails
	if len(req.ParticipationDetails) > 0 {
		if err := json.Unmarshal(req.ParticipationDetails, &participation); err != nil {
			participation = ParticipationDetails{} // Use empty struct on error
			return EventRequestResponse{}, err
		}
	}

	var roomConfig RoomConfiguration
	if len(req.RoomConfiguration) > 0 {
		if err := json.Unmarshal(req.RoomConfiguration, &roomConfig); err != nil {
			roomConfig = RoomConfiguration{}
			return EventRequestResponse{}, err
		}
	}

	var eventSpecifics *EventSpecifics
	if len(req.EventSpecifics) > 0 {
		eventSpecifics = &EventSpecifics{}
		if err := json.Unmarshal(req.EventSpecifics, eventSpecifics); err != nil {
			// Log error but continue - event specifics are optional
			eventSpecifics = nil
		}
	}

	return EventRequestResponse{
		ID:                   req.ID.Bytes,
		Status:               string(req.Status),
		Title:                req.Title,
		Description:          req.Description,
		ProposedStartDate:    req.ProposedStartDate.Time,
		ProposedEndDate:      req.ProposedEndDate.Time,
		ParticipationDetails: participation,
		RoomConfiguration:    roomConfig,
		EventSpecifics:       eventSpecifics,
		ApprovedEventID:      req.ApprovedEventID.Bytes,
		RejectionReason:      req.RejectionReason.String,
		RequesterGuildID:     req.RequesterGuildID.Bytes,
		Notes:                req.Notes.String,
	}, nil
}
