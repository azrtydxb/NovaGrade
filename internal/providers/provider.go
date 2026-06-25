// Package providers defines the AI provider abstraction used by NovaGrade and
// a concrete vLLM (OpenAI-compatible) implementation.
package providers

import (
	"context"
	"encoding/json"
	"time"
)

// AIProvider abstracts a chat-completion backend.
type AIProvider interface {
	Complete(ctx context.Context, req CompletionReq) (CompletionResp, error)
}

// CompletionReq is a single completion request.
type CompletionReq struct {
	Model         string
	PromptVersion string
	Messages      []Message
	Images        [][]byte
	Schema        json.RawMessage
	MaxTokens     int
	Temperature   float64
}

// Message is a single chat message.
type Message struct {
	Role    string
	Content string
}

// CompletionResp is the result of a completion.
type CompletionResp struct {
	Content     string
	Tokens      TokenUsage
	CostUSD     float64
	SchemaValid bool
}

// TokenUsage captures token accounting for a completion.
type TokenUsage struct {
	Prompt     int
	Completion int
	Total      int
}

// AICallLog is emitted (via VLLMConfig.LogSink) after every Complete call,
// including failures, for observability and cost tracking.
type AICallLog struct {
	Model         string
	PromptVersion string
	Tokens        TokenUsage
	CostUSD       float64
	SchemaValid   bool
}

// ModelPrice describes per-1K-token pricing for a model.
type ModelPrice struct {
	PromptPerKToken     float64
	CompletionPerKToken float64
}

// VLLMConfig configures a VLLMProvider.
type VLLMConfig struct {
	BaseURL    string
	APIKey     string
	PriceTable map[string]ModelPrice
	MaxRetries int
	RetryDelay time.Duration
	LogSink    func(AICallLog) // called after every Complete (even on error)
}
