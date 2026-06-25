// Package domain contains the core business logic for the NovaGrade submission
// state machine and quality gate evaluation. It has no dependency on cmd/,
// internal/store/, or internal/queue/ — it is a pure domain layer.
package domain

import (
	"fmt"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// Event is something that happens to a submission that drives a state transition.
type Event string

const (
	EventSubmitExam        Event = "SubmitExam"
	EventApplyFix          Event = "ApplyFix"
	EventRetryStage        Event = "RetryStage"
	EventApproveForGrading Event = "ApproveForGrading"
	EventStageSucceeded    Event = "StageSucceeded"
	EventStageFailed       Event = "StageFailed"
	EventFlaggedForReview  Event = "FlaggedForReview"

	// Explicit teacher/admin COMMAND events. These are the ONLY way to cross the
	// teacher-approval gate and the publish/export edges that follow it. A bare
	// stage-result (EventStageSucceeded) must NEVER trigger these — that would
	// finalize a grade with no human action. (Phase 2 wires these to HTTP routes;
	// for now they exist in the machine but are not yet bound to a route.)
	EventApproveByTeacher Event = "ApproveByTeacher" // teacher_review → approved
	EventPublish          Event = "Publish"          // approved → published
	EventExport           Event = "Export"           // published → exported
)

// QualityFlag is a flag set when transcription quality is suspect.
type QualityFlag string

const (
	FlagLowReadConfidence  QualityFlag = "low_read_confidence"
	FlagChecksumMismatch   QualityFlag = "checksum_mismatch"
	FlagBlankOverThreshold QualityFlag = "blank_over_threshold"
	FlagStructuralAnomaly  QualityFlag = "structural_anomaly"
)

// NextState computes the next SubmissionState given current state, event, and
// quality flags. It uses contracts.CanTransition to validate edges.
//
// Idempotency: if the computed next state equals cur, (cur, nil) is returned
// without consulting CanTransition. This handles safe re-delivery of messages
// that were already processed.
//
// Returns an error if the event is unknown or the transition is not allowed.
func NextState(cur contracts.SubmissionState, ev Event, flags []QualityFlag) (contracts.SubmissionState, error) {
	next, err := computeNext(cur, ev, flags)
	if err != nil {
		return cur, err
	}

	// Idempotency: already in the target state.
	if next == cur {
		return cur, nil
	}

	if !contracts.CanTransition(cur, next) {
		return cur, fmt.Errorf("domain: transition not allowed: %s → %s (event %s)", cur, next, ev)
	}
	return next, nil
}

// computeNext maps (cur, event, flags) to the desired next state without
// checking CanTransition. The caller is responsible for validation.
func computeNext(cur contracts.SubmissionState, ev Event, flags []QualityFlag) (contracts.SubmissionState, error) {
	switch ev {
	case EventSubmitExam:
		// uploaded → queued; idempotent if already queued
		if cur == contracts.StateUploaded || cur == contracts.StateQueued {
			return contracts.StateQueued, nil
		}
		return cur, fmt.Errorf("domain: EventSubmitExam invalid from state %s", cur)

	case EventStageFailed:
		return contracts.StateFailed, nil

	case EventStageSucceeded:
		return stageSucceededNext(cur, flags)

	case EventFlaggedForReview:
		if cur == contracts.StateTranscribing {
			return contracts.StateTranscriptionReviewRequired, nil
		}
		return cur, fmt.Errorf("domain: EventFlaggedForReview invalid from state %s", cur)

	case EventApplyFix:
		if cur == contracts.StateTranscriptionReviewRequired {
			return contracts.StateTranscribing, nil
		}
		return cur, fmt.Errorf("domain: EventApplyFix invalid from state %s", cur)

	case EventRetryStage:
		switch cur {
		case contracts.StateTranscriptionReviewRequired:
			return contracts.StateTranscribing, nil
		case contracts.StateGradingReviewRequired:
			return contracts.StateGrading, nil
		}
		return cur, fmt.Errorf("domain: EventRetryStage invalid from state %s", cur)

	case EventApproveForGrading:
		if cur == contracts.StateTranscriptionReviewRequired {
			return contracts.StateGrading, nil
		}
		return cur, fmt.Errorf("domain: EventApproveForGrading invalid from state %s", cur)

	case EventApproveByTeacher:
		// The sole trigger for crossing the teacher-approval gate.
		if cur == contracts.StateTeacherReview {
			return contracts.StateApproved, nil
		}
		return cur, fmt.Errorf("domain: EventApproveByTeacher invalid from state %s", cur)

	case EventPublish:
		if cur == contracts.StateApproved {
			return contracts.StatePublished, nil
		}
		return cur, fmt.Errorf("domain: EventPublish invalid from state %s", cur)

	case EventExport:
		if cur == contracts.StatePublished {
			return contracts.StateExported, nil
		}
		return cur, fmt.Errorf("domain: EventExport invalid from state %s", cur)

	default:
		return cur, fmt.Errorf("domain: unknown event %q", ev)
	}
}

// stageSucceededNext resolves the next state for EventStageSucceeded based on
// the current state and any quality flags raised.
func stageSucceededNext(cur contracts.SubmissionState, flags []QualityFlag) (contracts.SubmissionState, error) {
	switch cur {
	case contracts.StateQueued:
		return contracts.StateSplittingPages, nil
	case contracts.StateSplittingPages:
		return contracts.StateExtractingMetadata, nil
	case contracts.StateExtractingMetadata:
		return contracts.StateTranscribing, nil
	case contracts.StateTranscribing:
		if len(flags) > 0 {
			return contracts.StateTranscriptionReviewRequired, nil
		}
		return contracts.StateGrading, nil
	case contracts.StateGrading:
		return contracts.StateTeacherReview, nil
	case contracts.StateTeacherReview:
		// HARD STOP at the teacher-approval gate. A stage-result event must NEVER
		// auto-advance out of teacher_review — only an explicit EventApproveByTeacher
		// command may. Returning teacher_review unchanged makes a re-delivered
		// grade.result an idempotent no-op (no state write, no dispatch) instead of
		// silently crossing the gate to approved.
		return contracts.StateTeacherReview, nil
	}
	return cur, fmt.Errorf("domain: EventStageSucceeded invalid from state %s", cur)
}
