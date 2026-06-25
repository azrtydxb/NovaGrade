package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/azrtydxb/novagrade/internal/providers"
)

func main() {
	baseURL := os.Getenv("VLLM_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8000"
	}
	apiKey := os.Getenv("VLLM_API_KEY")
	model := os.Getenv("VLLM_MODEL")
	if model == "" {
		model = "default"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	cfg := providers.VLLMConfig{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		MaxRetries: 3,
		RetryDelay: 500 * time.Millisecond,
		LogSink: func(l providers.AICallLog) {
			log.Printf("ai-call model=%s prompt_version=%s tokens=%d cost=%.6f schema_valid=%v",
				l.Model, l.PromptVersion, l.Tokens.Total, l.CostUSD, l.SchemaValid)
		},
	}
	provider := providers.NewVLLMProvider(cfg)
	_ = model // default model, can be overridden per request

	http.HandleFunc("/complete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req providers.CompletionReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := provider.Complete(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	addr := ":" + port
	fmt.Printf("ai-gateway: listening on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
