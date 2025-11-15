package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/store"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/pkg/response"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// EventAssignmentMessage represents the message sent to RabbitMQ
// for processing guild-to-room assignments
type EventAssignmentMessage struct {
	EventID uuid.UUID `json:"event_id"`
}

// StartPendingEventsHandler is called by the Alpine cron service every 60 seconds.
// It finds events whose assignment_date has passed and triggers their processing.
//
// Flow:
// 1. Query events WHERE assignment_date <= NOW() AND status = 'pending'
// 2. Update their status to 'queued' atomically (prevents duplicate processing)
// 3. Publish each event ID to RabbitMQ for asynchronous processing
// 4. Return the list of events that were queued
func (hr *HandlerRepo) StartPendingEventsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	hr.logger.Info("StartPendingEventsHandler called - checking for events ready for assignment")

	// Step 1: Find all events that are ready for guild assignment
	pendingEvents, err := hr.queries.GetPendingEventsForAssignment(ctx)
	if err != nil {
		hr.logger.Error("Failed to query pending events", "error", err)
		hr.serverError(w, r, err)
		return
	}

	if len(pendingEvents) == 0 {
		hr.logger.Info("No events ready for assignment")
		response.JSON(w, response.JSONResponseParameters{
			Status:  http.StatusOK,
			Success: false,
			Msg:     "No events ready for assignment",
		})
		return
	}

	hr.logger.Info("Found events ready for assignment", "count", len(pendingEvents))

	successCount := 0
	queuedCount := 0
	failedEvents := []uuid.UUID{}

	// Step 2: Extract event IDs
	eventIDs := make([]uuid.UUID, len(pendingEvents))
	for i, event := range pendingEvents {
		eventIDs[i] = uuid.UUID(event.ID.Bytes)
		hr.logger.Info("Event ready for assignment",
			"event_id", eventIDs[i],
			"title", event.Title,
			"assignment_date", event.AssignmentDate.Time)

		// Step 3: Atomically update status to 'queued'
		// This prevents other instances from processing the same events
		// Only events still in 'pending' status will be updated
		// CRITICAL FIX: The query now returns the updated event, or error if already queued
		updatedEvent, err := hr.queries.UpdateEventStatusToQueued(ctx, event.ID)
		if err != nil {
			// Check if this is a "no rows updated" error (event already queued by another instance)
			if err.Error() == "no rows in result set" || strings.Contains(err.Error(), "no rows") {
				hr.logger.Info("Event already queued by another instance, skipping",
					"event_id", eventIDs[i],
					"title", event.Title)
				// This is OK - another instance is handling it
				// Don't count as queued by us, but don't fail either
				continue
			}

			// Actual database error occurred
			hr.logger.Error("Failed to update event status to queued",
				"event_id", eventIDs[i],
				"error", err)
			hr.serverError(w, r, err)
			return
		}

		queuedCount++
		hr.logger.Info("Successfully marked event as queued by this instance",
			"event_id", updatedEvent.ID.String(),
			"title", updatedEvent.Title)

		// Step 4: Publish each event to RabbitMQ for processing
		message := EventAssignmentMessage{
			EventID: event.ID.Bytes,
		}

		payload, err := json.Marshal(message)
		if err != nil {
			hr.logger.Error("Failed to marshal event assignment message",
				"event_id", event.ID,
				"error", err)
			failedEvents = append(failedEvents, event.ID.Bytes)
			continue
		}

		// Publish to RabbitMQ event assignment queue
		err = hr.rabbitClient.PublishEventAssignment(ctx, payload)
		if err != nil {
			hr.logger.Error("Failed to publish event assignment to RabbitMQ",
				"event_id", event.ID,
				"error", err)
			failedEvents = append(failedEvents, event.ID.Bytes)
			continue
		}

		hr.logger.Info("Published event assignment to RabbitMQ",
			"event_id", event.ID,
			"title", event.Title)
		successCount++
	}

	// Step 5: Return response
	responseData := map[string]any{
		"events_found":     len(pendingEvents),
		"events_queued":    queuedCount,
		"events_published": successCount,
	}

	if len(failedEvents) > 0 {
		responseData["failed_events"] = failedEvents
		hr.logger.Warn("Some events failed to publish to RabbitMQ", "count", len(failedEvents))
	}

	response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    responseData,
		Success: true,
		Msg:     "Succeeded",
	})
}

// ProcessEventAssignment processes a single event's guild-to-room assignment.
// This is called by the RabbitMQ consumer when it receives an EventAssignmentMessage.
//
// Flow:
// 1. Fetch event and verify it's in 'queued' status
// 2. Get all participating guilds
// 3. Get all rooms for the event
// 4. Assign guilds to rooms using round-robin distribution
// 5. Assign Code Problems and score distribution to each Code Problem to Event
// 6. Update event status to 'active'
func (hr *HandlerRepo) ProcessEventAssignment(ctx context.Context, eventID uuid.UUID) error {
	hr.logger.Info("Processing event assignment", "event_id", eventID)

	// Fetch the event
	event, err := hr.queries.GetEventByID(ctx, toPgtypeUUID(eventID))
	if err != nil {
		return fmt.Errorf("failed to fetch event: %w", err)
	}

	// Verify event is in correct status
	if event.Status != store.EventStatusQueued {
		hr.logger.Warn("Event is not in 'queued' status, skipping",
			"event_id", eventID,
			"status", event.Status)
		return nil
	}

	hr.logger.Info("Starting guild assignment for event",
		"event_id", eventID,
		"title", event.Title)

	// Get all participating guilds
	participants, err := hr.queries.GetEventParticipants(ctx, event.ID)
	if err != nil {
		return fmt.Errorf("failed to fetch event participants: %w", err)
	}

	if len(participants) == 0 {
		hr.logger.Warn("No participants found for event", "event_id", eventID)
		// Still mark as active even with no participants
		err = hr.queries.UpdateEventStatusToActive(ctx, event.ID)
		if err != nil {
			return fmt.Errorf("failed to update event status to active: %w", err)
		}
		return nil
	}

	// Get all rooms for this event
	rooms, err := hr.queries.GetRoomsByEvent(ctx, event.ID)
	if err != nil {
		return fmt.Errorf("failed to fetch rooms: %w", err)
	}

	if len(rooms) == 0 {
		return fmt.Errorf("no rooms found for event %s", eventID)
	}

	// Calculate guilds per room
	guildsPerRoom := int(event.GuildsPerRoom.Int32)
	if guildsPerRoom <= 0 {
		guildsPerRoom = len(participants) / len(rooms)
		if guildsPerRoom == 0 {
			guildsPerRoom = 1
		}
	}

	hr.logger.Info("Assigning guilds to rooms",
		"event_id", eventID,
		"total_guilds", len(participants),
		"total_rooms", len(rooms),
		"guilds_per_room", guildsPerRoom)

	// Use a transaction for atomic room assignment
	tx, err := hr.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := hr.queries.WithTx(tx)

	// Assign guilds to rooms (round-robin distribution)
	roomIndex := 0
	for i, participant := range participants {
		roomIndex = (i / guildsPerRoom) % len(rooms)

		err = qtx.AssignGuildToRoom(ctx, store.AssignGuildToRoomParams{
			EventID: event.ID,
			GuildID: participant.GuildID,
			RoomID:  rooms[roomIndex].ID,
		})
		if err != nil {
			return fmt.Errorf("failed to assign guild %s to room %s: %w",
				participant.GuildID.Bytes, rooms[roomIndex].ID.Bytes, err)
		}

		hr.logger.Info("Assigned guild to room",
			"guild_id", participant.GuildID.Bytes,
			"room_id", rooms[roomIndex].ID.Bytes,
			"room_name", rooms[roomIndex].Name)
	}

	// Step 5: Assign Code Problems and score distribution to Event
	err = hr.assignCodeProblemsToEvent(ctx, qtx, event)
	if err != nil {
		return fmt.Errorf("failed to assign code problems to event: %w", err)
	}

	// Update event status to 'active'
	err = qtx.UpdateEventStatusToActive(ctx, event.ID)
	if err != nil {
		return fmt.Errorf("failed to update event status to active: %w", err)
	}

	// Commit the transaction
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit guild assignments: %w", err)
	}

	hr.logger.Info("Successfully completed event assignment",
		"event_id", eventID,
		"guilds_assigned", len(participants),
		"rooms_used", len(rooms))

	return nil
}

// assignCodeProblemsToEvent assigns code problems to an event based on the event specifics
// from the original event request. It selects problems based on difficulty and topics,
// then assigns them with appropriate scores.
func (hr *HandlerRepo) assignCodeProblemsToEvent(ctx context.Context, qtx *store.Queries, event store.Event) error {
	// Get the original event request to access event_specifics
	if !event.OriginalRequestID.Valid {
		hr.logger.Warn("Event has no original request ID, skipping code problem assignment", "event_id", event.ID.Bytes)
		return nil
	}

	eventRequest, err := qtx.GetEventRequestByID(ctx, event.OriginalRequestID)
	if err != nil {
		return fmt.Errorf("failed to fetch original event request: %w", err)
	}

	// Check if event_specifics exists
	if len(eventRequest.EventSpecifics) == 0 {
		hr.logger.Warn("No event specifics found, skipping code problem assignment", "event_id", event.ID.Bytes)
		return nil
	}

	// Unmarshal event specifics
	var eventSpecifics EventSpecifics
	if err := json.Unmarshal(eventRequest.EventSpecifics, &eventSpecifics); err != nil {
		return fmt.Errorf("failed to unmarshal event specifics: %w", err)
	}

	// Check if this is a code battle event
	if eventSpecifics.CodeBattle == nil {
		hr.logger.Warn("Event is not a code battle, skipping code problem assignment", "event_id", event.ID.Bytes)
		return nil
	}

	codeBattle := eventSpecifics.CodeBattle
	hr.logger.Info("Starting code problem assignment",
		"event_id", event.ID.Bytes,
		"topics", len(codeBattle.Topics),
		"distributions", len(codeBattle.Distrbution))

	// Track all assigned problem IDs to avoid duplicates across distributions
	assignedProblemIDs := []pgtype.UUID{}
	totalAssigned := 0

	// Process each difficulty distribution
	for _, dist := range codeBattle.Distrbution {
		hr.logger.Info("Processing difficulty distribution",
			"difficulty", dist.Difficulty,
			"number_of_problems", dist.NumberOfProblems,
			"score", dist.Score)

		// Convert topic UUIDs to pgtype.UUID
		var tagIDs []pgtype.UUID
		if len(codeBattle.Topics) > 0 {
			tagIDs = make([]pgtype.UUID, len(codeBattle.Topics))
			for i, topicID := range codeBattle.Topics {
				tagIDs[i] = toPgtypeUUID(topicID)
			}
		}

		// Fetch random problems matching the criteria
		// The query handles: difficulty filtering, tag filtering, exclusion of already assigned problems, and random selection
		problems, err := qtx.GetRandomCodeProblemsByDifficultyAndTags(ctx, store.GetRandomCodeProblemsByDifficultyAndTagsParams{
			Difficulty:         int32(dist.Difficulty),
			TagIds:             tagIDs,
			ExcludedProblemIds: assignedProblemIDs,
			LimitCount:         int32(dist.NumberOfProblems),
		})

		hr.logger.Info("problems are randomly chosen!", "problems", problems)

		if err != nil {
			return fmt.Errorf("failed to fetch problems for difficulty %d: %w", dist.Difficulty, err)
		}

		if len(problems) == 0 {
			hr.logger.Warn("No problems found for difficulty level",
				"difficulty", dist.Difficulty,
				"topics", len(codeBattle.Topics))
			continue
		}

		// Assign all fetched problems to the event
		assignedCount := 0
		for _, problem := range problems {
			// Assign the problem to the event
			err = qtx.CreateEventCodeProblem(ctx, store.CreateEventCodeProblemParams{
				EventID:       event.ID,
				CodeProblemID: problem.ID,
				Score:         int32(dist.Score),
			})
			if err != nil {
				hr.logger.Warn("Failed to assign problem to event",
					"problem_id", problem.ID.Bytes,
					"error", err)
				continue
			}

			// Add to excluded list for next iterations
			assignedProblemIDs = append(assignedProblemIDs, problem.ID)
			assignedCount++

			hr.logger.Info("Assigned problem to event",
				"problem_id", problem.ID.Bytes,
				"problem_title", problem.Title,
				"difficulty", dist.Difficulty,
				"score", dist.Score)
		}

		totalAssigned += assignedCount

		// Warn if we couldn't assign all required problems
		if assignedCount < dist.NumberOfProblems {
			hr.logger.Warn("Could not assign all required problems for difficulty",
				"difficulty", dist.Difficulty,
				"required", dist.NumberOfProblems,
				"assigned", assignedCount)
		}
	}

	hr.logger.Info("Completed code problem assignment",
		"event_id", event.ID.Bytes,
		"total_problems_assigned", totalAssigned)

	return nil
}
