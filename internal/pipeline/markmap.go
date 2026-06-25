package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// MarkMap is the official marks distribution printed on an exam's front /
// instructions page: a paper total plus per-section budgets. It mirrors the
// {total, sections} dict produced by examgrader/markmap.py.
//
// Total is optional (nil == "no printed total"); Sections is keyed by the
// canonical section label (e.g. "A", "B").
type MarkMap struct {
	Total    *float64
	Sections map[string]float64
}

// markMapFromTextPrompt asks the reasoning model to read the official marks
// distribution off an already-transcribed front/instructions page. Mirrors
// MARKMAP_FROM_TEXT_PROMPT in the POC.
const markMapFromTextPrompt = "Below is the transcription of an exam's front/instructions page. " +
	"Extract the OFFICIAL marks distribution as JSON: " +
	`{"total": <total marks number or null>, "sections": {"A": <marks>, ...}}. ` +
	"Use section labels exactly as printed; if there are no labelled sections use {}. " +
	"Do not invent values.\n\nTEXT:\n"

// rawMarkMap is the wire shape the model returns for a mark map.
type rawMarkMap struct {
	Total    *float64           `json:"total"`
	Sections map[string]float64 `json:"sections"`
}

// normalizeMarkMap canonicalises a raw model mark map: it keeps a numeric total
// (or nil) and re-keys section budgets by their canonical label, dropping
// blanks. Mirrors _normalize_mark_map + map_sections from the POC.
func normalizeMarkMap(raw rawMarkMap) MarkMap {
	out := MarkMap{Sections: map[string]float64{}}
	out.Total = raw.Total
	for k, v := range raw.Sections {
		if ck := canonicalSection(&k); ck != nil {
			out.Sections[*ck] = v
		}
	}
	return out
}

// extractMarkMapFromText parses {total, sections} out of an already-transcribed
// instructions page. A missing or malformed mark map is non-fatal: it just
// disables reconciliation, so a zero-value MarkMap (no total, no sections) is
// returned in that case. Mirrors extract_mark_map_from_text in the POC.
func extractMarkMapFromText(pageText string) (MarkMap, bool) {
	raw, ok := extractJSON(pageText)
	if !ok {
		return MarkMap{Sections: map[string]float64{}}, false
	}
	var rmm rawMarkMap
	if err := json.Unmarshal(raw, &rmm); err != nil {
		return MarkMap{Sections: map[string]float64{}}, false
	}
	return normalizeMarkMap(rmm), true
}

// expectedTotal turns a MarkMap into the *float64 stored on TranscribedPaper.
// Precedence mirrors the POC's reconciliation source: a printed paper total
// wins; otherwise, if section budgets exist, their sum is used; otherwise nil.
func (m MarkMap) expectedTotal() *float64 {
	if m.Total != nil {
		return m.Total
	}
	if len(m.Sections) > 0 {
		var sum float64
		for _, v := range m.Sections {
			sum += v
		}
		return &sum
	}
	return nil
}

var sectionStripRe = regexp.MustCompile(`(?i)section`)

// canonicalSection normalises a transcribed section label to a budget key:
// "Section A" / "A" -> "A". Returns nil for empty / blank labels. Mirrors
// canonical_section in the POC.
func canonicalSection(section *string) *string {
	if section == nil {
		return nil
	}
	s := strings.TrimSpace(sectionStripRe.ReplaceAllString(*section, ""))
	if s == "" {
		return nil
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	out := strings.ToUpper(fields[0])
	return &out
}

var sectionHeaderRe = regexp.MustCompile(`(?i)section\s+([A-D])\b`)

// detectSection finds a "Section X" header in a page's printed text. Sections
// span pages and the header is not repeated, so callers carry the last header
// forward onto questions that lack one. Mirrors _detect_section in the POC.
func detectSection(printedText string) *string {
	m := sectionHeaderRe.FindStringSubmatch(printedText)
	if m == nil {
		return nil
	}
	s := strings.ToUpper(m[1])
	return &s
}

// markBudgetHint turns a MarkMap into a prompt hint that constrains mark
// allocation during structuring. Mirrors mark_budget_hint in the POC; returns
// "" for an empty mark map.
func markBudgetHint(m MarkMap) string {
	var parts []string
	if m.Total != nil {
		parts = append(parts, fmt.Sprintf("the whole paper is worth %g marks", *m.Total))
	}
	if len(m.Sections) > 0 {
		keys := make([]string, 0, len(m.Sections))
		for k := range m.Sections {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var secs []string
		for _, k := range keys {
			secs = append(secs, fmt.Sprintf("Section %s = %g", k, m.Sections[k]))
		}
		parts = append(parts, "section budgets are: "+strings.Join(secs, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return " IMPORTANT mark budget for the whole exam: " + strings.Join(parts, "; ") + ". " +
		"Assign max_marks so that, across the entire paper, each section's questions sum to " +
		"that section's budget and the paper sums to the total — if questions share a " +
		"'(N marks)' label, split N across them rather than repeating it."
}

// validSections returns the set of canonical section labels the paper states a
// budget for, used to decide whether a transcribed question's section is valid
// or should be replaced by the carried-forward header. Mirrors map_sections.
func (m MarkMap) validSections() map[string]struct{} {
	out := make(map[string]struct{}, len(m.Sections))
	for k := range m.Sections {
		out[k] = struct{}{}
	}
	return out
}

// carrySectionsForward stamps the last-seen section header onto questions that
// lack a valid section. Mirrors _carry_sections_forward in the POC.
func carrySectionsForward(perPage []pageResult, mm MarkMap) []contracts.TranscribedQuestion {
	valid := mm.validSections()
	var current *string
	var out []contracts.TranscribedQuestion
	for _, pr := range perPage {
		if pr.section != nil {
			current = pr.section
		}
		for _, q := range pr.questions {
			cs := canonicalSection(q.Section)
			invalid := cs == nil
			if !invalid && len(valid) > 0 {
				if _, ok := valid[*cs]; !ok {
					invalid = true
				}
			}
			if current != nil && invalid {
				sec := *current
				q.Section = &sec
			}
			out = append(out, q)
		}
	}
	return out
}
