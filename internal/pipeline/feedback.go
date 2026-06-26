// Package pipeline provides pure pipeline stage functions for NovaGrade.
//
// This file implements the feedback stage: AI-drafting of per-question student
// feedback. Feedback is strictly additive — it NEVER changes awarded_marks or
// max_marks. Each question is processed in isolation so that a provider failure
// on one question does not prevent feedback for the remaining questions.
package pipeline

import (
	"context"
	"fmt"
	"log"

	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// DraftFeedback populates the Feedback field of every GradedQuestion in graded
// that does not yet have feedback, using the given AIProvider and model.
//
// Guarantees:
//   - awarded_marks and max_marks on every question are NEVER modified.
//   - Paper-level Total, MaxTotal, Score100, SectionTotals are NEVER modified.
//   - Per-question isolation: if the provider returns an error for one question,
//     that question's Feedback remains empty and processing continues for the
//     remaining questions. The function returns nil even when individual questions
//     fail (errors are logged).
//   - Questions that already have a non-empty Feedback are skipped (idempotent).
//
// The returned GradedPaper is a new value; the caller's input is not mutated.
func DraftFeedback(
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

		// Skip questions that already have feedback (idempotent re-runs).
		if q.Feedback != "" {
			continue
		}

		prompt := buildFeedbackPrompt(*q, graded.Subject)
		req := providers.CompletionReq{
			Model:         model,
			PromptVersion: "feedback-v1",
			Messages: []providers.Message{
				{
					Role:    "system",
					Content: feedbackSystemPrompt,
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
			log.Printf("feedback: question %q: provider error (skipping): %v", q.QuestionNo, err)
			continue
		}

		q.Feedback = resp.Content
	}

	// Build the output paper: copy the input and replace the questions slice.
	// All marks fields (Total, MaxTotal, Score100, SectionTotals, per-question
	// AwardedMarks, MaxMarks) are copied verbatim — feedback never touches marks.
	out := graded
	out.Questions = questions
	return out, nil
}

// feedbackSystemPrompt is the system instruction given to the AI for all
// per-question feedback calls.
const feedbackSystemPrompt = `You are an expert examiner providing constructive, student-facing feedback on a marked exam answer.

Your feedback must:
- Be addressed directly to the student (use "you"/"your").
- Acknowledge what the student did correctly.
- Clearly explain any errors or omissions.
- Suggest how to improve if marks were lost.
- Be concise (2–5 sentences).
- NEVER mention the numerical marks awarded.
- NEVER suggest that marks could be changed or contested.`

// buildFeedbackPrompt constructs the user-turn prompt for a single question.
func buildFeedbackPrompt(q contracts.GradedQuestion, subject string) string {
	section := ""
	if q.Section != nil && *q.Section != "" {
		section = fmt.Sprintf("Section: %s\n", *q.Section)
	}
	return fmt.Sprintf(
		"Subject: %s\n%sQuestion: %s\nStudent answer: %s\nGrader justification: %s\n\nWrite student feedback:",
		subject,
		section,
		q.QuestionNo,
		q.StudentAnswer,
		q.Justification,
	)
}
