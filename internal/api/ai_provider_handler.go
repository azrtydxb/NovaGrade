package api

// ai_provider_handler.go — HTTP handlers for per-tenant AI provider config.
//
// Endpoints:
//   POST /v1/ai-providers              — register a provider (api_key encrypted at rest)
//   GET  /v1/ai-providers              — list providers for the tenant (no keys)
//   POST /v1/ai-providers/{id}/default — make a provider the tenant default
//
// RBAC: ActionEditTunables required on every endpoint; 404-on-denial.
// Tenant isolation: all operations scoped to the authenticated principal's tenant.
//
// Key handling: the POST caller supplies a plaintext api_key; the handler
// encrypts it with AES-GCM using INTEGRATION_ENC_KEY before storing. The API key
// is NEVER logged and NEVER returned in any response — the store.AIProviderConfig
// domain type has no key field, so JSON encoding cannot leak it.

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
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// AIProviderStore is the subset of store.Store required by AIProviderHandlers.
type AIProviderStore interface {
	CreateAIProviderConfig(ctx context.Context, p store.CreateAIProviderConfigParams) (store.AIProviderConfig, error)
	ListAIProviderConfigs(ctx context.Context, tenantID uuid.UUID) ([]store.AIProviderConfig, error)
	GetDefaultAIProviderConfigWithKey(ctx context.Context, tenantID uuid.UUID) (store.AIProviderConfig, []byte, error)
	SetDefaultAIProviderConfig(ctx context.Context, tenantID, id uuid.UUID) error
}

// AIProviderHandlers holds dependencies for the AI provider config HTTP handlers.
type AIProviderHandlers struct {
	Store      AIProviderStore
	DeployMode string
}

// aiProviderAuthz extracts and validates the principal and checks ActionEditTunables.
func (h *AIProviderHandlers) aiProviderAuthz(w http.ResponseWriter, r *http.Request) (auth.Principal, uuid.UUID, bool) {
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

// postAIProviderBody is the JSON body for POST /v1/ai-providers.
type postAIProviderBody struct {
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"api_key"` // plaintext; encrypted before storage, never returned
}

// CreateAIProvider handles POST /v1/ai-providers.
func (h *AIProviderHandlers) CreateAIProvider(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.aiProviderAuthz(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	var req postAIProviderBody
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.ProviderType == "" || req.BaseURL == "" || req.Model == "" {
		http.Error(w, "name, provider_type, base_url and model are required", http.StatusBadRequest)
		return
	}

	// Encrypt the api_key if provided.
	var encKey []byte
	if req.APIKey != "" {
		key, err := secrets.KeyFromEnv("INTEGRATION_ENC_KEY")
		if err != nil {
			http.Error(w, "server misconfiguration: encryption key unavailable", http.StatusInternalServerError)
			return
		}
		encKey, err = secrets.Encrypt(key, []byte(req.APIKey))
		if err != nil {
			http.Error(w, "encryption error", http.StatusInternalServerError)
			return
		}
	}

	cfg, err := h.Store.CreateAIProviderConfig(r.Context(), store.CreateAIProviderConfigParams{
		TenantID:     tenantID,
		Name:         req.Name,
		ProviderType: req.ProviderType,
		BaseURL:      req.BaseURL,
		Model:        req.Model,
		APIKeyEnc:    encKey,
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			http.Error(w, "provider with that name already exists", http.StatusConflict)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(cfg)
}

// ListAIProviders handles GET /v1/ai-providers.
func (h *AIProviderHandlers) ListAIProviders(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.aiProviderAuthz(w, r)
	if !ok {
		return
	}

	cfgs, err := h.Store.ListAIProviderConfigs(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if cfgs == nil {
		cfgs = []store.AIProviderConfig{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfgs)
}

// SetDefaultAIProvider handles POST /v1/ai-providers/{id}/default.
func (h *AIProviderHandlers) SetDefaultAIProvider(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.aiProviderAuthz(w, r)
	if !ok {
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.Store.SetDefaultAIProviderConfig(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
