package grade_test

import (
	"strings"
	"testing"

	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// ValidateGuide — happy paths
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateGuide_ValidGuide_AllMatchTypes(t *testing.T) {
	g := grade.Guide{
		"Q1": {MaxMarks: 2, Match: "exact", Answer: "Paris"},
		"Q2": {MaxMarks: 2, Match: "exact_ci", Answer: "h2o"},
		"Q3": {MaxMarks: 1, Match: "set", Accept: []string{"cat", "feline"}},
		"Q4": {MaxMarks: 5, Match: "rubric", Rubric: "Award marks for explaining entropy."},
		"Q5": {MaxMarks: 3, Match: "numeric", NumericAnswer: 9.81},
		"Q6": {
			MaxMarks: 4,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Marks: 1, Match: "exact", Answer: "KE = 0.5mv^2"},
				{Marks: 3, Match: "numeric", NumericAnswer: 50},
			},
		},
		"Q7": {
			MaxMarks: 3,
			Match:    "partial",
			Criteria: []grade.CriterionEntry{
				{Accept: []string{"mitochondria"}, Marks: 1},
				{Accept: []string{"ATP", "energy"}, Marks: 2},
			},
		},
	}
	require.NoError(t, grade.ValidateGuide(g))
}

func TestValidateGuide_Empty_Guide_Passes(t *testing.T) {
	require.NoError(t, grade.ValidateGuide(grade.Guide{}))
}

func TestValidateGuideJSON_Valid(t *testing.T) {
	json := []byte(`{
		"Q1": {"max_marks": 2, "match": "exact", "answer": "Paris"},
		"Q2": {"max_marks": 1, "match": "set", "accept": ["cat"]}
	}`)
	require.NoError(t, grade.ValidateGuideJSON(json))
}

// ─────────────────────────────────────────────────────────────────────────────
// ValidateGuide — error cases
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateGuide_UnknownMatchType(t *testing.T) {
	g := grade.Guide{
		"Q1": {MaxMarks: 2, Match: "fuzzy"},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q1")
	assert.Contains(t, err.Error(), "fuzzy")
}

func TestValidateGuide_NegativeMaxMarks(t *testing.T) {
	g := grade.Guide{
		"Q1": {MaxMarks: -1, Match: "exact", Answer: "Paris"},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q1")
	assert.Contains(t, err.Error(), "max_marks")
}

func TestValidateGuide_SetWithNoAccept(t *testing.T) {
	g := grade.Guide{
		"Q3": {MaxMarks: 1, Match: "set"},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q3")
	assert.Contains(t, err.Error(), "accept")
}

func TestValidateGuide_RubricWithEmptyRubric(t *testing.T) {
	g := grade.Guide{
		"Q4": {MaxMarks: 5, Match: "rubric", Rubric: ""},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q4")
	assert.Contains(t, err.Error(), "rubric")
}

func TestValidateGuide_NumericMissingNumericAnswer_ZeroIsAllowed(t *testing.T) {
	// Zero is a valid expected numeric answer.
	g := grade.Guide{
		"Q5": {MaxMarks: 3, Match: "numeric", NumericAnswer: 0},
	}
	require.NoError(t, grade.ValidateGuide(g))
}

func TestValidateGuide_MultiStepWithNoSteps(t *testing.T) {
	g := grade.Guide{
		"Q6": {MaxMarks: 4, Match: "multi_step", Steps: nil},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q6")
	assert.Contains(t, err.Error(), "step")
}

func TestValidateGuide_PartialWithNoCriteria(t *testing.T) {
	g := grade.Guide{
		"Q7": {MaxMarks: 3, Match: "partial", Criteria: nil},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q7")
	assert.Contains(t, err.Error(), "criterion")
}

func TestValidateGuide_PartialWithCriterionMissingAccept(t *testing.T) {
	g := grade.Guide{
		"Q7": {
			MaxMarks: 3,
			Match:    "partial",
			Criteria: []grade.CriterionEntry{
				{Accept: nil, Marks: 1}, // missing accept
			},
		},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q7")
}

func TestValidateGuide_MultipleErrors_AllReported(t *testing.T) {
	g := grade.Guide{
		"Q1": {MaxMarks: -1, Match: "bogus"},
		"Q2": {MaxMarks: 0, Match: "set"}, // no accept
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	// Both questions must appear in the error message.
	assert.True(t,
		strings.Contains(err.Error(), "Q1") && strings.Contains(err.Error(), "Q2"),
		"expected both Q1 and Q2 in error: %v", err,
	)
}

func TestValidateGuideJSON_InvalidJSON(t *testing.T) {
	err := grade.ValidateGuideJSON([]byte(`{not valid json`))
	require.Error(t, err)
}
