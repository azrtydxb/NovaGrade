package grade_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
)

// numPtr is a helper for *float64 fields in test guide entries.
func numPtr(v float64) *float64 { return &v }

// buildPreviewGuide builds a Guide with several deterministic entries plus
// a rubric entry for non-previewable tests.
func buildPreviewGuide() grade.Guide {
	return grade.Guide{
		// exact match
		"Q1": {
			MaxMarks: 2,
			Match:    "exact",
			Answer:   "Paris",
		},
		// exact_ci match
		"Q2": {
			MaxMarks: 3,
			Match:    "exact_ci",
			Answer:   "hydrogen",
		},
		// set match
		"Q3": {
			MaxMarks: 1,
			Match:    "set",
			Accept:   []string{"cat", "feline"},
		},
		// numeric match with abs tolerance
		"Q4": {
			MaxMarks:      4,
			Match:         "numeric",
			NumericAnswer: numPtr(9.81),
			Tolerance:     0.05,
			ToleranceType: "abs",
		},
		// multi_step match
		"Q5": {
			MaxMarks: 4,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Match: "exact", Answer: "F=ma", Marks: 2},
				{Match: "numeric", NumericAnswer: numPtr(20), Tolerance: 0, Marks: 2},
			},
		},
		// partial match
		"Q6": {
			MaxMarks: 3,
			Match:    "partial",
			Criteria: []grade.CriterionEntry{
				{Accept: []string{"photosynthesis"}, Marks: 1},
				{Accept: []string{"chlorophyll"}, Marks: 1},
				{Accept: []string{"sunlight", "light"}, Marks: 1},
			},
		},
		// normalize modifier
		"Q7": {
			MaxMarks:  2,
			Match:     "exact_ci",
			Answer:    "Newton's First Law",
			Normalize: true,
		},
		// rubric — NOT previewable deterministically
		"Q8": {
			MaxMarks: 5,
			Match:    "rubric",
			Rubric:   "Award up to 5 marks for quality of explanation.",
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestPreviewGuide_DeterministicTypes
// ─────────────────────────────────────────────────────────────────────────────

func TestPreviewGuide_Exact_Match(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{{QuestionNo: "Q1", StudentAnswer: "Paris"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, "Q1", r.QuestionNo)
	assert.Equal(t, float64(2), r.Awarded)
	assert.Equal(t, float64(2), r.MaxMarks)
	assert.Equal(t, "exact", r.MatchType)
	assert.True(t, r.Previewable)
}

func TestPreviewGuide_Exact_NoMatch(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{{QuestionNo: "Q1", StudentAnswer: "Lyon"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, float64(0), r.Awarded)
	assert.True(t, r.Previewable)
}

func TestPreviewGuide_ExactCI_Match(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{{QuestionNo: "Q2", StudentAnswer: "HYDROGEN"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, float64(3), r.Awarded)
	assert.True(t, r.Previewable)
}

func TestPreviewGuide_Set_Match(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{{QuestionNo: "Q3", StudentAnswer: "feline"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	assert.Equal(t, float64(1), results[0].Awarded)
}

func TestPreviewGuide_Set_NoMatch(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{{QuestionNo: "Q3", StudentAnswer: "dog"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	assert.Equal(t, float64(0), results[0].Awarded)
}

func TestPreviewGuide_Numeric_WithinTolerance(t *testing.T) {
	g := buildPreviewGuide()
	// 9.83 is within ±0.05 of 9.81
	samples := []grade.PreviewSample{{QuestionNo: "Q4", StudentAnswer: "9.83"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, float64(4), r.Awarded)
	assert.Equal(t, "numeric", r.MatchType)
	assert.True(t, r.Previewable)
}

func TestPreviewGuide_Numeric_OutsideTolerance(t *testing.T) {
	g := buildPreviewGuide()
	// 10.0 is outside ±0.05 of 9.81
	samples := []grade.PreviewSample{{QuestionNo: "Q4", StudentAnswer: "10.0"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	assert.Equal(t, float64(0), results[0].Awarded)
}

func TestPreviewGuide_MultiStep(t *testing.T) {
	g := buildPreviewGuide()
	// answer contains both steps: "F=ma" and "20"
	samples := []grade.PreviewSample{{QuestionNo: "Q5", StudentAnswer: "F=ma\n20"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, float64(4), r.Awarded)
	assert.Equal(t, "multi_step", r.MatchType)
	assert.True(t, r.Previewable)
}

func TestPreviewGuide_MultiStep_PartialCredit(t *testing.T) {
	g := buildPreviewGuide()
	// Only first step matches; second step doesn't
	samples := []grade.PreviewSample{{QuestionNo: "Q5", StudentAnswer: "F=ma"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, float64(2), r.Awarded) // only step 1 marks
	assert.True(t, r.Previewable)
}

func TestPreviewGuide_Partial(t *testing.T) {
	g := buildPreviewGuide()
	// answer hits two criteria: photosynthesis and light
	samples := []grade.PreviewSample{{QuestionNo: "Q6", StudentAnswer: "Plants use photosynthesis and need light."}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, float64(2), r.Awarded)
	assert.Equal(t, "partial", r.MatchType)
	assert.True(t, r.Previewable)
}

func TestPreviewGuide_Normalize(t *testing.T) {
	g := buildPreviewGuide()
	// Normalized form of "newtons first law" matches "Newton's First Law"
	samples := []grade.PreviewSample{{QuestionNo: "Q7", StudentAnswer: "newtons first law"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, float64(2), r.Awarded)
	assert.True(t, r.Previewable)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestPreviewGuide_Rubric — must be Previewable=false, no panic, no LLM
// ─────────────────────────────────────────────────────────────────────────────

func TestPreviewGuide_Rubric_NotPreviewable(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{{QuestionNo: "Q8", StudentAnswer: "Some long answer."}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, "Q8", r.QuestionNo)
	assert.Equal(t, float64(0), r.Awarded)
	assert.Equal(t, "rubric", r.MatchType)
	assert.False(t, r.Previewable)
	assert.NotEmpty(t, r.Justification, "justification should explain why not previewable")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestPreviewGuide_UnknownMatchType — guide entry with unknown match type
// ─────────────────────────────────────────────────────────────────────────────

func TestPreviewGuide_UnknownMatchType_NotPreviewable(t *testing.T) {
	g := grade.Guide{
		"QX": {
			MaxMarks: 3,
			Match:    "fuzzy", // unknown type — should never reach here if ValidateGuide passes
			Answer:   "something",
		},
	}
	samples := []grade.PreviewSample{{QuestionNo: "QX", StudentAnswer: "something"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.False(t, r.Previewable)
	assert.NotEmpty(t, r.Justification)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestPreviewGuide_MissingQuestionNo — sample qno not in guide
// ─────────────────────────────────────────────────────────────────────────────

func TestPreviewGuide_MissingQuestionNo_NotPreviewable(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{{QuestionNo: "Q99", StudentAnswer: "something"}}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 1)
	r := results[0]
	assert.Equal(t, "Q99", r.QuestionNo)
	assert.Equal(t, float64(0), r.Awarded)
	assert.False(t, r.Previewable)
	assert.NotEmpty(t, r.Justification)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestPreviewGuide_MultipleSamples
// ─────────────────────────────────────────────────────────────────────────────

func TestPreviewGuide_MultipleSamples(t *testing.T) {
	g := buildPreviewGuide()
	samples := []grade.PreviewSample{
		{QuestionNo: "Q1", StudentAnswer: "Paris"},  // match
		{QuestionNo: "Q1", StudentAnswer: "Berlin"}, // no match
		{QuestionNo: "Q4", StudentAnswer: "9.81"},   // numeric exact
		{QuestionNo: "Q8", StudentAnswer: "essay"},  // rubric → not previewable
	}
	results := grade.PreviewGuide(g, samples)

	require.Len(t, results, 4)
	assert.Equal(t, float64(2), results[0].Awarded)
	assert.Equal(t, float64(0), results[1].Awarded)
	assert.Equal(t, float64(4), results[2].Awarded)
	assert.False(t, results[3].Previewable)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestPreviewGuide_Empty
// ─────────────────────────────────────────────────────────────────────────────

func TestPreviewGuide_EmptySamples(t *testing.T) {
	g := buildPreviewGuide()
	results := grade.PreviewGuide(g, nil)
	assert.Len(t, results, 0)
}

func TestPreviewGuide_EmptyGuide(t *testing.T) {
	results := grade.PreviewGuide(grade.Guide{}, []grade.PreviewSample{
		{QuestionNo: "Q1", StudentAnswer: "Paris"},
	})
	require.Len(t, results, 1)
	assert.False(t, results[0].Previewable)
}
