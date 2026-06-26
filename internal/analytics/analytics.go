// Package analytics provides pure, deterministic functions for analysing
// a set of graded exam papers ([contracts.GradedPaper]).  All functions are
// free of I/O, randomness, and global state.  They never panic on empty or
// degenerate input; instead they return zero-value results.
package analytics

import (
	"fmt"
	"math"
	"sort"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// QuestionStat holds per-question aggregate statistics computed by
// [ItemAnalysis].
type QuestionStat struct {
	QuestionNo   string  `json:"question_no"`
	Responses    int     `json:"responses"`    // number of papers that contained this question
	MaxMarks     float64 `json:"max_marks"`    // first-seen MaxMarks across all papers (see MaxMarks policy below)
	MeanAwarded  float64 `json:"mean_awarded"` // arithmetic mean of AwardedMarks
	Difficulty   float64 `json:"difficulty"`   // MeanAwarded / MaxMarks; 0 if MaxMarks == 0
	PctFullMarks float64 `json:"pct_full_marks"` // fraction of responses where AwardedMarks == MaxMarks
	PctZero      float64 `json:"pct_zero"`       // fraction of responses where AwardedMarks == 0
	// Discrimination is the Pearson correlation coefficient between each
	// student's AwardedMarks on this question and their paper Total.
	//
	// Formula (for N students):
	//
	//   Let x_i = AwardedMarks[i] for student i,  x̄ = mean(x)
	//   Let y_i = paper Total[i] for student i,    ȳ = mean(y)
	//
	//   r = Σ(x_i - x̄)(y_i - ȳ) / [N * σ_x * σ_y]
	//
	// where σ_x = sqrt(Σ(x_i - x̄)² / N)  (population std-dev of question scores)
	// and   σ_y = sqrt(Σ(y_i - ȳ)² / N)  (population std-dev of paper totals).
	//
	// This is equivalent to Cov(x,y) / (σ_x * σ_y) using population statistics.
	//
	// Guard conditions: if N < 2, or σ_x == 0, or σ_y == 0, Discrimination = 0.
	Discrimination float64 `json:"discrimination"`
}

// Bucket is a single histogram bar in [Distribution].
type Bucket struct {
	Label string  `json:"label"` // e.g. "0-9", "10-19", …, "90-100"
	Lo    float64 `json:"lo"`    // inclusive lower bound
	Hi    float64 `json:"hi"`    // inclusive upper bound (last bucket) or exclusive (others)
	Count int     `json:"count"`
}

// Distribution holds the grade histogram and descriptive statistics computed
// by [GradeDistribution].
type Distribution struct {
	Buckets []Bucket `json:"buckets"`
	Mean    float64  `json:"mean"`
	Median  float64  `json:"median"`
	StdDev  float64  `json:"std_dev"`
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Count   int      `json:"count"`
}

// ---------------------------------------------------------------------------
// MaxMarks policy
// ---------------------------------------------------------------------------
// When the same question appears in multiple papers with different MaxMarks
// values, ItemAnalysis uses the FIRST-seen MaxMarks value (i.e. the MaxMarks
// from the first paper in the input slice that contains this QuestionNo).
// This is simple, deterministic, and avoids a potentially expensive mode
// computation across a large question set.  Callers who need a different
// policy should normalise MaxMarks before calling ItemAnalysis.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Bucketing scheme
// ---------------------------------------------------------------------------
// GradeDistribution creates exactly 10 buckets over Score100:
//
//	[0,10), [10,20), [20,30), …, [80,90), [90,100]
//
// Buckets 0-8 are half-open on the right (Lo ≤ x < Hi).
// Bucket 9 (label "90-100") is fully-closed (90 ≤ x ≤ 100) so that a score
// of exactly 100 lands in it rather than falling off the end.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ItemAnalysis
// ---------------------------------------------------------------------------

// ItemAnalysis groups graded questions by QuestionNo across all papers and
// returns one [QuestionStat] per distinct QuestionNo, sorted stably by
// QuestionNo (ascending lexicographic order).
//
// Empty input returns a nil slice; the function never panics.
func ItemAnalysis(papers []contracts.GradedPaper) []QuestionStat {
	if len(papers) == 0 {
		return nil
	}

	// Accumulate per-question data.
	type entry struct {
		maxMarks float64
		awarded  []float64 // one per paper that had this question
		totals   []float64 // paper total for the same paper
	}

	index := map[string]*entry{}
	order := []string{} // insertion order (for stable sort)

	for i := range papers {
		p := &papers[i]
		for j := range p.Questions {
			q := &p.Questions[j]
			e, exists := index[q.QuestionNo]
			if !exists {
				e = &entry{maxMarks: q.MaxMarks}
				index[q.QuestionNo] = e
				order = append(order, q.QuestionNo)
			}
			e.awarded = append(e.awarded, q.AwardedMarks)
			e.totals = append(e.totals, p.Total)
		}
	}

	// Build stats.
	stats := make([]QuestionStat, 0, len(index))
	for _, qno := range order {
		e := index[qno]
		n := len(e.awarded)

		// Mean awarded.
		sumA := 0.0
		for _, a := range e.awarded {
			sumA += a
		}
		mean := sumA / float64(n)

		// Difficulty.
		diff := 0.0
		if e.maxMarks != 0 {
			diff = mean / e.maxMarks
		}

		// PctFullMarks, PctZero.
		// When MaxMarks == 0, both percentages are undefined; set to 0.0.
		var pctFull, pctZero float64
		if e.maxMarks == 0 {
			pctFull = 0.0
			pctZero = 0.0
		} else {
			fullCount, zeroCount := 0, 0
			for _, a := range e.awarded {
				if a == e.maxMarks {
					fullCount++
				}
				if a == 0 {
					zeroCount++
				}
			}
			pctFull = float64(fullCount) / float64(n)
			pctZero = float64(zeroCount) / float64(n)
		}

		// Discrimination: Pearson r of (awarded, paper total).
		disc := pearson(e.awarded, e.totals)

		stats = append(stats, QuestionStat{
			QuestionNo:     qno,
			Responses:      n,
			MaxMarks:       e.maxMarks,
			MeanAwarded:    mean,
			Difficulty:     diff,
			PctFullMarks:   pctFull,
			PctZero:        pctZero,
			Discrimination: disc,
		})
	}

	// Sort by QuestionNo (stable).
	sort.SliceStable(stats, func(i, j int) bool {
		return stats[i].QuestionNo < stats[j].QuestionNo
	})

	return stats
}

// pearson computes the population Pearson correlation coefficient of two equal-
// length slices x and y.  Returns 0 if N < 2, or either slice has zero
// variance.
func pearson(x, y []float64) float64 {
	n := len(x)
	if n < 2 {
		return 0
	}

	meanX, meanY := mean(x), mean(y)

	cov, varX, varY := 0.0, 0.0, 0.0
	for i := 0; i < n; i++ {
		dx := x[i] - meanX
		dy := y[i] - meanY
		cov += dx * dy
		varX += dx * dx
		varY += dy * dy
	}
	denom := math.Sqrt(varX * varY)
	if denom == 0 {
		return 0
	}
	return cov / denom
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, v := range xs {
		s += v
	}
	return s / float64(len(xs))
}

// ---------------------------------------------------------------------------
// GradeDistribution
// ---------------------------------------------------------------------------

// bucketDefs defines the 10 fixed histogram buckets.
var bucketDefs = func() []Bucket {
	bs := make([]Bucket, 10)
	for i := 0; i < 9; i++ {
		lo := float64(i * 10)
		hi := float64((i + 1) * 10)
		bs[i] = Bucket{
			Label: fmt.Sprintf("%d-%d", i*10, (i+1)*10-1),
			Lo:    lo,
			Hi:    hi,
		}
	}
	bs[9] = Bucket{Label: "90-100", Lo: 90, Hi: 100}
	return bs
}()

// GradeDistribution builds a histogram of Score100 values across all papers
// (10 equal-width buckets covering [0,100]) together with descriptive
// statistics (mean, median, stddev, min, max, count).
//
// Empty input returns a zeroed Distribution with 10 empty buckets.
func GradeDistribution(papers []contracts.GradedPaper) Distribution {
	// Initialise buckets from template (reset counts).
	buckets := make([]Bucket, len(bucketDefs))
	copy(buckets, bucketDefs)

	if len(papers) == 0 {
		return Distribution{Buckets: buckets}
	}

	scores := make([]float64, len(papers))
	for i, p := range papers {
		scores[i] = p.Score100
	}

	// Descriptive stats.
	minVal, maxVal := scores[0], scores[0]
	sum := 0.0
	for _, s := range scores {
		sum += s
		if s < minVal {
			minVal = s
		}
		if s > maxVal {
			maxVal = s
		}
	}
	n := len(scores)
	avg := sum / float64(n)

	varianceSum := 0.0
	for _, s := range scores {
		d := s - avg
		varianceSum += d * d
	}
	stddev := math.Sqrt(varianceSum / float64(n))

	// Median (sort a copy).
	sorted := make([]float64, n)
	copy(sorted, scores)
	sort.Float64s(sorted)
	var med float64
	if n%2 == 1 {
		med = sorted[n/2]
	} else {
		med = (sorted[n/2-1] + sorted[n/2]) / 2
	}

	// Populate buckets.
	for _, s := range scores {
		idx := bucketIndex(s)
		buckets[idx].Count++
	}

	return Distribution{
		Buckets: buckets,
		Mean:    avg,
		Median:  med,
		StdDev:  stddev,
		Min:     minVal,
		Max:     maxVal,
		Count:   n,
	}
}

// bucketIndex returns the 0-based bucket index for a Score100 value.
// Values outside [0,100] are clamped to the nearest bucket.
func bucketIndex(score float64) int {
	if score >= 100 {
		return 9
	}
	if score < 0 {
		return 0
	}
	idx := int(score / 10)
	if idx > 9 {
		idx = 9
	}
	return idx
}

// ---------------------------------------------------------------------------
// HardestQuestions
// ---------------------------------------------------------------------------

// HardestQuestions returns the n [QuestionStat] items with the lowest
// Difficulty, sorted ascending by Difficulty (hardest first), with ties
// broken by QuestionNo (ascending).  If n <= 0 or n exceeds the length of
// stats, all items are returned sorted.  Empty input returns nil.
func HardestQuestions(stats []QuestionStat, n int) []QuestionStat {
	if len(stats) == 0 {
		return nil
	}

	// Sort a copy to avoid mutating the caller's slice.
	sorted := make([]QuestionStat, len(stats))
	copy(sorted, stats)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Difficulty != sorted[j].Difficulty {
			return sorted[i].Difficulty < sorted[j].Difficulty
		}
		return sorted[i].QuestionNo < sorted[j].QuestionNo
	})

	if n <= 0 || n > len(sorted) {
		return sorted
	}
	return sorted[:n]
}

// ---------------------------------------------------------------------------
// FlagFrequencies
// ---------------------------------------------------------------------------

// FlagFrequencies counts each flag string across all graded questions in all
// papers.  The returned map is never nil; empty input returns an empty map.
func FlagFrequencies(papers []contracts.GradedPaper) map[string]int {
	freq := make(map[string]int)
	for i := range papers {
		for j := range papers[i].Questions {
			for _, f := range papers[i].Questions[j].Flags {
				freq[f]++
			}
		}
	}
	return freq
}
