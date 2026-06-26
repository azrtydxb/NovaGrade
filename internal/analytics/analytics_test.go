package analytics_test

import (
	"math"
	"testing"

	"github.com/azrtydxb/novagrade/internal/analytics"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- helpers ---------------------------------------------------------------

func floatEq(t *testing.T, want, got float64, msg string) {
	t.Helper()
	if math.IsNaN(want) {
		assert.True(t, math.IsNaN(got), msg)
		return
	}
	assert.InDelta(t, want, got, 1e-9, msg)
}

func sectionPtr(s string) *string { return &s }

// ---- fixture ---------------------------------------------------------------
//
// 3 papers, 2 questions: Q1 and Q2.
//
//	Paper A: Q1=4/5, Q2=3/5  → total=7
//	Paper B: Q1=5/5, Q2=5/5  → total=10
//	Paper C: Q1=0/5, Q2=0/5  → total=0
//
// Q1 per-student:  [4, 5, 0]; Q2 per-student: [3, 5, 0]
// totals:          [7, 10, 0]
//
// Q1 mean = (4+5+0)/3 = 3
// Q2 mean = (3+5+0)/3 = 8/3 ≈ 2.6667
//
// Q1 difficulty = 3/5 = 0.6
// Q2 difficulty = (8/3)/5 = 8/15 ≈ 0.5333
//
// Q1 PctFullMarks = 1/3 (only B), PctZero = 1/3 (only C)
// Q2 PctFullMarks = 1/3 (only B), PctZero = 1/3 (only C)
//
// Discrimination (Pearson correlation of question-score vs total-score):
//
//	Q1 scores: x = [4, 5, 0], totals: y = [7, 10, 0]
//	mean_x = 3, mean_y = 17/3
//	dx = [-1, 2, -3-... wait let me redo:
//	x = [4,5,0], mean_x = 3
//	y = [7,10,0], mean_y = 17/3
//	dx = [1, 2, -3], dy = [7-17/3, 10-17/3, -17/3] = [4/3, 13/3, -17/3]
//	cov = (1*4/3 + 2*13/3 + (-3)*(-17/3)) / 3
//	    = (4/3 + 26/3 + 51/3) / 3
//	    = (81/3) / 3 = 27/3 = 9
//	var_x = (1 + 4 + 9)/3 = 14/3
//	var_y = ((4/3)^2 + (13/3)^2 + (17/3)^2)/3
//	      = (16/9 + 169/9 + 289/9)/3
//	      = (474/9)/3 = 474/27 = 158/9
//	r_Q1 = 9 / sqrt(14/3 * 158/9) = 9 / sqrt(2212/27)
//	     = 9 / sqrt(81.9259...) = 9 / 9.0513... ≈ 0.9943
//
// For a perfectly-tracking question (Q1 ≈ total), discrimination should be close to 1.
//
// Q2 for reference:
//
//	x = [3,5,0], mean_x = 8/3
//	dx = [1/3, 7/3, -8/3]
//	cov_Q2 = (1/3*4/3 + 7/3*13/3 + (-8/3)*(-17/3))/3
//	       = (4/9 + 91/9 + 136/9)/3 = (231/9)/3 = 231/27 = 77/9
//	var_x2 = ((1/9)+(49/9)+(64/9))/3 = (114/9)/3 = 114/27 = 38/9
//	r_Q2 = (77/9) / sqrt(38/9 * 158/9)
//	     = (77/9) / sqrt(6004/81)
//	     = (77/9) / (sqrt(6004)/9)
//	     = 77 / sqrt(6004)  ≈ 77/77.484... ≈ 0.9938
//
// Both are high since both questions track total perfectly in this fixture.

func makeFixturePapers() []contracts.GradedPaper {
	sec := sectionPtr("A")
	return []contracts.GradedPaper{
		{
			Subject: "Math", SourcePDF: "a.pdf",
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", Section: sec, MaxMarks: 5, AwardedMarks: 4, Flags: []string{"flag_a"}},
				{QuestionNo: "Q2", Section: sec, MaxMarks: 5, AwardedMarks: 3, Flags: []string{"flag_b", "flag_a"}},
			},
			Total: 7, MaxTotal: 10, Score100: 70,
		},
		{
			Subject: "Math", SourcePDF: "b.pdf",
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", Section: sec, MaxMarks: 5, AwardedMarks: 5, Flags: nil},
				{QuestionNo: "Q2", Section: sec, MaxMarks: 5, AwardedMarks: 5, Flags: []string{"flag_b"}},
			},
			Total: 10, MaxTotal: 10, Score100: 100,
		},
		{
			Subject: "Math", SourcePDF: "c.pdf",
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", Section: sec, MaxMarks: 5, AwardedMarks: 0, Flags: []string{"flag_a"}},
				{QuestionNo: "Q2", Section: sec, MaxMarks: 5, AwardedMarks: 0, Flags: nil},
			},
			Total: 0, MaxTotal: 10, Score100: 0,
		},
	}
}

// ---- ItemAnalysis ----------------------------------------------------------

func TestItemAnalysis_FixturePapers(t *testing.T) {
	papers := makeFixturePapers()
	stats := analytics.ItemAnalysis(papers)

	require.Len(t, stats, 2, "expected 2 question stats")

	// Sorted by QuestionNo → Q1 first
	q1 := stats[0]
	q2 := stats[1]

	assert.Equal(t, "Q1", q1.QuestionNo)
	assert.Equal(t, "Q2", q2.QuestionNo)

	// Responses
	assert.Equal(t, 3, q1.Responses)
	assert.Equal(t, 3, q2.Responses)

	// MaxMarks
	assert.Equal(t, 5.0, q1.MaxMarks)
	assert.Equal(t, 5.0, q2.MaxMarks)

	// MeanAwarded
	floatEq(t, 3.0, q1.MeanAwarded, "Q1 mean awarded")
	floatEq(t, 8.0/3.0, q2.MeanAwarded, "Q2 mean awarded")

	// Difficulty
	floatEq(t, 3.0/5.0, q1.Difficulty, "Q1 difficulty")
	floatEq(t, (8.0/3.0)/5.0, q2.Difficulty, "Q2 difficulty")

	// PctFullMarks (exactly 1 out of 3 got full marks for each)
	floatEq(t, 1.0/3.0, q1.PctFullMarks, "Q1 pct full marks")
	floatEq(t, 1.0/3.0, q2.PctFullMarks, "Q2 pct full marks")

	// PctZero (exactly 1 out of 3 got zero for each)
	floatEq(t, 1.0/3.0, q1.PctZero, "Q1 pct zero")
	floatEq(t, 1.0/3.0, q2.PctZero, "Q2 pct zero")

	// Discrimination — should be close to 1 (see comment block above)
	// We verify sign and approximate magnitude rather than exact value.
	assert.Greater(t, q1.Discrimination, 0.99, "Q1 discrimination should be close to 1")
	assert.Greater(t, q2.Discrimination, 0.99, "Q2 discrimination should be close to 1")
	assert.LessOrEqual(t, q1.Discrimination, 1.0, "discrimination cannot exceed 1")
	assert.LessOrEqual(t, q2.Discrimination, 1.0, "discrimination cannot exceed 1")

	// Exact hand-computed value for Q1: 9 / sqrt(14/3 * 158/9)
	expectedDiscQ1 := 9.0 / math.Sqrt(14.0/3.0*158.0/9.0)
	assert.InDelta(t, expectedDiscQ1, q1.Discrimination, 1e-9, "Q1 discrimination exact")
}

func TestItemAnalysis_ConstantQuestion_ZeroDiscrimination(t *testing.T) {
	// A question where every student gets the same mark → zero variance in question scores → discrimination = 0.
	// But totals differ (varying Q2 gives different totals).
	papers := []contracts.GradedPaper{
		{
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", MaxMarks: 5, AwardedMarks: 5},
				{QuestionNo: "Q2", MaxMarks: 5, AwardedMarks: 1},
			},
			Total: 6, Score100: 60,
		},
		{
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", MaxMarks: 5, AwardedMarks: 5},
				{QuestionNo: "Q2", MaxMarks: 5, AwardedMarks: 4},
			},
			Total: 9, Score100: 90,
		},
	}
	stats := analytics.ItemAnalysis(papers)
	require.Len(t, stats, 2)
	q1 := stats[0]
	assert.Equal(t, 0.0, q1.Discrimination, "constant question → zero discrimination")
}

func TestItemAnalysis_Empty(t *testing.T) {
	stats := analytics.ItemAnalysis(nil)
	assert.Empty(t, stats)

	stats2 := analytics.ItemAnalysis([]contracts.GradedPaper{})
	assert.Empty(t, stats2)
}

func TestItemAnalysis_SinglePaper_NLessThan2(t *testing.T) {
	// N=1 → cannot compute correlation → discrimination = 0
	papers := []contracts.GradedPaper{
		{
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", MaxMarks: 10, AwardedMarks: 7},
			},
			Total: 7, Score100: 70,
		},
	}
	stats := analytics.ItemAnalysis(papers)
	require.Len(t, stats, 1)
	assert.Equal(t, 0.0, stats[0].Discrimination, "N<2 → discrimination=0")
	assert.Equal(t, 1, stats[0].Responses)
	floatEq(t, 7.0, stats[0].MeanAwarded, "single paper mean")
	floatEq(t, 0.7, stats[0].Difficulty, "single paper difficulty")
}

func TestItemAnalysis_MaxMarksZero(t *testing.T) {
	// MaxMarks=0 → difficulty=0, no panic
	papers := []contracts.GradedPaper{
		{
			Questions: []contracts.GradedQuestion{
				{QuestionNo: "Q1", MaxMarks: 0, AwardedMarks: 0},
			},
			Total: 0, Score100: 0,
		},
	}
	stats := analytics.ItemAnalysis(papers)
	require.Len(t, stats, 1)
	assert.Equal(t, 0.0, stats[0].Difficulty, "MaxMarks=0 → difficulty=0")
}

// ---- GradeDistribution ----------------------------------------------------

func TestGradeDistribution_KnownScores(t *testing.T) {
	// Scores: 0, 55, 75, 100
	// mean = 57.5, min=0, max=100
	// median of [0, 55, 75, 100] (sorted even count) = (55+75)/2 = 65
	// Bucket 0: [0,10)    → {0}       count=1
	// Bucket 5: [50,60)   → {55}      count=1
	// Bucket 7: [70,80)   → {75}      count=1
	// Bucket 9: [90,100]  → {100}     count=1
	papers := []contracts.GradedPaper{
		{Score100: 0},
		{Score100: 55},
		{Score100: 75},
		{Score100: 100},
	}
	d := analytics.GradeDistribution(papers)

	assert.Equal(t, 4, d.Count)
	floatEq(t, 0.0, d.Min, "min")
	floatEq(t, 100.0, d.Max, "max")
	floatEq(t, 57.5, d.Mean, "mean")
	floatEq(t, 65.0, d.Median, "median")

	// StdDev: variance = ((0-57.5)^2 + (55-57.5)^2 + (75-57.5)^2 + (100-57.5)^2) / 4
	//       = (3306.25 + 6.25 + 306.25 + 1806.25) / 4 = 5425/4 = 1356.25
	//       stddev = sqrt(1356.25) ≈ 36.8274...
	expectedStdDev := math.Sqrt(1356.25)
	assert.InDelta(t, expectedStdDev, d.StdDev, 1e-9, "stddev")

	require.Len(t, d.Buckets, 10, "must have 10 buckets")

	// Check bucket labels and the counts we expect
	bucketCounts := make(map[string]int)
	for _, b := range d.Buckets {
		bucketCounts[b.Label] = b.Count
	}
	assert.Equal(t, 1, bucketCounts["0-9"], "bucket 0-9")
	assert.Equal(t, 1, bucketCounts["50-59"], "bucket 50-59")
	assert.Equal(t, 1, bucketCounts["70-79"], "bucket 70-79")
	assert.Equal(t, 1, bucketCounts["90-100"], "bucket 90-100 (includes 100)")

	// Verify sum of all bucket counts equals total
	total := 0
	for _, b := range d.Buckets {
		total += b.Count
	}
	assert.Equal(t, 4, total, "sum of bucket counts")
}

func TestGradeDistribution_Score100_LandsInLastBucket(t *testing.T) {
	papers := []contracts.GradedPaper{{Score100: 100}}
	d := analytics.GradeDistribution(papers)
	require.Len(t, d.Buckets, 10)
	lastBucket := d.Buckets[9]
	assert.Equal(t, 1, lastBucket.Count, "score 100 must land in the last bucket")
}

func TestGradeDistribution_Empty(t *testing.T) {
	d := analytics.GradeDistribution(nil)
	assert.Equal(t, 0, d.Count)
	assert.Equal(t, 0.0, d.Mean)
	assert.Equal(t, 0.0, d.Median)
	assert.Equal(t, 0.0, d.StdDev)
	assert.Equal(t, 0.0, d.Min)
	assert.Equal(t, 0.0, d.Max)
	require.Len(t, d.Buckets, 10, "10 buckets even when empty")
	for _, b := range d.Buckets {
		assert.Equal(t, 0, b.Count)
	}
}

// ---- HardestQuestions -----------------------------------------------------

func TestHardestQuestions_Order(t *testing.T) {
	stats := []analytics.QuestionStat{
		{QuestionNo: "Q1", Difficulty: 0.8},
		{QuestionNo: "Q2", Difficulty: 0.2},
		{QuestionNo: "Q3", Difficulty: 0.5},
		{QuestionNo: "Q4", Difficulty: 0.2}, // tie with Q2 → break by QuestionNo
	}
	hardest := analytics.HardestQuestions(stats, 3)
	require.Len(t, hardest, 3)
	assert.Equal(t, "Q2", hardest[0].QuestionNo, "lowest difficulty first; Q2 before Q4 (alpha)")
	assert.Equal(t, "Q4", hardest[1].QuestionNo)
	assert.Equal(t, "Q3", hardest[2].QuestionNo)
}

func TestHardestQuestions_NZeroOrExceedsLen(t *testing.T) {
	stats := []analytics.QuestionStat{
		{QuestionNo: "Q1", Difficulty: 0.8},
		{QuestionNo: "Q2", Difficulty: 0.2},
	}
	// n=0 → return all sorted
	all := analytics.HardestQuestions(stats, 0)
	require.Len(t, all, 2)
	assert.Equal(t, "Q2", all[0].QuestionNo)

	// n > len → return all sorted
	all2 := analytics.HardestQuestions(stats, 100)
	require.Len(t, all2, 2)
	assert.Equal(t, "Q2", all2[0].QuestionNo)
}

func TestHardestQuestions_Empty(t *testing.T) {
	result := analytics.HardestQuestions(nil, 5)
	assert.Empty(t, result)
}

// ---- FlagFrequencies -------------------------------------------------------

func TestFlagFrequencies_FixturePapers(t *testing.T) {
	// From makeFixturePapers():
	// Paper A: Q1 flags=[flag_a], Q2 flags=[flag_b, flag_a]
	// Paper B: Q1 flags=nil, Q2 flags=[flag_b]
	// Paper C: Q1 flags=[flag_a], Q2 flags=nil
	// Expected: flag_a=3, flag_b=2
	papers := makeFixturePapers()
	freq := analytics.FlagFrequencies(papers)
	assert.Equal(t, 3, freq["flag_a"])
	assert.Equal(t, 2, freq["flag_b"])
	assert.Len(t, freq, 2)
}

func TestFlagFrequencies_Empty(t *testing.T) {
	freq := analytics.FlagFrequencies(nil)
	assert.NotNil(t, freq)
	assert.Empty(t, freq)

	freq2 := analytics.FlagFrequencies([]contracts.GradedPaper{})
	assert.Empty(t, freq2)
}

func TestFlagFrequencies_NoFlags(t *testing.T) {
	papers := []contracts.GradedPaper{
		{Questions: []contracts.GradedQuestion{
			{QuestionNo: "Q1", Flags: nil},
			{QuestionNo: "Q2", Flags: []string{}},
		}},
	}
	freq := analytics.FlagFrequencies(papers)
	assert.Empty(t, freq)
}
