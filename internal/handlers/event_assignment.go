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

// StartPendingEventsHandler is called by the Alpine cron service every 60 seconds.
// It finds events whose assignment_date has passed and processes them directly.
//
// Flow:
// 1. Query events WHERE assignment_date <= NOW() AND status = 'pending'
// 2. Process each event directly with 30-second timeout per event
// 3. Atomic pending → active transition prevents duplicate processing
// 4. Return processing results
func (hr *HandlerRepo) StartPendingEventsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	hr.logger.Info("StartPendingEventsHandler called - checking for events ready for assignment")

	// Find all events that are ready for guild assignment
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
			Success: true,
			Msg:     "No events ready for assignment",
		})
		return
	}

	hr.logger.Info("Found events ready for assignment", "count", len(pendingEvents))

	successCount := 0
	skippedCount := 0
	failedEvents := []map[string]any{}

	// Process each event directly
	for _, event := range pendingEvents {
		eventID := uuid.UUID(event.ID.Bytes)

		hr.logger.Info("Processing event assignment",
			"event_id", eventID,
			"title", event.Title,
			"assignment_date", event.AssignmentDate.Time)

		// Process with 30-second timeout
		processCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := hr.ProcessEventAssignment(processCtx, eventID)
		cancel()

		if err != nil {
			// Check if event was already processed by checking the error message
			if strings.Contains(err.Error(), "not in 'pending' status") {
				hr.logger.Info("Event already processed, skipping",
					"event_id", eventID,
					"title", event.Title)
				skippedCount++
				continue
			}

			// Actual processing error
			hr.logger.Error("Failed to process event assignment",
				"event_id", eventID,
				"title", event.Title,
				"error", err)
			failedEvents = append(failedEvents, map[string]any{
				"event_id": eventID,
				"title":    event.Title,
				"error":    err.Error(),
			})
			continue
		}

		successCount++
		hr.logger.Info("Successfully processed event assignment",
			"event_id", eventID,
			"title", event.Title)
	}

	// Return response
	responseData := map[string]any{
		"events_found":     len(pendingEvents),
		"events_processed": successCount,
		"events_skipped":   skippedCount,
	}

	if len(failedEvents) > 0 {
		responseData["failed_events"] = failedEvents
	}

	response.JSON(w, response.JSONResponseParameters{
		Status:  http.StatusOK,
		Data:    responseData,
		Success: true,
		Msg:     "Succeeded",
	})
}

// ProcessEventAssignment processes a single event's guild-to-room assignment.
// This is called directly by StartPendingEventsHandler with a 30-second timeout.
//
// Flow (all within a single transaction):
// 1. Atomically update event status: pending → active (prevents duplicate processing)
// 2. Get all participating guilds
// 3. Get all rooms for the event
// 4. Assign guilds to rooms using round-robin distribution
// 5. Assign Code Problems and score distribution to each Code Problem to Event
//
// If any step fails, the transaction is rolled back and event remains 'pending'.
func (hr *HandlerRepo) ProcessEventAssignment(ctx context.Context, eventID uuid.UUID) error {
	hr.logger.Info("Processing event assignment", "event_id", eventID)

	// Start transaction for atomic processing
	tx, err := hr.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := hr.queries.WithTx(tx)

	// Atomically update status to 'active' (only if still 'pending')
	// This prevents duplicate processing if another cron cycle hits a different instance
	event, err := qtx.UpdateEventStatusToActive(ctx, toPgtypeUUID(eventID))
	if err != nil {
		// Check if this is a "no rows" error (event already processed)
		if err.Error() == "no rows in result set" || strings.Contains(err.Error(), "no rows") {
			return fmt.Errorf("event is not in 'pending' status, likely already processed")
		}
		return fmt.Errorf("failed to update event status: %w", err)
	}

	hr.logger.Info("Starting guild assignment for event",
		"event_id", eventID,
		"title", event.Title)

	// Get all participating guilds
	participants, err := qtx.GetEventParticipants(ctx, event.ID)
	if err != nil {
		return fmt.Errorf("failed to fetch event participants: %w", err)
	}

	if len(participants) == 0 {
		hr.logger.Warn("No participants found for event", "event_id", eventID)
		// Event is already marked as active, just commit and return
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}
		return nil
	}

	// Get all rooms for this event
	rooms, err := qtx.GetRoomsByEvent(ctx, event.ID)
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

	// Assign Code Problems and score distribution to Event
	err = hr.assignCodeProblemsToEvent(ctx, qtx, event)
	if err != nil {
		return fmt.Errorf("failed to assign code problems to event: %w", err)
	}

	// Commit the transaction (event is already marked as 'active' at the beginning)
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit event assignment: %w", err)
	}

	hr.logger.Info("Successfully completed event assignment",
		"event_id", eventID,
		"guilds_assigned", len(participants),
		"rooms_used", len(rooms))

	// Schedule event expiry timer
	// When timer fires, DB is updated and EventExpired is published to RabbitMQ
	endDate := event.EndDate.Time
	hr.eventHub.ScheduleEventExpiry(eventID, endDate)

	hr.logger.Info("Event expiry timer scheduled",
		"event_id", eventID,
		"end_date", endDate)

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
