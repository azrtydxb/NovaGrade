package pipeline_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/azrtydxb/novagrade/internal/pipeline"
	"github.com/azrtydxb/novagrade/internal/providers"
)

// fakeProvider is a scripted AIProvider. It routes each Complete call to a
// response based on the model name (which distinguishes the three roles: OCR,
// reasoning/structuring, and answer-reading VLM) and an optional matcher over
// the request so the same model can return different things per page.
type fakeProvider struct {
	// responses is consulted in order; the first entry whose match returns true
	// produces the response. This lets the test script page-by-page behaviour.
	responses []scriptedResponse
	calls     []providers.CompletionReq
}

type scriptedResponse struct {
	match   func(providers.CompletionReq) bool
	content string
	err     error
}

func (f *fakeProvider) Complete(_ context.Context, req providers.CompletionReq) (providers.CompletionResp, error) {
	f.calls = append(f.calls, req)
	for _, r := range f.responses {
		if r.match(req) {
			if r.err != nil {
				return providers.CompletionResp{}, r.err
			}
			return providers.CompletionResp{Content: r.content, SchemaValid: true}, nil
		}
	}
	return providers.CompletionResp{}, fmt.Errorf("fakeProvider: no scripted response for model %q", req.Model)
}

// lastUserText returns the concatenated text of the request's user messages,
// used by matchers to disambiguate which page/prompt is being answered.
func lastUserText(req providers.CompletionReq) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func isOCR(req providers.CompletionReq) bool {
	return req.Model == pipeline.DefaultTranscribeModels.OCR
}

func isReason(req providers.CompletionReq) bool {
	return req.Model == pipeline.DefaultTranscribeModels.Reason
}

func isVLM(req providers.CompletionReq) bool {
	return req.Model == pipeline.DefaultTranscribeModels.VLM
}

// TestTranscribe drives a two-page paper through the hybrid pipeline:
//   - page 1 is the front/instructions page (mark map: total 30, Section A=10, B=20)
//   - page 2 carries questions 1a and 1b plus the student's answers.
func TestTranscribe(t *testing.T) {
	prov := &fakeProvider{
		responses: []scriptedResponse{
			// --- mark map extraction (reason model, front page text) ---
			{
				match: func(r providers.CompletionReq) bool {
					return isReason(r) && strings.Contains(lastUserText(r), "marks distribution")
				},
				content: `{"total": 30, "sections": {"A": 10, "B": 20}}`,
			},
			// --- OCR page 1 (front page) ---
			{
				match: func(r providers.CompletionReq) bool {
					return isOCR(r) && containsImageIndex(r, 0)
				},
				content: "EXAM COVER PAGE\nSection A (10 marks) Section B (20 marks)\nTotal: 30 marks",
			},
			// --- OCR page 2 (questions) ---
			{
				match: func(r providers.CompletionReq) bool {
					return isOCR(r) && containsImageIndex(r, 1)
				},
				content: "Section A\n1a. Capital of France? (5 marks) Paris\n1b. 2+2? (5 marks) 4",
			},
			// --- structuring page 1 (front page → no real questions) ---
			{
				match: func(r providers.CompletionReq) bool {
					return isReason(r) && strings.Contains(lastUserText(r), "COVER PAGE")
				},
				content: `[]`,
			},
			// --- structuring page 2 (questions + marks, no answers) ---
			{
				match: func(r providers.CompletionReq) bool {
					return isReason(r) && strings.Contains(lastUserText(r), "Capital of France")
				},
				content: `[
					{"section":"A","question_no":"1a","max_marks":5,"question_text":"Capital of France?"},
					{"section":"A","question_no":"1b","max_marks":5,"question_text":"2+2?"}
				]`,
			},
			// --- answer reading page 2 (VLM) ---
			{
				match: func(r providers.CompletionReq) bool {
					return isVLM(r) && containsImageIndex(r, 1)
				},
				content: `[
					{"question_no":"1a","student_answer":"Paris"},
					{"question_no":"1b","student_answer":"4"}
				]`,
			},
		},
	}

	pages := [][]byte{pageBytes(0), pageBytes(1)}
	paper, err := pipeline.Transcribe(context.Background(), prov, pages, "Geography")
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}

	if paper.Subject != "Geography" {
		t.Errorf("subject: got %q want %q", paper.Subject, "Geography")
	}
	if len(paper.Questions) != 2 {
		t.Fatalf("expected 2 questions, got %d: %+v", len(paper.Questions), paper.Questions)
	}

	q1 := paper.Questions[0]
	if q1.QuestionNo != "1a" || q1.MaxMarks != 5 || q1.StudentAnswer != "Paris" {
		t.Errorf("q1 mismatch: %+v", q1)
	}
	if q1.ReadConfidence <= 0 {
		t.Errorf("q1 read_confidence should be set, got %v", q1.ReadConfidence)
	}
	if q1.Section == nil || *q1.Section != "A" {
		t.Errorf("q1 section: got %v want A", q1.Section)
	}

	q2 := paper.Questions[1]
	if q2.QuestionNo != "1b" || q2.MaxMarks != 5 || q2.StudentAnswer != "4" {
		t.Errorf("q2 mismatch: %+v", q2)
	}

	if paper.ExpectedTotal == nil || *paper.ExpectedTotal != 30 {
		t.Errorf("expected_total: got %v want 30", paper.ExpectedTotal)
	}
}

// TestTranscribeIsolation verifies per-item isolation: a malformed structuring
// response on one page is skipped while the rest of the paper still transcribes.
func TestTranscribeIsolation(t *testing.T) {
	prov := &fakeProvider{
		responses: []scriptedResponse{
			// no mark map (reason model asked for distribution returns junk → ignored)
			{
				match: func(r providers.CompletionReq) bool {
					return isReason(r) && strings.Contains(lastUserText(r), "marks distribution")
				},
				content: `not json at all`,
			},
			// OCR both pages
			{
				match:   func(r providers.CompletionReq) bool { return isOCR(r) && containsImageIndex(r, 0) },
				content: "Section A\n1a. Good question (5 marks) answer",
			},
			{
				match:   func(r providers.CompletionReq) bool { return isOCR(r) && containsImageIndex(r, 1) },
				content: "Section A\n2a. Bad page",
			},
			// structuring page 1 → valid
			{
				match: func(r providers.CompletionReq) bool {
					return isReason(r) && strings.Contains(lastUserText(r), "Good question")
				},
				content: `[{"section":"A","question_no":"1a","max_marks":5,"question_text":"Good question"}]`,
			},
			// structuring page 2 → malformed JSON: must be skipped, not fatal
			{
				match: func(r providers.CompletionReq) bool {
					return isReason(r) && strings.Contains(lastUserText(r), "Bad page")
				},
				content: `{ this is broken`,
			},
			// answers for page 1
			{
				match:   func(r providers.CompletionReq) bool { return isVLM(r) && containsImageIndex(r, 0) },
				content: `[{"question_no":"1a","student_answer":"answer"}]`,
			},
		},
	}

	pages := [][]byte{pageBytes(0), pageBytes(1)}
	paper, err := pipeline.Transcribe(context.Background(), prov, pages, "Math")
	if err != nil {
		t.Fatalf("Transcribe should not fail when one page is malformed: %v", err)
	}
	if len(paper.Questions) != 1 {
		t.Fatalf("expected 1 question (bad page skipped), got %d: %+v", len(paper.Questions), paper.Questions)
	}
	if paper.Questions[0].QuestionNo != "1a" {
		t.Errorf("surviving question mismatch: %+v", paper.Questions[0])
	}
}

// pageBytes builds a unique fake PNG byte slice for the page at index i. The
// last byte encodes the index so matchers can identify which page's image is in
// a given request.
func pageBytes(i int) []byte {
	return []byte{0x89, 'P', 'N', 'G', byte(i)}
}

// containsImageIndex reports whether the request carries the fake page image for
// the given index (see pageBytes).
func containsImageIndex(req providers.CompletionReq, i int) bool {
	want := pageBytes(i)
	for _, img := range req.Images {
		if len(img) == len(want) && img[len(img)-1] == byte(i) {
			return true
		}
	}
	return false
}
