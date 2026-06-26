package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/integration/webhook"
)

func makeTestSender(maxAttempts int, backoffBase time.Duration) *webhook.Sender {
	s := webhook.NewSender(5*time.Second, maxAttempts)
	s.BackoffBase = backoffBase
	return s
}

func makeTestEvent() webhook.Event {
	return webhook.Event{
		Type:         "published",
		TenantID:     "tenant-123",
		SubmissionID: "sub-456",
		OccurredAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// TestSender_HappyPath verifies: correct JSON body, valid HMAC, correct headers, 2xx → no error.
func TestSender_HappyPath(t *testing.T) {
	secret := []byte("test-secret-value-32byteslong!!!")
	var receivedBody []byte
	var receivedSig string
	var receivedEvent string
	var receivedContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		receivedBody = buf.Bytes()
		receivedSig = r.Header.Get("X-NovaGrade-Signature")
		receivedEvent = r.Header.Get("X-NovaGrade-Event")
		receivedContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := makeTestSender(3, 1*time.Millisecond)
	event := makeTestEvent()

	err := sender.Deliver(context.Background(), srv.URL, secret, event)
	require.NoError(t, err)

	// Validate JSON body.
	var got webhook.Event
	require.NoError(t, json.Unmarshal(receivedBody, &got))
	assert.Equal(t, "published", got.Type)
	assert.Equal(t, "tenant-123", got.TenantID)
	assert.Equal(t, "sub-456", got.SubmissionID)

	// Validate HMAC signature.
	assert.Equal(t, "application/json", receivedContentType)
	assert.Equal(t, "published", receivedEvent)
	require.NotEmpty(t, receivedSig)

	mac := hmac.New(sha256.New, secret)
	mac.Write(receivedBody)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	assert.Equal(t, expected, receivedSig, "HMAC signature must match")
}

// TestSender_RetryOn5xx verifies that 5xx responses cause retries up to MaxAttempts.
func TestSender_RetryOn5xx(t *testing.T) {
	secret := []byte("test-secret-value-32byteslong!!!")
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sender := makeTestSender(3, 1*time.Millisecond)
	event := makeTestEvent()

	err := sender.Deliver(context.Background(), srv.URL, secret, event)
	assert.Error(t, err, "all retries exhausted must return error")
	assert.Equal(t, int32(3), attempts.Load(), "must attempt exactly MaxAttempts=3 times")
}

// TestSender_NoRetryOn4xx verifies that 4xx responses are not retried.
func TestSender_NoRetryOn4xx(t *testing.T) {
	secret := []byte("test-secret-value-32byteslong!!!")
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	sender := makeTestSender(3, 1*time.Millisecond)
	event := makeTestEvent()

	err := sender.Deliver(context.Background(), srv.URL, secret, event)
	assert.Error(t, err, "4xx must return error")
	assert.Equal(t, int32(1), attempts.Load(), "must attempt exactly 1 time (no retry on 4xx)")
}

// TestSender_SecretNotLogged verifies that the secret never appears in log output.
func TestSender_SecretNotLogged(t *testing.T) {
	secretBytes := []byte("super-sensitive-webhook-secret!!")
	var logBuf bytes.Buffer

	// Redirect default logger to our buffer.
	oldWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldWriter)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := makeTestSender(3, 1*time.Millisecond)
	event := makeTestEvent()

	err := sender.Deliver(context.Background(), srv.URL, secretBytes, event)
	require.NoError(t, err)

	logOutput := logBuf.String()
	assert.NotContains(t, logOutput, string(secretBytes), "secret must never appear in log output")
	assert.NotContains(t, logOutput, hex.EncodeToString(secretBytes), "secret hex must never appear in log output")
}
