package analytics

// outcomes.go — pure, deterministic functions for outcome-mastery analytics.
//
// All functions are free of I/O, randomness, and global state.  They never
// panic on empty or degenerate input; instead they return zero-value results.
//
// Mastery thresholds (MeanPct):
//   >= 0.75 → "secure"
//   >= 0.50 → "developing"
//   else    → "emerging"

import (
	"sort"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// OutcomeMeta carries display metadata for a curriculum outcome.
type OutcomeMeta struct {
	Code        string
	Description string
}

// OutcomeStat holds per-outcome aggregate statistics computed by [OutcomeMastery].
type OutcomeStat struct {
	OutcomeID       string  `json:"outcome_id"`
	Code            string  `json:"code"`
	Description     string  `json:"description"`
	MappedQuestions int     `json:"mapped_questions"`
	Responses       int     `json:"responses"`
	MeanPct         float64 `json:"mean_pct"`
	Mastery         string  `json:"mastery"`
}

// OutcomeMastery computes per-outcome mastery statistics across a set of graded
// papers.
//
// Parameters:
//   - papers:  the graded papers to analyse.
//   - mapping: question_no → []outcomeID (string form of uuid). Questions not
//     present in any paper still contribute to MappedQuestions but not to
//     Responses or the Σawarded/Σmax sums.
//   - meta:    outcomeID → {Code, Description} for display fields.
//
// For each outcome:
//
//	MeanPct   = Σawarded / Σmax across all (paper, mapped-question) pairs
//	           where that question appears in the paper; guard: Σmax==0 → 0.
//	Responses = count of (paper, question) contributions.
//
// Result is sorted stably by Code ascending.  Empty input (papers or mapping)
// returns an empty slice and never panics.
func OutcomeMastery(
	papers []contracts.GradedPaper,
	mapping map[string][]string,
	meta map[string]OutcomeMeta,
) []OutcomeStat {
	if len(papers) == 0 || len(mapping) == 0 {
		return []OutcomeStat{}
	}

	// Accumulate per-outcome sums.
	type entry struct {
		sumAwarded float64
		sumMax     float64
		responses  int
		// distinct question_nos that map to this outcome
		questions map[string]struct{}
	}

	index := map[string]*entry{}

	// Initialise entries from the mapping so that every outcome in the mapping
	// appears in the result, even if no paper contributed data for it.
	for qno, outcomeIDs := range mapping {
		for _, oid := range outcomeIDs {
			e, exists := index[oid]
			if !exists {
				e = &entry{questions: map[string]struct{}{}}
				index[oid] = e
			}
			e.questions[qno] = struct{}{}
		}
	}

	// Walk every paper × question × outcome-id triple.
	for i := range papers {
		p := &papers[i]
		for j := range p.Questions {
			q := &p.Questions[j]
			outcomeIDs, mapped := mapping[q.QuestionNo]
			if !mapped {
				continue
			}
			for _, oid := range outcomeIDs {
				e := index[oid]
				e.sumAwarded += q.AwardedMarks
				e.sumMax += q.MaxMarks
				e.responses++
			}
		}
	}

	// Build the result slice.
	stats := make([]OutcomeStat, 0, len(index))
	for oid, e := range index {
		m := meta[oid]

		var meanPct float64
		if e.sumMax != 0 {
			meanPct = e.sumAwarded / e.sumMax
		}

		stats = append(stats, OutcomeStat{
			OutcomeID:       oid,
			Code:            m.Code,
			Description:     m.Description,
			MappedQuestions: len(e.questions),
			Responses:       e.responses,
			MeanPct:         meanPct,
			Mastery:         masteryLabel(meanPct),
		})
	}

	// Sort stably by Code ascending.
	sort.SliceStable(stats, func(i, j int) bool {
		return stats[i].Code < stats[j].Code
	})

	return stats
}

// masteryLabel maps a MeanPct value to a mastery label string.
func masteryLabel(pct float64) string {
	switch {
	case pct >= 0.75:
		return "secure"
	case pct >= 0.50:
		return "developing"
	default:
		return "emerging"
	}
}

// LearningGaps returns the n weakest outcomes (lowest MeanPct first; ties
// broken by Code ascending) among outcomes where Responses > 0.
//
// If n <= 0 or n >= count of eligible outcomes, all eligible outcomes are
// returned sorted.  Empty input or all zero-response outcomes returns an
// empty (non-nil) slice.  The function never panics.
func LearningGaps(stats []OutcomeStat, n int) []OutcomeStat {
	if len(stats) == 0 {
		return []OutcomeStat{}
	}

	// Filter to outcomes with at least one response.
	eligible := make([]OutcomeStat, 0, len(stats))
	for _, s := range stats {
		if s.Responses > 0 {
			eligible = append(eligible, s)
		}
	}
	if len(eligible) == 0 {
		return []OutcomeStat{}
	}

	// Sort by MeanPct ascending; ties broken by Code ascending.
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].MeanPct != eligible[j].MeanPct {
			return eligible[i].MeanPct < eligible[j].MeanPct
		}
		return eligible[i].Code < eligible[j].Code
	})

	if n <= 0 || n >= len(eligible) {
		return eligible
	}
	return eligible[:n]
}
