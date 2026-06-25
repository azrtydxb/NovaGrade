package contracts

import "testing"

func TestValidTransition(t *testing.T) {
	if !CanTransition(StateTranscribing, StateGradingReviewRequired) && !CanTransition(StateTranscribing, StateGrading) {
		t.Fatal("transcribing must be able to advance")
	}
	if CanTransition(StateApproved, StateUploaded) {
		t.Fatal("approved must not regress to uploaded")
	}
}

func TestTransitions(t *testing.T) {
	tests := []struct {
		from SubmissionState
		to   SubmissionState
		want bool
	}{
		{StateFailed, StateTranscribing, false},
		{StateArchived, StateApproved, false},
		{StateTranscriptionReviewRequired, StateTranscribing, true},
		{StateGradingReviewRequired, StateGrading, true},
		{StateTeacherReview, StateApproved, true},
		{StateApproved, StateUploaded, false},
		{StateGrading, StateFailed, true},
		{StateUploaded, StateQueued, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransition(%v, %v) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}
