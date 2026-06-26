package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/azrtydxb/novagrade/internal/pipeline"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// feedbackFakeProvider is a mock AIProvider for DraftFeedback tests.
// It matches on whether the user message content contains "Question: <qno>".
type feedbackFakeProvider struct {
	responses map[string]string // question_no → canned feedback text
	errs      map[string]error  // question_no → error to return
}

func (f *feedbackFakeProvider) Complete(_ context.Context, req providers.CompletionReq) (providers.CompletionResp, error) {
	content := ""
	if len(req.Messages) > 0 {
		content = req.Messages[len(req.Messages)-1].Content
	}
	// Check errors first.
	for qno, errVal := range f.errs {
		if strings.Contains(content, "Question: "+qno) {
			if errVal != nil {
				return providers.CompletionResp{}, errVal
			}
		}
	}
	// Then responses.
	for qno, fb := range f.responses {
		if strings.Contains(content, "Question: "+qno) {
			return providers.CompletionResp{
				Content:     fb,
				SchemaValid: true,
			}, nil
		}
	}
	return providers.CompletionResp{Content: "generic feedback"}, nil
}

// buildTestGradedPaper creates a test GradedPaper from a slice of questions.
func buildTestGradedPaper(questions []contracts.GradedQuestion) contracts.GradedPaper {
	return contracts.GradedPaper{
		Subject:       "Mathematics",
		SourcePDF:     "test.pdf",
		Questions:     questions,
		SectionTotals: map[string]float64{},
		Total:         10,
		MaxTotal:      20,
		Score100:      50,
	}
}

// TestDraftFeedback verifies that every question receives non-empty Feedback
// and that awarded_marks / max_marks are unchanged after DraftFeedback.
func TestDraftFeedback(t *testing.T) {
	questions := []contracts.GradedQuestion{
		{
			QuestionNo:    "1",
			MaxMarks:      5,
			AwardedMarks:  3,
			StudentAnswer: "The answer is 42",
			Justification: "Partially correct",
			Flags:         []string{},
		},
		{
			QuestionNo:    "2",
			MaxMarks:      10,
			AwardedMarks:  8,
			StudentAnswer: "Integration by parts",
			Justification: "Correct method",
			Flags:         []string{},
		},
	}
	input := buildTestGradedPaper(questions)

	prov := &feedbackFakeProvider{
		responses: map[string]string{
			"1": "Good attempt on question 1. You correctly identified the method but made a calculation error.",
			"2": "Excellent work on question 2. You applied integration by parts correctly.",
		},
		errs: map[string]error{},
	}

	result, err := pipeline.DraftFeedback(context.Background(), prov, "test-model", input)
	if err != nil {
		t.Fatalf("DraftFeedback returned unexpected error: %v", err)
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

		// Feedback must be non-empty.
		if q.Feedback == "" {
			t.Errorf("q[%d] (question_no=%q) got empty Feedback, want non-empty", i, q.QuestionNo)
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

// TestDraftFeedback_PerQuestionIsolation verifies that a provider error on one
// question does not fail the whole call: the failing question keeps empty
// Feedback while the others receive feedback; marks are untouched; DraftFeedback
// returns nil error.
func TestDraftFeedback_PerQuestionIsolation(t *testing.T) {
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
			"1": "Well done on question 1!",
			"3": "Good work on question 3.",
		},
		errs: map[string]error{
			"2": errors.New("provider timeout"),
		},
	}

	result, err := pipeline.DraftFeedback(context.Background(), prov, "test-model", input)
	if err != nil {
		t.Fatalf("DraftFeedback should not return error on per-question failure, got: %v", err)
	}

	if len(result.Questions) != 3 {
		t.Fatalf("question count changed: got %d, want 3", len(result.Questions))
	}

	// Q1 and Q3 must have feedback.
	if result.Questions[0].Feedback == "" {
		t.Errorf("q[0] (question_no=1) should have feedback after provider success")
	}
	if result.Questions[2].Feedback == "" {
		t.Errorf("q[2] (question_no=3) should have feedback after provider success")
	}

	// Q2 (the failing one) should have empty feedback (skipped).
	if result.Questions[1].Feedback != "" {
		t.Errorf("q[1] (question_no=2) should have empty feedback after provider error, got %q", result.Questions[1].Feedback)
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
