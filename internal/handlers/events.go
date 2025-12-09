package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/events"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/request"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/pkg/response"
	"github.com/FA25SE050-RogueLearn/RogueLearn.Event/protos"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrGuildMemberRegisteredFull = errors.New("Guild member registered reached limit")
)

func (hr *HandlerRepo) GetEventsHandler(w http.ResponseWriter, r *http.Request) {
	// Parse pagination parameters from query string
	pagination := parsePaginationParams(r)

	// Get status filter from query parameter
	statusFilter := r.URL.Query().Get("status")

	// Check if user is a Game Master (admin)
	isGameMaster := HasRole(r.Context(), "Game Master")

	// Authorization logic for status filtering
	if statusFilter != "" {
		// Validate status is a valid event_status value
		validStatuses := map[string]bool{
			"pending":   true,
			"active":    true,
			"completed": true,
			"cancelled": true,
		}

		if !validStatuses[statusFilter] {
			hr.badRequest(w, r, errors.New("invalid status value. Must be one of: pending, active, completed, cancelled"))
			return
		}

		// Only Game Masters can filter 'cancelled' status
		restrictedStatuses := statusFilter == "cancelled"
		if restrictedStatuses && !isGameMaster {
			hr.forbidden(w, r)
			return
		}
	}

	var events []store.Event
	var totalCount int64
	var err error

	if statusFilter != "" {
		// Get count with status filter
		totalCount, err = hr.queries.CountEventsByStatus(r.Context(), store.EventStatus(statusFilter))
		if err != nil {
			hr.logger.Error("failed to count events by status", "err", err, "status", statusFilter)
			hr.serverError(w, r, err)
			return
		}

		// Get filtered events
		events, err = hr.queries.GetEventsByStatus(r.Context(), store.GetEventsByStatusParams{
			Status: store.EventStatus(statusFilter),
			Limit:  pagination.Limit,
			Offset: pagination.Offset,
		})
		if err != nil {
			hr.logger.Error("failed to get events by status", "err", err, "status", statusFilter)
			hr.serverError(w, r, err)
			return
		}
	} else {
		// No status filter provided - return only active events
		// Get count with status filter
		totalCount, err = hr.queries.CountEventsByStatus(r.Context(), store.EventStatusActive)
		if err != nil {
			hr.logger.Error("failed to count events by status", "err", err, "status", "active")
			hr.serverError(w, r, err)
			return
		}

		// Get filtered events
		events, err = hr.queries.GetEventsByStatus(r.Context(), store.GetEventsByStatusParams{
			Status: store.EventStatusActive,
			Limit:  pagination.Limit,
			Offset: pagination.Offset,
		})
		if err != nil {
			hr.logger.Error("failed to get events by status", "err", err, "status", "active")
			hr.serverError(w, r, err)
			return
		}
	}

	// Create pagination response
	paginatedResponse := createPaginationResponse(events, totalCount, pagination)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    paginatedResponse,
		Success: true,
		Msg:     "Events retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

type EventCreationRequest struct {
	EventType         string               `json:"event_type"`
	Title             string               `json:"title"`
	Description       string               `json:"description"`
	ProposedStartDate time.Time            `json:"proposed_start_date"`
	ProposedEndDate   time.Time            `json:"proposed_end_date"`
	Participation     ParticipationDetails `json:"participation"`
	EventSpecifics    *EventSpecifics      `json:"event_specifics,omitempty"`
	Notes             string               `json:"notes"`
}

// ParticipationDetails defines the rules for who and how many can join the event.
type ParticipationDetails struct {
	MaxGuilds          int `json:"max_guilds"`
	MaxPlayersPerGuild int `json:"max_players_per_guild"`
}

// EventSpecifics contains details that vary depending on the event type.
type EventSpecifics struct {
	CodeBattle *CodeBattleDetails `json:"code_battle,omitempty"`
}

// CodeBattleDetails holds the specific requirements for a "code_battle" event.
type CodeBattleDetails struct {
	Topics       []uuid.UUID              `json:"topics"` // problem tag id
	Distribution []DifficultyDistribution `json:"distribution"`
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

type EventDetailsResponse struct {
	ID                  uuid.UUID  `json:"id"`
	RequesterGuildID    uuid.UUID  `json:"requester_guild_id"`
	Title               string     `json:"title"`
	Description         string     `json:"description"`
	Type                string     `json:"type"`
	StartedDate         time.Time  `json:"started_date"`
	EndDate             time.Time  `json:"end_date"`
	MaxGuilds           *int32     `json:"max_guilds"`
	MaxPlayersPerGuild  *int32     `json:"max_players_per_guild"`
	Status              string     `json:"status"`
	AssignmentDate      *time.Time `json:"assignment_date,omitempty"`
	GuildsLeft          *int32     `json:"guilds_left"`
	CurrentParticipants int64      `json:"current_participants"`
}

func (hr *HandlerRepo) GetEventDetailsHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	// Get the event details
	event, err := hr.queries.GetEventByID(r.Context(), toPgtypeUUID(eventID))
	if err == pgx.ErrNoRows {
		hr.logger.Info("event not found", "event_id", eventID)
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get event", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Count current participants
	participantCount, err := hr.queries.CountEventParticipants(r.Context(), event.ID)
	if err != nil {
		hr.logger.Error("failed to count event participants", "err", err)
		hr.serverError(w, r, err)
		return
	}

	requesterGuildID, err := hr.queries.GetEventRequesterIDByEventID(r.Context(), toPgtypeUUID(eventID))
	if err == pgx.ErrNoRows {
		hr.logger.Debug("requester_id not found")
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get event requester ID", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Build response with guilds_left calculation
	eventDetails := EventDetailsResponse{
		ID:                  event.ID.Bytes,
		RequesterGuildID:    requesterGuildID.Bytes,
		Title:               event.Title,
		Description:         event.Description,
		Type:                string(event.Type),
		StartedDate:         event.StartedDate.Time,
		EndDate:             event.EndDate.Time,
		Status:              string(event.Status),
		CurrentParticipants: participantCount,
	}

	// Add optional fields
	if event.MaxGuilds.Valid {
		eventDetails.MaxGuilds = &event.MaxGuilds.Int32
		guildsLeft := event.MaxGuilds.Int32 - int32(participantCount)
		if guildsLeft < 0 {
			guildsLeft = 0
		}
		eventDetails.GuildsLeft = &guildsLeft
	}

	if event.MaxPlayersPerGuild.Valid {
		eventDetails.MaxPlayersPerGuild = &event.MaxPlayersPerGuild.Int32
	}

	if event.AssignmentDate.Valid {
		eventDetails.AssignmentDate = &event.AssignmentDate.Time
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    eventDetails,
		Success: true,
		Msg:     "Event details retrieved successfully",
	})
	if err != nil {
		hr.serverError(w, r, err)
	}
}

// CreateEventHandler handles the submission of a new event creation request.
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

	if valid, msg := validDate(req); !valid {
		hr.badRequest(w, r, errors.New(msg))
		return
	}

	// Validate event specifics if provided
	if req.EventSpecifics != nil && req.EventSpecifics.CodeBattle != nil {
		// Check for duplicate topics
		if len(req.EventSpecifics.CodeBattle.Topics) > 0 {
			topicSet := make(map[uuid.UUID]bool)
			for _, topicID := range req.EventSpecifics.CodeBattle.Topics {
				if topicSet[topicID] {
					hr.badRequest(w, r, fmt.Errorf("duplicate topic ID found: %s", topicID))
					return
				}
				topicSet[topicID] = true
			}
		}
	}

	// get guild_id by grpc
	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.logger.Debug("Invalid claims", "err", err)
		hr.unauthorized(w, r)
		return
	}

	requesterGuild, err := hr.userClient.GetMyGuild(r.Context(), &protos.GetMyGuildRequest{
		AuthUserId: userClaims.Sub,
	})
	if err != nil {
		hr.logger.Error("failed to get requester guild_id", "err", err)
		hr.serverError(w, r, err)
		return
	}

	requesterGuildID, err := uuid.Parse(requesterGuild.Id)
	if err != nil {
		hr.logger.Error("failed to parse requester guild_id", "err", err)
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Debug("request_guild_id parsed", "requester_guild_id", requesterGuildID.String())

	// Marshal JSONB fields
	participationDetailsJSON, err := json.Marshal(req.Participation)
	if err != nil {
		hr.serverError(w, r, fmt.Errorf("failed to marshal participation details: %w", err))
		return
	}
	// Validate the marshaled JSON
	if !json.Valid(participationDetailsJSON) {
		hr.logger.Error("invalid participation details JSON", "json", string(participationDetailsJSON))
		hr.badRequest(w, r, errors.New("invalid participation details format"))
		return
	}

	var eventSpecificsJSON []byte
	if req.EventSpecifics != nil {
		eventSpecificsJSON, err = json.Marshal(req.EventSpecifics)
		if err != nil {
			hr.serverError(w, r, fmt.Errorf("failed to marshal event specifics: %w", err))
			return
		}
		// Validate the marshaled JSON
		if !json.Valid(eventSpecificsJSON) {
			hr.logger.Error("invalid event specifics JSON", "json", string(eventSpecificsJSON))
			hr.badRequest(w, r, errors.New("invalid event specifics format"))
			return
		}
		hr.logger.Info("event specifics marshaled", "json", string(eventSpecificsJSON))
	}

	// Log all JSON fields for debugging
	hr.logger.Info("marshaled JSON fields",
		"participation", string(participationDetailsJSON),
		"event_specifics", string(eventSpecificsJSON))

	// --- Database Insertion ---
	// Convert []byte to string for JSONB fields (required for simple protocol mode)
	params := store.CreateEventRequestParams{
		RequesterGuildID:     toPgtypeUUID(requesterGuildID),
		EventType:            store.EventType(req.EventType),
		Title:                req.Title,
		Description:          req.Description,
		ProposedStartDate:    pgtype.Timestamptz{Time: req.ProposedStartDate, Valid: true},
		ProposedEndDate:      pgtype.Timestamptz{Time: req.ProposedEndDate, Valid: true},
		Notes:                pgtype.Text{String: req.Notes, Valid: req.Notes != ""},
		ParticipationDetails: string(participationDetailsJSON),
		EventSpecifics:       string(eventSpecificsJSON),
	}

	eventRequest, err := hr.queries.CreateEventRequest(r.Context(), params)
	if err != nil {
		hr.logger.Error("failed to create event request in database",
			"err", err,
			"participation", string(participationDetailsJSON),
			"event_specifics", string(eventSpecificsJSON))
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

func validDate(req EventCreationRequest) (valid bool, msg string) {
	if req.ProposedStartDate.UTC().After(req.ProposedEndDate.UTC()) {
		return false, "Proposed start date cannot be after proposed end date"
	}

	if req.ProposedStartDate.UTC().Before(time.Now().UTC()) {
		return false, "Proposed start date cannot be in the past"
	}

	if req.ProposedEndDate.UTC().Before(time.Now().UTC()) {
		return false, "Proposed end date cannot be in the past"
	}

	return valid, msg
}

// GetMyEventRequestsHandler fetches a list of event requests submitted by a specific guild.
func (hr *HandlerRepo) GetMyEventRequestsHandler(w http.ResponseWriter, r *http.Request) {
	// Get Guild Master ID from JWT claims
	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.logger.Debug("Invalid claims", "err", err)
		hr.unauthorized(w, r)
		return
	}

	// Get guild ID via gRPC
	ctx, cancel := context.WithTimeout(r.Context(), DefaultQueryTimeoutSecond)
	defer cancel()
	guild, err := hr.userClient.GetMyGuild(ctx, &protos.GetMyGuildRequest{
		AuthUserId: userClaims.Sub,
	})
	if err != nil {
		hr.logger.Error("failed to get guild", "err", err)
		hr.serverError(w, r, err)
		return
	}

	guildID, err := uuid.Parse(guild.Id)
	if err != nil {
		hr.logger.Error("failed to parse guild ID", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Parse pagination parameters from query string
	pagination := parsePaginationParams(r)

	// Get total count
	totalCount, err := hr.queries.CountEventRequestsByGuild(r.Context(), toPgtypeUUID(guildID))
	if err != nil {
		hr.logger.Error("failed to count event requests by guild", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Get paginated data
	params := store.ListEventRequestsByGuildParams{
		RequesterGuildID: toPgtypeUUID(guildID),
		Limit:            pagination.Limit,
		Offset:           pagination.Offset,
	}

	requests, err := hr.queries.ListEventRequestsByGuild(r.Context(), params)
	if err != nil {
		hr.logger.Error("failed to get event requests by guild", "err", err)
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

	// Create pagination response
	paginatedResponse := createPaginationResponse(responseDTOs, totalCount, pagination)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    paginatedResponse,
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
	if err == pgx.ErrNoRows {
		hr.logger.Info("rooms not found for event", "event_id", eventID)
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get rooms for event", "err", err)
		hr.serverError(w, r, err)
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
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}
	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), DefaultQueryTimeoutSecond)
	defer cancel()
	// get the guild master guild
	guild, err := hr.userClient.GetMyGuild(ctx, &protos.GetMyGuildRequest{
		AuthUserId: userClaims.Sub,
	})
	if err != nil {
		hr.logger.Error("failed to get guild", "err", err)
		hr.serverError(w, r, err)
		return
	}

	guildID, err := uuid.Parse(guild.Id)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// Use transaction to prevent race condition on capacity check
	tx, err := hr.db.Begin(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := hr.queries.WithTx(tx)

	// Validate that the event exists and is in a valid state for registration
	event, err := qtx.GetEventByID(r.Context(), toPgtypeUUID(eventID))
	if err == pgx.ErrNoRows {
		hr.logger.Info("event not found", "event_id", eventID)
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get event", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Check if event is accepting registrations (only pending state)
	if event.Status != store.EventStatusPending {
		hr.badRequest(w, r, fmt.Errorf("event is not accepting registrations (status: %s)", event.Status))
		return
	}

	// Check if event has reached max guilds capacity
	// This check is now inside transaction, preventing race condition
	if event.MaxGuilds.Valid {
		participantCount, err := qtx.CountEventParticipants(r.Context(), event.ID)
		if err != nil {
			hr.serverError(w, r, err)
			return
		}

		if participantCount >= int64(event.MaxGuilds.Int32) {
			hr.badRequest(w, r, errors.New("event has reached maximum guild capacity"))
			return
		}
	}

	// room_id will be known later (assigned at assignment_date)
	_, err = qtx.CreateEventGuildParticipant(r.Context(), store.CreateEventGuildParticipantParams{
		EventID: toPgtypeUUID(eventID),
		GuildID: toPgtypeUUID(guildID),
	})
	if err != nil {
		// Check for duplicate registration and provide friendly error
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "event_guild_participants_pkey") {
			hr.badRequest(w, r, errors.New("guild is already registered for this event"))
			return
		}
		hr.logger.Error("failed to create event guild participant", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Commit the transaction
	if err = tx.Commit(r.Context()); err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("guild registered to event", "guild_id", guildID, "event_id", eventID)

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
	EventSpecifics       *EventSpecifics      `json:"event_specifics,omitempty"`
	RejectionReason      string               `json:"rejection_reason"`
	ApprovedEventID      uuid.UUID            `json:"approved_event_id"`
	Notes                string               `json:"notes"`
}

// GetEventRequestsHandler fetches a list of event requests, optionally filtered by status.
// This is intended for admin use.
func (hr *HandlerRepo) GetEventRequestsHandler(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	// Parse pagination parameters from query string
	pagination := parsePaginationParams(r)

	var requests []store.EventRequest
	var totalCount int64
	var err error

	if status != "" {
		// Get count with filter
		totalCount, err = hr.queries.CountEventRequestsByStatus(r.Context(), store.EventRequestStatus(status))
		if err != nil {
			hr.logger.Error("failed to count event requests by status", "err", err)
			hr.serverError(w, r, err)
			return
		}

		// Get filtered data
		requests, err = hr.queries.ListEventRequestsByStatus(r.Context(), store.ListEventRequestsByStatusParams{
			Status: store.EventRequestStatus(status),
			Limit:  pagination.Limit,
			Offset: pagination.Offset,
		})
	} else {
		// Get total count
		totalCount, err = hr.queries.CountEventRequests(r.Context())
		if err != nil {
			hr.logger.Error("failed to count event requests", "err", err)
			hr.serverError(w, r, err)
			return
		}

		// Get all data
		requests, err = hr.queries.ListEventRequests(r.Context(), store.ListEventRequestsParams{
			Limit:  pagination.Limit,
			Offset: pagination.Offset,
		})
	}

	if err != nil {
		hr.logger.Error("failed to get event requests", "err", err)
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

	// Create pagination response
	paginatedResponse := createPaginationResponse(responseDTOs, totalCount, pagination)

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    paginatedResponse,
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
	if err == pgx.ErrNoRows {
		hr.logger.Info("event request not found", "request_id", requestID)
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get event request", "err", err)
		hr.serverError(w, r, err)
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
	var participationDetails ParticipationDetails
	if err := json.Unmarshal([]byte(req.ParticipationDetails), &participationDetails); err != nil {
		return fmt.Errorf("failed to unmarshal participation details: %w", err)
	}

	// Use a transaction to ensure atomicity
	tx, err := hr.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := hr.queries.WithTx(tx)

	assignmentDate := req.ProposedStartDate.Time.Add(-time.Duration(hr.eventConfig.AssignmentDelayMinutes) * time.Minute)

	// Create the actual event (rooms will be created dynamically at assignment time)
	event, err := qtx.CreateEvent(ctx, store.CreateEventParams{
		Title:              req.Title,
		Description:        req.Description,
		Type:               req.EventType,
		StartedDate:        req.ProposedStartDate,
		EndDate:            req.ProposedEndDate,
		MaxGuilds:          pgtype.Int4{Int32: int32(participationDetails.MaxGuilds), Valid: true},
		MaxPlayersPerGuild: pgtype.Int4{Int32: int32(participationDetails.MaxPlayersPerGuild), Valid: true},
		OriginalRequestID:  req.ID,
		Status:             store.EventStatusPending, // Event starts in 'pending' state
		AssignmentDate:     pgtype.Timestamptz{Time: assignmentDate, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("failed to create event: %w", err)
	}

	// Note: Rooms will be created dynamically at assignment time based on the number of guilds that joined

	// Fetch guild name for the requester's guild via gRPC
	requesterGuildID := uuid.UUID(req.RequesterGuildID.Bytes)
	requesterGuildInfo, err := hr.userClient.GetGuildById(ctx, &protos.GetGuildByIdRequest{
		GuildId: requesterGuildID.String(),
	})
	if err != nil {
		hr.logger.Warn("failed to fetch requester guild name via gRPC, will use empty name",
			"guild_id", requesterGuildID,
			"error", err)
	}

	requesterGuildName := ""
	if requesterGuildInfo != nil && requesterGuildInfo.Name != "" {
		requesterGuildName = requesterGuildInfo.Name
	}

	// Register the requester's guild
	_, err = qtx.CreateEventGuildParticipant(ctx, store.CreateEventGuildParticipantParams{
		EventID:   event.ID,
		GuildID:   req.RequesterGuildID,
		RoomID:    pgtype.UUID{Valid: false}, // Room assignment happens at event start time
		GuildName: pgtype.Text{String: requesterGuildName, Valid: requesterGuildName != ""},
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

	hr.logger.Info("Event approved and created successfully (rooms will be created at assignment time)",
		"event_id", event.ID.Bytes,
		"assignment_date", assignmentDate,
		"start_date", req.ProposedStartDate.Time)

	// Note: Room creation and guild-to-room assignment will be triggered automatically by the
	// Alpine cron service when it calls /internal/start-pending-events
	// at the assignment_date (15 minutes before event start).
	// Rooms will be created dynamically based on the number of guilds that joined.

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

type SelectGuildMembersRequest struct {
	Members []GuildMemberSelection `json:"members"`
}

type GuildMemberSelection struct {
	UserID uuid.UUID `json:"user_id"`
}

// SelectGuildMembersHandler allows a Guild Master to select which members will compete in an event
func (hr *HandlerRepo) SelectGuildMembersHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	var req SelectGuildMembersRequest
	if err := request.DecodeJSON(w, r, &req); err != nil {
		hr.badRequest(w, r, err)
		return
	}

	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), DefaultQueryTimeoutSecond)
	defer cancel()
	// get the guild master guild
	guild, err := hr.userClient.GetMyGuild(ctx, &protos.GetMyGuildRequest{
		AuthUserId: userClaims.Sub,
	})
	if err != nil {
		hr.logger.Error("failed to get guild", "err", err)
		hr.serverError(w, r, err)
		return
	}

	guildID, err := uuid.Parse(guild.Id)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Debug("guild master's guild retrieved", "guild_id", guild.Id)

	// Validate that all members belong to the specified guild
	// Use a separate context with timeout for gRPC calls
	grpcCtx, grpcCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer grpcCancel()

	for _, member := range req.Members {
		// Try to get the member's roles in the guild
		// This will fail if the user is not a member of the guild
		temp, err := hr.userClient.GetGuildMemberRoles(grpcCtx, &protos.GetGuildMemberRolesRequest{
			GuildId:          guildID.String(),
			MemberAuthUserId: member.UserID.String(),
		})
		if len(temp.Roles) <= 0 {
			hr.logger.Warn("user is not a member of the guild",
				"user_id", member.UserID,
				"guild_id", guildID,
				"error", err)
			hr.badRequest(w, r, fmt.Errorf("user %s is not a member of guild %s", member.UserID, guildID))
			return
		}
		if err != nil {
			hr.logger.Error("Failed to get guild member roles",
				"error", err)
			hr.serverError(w, r, fmt.Errorf("user %s is not a member of guild %s", member.UserID, guildID))
			return
		}
		hr.logger.Debug("get guild member roles", "result", temp.Roles)
	}

	hr.logger.Info("All selected members validated as guild members",
		"guild_id", guildID,
		"member_count", len(req.Members))

	// Get event to check max_players_per_guild limit and status
	event, err := hr.queries.GetEventByID(r.Context(), toPgtypeUUID(eventID))
	if err == pgx.ErrNoRows {
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// validation
	// Verify event is in pending status (members can only be selected before event starts)
	if event.Status != store.EventStatusPending {
		hr.badRequest(w, r, errors.New("cannot select members for event that has already started"))
		return
	}

	// Verify member count doesn't exceed max_players_per_guild
	if event.MaxPlayersPerGuild.Valid && len(req.Members) > int(event.MaxPlayersPerGuild.Int32) {
		hr.badRequest(w, r, fmt.Errorf("cannot select more than %d members", event.MaxPlayersPerGuild.Int32))
		return
	}

	// Verify guild is registered for this event
	_, err = hr.queries.GetEventGuildParticipant(r.Context(), store.GetEventGuildParticipantParams{
		EventID: toPgtypeUUID(eventID),
		GuildID: toPgtypeUUID(guildID),
	})
	if err == pgx.ErrNoRows {
		hr.badRequest(w, r, errors.New("guild is not registered for this event"))
		return
	} else if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// Use transaction to ensure atomicity
	tx, err := hr.db.Begin(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := hr.queries.WithTx(tx)

	// Add selected members
	addedCount := 0
	skippedCount := 0
	for _, member := range req.Members {
		_, err = qtx.AddGuildMemberToEvent(r.Context(), store.AddGuildMemberToEventParams{
			EventID: toPgtypeUUID(eventID),
			GuildID: toPgtypeUUID(guildID),
			UserID:  toPgtypeUUID(member.UserID),
		})
		if err == pgx.ErrNoRows {
			// Member already exists (ON CONFLICT DO NOTHING), skip
			skippedCount++
			hr.logger.Debug("member already registered, skipping", "user_id", member.UserID)
			continue
		} else if err != nil {
			hr.logger.Error("failed to add member to event", "user_id", member.UserID, "error", err)
			hr.serverError(w, r, fmt.Errorf("failed to add member %s: %w", member.UserID, err))
			return
		}
		addedCount++
	}

	if err = tx.Commit(r.Context()); err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("Guild members selected successfully",
		"event_id", eventID,
		"guild_id", guildID,
		"added_count", addedCount,
		"skipped_count", skippedCount,
		"total_requested", len(req.Members))

	msg := fmt.Sprintf("Successfully processed %d members (%d added, %d already registered)",
		len(req.Members), addedCount, skippedCount)

	response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Success: true,
		Msg:     msg,
	})
}

type EventGuildMemberResponse struct {
	UserID     uuid.UUID `json:"user_id"`
	SelectedAt time.Time `json:"selected_at"`
}

// GetEventGuildMembersHandler returns the list of selected members for a guild in an event
func (hr *HandlerRepo) GetEventGuildMembersHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), DefaultQueryTimeoutSecond)
	defer cancel()
	// get the guild master guild
	guild, err := hr.userClient.GetMyGuild(ctx, &protos.GetMyGuildRequest{
		AuthUserId: userClaims.Sub,
	})
	if err != nil {
		hr.logger.Error("failed to get guild", "err", err)
		hr.serverError(w, r, err)
		return
	}

	guildID, err := uuid.Parse(guild.Id)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	members, err := hr.queries.GetEventGuildMembers(r.Context(), store.GetEventGuildMembersParams{
		EventID: toPgtypeUUID(eventID),
		GuildID: toPgtypeUUID(guildID),
	})
	if err != nil {
		hr.logger.Error("failed to get event guild members", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Convert to response DTOs
	responseDTOs := make([]EventGuildMemberResponse, 0, len(members))
	for _, member := range members {
		responseDTOs = append(responseDTOs, EventGuildMemberResponse{
			UserID:     member.UserID.Bytes,
			SelectedAt: member.SelectedAt.Time,
		})
	}

	response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    responseDTOs,
		Success: true,
		Msg:     "Event guild members retrieved successfully",
	})
}

// RemoveGuildMembersHandler removes selected members from a guild's event registration
func (hr *HandlerRepo) RemoveGuildMembersHandler(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), DefaultQueryTimeoutSecond)
	defer cancel()
	// get the guild master guild
	guild, err := hr.userClient.GetMyGuild(ctx, &protos.GetMyGuildRequest{
		AuthUserId: userClaims.Sub,
	})
	if err != nil {
		hr.logger.Error("failed to get guild", "err", err)
		hr.serverError(w, r, err)
		return
	}

	guildID, err := uuid.Parse(guild.Id)
	if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// Decode request body with list of user IDs to remove
	var req SelectGuildMembersRequest
	if err := request.DecodeJSON(w, r, &req); err != nil {
		hr.badRequest(w, r, err)
		return
	}

	// Get event to check status
	event, err := hr.queries.GetEventByID(r.Context(), toPgtypeUUID(eventID))
	if err == pgx.ErrNoRows {
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// Verify event is in pending status (members can only be modified before event starts)
	if event.Status != store.EventStatusPending {
		hr.badRequest(w, r, errors.New("cannot modify members for event that has already started"))
		return
	}

	// Verify guild is registered for this event
	_, err = hr.queries.GetEventGuildParticipant(r.Context(), store.GetEventGuildParticipantParams{
		EventID: toPgtypeUUID(eventID),
		GuildID: toPgtypeUUID(guildID),
	})
	if err == pgx.ErrNoRows {
		hr.badRequest(w, r, errors.New("guild is not registered for this event"))
		return
	} else if err != nil {
		hr.serverError(w, r, err)
		return
	}

	// Use transaction to ensure atomicity
	tx, err := hr.db.Begin(r.Context())
	if err != nil {
		hr.serverError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := hr.queries.WithTx(tx)

	// Remove selected members
	removedCount := 0
	for _, member := range req.Members {
		err = qtx.RemoveGuildMemberFromEvent(r.Context(), store.RemoveGuildMemberFromEventParams{
			EventID: toPgtypeUUID(eventID),
			GuildID: toPgtypeUUID(guildID),
			UserID:  toPgtypeUUID(member.UserID),
		})
		if err != nil {
			hr.logger.Error("failed to remove member from event", "user_id", member.UserID, "error", err)
			hr.serverError(w, r, fmt.Errorf("failed to remove member %s: %w", member.UserID, err))
			return
		}
		removedCount++
	}

	if err = tx.Commit(r.Context()); err != nil {
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("Guild members removed successfully",
		"event_id", eventID,
		"guild_id", guildID,
		"removed_count", removedCount)

	response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Success: true,
		Msg:     fmt.Sprintf("Successfully removed %d members from the event", removedCount),
	})
}

func toEventRequestResponse(req store.EventRequest) (EventRequestResponse, error) {
	var participation ParticipationDetails
	if len(req.ParticipationDetails) > 0 {
		if err := json.Unmarshal([]byte(req.ParticipationDetails), &participation); err != nil {
			participation = ParticipationDetails{} // Use empty struct on error
			return EventRequestResponse{}, err
		}
	}

	var eventSpecifics *EventSpecifics
	if len(req.EventSpecifics) > 0 {
		eventSpecifics = &EventSpecifics{}
		if err := json.Unmarshal([]byte(req.EventSpecifics), eventSpecifics); err != nil {
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
		EventSpecifics:       eventSpecifics,
		ApprovedEventID:      req.ApprovedEventID.Bytes,
		RejectionReason:      req.RejectionReason.String,
		RequesterGuildID:     req.RequesterGuildID.Bytes,
		Notes:                req.Notes.String,
	}, nil
}

// LeaveRoomHandler handles a user deliberately leaving a room.
// This is different from SSE disconnection - the user is explicitly leaving,
// so we delete the room_player record instead of just updating the state.
// However, we still send a PlayerLeft event to the room for real-time notification.
func (hr *HandlerRepo) LeaveRoomHandler(w http.ResponseWriter, r *http.Request) {
	hr.logger.Debug("-----------------LeaveRoomHandler called-----------------")
	eventIDStr := chi.URLParam(r, "event_id")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid event ID format"))
		return
	}

	roomIDStr := chi.URLParam(r, "room_id")
	roomID, err := uuid.Parse(roomIDStr)
	if err != nil {
		hr.badRequest(w, r, errors.New("invalid room ID format"))
		return
	}

	// Get user claims from JWT
	userClaims, err := GetUserClaims(r.Context())
	if err != nil {
		hr.logger.Debug("Invalid claims", "err", err)
		hr.unauthorized(w, r)
		return
	}

	userID, err := uuid.Parse(userClaims.Sub)
	if err != nil {
		hr.logger.Error("failed to parse user ID from claims", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Verify the room exists and belongs to the event
	room, err := hr.queries.GetRoomByID(r.Context(), toPgtypeUUID(roomID))
	if err == pgx.ErrNoRows {
		hr.logger.Info("room not found", "room_id", roomID)
		hr.notFound(w, r)
		return
	} else if err != nil {
		hr.logger.Error("failed to get room", "err", err)
		hr.serverError(w, r, err)
		return
	}

	// Verify room belongs to the specified event
	if uuid.UUID(room.EventID.Bytes) != eventID {
		hr.badRequest(w, r, errors.New("room does not belong to specified event"))
		return
	}

	// Verify user is actually in the room
	_, err = hr.queries.GetRoomPlayer(r.Context(), store.GetRoomPlayerParams{
		RoomID: toPgtypeUUID(roomID),
		UserID: toPgtypeUUID(userID),
	})
	if err == pgx.ErrNoRows {
		hr.badRequest(w, r, errors.New("player is not in this room"))
		return
	} else if err != nil {
		hr.logger.Error("failed to get room player", "err", err)
		hr.serverError(w, r, err)
		return
	}

	hr.logger.Info("User left room",
		"user_id", userID,
		"room_id", roomID,
		"event_id", eventID)

	// Get the room hub and send PlayerLeft event
	roomHub := hr.eventHub.GetRoomById(roomID)
	if roomHub != nil {
		// Send PlayerLeft event to notify other players
		roomHub.Events <- events.PlayerLeft{
			PlayerID: userID,
			RoomID:   roomID,
		}
		hr.logger.Info("Sent PlayerLeft event to room hub",
			"user_id", userID,
			"room_id", roomID)
	} else {
		hr.logger.Warn("Room hub not found in memory, PlayerLeft event not sent",
			"room_id", roomID)
	}

	err = response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Success: true,
		Msg:     "Successfully left the room",
	})

	if err != nil {
		hr.serverError(w, r, err)
	}
	hr.logger.Debug("-----------------LeaveRoomHandler END-----------------")
}
