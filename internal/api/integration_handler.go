package api

// integration_handler.go — HTTP handlers for integration connection management.
//
// Endpoints:
//   POST   /v1/integrations         — create/update a connection (upsert)
//   GET    /v1/integrations         — list connections for the tenant (no credentials)
//   DELETE /v1/integrations/{id}    — delete a connection
//
// RBAC: ActionEditTunables required for all endpoints.
// Credential encryption: POST caller supplies raw credentials JSON; handler encrypts
// with AES-GCM using INTEGRATION_ENC_KEY env var before storing. Credentials are
// NEVER returned in list or delete responses (integration.Connection has no
// credentials field, so JSON encoding cannot leak them).
//
// Tenant isolation: all operations are scoped to the authenticated principal's tenant.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/integration"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// IntegrationStore is the subset of store.Store required by IntegrationHandlers.
type IntegrationStore interface {
	UpsertConnection(ctx context.Context, p store.UpsertConnectionParams) (integration.Connection, error)
	ListConnections(ctx context.Context, tenantID uuid.UUID) ([]integration.Connection, error)
	DeleteConnection(ctx context.Context, tenantID, id uuid.UUID) error
}

// IntegrationHandlers holds dependencies for the integration config HTTP handlers.
type IntegrationHandlers struct {
	Store      IntegrationStore
	Registry   *integration.Registry
	DeployMode string
}

// integrationAuthz extracts and validates the principal and checks ActionEditTunables.
func (h *IntegrationHandlers) integrationAuthz(w http.ResponseWriter, r *http.Request) (auth.Principal, uuid.UUID, bool) {
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

// postIntegrationBody is the JSON body for POST /v1/integrations.
type postIntegrationBody struct {
	Category    string          `json:"category"`
	Provider    string          `json:"provider"`
	Config      json.RawMessage `json:"config"`
	Credentials json.RawMessage `json:"credentials"` // raw JSON; will be encrypted
}

// UpsertIntegration handles POST /v1/integrations.
func (h *IntegrationHandlers) UpsertIntegration(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.integrationAuthz(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	var req postIntegrationBody
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Category == "" || req.Provider == "" {
		http.Error(w, "category and provider are required", http.StatusBadRequest)
		return
	}

	// Validate (category, provider) via registry — 400 if unknown.
	if h.Registry != nil {
		if _, ok := h.Registry.Get(integration.Category(req.Category), req.Provider); !ok {
			http.Error(w, "unknown connector: "+req.Category+"/"+req.Provider, http.StatusBadRequest)
			return
		}
	}

	// Encrypt credentials if provided.
	var encCreds []byte
	if len(req.Credentials) > 0 && string(req.Credentials) != "null" {
		key, err := secrets.KeyFromEnv("INTEGRATION_ENC_KEY")
		if err != nil {
			http.Error(w, "server misconfiguration: encryption key unavailable", http.StatusInternalServerError)
			return
		}
		encCreds, err = secrets.Encrypt(key, req.Credentials)
		if err != nil {
			http.Error(w, "encryption error", http.StatusInternalServerError)
			return
		}
	}

	conn, err := h.Store.UpsertConnection(r.Context(), store.UpsertConnectionParams{
		TenantID:    tenantID,
		Category:    integration.Category(req.Category),
		Provider:    req.Provider,
		Config:      req.Config,
		Credentials: encCreds,
	})
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(conn)
}

// ListIntegrations handles GET /v1/integrations.
func (h *IntegrationHandlers) ListIntegrations(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.integrationAuthz(w, r)
	if !ok {
		return
	}

	conns, err := h.Store.ListConnections(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if conns == nil {
		conns = []integration.Connection{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(conns)
}

// DeleteIntegration handles DELETE /v1/integrations/{id}.
func (h *IntegrationHandlers) DeleteIntegration(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.integrationAuthz(w, r)
	if !ok {
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.Store.DeleteConnection(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
