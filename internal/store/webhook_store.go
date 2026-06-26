// Package store — webhook_store.go
//
// CALLER ENCRYPTS: This file persists webhook subscription secrets as raw []byte.
// The store has no knowledge of encryption keys. Callers MUST encrypt secrets with
// secrets.Encrypt before passing them here, and decrypt after retrieval with secrets.Decrypt.
// Secrets are NEVER logged or returned in list operations.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/azrtydxb/novagrade/internal/store/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WebhookSubscription is the domain struct for a webhook subscription (no secret).
// Used in list responses.
type WebhookSubscription struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Event     string
	URL       string
	Active    bool
	CreatedAt time.Time
}

// WebhookSubscriptionWithSecret is the domain struct that includes the encrypted
// secret. Used by the dispatch path — the caller decrypts before use.
type WebhookSubscriptionWithSecret struct {
	WebhookSubscription
	EncryptedSecret []byte
}

// CreateWebhookSubscription inserts a new webhook subscription.
// encryptedSecret must be already encrypted by the caller via secrets.Encrypt.
func (s *Store) CreateWebhookSubscription(ctx context.Context, tenant uuid.UUID, event, url string, encryptedSecret []byte) (WebhookSubscription, error) {
	row, err := s.queries.CreateWebhookSubscription(ctx, db.CreateWebhookSubscriptionParams{
		TenantID: tenant,
		Event:    event,
		Url:      url,
		Secret:   encryptedSecret,
	})
	if err != nil {
		return WebhookSubscription{}, fmt.Errorf("store: create webhook subscription: %w", err)
	}
	return domainWebhookSubscription(row), nil
}

// ListWebhookSubscriptions returns all subscriptions for a tenant WITHOUT secrets.
func (s *Store) ListWebhookSubscriptions(ctx context.Context, tenant uuid.UUID) ([]WebhookSubscription, error) {
	rows, err := s.queries.ListWebhookSubscriptions(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("store: list webhook subscriptions: %w", err)
	}
	out := make([]WebhookSubscription, 0, len(rows))
	for _, r := range rows {
		out = append(out, WebhookSubscription{
			ID:        r.ID,
			TenantID:  r.TenantID,
			Event:     r.Event,
			URL:       r.Url,
			Active:    r.Active,
			CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, nil
}

// GetActiveWebhooksForEvent returns all active subscriptions for the given tenant
// and event type, including the encrypted secret for dispatch.
func (s *Store) GetActiveWebhooksForEvent(ctx context.Context, tenant uuid.UUID, event string) ([]WebhookSubscriptionWithSecret, error) {
	rows, err := s.queries.GetActiveWebhooksForEvent(ctx, db.GetActiveWebhooksForEventParams{
		TenantID: tenant,
		Event:    event,
	})
	if err != nil {
		return nil, fmt.Errorf("store: get active webhooks for event: %w", err)
	}
	out := make([]WebhookSubscriptionWithSecret, 0, len(rows))
	for _, r := range rows {
		out = append(out, WebhookSubscriptionWithSecret{
			WebhookSubscription: domainWebhookSubscription(r),
			EncryptedSecret:     r.Secret,
		})
	}
	return out, nil
}

// DeleteWebhookSubscription deletes a subscription by id and tenant.
// Returns ErrNotFound if 0 rows are affected.
func (s *Store) DeleteWebhookSubscription(ctx context.Context, tenant, id uuid.UUID) error {
	n, err := s.queries.DeleteWebhookSubscription(ctx, db.DeleteWebhookSubscriptionParams{
		ID:       id,
		TenantID: tenant,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("store: delete webhook subscription: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// domainWebhookSubscription converts a db.WebhookSubscription to the domain struct.
func domainWebhookSubscription(row db.WebhookSubscription) WebhookSubscription {
	return WebhookSubscription{
		ID:        row.ID,
		TenantID:  row.TenantID,
		Event:     row.Event,
		URL:       row.Url,
		Active:    row.Active,
		CreatedAt: row.CreatedAt.Time,
	}
}
