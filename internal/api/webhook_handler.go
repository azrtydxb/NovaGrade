package api

// webhook_handler.go — HTTP handlers for webhook subscription management.
//
// Endpoints:
//   POST   /v1/webhooks      — create a subscription (secret returned ONCE)
//   GET    /v1/webhooks      — list subscriptions for the tenant (no secret)
//   DELETE /v1/webhooks/{id} — delete a subscription
//
// RBAC: ActionEditTunables required for all endpoints.
// Secret handling: a 32-byte random secret is generated per subscription.
// The plaintext is returned exactly once in the create response (base64 standard
// encoding). The store receives only the AES-GCM-encrypted form; it is NEVER
// returned in subsequent responses.
//
// Tenant isolation: all operations are scoped to the authenticated principal's tenant.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// WebhookStore is the subset of store.Store required by WebhookHandlers.
type WebhookStore interface {
	CreateWebhookSubscription(ctx context.Context, tenant uuid.UUID, event, url string, encryptedSecret []byte) (store.WebhookSubscription, error)
	ListWebhookSubscriptions(ctx context.Context, tenant uuid.UUID) ([]store.WebhookSubscription, error)
	DeleteWebhookSubscription(ctx context.Context, tenant, id uuid.UUID) error
}

// WebhookHandlers holds dependencies for the webhook subscription HTTP handlers.
type WebhookHandlers struct {
	Store      WebhookStore
	EncKey     []byte // AES-256-GCM key for encrypting secrets
	DeployMode string
}

// webhookAuthz extracts and validates the principal and checks ActionEditTunables.
func (h *WebhookHandlers) webhookAuthz(w http.ResponseWriter, r *http.Request) (auth.Principal, uuid.UUID, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return auth.Principal{}, uuid.UUID{}, false
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionEditTunables, rctx) {
		http.Error(w, "not found", http.StatusNotFound)
		return auth.Principal{}, uuid.UUID{}, false
	}
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return auth.Principal{}, uuid.UUID{}, false
	}
	return p, tenantID, true
}

// postWebhookBody is the JSON body for POST /v1/webhooks.
type postWebhookBody struct {
	Event string `json:"event"`
	URL   string `json:"url"`
}

// createWebhookResponse is the JSON body returned by POST /v1/webhooks.
// The secret is returned ONLY in this response.
type createWebhookResponse struct {
	ID        uuid.UUID `json:"id"`
	Event     string    `json:"event"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret"`    // base64-encoded plaintext; shown only once
	Note      string    `json:"note"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// listWebhookItem is the JSON shape for items in GET /v1/webhooks.
// No secret field — enforced by struct definition.
type listWebhookItem struct {
	ID        uuid.UUID `json:"id"`
	Event     string    `json:"event"`
	URL       string    `json:"url"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// Create handles POST /v1/webhooks.
func (h *WebhookHandlers) Create(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.webhookAuthz(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	var req postWebhookBody
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Event == "" || req.URL == "" {
		http.Error(w, "event and url are required", http.StatusBadRequest)
		return
	}

	// Generate 32 random bytes as the plaintext secret.
	plainSecret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, plainSecret); err != nil {
		http.Error(w, "failed to generate secret", http.StatusInternalServerError)
		return
	}

	// Encrypt secret — EncKey must be set.
	if len(h.EncKey) != 32 {
		http.Error(w, "server misconfiguration: encryption key unavailable", http.StatusInternalServerError)
		return
	}
	encSecret, err := secrets.Encrypt(h.EncKey, plainSecret)
	if err != nil {
		http.Error(w, "encryption error", http.StatusInternalServerError)
		return
	}

	sub, err := h.Store.CreateWebhookSubscription(r.Context(), tenantID, req.Event, req.URL, encSecret)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// Return plaintext secret exactly once (base64 standard encoding). NEVER log it.
	resp := createWebhookResponse{
		ID:        sub.ID,
		Event:     sub.Event,
		URL:       sub.URL,
		Secret:    base64.StdEncoding.EncodeToString(plainSecret),
		Note:      "Secret shown only once. Store it securely — it will not be returned again.",
		Active:    sub.Active,
		CreatedAt: sub.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)

	// Zero out plaintext secret from memory (best-effort).
	for i := range plainSecret {
		plainSecret[i] = 0
	}
}

// List handles GET /v1/webhooks.
func (h *WebhookHandlers) List(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.webhookAuthz(w, r)
	if !ok {
		return
	}

	subs, err := h.Store.ListWebhookSubscriptions(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	items := make([]listWebhookItem, 0, len(subs))
	for _, s := range subs {
		items = append(items, listWebhookItem{
			ID:        s.ID,
			Event:     s.Event,
			URL:       s.URL,
			Active:    s.Active,
			CreatedAt: s.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// Delete handles DELETE /v1/webhooks/{id}.
func (h *WebhookHandlers) Delete(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.webhookAuthz(w, r)
	if !ok {
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.Store.DeleteWebhookSubscription(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("store error: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
