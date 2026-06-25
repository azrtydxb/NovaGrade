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
	FailSubmission(ctx context.Context, id uuid.UUID, stage, detail string) error
}

// MessageBus is the messaging port used by the orchestrator.
type MessageBus interface {
	Publish(ctx context.Context, queue string, env contracts.Envelope) error
	Consume(ctx context.Context, queue string, handler func(contracts.Envelope) error) error
	MaxAttempts() int
}

// ArtifactStore is the object-store port used by the orchestrator to fetch
// stage sidecars (e.g. the transcribe-result.json flags) referenced by an
// envelope's PayloadRef. It is satisfied by *store.ObjStore.
type ArtifactStore interface {
	Get(ctx context.Context, bucket, key string) ([]byte, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Orchestrator
// ─────────────────────────────────────────────────────────────────────────────

// Orchestrator drives the submission state machine in response to bus events.
type Orchestrator struct {
	bus    MessageBus
	db     SubmissionStore
	obj    ArtifactStore
	bucket string
}

// NewOrchestrator creates an Orchestrator wired to the given store, bus, and
// object store. bucket is the object-store bucket that stage sidecars (e.g. the
// transcribe-result.json flags) are written to.
func NewOrchestrator(db SubmissionStore, bus MessageBus, obj ArtifactStore, bucket string) *Orchestrator {
	return &Orchestrator{db: db, bus: bus, obj: obj, bucket: bucket}
}

// stageDLQs maps each stage dead-letter queue (declared by DeclareTopology) to
// the stage label recorded in error_detail when a message lands there.
var stageDLQs = map[string]string{
	"render.q.dlq":     contracts.StageRender,
	"transcribe.q.dlq": contracts.StageTranscribe,
	"grade.q.dlq":      contracts.StageGrade,
}

// Start registers consumers on commands.q, results.q, and the per-stage DLQs,
// then blocks until ctx is cancelled.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.bus.Consume(ctx, "commands.q", o.handleEnvelope); err != nil {
		return fmt.Errorf("orchestrator: consume commands.q: %w", err)
	}
	if err := o.bus.Consume(ctx, "results.q", o.handleEnvelope); err != nil {
		return fmt.Errorf("orchestrator: consume results.q: %w", err)
	}

	// Consume the stage dead-letter queues so technical failures that exhausted
	// their retry budget are recorded as failed + error_detail.
	for dlq, stage := range stageDLQs {
		stage := stage // capture per-iteration
		if err := o.bus.Consume(ctx, dlq, o.dlqHandler(stage)); err != nil {
			return fmt.Errorf("orchestrator: consume %s: %w", dlq, err)
		}
	}

	<-ctx.Done()
	return ctx.Err()
}

// dlqHandler returns a handler that transitions a submission to failed and
// persists which stage failed (plus any available reason) in error_detail.
func (o *Orchestrator) dlqHandler(stage string) func(contracts.Envelope) error {
	return func(env contracts.Envelope) error {
		ctx := context.Background()

		subID, err := uuid.Parse(env.SubmissionID)
		if err != nil {
			return fmt.Errorf("orchestrator: dlq %s: invalid submission_id %q: %w", stage, env.SubmissionID, err)
		}

		detail := fmt.Sprintf("stage %q failed after exhausting retries", stage)
		if env.PayloadRef != "" {
			detail = fmt.Sprintf("%s (payload_ref=%s)", detail, env.PayloadRef)
		}

		if err := o.db.FailSubmission(ctx, subID, stage, detail); err != nil {
			return fmt.Errorf("orchestrator: dlq %s: FailSubmission %s: %w", stage, subID, err)
		}
		log.Printf("orchestrator: submission %s failed at stage %q (from DLQ)", subID, stage)
		return nil
	}
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

	ev, err := stageToEvent(env.Stage)
	if err != nil {
		return fmt.Errorf("orchestrator: map stage %q to event: %w", env.Stage, err)
	}

	// Evaluate quality gates when handling a transcription result. The flags
	// sidecar lives in object storage; env.PayloadRef is its key. A fetch or
	// decode failure is fatal — the message nacks/retries/DLQs rather than
	// being silently treated as a clean transcript.
	var flags []domain.QualityFlag
	if env.Stage == contracts.StageTranscribeResult {
		flags, err = o.transcribeFlags(ctx, env.PayloadRef)
		if err != nil {
			return fmt.Errorf("orchestrator: evaluate transcribe gates: %w", err)
		}
	}

	next, err := domain.NextState(sub.State, ev, flags)
	if err != nil {
		return fmt.Errorf("orchestrator: NextState(%s, %s): %w", sub.State, ev, err)
	}

	// Idempotent no-op: the state did not advance (e.g. a re-delivered result
	// for a submission already past this state). Skip BOTH the state write and
	// the dispatch so re-delivery never duplicates downstream work.
	if next == sub.State {
		return nil
	}

	// Persist the new state.
	if err := o.db.SetSubmissionState(ctx, subID, next); err != nil {
		return fmt.Errorf("orchestrator: SetSubmissionState: %w", err)
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

// stageToEvent maps a contracts.Stage* constant (result event) or a command
// verb (arriving on commands.q) to a domain.Event. Technical failures are not
// mapped here — they are consumed from the per-stage DLQs (see dlqHandler).
func stageToEvent(stage string) (domain.Event, error) {
	switch stage {
	// Result events that arrive on results.q.
	case contracts.StageRenderResult,
		contracts.StageTranscribeResult,
		contracts.StageGradeResult,
		contracts.StagePublishResult,
		contracts.StageExportResult:
		return domain.EventStageSucceeded, nil

	// Command events that arrive on commands.q.
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
	}
	return "", fmt.Errorf("unknown stage %q", stage)
}

// transcribeFlags fetches the transcribe-result.json sidecar referenced by
// payloadRef (an object-store key) from the configured bucket, decodes it into
// the TranscribeFlags shape, and evaluates the quality gates. A fetch or decode
// error is returned to the caller so the message can be retried/dead-lettered;
// it is NOT silently treated as a clean transcript.
func (o *Orchestrator) transcribeFlags(ctx context.Context, payloadRef string) ([]domain.QualityFlag, error) {
	if payloadRef == "" {
		return nil, fmt.Errorf("transcribeFlags: empty payload_ref")
	}
	data, err := o.obj.Get(ctx, o.bucket, payloadRef)
	if err != nil {
		return nil, fmt.Errorf("transcribeFlags: fetch sidecar %q/%q: %w", o.bucket, payloadRef, err)
	}
	var tf domain.TranscribeFlags
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, fmt.Errorf("transcribeFlags: decode sidecar %q: %w", payloadRef, err)
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

	// ── Object store ──────────────────────────────────────────────────────────
	// The orchestrator fetches stage sidecars (e.g. transcribe-result.json) to
	// evaluate quality gates. It must read from the same bucket the workers
	// write to (transcribe worker default: "submissions").
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
	orch := NewOrchestrator(st, &rabbitBusAdapter{bus}, obj, bucket)

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
