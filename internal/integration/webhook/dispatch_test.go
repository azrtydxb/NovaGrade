package webhook_test

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/integration/webhook"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────────────

type fakeWebhookStore struct {
	mu   sync.Mutex
	subs []store.WebhookSubscriptionWithSecret
}

func (f *fakeWebhookStore) GetActiveWebhooksForEvent(_ context.Context, tenant uuid.UUID, event string) ([]store.WebhookSubscriptionWithSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.WebhookSubscriptionWithSecret
	for _, s := range f.subs {
		if s.TenantID == tenant && s.Event == event {
			out = append(out, s)
		}
	}
	return out, nil
}

type deliveryRecord struct {
	URL    string
	Secret []byte
	Event  webhook.Event
}

type fakeSender struct {
	mu        sync.Mutex
	deliveries []deliveryRecord
	err        error // if non-nil, Deliver returns this error
}

func (f *fakeSender) DeliverCall(ctx context.Context, url string, secret []byte, event webhook.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, deliveryRecord{URL: url, Secret: secret, Event: event})
	return f.err
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func testEncKeyBytes(t *testing.T) []byte {
	t.Helper()
	t.Setenv("INTEGRATION_ENC_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	key, err := secrets.KeyFromEnv("INTEGRATION_ENC_KEY")
	require.NoError(t, err)
	return key
}

// TestDispatch_TwoSubscribers verifies both active subscribers receive the event,
// and that the decrypted secret is delivered to the sender.
func TestDispatch_TwoSubscribers(t *testing.T) {
	key := testEncKeyBytes(t)
	tenantID := uuid.New()

	plainSecret1 := []byte("plain-secret-one-32bytes-padding!")
	plainSecret2 := []byte("plain-secret-two-32bytes-padding!")
	enc1, err := secrets.Encrypt(key, plainSecret1)
	require.NoError(t, err)
	enc2, err := secrets.Encrypt(key, plainSecret2)
	require.NoError(t, err)

	sub1 := store.WebhookSubscriptionWithSecret{
		WebhookSubscription: store.WebhookSubscription{
			ID:        uuid.New(),
			TenantID:  tenantID,
			Event:     "published",
			URL:       "https://example.com/hook1",
			Active:    true,
			CreatedAt: time.Now(),
		},
		EncryptedSecret: enc1,
	}
	sub2 := store.WebhookSubscriptionWithSecret{
		WebhookSubscription: store.WebhookSubscription{
			ID:        uuid.New(),
			TenantID:  tenantID,
			Event:     "published",
			URL:       "https://example.com/hook2",
			Active:    true,
			CreatedAt: time.Now(),
		},
		EncryptedSecret: enc2,
	}

	fakeStore := &fakeWebhookStore{subs: []store.WebhookSubscriptionWithSecret{sub1, sub2}}
	fake := &fakeSender{}
	realSender := webhook.NewSender(5*time.Second, 3)
	_ = realSender // not used — we use a wrapper

	// Wrap fake into a *Sender-compatible call by using DispatchFunc.
	// Dispatch takes a *Sender, so use a real Sender but intercept via a test server.
	// Instead, use DispatchFunc variant.
	event := webhook.Event{
		Type:         "published",
		TenantID:     tenantID.String(),
		SubmissionID: uuid.New().String(),
		OccurredAt:   time.Now(),
	}

	webhook.DispatchFunc(context.Background(), fakeStore, fake.DeliverCall, key, tenantID, event)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.Len(t, fake.deliveries, 2, "both subscribers must be delivered")

	// Check URLs.
	urls := map[string]bool{}
	for _, d := range fake.deliveries {
		urls[d.URL] = true
	}
	assert.True(t, urls["https://example.com/hook1"])
	assert.True(t, urls["https://example.com/hook2"])

	// Check secrets were decrypted correctly.
	secretsByURL := map[string][]byte{}
	for _, d := range fake.deliveries {
		secretsByURL[d.URL] = d.Secret
	}
	assert.Equal(t, plainSecret1, secretsByURL["https://example.com/hook1"])
	assert.Equal(t, plainSecret2, secretsByURL["https://example.com/hook2"])
}

// TestDispatch_NoSubscribers verifies that an event with no matching subs results in 0 deliveries.
func TestDispatch_NoSubscribers(t *testing.T) {
	key := testEncKeyBytes(t)
	tenantID := uuid.New()

	fakeStore := &fakeWebhookStore{subs: nil}
	fake := &fakeSender{}

	event := webhook.Event{
		Type:         "graded",
		TenantID:     tenantID.String(),
		SubmissionID: uuid.New().String(),
		OccurredAt:   time.Now(),
	}

	webhook.DispatchFunc(context.Background(), fakeStore, fake.DeliverCall, key, tenantID, event)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Len(t, fake.deliveries, 0, "no deliveries when no matching subscribers")
}

// TestDispatch_SenderErrorSwallowed verifies that delivery errors are logged, not returned.
func TestDispatch_SenderErrorSwallowed(t *testing.T) {
	key := testEncKeyBytes(t)
	tenantID := uuid.New()

	plainSecret := []byte("plain-secret-one-32bytes-padding!")
	enc, err := secrets.Encrypt(key, plainSecret)
	require.NoError(t, err)

	sub := store.WebhookSubscriptionWithSecret{
		WebhookSubscription: store.WebhookSubscription{
			ID:        uuid.New(),
			TenantID:  tenantID,
			Event:     "published",
			URL:       "https://example.com/hook",
			Active:    true,
			CreatedAt: time.Now(),
		},
		EncryptedSecret: enc,
	}
	fakeStore := &fakeWebhookStore{subs: []store.WebhookSubscriptionWithSecret{sub}}

	// Sender that always errors.
	errSender := &fakeSender{err: assert.AnError}

	// Must not panic or return error.
	assert.NotPanics(t, func() {
		webhook.DispatchFunc(context.Background(), fakeStore, errSender.DeliverCall, key, tenantID, webhook.Event{
			Type:         "published",
			TenantID:     tenantID.String(),
			SubmissionID: uuid.New().String(),
			OccurredAt:   time.Now(),
		})
	})
}
