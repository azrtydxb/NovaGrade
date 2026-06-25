package grade_test

import (
	"context"
	"errors"
	"testing"

	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock AIProvider
// ─────────────────────────────────────────────────────────────────────────────

// mockProvider records whether Complete was called and optionally returns an
// error or a canned response.
type mockProvider struct {
	called   bool
	response providers.CompletionResp
	err      error
}

func (m *mockProvider) Complete(_ context.Context, _ providers.CompletionReq) (providers.CompletionResp, error) {
	m.called = true
	return m.response, m.err
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 1: deterministic grading (exact_ci) — provider MUST NOT be called
// ─────────────────────────────────────────────────────────────────────────────

func TestGuideMarkScheme_ExactCI_Match_NoLLMCall(t *testing.T) {
	// Use two independent mocks so the assertion "provider must NOT be called"
	// is unambiguous: fallbackMock is wired to the LLMJudge fallback, and
	// schemeMock is wired to the GuideMarkScheme's own rubric provider.
	// Neither must be invoked for a deterministic exact_ci match.
	fallbackMock := &mockProvider{}
	schemeMock := &mockProvider{}
	guide := grade.Guide{
		"Q1": {MaxMarks: 2, Answer: "Paris", Match: "exact_ci"},
	}
	fallback := grade.NewLLMJudge(fallbackMock, "any-model")
	scheme := grade.NewGuideMarkScheme(guide, fallback, schemeMock, "any-model")

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q1",
		MaxMarks:       2,
		QuestionText:   "What is the capital of France?",
		StudentAnswer:  "paris", // matches "Paris" case-insensitively
		ReadConfidence: 0.95,
	}

	ctx := context.Background()
	gq, err := scheme.Grade(ctx, q)

	require.NoError(t, err)
	assert.Equal(t, 2.0, gq.AwardedMarks, "full marks expected on exact_ci match")
	assert.Equal(t, "matches marking guide", gq.Justification)
	assert.Equal(t, 1.0, gq.GradeConfidence)
	assert.NotNil(t, gq.Flags, "Flags must never be nil")

	// CRITICAL: neither the fallback LLMJudge nor the scheme's rubric provider
	// must have been called for a deterministic exact_ci match.
	assert.False(t, fallbackMock.called, "fallback AIProvider.Complete must NOT be called for exact_ci match")
	assert.False(t, schemeMock.called, "scheme AIProvider.Complete must NOT be called for exact_ci match")
}

func TestGuideMarkScheme_ExactCI_NoMatch_NoLLMCall(t *testing.T) {
	mock := &mockProvider{}
	guide := grade.Guide{
		"Q1": {MaxMarks: 2, Answer: "Paris", Match: "exact_ci"},
	}
	fallback := grade.NewLLMJudge(mock, "any-model")
	scheme := grade.NewGuideMarkScheme(guide, fallback, mock, "any-model")

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q1",
		MaxMarks:       2,
		QuestionText:   "What is the capital of France?",
		StudentAnswer:  "London", // wrong answer
		ReadConfidence: 0.95,
	}

	ctx := context.Background()
	gq, err := scheme.Grade(ctx, q)

	require.NoError(t, err)
	assert.Equal(t, 0.0, gq.AwardedMarks, "zero marks expected on non-match")
	assert.Equal(t, "does not match marking guide", gq.Justification)
	assert.False(t, mock.called, "AIProvider.Complete must NOT be called for exact_ci non-match")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 2: fallback path — question absent from guide → LLMJudge MUST be called
// ─────────────────────────────────────────────────────────────────────────────

func TestGuideMarkScheme_AbsentQuestion_FallsBackToLLM(t *testing.T) {
	mock := &mockProvider{
		response: providers.CompletionResp{
			Content: `{"awarded_marks": 3, "justification": "correct reasoning", "grade_confidence": 0.9}`,
		},
	}
	guide := grade.Guide{
		"Q1": {MaxMarks: 2, Answer: "Paris", Match: "exact_ci"},
		// Q2 is intentionally absent from the guide
	}
	fallback := grade.NewLLMJudge(mock, "grader-model")
	scheme := grade.NewGuideMarkScheme(guide, fallback, mock, "grader-model")

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q2",
		MaxMarks:       5,
		QuestionText:   "Explain photosynthesis.",
		StudentAnswer:  "Plants convert sunlight to energy using chlorophyll.",
		ReadConfidence: 0.88,
	}

	ctx := context.Background()
	gq, err := scheme.Grade(ctx, q)

	require.NoError(t, err)
	// The mock LLM returned 3 marks; the question's max_marks is 5, so 3 is within bounds.
	assert.Equal(t, 3.0, gq.AwardedMarks, "LLM-awarded marks should be used for absent question")
	assert.Equal(t, "correct reasoning", gq.Justification)
	assert.True(t, mock.called, "AIProvider.Complete MUST be called for question absent from guide")
	assert.NotNil(t, gq.Flags, "Flags must never be nil")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 3: exact match (case-sensitive)
// ─────────────────────────────────────────────────────────────────────────────

func TestGuideMarkScheme_Exact_CaseSensitive(t *testing.T) {
	mock := &mockProvider{}
	guide := grade.Guide{
		"Q1": {MaxMarks: 3, Answer: "H2O", Match: "exact"},
	}
	scheme := grade.NewGuideMarkScheme(guide, grade.NewLLMJudge(mock, "m"), mock, "m")
	ctx := context.Background()

	// Correct exact match
	gqOK, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
		QuestionNo: "Q1", MaxMarks: 3, StudentAnswer: "H2O", ReadConfidence: 1.0,
	})
	require.NoError(t, err)
	assert.Equal(t, 3.0, gqOK.AwardedMarks)
	assert.False(t, mock.called)

	// Wrong case
	gqBad, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
		QuestionNo: "Q1", MaxMarks: 3, StudentAnswer: "h2o", ReadConfidence: 1.0,
	})
	require.NoError(t, err)
	assert.Equal(t, 0.0, gqBad.AwardedMarks)
	assert.False(t, mock.called, "exact match must not call LLM")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 4: set match
// ─────────────────────────────────────────────────────────────────────────────

func TestGuideMarkScheme_Set_Match(t *testing.T) {
	mock := &mockProvider{}
	guide := grade.Guide{
		"Q1": {MaxMarks: 1, Accept: []string{"cat", "feline"}, Match: "set"},
	}
	scheme := grade.NewGuideMarkScheme(guide, grade.NewLLMJudge(mock, "m"), mock, "m")
	ctx := context.Background()

	for _, ans := range []string{"cat", "Cat", "FELINE", "feline"} {
		gq, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
			QuestionNo: "Q1", MaxMarks: 1, StudentAnswer: ans, ReadConfidence: 1.0,
		})
		require.NoError(t, err)
		assert.Equal(t, 1.0, gq.AwardedMarks, "expected match for %q", ans)
	}
	assert.False(t, mock.called, "set match must not call LLM")

	// Non-member
	gq, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
		QuestionNo: "Q1", MaxMarks: 1, StudentAnswer: "dog", ReadConfidence: 1.0,
	})
	require.NoError(t, err)
	assert.Equal(t, 0.0, gq.AwardedMarks)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 5: blank answer — zero marks, blank_answer flag, no LLM call
// ─────────────────────────────────────────────────────────────────────────────

func TestGuideMarkScheme_BlankAnswer_NoLLMCall(t *testing.T) {
	mock := &mockProvider{}
	guide := grade.Guide{
		"Q1": {MaxMarks: 4, Answer: "Paris", Match: "exact_ci"},
	}
	scheme := grade.NewGuideMarkScheme(guide, grade.NewLLMJudge(mock, "m"), mock, "m")
	ctx := context.Background()

	gq, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
		QuestionNo: "Q1", MaxMarks: 4, StudentAnswer: "   ", ReadConfidence: 1.0,
	})
	require.NoError(t, err)
	assert.Equal(t, 0.0, gq.AwardedMarks)
	assert.Contains(t, gq.Flags, "blank_answer")
	assert.False(t, mock.called, "blank answer must not call LLM")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 6: GradePaper — totals, section_totals, score_100
// ─────────────────────────────────────────────────────────────────────────────

func TestGradePaper_Totals(t *testing.T) {
	mock := &mockProvider{}
	sectionA := "A"
	sectionB := "B"

	guide := grade.Guide{
		"Q1": {MaxMarks: 2, Answer: "Paris", Match: "exact_ci"},
		"Q2": {MaxMarks: 3, Answer: "H2O", Match: "exact"},
	}
	fallback := grade.NewLLMJudge(mock, "m")
	scheme := grade.NewGuideMarkScheme(guide, fallback, mock, "m")

	paper := contracts.TranscribedPaper{
		Subject:   "Science",
		SourcePDF: "exam.pdf",
		Questions: []contracts.TranscribedQuestion{
			{QuestionNo: "Q1", Section: &sectionA, MaxMarks: 2, StudentAnswer: "paris", ReadConfidence: 0.9},
			{QuestionNo: "Q2", Section: &sectionB, MaxMarks: 3, StudentAnswer: "H2O", ReadConfidence: 0.9},
		},
	}

	ctx := context.Background()
	gp, err := grade.GradePaper(ctx, scheme, paper)
	require.NoError(t, err)

	assert.Equal(t, 5.0, gp.Total)
	assert.Equal(t, 5.0, gp.MaxTotal)
	assert.Equal(t, 100.0, gp.Score100)
	assert.Equal(t, 2.0, gp.SectionTotals["A"])
	assert.Equal(t, 3.0, gp.SectionTotals["B"])
	assert.False(t, mock.called, "no LLM calls for deterministic guide questions")
}

func TestGradePaper_Score100_Partial(t *testing.T) {
	mock := &mockProvider{}
	guide := grade.Guide{
		"Q1": {MaxMarks: 10, Answer: "correct", Match: "exact"},
	}
	scheme := grade.NewGuideMarkScheme(guide, grade.NewLLMJudge(mock, "m"), mock, "m")

	paper := contracts.TranscribedPaper{
		Questions: []contracts.TranscribedQuestion{
			{QuestionNo: "Q1", MaxMarks: 10, StudentAnswer: "wrong", ReadConfidence: 0.9},
		},
	}

	gp, err := grade.GradePaper(context.Background(), scheme, paper)
	require.NoError(t, err)
	assert.Equal(t, 0.0, gp.Score100)
	assert.Equal(t, 0.0, gp.Total)
	assert.Equal(t, 10.0, gp.MaxTotal)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 7: per-question isolation — one failing question does not abort others
// ─────────────────────────────────────────────────────────────────────────────

// errorScheme always returns an error from Grade.
type errorScheme struct{}

func (e *errorScheme) Grade(_ context.Context, q contracts.TranscribedQuestion) (contracts.GradedQuestion, error) {
	return contracts.GradedQuestion{}, errors.New("simulated grading failure")
}

func TestGradePaper_PerQuestionIsolation(t *testing.T) {
	guide := grade.Guide{
		"Q1": {MaxMarks: 5, Answer: "ok", Match: "exact"},
	}
	// Use errorScheme as fallback so Q2 (absent from guide) triggers a failure.
	scheme := grade.NewGuideMarkScheme(guide, &errorScheme{}, nil, "")

	paper := contracts.TranscribedPaper{
		Questions: []contracts.TranscribedQuestion{
			{QuestionNo: "Q1", MaxMarks: 5, StudentAnswer: "ok", ReadConfidence: 1.0},
			{QuestionNo: "Q2", MaxMarks: 3, StudentAnswer: "something", ReadConfidence: 1.0},
		},
	}

	gp, err := grade.GradePaper(context.Background(), scheme, paper)
	require.NoError(t, err, "GradePaper must not return an error on per-question failure")

	require.Len(t, gp.Questions, 2)

	// Q1 passed deterministically
	assert.Equal(t, 5.0, gp.Questions[0].AwardedMarks)
	assert.NotContains(t, gp.Questions[0].Flags, "grading_failed")

	// Q2 was isolated: zero marks, grading_failed flag
	assert.Equal(t, 0.0, gp.Questions[1].AwardedMarks)
	assert.Contains(t, gp.Questions[1].Flags, "grading_failed")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 8: Flags are never nil
// ─────────────────────────────────────────────────────────────────────────────

func TestGradedQuestion_FlagsNeverNil(t *testing.T) {
	mock := &mockProvider{
		response: providers.CompletionResp{
			Content: `{"awarded_marks": 2, "justification": "ok", "grade_confidence": 0.8}`,
		},
	}
	judge := grade.NewLLMJudge(mock, "m")
	ctx := context.Background()

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q1",
		MaxMarks:       5,
		StudentAnswer:  "some answer",
		ReadConfidence: 0.9,
	}

	gq, err := judge.Grade(ctx, q)
	require.NoError(t, err)
	assert.NotNil(t, gq.Flags, "Flags must never be nil")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 9: LoadGuideFromJSON round-trip
// ─────────────────────────────────────────────────────────────────────────────

func TestLoadGuideFromJSON(t *testing.T) {
	raw := []byte(`{
		"Q1": {"max_marks": 2, "answer": "Paris", "match": "exact_ci"},
		"Q2": {"max_marks": 3, "answer": "H2O",   "match": "exact"},
		"Q3": {"max_marks": 1, "accept": ["cat", "feline"], "match": "set"},
		"Q4": {"max_marks": 5, "rubric": "Award marks for...", "match": "rubric"}
	}`)

	g, err := grade.LoadGuideFromJSON(raw)
	require.NoError(t, err)
	require.Len(t, g, 4)

	assert.Equal(t, 2.0, g["Q1"].MaxMarks)
	assert.Equal(t, "Paris", g["Q1"].Answer)
	assert.Equal(t, "exact_ci", g["Q1"].Match)
	assert.Equal(t, []string{"cat", "feline"}, g["Q3"].Accept)
	assert.Equal(t, "Award marks for...", g["Q4"].Rubric)
	assert.Equal(t, 11.0, g.TotalMaxMarks())
}

func TestLoadGuideFromJSON_InvalidJSON(t *testing.T) {
	_, err := grade.LoadGuideFromJSON([]byte(`not json`))
	assert.Error(t, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 10: LLMJudge blank answer path — no LLM call
// ─────────────────────────────────────────────────────────────────────────────

func TestLLMJudge_BlankAnswer_NoLLMCall(t *testing.T) {
	mock := &mockProvider{}
	judge := grade.NewLLMJudge(mock, "m")

	gq, err := judge.Grade(context.Background(), contracts.TranscribedQuestion{
		QuestionNo:    "Q1",
		MaxMarks:      5,
		StudentAnswer: "",
		ReadConfidence: 0.9,
	})
	require.NoError(t, err)
	assert.Equal(t, 0.0, gq.AwardedMarks)
	assert.Contains(t, gq.Flags, "blank_answer")
	assert.False(t, mock.called, "LLMJudge must not call provider for blank answer")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 11: LLM error isolation — grading_failed flag, AwardedMarks=0
// ─────────────────────────────────────────────────────────────────────────────

func TestLLMJudge_LLMError_Isolated(t *testing.T) {
	mock := &mockProvider{err: errors.New("network timeout")}
	judge := grade.NewLLMJudge(mock, "m")

	gq, err := judge.Grade(context.Background(), contracts.TranscribedQuestion{
		QuestionNo:     "Q1",
		MaxMarks:       5,
		StudentAnswer:  "some answer",
		ReadConfidence: 0.9,
	})
	// GradePaper isolation means Grade itself returns nil error but a flagged result.
	require.NoError(t, err)
	assert.Equal(t, 0.0, gq.AwardedMarks)
	assert.Contains(t, gq.Flags, "grading_failed")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 12: LLM awarded_marks clamped to [0, max_marks]
// ─────────────────────────────────────────────────────────────────────────────

func TestLLMJudge_AwardedMarksClamped(t *testing.T) {
	mock := &mockProvider{
		response: providers.CompletionResp{
			// LLM returns more marks than max_marks
			Content: `{"awarded_marks": 999, "justification": "over generous", "grade_confidence": 1.0}`,
		},
	}
	judge := grade.NewLLMJudge(mock, "m")

	gq, err := judge.Grade(context.Background(), contracts.TranscribedQuestion{
		QuestionNo:     "Q1",
		MaxMarks:       5,
		StudentAnswer:  "answer",
		ReadConfidence: 0.9,
	})
	require.NoError(t, err)
	assert.Equal(t, 5.0, gq.AwardedMarks, "awarded marks must be clamped to max_marks")
}
