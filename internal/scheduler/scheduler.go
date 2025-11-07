package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/client/rabbitmq"
	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

// EventScheduler handles the scheduling and processing of delayed event creation.
type EventScheduler struct {
	queries      *store.Queries
	db           *pgxpool.Pool
	rabbitClient *rabbitmq.RabbitMQClient
	logger       *slog.Logger
}

// NewEventScheduler creates a new EventScheduler instance.
func NewEventScheduler(queries *store.Queries, db *pgxpool.Pool, rabbitClient *rabbitmq.RabbitMQClient, logger *slog.Logger) *EventScheduler {
	return &EventScheduler{
		queries:      queries,
		db:           db,
		rabbitClient: rabbitClient,
		logger:       logger,
	}
}

// DelayedEventMessage represents the message structure for delayed event creation.
type DelayedEventMessage struct {
	EventRequestID uuid.UUID `json:"event_request_id"`
	ScheduledFor   time.Time `json:"scheduled_for"`
}

// ScheduleEventCreation schedules an event to be created at the proposed start time.
func (s *EventScheduler) ScheduleEventCreation(ctx context.Context, eventRequestID uuid.UUID, proposedStartTime time.Time) error {
	// Calculate the delay until the proposed start time
	delay := time.Until(proposedStartTime)

	// If the delay is negative or very small, process immediately
	if delay <= 0 {
		s.logger.Warn("Proposed start time is in the past or very near, processing immediately",
			"event_request_id", eventRequestID,
			"proposed_start_time", proposedStartTime)
		return s.ProcessScheduledEvent(ctx, eventRequestID)
	}

	message := DelayedEventMessage{
		EventRequestID: eventRequestID,
		ScheduledFor:   proposedStartTime,
	}

	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal delayed event message: %w", err)
	}

	// Publish the message with the calculated delay
	err = s.rabbitClient.PublishDelayed(ctx, payload, delay)
	if err != nil {
		return fmt.Errorf("failed to publish delayed event message: %w", err)
	}

	s.logger.Info("Scheduled event creation",
		"event_request_id", eventRequestID,
		"scheduled_for", proposedStartTime,
		"delay", delay)

	return nil
}

// ProcessScheduledEvent processes a scheduled event by creating rooms, assigning guilds, etc.
func (s *EventScheduler) ProcessScheduledEvent(ctx context.Context, eventRequestID uuid.UUID) error {
	s.logger.Info("Processing scheduled event", "event_request_id", eventRequestID)

	// Fetch the event request
	eventRequest, err := s.queries.GetEventRequestByID(ctx, toPgtypeUUID(eventRequestID))
	if err != nil {
		return fmt.Errorf("failed to fetch event request: %w", err)
	}

	// Check if the event was approved
	if eventRequest.Status != store.EventRequestStatusApproved {
		s.logger.Warn("Event request is not approved, skipping processing",
			"event_request_id", eventRequestID,
			"status", eventRequest.Status)
		return nil
	}

	// Check if the event was already created
	if !eventRequest.ApprovedEventID.Valid {
		s.logger.Error("Event request is approved but has no associated event ID",
			"event_request_id", eventRequestID)
		return fmt.Errorf("approved event ID not found for request %s", eventRequestID)
	}

	eventID := eventRequest.ApprovedEventID.Bytes

	// Fetch the event
	event, err := s.queries.GetEventByID(ctx, eventRequest.ApprovedEventID)
	if err != nil {
		return fmt.Errorf("failed to fetch event: %w", err)
	}

	s.logger.Info("Starting event setup", "event_id", eventID, "title", event.Title)

	// Get all participating guilds for this event
	participants, err := s.queries.GetEventParticipants(ctx, eventRequest.ApprovedEventID)
	if err != nil {
		return fmt.Errorf("failed to fetch event participants: %w", err)
	}

	if len(participants) == 0 {
		s.logger.Warn("No participants found for event", "event_id", eventID)
		return nil
	}

	// Get all rooms for this event
	rooms, err := s.queries.GetRoomsByEvent(ctx, eventRequest.ApprovedEventID)
	if err != nil {
		return fmt.Errorf("failed to fetch rooms: %w", err)
	}

	if len(rooms) == 0 {
		s.logger.Error("No rooms found for event", "event_id", eventID)
		return fmt.Errorf("no rooms found for event %s", eventID)
	}

	// Assign guilds to rooms (simple round-robin distribution)
	guildsPerRoom := int(event.GuildsPerRoom.Int32)
	if guildsPerRoom <= 0 {
		guildsPerRoom = len(participants) / len(rooms)
		if guildsPerRoom == 0 {
			guildsPerRoom = 1
		}
	}

	s.logger.Info("Assigning guilds to rooms",
		"total_guilds", len(participants),
		"total_rooms", len(rooms),
		"guilds_per_room", guildsPerRoom)

	// Use a transaction for atomic room assignment
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.queries.WithTx(tx)

	roomIndex := 0
	for i, participant := range participants {
		// Determine which room this guild should be assigned to
		roomIndex = (i / guildsPerRoom) % len(rooms)

		// Update the participant with their room assignment
		err = qtx.AssignGuildToRoom(ctx, store.AssignGuildToRoomParams{
			EventID: eventRequest.ApprovedEventID,
			GuildID: participant.GuildID,
			RoomID:  rooms[roomIndex].ID,
		})
		if err != nil {
			return fmt.Errorf("failed to assign guild %s to room %s: %w",
				participant.GuildID.Bytes, rooms[roomIndex].ID.Bytes, err)
		}

		s.logger.Info("Assigned guild to room",
			"guild_id", participant.GuildID.Bytes,
			"room_id", rooms[roomIndex].ID.Bytes,
			"room_name", rooms[roomIndex].Name)
	}

	// Commit the transaction
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit room assignments: %w", err)
	}

	s.logger.Info("Successfully processed scheduled event",
		"event_id", eventID,
		"guilds_assigned", len(participants),
		"rooms_used", len(rooms))

	return nil
}

// StartConsumer starts the background consumer that listens for delayed messages.
func (s *EventScheduler) StartConsumer(ctx context.Context) error {
	msgs, err := s.rabbitClient.ConsumeDelayed()
	if err != nil {
		return fmt.Errorf("failed to start consuming delayed messages: %w", err)
	}

	s.logger.Info("Event scheduler consumer started, waiting for delayed messages")

	go func() {
		for {
			select {
			case <-ctx.Done():
				s.logger.Info("Event scheduler consumer stopping")
				return
			case msg, ok := <-msgs:
				if !ok {
					s.logger.Error("Delayed messages channel closed")
					return
				}

				s.processDelayedMessage(msg)
			}
		}
	}()

	return nil
}

// processDelayedMessage handles incoming delayed messages.
func (s *EventScheduler) processDelayedMessage(msg amqp.Delivery) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.logger.Info("Received delayed message", "body_size", len(msg.Body))

	var delayedMsg DelayedEventMessage
	if err := json.Unmarshal(msg.Body, &delayedMsg); err != nil {
		s.logger.Error("Failed to unmarshal delayed message", "error", err)
		msg.Nack(false, false) // Don't requeue invalid messages
		return
	}

	s.logger.Info("Processing delayed event creation",
		"event_request_id", delayedMsg.EventRequestID,
		"scheduled_for", delayedMsg.ScheduledFor)

	// Process the scheduled event
	if err := s.ProcessScheduledEvent(ctx, delayedMsg.EventRequestID); err != nil {
		s.logger.Error("Failed to process scheduled event",
			"event_request_id", delayedMsg.EventRequestID,
			"error", err)
		// Nack and requeue for retry
		msg.Nack(false, true)
		return
	}

	// Acknowledge successful processing
	msg.Ack(false)
	s.logger.Info("Successfully processed delayed event",
		"event_request_id", delayedMsg.EventRequestID)
}

func toPgtypeUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: id,
		Valid: true,
	}
}
