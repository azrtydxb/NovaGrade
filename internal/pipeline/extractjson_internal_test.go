package pipeline

import (
	"testing"
)

// TestTranscribeExtractJSON_firstFence verifies that extractJSON extracts only
// the FIRST fenced JSON block and ignores any trailing fenced blocks (e.g. an
// explanation fence that follows the JSON fence).
func TestTranscribeExtractJSON_firstFence(t *testing.T) {
	// Response has a JSON fence first, then an explanation fence after.
	// The old LastIndex-based code would find the closing ``` of the second
	// fence and thus include the explanation as part of the JSON string,
	// causing json.Unmarshal to fail or return wrong data.
	// The new first-fence code must extract only the JSON object.
	input := "```json\n{\"key\": \"value\"}\n```\n\nHere is an explanation:\n\n```\nsome text\n```"

	raw, ok := extractJSON(input)
	if !ok {
		t.Fatalf("extractJSON returned ok=false; want the JSON object to be extracted")
	}
	// The extracted bytes must be valid JSON and must be the inner object only.
	const wantKey = `"key"`
	const wantVal = `"value"`
	got := string(raw)
	if got != `{"key": "value"}` {
		t.Errorf("extractJSON first-fence: got %q, want %q", got, `{"key": "value"}`)
	}
	_ = wantKey
	_ = wantVal
}
