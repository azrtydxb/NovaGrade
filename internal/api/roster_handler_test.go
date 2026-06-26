package api_test

// Tests for the roster import endpoint:
//   POST /v1/rosters/import?provider=csv

import (
	"context"
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
	"github.com/azrtydxb/novagrade/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// RosterFakeStore — implements api.RosterStore
// ─────────────────────────────────────────────────────────────────────────────

type RosterFakeStore struct {
	mu       sync.Mutex
	students map[string]store.Student // keyed by email
}

func newRosterFakeStore() *RosterFakeStore {
	return &RosterFakeStore{students: make(map[string]store.Student)}
}

func (f *RosterFakeStore) UpsertStudent(_ context.Context, tenantID uuid.UUID, email, fullName string) (store.Student, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := store.Student{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Email:     email,
		FullName:  fullName,
		CreatedAt: time.Now(),
	}
	f.students[email] = s
	return s, nil
}

func (f *RosterFakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.students)
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildRosterRouter(t *testing.T, h *api.RosterHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/rosters/import", h.ImportRoster)
	return r
}

type rosterImportResponse struct {
	Imported int      `json:"imported"`
	Errors   []string `json:"errors"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestImportRoster_CSV_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newRosterFakeStore()
	h := &api.RosterHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	csvBody := "email,full_name\nalice@example.com,Alice Smith\nbob@example.com,Bob Jones\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import?provider=csv", strings.NewReader(csvBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildRosterRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp rosterImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Imported)
	assert.Empty(t, resp.Errors)
	assert.Equal(t, 2, fakeStore.count())
}

func TestImportRoster_Unauthorized(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	fakeStore := newRosterFakeStore()
	h := &api.RosterHandlers{Store: fakeStore, DeployMode: "onprem"}

	req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import", strings.NewReader("email,full_name\n"))
	rec := httptest.NewRecorder()

	router := buildRosterRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestImportRoster_Forbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newRosterFakeStore()
	h := &api.RosterHandlers{Store: fakeStore, DeployMode: "onprem"}

	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("scanner-key", scanner)

	req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import", strings.NewReader("email,full_name\n"))
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	router := buildRosterRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestImportRoster_EmptyBody verifies a 200 with 0 imported and a parse error
// recorded (the CSV connector reports an empty-file error).
func TestImportRoster_EmptyBody(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newRosterFakeStore()
	h := &api.RosterHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildRosterRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp rosterImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Imported)
	assert.Equal(t, 0, fakeStore.count())
	assert.NotEmpty(t, resp.Errors, "empty CSV should report a parse error")
}
