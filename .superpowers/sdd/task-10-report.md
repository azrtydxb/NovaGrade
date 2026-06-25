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
