package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// VLLMProvider is an AIProvider backed by an OpenAI-compatible (vLLM) endpoint.
type VLLMProvider struct {
	cfg    VLLMConfig
	client *http.Client
}

// NewVLLMProvider constructs a VLLMProvider from cfg.
func NewVLLMProvider(cfg VLLMConfig) *VLLMProvider {
	if cfg.MaxRetries < 1 {
		cfg.MaxRetries = 1
	}
	return &VLLMProvider{
		cfg:    cfg,
		client: &http.Client{},
	}
}

// openAI request/response shapes.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []interface{} `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// textContentPart and imageContentPart model OpenAI multi-part message content.
type textContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageURL struct {
	URL string `json:"url"`
}

type imageContentPart struct {
	Type     string   `json:"type"`
	ImageURL imageURL `json:"image_url"`
}

type simpleMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type multipartMessage struct {
	Role    string        `json:"role"`
	Content []interface{} `json:"content"`
}

// Complete implements AIProvider.
func (p *VLLMProvider) Complete(ctx context.Context, req CompletionReq) (CompletionResp, error) {
	resp, err := p.complete(ctx, req)

	// Always emit a log, even on error.
	if p.cfg.LogSink != nil {
		p.cfg.LogSink(AICallLog{
			Model:         req.Model,
			PromptVersion: req.PromptVersion,
			Tokens:        resp.Tokens,
			CostUSD:       resp.CostUSD,
			SchemaValid:   resp.SchemaValid,
		})
	}
	return resp, err
}

func (p *VLLMProvider) complete(ctx context.Context, req CompletionReq) (CompletionResp, error) {
	messages := buildMessages(req.Messages, req.Images)

	apiResp, err := p.call(ctx, req, messages)
	if err != nil {
		return CompletionResp{}, err
	}

	content := ""
	if len(apiResp.Choices) > 0 {
		content = apiResp.Choices[0].Message.Content
	}

	usage := TokenUsage{
		Prompt:     apiResp.Usage.PromptTokens,
		Completion: apiResp.Usage.CompletionTokens,
		Total:      apiResp.Usage.TotalTokens,
	}
	cost := p.cost(req.Model, usage)

	out := CompletionResp{
		Content:     content,
		Tokens:      usage,
		CostUSD:     cost,
		SchemaValid: true,
	}

	// No schema: nothing to validate.
	if len(req.Schema) == 0 {
		return out, nil
	}

	sch, err := compileSchema(req.Schema)
	if err != nil {
		out.SchemaValid = false
		return out, fmt.Errorf("compile schema: %w", err)
	}

	if validateJSON(sch, content) == nil {
		return out, nil
	}

	// Re-ask once.
	reask := append([]interface{}{}, messages...)
	reask = append(reask,
		simpleMessage{Role: "assistant", Content: content},
		simpleMessage{Role: "user", Content: "Your reply was not valid JSON matching the required schema. Reply ONLY with valid JSON."},
	)

	apiResp2, err := p.call(ctx, req, reask)
	if err != nil {
		out.SchemaValid = false
		return out, err
	}

	content2 := ""
	if len(apiResp2.Choices) > 0 {
		content2 = apiResp2.Choices[0].Message.Content
	}

	// Accumulate token usage from the re-ask.
	usage.Prompt += apiResp2.Usage.PromptTokens
	usage.Completion += apiResp2.Usage.CompletionTokens
	usage.Total += apiResp2.Usage.TotalTokens
	out.Tokens = usage
	out.CostUSD = p.cost(req.Model, usage)
	out.Content = content2

	if err := validateJSON(sch, content2); err != nil {
		out.SchemaValid = false
		return out, fmt.Errorf("schema validation failed after re-ask: %w", err)
	}

	out.SchemaValid = true
	return out, nil
}

// buildMessages converts request messages into OpenAI-compatible message
// objects, transforming the last user message into multi-part content when
// images are present.
func buildMessages(msgs []Message, images [][]byte) []interface{} {
	out := make([]interface{}, 0, len(msgs))

	lastUser := -1
	if len(images) > 0 {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "user" {
				lastUser = i
				break
			}
		}
	}

	for i, m := range msgs {
		if i == lastUser {
			parts := []interface{}{textContentPart{Type: "text", Text: m.Content}}
			for _, img := range images {
				b64 := base64.StdEncoding.EncodeToString(img)
				parts = append(parts, imageContentPart{
					Type:     "image_url",
					ImageURL: imageURL{URL: "data:image/png;base64," + b64},
				})
			}
			out = append(out, multipartMessage{Role: m.Role, Content: parts})
			continue
		}
		out = append(out, simpleMessage{Role: m.Role, Content: m.Content})
	}
	return out
}

// call POSTs a chat-completion request with linear-backoff retry.
func (p *VLLMProvider) call(ctx context.Context, req CompletionReq, messages []interface{}) (chatResponse, error) {
	body := chatRequest{
		Model:       req.Model,
		Messages:    messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return chatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/v1/chat/completions"

	var lastErr error
	for attempt := 0; attempt < p.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			// Linear backoff mirroring the POC: sleep RetryDelay * (attempt+1).
			// POC: sleep 0.5 * (attempt + 1) where attempt starts at 0.
			select {
			case <-ctx.Done():
				return chatResponse{}, ctx.Err()
			case <-time.After(p.cfg.RetryDelay * time.Duration(attempt+1)):
			}
		}

		resp, err := p.doRequest(ctx, url, raw)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return chatResponse{}, fmt.Errorf("request failed after %d attempts: %w", p.cfg.MaxRetries, lastErr)
}

func (p *VLLMProvider) doRequest(ctx context.Context, url string, raw []byte) (chatResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return chatResponse{}, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return chatResponse{}, fmt.Errorf("http do: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return chatResponse{}, fmt.Errorf("read body: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return chatResponse{}, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(respBody))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return chatResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return parsed, nil
}

func (p *VLLMProvider) cost(model string, usage TokenUsage) float64 {
	price, ok := p.cfg.PriceTable[model]
	if !ok {
		return 0
	}
	return (float64(usage.Prompt)/1000.0)*price.PromptPerKToken +
		(float64(usage.Completion)/1000.0)*price.CompletionPerKToken
}

// compileSchema compiles a JSON schema document.
func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	const id = "schema.json"
	if err := c.AddResource(id, doc); err != nil {
		return nil, err
	}
	return c.Compile(id)
}

// validateJSON extracts JSON from content and validates it against sch.
func validateJSON(sch *jsonschema.Schema, content string) error {
	extracted, ok := extractJSON(content)
	if !ok {
		return fmt.Errorf("no JSON found in content")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(extracted))
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return sch.Validate(doc)
}

// extractJSON mirrors the Python POC's extract_json:
//   - strip ``` fences (with optional `json` suffix)
//   - try json.Unmarshal on stripped text
//   - fall back: find first { or [, scan for balanced close brace/bracket
func extractJSON(s string) (json.RawMessage, bool) {
	stripped := strings.TrimSpace(s)

	// Strip code fences.
	if strings.HasPrefix(stripped, "```") {
		stripped = strings.TrimPrefix(stripped, "```")
		stripped = strings.TrimPrefix(stripped, "json")
		stripped = strings.TrimSpace(stripped)
		if idx := strings.LastIndex(stripped, "```"); idx >= 0 {
			stripped = stripped[:idx]
		}
		stripped = strings.TrimSpace(stripped)
	}

	if json.Valid([]byte(stripped)) {
		return json.RawMessage(stripped), true
	}

	// Fall back: locate first { or [ and scan for the balanced closer.
	start := -1
	var open, closeCh byte
	for i := 0; i < len(stripped); i++ {
		if stripped[i] == '{' {
			start, open, closeCh = i, '{', '}'
			break
		}
		if stripped[i] == '[' {
			start, open, closeCh = i, '[', ']'
			break
		}
	}
	if start < 0 {
		return nil, false
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
