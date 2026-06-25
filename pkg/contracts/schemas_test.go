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
