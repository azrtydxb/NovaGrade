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
		"Q5": {MaxMarks: 3, Match: "numeric", NumericAnswer: f64(9.81)},
		"Q6": {
			MaxMarks: 4,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Marks: 1, Match: "exact", Answer: "KE = 0.5mv^2"},
				{Marks: 3, Match: "numeric", NumericAnswer: f64(50)},
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
	// Zero is a valid expected numeric answer when explicitly provided (non-nil pointer).
	g := grade.Guide{
		"Q5": {MaxMarks: 3, Match: "numeric", NumericAnswer: f64(0)},
	}
	require.NoError(t, grade.ValidateGuide(g))
}

func TestValidateGuide_NumericMissingNumericAnswer_NilRejected(t *testing.T) {
	// A numeric entry with no numeric_answer field (nil pointer) must be rejected.
	g := grade.Guide{
		"Q5": {MaxMarks: 3, Match: "numeric"},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q5")
	assert.Contains(t, err.Error(), "numeric_answer")
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

// ─────────────────────────────────────────────────────────────────────────────
// Fix 3: exact / exact_ci must have a non-empty answer field
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateGuide_ExactMissingAnswer_Rejected(t *testing.T) {
	g := grade.Guide{
		"Q1": {MaxMarks: 2, Match: "exact"},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q1")
	assert.Contains(t, err.Error(), "answer")
}

func TestValidateGuide_ExactCI_MissingAnswer_Rejected(t *testing.T) {
	g := grade.Guide{
		"Q2": {MaxMarks: 2, Match: "exact_ci"},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q2")
	assert.Contains(t, err.Error(), "answer")
}

func TestValidateGuide_ExactWithAnswer_Valid(t *testing.T) {
	g := grade.Guide{
		"Q1": {MaxMarks: 2, Match: "exact", Answer: "Paris"},
	}
	require.NoError(t, grade.ValidateGuide(g))
}

func TestValidateGuide_ExactCI_WithAnswer_Valid(t *testing.T) {
	g := grade.Guide{
		"Q2": {MaxMarks: 2, Match: "exact_ci", Answer: "h2o"},
	}
	require.NoError(t, grade.ValidateGuide(g))
}

// ─────────────────────────────────────────────────────────────────────────────
// Fix 1(a): multi_step step-level required field validation
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateGuide_MultiStep_ExactStepMissingAnswer_Rejected(t *testing.T) {
	// An exact step inside multi_step without an answer field must be rejected.
	g := grade.Guide{
		"Q1": {
			MaxMarks: 4,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Match: "exact", Marks: 2}, // missing answer
				{Match: "exact_ci", Answer: "F=ma", Marks: 2},
			},
		},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err, "multi_step with an exact step missing answer must be rejected")
	assert.Contains(t, err.Error(), "Q1")
	assert.Contains(t, err.Error(), "step[0]")
	assert.Contains(t, err.Error(), "answer")
}

func TestValidateGuide_MultiStep_ExactCIStepMissingAnswer_Rejected(t *testing.T) {
	// An exact_ci step inside multi_step without an answer field must be rejected.
	g := grade.Guide{
		"Q2": {
			MaxMarks: 4,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Match: "exact_ci", Marks: 2}, // missing answer
			},
		},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err, "multi_step with exact_ci step missing answer must be rejected")
	assert.Contains(t, err.Error(), "Q2")
	assert.Contains(t, err.Error(), "answer")
}

func TestValidateGuide_MultiStep_SetStepMissingAccept_Rejected(t *testing.T) {
	// A set step inside multi_step without any accept values must be rejected.
	g := grade.Guide{
		"Q3": {
			MaxMarks: 3,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Match: "set", Marks: 1}, // missing accept
				{Match: "numeric", NumericAnswer: f64(20), Marks: 2},
			},
		},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err, "multi_step with set step missing accept must be rejected")
	assert.Contains(t, err.Error(), "Q3")
	assert.Contains(t, err.Error(), "step[0]")
	assert.Contains(t, err.Error(), "accept")
}

func TestValidateGuide_MultiStep_EmptyStepMatch_Rejected(t *testing.T) {
	// A step with an empty match field must be rejected.
	g := grade.Guide{
		"Q4": {
			MaxMarks: 2,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Match: "", Answer: "something", Marks: 2}, // empty match
			},
		},
	}
	err := grade.ValidateGuide(g)
	require.Error(t, err, "multi_step with step having empty match must be rejected")
	assert.Contains(t, err.Error(), "Q4")
}

func TestValidateGuide_MultiStep_ValidSteps_Accepted(t *testing.T) {
	// A well-formed multi_step with all required fields must pass.
	g := grade.Guide{
		"Q5": {
			MaxMarks: 5,
			Match:    "multi_step",
			Steps: []grade.StepEntry{
				{Match: "exact", Answer: "F=ma", Marks: 1},
				{Match: "exact_ci", Answer: "newton", Marks: 1},
				{Match: "set", Accept: []string{"20 N", "20N"}, Marks: 1},
				{Match: "numeric", NumericAnswer: f64(20), Tolerance: 0.5, Marks: 2},
			},
		},
	}
	require.NoError(t, grade.ValidateGuide(g))
}
