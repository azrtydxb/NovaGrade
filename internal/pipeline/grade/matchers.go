package grade

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// ─────────────────────────────────────────────────────────────────────────────
// normalize helpers
// ─────────────────────────────────────────────────────────────────────────────

// normalizeText applies casefold + collapsed whitespace + stripped punctuation.
// This is used by the normalize modifier on exact/exact_ci/set match types.
//
// Rules:
//   - Apostrophes and similar contracting characters (', ') are silently dropped
//     so that "newton's" and "newtons" normalize identically.
//   - All other punctuation is replaced with a space so that "carbon-dioxide"
//     and "carbon,dioxide" normalize to "carbon dioxide".
//   - Multiple spaces are collapsed and the result is trimmed.
func normalizeText(s string) string {
	// casefold (unicode-safe lower)
	s = strings.ToLower(s)

	// Process each rune: drop contracting punctuation, replace other punctuation
	// with space, keep letters/digits/space as-is.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r):
			b.WriteRune(r)
		case r == '\'' || r == '’' || r == '‘': // apostrophe / smart quotes — drop
			// intentionally dropped
		default:
			b.WriteRune(' ') // replace other punctuation with a space
		}
	}
	s = b.String()

	// Collapse internal whitespace and trim
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// ─────────────────────────────────────────────────────────────────────────────
// MatchObjectiveNormalized — exact/exact_ci/set with optional normalize
// ─────────────────────────────────────────────────────────────────────────────

// MatchObjectiveNormalized grades an exact/exact_ci/set entry, applying
// normalize normalization when entry.Normalize is true.
// When Normalize is false it falls back to the original objectiveMatch behaviour.
//
// Returns (awarded, confidence, justification).
// Confidence is always 1.0 (deterministic decision).
// Awarded is entry.MaxMarks on match, 0 otherwise.
func MatchObjectiveNormalized(entry GuideEntry, studentAnswer string) (float64, float64, string) {
	matched := false

	if entry.Normalize {
		normStudent := normalizeText(studentAnswer)
		switch entry.Match {
		case "set":
			for _, a := range entry.Accept {
				if normalizeText(a) == normStudent {
					matched = true
					break
				}
			}
		case "exact_ci":
			matched = normalizeText(entry.Answer) == normStudent
		default: // "exact" with normalize also casefolds
			matched = normalizeText(entry.Answer) == normStudent
		}
	} else {
		matched = objectiveMatch(entry, studentAnswer, entry.Match)
	}

	if matched {
		return entry.MaxMarks, 1.0, "matches marking guide"
	}
	return 0, 1.0, "does not match marking guide"
}

// ─────────────────────────────────────────────────────────────────────────────
// MatchNumeric — numeric tolerance matching with optional unit handling
// ─────────────────────────────────────────────────────────────────────────────

// MatchNumeric grades a numeric entry deterministically.
//
// Algorithm:
//  1. If entry.Unit is set, attempt to strip a trailing unit token from the
//     student answer before parsing. The stripped token is compared case-
//     insensitively to entry.Unit to determine unit correctness.
//  2. Parse the remaining string as a float64. Non-numeric answers → 0 marks.
//  3. Compare parsed value to entry.NumericAnswer within the tolerance:
//     - "abs" (default): |parsed - answer| <= tolerance
//     - "pct":           |parsed - answer| / |answer| * 100 <= tolerance
//  4. If entry.UnitMarks > 0:
//     - Value correct + unit correct → full MaxMarks
//     - Value correct + unit wrong/missing → MaxMarks - UnitMarks
//     - Value wrong → 0 (regardless of unit)
//
// Confidence rule:
//   - 1.0 for a successful numeric parse with a clear in/out determination.
//   - 1.0 for a non-numeric answer (confident zero).
//
// Returns (awarded, confidence, justification).
func MatchNumeric(entry GuideEntry, studentAnswer string) (float64, float64, string) {
	maxMarks := entry.MaxMarks
	tolType := entry.ToleranceType
	if tolType == "" {
		tolType = "abs"
	}

	// ── unit stripping / detection ──────────────────────────────────────────
	unitCorrect := false
	toParse := strings.TrimSpace(studentAnswer)

	if entry.Unit != "" {
		// Try to strip a trailing unit token. The token is the last whitespace-
		// separated field of the student answer; if it does not parse as a
		// number we treat it as the unit.
		fields := strings.Fields(toParse)
		if len(fields) >= 2 {
			lastField := fields[len(fields)-1]
			if _, err := strconv.ParseFloat(lastField, 64); err != nil {
				// Last field is non-numeric → treat it as the unit token.
				unitCorrect = strings.EqualFold(lastField, entry.Unit)
				toParse = strings.TrimSpace(strings.Join(fields[:len(fields)-1], " "))
			}
			// If the last field IS numeric, the whole answer is the number (no unit).
		} else if len(fields) == 1 {
			// Only one token: it might be the number with no unit, or a pure unit
			// string. Try to parse it; if it fails it's not a valid numeric answer.
			// unitCorrect stays false.
		}
	}

	// ── parse the numeric value ──────────────────────────────────────────────
	parsed, err := strconv.ParseFloat(strings.TrimSpace(toParse), 64)
	if err != nil {
		// Non-numeric student answer → confident 0.
		return 0, 1.0, "student answer is not a valid number"
	}

	// ── tolerance check ──────────────────────────────────────────────────────
	diff := math.Abs(parsed - entry.NumericAnswer)
	var inTolerance bool
	switch tolType {
	case "pct":
		if entry.NumericAnswer == 0 {
			inTolerance = diff == 0
		} else {
			pctDiff := diff / math.Abs(entry.NumericAnswer) * 100
			inTolerance = pctDiff <= entry.Tolerance
		}
	default: // "abs"
		inTolerance = diff <= entry.Tolerance
	}

	if !inTolerance {
		return 0, 1.0, fmt.Sprintf(
			"value %g is outside tolerance (expected %g ±%g %s)",
			parsed, entry.NumericAnswer, entry.Tolerance, tolType,
		)
	}

	// ── value is correct: apply unit_marks logic ────────────────────────────
	if entry.UnitMarks > 0 {
		if unitCorrect {
			return maxMarks, 1.0, fmt.Sprintf(
				"value %g correct and unit %q correct", parsed, entry.Unit,
			)
		}
		// Value correct but unit wrong/missing: award value marks only.
		valueMarks := maxMarks - entry.UnitMarks
		return valueMarks, 1.0, fmt.Sprintf(
			"value %g correct but unit wrong or missing (expected %q)", parsed, entry.Unit,
		)
	}

	// No unit_marks: full marks for a value within tolerance.
	return maxMarks, 1.0, fmt.Sprintf(
		"value %g is within tolerance of %g (±%g %s)", parsed, entry.NumericAnswer, entry.Tolerance, tolType,
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// MatchMultiStep — per-step method marks
// ─────────────────────────────────────────────────────────────────────────────

// MatchMultiStep grades a multi_step entry deterministically.
//
// Matching model: the student's full answer text is used as a single string.
// Each step's matcher is applied against the entire student answer. This works
// well for structured answers where each step's output appears as part of the
// overall written answer (e.g., "F=ma\n20 N"). Steps are independent: a correct
// step earns its marks even if other steps are wrong (method marks).
//
// Steps can have match types: "set", "exact", "exact_ci", or "numeric".
// The sum of awarded step marks is clamped to entry.MaxMarks.
//
// Returns (awarded, confidence, justification).
// Confidence is always 1.0 (deterministic).
func MatchMultiStep(entry GuideEntry, studentAnswer string) (float64, float64, string) {
	var total float64
	matched := []string{}
	missed := []string{}

	for i, step := range entry.Steps {
		stepLabel := fmt.Sprintf("step %d", i+1)
		var stepAwarded float64

		switch step.Match {
		case "numeric":
			// Build a synthetic GuideEntry for the numeric step matcher.
			numEntry := GuideEntry{
				MaxMarks:      step.Marks,
				NumericAnswer: step.NumericAnswer,
				Tolerance:     step.Tolerance,
				ToleranceType: step.ToleranceType,
			}
			// Try each line of the student answer; award marks on first match.
			lines := strings.Split(studentAnswer, "\n")
			for _, line := range lines {
				a, _, _ := MatchNumeric(numEntry, strings.TrimSpace(line))
				if a > 0 {
					stepAwarded = step.Marks
					break
				}
			}
			// Also try the whole answer as a fallback.
			if stepAwarded == 0 {
				a, _, _ := MatchNumeric(numEntry, strings.TrimSpace(studentAnswer))
				if a > 0 {
					stepAwarded = step.Marks
				}
			}

		default: // set / exact / exact_ci
			stepEntry := GuideEntry{
				Answer: step.Answer,
				Accept: step.Accept,
				Match:  step.Match,
			}
			// Try each line of the student answer as well as the whole answer.
			lines := strings.Split(studentAnswer, "\n")
			for _, line := range lines {
				if objectiveMatch(stepEntry, strings.TrimSpace(line), step.Match) {
					stepAwarded = step.Marks
					break
				}
			}
			// Also try the whole answer as one token.
			if stepAwarded == 0 && objectiveMatch(stepEntry, strings.TrimSpace(studentAnswer), step.Match) {
				stepAwarded = step.Marks
			}
		}

		if stepAwarded > 0 {
			total += stepAwarded
			matched = append(matched, stepLabel)
		} else {
			missed = append(missed, stepLabel)
		}
	}

	clamped := clamp(total, 0, entry.MaxMarks)

	var justification string
	if len(matched) == 0 {
		justification = "no steps matched"
	} else if len(missed) == 0 {
		justification = fmt.Sprintf("all %d steps matched", len(entry.Steps))
	} else {
		justification = fmt.Sprintf("matched: %s; missed: %s",
			strings.Join(matched, ", "), strings.Join(missed, ", "))
	}

	return clamped, 1.0, justification
}

// ─────────────────────────────────────────────────────────────────────────────
// MatchPartial — per-criterion partial marks
// ─────────────────────────────────────────────────────────────────────────────

// MatchPartial grades a partial entry deterministically.
//
// Each criterion in entry.Criteria is matched by checking whether any of its
// Accept strings appears as a case-insensitive substring of the student answer.
// Matched criteria contribute their Marks; the sum is clamped to entry.MaxMarks.
//
// Returns (awarded, confidence, justification).
// Confidence is always 1.0 (deterministic).
func MatchPartial(entry GuideEntry, studentAnswer string) (float64, float64, string) {
	lowerAnswer := strings.ToLower(studentAnswer)
	var total float64
	hitCount := 0

	for _, criterion := range entry.Criteria {
		for _, accept := range criterion.Accept {
			if strings.Contains(lowerAnswer, strings.ToLower(accept)) {
				total += criterion.Marks
				hitCount++
				break // count each criterion at most once
			}
		}
	}

	clamped := clamp(total, 0, entry.MaxMarks)

	justification := fmt.Sprintf(
		"%d/%d criteria matched; %g/%g marks awarded (before clamp)",
		hitCount, len(entry.Criteria), total, entry.MaxMarks,
	)

	return clamped, 1.0, justification
}
