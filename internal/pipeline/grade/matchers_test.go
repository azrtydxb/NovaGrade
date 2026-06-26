package grade_test

import (
	"context"
	"testing"

	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// panicProvider fails the test if Complete is ever called. Use it to assert
// that a deterministic match type never reaches the AI provider.
type panicProvider struct {
	t *testing.T
}

func (p *panicProvider) Complete(_ context.Context, _ interface{}) (interface{}, error) {
	p.t.Fatal("panicProvider.Complete must not be called for deterministic match types")
	return nil, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Numeric matcher
// ─────────────────────────────────────────────────────────────────────────────

func TestMatchNumeric_AbsoluteTolerance(t *testing.T) {
	tests := []struct {
		name       string
		entry      grade.GuideEntry
		answer     string
		wantMarks  float64
		wantConf   float64
	}{
		{
			name: "exact match abs tol=0",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0, ToleranceType: "abs",
			},
			answer:    "9.81",
			wantMarks: 4,
			wantConf:  1.0,
		},
		{
			name: "within abs tolerance",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
			},
			answer:    "9.83",
			wantMarks: 4,
			wantConf:  1.0,
		},
		{
			name: "outside abs tolerance",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
			},
			answer:    "9.90",
			wantMarks: 0,
			wantConf:  1.0,
		},
		{
			name: "boundary exactly at tolerance edge (inclusive)",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 10.0, Tolerance: 0.1, ToleranceType: "abs",
			},
			answer:    "10.1",
			wantMarks: 4,
			wantConf:  1.0,
		},
		{
			name: "non-numeric answer → 0 marks",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
			},
			answer:    "not a number",
			wantMarks: 0,
			wantConf:  1.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			awarded, conf, _ := grade.MatchNumeric(tc.entry, tc.answer)
			assert.Equal(t, tc.wantMarks, awarded, "awarded marks")
			assert.Equal(t, tc.wantConf, conf, "confidence")
		})
	}
}

func TestMatchNumeric_PercentTolerance(t *testing.T) {
	tests := []struct {
		name      string
		entry     grade.GuideEntry
		answer    string
		wantMarks float64
	}{
		{
			name: "within pct tolerance (5%)",
			entry: grade.GuideEntry{
				MaxMarks: 3, Match: "numeric",
				NumericAnswer: 100.0, Tolerance: 5, ToleranceType: "pct",
			},
			answer:    "104",
			wantMarks: 3,
		},
		{
			name: "outside pct tolerance (5%)",
			entry: grade.GuideEntry{
				MaxMarks: 3, Match: "numeric",
				NumericAnswer: 100.0, Tolerance: 5, ToleranceType: "pct",
			},
			answer:    "106",
			wantMarks: 0,
		},
		{
			name: "exactly at pct tolerance boundary (inclusive)",
			entry: grade.GuideEntry{
				MaxMarks: 3, Match: "numeric",
				NumericAnswer: 100.0, Tolerance: 5, ToleranceType: "pct",
			},
			answer:    "95",
			wantMarks: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			awarded, _, _ := grade.MatchNumeric(tc.entry, tc.answer)
			assert.Equal(t, tc.wantMarks, awarded)
		})
	}
}

func TestMatchNumeric_UnitStripping(t *testing.T) {
	tests := []struct {
		name      string
		entry     grade.GuideEntry
		answer    string
		wantMarks float64
	}{
		{
			name: "numeric value with unit stripped before parsing",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
				Unit: "m/s^2",
			},
			answer:    "9.83 m/s^2",
			wantMarks: 4,
		},
		{
			name: "no unit in student answer (unit not required for value marks)",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
				Unit: "m/s^2",
			},
			answer:    "9.82",
			wantMarks: 4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			awarded, _, _ := grade.MatchNumeric(tc.entry, tc.answer)
			assert.Equal(t, tc.wantMarks, awarded)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit handling (unit_marks modifier on numeric)
// ─────────────────────────────────────────────────────────────────────────────

func TestMatchNumeric_UnitMarks(t *testing.T) {
	tests := []struct {
		name      string
		entry     grade.GuideEntry
		answer    string
		wantMarks float64
		wantConf  float64
	}{
		{
			name: "value correct + unit correct → full marks",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
				Unit: "m/s^2", UnitMarks: 1,
			},
			answer:    "9.83 m/s^2",
			wantMarks: 4,
			wantConf:  1.0,
		},
		{
			name: "value correct + unit wrong → value marks only (max - unit_marks)",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
				Unit: "m/s^2", UnitMarks: 1,
			},
			answer:    "9.83 N",
			wantMarks: 3, // 4 - 1 unit_mark
			wantConf:  1.0,
		},
		{
			name: "value correct + unit missing → value marks only",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
				Unit: "m/s^2", UnitMarks: 1,
			},
			answer:    "9.82",
			wantMarks: 3, // 4 - 1 unit_mark
			wantConf:  1.0,
		},
		{
			name: "value wrong → 0 marks regardless of unit",
			entry: grade.GuideEntry{
				MaxMarks: 4, Match: "numeric",
				NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
				Unit: "m/s^2", UnitMarks: 1,
			},
			answer:    "5.0 m/s^2",
			wantMarks: 0,
			wantConf:  1.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			awarded, conf, _ := grade.MatchNumeric(tc.entry, tc.answer)
			assert.Equal(t, tc.wantMarks, awarded, "awarded marks")
			assert.Equal(t, tc.wantConf, conf, "confidence")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Multi-step matcher
// ─────────────────────────────────────────────────────────────────────────────

func TestMatchMultiStep_MethodMarks(t *testing.T) {
	// Key test: a correct step earns its marks EVEN IF the final answer is wrong.
	// Step 1: set up the equation correctly (accept "F=ma"); 1 mark
	// Step 2: correct final value "20 N"; 2 marks
	// Student writes "F=ma" but gets the final wrong ("10 N").
	entry := grade.GuideEntry{
		MaxMarks: 3,
		Match:    "multi_step",
		Steps: []grade.StepEntry{
			{Accept: []string{"F=ma", "F = ma"}, Marks: 1, Match: "set"},
			{Accept: []string{"20 N", "20N"}, Marks: 2, Match: "set"},
		},
	}

	awarded, conf, justification := grade.MatchMultiStep(entry, "F=ma\n10 N")
	assert.Equal(t, 1.0, awarded, "method mark for correct step 1 despite wrong final answer")
	assert.Equal(t, 1.0, conf)
	assert.NotEmpty(t, justification)
}

func TestMatchMultiStep_AllStepsCorrect(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks: 3,
		Match:    "multi_step",
		Steps: []grade.StepEntry{
			{Accept: []string{"F=ma"}, Marks: 1, Match: "set"},
			{Accept: []string{"20 N"}, Marks: 2, Match: "set"},
		},
	}

	awarded, _, _ := grade.MatchMultiStep(entry, "F=ma\n20 N")
	assert.Equal(t, 3.0, awarded)
}

func TestMatchMultiStep_ClampAtMax(t *testing.T) {
	// Steps sum to more than MaxMarks — must be clamped.
	entry := grade.GuideEntry{
		MaxMarks: 2,
		Match:    "multi_step",
		Steps: []grade.StepEntry{
			{Accept: []string{"A"}, Marks: 2, Match: "set"},
			{Accept: []string{"B"}, Marks: 2, Match: "set"},
		},
	}

	awarded, _, _ := grade.MatchMultiStep(entry, "A\nB")
	assert.Equal(t, 2.0, awarded, "must clamp to max_marks")
}

func TestMatchMultiStep_ZeroStepsMatch(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks: 3,
		Match:    "multi_step",
		Steps: []grade.StepEntry{
			{Accept: []string{"correct"}, Marks: 3, Match: "set"},
		},
	}

	awarded, _, _ := grade.MatchMultiStep(entry, "wrong answer entirely")
	assert.Equal(t, 0.0, awarded)
}

func TestMatchMultiStep_NumericStep(t *testing.T) {
	// A multi_step entry can embed a numeric step.
	entry := grade.GuideEntry{
		MaxMarks: 3,
		Match:    "multi_step",
		Steps: []grade.StepEntry{
			{Accept: []string{"F=ma"}, Marks: 1, Match: "set"},
			{Marks: 2, Match: "numeric", NumericAnswer: 20.0, Tolerance: 0.5, ToleranceType: "abs"},
		},
	}

	// Step 1 correct, step 2 within tolerance
	awarded, _, _ := grade.MatchMultiStep(entry, "F=ma\n20.3")
	assert.Equal(t, 3.0, awarded)

	// Only step 1 correct
	awarded2, _, _ := grade.MatchMultiStep(entry, "F=ma\n99")
	assert.Equal(t, 1.0, awarded2, "method mark for step 1 only")
}

// ─────────────────────────────────────────────────────────────────────────────
// Partial matcher
// ─────────────────────────────────────────────────────────────────────────────

func TestMatchPartial_SumMatchedCriteria(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks: 4,
		Match:    "partial",
		Criteria: []grade.CriterionEntry{
			{Accept: []string{"chlorophyll"}, Marks: 1},
			{Accept: []string{"sunlight", "light"}, Marks: 1},
			{Accept: []string{"CO2", "carbon dioxide"}, Marks: 1},
			{Accept: []string{"glucose", "sugar"}, Marks: 1},
		},
	}

	// Student mentions chlorophyll and CO2 — should earn 2 marks
	awarded, conf, _ := grade.MatchPartial(entry, "plants use chlorophyll and CO2 to make food")
	assert.Equal(t, 2.0, awarded)
	assert.Equal(t, 1.0, conf)
}

func TestMatchPartial_ClampAtMax(t *testing.T) {
	// Criteria sum to 6 but max_marks is 4.
	entry := grade.GuideEntry{
		MaxMarks: 4,
		Match:    "partial",
		Criteria: []grade.CriterionEntry{
			{Accept: []string{"A"}, Marks: 2},
			{Accept: []string{"B"}, Marks: 2},
			{Accept: []string{"C"}, Marks: 2},
		},
	}

	awarded, _, _ := grade.MatchPartial(entry, "A B C")
	assert.Equal(t, 4.0, awarded, "must clamp to max_marks")
}

func TestMatchPartial_ZeroMatches(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks: 4,
		Match:    "partial",
		Criteria: []grade.CriterionEntry{
			{Accept: []string{"chlorophyll"}, Marks: 2},
			{Accept: []string{"sunlight"}, Marks: 2},
		},
	}

	awarded, _, _ := grade.MatchPartial(entry, "mitochondria and ribosomes")
	assert.Equal(t, 0.0, awarded)
}

func TestMatchPartial_ConfidenceAlwaysOne(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks: 2,
		Match:    "partial",
		Criteria: []grade.CriterionEntry{
			{Accept: []string{"x"}, Marks: 1},
		},
	}

	_, conf, _ := grade.MatchPartial(entry, "x")
	assert.Equal(t, 1.0, conf)

	_, conf2, _ := grade.MatchPartial(entry, "y")
	assert.Equal(t, 1.0, conf2)
}

// ─────────────────────────────────────────────────────────────────────────────
// Normalize modifier on exact / exact_ci / set
// ─────────────────────────────────────────────────────────────────────────────

func TestNormalize_Set_AcceptsAlternateWording(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks:  2,
		Match:     "set",
		Accept:    []string{"carbon dioxide"},
		Normalize: true,
	}

	tests := []struct {
		answer    string
		wantMatch bool
	}{
		{"carbon dioxide", true},
		{"Carbon Dioxide", true},          // different case
		{"CARBON  DIOXIDE", true},         // extra whitespace
		{"carbon,dioxide", true},          // punctuation stripped
		{"carbon-dioxide", true},          // hyphen stripped
		{"CO2", false},                    // genuinely different
	}

	for _, tc := range tests {
		t.Run(tc.answer, func(t *testing.T) {
			awarded, _, _ := grade.MatchObjectiveNormalized(entry, tc.answer)
			if tc.wantMatch {
				assert.Equal(t, 2.0, awarded, "expected match for %q", tc.answer)
			} else {
				assert.Equal(t, 0.0, awarded, "expected no match for %q", tc.answer)
			}
		})
	}
}

func TestNormalize_Exact_WithoutNormalize_StrictBehavior(t *testing.T) {
	// Without normalize, existing behavior is preserved.
	entry := grade.GuideEntry{
		MaxMarks:  2,
		Match:     "exact",
		Answer:    "carbon dioxide",
		Normalize: false,
	}

	awarded, _, _ := grade.MatchObjectiveNormalized(entry, "Carbon Dioxide")
	assert.Equal(t, 0.0, awarded, "without normalize, case difference must not match on exact")
}

func TestNormalize_ExactCI_WithNormalize(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks:  1,
		Match:     "exact_ci",
		Answer:    "newton's second law",
		Normalize: true,
	}

	// These should all match with normalize.
	matching := []string{
		"Newton's Second Law",
		"Newton's  Second  Law",  // collapsed whitespace
		"Newton's Second Law.",   // trailing punctuation
		"newtons second law",     // apostrophe stripped
	}
	for _, ans := range matching {
		awarded, _, _ := grade.MatchObjectiveNormalized(entry, ans)
		assert.Equal(t, 1.0, awarded, "expected match for %q", ans)
	}
}

func TestNormalize_ConfidenceIsOne(t *testing.T) {
	entry := grade.GuideEntry{
		MaxMarks:  1,
		Match:     "exact_ci",
		Answer:    "hello",
		Normalize: true,
	}

	_, conf, _ := grade.MatchObjectiveNormalized(entry, "Hello!")
	assert.Equal(t, 1.0, conf)
}

// ─────────────────────────────────────────────────────────────────────────────
// Guard: deterministic match types must NOT invoke the provider
// ─────────────────────────────────────────────────────────────────────────────

// failProvider fails the test immediately if Complete is ever called.
type failProvider struct {
	t *testing.T
}

func (f *failProvider) Complete(_ context.Context, req interface{}) (interface{}, error) {
	f.t.Fatal("provider.Complete must NOT be called for deterministic match types")
	return nil, nil
}

func TestGuideMarkScheme_Numeric_NoProviderCall(t *testing.T) {
	// Create a real failing provider wired into a GuideMarkScheme with a numeric entry.
	// The test will fail if Complete is called.
	fp := &mockProvider{}
	fp.err = nil
	// Instead, use a mock that panics if called for the scheme's rubric provider.
	// We'll set the scheme's provider to a mock that records calls, then assert called==false.
	schemeMock := &mockProvider{}
	fallbackMock := &mockProvider{}

	guide := grade.Guide{
		"Q1": grade.GuideEntry{
			MaxMarks: 4, Match: "numeric",
			NumericAnswer: 9.81, Tolerance: 0.05, ToleranceType: "abs",
		},
	}
	fallback := grade.NewLLMJudge(fallbackMock, "fallback-model")
	scheme := grade.NewGuideMarkScheme(guide, fallback, schemeMock, "scheme-model")

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q1",
		MaxMarks:       4,
		QuestionText:   "What is g?",
		StudentAnswer:  "9.82",
		ReadConfidence: 0.95,
	}

	gq, err := scheme.Grade(context.Background(), q)
	require.NoError(t, err)
	assert.Equal(t, 4.0, gq.AwardedMarks)
	assert.Equal(t, 1.0, gq.GradeConfidence)
	assert.False(t, schemeMock.called, "scheme provider must NOT be called for numeric match")
	assert.False(t, fallbackMock.called, "fallback provider must NOT be called for numeric match")
}

func TestGuideMarkScheme_Partial_NoProviderCall(t *testing.T) {
	schemeMock := &mockProvider{}
	fallbackMock := &mockProvider{}

	guide := grade.Guide{
		"Q2": grade.GuideEntry{
			MaxMarks: 3,
			Match:    "partial",
			Criteria: []grade.CriterionEntry{
				{Accept: []string{"chlorophyll"}, Marks: 1},
				{Accept: []string{"sunlight"}, Marks: 1},
				{Accept: []string{"CO2"}, Marks: 1},
			},
		},
	}
	fallback := grade.NewLLMJudge(fallbackMock, "fallback-model")
	scheme := grade.NewGuideMarkScheme(guide, fallback, schemeMock, "scheme-model")

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q2",
		MaxMarks:       3,
		QuestionText:   "Explain photosynthesis.",
		StudentAnswer:  "plants use chlorophyll and sunlight",
		ReadConfidence: 0.9,
	}

	gq, err := scheme.Grade(context.Background(), q)
	require.NoError(t, err)
	assert.Equal(t, 2.0, gq.AwardedMarks)
	assert.False(t, schemeMock.called, "scheme provider must NOT be called for partial match")
	assert.False(t, fallbackMock.called, "fallback provider must NOT be called for partial match")
}

func TestGuideMarkScheme_MultiStep_NoProviderCall(t *testing.T) {
	schemeMock := &mockProvider{}
	fallbackMock := &mockProvider{}

	guide := grade.Guide{
		"Q3": grade.GuideEntry{
			MaxMarks: 3,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Accept: []string{"F=ma"}, Marks: 1, Match: "set"},
				{Accept: []string{"20 N"}, Marks: 2, Match: "set"},
			},
		},
	}
	fallback := grade.NewLLMJudge(fallbackMock, "fallback-model")
	scheme := grade.NewGuideMarkScheme(guide, fallback, schemeMock, "scheme-model")

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q3",
		MaxMarks:       3,
		QuestionText:   "Find the force.",
		StudentAnswer:  "F=ma\n10 N", // step 1 correct, step 2 wrong
		ReadConfidence: 0.95,
	}

	gq, err := scheme.Grade(context.Background(), q)
	require.NoError(t, err)
	assert.Equal(t, 1.0, gq.AwardedMarks, "method mark for step 1")
	assert.False(t, schemeMock.called, "scheme provider must NOT be called for multi_step match")
	assert.False(t, fallbackMock.called, "fallback provider must NOT be called for multi_step match")
}

func TestGuideMarkScheme_NormalizedSet_NoProviderCall(t *testing.T) {
	schemeMock := &mockProvider{}
	fallbackMock := &mockProvider{}

	guide := grade.Guide{
		"Q4": grade.GuideEntry{
			MaxMarks:  2,
			Match:     "set",
			Accept:    []string{"carbon dioxide"},
			Normalize: true,
		},
	}
	fallback := grade.NewLLMJudge(fallbackMock, "fallback-model")
	scheme := grade.NewGuideMarkScheme(guide, fallback, schemeMock, "scheme-model")

	q := contracts.TranscribedQuestion{
		QuestionNo:     "Q4",
		MaxMarks:       2,
		QuestionText:   "Name the gas.",
		StudentAnswer:  "Carbon  Dioxide",
		ReadConfidence: 0.9,
	}

	gq, err := scheme.Grade(context.Background(), q)
	require.NoError(t, err)
	assert.Equal(t, 2.0, gq.AwardedMarks)
	assert.False(t, schemeMock.called, "scheme provider must NOT be called for normalized set match")
	assert.False(t, fallbackMock.called, "fallback provider must NOT be called for normalized set match")
}

// ─────────────────────────────────────────────────────────────────────────────
// Backward compatibility: existing match types must be unaffected
// ─────────────────────────────────────────────────────────────────────────────

func TestBackwardCompat_ExistingMatchTypes_Unaffected(t *testing.T) {
	mock := &mockProvider{}
	guide := grade.Guide{
		"Q1": {MaxMarks: 2, Answer: "Paris", Match: "exact_ci"},
		"Q2": {MaxMarks: 3, Answer: "H2O", Match: "exact"},
		"Q3": {MaxMarks: 1, Accept: []string{"cat", "feline"}, Match: "set"},
	}
	fallback := grade.NewLLMJudge(mock, "m")
	scheme := grade.NewGuideMarkScheme(guide, fallback, mock, "m")
	ctx := context.Background()

	// exact_ci match
	gq1, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
		QuestionNo: "Q1", MaxMarks: 2, StudentAnswer: "paris", ReadConfidence: 1.0,
	})
	require.NoError(t, err)
	assert.Equal(t, 2.0, gq1.AwardedMarks)

	// exact match
	gq2, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
		QuestionNo: "Q2", MaxMarks: 3, StudentAnswer: "H2O", ReadConfidence: 1.0,
	})
	require.NoError(t, err)
	assert.Equal(t, 3.0, gq2.AwardedMarks)

	// set match (case-insensitive member)
	gq3, err := scheme.Grade(ctx, contracts.TranscribedQuestion{
		QuestionNo: "Q3", MaxMarks: 1, StudentAnswer: "FELINE", ReadConfidence: 1.0,
	})
	require.NoError(t, err)
	assert.Equal(t, 1.0, gq3.AwardedMarks)

	// None of these should have called the provider.
	assert.False(t, mock.called, "no LLM calls for existing deterministic match types")
}
