package grade

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

const rubricPrompt = `You are grading one exam question strictly against the official marking guide's rubric. ` +
	`Award marks ONLY as the rubric allows — do not invent your own criteria. ` +
	`Return ONLY a JSON object with keys: ` +
	`"awarded_marks" (number, 0..max), "justification" (one sentence), ` +
	`"grade_confidence" (0..1).`

// StepEntry is a single step within a multi_step marking entry.
// Each step is matched independently against the student answer.
// Fields:
//   - Answer:        expected answer string (for exact/exact_ci step match types)
//   - Accept:        list of acceptable answers (for set step match types)
//   - Marks:         marks awarded when this step matches
//   - Match:         step match type: "exact", "exact_ci", "set", or "numeric"
//   - NumericAnswer: expected numeric value (for numeric step match type)
//   - Tolerance:     tolerance value for numeric step matching
//   - ToleranceType: "abs" or "pct" for numeric step matching
type StepEntry struct {
	Answer        string   `json:"answer,omitempty"`
	Accept        []string `json:"accept,omitempty"`
	Marks         float64  `json:"marks"`
	Match         string   `json:"match"`
	NumericAnswer float64  `json:"numeric_answer,omitempty"`
	Tolerance     float64  `json:"tolerance,omitempty"`
	ToleranceType string   `json:"tolerance_type,omitempty"`
}

// CriterionEntry is one sub-criterion within a partial marking entry.
// Fields:
//   - Accept: list of acceptable answer fragments (case-insensitive substring match)
//   - Marks:  marks awarded when any Accept fragment is found in the student answer
type CriterionEntry struct {
	Accept []string `json:"accept"`
	Marks  float64  `json:"marks"`
}

// GuideEntry is a single entry in a marking guide JSON file.
//
// Core fields (all match types):
//   - MaxMarks: maximum marks for this question (required)
//   - Match:    match type (see below)
//
// Phase-1/2 fields:
//   - Answer:   expected answer string (used by exact / exact_ci match types)
//   - Accept:   list of acceptable answers (used by set match type)
//   - Rubric:   marking rubric prose (used by rubric match type)
//   - Normalize: when true on exact/exact_ci/set, compare after casefold +
//               collapsed whitespace + stripped punctuation (default false)
//
// Phase-3 numeric fields (match type "numeric"):
//   - NumericAnswer: the expected numeric value
//   - Tolerance:     allowed deviation (default 0)
//   - ToleranceType: "abs" (absolute, default) or "pct" (percentage)
//   - Unit:          required unit token (stripped before parsing the number)
//   - UnitMarks:     marks reserved for the correct unit; when > 0, value-correct
//                   but unit-wrong/missing awards (MaxMarks - UnitMarks)
//
// Phase-3 multi_step fields (match type "multi_step"):
//   - Steps: ordered list of StepEntry; each step is matched independently,
//            awarding method marks even when later steps are wrong
//
// Phase-3 partial fields (match type "partial"):
//   - Criteria: list of CriterionEntry; each criterion that matches contributes
//               its Marks to the total (clamped to MaxMarks)
type GuideEntry struct {
	MaxMarks float64  `json:"max_marks"`
	Answer   string   `json:"answer,omitempty"`
	Accept   []string `json:"accept,omitempty"`
	Rubric   string   `json:"rubric,omitempty"`
	Match    string   `json:"match"`
	Normalize bool    `json:"normalize,omitempty"`

	// numeric match type fields
	NumericAnswer float64 `json:"numeric_answer,omitempty"`
	Tolerance     float64 `json:"tolerance,omitempty"`
	ToleranceType string  `json:"tolerance_type,omitempty"`
	Unit          string  `json:"unit,omitempty"`
	UnitMarks     float64 `json:"unit_marks,omitempty"`

	// multi_step match type fields
	Steps []StepEntry `json:"steps,omitempty"`

	// partial match type fields
	Criteria []CriterionEntry `json:"criteria,omitempty"`
}

// Guide is a map from question_no to its GuideEntry.
// Example JSON:
//
//	{
//	  "Q1": {"max_marks": 2, "answer": "Paris", "match": "exact_ci"},
//	  "Q2": {"max_marks": 3, "answer": "H2O",   "match": "exact"},
//	  "Q3": {"max_marks": 1, "accept": ["cat","feline"], "match": "set"},
//	  "Q4": {"max_marks": 5, "rubric": "Award marks for…", "match": "rubric"}
//	}
type Guide map[string]GuideEntry

// LoadGuideFromJSON parses a JSON guide from raw bytes and returns a Guide map.
// Returns an error if the bytes are not valid JSON or do not match the expected
// shape.
func LoadGuideFromJSON(data []byte) (Guide, error) {
	var g Guide
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("grade: parse guide JSON: %w", err)
	}
	return g, nil
}

// TotalMaxMarks returns the sum of MaxMarks across all guide entries, which is
// the canonical maximum score for a paper graded against this guide.
func (g Guide) TotalMaxMarks() float64 {
	var total float64
	for _, e := range g {
		total += e.MaxMarks
	}
	return total
}

// GuideMarkScheme grades against an official marking guide (question_no → GuideEntry).
//
// Objective entries (match exact / exact_ci / set) are graded by deterministic
// string comparison — no LLM call. Open-ended entries (match rubric) invoke the
// LLM bounded by the guide's max_marks. Questions absent from the guide, and any
// entry with an unrecognised match type, fall back to the fallback MarkScheme.
type GuideMarkScheme struct {
	guide    Guide
	fallback MarkScheme
	provider providers.AIProvider
	model    string
}

// NewGuideMarkScheme constructs a GuideMarkScheme.
//   - guide:    the parsed marking guide
//   - fallback: the MarkScheme used for absent/unrecognised questions
//   - provider: AIProvider used for rubric entries (may be nil if no rubric entries)
//   - model:    model name passed to provider.Complete
func NewGuideMarkScheme(guide Guide, fallback MarkScheme, provider providers.AIProvider, model string) *GuideMarkScheme {
	return &GuideMarkScheme{
		guide:    guide,
		fallback: fallback,
		provider: provider,
		model:    model,
	}
}

// Grade grades a single TranscribedQuestion against the guide.
//
// Decision tree:
//  1. If question_no is absent from the guide → fallback.Grade
//  2. If the answer is blank → deterministic zero (no LLM)
//  3. If match is exact/exact_ci/set → deterministic string comparison
//  4. If match is rubric → LLM call bounded by guide max_marks
//  5. Otherwise (unknown match type) → fallback.Grade
func (g *GuideMarkScheme) Grade(ctx context.Context, q contracts.TranscribedQuestion) (contracts.GradedQuestion, error) {
	entry, ok := g.guide[q.QuestionNo]
	if !ok {
		return g.fallback.Grade(ctx, q)
	}

	maxMarks := entry.MaxMarks
	if maxMarks == 0 {
		maxMarks = q.MaxMarks // guide omitted max_marks; use the paper's value
	}

	flags := answerFlags(q)

	// Blank answer: deterministic zero, no LLM.
	if strings.TrimSpace(q.StudentAnswer) == "" {
		return contracts.GradedQuestion{
			QuestionNo:      q.QuestionNo,
			Section:         q.Section,
			MaxMarks:        maxMarks,
			AwardedMarks:    0,
			StudentAnswer:   q.StudentAnswer,
			Justification:   "blank answer",
			GradeConfidence: 1.0,
			Flags:           nonNilFlags(flags),
		}, nil
	}

	switch entry.Match {
	case "exact", "exact_ci", "set":
		var awarded float64
		var justification string
		var confidence float64

		if entry.Normalize {
			// Phase-3: normalized wording comparison
			var awardedN float64
			awardedN, confidence, justification = MatchObjectiveNormalized(entry, strings.TrimSpace(q.StudentAnswer))
			awarded = awardedN
		} else {
			// Original deterministic string comparison (unchanged behavior)
			ok := objectiveMatch(entry, strings.TrimSpace(q.StudentAnswer), entry.Match)
			if ok {
				awarded = maxMarks
				justification = "matches marking guide"
			} else {
				awarded = 0
				justification = "does not match marking guide"
			}
			confidence = 1.0
		}

		return contracts.GradedQuestion{
			QuestionNo:      q.QuestionNo,
			Section:         q.Section,
			MaxMarks:        maxMarks,
			AwardedMarks:    clamp(awarded, 0, maxMarks),
			StudentAnswer:   q.StudentAnswer,
			Justification:   justification,
			GradeConfidence: confidence,
			Flags:           nonNilFlags(flags),
		}, nil

	case "numeric":
		// Phase-3: deterministic numeric matching with tolerance and optional unit marks.
		awarded, confidence, justification := MatchNumeric(entry, strings.TrimSpace(q.StudentAnswer))
		return contracts.GradedQuestion{
			QuestionNo:      q.QuestionNo,
			Section:         q.Section,
			MaxMarks:        maxMarks,
			AwardedMarks:    clamp(awarded, 0, maxMarks),
			StudentAnswer:   q.StudentAnswer,
			Justification:   justification,
			GradeConfidence: confidence,
			Flags:           nonNilFlags(flags),
		}, nil

	case "multi_step":
		// Phase-3: deterministic per-step method marks.
		awarded, confidence, justification := MatchMultiStep(entry, q.StudentAnswer)
		return contracts.GradedQuestion{
			QuestionNo:      q.QuestionNo,
			Section:         q.Section,
			MaxMarks:        maxMarks,
			AwardedMarks:    clamp(awarded, 0, maxMarks),
			StudentAnswer:   q.StudentAnswer,
			Justification:   justification,
			GradeConfidence: confidence,
			Flags:           nonNilFlags(flags),
		}, nil

	case "partial":
		// Phase-3: deterministic per-criterion partial marks.
		awarded, confidence, justification := MatchPartial(entry, q.StudentAnswer)
		return contracts.GradedQuestion{
			QuestionNo:      q.QuestionNo,
			Section:         q.Section,
			MaxMarks:        maxMarks,
			AwardedMarks:    clamp(awarded, 0, maxMarks),
			StudentAnswer:   q.StudentAnswer,
			Justification:   justification,
			GradeConfidence: confidence,
			Flags:           nonNilFlags(flags),
		}, nil

	case "rubric":
		if g.provider == nil {
			log.Printf("grade: rubric entry %q has no AI provider; grading via fallback without rubric context", q.QuestionNo)
			return g.fallback.Grade(ctx, q)
		}
		prompt := fmt.Sprintf(
			"%s\n\nQuestion %s: %s\nMaximum marks: %g\nMarking guide rubric: %s\nStudent answer: %q",
			rubricPrompt, q.QuestionNo, q.QuestionText, maxMarks, entry.Rubric, q.StudentAnswer,
		)
		return awardFromLLM(ctx, g.provider, g.model, prompt, q, maxMarks, flags)

	default:
		// Unknown match type — defer to fallback (LLMJudge).
		return g.fallback.Grade(ctx, q)
	}
}

// objectiveMatch performs a deterministic string comparison between answer and
// the guide entry according to the match type.
func objectiveMatch(entry GuideEntry, answer, match string) bool {
	switch match {
	case "set":
		for _, a := range entry.Accept {
			if strings.EqualFold(answer, strings.TrimSpace(a)) {
				return true
			}
		}
		return false
	case "exact_ci":
		return strings.EqualFold(answer, strings.TrimSpace(entry.Answer))
	default: // "exact"
		return answer == strings.TrimSpace(entry.Answer)
	}
}
