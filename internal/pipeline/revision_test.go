package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/azrtydxb/novagrade/internal/pipeline"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// revisionFakeProvider wraps feedbackFakeProvider for DraftRevisionSuggestions
// tests. It inspects the last message content for "Question: <qno>" to route
// canned responses or errors — identical pattern to feedbackFakeProvider.
//
// Note: feedbackFakeProvider is defined in feedback_test.go (same package).

// TestDraftRevisionSuggestions_AllQuestions verifies that every question in the
// paper receives a non-empty Revision and that awarded_marks / max_marks are
// unchanged.
func TestDraftRevisionSuggestions_AllQuestions(t *testing.T) {
	questions := []contracts.GradedQuestion{
		{
			QuestionNo:    "1",
			MaxMarks:      5,
			AwardedMarks:  3,
			StudentAnswer: "Partial",
			Justification: "Partially correct",
			Flags:         []string{},
		},
		{
			QuestionNo:    "2",
			MaxMarks:      10,
			AwardedMarks:  8,
			StudentAnswer: "Almost there",
			Justification: "Correct method",
			Flags:         []string{},
		},
	}
	input := buildTestGradedPaper(questions)

	prov := &feedbackFakeProvider{
		responses: map[string]string{
			"1": "To improve Q1, show your working step by step.",
			"2": "To improve Q2, explicitly state the theorem you applied.",
		},
		errs: map[string]error{},
	}

	result, err := pipeline.DraftRevisionSuggestions(context.Background(), prov, "test-model", input)
	if err != nil {
		t.Fatalf("DraftRevisionSuggestions returned unexpected error: %v", err)
	}

	if len(result.Questions) != len(input.Questions) {
		t.Fatalf("question count changed: got %d, want %d", len(result.Questions), len(input.Questions))
	}

	for i, q := range result.Questions {
		orig := input.Questions[i]

		// Marks must be unchanged.
		if q.AwardedMarks != orig.AwardedMarks {
			t.Errorf("q[%d] awarded_marks changed: got %v, want %v", i, q.AwardedMarks, orig.AwardedMarks)
		}
		if q.MaxMarks != orig.MaxMarks {
			t.Errorf("q[%d] max_marks changed: got %v, want %v", i, q.MaxMarks, orig.MaxMarks)
		}

		// Revision must be non-empty.
		if q.Revision == "" {
			t.Errorf("q[%d] (question_no=%q) got empty Revision, want non-empty", i, q.QuestionNo)
		}
	}

	// Paper-level marks are also unchanged.
	if result.Total != input.Total {
		t.Errorf("Total changed: got %v, want %v", result.Total, input.Total)
	}
	if result.MaxTotal != input.MaxTotal {
		t.Errorf("MaxTotal changed: got %v, want %v", result.MaxTotal, input.MaxTotal)
	}
	if result.Score100 != input.Score100 {
		t.Errorf("Score100 changed: got %v, want %v", result.Score100, input.Score100)
	}
}

// TestDraftRevisionSuggestions_PerQuestionIsolation verifies that a provider
// error on one question does not fail the whole call: the failing question keeps
// empty Revision while the others receive suggestions; marks are untouched;
// DraftRevisionSuggestions returns nil error.
func TestDraftRevisionSuggestions_PerQuestionIsolation(t *testing.T) {
	questions := []contracts.GradedQuestion{
		{
			QuestionNo:    "1",
			MaxMarks:      5,
			AwardedMarks:  5,
			StudentAnswer: "perfect",
			Justification: "Correct",
			Flags:         []string{},
		},
		{
			QuestionNo:    "2",
			MaxMarks:      5,
			AwardedMarks:  2,
			StudentAnswer: "wrong",
			Justification: "Incorrect",
			Flags:         []string{},
		},
		{
			QuestionNo:    "3",
			MaxMarks:      5,
			AwardedMarks:  4,
			StudentAnswer: "mostly right",
			Justification: "Mostly correct",
			Flags:         []string{},
		},
	}
	input := buildTestGradedPaper(questions)

	// Question 2 will error; questions 1 and 3 will succeed.
	prov := &feedbackFakeProvider{
		responses: map[string]string{
			"1": "Q1 revision hint.",
			"3": "Q3 revision hint.",
		},
		errs: map[string]error{
			"2": errors.New("provider timeout"),
		},
	}

	result, err := pipeline.DraftRevisionSuggestions(context.Background(), prov, "test-model", input)
	if err != nil {
		t.Fatalf("DraftRevisionSuggestions should not return error on per-question failure, got: %v", err)
	}

	if len(result.Questions) != 3 {
		t.Fatalf("question count changed: got %d, want 3", len(result.Questions))
	}

	// Q1 and Q3 must have revisions.
	if result.Questions[0].Revision == "" {
		t.Errorf("q[0] (question_no=1) should have revision after provider success")
	}
	if result.Questions[2].Revision == "" {
		t.Errorf("q[2] (question_no=3) should have revision after provider success")
	}

	// Q2 (the failing one) should have empty revision.
	if result.Questions[1].Revision != "" {
		t.Errorf("q[1] (question_no=2) should have empty revision after provider error, got %q", result.Questions[1].Revision)
	}

	// All marks must be unchanged.
	for i, q := range result.Questions {
		orig := input.Questions[i]
		if q.AwardedMarks != orig.AwardedMarks {
			t.Errorf("q[%d] awarded_marks changed: got %v, want %v", i, q.AwardedMarks, orig.AwardedMarks)
		}
		if q.MaxMarks != orig.MaxMarks {
			t.Errorf("q[%d] max_marks changed: got %v, want %v", i, q.MaxMarks, orig.MaxMarks)
		}
	}
}

// TestDraftRevisionSuggestions_MarksUnchanged verifies that the paper-level
// Total, MaxTotal, Score100, and SectionTotals are strictly unchanged even when
// all questions receive revisions.
func TestDraftRevisionSuggestions_MarksUnchanged(t *testing.T) {
	section := "A"
	questions := []contracts.GradedQuestion{
		{
			QuestionNo:    "1",
			Section:       &section,
			MaxMarks:      10,
			AwardedMarks:  7,
			StudentAnswer: "Good attempt",
			Justification: "Missing one step",
			Flags:         []string{},
		},
	}
	paper := contracts.GradedPaper{
		Subject:       "Physics",
		SourcePDF:     "test.pdf",
		Questions:     questions,
		SectionTotals: map[string]float64{"A": 7},
		Total:         7,
		MaxTotal:      10,
		Score100:      70,
	}

	prov := &feedbackFakeProvider{
		responses: map[string]string{
			"1": "Next time, explicitly state Newton's third law.",
		},
		errs: map[string]error{},
	}

	result, err := pipeline.DraftRevisionSuggestions(context.Background(), prov, "test-model", paper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Total != paper.Total {
		t.Errorf("Total changed: got %v, want %v", result.Total, paper.Total)
	}
	if result.MaxTotal != paper.MaxTotal {
		t.Errorf("MaxTotal changed: got %v, want %v", result.MaxTotal, paper.MaxTotal)
	}
	if result.Score100 != paper.Score100 {
		t.Errorf("Score100 changed: got %v, want %v", result.Score100, paper.Score100)
	}
	if result.SectionTotals["A"] != paper.SectionTotals["A"] {
		t.Errorf("SectionTotals[A] changed: got %v, want %v", result.SectionTotals["A"], paper.SectionTotals["A"])
	}
	if result.Questions[0].AwardedMarks != 7 {
		t.Errorf("awarded_marks changed: got %v, want 7", result.Questions[0].AwardedMarks)
	}
	if result.Questions[0].MaxMarks != 10 {
		t.Errorf("max_marks changed: got %v, want 10", result.Questions[0].MaxMarks)
	}
}

// TestDraftRevisionSuggestions_Idempotent verifies that questions with a
// non-empty Revision are skipped (the provider is never called for them), and
// that existing Revision values are preserved unchanged.
func TestDraftRevisionSuggestions_Idempotent(t *testing.T) {
	questions := []contracts.GradedQuestion{
		{
			QuestionNo:    "1",
			MaxMarks:      5,
			AwardedMarks:  3,
			StudentAnswer: "Initial answer",
			Justification: "Partially correct",
			Flags:         []string{},
			Revision:      "pre-existing revision hint",
		},
		{
			QuestionNo:    "2",
			MaxMarks:      10,
			AwardedMarks:  8,
			StudentAnswer: "Second answer",
			Justification: "Correct method",
			Flags:         []string{},
			Revision:      "", // should be filled
		},
	}
	input := buildTestGradedPaper(questions)

	// Track which questions the provider is called for.
	callTracker := map[string]bool{}
	prov := &callTrackingProvider{
		feedbackFakeProvider: &feedbackFakeProvider{
			responses: map[string]string{
				"1": "Should not be called for Q1",
				"2": "New revision hint for question 2",
			},
			errs: map[string]error{},
		},
		callTracker: callTracker,
	}

	result, err := pipeline.DraftRevisionSuggestions(context.Background(), prov, "test-model", input)
	if err != nil {
		t.Fatalf("DraftRevisionSuggestions returned unexpected error: %v", err)
	}

	if len(result.Questions) != 2 {
		t.Fatalf("question count changed: got %d, want 2", len(result.Questions))
	}

	// Q1 (already had revision) must NOT have been called on the provider.
	if callTracker["1"] {
		t.Errorf("q[0] (question_no=1) provider should NOT have been called (already had revision), but was")
	}

	// Q1.Revision must be unchanged.
	if result.Questions[0].Revision != "pre-existing revision hint" {
		t.Errorf("q[0] (question_no=1) Revision was changed: got %q, want %q",
			result.Questions[0].Revision, "pre-existing revision hint")
	}

	// Q2 (had empty revision) should HAVE been called on the provider.
	if !callTracker["2"] {
		t.Errorf("q[1] (question_no=2) provider should have been called (had empty revision), but was not")
	}

	// Q2 must have received non-empty revision.
	if result.Questions[1].Revision != "New revision hint for question 2" {
		t.Errorf("q[1] (question_no=2) Revision mismatch: got %q, want %q",
			result.Questions[1].Revision, "New revision hint for question 2")
	}

	// All marks must be unchanged.
	for i, q := range result.Questions {
		orig := input.Questions[i]
		if q.AwardedMarks != orig.AwardedMarks {
			t.Errorf("q[%d] awarded_marks changed: got %v, want %v", i, q.AwardedMarks, orig.AwardedMarks)
		}
		if q.MaxMarks != orig.MaxMarks {
			t.Errorf("q[%d] max_marks changed: got %v, want %v", i, q.MaxMarks, orig.MaxMarks)
		}
	}
}
