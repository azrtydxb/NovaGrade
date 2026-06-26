package api_test

// Tests for the per-tenant AI provider config endpoints:
//   POST /v1/ai-providers
//   GET  /v1/ai-providers
//   POST /v1/ai-providers/{id}/default

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// AIProviderFakeStore — implements api.AIProviderStore
// ─────────────────────────────────────────────────────────────────────────────

type AIProviderFakeStore struct {
	mu      sync.Mutex
	configs map[uuid.UUID]store.AIProviderConfig
	// rawKeys records the encrypted bytes passed to CreateAIProviderConfig.
	rawKeys map[uuid.UUID][]byte
	// notFoundOnSetDefault forces SetDefault to return ErrNotFound (cross-tenant sim).
	notFoundOnSetDefault bool
}

func newAIProviderFakeStore() *AIProviderFakeStore {
	return &AIProviderFakeStore{
		configs: make(map[uuid.UUID]store.AIProviderConfig),
		rawKeys: make(map[uuid.UUID][]byte),
	}
}

func (f *AIProviderFakeStore) CreateAIProviderConfig(_ context.Context, p store.CreateAIProviderConfigParams) (store.AIProviderConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg := store.AIProviderConfig{
		ID:           uuid.New(),
		TenantID:     p.TenantID,
		Name:         p.Name,
		ProviderType: p.ProviderType,
		BaseURL:      p.BaseURL,
		Model:        p.Model,
	}
	f.configs[cfg.ID] = cfg
	f.rawKeys[cfg.ID] = p.APIKeyEnc
	return cfg, nil
}

func (f *AIProviderFakeStore) ListAIProviderConfigs(_ context.Context, tenantID uuid.UUID) ([]store.AIProviderConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AIProviderConfig
	for _, c := range f.configs {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *AIProviderFakeStore) GetDefaultAIProviderConfigWithKey(_ context.Context, tenantID uuid.UUID) (store.AIProviderConfig, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.configs {
		if c.TenantID == tenantID && c.IsDefault {
			return c, f.rawKeys[c.ID], nil
		}
	}
	return store.AIProviderConfig{}, nil, store.ErrNotFound
}

func (f *AIProviderFakeStore) SetDefaultAIProviderConfig(_ context.Context, tenantID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notFoundOnSetDefault {
		return store.ErrNotFound
	}
	c, ok := f.configs[id]
	if !ok || c.TenantID != tenantID {
		return store.ErrNotFound
	}
	c.IsDefault = true
	f.configs[id] = c
	return nil
}

func (f *AIProviderFakeStore) keyFor(id uuid.UUID) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rawKeys[id]
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildAIProviderRouter(t *testing.T, h *api.AIProviderHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/ai-providers", h.CreateAIProvider)
	r.Get("/v1/ai-providers", h.ListAIProviders)
	r.Post("/v1/ai-providers/{id}/default", h.SetDefaultAIProvider)
	return r
}

func adminToken(t *testing.T, tenantID uuid.UUID) string {
	t.Helper()
	return issueToken(t, auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateAIProvider_EncryptsKey verifies the stored bytes are not the
// plaintext and decrypt back to the original api_key.
func TestCreateAIProvider_EncryptsKey(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)

	tenantID := uuid.New()
	fakeStore := newAIProviderFakeStore()
	h := &api.AIProviderHandlers{Store: fakeStore, DeployMode: "onprem"}

	bodyJSON := `{"name":"primary","provider_type":"openai","base_url":"https://api.openai.com","model":"gpt-4o","api_key":"plaintext-key"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/ai-providers", strings.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer "+adminToken(t, tenantID))
	rec := httptest.NewRecorder()

	buildAIProviderRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var cfg store.AIProviderConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))

	stored := fakeStore.keyFor(cfg.ID)
	require.NotEmpty(t, stored)
	assert.NotEqual(t, "plaintext-key", string(stored), "stored bytes must be ciphertext")
	assert.NotContains(t, string(stored), "plaintext-key")

	// Round-trip decrypt must recover the original key.
	key, err := secrets.KeyFromEnv("INTEGRATION_ENC_KEY")
	require.NoError(t, err)
	plain, err := secrets.Decrypt(key, stored)
	require.NoError(t, err)
	assert.Equal(t, "plaintext-key", string(plain))
}

// TestCreateAIProvider_NoKeyInResponse verifies the create response carries no
// api_key or api_key_enc field.
func TestCreateAIProvider_NoKeyInResponse(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)

	tenantID := uuid.New()
	fakeStore := newAIProviderFakeStore()
	h := &api.AIProviderHandlers{Store: fakeStore, DeployMode: "onprem"}

	bodyJSON := `{"name":"primary","provider_type":"vllm","base_url":"http://localhost:8000","model":"default","api_key":"super-secret-value"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/ai-providers", strings.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer "+adminToken(t, tenantID))
	rec := httptest.NewRecorder()

	buildAIProviderRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	body := rec.Body.String()
	assert.NotContains(t, body, "api_key")
	assert.NotContains(t, body, "api_key_enc")
	assert.NotContains(t, body, "super-secret-value")
}

// TestListAIProviders_NoKeys verifies the list response carries no key fields.
func TestListAIProviders_NoKeys(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAIProviderFakeStore()
	_, _ = fakeStore.CreateAIProviderConfig(context.Background(), store.CreateAIProviderConfigParams{
		TenantID: tenantID, Name: "p1", ProviderType: "openai",
		BaseURL: "https://api.openai.com", Model: "gpt-4o", APIKeyEnc: []byte("ciphertext"),
	})
	h := &api.AIProviderHandlers{Store: fakeStore, DeployMode: "onprem"}

	req := httptest.NewRequest(http.MethodGet, "/v1/ai-providers", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken(t, tenantID))
	rec := httptest.NewRecorder()

	buildAIProviderRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.NotContains(t, body, "api_key")
	assert.NotContains(t, body, "ciphertext")
	assert.Contains(t, body, "p1")
}

// TestSetDefault_OK verifies a tenant can set its own provider as default.
func TestSetDefault_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAIProviderFakeStore()
	cfg, _ := fakeStore.CreateAIProviderConfig(context.Background(), store.CreateAIProviderConfigParams{
		TenantID: tenantID, Name: "p1", ProviderType: "openai",
		BaseURL: "https://api.openai.com", Model: "gpt-4o",
	})
	h := &api.AIProviderHandlers{Store: fakeStore, DeployMode: "onprem"}

	req := httptest.NewRequest(http.MethodPost, "/v1/ai-providers/"+cfg.ID.String()+"/default", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken(t, tenantID))
	rec := httptest.NewRecorder()

	buildAIProviderRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// TestAIProvider_MissingEditTunables_404 verifies a principal without
// ActionEditTunables gets 404 (404-on-denial).
func TestAIProvider_MissingEditTunables_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAIProviderFakeStore()
	h := &api.AIProviderHandlers{Store: fakeStore, DeployMode: "onprem"}

	resolver := auth.NewAPIKeyResolver()
	resolver.Register("scanner-key", auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/ai-providers", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	buildAIProviderRouter(t, h, resolver).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAIProvider_CrossTenant_404 verifies set-default against an id owned by
// another tenant returns 404 (ErrNotFound from the store).
func TestAIProvider_CrossTenant_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := newAIProviderFakeStore()
	cfg, _ := fakeStore.CreateAIProviderConfig(context.Background(), store.CreateAIProviderConfigParams{
		TenantID: tenantA, Name: "p1", ProviderType: "openai",
		BaseURL: "https://api.openai.com", Model: "gpt-4o",
	})
	h := &api.AIProviderHandlers{Store: fakeStore, DeployMode: "onprem"}

	// TenantB attempts to set TenantA's config as default.
	req := httptest.NewRequest(http.MethodPost, "/v1/ai-providers/"+cfg.ID.String()+"/default", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken(t, tenantB))
	rec := httptest.NewRecorder()

	buildAIProviderRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant set-default must return 404")
}
