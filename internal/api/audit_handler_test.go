package api_test

// Tests for GET /v1/audit?submission={id}
// These extend the existing api_test package so they can share FakeStore and
// helper functions defined in api_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// Fake audit service for HTTP handler tests
// ─────────────────────────────────────────────────────────────────────────────

type fakeAuditSvc struct {
	mu     sync.Mutex
	events []store.AuditEvent
}

func (f *fakeAuditSvc) ListBySubmission(_ context.Context, tenantID, submissionID uuid.UUID) ([]store.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []store.AuditEvent
	for _, ev := range f.events {
		if ev.TenantID == tenantID && ev.EntityType == "submission" && ev.EntityID != nil && *ev.EntityID == submissionID {
			result = append(result, ev)
		}
	}
	return result, nil
}

func (f *fakeAuditSvc) seed(tenantID, submissionID uuid.UUID, action string) store.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	sid := submissionID
	ev := store.AuditEvent{
		ID:         uuid.New(),
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &sid,
		Actor:      "teacher@school.io",
		Action:     action,
		Reason:     "test",
		CreatedAt:  time.Now(),
	}
	f.events = append(f.events, ev)
	return ev
}

// buildAuditRouter wires the audit endpoint into a chi router with auth middleware.
func buildAuditRouter(t *testing.T, h *api.AuditHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Get("/v1/audit", h.GetAuditEvents)
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestGetAuditEvents_OK verifies that a teacher can list audit events for a
// submission in their own tenant.
func TestGetAuditEvents_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	subID := uuid.New()

	svc := &fakeAuditSvc{}
	ev := svc.seed(tenantID, subID, "override")

	h := &api.AuditHandlers{Audit: svc, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission="+subID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var events []store.AuditEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.Len(t, events, 1)
	assert.Equal(t, ev.ID, events[0].ID)
	assert.Equal(t, "override", events[0].Action)
}

// TestGetAuditEvents_Empty verifies that a 200 with an empty array is returned
// when there are no audit events for the submission.
func TestGetAuditEvents_Empty(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()

	svc := &fakeAuditSvc{}
	h := &api.AuditHandlers{Audit: svc, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission="+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var events []store.AuditEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	assert.Empty(t, events)
}

// TestGetAuditEvents_CrossTenant verifies tenant isolation: tenant B querying
// a submission owned by tenant A receives an empty list (not a 4xx error).
func TestGetAuditEvents_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	subA := uuid.New()

	svc := &fakeAuditSvc{}
	svc.seed(tenantA, subA, "approve") // event belongs to tenantA

	h := &api.AuditHandlers{Audit: svc, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	// Caller from tenantB queries subA.
	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission="+subA.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// 200 with empty array — the store filters by tenant so no events leak.
	require.Equal(t, http.StatusOK, rec.Code)
	var events []store.AuditEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	assert.Empty(t, events, "cross-tenant query must not return events from another tenant")
}

// TestGetAuditEvents_Unauthorized verifies 401 when no credentials are provided.
func TestGetAuditEvents_Unauthorized(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission="+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestGetAuditEvents_Forbidden verifies 403 when the caller lacks ViewResults.
func TestGetAuditEvents_Forbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	// RoleScanner only has ActionSubmitExam, not ActionViewResults.
	principal := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission="+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestGetAuditEvents_MissingSubmissionParam verifies 400 when the submission
// query parameter is absent.
func TestGetAuditEvents_MissingSubmissionParam(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil) // no ?submission=
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestGetAuditEvents_InvalidSubmissionParam verifies 400 when submission is
// not a valid UUID.
func TestGetAuditEvents_InvalidSubmissionParam(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission=not-a-uuid", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
