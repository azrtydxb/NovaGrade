package grade

import (
	"context"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// GradePaper grades all questions in paper using scheme and assembles a
// GradedPaper with derived totals and section breakdowns.
//
// Behaviour:
//   - Per-question isolation: a grading error for one question does not abort
//     the remaining questions. The failed question is flagged "grading_failed"
//     and receives AwardedMarks=0.
//   - Blank answers (strings.TrimSpace == "") are flagged "blank_answer" and
//     receive AwardedMarks=0 without any LLM call. This is enforced inside
//     each MarkScheme implementation; GradePaper itself just accumulates results.
//   - AwardedMarks are clamped to [0, MaxMarks] inside each MarkScheme.
//   - Flags are guaranteed non-nil ([]string{}) by each MarkScheme implementation.
//   - max_total is derived from the sum of GradedQuestion.MaxMarks returned by
//     the scheme (which may differ from TranscribedQuestion.MaxMarks when a
//     guide overrides per-question max_marks).
//   - score_100 = round(100 * total / max_total, 1); 0 when max_total == 0.
//   - section_totals maps each section letter (GradedQuestion.Section) to the
//     sum of AwardedMarks for that section. Questions with nil Section are
//     bucketed under "?".
func GradePaper(ctx context.Context, scheme MarkScheme, paper contracts.TranscribedPaper) (contracts.GradedPaper, error) {
	graded := make([]contracts.GradedQuestion, 0, len(paper.Questions))

	for _, q := range paper.Questions {
		gq, err := scheme.Grade(ctx, q)
		if err != nil {
			// Isolate the failure: flag it and continue.
			gq = gradingFailed(q, q.MaxMarks, answerFlags(q), err.Error())
		}
		// Guarantee non-nil flags at the aggregation layer as well.
		gq.Flags = nonNilFlags(gq.Flags)
		graded = append(graded, gq)
	}

	// Aggregate totals.
	var total, maxTotal float64
	sectionTotals := map[string]float64{}

	for _, gq := range graded {
		total += gq.AwardedMarks
		maxTotal += gq.MaxMarks

		key := "?"
		if gq.Section != nil {
			key = *gq.Section
		}
		sectionTotals[key] += gq.AwardedMarks
	}

	var score100 float64
	if maxTotal > 0 {
		score100 = roundTo1(100 * total / maxTotal)
	}

	return contracts.GradedPaper{
		Subject:       paper.Subject,
		SourcePDF:     paper.SourcePDF,
		Questions:     graded,
		SectionTotals: sectionTotals,
		Total:         total,
		MaxTotal:      maxTotal,
		Score100:      score100,
		ExpectedTotal: paper.ExpectedTotal,
	}, nil
}

// roundTo1 rounds f to one decimal place (mirrors the Python round(x, 1)).
func roundTo1(f float64) float64 {
	// Use integer arithmetic to avoid floating-point drift.
	shifted := f * 10
	rounded := float64(int64(shifted+0.5)) / 10
	return rounded
}
