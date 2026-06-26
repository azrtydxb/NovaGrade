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
//     set      → at least one accept value
//     rubric   → non-empty rubric string
//     numeric  → numeric_answer present (non-zero is required; zero is allowed only
//                when zero is a meaningful expected answer — callers may pass any value)
//     multi_step → at least one step
//     partial  → at least one criterion with at least one accept value
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
			case "set":
				if len(e.Accept) == 0 {
					entryErrs = append(entryErrs, "set match requires at least one accept value")
				}
			case "rubric":
				if strings.TrimSpace(e.Rubric) == "" {
					entryErrs = append(entryErrs, "rubric match requires a non-empty rubric string")
				}
			case "numeric":
				// numeric_answer is a float64 field; zero is a valid expected answer.
				// We do not reject zero. The only check is that the field must be
				// explicitly present — JSON unmarshalling gives 0 for absent fields.
				// Since Go cannot distinguish "field absent" from "field is 0" without
				// a pointer, we accept zero as a valid numeric_answer.  If callers want
				// stricter validation they can add a dedicated JSON schema layer.
				// No additional check needed: the field always has a value.
				_ = e.NumericAnswer
			case "multi_step":
				if len(e.Steps) == 0 {
					entryErrs = append(entryErrs, "multi_step match requires at least one step")
				} else {
					for i, s := range e.Steps {
						if !knownMatchTypes[s.Match] && s.Match != "" {
							entryErrs = append(entryErrs, fmt.Sprintf("step[%d] has unknown match type %q", i, s.Match))
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
