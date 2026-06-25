// Command orchestrator consumes command and result events from the message bus,
// drives the submission state machine, persists state transitions, and dispatches
// next-stage commands to the appropriate work queues.
//
// # Design
//
// The orchestrator core logic lives in internal/orchestrator so it can be
// imported by integration tests without importing package main. This file is a
// thin wrapper: it reads environment variables, wires up the concrete
// dependencies (store, queue, object store), and delegates to
// internal/orchestrator.New(...).Start(ctx).
//
// Unit tests (package main) use the type aliases and wrapper functions defined
// below to call into the internal package with lightweight fakes.
//
// # Message flow
//
//  1. A worker publishes a result event (e.g. StageTranscribeResult) to results.q.
//  2. The orchestrator loads the submission, computes the next state, persists it,
//     and publishes the next command to the appropriate work queue.
//  3. Human-review states (transcription_review_required) have no automatic next
//     dispatch — an external API call must send an ApproveForGrading command to
//     commands.q.
package main

import (
	"context"
	"log"
	"os"
	"strconv"

	internalOrch "github.com/azrtydxb/novagrade/internal/orchestrator"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Port interface aliases — kept in main so that orchestrator_test.go (package
// main) can implement lightweight fakes without importing internal/orchestrator.
// ─────────────────────────────────────────────────────────────────────────────

// SubmissionStore is the persistence port used by the orchestrator.
type SubmissionStore = internalOrch.SubmissionStore

// MessageBus is the messaging port used by the orchestrator.
type MessageBus = internalOrch.MessageBus

// ArtifactStore is the object-store port used by the orchestrator.
type ArtifactStore = internalOrch.ArtifactStore

// Orchestrator is the state machine driver. It is an alias so that
// orchestrator_test.go can call o.handleEnvelope and o.dlqHandler directly.
type Orchestrator = internalOrch.Orchestrator

// NewOrchestrator creates an Orchestrator.
// This wrapper keeps orchestrator_test.go (package main) compiling as-is.
var NewOrchestrator = internalOrch.New

// ─────────────────────────────────────────────────────────────────────────────
// RabbitMQ bus adapter
// ─────────────────────────────────────────────────────────────────────────────

type rabbitBusAdapter struct {
	b *queue.Bus
}

func (a *rabbitBusAdapter) Publish(ctx context.Context, q string, env contracts.Envelope) error {
	return a.b.Publish(ctx, q, env)
}

func (a *rabbitBusAdapter) Consume(ctx context.Context, q string, handler func(contracts.Envelope) error) error {
	return a.b.Consume(ctx, q, handler)
}

func (a *rabbitBusAdapter) MaxAttempts() int { return a.b.MaxAttempts }

// ─────────────────────────────────────────────────────────────────────────────
// main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	ctx := context.Background()

	// ── Bus ──────────────────────────────────────────────────────────────────
	amqpURL := envRequired("AMQP_URL")
	bus, err := queue.Connect(amqpURL)
	if err != nil {
		log.Fatalf("orchestrator: connect to RabbitMQ: %v", err)
	}
	defer bus.Close()

	if err := bus.DeclareTopology(); err != nil {
		log.Fatalf("orchestrator: declare topology: %v", err)
	}

	// ── Store ─────────────────────────────────────────────────────────────────
	port := 5432
	if p := os.Getenv("PG_PORT"); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			log.Fatalf("orchestrator: invalid PG_PORT: %v", err)
		}
	}

	st, err := store.NewStore(ctx, store.DBConfig{
		Host:     envRequired("PG_HOST"),
		Port:     port,
		User:     envRequired("PG_USER"),
		Password: envRequired("PG_PASSWORD"),
		Database: envRequired("PG_DATABASE"),
		SSLMode:  envOrDefault("PG_SSLMODE", "disable"),
	})
	if err != nil {
		log.Fatalf("orchestrator: connect to Postgres: %v", err)
	}
	defer st.Close()

	// ── Object store ──────────────────────────────────────────────────────────
	bucket := envOrDefault("MINIO_BUCKET", "submissions")
	obj, err := store.New(store.Config{
		Endpoint:  envRequired("MINIO_ENDPOINT"),
		AccessKey: envRequired("MINIO_ACCESS_KEY"),
		SecretKey: envRequired("MINIO_SECRET_KEY"),
		UseSSL:    envOrDefault("MINIO_USE_SSL", "false") == "true",
	})
	if err != nil {
		log.Fatalf("orchestrator: connect to object store: %v", err)
	}

	// ── Orchestrator ──────────────────────────────────────────────────────────
	orch := internalOrch.New(st, &rabbitBusAdapter{bus}, obj, bucket)

	log.Println("orchestrator: started; listening on commands.q, results.q, and stage DLQs")
	if err := orch.Start(ctx); err != nil && err != context.Canceled {
		log.Fatalf("orchestrator: %v", err)
	}
}

func envRequired(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("orchestrator: required env var %q not set", key)
	}
	return v
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

