package queue_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func startRabbitMQ(t *testing.T) string {
	t.Helper()
	if os.Getenv("SKIP_DOCKER_TESTS") != "" || testing.Short() {
		t.Skip("requires Docker (set SKIP_DOCKER_TESTS to skip, or omit -short)")
	}
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		// rabbitmq:3.13-management includes quorum queue support by default.
		Image:        "rabbitmq:3.13",
		ExposedPorts: []string{"5672/tcp"},
		WaitingFor:   wait.ForListeningPort("5672/tcp").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5672/tcp")
	require.NoError(t, err)

	return fmt.Sprintf("amqp://guest:guest@%s:%s/", host, port.Port())
}

func TestBus_RoundTrip(t *testing.T) {
	url := startRabbitMQ(t)

	bus, err := queue.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })

	require.NoError(t, bus.DeclareTopology())

	want := contracts.Envelope{
		TenantID:      "tenant-abc",
		Principal:     "user-xyz",
		SubmissionID:  "sub-001",
		BatchID:       "batch-007",
		Stage:         "render",
		Attempt:       1,
		CorrelationID: "corr-999",
		PayloadRef:    "s3://bucket/key",
	}

	require.NoError(t, bus.Publish(context.Background(), "render.q", want))

	received := make(chan contracts.Envelope, 1)
	require.NoError(t, bus.Consume(context.Background(), "render.q", func(env contracts.Envelope) error {
		received <- env
		return nil
	}))

	select {
	case got := <-received:
		require.Equal(t, want.TenantID, got.TenantID)
		require.Equal(t, want.Principal, got.Principal)
		require.Equal(t, want.SubmissionID, got.SubmissionID)
		require.Equal(t, want.BatchID, got.BatchID)
		require.Equal(t, want.Stage, got.Stage)
		require.Equal(t, want.Attempt, got.Attempt)
		require.Equal(t, want.CorrelationID, got.CorrelationID)
		require.Equal(t, want.PayloadRef, got.PayloadRef)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestBus_DLQ(t *testing.T) {
	url := startRabbitMQ(t)

	bus, err := queue.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })

	require.NoError(t, bus.DeclareTopology())

	const maxAttempts = 2
	bus.MaxAttempts = maxAttempts

	// handlerCalls counts how many times the always-failing handler is invoked.
	// Using atomic so it's safe from the consumer goroutine.
	var handlerCalls atomic.Int64

	// Consume from render.q with a handler that always fails.
	require.NoError(t, bus.Consume(context.Background(), "render.q", func(env contracts.Envelope) error {
		handlerCalls.Add(1)
		return errors.New("simulated failure")
	}))

	// Publish one message.
	msg := contracts.Envelope{
		TenantID:     "dlq-tenant",
		SubmissionID: "dlq-sub",
		Stage:        "render",
	}
	require.NoError(t, bus.Publish(context.Background(), "render.q", msg))

	// Wait for the message to land in render.q.dlq.
	dlqReceived := make(chan contracts.Envelope, 1)
	require.NoError(t, bus.Consume(context.Background(), "render.q.dlq", func(env contracts.Envelope) error {
		dlqReceived <- env
		return nil
	}))

	select {
	case got := <-dlqReceived:
		require.Equal(t, msg.TenantID, got.TenantID)
		require.Equal(t, msg.SubmissionID, got.SubmissionID)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for DLQ message")
	}

	// Pin the exact boundary: the handler must have been called exactly
	// MaxAttempts times — no more (off-by-one would call it MaxAttempts+1),
	// no less (premature dead-lettering).
	require.Equal(t, int64(maxAttempts), handlerCalls.Load(),
		"handler must be invoked exactly MaxAttempts times before dead-lettering")
}
