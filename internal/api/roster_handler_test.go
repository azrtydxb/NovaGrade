package api_test

// Tests for the roster import endpoint:
//   POST /v1/rosters/import?provider=csv

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
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

// makeRosterMultipart builds a multipart/form-data body with the CSV content in
// the "file" field and returns the body buffer and Content-Type header value.
func makeRosterMultipart(t *testing.T, csvContent string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "roster.csv")
	require.NoError(t, err)
	_, err = fw.Write([]byte(csvContent))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return body, mw.FormDataContentType()
}

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
	Skipped  int      `json:"skipped"`
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
	body, ct := makeRosterMultipart(t, csvBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import?provider=csv", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	router := buildRosterRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp rosterImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Imported)
	assert.Equal(t, 0, resp.Skipped)
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

	body, ct := makeRosterMultipart(t, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", ct)
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

// TestImportRoster_Idempotent verifies that sending the same CSV twice succeeds
// both times (upsert is idempotent) and each response reports imported == 2.
func TestImportRoster_Idempotent(t *testing.T) {
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
	router := buildRosterRouter(t, h, auth.NewAPIKeyResolver())

	for i := 0; i < 2; i++ {
		body, ct := makeRosterMultipart(t, csvBody)
		req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import?provider=csv", body)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "attempt %d body: %s", i+1, rec.Body.String())

		var resp rosterImportResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, 2, resp.Imported, "attempt %d: imported should be exactly 2", i+1)
		assert.Empty(t, resp.Errors, "attempt %d: no errors expected for valid CSV", i+1)
	}
}

// TestImportRoster_MalformedRows verifies that a CSV with one valid row and one
// malformed row (missing email) returns imported=1 and len(errors)>=1.
func TestImportRoster_MalformedRows(t *testing.T) {
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

	// Valid header + 1 valid row + 1 malformed row (empty email field).
	csvBody := "email,full_name\nalice@example.com,Alice Smith\n,Bob Jones\n"
	body, ct := makeRosterMultipart(t, csvBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/rosters/import?provider=csv", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	router := buildRosterRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp rosterImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Imported, "only the valid row should be imported")
	assert.Equal(t, 1, resp.Skipped, "skipped count must equal number of malformed rows (via RosterImportError)")
	assert.GreaterOrEqual(t, len(resp.Errors), 1, "malformed row should be reported as an error")
}
