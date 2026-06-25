# Task 10 Report: Orchestrator — Submission State Machine + Gate Rules

## NextState Signature + Event Enum Values

```go
func NextState(cur contracts.SubmissionState, ev Event, flags []QualityFlag) (contracts.SubmissionState, error)
```

Event constants:
- `EventSubmitExam` — uploaded → queued
- `EventStageSucceeded` — advances pipeline; routing depends on state + flags
- `EventStageFailed` — any non-terminal state → failed
- `EventFlaggedForReview` — transcribing → transcription_review_required
- `EventApplyFix` — transcription_review_required → transcribing
- `EventRetryStage` — transcription_review_required → transcribing; grading_review_required → grading
- `EventApproveForGrading` — transcription_review_required → grading

## QualityFlag Set + EvaluateGates Rules + Tunables Defaults

```go
type QualityFlag string

const (
    FlagLowReadConfidence  QualityFlag = "low_read_confidence"
    FlagChecksumMismatch   QualityFlag = "checksum_mismatch"
    FlagBlankOverThreshold QualityFlag = "blank_over_threshold"
    FlagStructuralAnomaly  QualityFlag = "structural_anomaly"
)
```

`EvaluateGates(tf TranscribeFlags, tun Tunables) []QualityFlag` rules:
- `tf.LowReadConfidence > 0` → `FlagLowReadConfidence`
- `!tf.ChecksumOK || (tf.ChecksumDifference != nil && *tf.ChecksumDifference != 0)` → `FlagChecksumMismatch`
- `tf.BlankAnswers > tun.BlankThreshold` → `FlagBlankOverThreshold`
- `tun.ExpectedQuestions > 0 && abs(tf.QuestionCount - tun.ExpectedQuestions) > tun.QuestionCountTol` → `FlagStructuralAnomaly`

`DefaultTunables()`:
- `BlankThreshold: 0` — any blank answer triggers the flag
- `ExpectedQuestions: 0` — unknown; disables structural anomaly check
- `QuestionCountTol: 2` — ±2 questions tolerated

## How Idempotency Is Enforced

Two layers:

1. **`NextState` itself**: The `computeNext` helper for `EventSubmitExam` returns `StateQueued` when `cur` is already `StateQueued` — so the idempotency check `if next == cur { return cur, nil }` catches re-deliveries without consulting `CanTransition`.

2. **Orchestrator**: Before calling `SetSubmissionState`, the orchestrator compares `next != sub.State`. If the state hasn't changed (already transitioned by a prior delivery), `SetSubmissionState` is skipped entirely.

## How StageFailed → failed_* Uses Attempt Count

The orchestrator's `stageToEvent` function receives `sub.Attempt` and `bus.MaxAttempts()`. The function maps a `stage_failed` stage command to `domain.EventStageFailed`, and `NextState` with that event always computes `contracts.StateFailed`. The attempt count enforcement is done at the bus layer (AMQP `x-delivery-count` vs `MaxAttempts`) before the orchestrator even receives the message — messages exhausting retry budget are routed to the DLQ by the bus. Within the orchestrator, any `stage_failed` stage command maps unconditionally to `EventStageFailed` → `failed`.

## Orchestrator Dispatch Map (State → Queue)

| New State | Queue | Stage |
|-----------|-------|-------|
| `transcribing` | `transcribe.q` | `transcribe` |
| `grading` | `grade.q` | `grade` |
| `transcription_review_required` | (none — await human) | — |
| `grading_review_required` | (none — await human) | — |
| `failed` | (none — terminal) | — |
| `exported`, `archived` | (none — pipeline complete) | — |
| others | (none — handled by render/other workers) | — |

## New CanTransition Edges Added

One new edge was added to `pkg/contracts/states.go`:

```go
StateTranscriptionReviewRequired: {
    StateTranscribing,
    StateGrading, // NEW: approve-for-grading skips re-transcription
    StateFailed,
},
```

This enables `EventApproveForGrading` to bypass re-transcription and go directly to grading when a reviewer confirms the transcript is acceptable as-is.

## TDD Evidence

### RED (tests written before implementation)

At first write, `go test ./internal/domain/...` failed with:
```
build error: package "github.com/azrtydxb/novagrade/internal/domain" not found
```

because the source files did not exist yet.

### GREEN (implementation makes tests pass)

After implementing `statemachine.go` and `gates.go`:

```
=== RUN   TestEvaluateGates_Clean
--- PASS: TestEvaluateGates_Clean (0.00s)
...
=== RUN   TestNextState_StageSucceeded_Pipeline/published->exported
--- PASS: TestNextState_StageSucceeded_Pipeline/published->exported (0.00s)
...
PASS
ok  github.com/azrtydxb/novagrade/internal/domain  0.919s
```

All 20 domain tests pass. Full suite: `go build ./... && go vet ./... && go test ./... -short` all green.

## Files Changed

| File | Action |
|------|--------|
| `internal/domain/statemachine.go` | Created — Event type, QualityFlag type, NextState function |
| `internal/domain/statemachine_test.go` | Created — 14 test functions |
| `internal/domain/gates.go` | Created — TranscribeFlags, Tunables, EvaluateGates |
| `internal/domain/gates_test.go` | Created — 10 test functions |
| `cmd/orchestrator/main.go` | Created — Orchestrator type, interfaces, main() wiring |
| `pkg/contracts/states.go` | Modified — added StateGrading to StateTranscriptionReviewRequired transitions |

## Concerns

1. **extractFlags in orchestrator**: The `extractFlags` function tries to JSON-decode `PayloadRef` directly. In production, `PayloadRef` is an object-store key (e.g. `tenant/submission/transcribe-result.json`) — the orchestrator would need an `ObjStore` client to fetch and decode the flags payload. The current implementation logs a warning and proceeds with no flags (treating the transcript as clean). This is intentional for the MVP but should be wired to real object storage in production.

2. **Context propagation**: `handleEnvelope` creates a `context.Background()` internally. A production version should pass a context with timeout derived from the consumer goroutine's context.

3. **No orchestrator unit tests with fakes**: The interfaces `SubmissionStore` and `MessageBus` are defined to enable fake-based tests, but no `orchestrator_test.go` was created. This is the next natural step.

4. **`stage_failed` stage name**: The `stageToEvent` mapping includes a `"stage_failed"` case, but there is no `contracts.StageFailed` constant defined. This is a string literal — should be promoted to a named constant in `pkg/contracts/envelope.go` for consistency.

---

## Critical review fixes

This follow-up commit resolves the correctness defects flagged in review. All
pure-domain signatures (`NextState`, `EvaluateGates`, `QualityFlag`,
`TranscribeFlags`, `Tunables`) are unchanged, and the orchestrator remains the
sole writer of submission state.

### Fix 1 — no double-dispatch on re-delivered messages
`handleEnvelope` now short-circuits the idempotent no-op case: when
`domain.NextState` returns `next == sub.State` the handler returns `nil`
immediately, skipping **both** `SetSubmissionState` and `dispatchNext`. Previously
`dispatchNext` ran even on a no-op, so a re-delivered result could re-publish a
work command. State is written and the next stage dispatched only when the state
actually advances.

### Fix 2 — gates fire: fetch the transcribe-result sidecar from object storage
`env.PayloadRef` on a `StageTranscribeResult` event is an object-store **key**
(`{tenant}/{submission}/transcribe-result.json`), not inline JSON, so the old
`extractFlags` always failed to decode and every paper routed to grading with
empty flags. The orchestrator now depends on a new `ArtifactStore` port
(`Get(ctx, bucket, key) ([]byte, error)`, satisfied by `*store.ObjStore`).
`(*Orchestrator).transcribeFlags` fetches the sidecar bytes by key from the
configured bucket, JSON-decodes them into `domain.TranscribeFlags`, and calls
`domain.EvaluateGates`. A fetch **or** decode failure (including an empty
`PayloadRef`) returns an error from the handler, so the delivery
nacks/retries/DLQs — it is never silently treated as a clean transcript. `main()`
constructs a real `*store.ObjStore` and the bucket (`MINIO_BUCKET`, default
`submissions`, matching the transcribe worker), and injects them; tests inject a
fake.

### Fix 3 — wire the failure path: consume the stage DLQs
`Start` now additionally consumes the three stage dead-letter queues declared by
`DeclareTopology` — `render.q.dlq`, `transcribe.q.dlq`, `grade.q.dlq` — each via
`dlqHandler(stage)`. On a DLQ delivery the submission is transitioned to the
single canonical `contracts.StateFailed` and `error_detail` is persisted with the
failing stage (derived from the queue name through the `stageDLQs` map) plus the
`payload_ref` for context. No `failed_render`/`failed_transcribe`/`failed_grade`
states were invented; the stage is recorded in `current_stage`/`error_detail`
instead. The dead `attempt >= maxAttempts` branch (both arms returned
`EventStageFailed`) and the now-unused `attempt`/`maxAttempts` parameters were
removed from `stageToEvent`.

### Fix 4 — persist error_detail (+ current_stage) via the store
Added `FailSubmission` as a sqlc `:execrows` query in
`internal/store/queries/submission.sql` that sets `state='failed'`,
`current_stage=$2`, `error_detail=$3`, `updated_at=now()`. Regenerated the db
package with sqlc (committed) and added the public method
`Store.FailSubmission(ctx, id, stage, detail) error`, returning `store.ErrNotFound`
when zero rows are affected. All existing store signatures are unchanged; this is
purely additive.

### Fix 5 — orchestrator wiring tests with fakes
`cmd/orchestrator/orchestrator_test.go` uses a fake `SubmissionStore`, fake
`MessageBus`, and fake `ArtifactStore`:

- `TestHandleEnvelope_CleanTranscript_AdvancesToGrading` — a clean sidecar
  advances `transcribing → grading`, asserts exactly one `SetSubmissionState`
  (to `grading`), exactly one dispatch to `grade.q` with stage `grade`, and zero
  `FailSubmission` calls.
- `TestHandleEnvelope_GateTripped_RequiresReview` — a sidecar tripping
  low-read/blank gates advances to `transcription_review_required`, asserts one
  state write and **zero** dispatches.
- `TestHandleEnvelope_Redelivery_NoDoubleDispatch` — a re-delivered event whose
  computed next state equals the current state (the `queued → queued` no-op)
  asserts **zero** `SetSubmissionState` calls and **zero** dispatches (proves
  Fix 1).
- `TestDLQHandler_TranscribeFailure_MarksFailed` — a message on
  `transcribe.q.dlq` asserts exactly one `FailSubmission` for the submission id
  with stage `transcribe` and non-empty `error_detail`, and zero state
  writes/dispatches (proves Fix 3 + 4).
- `TestHandleEnvelope_MissingSidecar_Errors` — a missing sidecar returns a
  wrapped `store.ErrNotFound` and writes no state / dispatches nothing (proves
  Fix 2's fail-closed behaviour).

Also removed the dead `Orchestrator.store` field (declared, never assigned).

### Verification

```
go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate   # clean
go build ./...   # clean
go vet ./...     # clean
go test ./...
?   github.com/azrtydxb/novagrade/cmd/ai-gateway      [no test files]
?   github.com/azrtydxb/novagrade/cmd/grade           [no test files]
ok  github.com/azrtydxb/novagrade/cmd/orchestrator
?   github.com/azrtydxb/novagrade/cmd/render          [no test files]
?   github.com/azrtydxb/novagrade/cmd/transcribe      [no test files]
ok  github.com/azrtydxb/novagrade/internal/domain
ok  github.com/azrtydxb/novagrade/internal/pipeline
ok  github.com/azrtydxb/novagrade/internal/pipeline/grade
ok  github.com/azrtydxb/novagrade/internal/providers
ok  github.com/azrtydxb/novagrade/internal/queue
ok  github.com/azrtydxb/novagrade/internal/store
?   github.com/azrtydxb/novagrade/internal/store/db    [no test files]
ok  github.com/azrtydxb/novagrade/internal/version
ok  github.com/azrtydxb/novagrade/pkg/contracts
```

### Stability note

Pure-domain signatures (`NextState`, `EvaluateGates`, `QualityFlag`,
`TranscribeFlags`, `Tunables`) are unchanged. Public store signatures are stable;
the only addition is the new `Store.FailSubmission` method. The orchestrator
gains a new `ArtifactStore` interface (additive). The orchestrator remains the
sole writer of submission state.

**Commit:** this commit
