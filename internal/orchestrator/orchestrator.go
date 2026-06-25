// Package orchestrator implements the NovaGrade submission state machine driver.
// It consumes command and result events from the message bus, drives state
// transitions, persists them to the store, and dispatches follow-up commands
// to stage work queues.
//
// This package was extracted from cmd/orchestrator/main.go so that the
// Orchestrator type and its port interfaces can be imported by integration tests
// without importing package main.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Port interfaces
// ─────────────────────────────────────────────────────────────────────────────

// SubmissionStore is the persistence port used by the Orchestrator.
type SubmissionStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	SetSubmissionState(ctx context.Context, id uuid.UUID, state contracts.SubmissionState) error
	FailSubmission(ctx context.Context, id uuid.UUID, stage, detail string) error
}

// MessageBus is the messaging port used by the Orchestrator.
type MessageBus interface {
	Publish(ctx context.Context, queue string, env contracts.Envelope) error
	Consume(ctx context.Context, queue string, handler func(contracts.Envelope) error) error
	MaxAttempts() int
}

// ArtifactStore is the object-store port used by the Orchestrator to fetch
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

// New creates an Orchestrator wired to the given store, bus, and object store.
// bucket is the object-store bucket that stage sidecars (e.g. the
// transcribe-result.json flags) are written to.
func New(db SubmissionStore, bus MessageBus, obj ArtifactStore, bucket string) *Orchestrator {
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
	if err := o.bus.Consume(ctx, "commands.q", o.HandleEnvelope); err != nil {
		return fmt.Errorf("orchestrator: consume commands.q: %w", err)
	}
	if err := o.bus.Consume(ctx, "results.q", o.HandleEnvelope); err != nil {
		return fmt.Errorf("orchestrator: consume results.q: %w", err)
	}

	// Consume the stage dead-letter queues so technical failures that exhausted
	// their retry budget are recorded as failed + error_detail.
	for dlq, stage := range stageDLQs {
		stage := stage // capture per-iteration
		if err := o.bus.Consume(ctx, dlq, o.DLQHandler(stage)); err != nil {
			return fmt.Errorf("orchestrator: consume %s: %w", dlq, err)
		}
	}

	<-ctx.Done()
	return ctx.Err()
}

// DLQHandler returns a handler that transitions a submission to failed and
// persists which stage failed (plus any available reason) in error_detail.
func (o *Orchestrator) DLQHandler(stage string) func(contracts.Envelope) error {
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

// HandleEnvelope processes a single envelope from the bus.
func (o *Orchestrator) HandleEnvelope(env contracts.Envelope) error {
	ctx := context.Background()

	subID, err := uuid.Parse(env.SubmissionID)
	if err != nil {
		return fmt.Errorf("orchestrator: invalid submission_id %q: %w", env.SubmissionID, err)
	}

	sub, err := o.db.GetSubmission(ctx, subID)
	if err != nil {
		return fmt.Errorf("orchestrator: get submission %s: %w", subID, err)
	}

	ev, err := StageToEvent(env.Stage)
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
	return o.dispatchNext(ctx, env, sub, next)
}

// dispatchNext publishes a follow-up command envelope to the appropriate queue
// based on the new state. Some states (human-review, terminal) have no dispatch.
// sub is the current submission row; it is consulted when the PayloadRef must be
// derived from stored data (e.g. SourcePDFKey when dispatching to render.q).
func (o *Orchestrator) dispatchNext(ctx context.Context, orig contracts.Envelope, sub store.Submission, state contracts.SubmissionState) error {
	var targetQueue string
	var stage string

	switch state {
	case contracts.StateQueued:
		// Dispatch to render.q; PayloadRef must point to the source PDF key which
		// was persisted in the submission row by the API handler.
		// Use sub.SourcePDFKey (from the DB row) so the render worker can locate
		// the PDF even when the originating envelope carried an empty PayloadRef
		// (as the API's submit_exam command does).
		if sub.SourcePDFKey != nil {
			orig.PayloadRef = *sub.SourcePDFKey
		}
		targetQueue = "render.q"
		stage = contracts.StageRender
	case contracts.StateSplittingPages, contracts.StateExtractingMetadata:
		// The render worker produces a single render.result that drives the submission
		// through splitting_pages → extracting_metadata → transcribing in three hops.
		// Re-publish the same render.result onto results.q so the orchestrator
		// advances the remaining intermediate states without requiring re-render.
		targetQueue = "results.q"
		stage = contracts.StageRenderResult
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
		// States like teacher_review, approved, published are intermediate states
		// that require human intervention or a subsequent command; no automatic dispatch.
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

// StageToEvent maps a contracts.Stage* constant (result event) or a command
// verb (arriving on commands.q) to a domain.Event. Technical failures are not
// mapped here — they are consumed from the per-stage DLQs (see DLQHandler).
func StageToEvent(stage string) (domain.Event, error) {
	switch stage {
	// Result events that arrive on results.q.
	case contracts.StageRenderResult,
		contracts.StageTranscribeResult,
		contracts.StageGradeResult,
		contracts.StagePublishResult,
		contracts.StageExportResult:
		return domain.EventStageSucceeded, nil

	// Command events that arrive on commands.q.
	case "submit", contracts.StageSubmitExam:
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
// the TranscribeFlags shape, and evaluates the quality gates.
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
