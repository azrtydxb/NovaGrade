package analytics_test

// Tests for OutcomeMastery and LearningGaps pure functions.
// These tests were written BEFORE the implementation (TDD RED phase).
//
// Hand-computed test case:
//
//	Papers (3 graded papers, 2 questions each):
//	  Paper A: Q1=4/5, Q2=3/5
//	  Paper B: Q1=5/5, Q2=5/5
//	  Paper C: Q1=0/5, Q2=0/5
//
//	Mapping:
//	  Q1 → ["outcome-alpha"]
//	  Q2 → ["outcome-alpha", "outcome-beta"]
//
//	For outcome-alpha (Q1 + Q2):
//	  Q1: sum_awarded+=9, sum_max+=15, responses+=3
//	  Q2: sum_awarded+=8, sum_max+=15, responses+=3
//	  Total: sum_awarded=17, sum_max=30, responses=6, MappedQuestions=2
//	  MeanPct = 17/30 ≈ 0.5667 → "developing"
//
//	For outcome-beta (Q2 only):
//	  Q2: sum_awarded=8, sum_max=15, responses=3, MappedQuestions=1
//	  MeanPct = 8/15 ≈ 0.5333 → "developing"
//
//	Sorted by Code: ALPHA first, then BETA.
//
//	LearningGaps(stats, 5): both Responses>0; sorted by MeanPct asc → BETA first, then ALPHA.

import (
	"testing"

	"github.com/azrtydxb/novagrade/internal/analytics"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeOutcomeFixturePapers returns 3 graded papers with Q1 and Q2, each max 5.
func makeOutcomeFixturePapers() []contracts.GradedPaper {
	sec := sectionPtr("A")
	return []contracts.GradedPaper{
		{
			Subject: "Math", SourcePDF: "a.pdf",
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", Section: sec, MaxMarks: 5, AwardedMarks: 4},
				{QuestionNo: "Q2", Section: sec, MaxMarks: 5, AwardedMarks: 3},
			},
			Total: 7, MaxTotal: 10, Score100: 70,
		},
		{
			Subject: "Math", SourcePDF: "b.pdf",
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", Section: sec, MaxMarks: 5, AwardedMarks: 5},
				{QuestionNo: "Q2", Section: sec, MaxMarks: 5, AwardedMarks: 5},
			},
			Total: 10, MaxTotal: 10, Score100: 100,
		},
		{
			Subject: "Math", SourcePDF: "c.pdf",
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", Section: sec, MaxMarks: 5, AwardedMarks: 0},
				{QuestionNo: "Q2", Section: sec, MaxMarks: 5, AwardedMarks: 0},
			},
			Total: 0, MaxTotal: 10, Score100: 0,
		},
	}
}

// ── OutcomeMastery ──────────────────────────────────────────────────────────

func TestOutcomeMastery_HandComputedCase(t *testing.T) {
	papers := makeOutcomeFixturePapers()

	mapping := map[string][]string{
		"Q1": {"outcome-alpha"},
		"Q2": {"outcome-alpha", "outcome-beta"},
	}
	meta := map[string]analytics.OutcomeMeta{
		"outcome-alpha": {Code: "ALPHA", Description: "Alpha outcome"},
		"outcome-beta":  {Code: "BETA", Description: "Beta outcome"},
	}

	stats := analytics.OutcomeMastery(papers, mapping, meta)

	require.Len(t, stats, 2, "expected 2 outcome stats")

	// Sorted by Code: ALPHA first, BETA second.
	alpha := stats[0]
	beta := stats[1]

	// ── ALPHA checks ──
	assert.Equal(t, "outcome-alpha", alpha.OutcomeID)
	assert.Equal(t, "ALPHA", alpha.Code)
	assert.Equal(t, "Alpha outcome", alpha.Description)
	assert.Equal(t, 2, alpha.MappedQuestions, "ALPHA maps to Q1 and Q2")
	assert.Equal(t, 6, alpha.Responses, "3 papers × 2 questions = 6 contributions")
	floatEq(t, 17.0/30.0, alpha.MeanPct, "ALPHA MeanPct = 17/30")
	assert.Equal(t, "developing", alpha.Mastery, "17/30 ≈ 0.5667 → developing")

	// ── BETA checks ──
	assert.Equal(t, "outcome-beta", beta.OutcomeID)
	assert.Equal(t, "BETA", beta.Code)
	assert.Equal(t, "Beta outcome", beta.Description)
	assert.Equal(t, 1, beta.MappedQuestions, "BETA maps to Q2 only")
	assert.Equal(t, 3, beta.Responses, "3 papers × 1 question = 3 contributions")
	floatEq(t, 8.0/15.0, beta.MeanPct, "BETA MeanPct = 8/15")
	assert.Equal(t, "developing", beta.Mastery, "8/15 ≈ 0.5333 → developing")
}

func TestOutcomeMastery_MasteryThresholds(t *testing.T) {
	// One outcome mapped to Q1 with known MeanPct values to test all 3 buckets.
	// We use 3 separate tests with artificial papers.

	meta := map[string]analytics.OutcomeMeta{
		"o1": {Code: "C1", Description: "Test outcome"},
	}
	mapping := map[string][]string{
		"Q1": {"o1"},
	}

	tests := []struct {
		name        string
		awarded     float64
		max         float64
		wantMastery string
	}{
		{"secure (>= 0.75)", 75, 100, "secure"},
		{"developing (>= 0.50)", 50, 100, "developing"},
		{"emerging (< 0.50)", 40, 100, "emerging"},
		{"exactly 0.75 → secure", 3, 4, "secure"},
		{"exactly 0.50 → developing", 1, 2, "developing"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			papers := []contracts.GradedPaper{
				{
					Questions: []contracts.GradedQuestion{
						{QuestionNo: "Q1", MaxMarks: tc.max, AwardedMarks: tc.awarded},
					},
				},
			}
			stats := analytics.OutcomeMastery(papers, mapping, meta)
			require.Len(t, stats, 1)
			assert.Equal(t, tc.wantMastery, stats[0].Mastery, "MeanPct=%.4f", tc.awarded/tc.max)
		})
	}
}

func TestOutcomeMastery_EmptyPapers(t *testing.T) {
	mapping := map[string][]string{"Q1": {"o1"}}
	meta := map[string]analytics.OutcomeMeta{"o1": {Code: "C1"}}

	stats := analytics.OutcomeMastery(nil, mapping, meta)
	assert.Empty(t, stats, "nil papers → empty, no panic")

	stats2 := analytics.OutcomeMastery([]contracts.GradedPaper{}, mapping, meta)
	assert.Empty(t, stats2, "empty papers → empty, no panic")
}

func TestOutcomeMastery_EmptyMapping(t *testing.T) {
	papers := makeOutcomeFixturePapers()

	stats := analytics.OutcomeMastery(papers, nil, nil)
	assert.Empty(t, stats, "nil mapping → empty, no panic")

	stats2 := analytics.OutcomeMastery(papers, map[string][]string{}, map[string]analytics.OutcomeMeta{})
	assert.Empty(t, stats2, "empty mapping → empty, no panic")
}

func TestOutcomeMastery_MaxZero_NoPanic(t *testing.T) {
	// All MaxMarks == 0 → Σmax == 0 → MeanPct = 0, Mastery = "emerging"
	mapping := map[string][]string{"Q1": {"o1"}}
	meta := map[string]analytics.OutcomeMeta{"o1": {Code: "C1", Description: "D1"}}
	papers := []contracts.GradedPaper{
		{Questions: []contracts.GradedQuestion{{QuestionNo: "Q1", MaxMarks: 0, AwardedMarks: 0}}},
	}
	stats := analytics.OutcomeMastery(papers, mapping, meta)
	require.Len(t, stats, 1)
	floatEq(t, 0.0, stats[0].MeanPct, "Σmax=0 → MeanPct=0")
	assert.Equal(t, "emerging", stats[0].Mastery)
}

func TestOutcomeMastery_StableOrderByCode(t *testing.T) {
	// Outcomes inserted in reverse order; result must be sorted by Code ascending.
	mapping := map[string][]string{
		"Q1": {"o-z"},
		"Q2": {"o-a"},
		"Q3": {"o-m"},
	}
	meta := map[string]analytics.OutcomeMeta{
		"o-z": {Code: "Z", Description: "Z"},
		"o-a": {Code: "A", Description: "A"},
		"o-m": {Code: "M", Description: "M"},
	}
	papers := []contracts.GradedPaper{
		{Questions: []contracts.GradedQuestion{
			{QuestionNo: "Q1", MaxMarks: 10, AwardedMarks: 8},
			{QuestionNo: "Q2", MaxMarks: 10, AwardedMarks: 6},
			{QuestionNo: "Q3", MaxMarks: 10, AwardedMarks: 4},
		}},
	}
	stats := analytics.OutcomeMastery(papers, mapping, meta)
	require.Len(t, stats, 3)
	assert.Equal(t, "A", stats[0].Code)
	assert.Equal(t, "M", stats[1].Code)
	assert.Equal(t, "Z", stats[2].Code)
}

func TestOutcomeMastery_QuestionNotInPaper_Skipped(t *testing.T) {
	// Mapping references Q99 which is not in any paper → outcome has MappedQuestions=1 but Responses=0.
	mapping := map[string][]string{
		"Q1":  {"o1"},
		"Q99": {"o2"}, // no paper has Q99
	}
	meta := map[string]analytics.OutcomeMeta{
		"o1": {Code: "C1"},
		"o2": {Code: "C2"},
	}
	papers := []contracts.GradedPaper{
		{Questions: []contracts.GradedQuestion{{QuestionNo: "Q1", MaxMarks: 5, AwardedMarks: 4}}},
	}
	stats := analytics.OutcomeMastery(papers, mapping, meta)
	require.Len(t, stats, 2)

	// Find C2 (o2)
	var c2 analytics.OutcomeStat
	for _, s := range stats {
		if s.Code == "C2" {
			c2 = s
		}
	}
	assert.Equal(t, 1, c2.MappedQuestions, "Q99 is mapped but not present in papers")
	assert.Equal(t, 0, c2.Responses, "no paper had Q99")
	floatEq(t, 0.0, c2.MeanPct, "no responses → MeanPct=0")
}

// ── LearningGaps ────────────────────────────────────────────────────────────

func TestLearningGaps_HandComputedCase(t *testing.T) {
	papers := makeOutcomeFixturePapers()
	mapping := map[string][]string{
		"Q1": {"outcome-alpha"},
		"Q2": {"outcome-alpha", "outcome-beta"},
	}
	meta := map[string]analytics.OutcomeMeta{
		"outcome-alpha": {Code: "ALPHA", Description: "Alpha outcome"},
		"outcome-beta":  {Code: "BETA", Description: "Beta outcome"},
	}

	stats := analytics.OutcomeMastery(papers, mapping, meta)
	gaps := analytics.LearningGaps(stats, 5)

	// Both outcomes have Responses>0. Sorted by MeanPct asc:
	// BETA (8/15 ≈ 0.5333) before ALPHA (17/30 ≈ 0.5667).
	require.Len(t, gaps, 2)
	assert.Equal(t, "BETA", gaps[0].Code, "BETA is weaker")
	assert.Equal(t, "ALPHA", gaps[1].Code, "ALPHA is stronger")
}

func TestLearningGaps_NLimitsResult(t *testing.T) {
	stats := []analytics.OutcomeStat{
		{OutcomeID: "o1", Code: "A", MeanPct: 0.1, Responses: 3},
		{OutcomeID: "o2", Code: "B", MeanPct: 0.2, Responses: 3},
		{OutcomeID: "o3", Code: "C", MeanPct: 0.3, Responses: 3},
		{OutcomeID: "o4", Code: "D", MeanPct: 0.4, Responses: 3},
	}

	gaps := analytics.LearningGaps(stats, 2)
	require.Len(t, gaps, 2)
	assert.Equal(t, "A", gaps[0].Code)
	assert.Equal(t, "B", gaps[1].Code)
}

func TestLearningGaps_TiesBreakByCode(t *testing.T) {
	stats := []analytics.OutcomeStat{
		{OutcomeID: "o1", Code: "Z", MeanPct: 0.4, Responses: 3},
		{OutcomeID: "o2", Code: "A", MeanPct: 0.4, Responses: 3},
		{OutcomeID: "o3", Code: "M", MeanPct: 0.4, Responses: 3},
	}
	gaps := analytics.LearningGaps(stats, 5)
	require.Len(t, gaps, 3)
	assert.Equal(t, "A", gaps[0].Code, "tie broken by Code ascending")
	assert.Equal(t, "M", gaps[1].Code)
	assert.Equal(t, "Z", gaps[2].Code)
}

func TestLearningGaps_ZeroResponsesExcluded(t *testing.T) {
	stats := []analytics.OutcomeStat{
		{OutcomeID: "o1", Code: "A", MeanPct: 0.1, Responses: 0}, // no responses → excluded
		{OutcomeID: "o2", Code: "B", MeanPct: 0.5, Responses: 3},
	}
	gaps := analytics.LearningGaps(stats, 5)
	require.Len(t, gaps, 1, "outcome with Responses=0 must be excluded")
	assert.Equal(t, "B", gaps[0].Code)
}

func TestLearningGaps_NZeroOrExceedsLen(t *testing.T) {
	stats := []analytics.OutcomeStat{
		{OutcomeID: "o1", Code: "A", MeanPct: 0.3, Responses: 3},
		{OutcomeID: "o2", Code: "B", MeanPct: 0.1, Responses: 3},
	}

	// n=0 → return all eligible sorted
	all := analytics.LearningGaps(stats, 0)
	require.Len(t, all, 2)
	assert.Equal(t, "B", all[0].Code, "lowest MeanPct first")

	// n > len → return all eligible sorted
	all2 := analytics.LearningGaps(stats, 100)
	require.Len(t, all2, 2)
	assert.Equal(t, "B", all2[0].Code)
}

func TestLearningGaps_Empty(t *testing.T) {
	gaps := analytics.LearningGaps(nil, 5)
	assert.Empty(t, gaps, "nil → empty, no panic")

	gaps2 := analytics.LearningGaps([]analytics.OutcomeStat{}, 5)
	assert.Empty(t, gaps2, "empty → empty, no panic")
}
