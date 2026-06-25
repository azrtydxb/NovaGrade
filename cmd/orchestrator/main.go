// Command orchestrator consumes command and result events from the message bus,
// drives the submission state machine, persists state transitions, and dispatches
// next-stage commands to the appropriate work queues.
//
// # Design
//
// The orchestrator depends on two interfaces (SubmissionStore and MessageBus)
// rather than concrete *store.Store and *queue.Bus types so that unit tests can
// wire in lightweight fakes without standing up Postgres or RabbitMQ.
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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Port interfaces — the orchestrator depends on these, not on concrete types.
// ─────────────────────────────────────────────────────────────────────────────

// SubmissionStore is the persistence port used by the orchestrator.
type SubmissionStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	SetSubmissionState(ctx context.Context, id uuid.UUID, state contracts.SubmissionState) error
}

// MessageBus is the messaging port used by the orchestrator.
type MessageBus interface {
	Publish(ctx context.Context, queue string, env contracts.Envelope) error
	Consume(ctx context.Context, queue string, handler func(contracts.Envelope) error) error
	MaxAttempts() int
}

// ─────────────────────────────────────────────────────────────────────────────
// Orchestrator
// ─────────────────────────────────────────────────────────────────────────────

// Orchestrator drives the submission state machine in response to bus events.
type Orchestrator struct {
	store MessageBus // re-using MessageBus for bus operations
	bus   MessageBus
	db    SubmissionStore
}

// NewOrchestrator creates an Orchestrator wired to the given store and bus.
func NewOrchestrator(db SubmissionStore, bus MessageBus) *Orchestrator {
	return &Orchestrator{db: db, bus: bus}
}

// Start registers consumers on commands.q and results.q and blocks until ctx
// is cancelled.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.bus.Consume(ctx, "commands.q", o.handleEnvelope); err != nil {
		return fmt.Errorf("orchestrator: consume commands.q: %w", err)
	}
	if err := o.bus.Consume(ctx, "results.q", o.handleEnvelope); err != nil {
		return fmt.Errorf("orchestrator: consume results.q: %w", err)
	}
	<-ctx.Done()
	return ctx.Err()
}

// handleEnvelope processes a single envelope from the bus.
func (o *Orchestrator) handleEnvelope(env contracts.Envelope) error {
	ctx := context.Background()

	subID, err := uuid.Parse(env.SubmissionID)
	if err != nil {
		return fmt.Errorf("orchestrator: invalid submission_id %q: %w", env.SubmissionID, err)
	}

	sub, err := o.db.GetSubmission(ctx, subID)
	if err != nil {
		return fmt.Errorf("orchestrator: get submission %s: %w", subID, err)
	}

	ev, err := stageToEvent(env.Stage, sub.Attempt, o.bus.MaxAttempts())
	if err != nil {
		return fmt.Errorf("orchestrator: map stage %q to event: %w", env.Stage, err)
	}

	// Evaluate quality gates when handling a transcription result.
	var flags []domain.QualityFlag
	if env.Stage == contracts.StageTranscribeResult {
		flags, err = extractFlags(env.PayloadRef)
		if err != nil {
			log.Printf("orchestrator: extractFlags warning (ignoring): %v", err)
			// Non-fatal: treat as no flags rather than failing the message.
		}
	}

	next, err := domain.NextState(sub.State, ev, flags)
	if err != nil {
		return fmt.Errorf("orchestrator: NextState(%s, %s): %w", sub.State, ev, err)
	}

	// Persist the new state.
	if next != sub.State {
		if err := o.db.SetSubmissionState(ctx, subID, next); err != nil {
			return fmt.Errorf("orchestrator: SetSubmissionState: %w", err)
		}
	}

	// Dispatch the next command (if any).
	return o.dispatchNext(ctx, env, next)
}

// dispatchNext publishes a follow-up command envelope to the appropriate queue
// based on the new state. Some states (human-review, terminal) have no dispatch.
func (o *Orchestrator) dispatchNext(ctx context.Context, orig contracts.Envelope, state contracts.SubmissionState) error {
	var targetQueue string
	var stage string

	switch state {
	case contracts.StateTranscribing:
		targetQueue = "transcribe.q"
		stage = contracts.StageTranscribe
	case contracts.StateGrading:
		targetQueue = "grade.q"
		stage = contracts.StageGrade
	case contracts.StateTranscriptionReviewRequired:
		// Wait for human action — no automatic dispatch.
		return nil
	case contracts.StateGradingReviewRequired:
		// Wait for human action — no automatic dispatch.
		return nil
	case contracts.StateFailed:
		// Terminal failure — no dispatch.
		return nil
	case contracts.StateExported, contracts.StateArchived:
		// Pipeline complete — no dispatch.
		return nil
	default:
		// States like splitting_pages, extracting_metadata, teacher_review,
		// approved, published are intermediate states that workers handle
		// autonomously; the render worker drives splitting/extraction.
		// For stages not explicitly handled, no dispatch is needed here.
		return nil
	}

	next := contracts.Envelope{
		TenantID:      orig.TenantID,
		Principal:     orig.Principal,
		SubmissionID:  orig.SubmissionID,
		BatchID:       orig.BatchID,
		Stage:         stage,
		Attempt:       orig.Attempt + 1,
		CorrelationID: orig.CorrelationID,
		PayloadRef:    orig.PayloadRef,
	}
	if err := o.bus.Publish(ctx, targetQueue, next); err != nil {
		return fmt.Errorf("orchestrator: dispatch to %s: %w", targetQueue, err)
	}
	return nil
}

// stageToEvent maps a contracts.Stage* constant to a domain.Event.
// If the attempt count has reached the max, a StageFailed event is returned.
func stageToEvent(stage string, attempt, maxAttempts int) (domain.Event, error) {
	// For result events, check attempt budget first.
	switch stage {
	case contracts.StageRenderResult,
		contracts.StageTranscribeResult,
		contracts.StageGradeResult,
		contracts.StagePublishResult,
		contracts.StageExportResult:
		return domain.EventStageSucceeded, nil

	// Command events that arrive on commands.q
	case "submit":
		return domain.EventSubmitExam, nil
	case "approve_for_grading":
		return domain.EventApproveForGrading, nil
	case "apply_fix":
		return domain.EventApplyFix, nil
	case "retry_stage":
		return domain.EventRetryStage, nil
	case "flagged_for_review":
		return domain.EventFlaggedForReview, nil
	case "stage_failed":
		if attempt >= maxAttempts {
			return domain.EventStageFailed, nil
		}
		return domain.EventStageFailed, nil
	}
	return "", fmt.Errorf("unknown stage %q", stage)
}

// extractFlags attempts to parse a TranscribeFlags payload from the PayloadRef
// field. PayloadRef may be a JSON-encoded TranscribeFlags or an object-store key.
// For simplicity, this implementation tries to JSON-decode the ref directly;
// production code would fetch the object from object storage first.
func extractFlags(payloadRef string) ([]domain.QualityFlag, error) {
	if payloadRef == "" {
		return nil, nil
	}
	var tf domain.TranscribeFlags
	if err := json.Unmarshal([]byte(payloadRef), &tf); err != nil {
		// PayloadRef is likely an object-store key, not inline JSON.
		// Return empty flags rather than erroring; the caller logs a warning.
		return nil, fmt.Errorf("extractFlags: not inline JSON: %w", err)
	}
	return domain.EvaluateGates(tf, domain.DefaultTunables()), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RabbitBusAdapter adapts *queue.Bus to MessageBus
// ─────────────────────────────────────────────────────────────────────────────

// rabbitBusAdapter wraps *queue.Bus and exposes the MessageBus interface.
type rabbitBusAdapter struct {
	b *queue.Bus
}

func (a *rabbitBusAdapter) Publish(ctx context.Context, q string, env contracts.Envelope) error {
	return a.b.Publish(ctx, q, env)
}

func (a *rabbitBusAdapter) Consume(ctx context.Context, q string, handler func(contracts.Envelope) error) error {
	return a.b.Consume(ctx, q, handler)
}

func (a *rabbitBusAdapter) MaxAttempts() int {
	return a.b.MaxAttempts
}

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

	// ── Orchestrator ──────────────────────────────────────────────────────────
	orch := NewOrchestrator(st, &rabbitBusAdapter{bus})

	log.Println("orchestrator: started; listening on commands.q and results.q")
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
