// Package queue provides an AMQP-backed message bus backed by RabbitMQ.
// It declares a fixed topology of work queues each backed by a per-queue
// dead-letter queue (DLQ) and supports manual-ack consumption with
// configurable retry/dead-letter behaviour.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

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
}

// Connect dials the AMQP server at url and returns a ready-to-use *Bus.
// Call DeclareTopology before Publish / Consume.
func Connect(url string) (*Bus, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("queue: dial %s: %w", url, err)
	}
	return &Bus{
		conn:        conn,
		MaxAttempts: 3,
	}, nil
}

// Close releases the AMQP connection.
func (b *Bus) Close() error {
	return b.conn.Close()
}

// DeclareTopology declares the six work queues (as quorum queues) and their
// companion DLQs. Quorum queues are durable, replicated queues that carry the
// x-delivery-count header on each delivery, enabling broker-native retry
// accounting without any in-process state.
//
// It opens a temporary channel for the declarations and closes it on return.
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

		// Declare the work queue as a quorum queue with dead-letter routing
		// to the DLQ via the default exchange (empty string).
		// Quorum queues populate x-delivery-count on each redelivery, which
		// allows broker-native attempt counting across all consumer replicas
		// and across restarts.
		args := amqp.Table{
			"x-dead-letter-exchange":    "",
			"x-dead-letter-routing-key": dlq,
			"x-queue-type":              "quorum",
		}
		if _, err := ch.QueueDeclare(q, true, false, false, false, args); err != nil {
			return fmt.Errorf("queue: declare %s: %w", q, err)
		}
	}
	return nil
}

// Publish JSON-marshals env and publishes it to queue via the default
// exchange with persistent delivery mode.
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
// On handler error the delivery is nack'd using the broker-native x-delivery-count
// header (populated by quorum queues) to determine the attempt number:
//   - if deliveryCount + 1 < MaxAttempts the message is requeued (requeue=true)
//     so the broker redelivers it and increments x-delivery-count;
//   - once deliveryCount + 1 >= MaxAttempts the message is nack'd with
//     requeue=false, routing it to the DLQ via x-dead-letter-routing-key.
//
// On unmarshalable messages the delivery is nack'd immediately with requeue=false
// (dead-letter, no retries).
//
// Each call to Consume opens its own dedicated AMQP channel and sets QoS
// prefetch=1 so each consumer holds at most one unacked message at a time,
// providing even load distribution and bounded redelivery spikes on crash.
//
// Consume launches a goroutine and returns immediately (non-blocking).
// The goroutine exits when the delivery channel closes or ctx is cancelled,
// and closes its AMQP channel on exit.
func (b *Bus) Consume(ctx context.Context, queue string, handler func(contracts.Envelope) error) error {
	ch, err := b.conn.Channel()
	if err != nil {
		return fmt.Errorf("queue: open channel for consume %s: %w", queue, err)
	}

	// Set prefetch to 1: each consumer holds at most one unacked message,
	// ensuring even load distribution across replicas.
	if err := ch.Qos(1, 0, false); err != nil {
		ch.Close()
		return fmt.Errorf("queue: set QoS for consume %s: %w", queue, err)
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

// deliveryCount reads the x-delivery-count header from a quorum queue delivery.
// Quorum queues set this to 0 on first delivery and increment it on each
// redelivery. If the header is absent (e.g. DLQ consumers, classic queue
// fallback), 0 is returned.
func deliveryCount(d amqp.Delivery) int64 {
	if d.Headers == nil {
		return 0
	}
	v, ok := d.Headers["x-delivery-count"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	}
	return 0
}

// handleDelivery processes one AMQP delivery inside the consumer goroutine.
func (b *Bus) handleDelivery(d amqp.Delivery, handler func(contracts.Envelope) error) {
	var env contracts.Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		// Malformed message — send to DLQ immediately, no retries.
		if err := d.Nack(false, false); err != nil {
			log.Printf("queue: nack (unmarshal error) failed: %v", err)
		}
		return
	}

	if err := handler(env); err == nil {
		if err := d.Ack(false); err != nil {
			log.Printf("queue: ack failed: %v", err)
		}
		return
	}

	// Handler failed — use broker-native x-delivery-count for attempt accounting.
	// x-delivery-count is 0 on first delivery and increments on each redelivery,
	// so the current attempt number is deliveryCount + 1.
	count := deliveryCount(d)
	if count+1 >= int64(b.MaxAttempts) {
		// Exhausted retry budget — route to DLQ (requeue=false).
		if err := d.Nack(false, false); err != nil {
			log.Printf("queue: nack (dead-letter) failed: %v", err)
		}
	} else {
		// Still have attempts remaining — requeue so broker redelivers and
		// increments x-delivery-count.
		if err := d.Nack(false, true); err != nil {
			log.Printf("queue: nack (requeue) failed: %v", err)
		}
	}
}
