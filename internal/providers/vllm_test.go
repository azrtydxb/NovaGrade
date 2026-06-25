package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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

	provider := NewVLLMProvider(VLLMConfig{
		BaseURL:    srv.URL,
		MaxRetries: 3,
		RetryDelay: 1 * time.Millisecond,
		LogSink:    logSink,
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

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, logged, 1)
	assert.Equal(t, "test-model", logged[0].Model)
	assert.Equal(t, 10, logged[0].Tokens.Prompt)
	assert.Equal(t, 5, logged[0].Tokens.Completion)
	assert.Equal(t, 15, logged[0].Tokens.Total)
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
