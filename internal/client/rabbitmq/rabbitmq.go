package rabbitmq

import (
	"context"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	SseEventsExchange    = "sse_events_exchange"
	EventAssignmentQueue = "event_assignment_queue" // Queue for guild-to-room assignments

	// Connection retry configuration
	maxRetries     = 10               // Maximum number of connection attempts
	initialBackoff = 1 * time.Second  // Initial backoff duration
	maxBackoff     = 30 * time.Second // Maximum backoff duration
)

// RabbitMQClient holds the connection and channel for RabbitMQ operations.
type RabbitMQClient struct {
	Conn    *amqp.Connection
	Channel *amqp.Channel
	logger  *slog.Logger
}

// NewRabbitMQClient establishes a connection with retry logic, creates a channel, and declares the fanout exchange.
// It will retry connection with exponential backoff if RabbitMQ is not immediately available.
func NewRabbitMQClient(url string, logger *slog.Logger) (*RabbitMQClient, error) {
	var conn *amqp.Connection
	var err error
	backoff := initialBackoff

	// Retry connection with exponential backoff
	for attempt := 1; attempt <= maxRetries; attempt++ {
		logger.Info("Attempting to connect to RabbitMQ",
			"attempt", attempt,
			"max_retries", maxRetries)

		conn, err = amqp.Dial(url)
		if err == nil {
			// Connection successful
			break
		}

		// Connection failed
		logger.Warn("Failed to connect to RabbitMQ, will retry",
			"attempt", attempt,
			"max_retries", maxRetries,
			"backoff", backoff,
			"error", err)

		if attempt == maxRetries {
			// Final attempt failed
			logger.Error("Failed to connect to RabbitMQ after all retries",
				"attempts", attempt,
				"error", err)
			return nil, err
		}

		// Wait before retrying
		time.Sleep(backoff)

		// Exponential backoff with cap
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	logger.Info("Successfully connected to RabbitMQ")

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, err
	}

	// Declare a durable fanout exchange. It will survive broker restarts.
	err = ch.ExchangeDeclare(
		SseEventsExchange, // name
		"fanout",          // type
		true,              // durable
		false,             // auto-deleted
		false,             // internal
		false,             // no-wait
		nil,               // arguments
	)

	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	// Declare the event assignment queue (for guild-to-room assignments)
	// This is a standard work queue - multiple consumers can process from it
	_, err = ch.QueueDeclare(
		EventAssignmentQueue, // name
		true,                 // durable (survives broker restart)
		false,                // delete when unused
		false,                // exclusive
		false,                // no-wait
		nil,                  // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	logger.Info("Successfully initialized RabbitMQ client",
		"sse_exchange", SseEventsExchange,
		"assignment_queue", EventAssignmentQueue)
	return &RabbitMQClient{Conn: conn, Channel: ch, logger: logger}, nil
}

// Publish sends a message to the fanout exchange.
func (c *RabbitMQClient) Publish(ctx context.Context, exchange, routingKey string, body []byte) error {
	return c.Channel.PublishWithContext(ctx,
		exchange,   // exchange
		routingKey, // routing key (ignored by fanout)
		false,      // mandatory
		false,      // immediate
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		})
}

// Consume sets up a temporary, exclusive queue bound to the fanout exchange
// and returns a channel of message deliveries.
func (c *RabbitMQClient) Consume() (<-chan amqp.Delivery, error) {
	// Declare an exclusive, auto-deleting queue with a server-generated name.
	// This queue is tied to the lifecycle of this connection.
	q, err := c.Channel.QueueDeclare(
		"",    // name (let RabbitMQ generate one)
		false, // durable
		true,  // delete when unused
		true,  // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		return nil, err
	}

	// Bind the new queue to our fanout exchange.
	err = c.Channel.QueueBind(
		q.Name,            // queue name
		"",                // routing key (ignored by fanout)
		SseEventsExchange, // exchange
		false,
		nil,
	)
	if err != nil {
		return nil, err
	}

	c.logger.Info("Queue declared and bound to exchange", "queue", q.Name, "exchange", SseEventsExchange)

	// Start consuming from the queue.
	return c.Channel.Consume(
		q.Name, // queue
		"",     // consumer
		true,   // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
}

// PublishEventAssignment publishes an event assignment message to the queue.
// This is called by the /start-pending-events endpoint to trigger guild assignments.
func (c *RabbitMQClient) PublishEventAssignment(ctx context.Context, body []byte) error {
	return c.Channel.PublishWithContext(ctx,
		"",                   // exchange (empty = default)
		EventAssignmentQueue, // routing key (queue name for default exchange)
		false,                // mandatory
		false,                // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         body,
			DeliveryMode: amqp.Persistent, // Persist messages to disk
		})
}

// ConsumeEventAssignments returns a channel of messages from the event assignment queue.
// Multiple instances can consume from this queue - RabbitMQ distributes messages among them.
func (c *RabbitMQClient) ConsumeEventAssignments() (<-chan amqp.Delivery, error) {
	return c.Channel.Consume(
		EventAssignmentQueue, // queue
		"",                   // consumer tag (empty = auto-generated)
		false,                // auto-ack (false = manual ack after processing)
		false,                // exclusive
		false,                // no-local
		false,                // no-wait
		nil,                  // args
	)
}

// Close gracefully shuts down the channel and connection.
func (c *RabbitMQClient) Close() {
	if c.Channel != nil {
		c.Channel.Close()
	}
	if c.Conn != nil {
		c.Conn.Close()
	}
	c.logger.Info("RabbitMQ connection closed.")
}
