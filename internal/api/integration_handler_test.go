package api_test

// Tests for the integration connection management endpoints:
//   POST   /v1/integrations
//   GET    /v1/integrations
//   DELETE /v1/integrations/{id}

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/integration"
	integrationcsv "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// testEncKey is a base64-std-encoded 32-byte key for INTEGRATION_ENC_KEY.
var testEncKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

// ─────────────────────────────────────────────────────────────────────────────
// IntegrationFakeStore — implements api.IntegrationStore
// ─────────────────────────────────────────────────────────────────────────────

type IntegrationFakeStore struct {
	mu          sync.Mutex
	connections map[uuid.UUID]integration.Connection
	// rawCreds records the encrypted bytes passed to UpsertConnection per connection.
	rawCreds map[uuid.UUID][]byte
}

func newIntegrationFakeStore() *IntegrationFakeStore {
	return &IntegrationFakeStore{
		connections: make(map[uuid.UUID]integration.Connection),
		rawCreds:    make(map[uuid.UUID][]byte),
	}
}

func (f *IntegrationFakeStore) UpsertConnection(_ context.Context, p store.UpsertConnectionParams) (integration.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	conn := integration.Connection{
		ID:        uuid.New(),
		TenantID:  p.TenantID,
		Category:  p.Category,
		Provider:  p.Provider,
		Status:    "active",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	f.connections[conn.ID] = conn
	f.rawCreds[conn.ID] = p.Credentials
	return conn, nil
}

func (f *IntegrationFakeStore) ListConnections(_ context.Context, tenantID uuid.UUID) ([]integration.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []integration.Connection
	for _, c := range f.connections {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *IntegrationFakeStore) DeleteConnection(_ context.Context, tenantID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.connections[id]
	if !ok || c.TenantID != tenantID {
		return store.ErrNotFound
	}
	delete(f.connections, id)
	delete(f.rawCreds, id)
	return nil
}

// lastCreds returns the encrypted bytes recorded for the only stored connection.
func (f *IntegrationFakeStore) credsFor(id uuid.UUID) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rawCreds[id]
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildIntegrationRouter(t *testing.T, h *api.IntegrationHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/integrations", h.UpsertIntegration)
	r.Get("/v1/integrations", h.ListIntegrations)
	r.Delete("/v1/integrations/{id}", h.DeleteIntegration)
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestUpsertIntegration_Created(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()
	reg := integration.NewRegistry()
	integrationcsv.Register(reg)
	h := &api.IntegrationHandlers{Store: fakeStore, Registry: reg, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	bodyJSON := `{"category":"roster","provider":"csv","config":{"k":"v"},"credentials":{"token":"secret"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations", strings.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var conn integration.Connection
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &conn))
	assert.Equal(t, integration.CategoryRoster, conn.Category)
	assert.Equal(t, "csv", conn.Provider)
	assert.Equal(t, tenantID, conn.TenantID)

	// Stored credentials must be encrypted (non-empty, not the plaintext).
	stored := fakeStore.credsFor(conn.ID)
	require.NotEmpty(t, stored)
	assert.NotContains(t, string(stored), "secret")
}

func TestUpsertIntegration_Unauthorized(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	fakeStore := newIntegrationFakeStore()
	h := &api.IntegrationHandlers{Store: fakeStore, DeployMode: "onprem"}

	req := httptest.NewRequest(http.MethodPost, "/v1/integrations", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestUpsertIntegration_Forbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()
	h := &api.IntegrationHandlers{Store: fakeStore, DeployMode: "onprem"}

	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("scanner-key", scanner)

	req := httptest.NewRequest(http.MethodPost, "/v1/integrations", strings.NewReader(`{"category":"roster","provider":"csv"}`))
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListIntegrations_Empty(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()
	h := &api.IntegrationHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/integrations", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestDeleteIntegration_NotFound(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()
	h := &api.IntegrationHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodDelete, "/v1/integrations/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteIntegration_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()
	h := &api.IntegrationHandlers{Store: fakeStore, DeployMode: "onprem"}

	conn, err := fakeStore.UpsertConnection(context.Background(), store.UpsertConnectionParams{
		TenantID: tenantID,
		Category: integration.CategoryRoster,
		Provider: "csv",
	})
	require.NoError(t, err)

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodDelete, "/v1/integrations/"+conn.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestUpsertIntegration_CredentialsEncrypted verifies the stored bytes differ
// from the plaintext and decrypt back to the original credentials JSON.
func TestUpsertIntegration_CredentialsEncrypted(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()
	reg := integration.NewRegistry()
	integrationcsv.Register(reg)
	h := &api.IntegrationHandlers{Store: fakeStore, Registry: reg, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	creds := `{"api_key":"super-secret-value"}`
	bodyJSON := `{"category":"sis","provider":"csv","credentials":` + creds + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations", strings.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var conn integration.Connection
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &conn))

	stored := fakeStore.credsFor(conn.ID)
	require.NotEmpty(t, stored)
	assert.NotEqual(t, creds, string(stored), "stored bytes must not be plaintext")

	// Round-trip decrypt.
	key, err := secrets.KeyFromEnv("INTEGRATION_ENC_KEY")
	require.NoError(t, err)
	plain, err := secrets.Decrypt(key, stored)
	require.NoError(t, err)
	assert.JSONEq(t, creds, string(plain))

	// The response body must never contain the secret.
	assert.NotContains(t, rec.Body.String(), "super-secret-value")
}

// TestUpsertIntegration_UnknownProvider verifies that POST /v1/integrations
// returns HTTP 400 when the (category, provider) pair is not registered.
func TestUpsertIntegration_UnknownProvider(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()

	// Build a registry with only the CSV connectors registered.
	reg := integration.NewRegistry()
	integrationcsv.Register(reg)

	h := &api.IntegrationHandlers{Store: fakeStore, Registry: reg, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	// "unknown-provider" is not registered in the registry.
	bodyJSON := `{"category":"roster","provider":"unknown-provider"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations", strings.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unknown connector")
}

// TestUpsertIntegration_NilRegistry verifies that POST /v1/integrations returns
// HTTP 500 when the handler's Registry is nil (server misconfiguration).
func TestUpsertIntegration_NilRegistry(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)

	tenantID := uuid.New()
	fakeStore := newIntegrationFakeStore()
	// Intentionally leave Registry nil to simulate misconfiguration.
	h := &api.IntegrationHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	bodyJSON := `{"category":"roster","provider":"csv"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations", strings.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, "nil registry must return 500, got body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "misconfiguration")
}

// TestDeleteIntegration_CrossTenant verifies that a connection created for
// TenantA cannot be deleted by TenantB (returns HTTP 404).
func TestDeleteIntegration_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := newIntegrationFakeStore()
	h := &api.IntegrationHandlers{Store: fakeStore, DeployMode: "onprem"}

	// Create a connection belonging to TenantA.
	conn, err := fakeStore.UpsertConnection(context.Background(), store.UpsertConnectionParams{
		TenantID: tenantA,
		Category: integration.CategoryRoster,
		Provider: "csv",
	})
	require.NoError(t, err)

	// TenantB attempts to delete TenantA's connection.
	principal := auth.Principal{
		ID:       "admin-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodDelete, "/v1/integrations/"+conn.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildIntegrationRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant delete must return 404")
}
