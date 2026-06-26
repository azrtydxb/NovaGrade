package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/azrtydxb/novagrade/internal/secrets"
)

func TestWebhookStore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tenantID := mustCreateSchool(t, s)

	// Generate a 32-byte AES-256-GCM encryption key.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}

	plainSecret := []byte("test-secret-value")
	encSecret, err := secrets.Encrypt(key, plainSecret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Sanity check: encrypted form must differ from plaintext.
	if bytes.Equal(encSecret, plainSecret) {
		t.Fatal("ciphertext must not equal plaintext")
	}

	const event = "published"
	const hookURL = "https://example.com/hook"

	var subID interface{ String() string }

	t.Run("CreateWebhookSubscription stores encrypted secret", func(t *testing.T) {
		sub, err := s.CreateWebhookSubscription(ctx, tenantID, event, hookURL, encSecret)
		if err != nil {
			t.Fatalf("CreateWebhookSubscription: %v", err)
		}
		if sub.ID.String() == "00000000-0000-0000-0000-000000000000" {
			t.Fatal("expected non-zero ID")
		}
		if sub.Event != event {
			t.Fatalf("event: got %q, want %q", sub.Event, event)
		}
		if sub.URL != hookURL {
			t.Fatalf("url: got %q, want %q", sub.URL, hookURL)
		}
		subID = sub.ID

		// Verify the stored bytes are the ciphertext, not the plaintext.
		// GetActiveWebhooksForEvent returns the raw encrypted bytes.
		active, err := s.GetActiveWebhooksForEvent(ctx, tenantID, event)
		if err != nil {
			t.Fatalf("GetActiveWebhooksForEvent: %v", err)
		}
		if len(active) == 0 {
			t.Fatal("expected at least one active subscription")
		}
		stored := active[0].EncryptedSecret
		if bytes.Equal(stored, plainSecret) {
			t.Fatal("stored bytes must be ciphertext, not plaintext")
		}
		if !bytes.Equal(stored, encSecret) {
			t.Fatal("stored ciphertext must match what was passed to CreateWebhookSubscription")
		}
	})

	t.Run("GetActiveWebhooksForEvent returns active subscription with encrypted secret", func(t *testing.T) {
		active, err := s.GetActiveWebhooksForEvent(ctx, tenantID, event)
		if err != nil {
			t.Fatalf("GetActiveWebhooksForEvent: %v", err)
		}
		if len(active) == 0 {
			t.Fatal("expected active subscription")
		}
		found := false
		for _, sub := range active {
			if sub.TenantID == tenantID && sub.Event == event && sub.URL == hookURL {
				found = true
				if !bytes.Equal(sub.EncryptedSecret, encSecret) {
					t.Fatalf("EncryptedSecret mismatch: got %x, want %x", sub.EncryptedSecret, encSecret)
				}
			}
		}
		if !found {
			t.Fatal("expected to find the created subscription in active webhooks")
		}
	})

	t.Run("ListWebhookSubscriptions returns subscription without secret field", func(t *testing.T) {
		// WebhookSubscription (returned by List) has no Secret field — compile-enforced.
		// Verify the row appears with correct event and URL.
		subs, err := s.ListWebhookSubscriptions(ctx, tenantID)
		if err != nil {
			t.Fatalf("ListWebhookSubscriptions: %v", err)
		}
		if len(subs) == 0 {
			t.Fatal("expected at least one subscription")
		}
		found := false
		for _, sub := range subs {
			if sub.TenantID == tenantID && sub.Event == event && sub.URL == hookURL {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected subscription with event=%q url=%q in list, got %+v", event, hookURL, subs)
		}
	})

	t.Run("DeleteWebhookSubscription deletes and second delete returns ErrNotFound", func(t *testing.T) {
		_ = subID // captured above
		// Create a fresh subscription to delete so we don't disturb the others.
		sub, err := s.CreateWebhookSubscription(ctx, tenantID, "graded", "https://example.com/delete-me", encSecret)
		if err != nil {
			t.Fatalf("CreateWebhookSubscription (for delete): %v", err)
		}

		if err := s.DeleteWebhookSubscription(ctx, tenantID, sub.ID); err != nil {
			t.Fatalf("first DeleteWebhookSubscription: %v", err)
		}
		if err := s.DeleteWebhookSubscription(ctx, tenantID, sub.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete: expected ErrNotFound, got %v", err)
		}
	})
}
