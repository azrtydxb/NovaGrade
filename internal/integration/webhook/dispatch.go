package webhook

import (
	"context"
	"log"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// WebhookStore is the minimal interface for dispatch (testable with a fake).
type WebhookStore interface {
	GetActiveWebhooksForEvent(ctx context.Context, tenant uuid.UUID, event string) ([]store.WebhookSubscriptionWithSecret, error)
}

// DeliverFunc is the function signature for delivering an event. It matches
// (*Sender).Deliver so a real Sender can be used in production and a fake
// can be substituted in tests.
type DeliverFunc func(ctx context.Context, url string, secret []byte, event Event) error

// DispatchFunc fetches active subscriptions for the event, decrypts each secret,
// and calls deliverFn for each. Errors (decrypt or deliver) are LOGGED, never
// returned — this function is intended to run in a fire-and-forget goroutine.
//
// key is the AES-256-GCM key from secrets.KeyFromEnv("INTEGRATION_ENC_KEY").
func DispatchFunc(ctx context.Context, st WebhookStore, deliverFn DeliverFunc, key []byte, tenantID uuid.UUID, event Event) {
	subs, err := st.GetActiveWebhooksForEvent(ctx, tenantID, event.Type)
	if err != nil {
		log.Printf("webhook: dispatch: get active subs for tenant=%s event=%s: %v", tenantID, event.Type, err)
		return
	}
	for _, sub := range subs {
		plainSecret, err := secrets.Decrypt(key, sub.EncryptedSecret)
		if err != nil {
			log.Printf("webhook: dispatch: decrypt secret for sub=%s: %v", sub.ID, err)
			continue
		}
		if err := deliverFn(ctx, sub.URL, plainSecret, event); err != nil {
			log.Printf("webhook: dispatch: deliver to sub=%s url=%s: %v", sub.ID, sub.URL, err)
		}
	}
}

// Dispatch is the production helper that wraps DispatchFunc using a real *Sender.
// Errors are LOGGED, never returned. Intended to run in a fire-and-forget goroutine.
//
// key is the AES-256-GCM key from secrets.KeyFromEnv("INTEGRATION_ENC_KEY").
func Dispatch(ctx context.Context, st WebhookStore, sender *Sender, key []byte, tenantID uuid.UUID, event Event) {
	DispatchFunc(ctx, st, sender.Deliver, key, tenantID, event)
}
