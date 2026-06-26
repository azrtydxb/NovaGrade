// Package pipeline provides pure pipeline stage functions for NovaGrade.
//
// This file implements the revision-suggestions stage: AI-drafting of
// per-question "how to improve" guidance for students. Revision suggestions
// are strictly additive — they NEVER change awarded_marks or max_marks.
// Each question is processed in isolation so that a provider failure on one
// question does not prevent suggestions for the remaining questions.
package pipeline

import (
	"context"
	"fmt"
	"log"

	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// DraftRevisionSuggestions populates the Revision field of every GradedQuestion
// in graded that does not yet have a revision suggestion, using the given
// AIProvider and model.
//
// Guarantees:
//   - awarded_marks and max_marks on every question are NEVER modified.
//   - Paper-level Total, MaxTotal, Score100, SectionTotals are NEVER modified.
//   - Per-question isolation: if the provider returns an error for one question,
//     that question's Revision remains empty and processing continues for the
//     remaining questions. The function returns nil even when individual questions
//     fail (errors are logged).
//   - Questions that already have a non-empty Revision are skipped (idempotent).
//
// The returned GradedPaper is a new value; the caller's input is not mutated.
func DraftRevisionSuggestions(
	ctx context.Context,
	prov providers.AIProvider,
	model string,
	graded contracts.GradedPaper,
) (contracts.GradedPaper, error) {
	// Copy the questions slice so we don't mutate the caller's input.
	questions := make([]contracts.GradedQuestion, len(graded.Questions))
	copy(questions, graded.Questions)

	for i := range questions {
		q := &questions[i]

		// Skip questions that already have a revision suggestion (idempotent re-runs).
		if q.Revision != "" {
			continue
		}

		prompt := buildRevisionPrompt(*q, graded.Subject)
		req := providers.CompletionReq{
			Model:         model,
			PromptVersion: "revision-v1",
			Messages: []providers.Message{
				{
					Role:    "system",
					Content: revisionSystemPrompt,
				},
				{
					Role:    "user",
					Content: prompt,
				},
			},
			MaxTokens:   512,
			Temperature: 0.3,
		}

		resp, err := prov.Complete(ctx, req)
		if err != nil {
			// Per-question isolation: log and skip, do not abort.
			log.Printf("revision: question %q: provider error (skipping): %v", q.QuestionNo, err)
			continue
		}

		q.Revision = resp.Content
	}

	// Build the output paper: copy the input and replace the questions slice.
	// All marks fields (Total, MaxTotal, Score100, SectionTotals, per-question
	// AwardedMarks, MaxMarks) are copied verbatim — revision never touches marks.
	out := graded
	out.Questions = questions
	return out, nil
}

// revisionSystemPrompt is the system instruction given to the AI for all
// per-question revision-suggestion calls.
const revisionSystemPrompt = `You are an expert examiner providing actionable revision guidance to a student after their exam has been marked.

Your guidance must:
- Be addressed directly to the student (use "you"/"your").
- Focus on concrete, actionable next steps the student should take to improve.
- Reference the specific gap between their answer and what was required, given the marks awarded versus the maximum.
- Suggest study strategies, topics to revisit, or skills to practise.
- Be concise (2–5 sentences).
- NEVER mention the numerical marks awarded or suggest marks could be changed.
- NEVER repeat the feedback already given — focus on forward-looking improvement.`

// buildRevisionPrompt constructs the user-turn prompt for a single question's
// revision suggestion.
func buildRevisionPrompt(q contracts.GradedQuestion, subject string) string {
	section := ""
	if q.Section != nil && *q.Section != "" {
		section = fmt.Sprintf("Section: %s\n", *q.Section)
	}
	return fmt.Sprintf(
		"Subject: %s\n%sQuestion: %s\nAwarded: %.1f / %.1f marks\nStudent answer: %s\nGrader justification: %s\n\nWrite revision guidance for the student:",
		subject,
		section,
		q.QuestionNo,
		q.AwardedMarks,
		q.MaxMarks,
		q.StudentAnswer,
		q.Justification,
	)
}
