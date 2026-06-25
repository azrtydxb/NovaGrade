package grade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

const judgePrompt = `You are grading one exam question. You are given the question, its maximum marks, ` +
	`and the student's answer (already transcribed from handwriting). Decide the correct ` +
	`answer yourself, then award marks. For questions that show working, give partial ` +
	`credit for correct method. Return ONLY a JSON object with keys: ` +
	`"awarded_marks" (number, 0..max), ` +
	`"justification" (one sentence), ` +
	`"grade_confidence" (0..1).`

// llmGradeResponse is the JSON structure returned by the LLM grading prompt.
type llmGradeResponse struct {
	AwardedMarks    float64 `json:"awarded_marks"`
	Justification   string  `json:"justification"`
	GradeConfidence float64 `json:"grade_confidence"`
}

// LLMJudge is a MarkScheme implementation that uses an AIProvider to grade
// questions. It is the fallback for questions absent from the GuideMarkScheme
// or with unknown match types. It is also the sole mark scheme used when no
// guide is available.
type LLMJudge struct {
	provider providers.AIProvider
	model    string
}

// NewLLMJudge constructs a new LLMJudge backed by the given AIProvider and
// using the specified model name.
func NewLLMJudge(provider providers.AIProvider, model string) *LLMJudge {
	return &LLMJudge{provider: provider, model: model}
}

// Grade calls the LLM to grade a single TranscribedQuestion. Blank answers are
// returned immediately as zero without an LLM call. Any LLM or parse failure
// is isolated: the question receives AwardedMarks=0 and the flag "grading_failed".
func (j *LLMJudge) Grade(ctx context.Context, q contracts.TranscribedQuestion) (contracts.GradedQuestion, error) {
	flags := answerFlags(q)

	// Blank answer: deterministic zero, no LLM call.
	if strings.TrimSpace(q.StudentAnswer) == "" {
		return contracts.GradedQuestion{
			QuestionNo:      q.QuestionNo,
			Section:         q.Section,
			MaxMarks:        q.MaxMarks,
			AwardedMarks:    0,
			StudentAnswer:   q.StudentAnswer,
			Justification:   "blank answer",
			GradeConfidence: 1.0,
			Flags:           nonNilFlags(flags),
		}, nil
	}

	prompt := fmt.Sprintf(
		"%s\n\nQuestion %s: %s\nMaximum marks: %g\nStudent answer: %q",
		judgePrompt, q.QuestionNo, q.QuestionText, q.MaxMarks, q.StudentAnswer,
	)

	return awardFromLLM(ctx, j.provider, j.model, prompt, q, q.MaxMarks, flags)
}

// awardFromLLM runs one grading LLM call and maps the reply to a GradedQuestion.
// On failure it returns a zero-mark question flagged with "grading_failed".
// This function is shared between LLMJudge and GuideMarkScheme (rubric mode).
func awardFromLLM(
	ctx context.Context,
	provider providers.AIProvider,
	model string,
	prompt string,
	q contracts.TranscribedQuestion,
	maxMarks float64,
	flags []string,
) (contracts.GradedQuestion, error) {
	resp, err := provider.Complete(ctx, providers.CompletionReq{
		Model:     model,
		MaxTokens: 400,
		Messages: []providers.Message{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return gradingFailed(q, maxMarks, flags, fmt.Sprintf("LLM call failed: %v", err)), nil
	}

	var result llmGradeResponse
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return gradingFailed(q, maxMarks, flags, fmt.Sprintf("parse LLM response: %v", err)), nil
	}

	awarded := clamp(result.AwardedMarks, 0, maxMarks)
	return contracts.GradedQuestion{
		QuestionNo:      q.QuestionNo,
		Section:         q.Section,
		MaxMarks:        maxMarks,
		AwardedMarks:    awarded,
		StudentAnswer:   q.StudentAnswer,
		Justification:   result.Justification,
		GradeConfidence: result.GradeConfidence,
		Flags:           nonNilFlags(flags),
	}, nil
}

// gradingFailed constructs a zero-mark GradedQuestion carrying the
// "grading_failed" flag. Used by awardFromLLM on any error path.
func gradingFailed(q contracts.TranscribedQuestion, maxMarks float64, flags []string, reason string) contracts.GradedQuestion {
	return contracts.GradedQuestion{
		QuestionNo:      q.QuestionNo,
		Section:         q.Section,
		MaxMarks:        maxMarks,
		AwardedMarks:    0,
		StudentAnswer:   q.StudentAnswer,
		Justification:   fmt.Sprintf("grading failed: %s", reason),
		GradeConfidence: 0,
		Flags:           nonNilFlags(append(flags, "grading_failed")),
	}
}

// clamp returns v bounded to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
