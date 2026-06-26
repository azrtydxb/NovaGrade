// Package grade implements the grade pipeline stage. It turns a
// TranscribedPaper into a GradedPaper using a MarkScheme. Objective entries
// (exact / exact_ci / set) are evaluated deterministically — no LLM call.
// Open-ended entries (rubric) and questions absent from the guide fall back to
// an LLMJudge.
//
// Phase-3 match types are deterministically implemented:
// numeric, multi_step, partial, normalize are evaluated without LLM.
// Only rubric entries and unknown/absent match types fall through to the
// fallback LLMJudge.
package grade

import (
	"context"
	"strings"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// MarkScheme is the single grading abstraction used by GradePaper. Any
// implementation must be safe for concurrent use.
type MarkScheme interface {
	// Grade evaluates one transcribed question and returns a GradedQuestion.
	// Implementations must never return a nil Flags slice; use []string{} instead.
	Grade(ctx context.Context, q contracts.TranscribedQuestion) (contracts.GradedQuestion, error)
}

// answerFlags returns the universal per-question flags derived from the raw
// transcribed answer. These are independent of the mark scheme and are
// prepended to every GradedQuestion's Flags slice.
func answerFlags(q contracts.TranscribedQuestion) []string {
	flags := []string{}
	if strings.TrimSpace(q.StudentAnswer) == "" {
		flags = append(flags, "blank_answer")
	} else if q.ReadConfidence < 0.5 {
		flags = append(flags, "low_read_confidence")
	}
	return flags
}

// nonNilFlags guarantees the returned slice is non-nil, satisfying the Task 3
// contract that GradedQuestion.Flags must be []string{}, never nil.
func nonNilFlags(flags []string) []string {
	if flags == nil {
		return []string{}
	}
	return flags
}
