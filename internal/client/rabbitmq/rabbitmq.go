package rabbitmq

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	SseEventsExchange        = "sse_events_exchange"
	DelayedEventsExchange    = "delayed_events_exchange"
	DelayedEventsQueue       = "delayed_events_queue"
	ProcessDelayedEventsQueue = "process_delayed_events_queue"
)

// RabbitMQClient holds the connection and channel for RabbitMQ operations.
type RabbitMQClient struct {
	Conn    *amqp.Connection
	Channel *amqp.Channel
	logger  *slog.Logger
}

// NewRabbitMQClient establishes a connection, creates a channel, and declares the fanout exchange.
func NewRabbitMQClient(url string, logger *slog.Logger) (*RabbitMQClient, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, err
	}

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

	// Declare delayed events exchange (direct exchange)
	err = ch.ExchangeDeclare(
		DelayedEventsExchange, // name
		"direct",              // type
		true,                  // durable
		false,                 // auto-deleted
		false,                 // internal
		false,                 // no-wait
		nil,                   // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	// Declare the delayed queue with DLX (Dead Letter Exchange) configuration
	// Messages will sit here with a TTL, then get routed to the processing queue
	args := amqp.Table{
		"x-dead-letter-exchange": DelayedEventsExchange,
		"x-dead-letter-routing-key": "process",
	}
	_, err = ch.QueueDeclare(
		DelayedEventsQueue, // name
		true,               // durable
		false,              // delete when unused
		false,              // exclusive
		false,              // no-wait
		args,               // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	// Declare the processing queue (where delayed messages end up after TTL expires)
	_, err = ch.QueueDeclare(
		ProcessDelayedEventsQueue, // name
		true,                      // durable
		false,                     // delete when unused
		false,                     // exclusive
		false,                     // no-wait
		nil,                       // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	// Bind the processing queue to the delayed exchange
	err = ch.QueueBind(
		ProcessDelayedEventsQueue, // queue name
		"process",                 // routing key
		DelayedEventsExchange,     // exchange
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	logger.Info("Successfully connected to RabbitMQ and declared exchanges", "sse_exchange", SseEventsExchange, "delayed_exchange", DelayedEventsExchange)
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

// PublishDelayed sends a message to the delayed queue with a specified delay duration.
// After the delay expires, the message will be routed to the processing queue.
func (c *RabbitMQClient) PublishDelayed(ctx context.Context, body []byte, delay time.Duration) error {
	// Calculate TTL in milliseconds
	ttlMs := int64(delay / time.Millisecond)

	return c.Channel.PublishWithContext(ctx,
		"",                // exchange (empty for direct routing to queue)
		DelayedEventsQueue, // routing key (queue name)
		false,             // mandatory
		false,             // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         body,
			Expiration:   fmt.Sprintf("%d", ttlMs), // TTL in milliseconds as string
			DeliveryMode: amqp.Persistent,          // Make message persistent
		})
}

// ConsumeDelayed sets up consumption from the processing queue where delayed messages end up.
func (c *RabbitMQClient) ConsumeDelayed() (<-chan amqp.Delivery, error) {
	return c.Channel.Consume(
		ProcessDelayedEventsQueue, // queue
		"",                        // consumer
		false,                     // auto-ack (false so we can manually ack after processing)
		false,                     // exclusive
		false,                     // no-local
		false,                     // no-wait
		nil,                       // args
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
