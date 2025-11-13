package consumer

import (
	"context"
	"encoding/json"
	"time"

	"github.com/FA25SE050-RogueLearn/RogueLearn.CodeBattle/internal/handlers"
	amqp "github.com/rabbitmq/amqp091-go"
)

// EventAssignmentConsumer handles consuming and processing event assignment messages.
type EventAssignmentConsumer struct {
	handlerRepo *handlers.HandlerRepo
}

// NewEventAssignmentConsumer creates a new consumer instance.
func NewEventAssignmentConsumer(handlerRepo *handlers.HandlerRepo) *EventAssignmentConsumer {
	return &EventAssignmentConsumer{
		handlerRepo: handlerRepo,
	}
}

// Start begins consuming event assignment messages from RabbitMQ.
// This runs in a background goroutine and processes messages as they arrive.
//
// How it works:
// 1. All instances consume from the same queue
// 2. RabbitMQ distributes messages among instances (round-robin)
// 3. Each instance processes its assigned messages
// 4. Messages are acknowledged only after successful processing
//
// This approach:
// - ✅ No leader election needed
// - ✅ Work distributed across instances
// - ✅ Automatic retry if instance crashes (message requeued)
// - ✅ No duplicate processing (atomic status update in endpoint)
func (c *EventAssignmentConsumer) Start(ctx context.Context) error {
	msgs, err := c.handlerRepo.GetRabbitClient().ConsumeEventAssignments()
	if err != nil {
		return err
	}

	c.handlerRepo.GetLogger().Info("Event assignment consumer started")

	go func() {
		for {
			select {
			case <-ctx.Done():
				c.handlerRepo.GetLogger().Info("Event assignment consumer stopping")
				return

			case msg, ok := <-msgs:
				if !ok {
					c.handlerRepo.GetLogger().Error("Event assignment messages channel closed")
					return
				}

				c.processMessage(msg)
			}
		}
	}()

	return nil
}

// processMessage handles a single event assignment message.
func (c *EventAssignmentConsumer) processMessage(msg amqp.Delivery) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := c.handlerRepo.GetLogger()
	logger.Info("Received event assignment message", "body_size", len(msg.Body))

	// Parse the message
	var assignmentMsg handlers.EventAssignmentMessage
	if err := json.Unmarshal(msg.Body, &assignmentMsg); err != nil {
		logger.Error("Failed to unmarshal event assignment message", "error", err)
		msg.Nack(false, false) // Don't requeue invalid messages
		return
	}

	logger.Info("Processing event assignment",
		"event_id", assignmentMsg.EventID)

	// Process the event assignment
	if err := c.handlerRepo.ProcessEventAssignment(ctx, assignmentMsg.EventID); err != nil {
		logger.Error("Failed to process event assignment",
			"event_id", assignmentMsg.EventID,
			"error", err)
		// Nack and requeue for retry
		msg.Nack(false, true)
		return
	}

	// Acknowledge successful processing
	msg.Ack(false)
	logger.Info("Successfully processed event assignment",
		"event_id", assignmentMsg.EventID)
}
