package contracts

import (
	"encoding/json"
	"testing"
)

func TestGradedPaperRoundTrip(t *testing.T) {
	expectedTotal := 50.0
	section := "A"

	original := GradedPaper{
		Subject:   "Mathematics",
		SourcePDF: "papers/math_2024.pdf",
		Questions: []GradedQuestion{
			{
				QuestionNo:      "1",
				Section:         &section,
				MaxMarks:        10.0,
				AwardedMarks:    8.5,
				StudentAnswer:   "x = 42",
				Justification:   "Correct method, minor arithmetic error.",
				GradeConfidence: 0.95,
				Flags:           []string{"partial_credit"},
			},
			{
				QuestionNo:      "2",
				Section:         nil,
				MaxMarks:        5.0,
				AwardedMarks:    5.0,
				StudentAnswer:   "y = 7",
				Justification:   "Fully correct.",
				GradeConfidence: 1.0,
				Flags:           []string{},
			},
		},
		SectionTotals: map[string]float64{"A": 8.5},
		Total:         13.5,
		MaxTotal:      15.0,
		Score100:      90.0,
		ExpectedTotal: &expectedTotal,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var roundTripped GradedPaper
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Compare scalar fields
	if roundTripped.Subject != original.Subject {
		t.Errorf("Subject: got %q, want %q", roundTripped.Subject, original.Subject)
	}
	if roundTripped.SourcePDF != original.SourcePDF {
		t.Errorf("SourcePDF: got %q, want %q", roundTripped.SourcePDF, original.SourcePDF)
	}
	if roundTripped.Total != original.Total {
		t.Errorf("Total: got %v, want %v", roundTripped.Total, original.Total)
	}
	if roundTripped.MaxTotal != original.MaxTotal {
		t.Errorf("MaxTotal: got %v, want %v", roundTripped.MaxTotal, original.MaxTotal)
	}
	if roundTripped.Score100 != original.Score100 {
		t.Errorf("Score100: got %v, want %v", roundTripped.Score100, original.Score100)
	}

	// Compare optional ExpectedTotal
	if roundTripped.ExpectedTotal == nil || *roundTripped.ExpectedTotal != *original.ExpectedTotal {
		t.Errorf("ExpectedTotal: got %v, want %v", roundTripped.ExpectedTotal, original.ExpectedTotal)
	}

	// Compare questions length
	if len(roundTripped.Questions) != len(original.Questions) {
		t.Fatalf("Questions length: got %d, want %d", len(roundTripped.Questions), len(original.Questions))
	}

	// Verify optional Section field round-trips correctly
	q0 := roundTripped.Questions[0]
	if q0.Section == nil || *q0.Section != section {
		t.Errorf("Questions[0].Section: got %v, want %q", q0.Section, section)
	}
	if roundTripped.Questions[1].Section != nil {
		t.Errorf("Questions[1].Section: expected nil, got %v", roundTripped.Questions[1].Section)
	}

	// Compare section totals
	for k, v := range original.SectionTotals {
		if roundTripped.SectionTotals[k] != v {
			t.Errorf("SectionTotals[%q]: got %v, want %v", k, roundTripped.SectionTotals[k], v)
		}
	}
}

// TestGradedQuestionRevisionRoundTrip verifies that the Revision field is
// correctly serialised (present when non-empty, absent when empty) and that
// awarded_marks / max_marks are unchanged after a marshal/unmarshal cycle.
func TestGradedQuestionRevisionRoundTrip(t *testing.T) {
	withRevision := GradedQuestion{
		QuestionNo:   "3",
		MaxMarks:     5.0,
		AwardedMarks: 2.0,
		StudentAnswer: "partial answer",
		Justification: "Missing key steps",
		GradeConfidence: 0.8,
		Flags:        []string{},
		Feedback:     "Good start but incomplete.",
		Revision:     "To improve, show all working and state each theorem used.",
	}
	withoutRevision := GradedQuestion{
		QuestionNo:   "4",
		MaxMarks:     3.0,
		AwardedMarks: 3.0,
		StudentAnswer: "full answer",
		Justification: "Fully correct",
		GradeConfidence: 1.0,
		Flags:        []string{},
		Feedback:     "Excellent.",
		Revision:     "", // omitempty — must not appear in JSON
	}

	// Round-trip withRevision.
	dataWith, err := json.Marshal(withRevision)
	if err != nil {
		t.Fatalf("marshal withRevision: %v", err)
	}
	var rtWith GradedQuestion
	if err := json.Unmarshal(dataWith, &rtWith); err != nil {
		t.Fatalf("unmarshal withRevision: %v", err)
	}
	if rtWith.Revision != withRevision.Revision {
		t.Errorf("Revision round-trip: got %q, want %q", rtWith.Revision, withRevision.Revision)
	}
	// Marks must not be affected.
	if rtWith.AwardedMarks != withRevision.AwardedMarks {
		t.Errorf("awarded_marks changed: got %v, want %v", rtWith.AwardedMarks, withRevision.AwardedMarks)
	}
	if rtWith.MaxMarks != withRevision.MaxMarks {
		t.Errorf("max_marks changed: got %v, want %v", rtWith.MaxMarks, withRevision.MaxMarks)
	}

	// Empty Revision must be absent from JSON (omitempty).
	dataWithout, err := json.Marshal(withoutRevision)
	if err != nil {
		t.Fatalf("marshal withoutRevision: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(dataWithout, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, present := raw["revision"]; present {
		t.Errorf("revision key present in JSON for empty Revision (omitempty violated)")
	}

	// Round-trip without-Revision preserves marks.
	var rtWithout GradedQuestion
	if err := json.Unmarshal(dataWithout, &rtWithout); err != nil {
		t.Fatalf("unmarshal withoutRevision: %v", err)
	}
	if rtWithout.AwardedMarks != withoutRevision.AwardedMarks {
		t.Errorf("awarded_marks changed: got %v, want %v", rtWithout.AwardedMarks, withoutRevision.AwardedMarks)
	}
	if rtWithout.MaxMarks != withoutRevision.MaxMarks {
		t.Errorf("max_marks changed: got %v, want %v", rtWithout.MaxMarks, withoutRevision.MaxMarks)
	}
}
