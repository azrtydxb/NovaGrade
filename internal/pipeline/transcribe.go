package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// TranscribeModels names the three model roles the hybrid pipeline drives,
// each dispatched through providers.AIProvider.Complete via its Model field:
//
//   - OCR: a faithful full-page OCR model (dots.ocr in the POC) that reads
//     printed questions, marks labels, and handwriting verbatim.
//   - Reason: a text model that structures one page's OCR text into question
//     records (and extracts the official mark map off the front page).
//   - VLM: a vision model (qwen3-vl in the POC) that reads the student's
//     answers for the exact question numbers the Reason model produced.
type TranscribeModels struct {
	OCR    string
	Reason string
	VLM    string
}

// DefaultTranscribeModels is the model set used by Transcribe. The names match
// the deployed model identifiers behind the ai-gateway / vLLM provider.
var DefaultTranscribeModels = TranscribeModels{
	OCR:    "dots.ocr",
	Reason: "qwen3",
	VLM:    "qwen3-vl",
}

const (
	maxTokensOCR       = 4000
	maxTokensStructure = 2500
	maxTokensAnswers   = 2000
	maxTokensMarkMap   = 400

	// defaultReadConfidence mirrors the POC, which sets read_confidence to 1.0
	// when the model does not supply one.
	defaultReadConfidence = 1.0
)

// Prompts mirror examgrader/dots_transcriber.py and markmap.py verbatim so the
// Go port reproduces the POC's behaviour.
const (
	ocrPrompt = "Transcribe this scanned exam page exactly as printed. Preserve every question number, " +
		"every printed marks label in parentheses such as (5 marks) or (1 mark), and transcribe " +
		"the student's HANDWRITTEN answers in place next to each question. Output faithful " +
		"text/markdown — do not summarize, do not invent, do not skip the marks labels."

	questionsPrompt = "Below is a faithful transcription of ONE exam page. Extract the real, answerable " +
		"questions as a JSON array; each element: " +
		`"section" (the section letter/number or null), "question_no" (e.g. "1a"), ` +
		`"max_marks" (from the printed "(N marks)"; if a single label covers sub-parts a, b, c ` +
		"divide it evenly so they sum to N; 0 if none shown), " +
		`"question_text" (the printed question, concise). ` +
		"Do NOT include the student's answers. IGNORE instructions, section-overview lines, the " +
		"cover/registration page and exam metadata. Return ONLY the JSON array.\n\n" +
		"PAGE TRANSCRIPTION:\n"

	answersForPrompt = "Look at this scanned exam page. You are given the list of questions on it. For EACH " +
		"question, report the student's answer EXACTLY: the option they circled or ticked (e.g. " +
		`"B", or the circled word), or what they handwrote; use an empty string if blank. ` +
		"Use the SAME question_no values you are given. " +
		`Return ONLY a JSON array: [{"question_no": "1a", "student_answer": "..."}].` + "\n\n" +
		"QUESTIONS:\n"
)

// pageResult holds the per-page output of the hybrid pipeline: the section
// header detected on the page (if any) and its structured, answered questions.
type pageResult struct {
	section   *string
	questions []contracts.TranscribedQuestion
}

// rawQuestion is the wire shape the Reason model returns for structuring.
type rawQuestion struct {
	Section      *string `json:"section"`
	QuestionNo   string  `json:"question_no"`
	MaxMarks     float64 `json:"max_marks"`
	QuestionText string  `json:"question_text"`
}

// rawAnswer is the wire shape the VLM returns for answer-reading.
type rawAnswer struct {
	QuestionNo    string `json:"question_no"`
	StudentAnswer string `json:"student_answer"`
}

// Transcribe runs the hybrid OCR + reason + VLM pipeline over the page PNGs of a
// single paper and returns a TranscribedPaper. It uses DefaultTranscribeModels.
//
// Per-item isolation is the central guarantee: a failure on any individual page
// (OCR, structuring, or answer-reading) or any individual malformed question is
// logged and skipped — it never fails the whole paper. The returned error is
// non-nil only for a programming-level fault (currently none), so callers can
// treat a returned paper as best-effort-complete.
func Transcribe(ctx context.Context, prov providers.AIProvider, pages [][]byte, subject string) (contracts.TranscribedPaper, error) {
	return TranscribeWithModels(ctx, prov, DefaultTranscribeModels, pages, subject)
}

// TranscribeWithModels is Transcribe with an explicit model set, exposed for
// testing and for callers that route roles to non-default model identifiers.
func TranscribeWithModels(ctx context.Context, prov providers.AIProvider, models TranscribeModels, pages [][]byte, subject string) (contracts.TranscribedPaper, error) {
	// 1. OCR every page up front. The first page's text doubles as the source
	//    for the mark map. Per-page OCR failures yield "" and are skipped later.
	ocrTexts := make([]string, len(pages))
	for i := range pages {
		text, err := ocrPage(ctx, prov, models, pages[i])
		if err != nil {
			log.Printf("[transcribe] OCR skipped page %d: %v", i+1, err)
			ocrTexts[i] = ""
			continue
		}
		ocrTexts[i] = text
	}

	// 2. Extract the official mark map off the front page's OCR text. A missing
	//    or malformed mark map is non-fatal — it just disables reconciliation.
	var markMap MarkMap
	if len(pages) > 0 && ocrTexts[0] != "" {
		markMap = extractMarkMap(ctx, prov, models, ocrTexts[0])
	} else {
		markMap = MarkMap{Sections: map[string]float64{}}
	}

	// 3. Structure + answer each page. Sections are carried forward afterwards.
	perPage := make([]pageResult, len(pages))
	for i := range pages {
		perPage[i] = onePageHybrid(ctx, prov, models, ocrTexts[i], pages[i], markMap, i)
	}

	questions := dedupeQuestionNos(carrySectionsForward(perPage, markMap))

	paper := contracts.TranscribedPaper{
		Subject:       subject,
		Questions:     questions,
		ExpectedTotal: markMap.expectedTotal(),
	}
	return paper, nil
}

// onePageHybrid structures one page's OCR text into questions+marks (Reason),
// then asks the VLM for the answers to those exact question numbers and attaches
// them. Each sub-step is isolated: a failure keeps whatever was produced so far.
func onePageHybrid(ctx context.Context, prov providers.AIProvider, models TranscribeModels, printed string, img []byte, markMap MarkMap, idx int) pageResult {
	if printed == "" {
		return pageResult{}
	}
	section := detectSection(printed)

	raws, err := structureQuestions(ctx, prov, models, printed, markMap)
	if err != nil {
		log.Printf("[transcribe] structuring skipped page %d: %v", idx+1, err)
		return pageResult{section: section}
	}
	if len(raws) == 0 {
		return pageResult{section: section}
	}

	answers, err := readAnswersFor(ctx, prov, models, img, raws)
	if err != nil {
		// Answers are best-effort; keep the questions with empty answers.
		log.Printf("[transcribe] answers skipped page %d: %v", idx+1, err)
		answers = map[string]string{}
	}

	out := make([]contracts.TranscribedQuestion, 0, len(raws))
	for _, r := range raws {
		if r.QuestionNo == "" {
			// One bad question must not drop the page.
			log.Printf("[transcribe] question skipped on page %d: empty question_no", idx+1)
			continue
		}
		sec := r.Section
		out = append(out, contracts.TranscribedQuestion{
			Section:        sec,
			QuestionNo:     r.QuestionNo,
			MaxMarks:       r.MaxMarks,
			QuestionText:   r.QuestionText,
			StudentAnswer:  answers[r.QuestionNo],
			ReadConfidence: defaultReadConfidence,
		})
	}
	return pageResult{section: section, questions: out}
}

// ocrPage produces a faithful full-page transcription via the OCR model.
func ocrPage(ctx context.Context, prov providers.AIProvider, models TranscribeModels, img []byte) (string, error) {
	resp, err := prov.Complete(ctx, providers.CompletionReq{
		Model:     models.OCR,
		Messages:  []providers.Message{{Role: "user", Content: ocrPrompt}},
		Images:    [][]byte{img},
		MaxTokens: maxTokensOCR,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// structureQuestions extracts questions + marks (no answers) from a faithful
// page transcription using the Reason model.
func structureQuestions(ctx context.Context, prov providers.AIProvider, models TranscribeModels, printed string, markMap MarkMap) ([]rawQuestion, error) {
	prompt := questionsPrompt + printed + markBudgetHint(markMap)
	resp, err := prov.Complete(ctx, providers.CompletionReq{
		Model:     models.Reason,
		Messages:  []providers.Message{{Role: "user", Content: prompt}},
		MaxTokens: maxTokensStructure,
	})
	if err != nil {
		return nil, err
	}
	return parseQuestions(resp.Content)
}

// readAnswersFor asks the VLM for the answers to the GIVEN questions and returns
// a {question_no: answer} map. Anchoring the reader to the structured question
// numbers keeps answers aligned with questions.
func readAnswersFor(ctx context.Context, prov providers.AIProvider, models TranscribeModels, img []byte, questions []rawQuestion) (map[string]string, error) {
	type qref struct {
		QuestionNo   string `json:"question_no"`
		QuestionText string `json:"question_text"`
	}
	refs := make([]qref, 0, len(questions))
	for _, q := range questions {
		refs = append(refs, qref{QuestionNo: q.QuestionNo, QuestionText: q.QuestionText})
	}
	qjson, err := json.Marshal(refs)
	if err != nil {
		return nil, fmt.Errorf("marshal question refs: %w", err)
	}
	resp, err := prov.Complete(ctx, providers.CompletionReq{
		Model:     models.VLM,
		Messages:  []providers.Message{{Role: "user", Content: answersForPrompt + string(qjson)}},
		Images:    [][]byte{img},
		MaxTokens: maxTokensAnswers,
	})
	if err != nil {
		return nil, err
	}
	answers, err := parseAnswers(resp.Content)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(answers))
	for _, a := range answers {
		out[a.QuestionNo] = a.StudentAnswer
	}
	return out, nil
}

// extractMarkMap reads the official {total, sections} off the front-page OCR
// text using the Reason model. A failure returns an empty mark map.
func extractMarkMap(ctx context.Context, prov providers.AIProvider, models TranscribeModels, frontText string) MarkMap {
	empty := MarkMap{Sections: map[string]float64{}}
	resp, err := prov.Complete(ctx, providers.CompletionReq{
		Model:     models.Reason,
		Messages:  []providers.Message{{Role: "user", Content: markMapFromTextPrompt + frontText}},
		MaxTokens: maxTokensMarkMap,
	})
	if err != nil {
		log.Printf("[transcribe] mark map extraction failed: %v", err)
		return empty
	}
	mm, _ := extractMarkMapFromText(resp.Content)
	return mm
}

// parseQuestions decodes the structuring response: either a bare JSON array or
// a {"questions": [...]} object. Malformed JSON is an error (caller isolates).
func parseQuestions(content string) ([]rawQuestion, error) {
	raw, ok := extractJSON(content)
	if !ok {
		return nil, fmt.Errorf("no JSON found in structuring response")
	}
	var arr []rawQuestion
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrapper struct {
		Questions []rawQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("unmarshal questions: %w", err)
	}
	return wrapper.Questions, nil
}

// parseAnswers decodes the answer-reading response: either a bare JSON array or
// a {"answers": [...]} object.
func parseAnswers(content string) ([]rawAnswer, error) {
	raw, ok := extractJSON(content)
	if !ok {
		return nil, fmt.Errorf("no JSON found in answers response")
	}
	var arr []rawAnswer
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrapper struct {
		Answers []rawAnswer `json:"answers"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("unmarshal answers: %w", err)
	}
	return wrapper.Answers, nil
}

// dedupeQuestionNos makes question_no values unique so none is silently lost
// downstream. Repeats get a "#N" suffix. Mirrors _dedupe_question_nos.
func dedupeQuestionNos(questions []contracts.TranscribedQuestion) []contracts.TranscribedQuestion {
	seen := map[string]int{}
	for i := range questions {
		n := questions[i].QuestionNo
		if c, ok := seen[n]; ok {
			seen[n] = c + 1
			questions[i].QuestionNo = fmt.Sprintf("%s#%d", n, c+1)
		} else {
			seen[n] = 1
		}
	}
	return questions
}

// extractJSON pulls a JSON value (array or object) out of a model response,
// stripping ``` fences and scanning for the earliest balanced array/object.
// It mirrors the provider package's extractJSON so the pipeline does not depend
// on provider internals.
func extractJSON(s string) (json.RawMessage, bool) {
	stripped := strings.TrimSpace(s)

	if after, found := strings.CutPrefix(stripped, "```"); found {
		// Find the FIRST closing fence after the opening one so we do not
		// accidentally truncate JSON that contains a literal ``` later in
		// the text (e.g. a second fenced block with an explanation).
		remainder := after
		if after2, found2 := strings.CutPrefix(remainder, "json"); found2 {
			remainder = after2
		}
		remainder = strings.TrimSpace(remainder)
		if idx := strings.Index(remainder, "```"); idx >= 0 {
			remainder = remainder[:idx]
		}
		stripped = strings.TrimSpace(remainder)
	}

	if json.Valid([]byte(stripped)) {
		return json.RawMessage(stripped), true
	}

	braceIdx := strings.IndexByte(stripped, '{')
	bracketIdx := strings.IndexByte(stripped, '[')

	var start int
	var open, closeCh byte
	switch {
	case braceIdx < 0 && bracketIdx < 0:
		return nil, false
	case braceIdx < 0:
		start, open, closeCh = bracketIdx, '[', ']'
	case bracketIdx < 0:
		start, open, closeCh = braceIdx, '{', '}'
	case bracketIdx < braceIdx:
		start, open, closeCh = bracketIdx, '[', ']'
	default:
		start, open, closeCh = braceIdx, '{', '}'
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(stripped); i++ {
		c := stripped[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				candidate := stripped[start : i+1]
				if json.Valid([]byte(candidate)) {
					return json.RawMessage(candidate), true
				}
				return nil, false
			}
		}
	}
	return nil, false
}
