// Package queue provides an AMQP-backed message bus backed by RabbitMQ.
// It declares a fixed topology of work queues each backed by a per-queue
// dead-letter queue (DLQ) and supports manual-ack consumption with
// configurable retry/dead-letter behaviour.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/azrtydxb/novagrade/pkg/contracts"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/google/uuid"
)

// workQueues are the canonical queues declared by DeclareTopology.
var workQueues = []string{
	"render.q",
	"transcribe.q",
	"grade.q",
	"feedback.q",
	"commands.q",
	"results.q",
}

// Bus wraps an AMQP connection, providing Publish / Consume helpers with
// manual acknowledgement and dead-letter routing.
//
// Each call to Publish, DeclareTopology, and Consume opens its own dedicated
// AMQP channel, which is required because amqp091-go channels are not safe
// for concurrent use across goroutines.
type Bus struct {
	conn *amqp.Connection

	// MaxAttempts is the number of delivery attempts before a message is
	// nack'd with requeue=false and routed to the DLQ.  Defaults to 3.
	MaxAttempts int

	// redeliveryCount tracks in-process retry counts for classic queues,
	// which do not carry x-delivery-count in the message headers.
	mu              sync.Mutex
	redeliveryCount map[string]int
}

// Connect dials the AMQP server at url and returns a ready-to-use *Bus.
// Call DeclareTopology before Publish / Consume.
func Connect(url string) (*Bus, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("queue: dial %s: %w", url, err)
	}
	return &Bus{
		conn:            conn,
		MaxAttempts:     3,
		redeliveryCount: make(map[string]int),
	}, nil
}

// Close releases the AMQP connection.
func (b *Bus) Close() error {
	return b.conn.Close()
}

// DeclareTopology declares the six work queues and their companion DLQs.
// It opens a temporary channel for the declarations and closes it on return.
//
// Queue type choice: we use x-queue-type = "classic" rather than "quorum"
// because testcontainers typically starts a single-node RabbitMQ instance,
// and quorum queues require a quorum of nodes (≥3 for HA) which makes them
// unreliable in ephemeral single-node environments.  Classic queues work
// correctly in both single-node and clustered deployments.
func (b *Bus) DeclareTopology() error {
	ch, err := b.conn.Channel()
	if err != nil {
		return fmt.Errorf("queue: open channel for topology: %w", err)
	}
	defer ch.Close()

	for _, q := range workQueues {
		dlq := q + ".dlq"

		// Declare the DLQ first (plain durable queue, no special args).
		if _, err := ch.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
			return fmt.Errorf("queue: declare DLQ %s: %w", dlq, err)
		}

		// Declare the work queue with dead-letter routing to the DLQ via the
		// default exchange (empty string) and queue-type = classic.
		args := amqp.Table{
			"x-dead-letter-exchange":    "",
			"x-dead-letter-routing-key": dlq,
			"x-queue-type":              "classic",
		}
		if _, err := ch.QueueDeclare(q, true, false, false, false, args); err != nil {
			return fmt.Errorf("queue: declare %s: %w", q, err)
		}
	}
	return nil
}

// Publish JSON-marshals env and publishes it to queue via the default
// exchange with persistent delivery mode.  A UUID MessageId is set so that
// the in-memory redelivery counter in Consume can key on a stable value.
//
// A fresh channel is opened for each Publish call and closed before returning,
// ensuring goroutine safety without shared channel state.
func (b *Bus) Publish(ctx context.Context, queue string, env contracts.Envelope) error {
	ch, err := b.conn.Channel()
	if err != nil {
		return fmt.Errorf("queue: open channel for publish: %w", err)
	}
	defer ch.Close()

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("queue: marshal envelope: %w", err)
	}
	msgID := uuid.New().String()
	msg := amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    msgID,
		Body:         body,
	}
	if err := ch.PublishWithContext(ctx, "", queue, false, false, msg); err != nil {
		return fmt.Errorf("queue: publish to %s: %w", queue, err)
	}
	return nil
}

// Consume registers a consumer on queue with manual acknowledgement.
// handler is called for each delivery; on success the delivery is ack'd.
// On handler error the delivery is nack'd:
//   - if the attempt count has not reached MaxAttempts the message is
//     requeued (requeue=true);
//   - once MaxAttempts is reached the message is discarded to the DLQ
//     (requeue=false, routed by x-dead-letter-routing-key).
//
// Each call to Consume opens its own dedicated AMQP channel, making
// concurrent Consume calls goroutine-safe.
//
// Consume launches a goroutine and returns immediately (non-blocking).
// The goroutine exits when the delivery channel closes or ctx is cancelled,
// and closes its AMQP channel on exit.
func (b *Bus) Consume(ctx context.Context, queue string, handler func(contracts.Envelope) error) error {
	ch, err := b.conn.Channel()
	if err != nil {
		return fmt.Errorf("queue: open channel for consume %s: %w", queue, err)
	}

	deliveries, err := ch.Consume(
		queue,
		"",    // server-generated consumer tag
		false, // autoAck=false: manual acknowledgement
		false, // exclusive
		false, // noLocal
		false, // noWait
		nil,
	)
	if err != nil {
		ch.Close()
		return fmt.Errorf("queue: consume %s: %w", queue, err)
	}

	go func() {
		defer ch.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-deliveries:
				if !ok {
					return
				}
				b.handleDelivery(d, handler)
			}
		}
	}()
	return nil
}

// handleDelivery processes one AMQP delivery inside the consumer goroutine.
func (b *Bus) handleDelivery(d amqp.Delivery, handler func(contracts.Envelope) error) {
	var env contracts.Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		// Malformed message — send to DLQ immediately.
		_ = d.Nack(false, false)
		return
	}

	if err := handler(env); err == nil {
		_ = d.Ack(false)
		return
	}

	// Handler failed — determine attempt count.
	// Classic queues do not populate x-delivery-count; we maintain an
	// in-process counter keyed by MessageId.
	count := b.incrementAttempt(d.MessageId)

	if count >= b.MaxAttempts {
		// Exceeded retry budget — route to DLQ.
		b.mu.Lock()
		delete(b.redeliveryCount, d.MessageId)
		b.mu.Unlock()
		_ = d.Nack(false, false)
	} else {
		_ = d.Nack(false, true)
	}
}

// incrementAttempt atomically increments and returns the attempt count for
// the given message ID.
func (b *Bus) incrementAttempt(msgID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.redeliveryCount[msgID]++
	return b.redeliveryCount[msgID]
}
