package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVLLMComplete_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"{\"score\":42}"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`))
	}))
	defer srv.Close()

	var (
		mu      sync.Mutex
		logged  []AICallLog
		logSink = func(l AICallLog) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, l)
		}
	)

	// Fix 6: supply a price table so cost computation is covered.
	// cost = (10/1000)*0.01 + (5/1000)*0.02 = 0.0001 + 0.0001 = 0.0002
	const wantCost = (10.0/1000.0)*0.01 + (5.0/1000.0)*0.02

	provider := NewVLLMProvider(VLLMConfig{
		BaseURL:    srv.URL,
		MaxRetries: 3,
		RetryDelay: 1 * time.Millisecond,
		LogSink:    logSink,
		PriceTable: map[string]ModelPrice{
			"test-model": {PromptPerKToken: 0.01, CompletionPerKToken: 0.02},
		},
	})

	resp, err := provider.Complete(context.Background(), CompletionReq{
		Model:     "test-model",
		Messages:  []Message{{Role: "user", Content: "hello"}},
		MaxTokens: 100,
	})
	require.NoError(t, err)

	assert.Contains(t, resp.Content, "score")
	assert.Equal(t, 10, resp.Tokens.Prompt)
	assert.Equal(t, 5, resp.Tokens.Completion)
	assert.Equal(t, 15, resp.Tokens.Total)
	// Fix 6: assert cost is the expected non-zero value.
	assert.InDelta(t, wantCost, resp.CostUSD, 1e-9, "resp.CostUSD should be non-zero")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, logged, 1)
	assert.Equal(t, "test-model", logged[0].Model)
	assert.Equal(t, 10, logged[0].Tokens.Prompt)
	assert.Equal(t, 5, logged[0].Tokens.Completion)
	assert.Equal(t, 15, logged[0].Tokens.Total)
	// Fix 6: assert cost is logged correctly.
	assert.InDelta(t, wantCost, logged[0].CostUSD, 1e-9, "logged CostUSD should match")
}

func TestVLLMComplete_schemaInvalid(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"not valid json at all"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`))
	}))
	defer srv.Close()

	var (
		logMu  sync.Mutex
		logged []AICallLog
	)

	provider := NewVLLMProvider(VLLMConfig{
		BaseURL:    srv.URL,
		MaxRetries: 3,
		RetryDelay: 1 * time.Millisecond,
		LogSink: func(l AICallLog) {
			logMu.Lock()
			defer logMu.Unlock()
			logged = append(logged, l)
		},
	})

	schema := json.RawMessage(`{"type":"object","properties":{"score":{"type":"number"}},"required":["score"]}`)

	_, err := provider.Complete(context.Background(), CompletionReq{
		Model:     "test-model",
		Messages:  []Message{{Role: "user", Content: "hello"}},
		Schema:    schema,
		MaxTokens: 100,
	})
	require.Error(t, err)

	mu.Lock()
	assert.Equal(t, 2, requests, "expected one initial request + one re-ask")
	mu.Unlock()

	logMu.Lock()
	defer logMu.Unlock()
	require.Len(t, logged, 1)
	assert.False(t, logged[0].SchemaValid, "expected SchemaValid=false on failed validation")
}

// TestExtractJSON_prefersEarliestStart verifies that when both '[' and '{' appear
// in the input, extractJSON picks whichever comes FIRST (mirrors Python POC
// `start = min(starts)`).
func TestExtractJSON_prefersEarliestStart(t *testing.T) {
	// '[' appears before '{', so the array should be extracted, not the trailing object.
	input := `[{"a":1}] trailing {x}`
	got, ok := extractJSON(input)
	require.True(t, ok, "extractJSON should succeed")
	assert.Equal(t, `[{"a":1}]`, string(got))
}

// TestVLLMComplete_no4xxRetry asserts that a 400 response is NOT retried —
// the server should be hit exactly once and Complete must return an error.
func TestVLLMComplete_no4xxRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	provider := NewVLLMProvider(VLLMConfig{
		BaseURL:    srv.URL,
		MaxRetries: 3,
		RetryDelay: 1 * time.Millisecond,
	})

	_, err := provider.Complete(context.Background(), CompletionReq{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	require.Error(t, err, "Complete should return an error on 400")
	assert.EqualValues(t, 1, hits.Load(), "server should be hit exactly once (no retries for 4xx)")
}
