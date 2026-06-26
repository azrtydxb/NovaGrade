package grade

import (
	"fmt"
	"strings"
)

// knownMatchTypes is the exhaustive list of match types accepted by the grader.
var knownMatchTypes = map[string]bool{
	"exact":      true,
	"exact_ci":   true,
	"set":        true,
	"rubric":     true,
	"numeric":    true,
	"multi_step": true,
	"partial":    true,
}

// ValidateGuide validates the content of a Guide.  It checks:
//   - Every entry has a known match type (exact/exact_ci/set/rubric/numeric/multi_step/partial).
//   - Required fields are present per match type:
//     exact / exact_ci → non-empty answer field
//     set              → at least one accept value
//     rubric           → non-empty rubric string
//     numeric          → numeric_answer must be explicitly present (non-nil pointer);
//                        zero is a valid expected answer but absent is rejected
//     multi_step       → at least one step; numeric steps must have numeric_answer present
//     partial          → at least one criterion with at least one accept value
//   - max_marks >= 0 for every entry.
//
// The function returns a single error that lists every offending question_no and
// the reason so callers can surface actionable feedback.
func ValidateGuide(g Guide) error {
	var errs []string
	for qno, e := range g {
		var entryErrs []string

		// max_marks must be non-negative.
		if e.MaxMarks < 0 {
			entryErrs = append(entryErrs, "max_marks must be >= 0")
		}

		// match type must be known.
		if !knownMatchTypes[e.Match] {
			entryErrs = append(entryErrs, fmt.Sprintf("unknown match type %q", e.Match))
		} else {
			// per-type required field checks (only when the match type is known)
			switch e.Match {
			case "exact", "exact_ci":
				if strings.TrimSpace(e.Answer) == "" {
					entryErrs = append(entryErrs, fmt.Sprintf("%s match requires a non-empty answer field", e.Match))
				}
			case "set":
				if len(e.Accept) == 0 {
					entryErrs = append(entryErrs, "set match requires at least one accept value")
				}
			case "rubric":
				if strings.TrimSpace(e.Rubric) == "" {
					entryErrs = append(entryErrs, "rubric match requires a non-empty rubric string")
				}
			case "numeric":
				// numeric_answer is *float64; nil means the field was absent from the JSON.
				// Zero is a valid expected answer; absent is rejected so callers must be explicit.
				if e.NumericAnswer == nil {
					entryErrs = append(entryErrs, "numeric match requires numeric_answer to be present (use 0 for an expected answer of zero)")
				}
			case "multi_step":
				if len(e.Steps) == 0 {
					entryErrs = append(entryErrs, "multi_step match requires at least one step")
				} else {
					for i, s := range e.Steps {
						// Reject empty/missing step match type.
						if s.Match == "" {
							entryErrs = append(entryErrs, fmt.Sprintf("step[%d] has empty/missing match type (question %s)", i, qno))
							continue
						}
						if !knownMatchTypes[s.Match] {
							entryErrs = append(entryErrs, fmt.Sprintf("step[%d] has unknown match type %q", i, s.Match))
							continue
						}
						// Per-type required field checks for each step.
						switch s.Match {
						case "exact", "exact_ci":
							if strings.TrimSpace(s.Answer) == "" {
								entryErrs = append(entryErrs, fmt.Sprintf("step[%d] %s match requires a non-empty answer field (question %s)", i, s.Match, qno))
							}
						case "set":
							if len(s.Accept) == 0 {
								entryErrs = append(entryErrs, fmt.Sprintf("step[%d] set match requires at least one accept value (question %s)", i, qno))
							}
						case "numeric":
							if s.NumericAnswer == nil {
								entryErrs = append(entryErrs, fmt.Sprintf("step[%d] numeric match requires numeric_answer to be present", i))
							}
						}
					}
				}
			case "partial":
				if len(e.Criteria) == 0 {
					entryErrs = append(entryErrs, "partial match requires at least one criterion")
				} else {
					for i, c := range e.Criteria {
						if len(c.Accept) == 0 {
							entryErrs = append(entryErrs, fmt.Sprintf("criterion[%d] has no accept values", i))
						}
					}
				}
			}
		}

		if len(entryErrs) > 0 {
			errs = append(errs, fmt.Sprintf("%s: %s", qno, strings.Join(entryErrs, "; ")))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("guide validation failed:\n  %s", strings.Join(errs, "\n  "))
}

// ValidateGuideJSON is a convenience wrapper that parses raw JSON bytes and
// then calls ValidateGuide. It returns an error if the bytes are not valid JSON
// or if the guide fails validation.
func ValidateGuideJSON(data []byte) error {
	g, err := LoadGuideFromJSON(data)
	if err != nil {
		return fmt.Errorf("invalid guide JSON: %w", err)
	}
	return ValidateGuide(g)
}
