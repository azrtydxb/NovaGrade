package contracts

import "slices"

// SubmissionState is the canonical state of a submission in the grading pipeline.
type SubmissionState string

const (
	StateUploaded                    SubmissionState = "uploaded"
	StateQueued                      SubmissionState = "queued"
	StateSplittingPages              SubmissionState = "splitting_pages"
	StateExtractingMetadata          SubmissionState = "extracting_metadata"
	StateTranscribing                SubmissionState = "transcribing"
	StateTranscriptionReviewRequired SubmissionState = "transcription_review_required"
	StateGrading                     SubmissionState = "grading"
	StateGradingReviewRequired       SubmissionState = "grading_review_required"
	StateTeacherReview               SubmissionState = "teacher_review"
	StateApproved                    SubmissionState = "approved"
	StatePublished                   SubmissionState = "published"
	StateExported                    SubmissionState = "exported"
	StateFailed                      SubmissionState = "failed"
	StateArchived                    SubmissionState = "archived"
)

// transitions defines the allowed forward progressions in the grading pipeline.
// Rationale:
//   - Forward pipeline: uploaded → queued → splitting_pages → extracting_metadata →
//     transcribing → grading → teacher_review → approved → published → exported → archived
//   - Review loops: transcription_review_required loops back to transcribing;
//     grading_review_required loops back to grading.
//   - Any state except failed/archived can transition to failed (error escape hatch).
//   - No backward regression: approved cannot return to earlier pipeline states.
var transitions = map[SubmissionState][]SubmissionState{
	StateUploaded: {
		StateQueued,
		StateFailed,
	},
	StateQueued: {
		StateSplittingPages,
		StateFailed,
	},
	StateSplittingPages: {
		StateExtractingMetadata,
		StateFailed,
	},
	StateExtractingMetadata: {
		StateTranscribing,
		StateFailed,
	},
	StateTranscribing: {
		StateGrading,
		StateGradingReviewRequired,
		StateTranscriptionReviewRequired,
		StateFailed,
	},
	StateTranscriptionReviewRequired: {
		StateTranscribing,
		StateFailed,
	},
	StateGrading: {
		StateGradingReviewRequired,
		StateTeacherReview,
		StateFailed,
	},
	StateGradingReviewRequired: {
		StateGrading,
		StateFailed,
	},
	StateTeacherReview: {
		StateApproved,
		StateGrading,
		StateFailed,
	},
	StateApproved: {
		StatePublished,
		StateFailed,
	},
	StatePublished: {
		StateExported,
		StateArchived,
		StateFailed,
	},
	StateExported: {
		StateArchived,
		StateFailed,
	},
	StateFailed:   {},
	StateArchived: {},
}

// CanTransition reports whether transitioning from one SubmissionState to another is allowed.
func CanTransition(from, to SubmissionState) bool {
	allowed, ok := transitions[from]
	if !ok {
		return false
	}
	return slices.Contains(allowed, to)
}
