package rabbitmq

import (
	"context"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"
)

const SseEventsExchange = "sse_events_exchange"

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

	logger.Info("Successfully connected to RabbitMQ and declared exchange", "exchange", SseEventsExchange)
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
