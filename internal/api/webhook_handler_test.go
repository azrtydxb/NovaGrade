package api_test

// Tests for the webhook subscription management endpoints:
//   POST   /v1/webhooks         — create subscription (secret returned once)
//   GET    /v1/webhooks         — list subscriptions (no secret)
//   DELETE /v1/webhooks/{id}    — delete subscription

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
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// WebhookFakeStore — implements api.WebhookStore
// ─────────────────────────────────────────────────────────────────────────────

type WebhookFakeStore struct {
	mu            sync.Mutex
	subs          map[uuid.UUID]store.WebhookSubscription
	encryptedSecs map[uuid.UUID][]byte // encrypted secret per sub ID
}

func newWebhookFakeStore() *WebhookFakeStore {
	return &WebhookFakeStore{
		subs:          make(map[uuid.UUID]store.WebhookSubscription),
		encryptedSecs: make(map[uuid.UUID][]byte),
	}
}

func (f *WebhookFakeStore) CreateWebhookSubscription(_ context.Context, tenant uuid.UUID, event, url string, encryptedSecret []byte) (store.WebhookSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := store.WebhookSubscription{
		ID:        uuid.New(),
		TenantID:  tenant,
		Event:     event,
		URL:       url,
		Active:    true,
		CreatedAt: time.Now(),
	}
	f.subs[sub.ID] = sub
	f.encryptedSecs[sub.ID] = encryptedSecret
	return sub, nil
}

func (f *WebhookFakeStore) ListWebhookSubscriptions(_ context.Context, tenant uuid.UUID) ([]store.WebhookSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.WebhookSubscription
	for _, s := range f.subs {
		if s.TenantID == tenant {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *WebhookFakeStore) DeleteWebhookSubscription(_ context.Context, tenant, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[id]
	if !ok || s.TenantID != tenant {
		return store.ErrNotFound
	}
	delete(f.subs, id)
	delete(f.encryptedSecs, id)
	return nil
}

// encryptedSecFor returns the encrypted secret stored for subID (for verification in tests).
func (f *WebhookFakeStore) encryptedSecFor(id uuid.UUID) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.encryptedSecs[id]
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildWebhookRouter(t *testing.T, h *api.WebhookHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/webhooks", h.Create)
	r.Get("/v1/webhooks", h.List)
	r.Delete("/v1/webhooks/{id}", h.Delete)
	return r
}

func webhookTestKey(t *testing.T) []byte {
	t.Helper()
	t.Setenv("INTEGRATION_ENC_KEY", testEncKey)
	key, err := secrets.KeyFromEnv("INTEGRATION_ENC_KEY")
	require.NoError(t, err)
	return key
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateWebhook_Created verifies POST /v1/webhooks returns 201 with
// {id, event, url, secret (non-empty), note}. The returned secret decrypts
// correctly from the stored encrypted bytes.
func TestCreateWebhook_Created(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	tenantID := uuid.New()
	fakeStore := newWebhookFakeStore()
	encKey := webhookTestKey(t)

	h := &api.WebhookHandlers{Store: fakeStore, EncKey: encKey, DeployMode: "onprem"}
	tok := issueToken(t, adminPrincipal(tenantID.String()))

	body := `{"event":"published","url":"https://example.com/hook"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildWebhookRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Must have required fields.
	assert.NotEmpty(t, resp["id"])
	assert.Equal(t, "published", resp["event"])
	assert.Equal(t, "https://example.com/hook", resp["url"])
	assert.NotEmpty(t, resp["secret"], "secret must be returned once")
	assert.NotEmpty(t, resp["note"])

	// Returned secret must be base64 plaintext that decrypts from stored bytes.
	returnedSecB64, ok := resp["secret"].(string)
	require.True(t, ok, "secret must be a string")
	returnedPlain, err := base64.StdEncoding.DecodeString(returnedSecB64)
	require.NoError(t, err, "secret must be valid base64")

	subID, err := uuid.Parse(resp["id"].(string))
	require.NoError(t, err)
	storedEnc := fakeStore.encryptedSecFor(subID)
	require.NotEmpty(t, storedEnc, "encrypted secret must be stored")

	decrypted, err := secrets.Decrypt(encKey, storedEnc)
	require.NoError(t, err)
	assert.Equal(t, returnedPlain, decrypted, "stored secret must round-trip via decrypt")

	// Stored bytes must differ from plaintext (they are encrypted).
	assert.NotEqual(t, string(returnedPlain), string(storedEnc))
}

// TestCreateWebhook_MissingFields verifies that POST /v1/webhooks returns 400
// when event or url is missing.
func TestCreateWebhook_MissingFields(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	tenantID := uuid.New()
	fakeStore := newWebhookFakeStore()
	encKey := webhookTestKey(t)

	h := &api.WebhookHandlers{Store: fakeStore, EncKey: encKey, DeployMode: "onprem"}
	tok := issueToken(t, adminPrincipal(tenantID.String()))

	for _, body := range []string{
		`{"event":"published"}`,                   // missing url
		`{"url":"https://example.com"}`,           // missing event
		`{}`,                                       // both missing
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		buildWebhookRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s resp=%s", body, rec.Body.String())
	}
}

// TestListWebhooks_NoSecretField verifies GET /v1/webhooks returns 200 JSON
// array with no "secret" field in any element.
func TestListWebhooks_NoSecretField(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	tenantID := uuid.New()
	fakeStore := newWebhookFakeStore()
	encKey := webhookTestKey(t)

	// Pre-populate store directly.
	_, err := fakeStore.CreateWebhookSubscription(context.Background(), tenantID, "published", "https://a.com", []byte("fake-enc"))
	require.NoError(t, err)

	h := &api.WebhookHandlers{Store: fakeStore, EncKey: encKey, DeployMode: "onprem"}
	tok := issueToken(t, adminPrincipal(tenantID.String()))

	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildWebhookRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var arr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	require.Len(t, arr, 1)

	// Must not have secret field.
	_, hasSecret := arr[0]["secret"]
	assert.False(t, hasSecret, "list response must not include secret field")

	// Must have these fields.
	assert.NotEmpty(t, arr[0]["id"])
	assert.Equal(t, "published", arr[0]["event"])
	assert.Equal(t, "https://a.com", arr[0]["url"])
}

// TestDeleteWebhook_OK verifies DELETE /v1/webhooks/{id} returns 204 on success.
func TestDeleteWebhook_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	tenantID := uuid.New()
	fakeStore := newWebhookFakeStore()
	encKey := webhookTestKey(t)

	sub, err := fakeStore.CreateWebhookSubscription(context.Background(), tenantID, "published", "https://a.com", []byte("enc"))
	require.NoError(t, err)

	h := &api.WebhookHandlers{Store: fakeStore, EncKey: encKey, DeployMode: "onprem"}
	tok := issueToken(t, adminPrincipal(tenantID.String()))

	req := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/"+sub.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildWebhookRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestDeleteWebhook_NotFound verifies DELETE /v1/webhooks/{id} returns 404 for nonexistent id.
func TestDeleteWebhook_NotFound(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	tenantID := uuid.New()
	fakeStore := newWebhookFakeStore()
	encKey := webhookTestKey(t)

	h := &api.WebhookHandlers{Store: fakeStore, EncKey: encKey, DeployMode: "onprem"}
	tok := issueToken(t, adminPrincipal(tenantID.String()))

	req := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildWebhookRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestDeleteWebhook_CrossTenant verifies that another tenant's webhook returns 404.
func TestDeleteWebhook_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := newWebhookFakeStore()
	encKey := webhookTestKey(t)

	subA, err := fakeStore.CreateWebhookSubscription(context.Background(), tenantA, "published", "https://a.com", []byte("enc"))
	require.NoError(t, err)

	h := &api.WebhookHandlers{Store: fakeStore, EncKey: encKey, DeployMode: "onprem"}
	tok := issueToken(t, auth.Principal{
		ID:       "admin-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	})

	req := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/"+subA.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildWebhookRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant delete must return 404")
}

// TestWebhook_NoEditTunables verifies that a role without EditTunables returns 404.
func TestWebhook_NoEditTunables(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")
	tenantID := uuid.New()
	fakeStore := newWebhookFakeStore()
	encKey := webhookTestKey(t)

	h := &api.WebhookHandlers{Store: fakeStore, EncKey: encKey, DeployMode: "onprem"}

	// RoleScanner does not have ActionEditTunables.
	resolver := auth.NewAPIKeyResolver()
	resolver.Register("scanner-key", auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	buildWebhookRouter(t, h, resolver).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "scanner role must not have access")
}
