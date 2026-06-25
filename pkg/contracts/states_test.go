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
