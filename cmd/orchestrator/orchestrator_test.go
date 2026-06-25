package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────────────

// fakeStore is an in-memory SubmissionStore that records every mutating call.
type fakeStore struct {
	sub store.Submission

	setStateCalls []contracts.SubmissionState
	failCalls     []failCall
}

type failCall struct {
	id     uuid.UUID
	stage  string
	detail string
}

func (f *fakeStore) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	if f.sub.ID != id {
		return store.Submission{}, store.ErrNotFound
	}
	return f.sub, nil
}

func (f *fakeStore) SetSubmissionState(_ context.Context, id uuid.UUID, state contracts.SubmissionState) error {
	f.setStateCalls = append(f.setStateCalls, state)
	f.sub.State = state
	return nil
}

func (f *fakeStore) FailSubmission(_ context.Context, id uuid.UUID, stage, detail string) error {
	f.failCalls = append(f.failCalls, failCall{id: id, stage: stage, detail: detail})
	f.sub.State = contracts.StateFailed
	return nil
}

// publishedMsg records a single Publish call.
type publishedMsg struct {
	queue string
	env   contracts.Envelope
}

// fakeBus is a MessageBus that records published messages. Consume is a no-op
// because the tests drive the handlers directly.
type fakeBus struct {
	published []publishedMsg
}

func (b *fakeBus) Publish(_ context.Context, queue string, env contracts.Envelope) error {
	b.published = append(b.published, publishedMsg{queue: queue, env: env})
	return nil
}

func (b *fakeBus) Consume(_ context.Context, _ string, _ func(contracts.Envelope) error) error {
	return nil
}

func (b *fakeBus) MaxAttempts() int { return 3 }

// fakeArtifacts is an ArtifactStore backed by an in-memory map keyed by
// "bucket/key". A missing key returns store.ErrNotFound.
type fakeArtifacts struct {
	objects map[string][]byte
}

func (a *fakeArtifacts) Get(_ context.Context, bucket, key string) ([]byte, error) {
	if data, ok := a.objects[bucket+"/"+key]; ok {
		return data, nil
	}
	return nil, store.ErrNotFound
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

const testBucket = "submissions"

// testTenant is the tenant UUID shared by the fake submission row and the
// envelopes the tests publish. The orchestrator's tenant-match guard compares
// env.TenantID against sub.TenantID, so both must agree for the happy paths.
var testTenant = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func mustFlags(t *testing.T, tf domain.TranscribeFlags) []byte {
	t.Helper()
	b, err := json.Marshal(tf)
	if err != nil {
		t.Fatalf("marshal flags: %v", err)
	}
	return b
}

func transcribeResultEnv(subID uuid.UUID, payloadRef string) contracts.Envelope {
	return contracts.Envelope{
		TenantID:     testTenant.String(),
		SubmissionID: subID.String(),
		Stage:        contracts.StageTranscribeResult,
		PayloadRef:   payloadRef,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// Happy path: a clean transcribe-result sidecar advances transcribing → grading,
// writes state exactly once, and dispatches exactly one message to grade.q.
func TestHandleEnvelope_CleanTranscript_AdvancesToGrading(t *testing.T) {
	subID := uuid.New()
	key := "tenant-1/" + subID.String() + "/transcribe-result.json"

	// A clean transcript: no low-read, no blanks, checksum OK.
	clean := domain.TranscribeFlags{
		QuestionCount:     10,
		LowReadConfidence: 0,
		BlankAnswers:      0,
		ChecksumOK:        true,
	}

	st := &fakeStore{sub: store.Submission{ID: subID, TenantID: testTenant, State: contracts.StateTranscribing}}
	bus := &fakeBus{}
	art := &fakeArtifacts{objects: map[string][]byte{
		testBucket + "/" + key: mustFlags(t, clean),
	}}
	o := NewOrchestrator(st, bus, art, testBucket)

	if err := o.HandleEnvelope(transcribeResultEnv(subID, key)); err != nil {
		t.Fatalf("HandleEnvelope: %v", err)
	}

	if len(st.setStateCalls) != 1 {
		t.Fatalf("want 1 SetSubmissionState call, got %d: %v", len(st.setStateCalls), st.setStateCalls)
	}
	if st.setStateCalls[0] != contracts.StateGrading {
		t.Fatalf("want state grading, got %s", st.setStateCalls[0])
	}
	if len(bus.published) != 1 {
		t.Fatalf("want 1 dispatch, got %d: %+v", len(bus.published), bus.published)
	}
	if bus.published[0].queue != "grade.q" {
		t.Fatalf("want dispatch to grade.q, got %s", bus.published[0].queue)
	}
	if bus.published[0].env.Stage != contracts.StageGrade {
		t.Fatalf("want stage grade, got %s", bus.published[0].env.Stage)
	}
	if len(st.failCalls) != 0 {
		t.Fatalf("want 0 FailSubmission calls, got %d", len(st.failCalls))
	}
}

// Gate path: a sidecar that trips a gate advances to transcription_review_required
// and dispatches nothing.
func TestHandleEnvelope_GateTripped_RequiresReview(t *testing.T) {
	subID := uuid.New()
	key := "tenant-1/" + subID.String() + "/transcribe-result.json"

	// Trips FlagLowReadConfidence (and FlagBlankOverThreshold).
	flagged := domain.TranscribeFlags{
		QuestionCount:     10,
		LowReadConfidence: 3,
		BlankAnswers:      2,
		ChecksumOK:        true,
	}

	st := &fakeStore{sub: store.Submission{ID: subID, TenantID: testTenant, State: contracts.StateTranscribing}}
	bus := &fakeBus{}
	art := &fakeArtifacts{objects: map[string][]byte{
		testBucket + "/" + key: mustFlags(t, flagged),
	}}
	o := NewOrchestrator(st, bus, art, testBucket)

	if err := o.HandleEnvelope(transcribeResultEnv(subID, key)); err != nil {
		t.Fatalf("HandleEnvelope: %v", err)
	}

	if len(st.setStateCalls) != 1 {
		t.Fatalf("want 1 SetSubmissionState call, got %d", len(st.setStateCalls))
	}
	if st.setStateCalls[0] != contracts.StateTranscriptionReviewRequired {
		t.Fatalf("want state transcription_review_required, got %s", st.setStateCalls[0])
	}
	if len(bus.published) != 0 {
		t.Fatalf("want 0 dispatches, got %d: %+v", len(bus.published), bus.published)
	}
}

// Idempotency: re-delivering a result/command whose computed next state equals
// the submission's CURRENT state (i.e. it has already been processed) writes
// state ZERO times AND dispatches ZERO messages. This proves Fix 1: the
// idempotent no-op short-circuit skips both the state write and the dispatch.
//
// The canonical no-op edge is a re-delivered "submit" command for a submission
// already in queued: NextState(queued, SubmitExam) == queued.
func TestHandleEnvelope_Redelivery_NoDoubleDispatch(t *testing.T) {
	subID := uuid.New()

	// Submission has ALREADY advanced to queued; the re-delivered submit is a
	// no-op (queued → queued).
	st := &fakeStore{sub: store.Submission{ID: subID, TenantID: testTenant, State: contracts.StateQueued}}
	bus := &fakeBus{}
	art := &fakeArtifacts{objects: map[string][]byte{}}
	o := NewOrchestrator(st, bus, art, testBucket)

	env := contracts.Envelope{
		TenantID:     testTenant.String(),
		SubmissionID: subID.String(),
		Stage:        "submit",
	}
	if err := o.HandleEnvelope(env); err != nil {
		t.Fatalf("HandleEnvelope: %v", err)
	}

	if len(st.setStateCalls) != 0 {
		t.Fatalf("want 0 SetSubmissionState calls on re-delivery, got %d: %v", len(st.setStateCalls), st.setStateCalls)
	}
	if len(bus.published) != 0 {
		t.Fatalf("want 0 dispatches on re-delivery, got %d: %+v", len(bus.published), bus.published)
	}
}

// Failure path: a message on transcribe.q.dlq transitions the submission to
// failed and persists error_detail naming the transcribe stage.
func TestDLQHandler_TranscribeFailure_MarksFailed(t *testing.T) {
	subID := uuid.New()

	st := &fakeStore{sub: store.Submission{ID: subID, TenantID: testTenant, State: contracts.StateTranscribing}}
	bus := &fakeBus{}
	art := &fakeArtifacts{objects: map[string][]byte{}}
	o := NewOrchestrator(st, bus, art, testBucket)

	env := contracts.Envelope{
		TenantID:     testTenant.String(),
		SubmissionID: subID.String(),
		Stage:        contracts.StageTranscribe,
		PayloadRef:   "tenant-1/" + subID.String() + "/render-result.json",
	}

	handler := o.DLQHandler(contracts.StageTranscribe)
	if err := handler(env); err != nil {
		t.Fatalf("dlqHandler: %v", err)
	}

	if len(st.failCalls) != 1 {
		t.Fatalf("want 1 FailSubmission call, got %d", len(st.failCalls))
	}
	got := st.failCalls[0]
	if got.id != subID {
		t.Fatalf("want fail for %s, got %s", subID, got.id)
	}
	if got.stage != contracts.StageTranscribe {
		t.Fatalf("want stage %q, got %q", contracts.StageTranscribe, got.stage)
	}
	if got.detail == "" {
		t.Fatalf("want non-empty error_detail")
	}
	// No state write or dispatch on the failure path.
	if len(st.setStateCalls) != 0 {
		t.Fatalf("want 0 SetSubmissionState calls, got %d", len(st.setStateCalls))
	}
	if len(bus.published) != 0 {
		t.Fatalf("want 0 dispatches, got %d", len(bus.published))
	}
}

// A missing sidecar must produce an error (so the message nacks/retries/DLQs),
// not a silent clean-transcript advance.
func TestHandleEnvelope_MissingSidecar_Errors(t *testing.T) {
	subID := uuid.New()
	key := "tenant-1/" + subID.String() + "/transcribe-result.json"

	st := &fakeStore{sub: store.Submission{ID: subID, TenantID: testTenant, State: contracts.StateTranscribing}}
	bus := &fakeBus{}
	art := &fakeArtifacts{objects: map[string][]byte{}} // sidecar absent
	o := NewOrchestrator(st, bus, art, testBucket)

	err := o.HandleEnvelope(transcribeResultEnv(subID, key))
	if err == nil {
		t.Fatalf("want error for missing sidecar, got nil")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want wrapped ErrNotFound, got %v", err)
	}
	if len(st.setStateCalls) != 0 || len(bus.published) != 0 {
		t.Fatalf("must not write state or dispatch on fetch failure")
	}
}

// FIX 1 (gate regression): a re-delivered grade.result for a submission ALREADY
// in teacher_review must NOT cross the approval gate. NextState(teacher_review,
// StageSucceeded) is now a no-op (teacher_review), so the orchestrator writes
// state ZERO times and dispatches ZERO messages — the gate cannot be crossed by
// message re-delivery; only an explicit teacher approval command may.
func TestHandleEnvelope_GradeResultRedelivered_DoesNotCrossApprovalGate(t *testing.T) {
	subID := uuid.New()

	st := &fakeStore{sub: store.Submission{ID: subID, TenantID: testTenant, State: contracts.StateTeacherReview}}
	bus := &fakeBus{}
	art := &fakeArtifacts{objects: map[string][]byte{}}
	o := NewOrchestrator(st, bus, art, testBucket)

	env := contracts.Envelope{
		TenantID:     testTenant.String(),
		SubmissionID: subID.String(),
		Stage:        contracts.StageGradeResult,
	}
	if err := o.HandleEnvelope(env); err != nil {
		t.Fatalf("HandleEnvelope: %v", err)
	}

	if len(st.setStateCalls) != 0 {
		t.Fatalf("want 0 SetSubmissionState calls (gate not crossed), got %d: %v", len(st.setStateCalls), st.setStateCalls)
	}
	if len(bus.published) != 0 {
		t.Fatalf("want 0 dispatches (gate not crossed), got %d: %+v", len(bus.published), bus.published)
	}
	if st.sub.State != contracts.StateTeacherReview {
		t.Fatalf("want submission still in teacher_review, got %s", st.sub.State)
	}
}

// FIX 3: an envelope whose TenantID does not match the persisted submission's
// tenant is dropped without transitioning state — no state write, no dispatch.
func TestHandleEnvelope_TenantMismatch_NoStateChangeNoDispatch(t *testing.T) {
	subID := uuid.New()

	st := &fakeStore{sub: store.Submission{ID: subID, TenantID: testTenant, State: contracts.StateTranscribing}}
	bus := &fakeBus{}
	art := &fakeArtifacts{objects: map[string][]byte{}}
	o := NewOrchestrator(st, bus, art, testBucket)

	// Envelope claims a DIFFERENT tenant than the persisted submission row.
	otherTenant := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	env := contracts.Envelope{
		TenantID:     otherTenant.String(),
		SubmissionID: subID.String(),
		Stage:        contracts.StageTranscribeResult,
		PayloadRef:   "x/" + subID.String() + "/transcribe-result.json",
	}
	if err := o.HandleEnvelope(env); err != nil {
		t.Fatalf("HandleEnvelope should drop (ack) on tenant mismatch, got err: %v", err)
	}

	if len(st.setStateCalls) != 0 {
		t.Fatalf("want 0 SetSubmissionState calls on tenant mismatch, got %d: %v", len(st.setStateCalls), st.setStateCalls)
	}
	if len(bus.published) != 0 {
		t.Fatalf("want 0 dispatches on tenant mismatch, got %d: %+v", len(bus.published), bus.published)
	}
}
