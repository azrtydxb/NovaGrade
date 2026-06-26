package grade

import (
	"fmt"
	"strings"
)

// PreviewSample is a single (question_no, student_answer) pair submitted to
// PreviewGuide. No persistence, no LLM — purely deterministic evaluation.
type PreviewSample struct {
	QuestionNo    string `json:"question_no"`
	StudentAnswer string `json:"student_answer"`
}

// PreviewResult is the per-sample result returned by PreviewGuide.
//
//   - Previewable: true when the match type is deterministic (exact/exact_ci/
//     set/numeric/multi_step/partial, with or without normalize); false for rubric,
//     unknown match types, or question_no absent from the guide.
//   - When Previewable is false, Awarded is always 0 and Justification explains why.
type PreviewResult struct {
	QuestionNo    string  `json:"question_no"`
	Awarded       float64 `json:"awarded"`
	MaxMarks      float64 `json:"max_marks"`
	MatchType     string  `json:"match_type"`
	Confidence    float64 `json:"confidence"`
	Justification string  `json:"justification"`
	Previewable   bool    `json:"previewable"`
}

// deterministic match types that PreviewGuide can evaluate without an LLM.
var deterministicMatchTypes = map[string]bool{
	"exact":      true,
	"exact_ci":   true,
	"set":        true,
	"numeric":    true,
	"multi_step": true,
	"partial":    true,
}

// PreviewGuide evaluates each sample against the guide deterministically, using
// the SAME matcher functions as GuideMarkScheme.Grade (single source of truth).
//
// Guarantees:
//   - No AI provider is called — ever.
//   - No persistence (stateless, pure).
//   - rubric entries → Previewable=false, Awarded=0.
//   - unknown match types → Previewable=false, Awarded=0.
//   - question_no absent from guide → Previewable=false, Awarded=0.
//   - len(result) == len(samples) always.
func PreviewGuide(g Guide, samples []PreviewSample) []PreviewResult {
	results := make([]PreviewResult, 0, len(samples))

	for _, s := range samples {
		entry, ok := g[s.QuestionNo]
		if !ok {
			results = append(results, PreviewResult{
				QuestionNo:    s.QuestionNo,
				Awarded:       0,
				MaxMarks:      0,
				MatchType:     "",
				Confidence:    0,
				Justification: fmt.Sprintf("question_no %q not found in guide", s.QuestionNo),
				Previewable:   false,
			})
			continue
		}

		if !deterministicMatchTypes[entry.Match] {
			// rubric or unknown match type — cannot preview without LLM
			results = append(results, PreviewResult{
				QuestionNo:    s.QuestionNo,
				Awarded:       0,
				MaxMarks:      entry.MaxMarks,
				MatchType:     entry.Match,
				Confidence:    0,
				Justification: fmt.Sprintf("match type %q requires LLM evaluation; not previewable deterministically", entry.Match),
				Previewable:   false,
			})
			continue
		}

		// Deterministic evaluation — mirrors GuideMarkScheme.Grade's dispatch exactly.
		var awarded, confidence float64
		var justification string

		switch entry.Match {
		case "exact", "exact_ci", "set":
			if entry.Normalize {
				awarded, confidence, justification = MatchObjectiveNormalized(entry, strings.TrimSpace(s.StudentAnswer))
			} else {
				ok := objectiveMatch(entry, strings.TrimSpace(s.StudentAnswer), entry.Match)
				if ok {
					awarded = entry.MaxMarks
					justification = "matches marking guide"
				} else {
					awarded = 0
					justification = "does not match marking guide"
				}
				confidence = 1.0
			}

		case "numeric":
			awarded, confidence, justification = MatchNumeric(entry, strings.TrimSpace(s.StudentAnswer))

		case "multi_step":
			awarded, confidence, justification = MatchMultiStep(entry, s.StudentAnswer)

		case "partial":
			awarded, confidence, justification = MatchPartial(entry, s.StudentAnswer)
		}

		results = append(results, PreviewResult{
			QuestionNo:    s.QuestionNo,
			Awarded:       clamp(awarded, 0, entry.MaxMarks),
			MaxMarks:      entry.MaxMarks,
			MatchType:     entry.Match,
			Confidence:    confidence,
			Justification: justification,
			Previewable:   true,
		})
	}

	return results
}
