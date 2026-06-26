// Package webhook delivers HMAC-signed JSON events to subscriber URLs.
//
// Delivery is fire-and-forget from the caller's perspective: the Dispatch
// function logs errors rather than returning them. The Sender type handles
// signing, HTTP posting, and retry-with-backoff.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Event is the payload delivered to subscriber URLs.
type Event struct {
	Type         string         `json:"type"`
	TenantID     string         `json:"tenant_id"`
	SubmissionID string         `json:"submission_id"`
	Data         map[string]any `json:"data,omitempty"`
	OccurredAt   time.Time      `json:"occurred_at"`
}

// Sender posts events to webhook subscriber URLs with HMAC-SHA256 signing and
// exponential backoff on transport errors and 5xx responses.
type Sender struct {
	Client      *http.Client
	MaxAttempts int           // total attempts (including first); default 3
	BackoffBase time.Duration // starting backoff (doubles each retry); default 200ms
}

// NewSender creates a Sender with the given HTTP timeout and max attempts.
// BackoffBase defaults to 200ms.
func NewSender(timeout time.Duration, maxAttempts int) *Sender {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return &Sender{
		Client:      &http.Client{Timeout: timeout},
		MaxAttempts: maxAttempts,
		BackoffBase: 200 * time.Millisecond,
	}
}

// Deliver POSTs the event to url, signing the JSON body with HMAC-SHA256(body, secret).
//
// Headers sent:
//   - Content-Type: application/json
//   - X-NovaGrade-Signature: sha256=<hex>
//   - X-NovaGrade-Event: <event.Type>
//
// Retry policy:
//   - Transport errors or 5xx: retry up to MaxAttempts total, with doubling backoff
//     starting at BackoffBase.
//   - 4xx: no retry, return error immediately.
//   - 2xx: success, return nil.
//
// The secret is NEVER logged.
func (s *Sender) Deliver(ctx context.Context, url string, secret []byte, event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("webhook: marshal event: %w", err)
	}

	// Compute HMAC-SHA256 once — body is immutable across retries.
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	backoff := s.BackoffBase
	var lastErr error

	for attempt := 1; attempt <= s.MaxAttempts; attempt++ {
		if attempt > 1 {
			// Backoff before retry. Respect context cancellation.
			select {
			case <-ctx.Done():
				return fmt.Errorf("webhook: context cancelled during backoff: %w", ctx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("webhook: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NovaGrade-Signature", sig)
		req.Header.Set("X-NovaGrade-Event", event.Type)

		resp, err := s.Client.Do(req)
		if err != nil {
			// Transport error — log and retry.
			log.Printf("webhook: deliver attempt %d/%d to %s: transport error: %v", attempt, s.MaxAttempts, url, err)
			lastErr = fmt.Errorf("webhook: deliver attempt %d: transport error: %w", attempt, err)
			continue
		}
		_ = resp.Body.Close()

		status := resp.StatusCode
		if status >= 200 && status < 300 {
			return nil
		}
		if status >= 400 && status < 500 {
			// 4xx — no retry.
			return fmt.Errorf("webhook: deliver to %s: status %d (no retry)", url, status)
		}
		// 5xx — log and retry.
		log.Printf("webhook: deliver attempt %d/%d to %s: status %d", attempt, s.MaxAttempts, url, status)
		lastErr = fmt.Errorf("webhook: deliver attempt %d: status %d", attempt, status)
	}

	return fmt.Errorf("webhook: all %d attempts failed for %s: %w", s.MaxAttempts, url, lastErr)
}
