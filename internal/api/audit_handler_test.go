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

// ─────────────────────────────────────────────────────────────────────────────
// Fake submission lookup for HTTP handler tests
// ─────────────────────────────────────────────────────────────────────────────

// fakeSubmissionLookup implements api.SubmissionLookup backed by an in-memory map.
type fakeSubmissionLookup struct {
	mu          sync.Mutex
	submissions map[uuid.UUID]store.Submission
}

func newFakeSubmissionLookup() *fakeSubmissionLookup {
	return &fakeSubmissionLookup{
		submissions: make(map[uuid.UUID]store.Submission),
	}
}

func (f *fakeSubmissionLookup) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.submissions[id]
	if !ok {
		return store.Submission{}, store.ErrNotFound
	}
	return sub, nil
}

func (f *fakeSubmissionLookup) seed(tenantID, submissionID uuid.UUID) store.Submission {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := store.Submission{
		ID:       submissionID,
		TenantID: tenantID,
	}
	f.submissions[submissionID] = sub
	return sub
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

	subLookup := newFakeSubmissionLookup()
	subLookup.seed(tenantID, subID)

	h := &api.AuditHandlers{Audit: svc, Store: subLookup, DeployMode: "onprem"}
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
// when the submission exists in the caller's tenant but has no audit events.
func TestGetAuditEvents_Empty(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	subID := uuid.New()

	svc := &fakeAuditSvc{}

	subLookup := newFakeSubmissionLookup()
	subLookup.seed(tenantID, subID) // submission exists, no audit events

	h := &api.AuditHandlers{Audit: svc, Store: subLookup, DeployMode: "onprem"}
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
	assert.Empty(t, events)
}

// TestGetAuditEvents_CrossTenant verifies tenant isolation: tenant B querying
// a submission owned by tenant A receives 404 (not 200+[]) — the same
// no-enumeration behaviour as GetSubmission.
func TestGetAuditEvents_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	subA := uuid.New()

	svc := &fakeAuditSvc{}
	svc.seed(tenantA, subA, "approve") // event belongs to tenantA

	subLookup := newFakeSubmissionLookup()
	subLookup.seed(tenantA, subA) // submission belongs to tenantA

	h := &api.AuditHandlers{Audit: svc, Store: subLookup, DeployMode: "onprem"}
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

	// 404 — cross-tenant request must not reveal that the submission exists or
	// return any events. Matches the no-enumeration convention of GetSubmission.
	assert.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant query must return 404, not 200+[]")
}

// TestGetAuditEvents_Unauthorized verifies 401 when no credentials are provided.
func TestGetAuditEvents_Unauthorized(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, Store: newFakeSubmissionLookup(), DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission="+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestGetAuditEvents_Forbidden verifies that a caller who lacks ViewResults
// receives 404 (not 403) — consistent with the no-enumeration convention used
// by GetSubmission when RBAC denies access to a resource.
func TestGetAuditEvents_Forbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	subID := uuid.New()

	subLookup := newFakeSubmissionLookup()
	subLookup.seed(tenantID, subID) // submission must exist so RBAC check is reached

	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, Store: subLookup, DeployMode: "onprem"}
	router := buildAuditRouter(t, h, auth.NewAPIKeyResolver())

	// RoleScanner only has ActionSubmitExam, not ActionViewResults.
	principal := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit?submission="+subID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// 404 — RBAC denial surfaces as not-found (no-enumeration), matching GetSubmission.
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestGetAuditEvents_MissingSubmissionParam verifies 400 when the submission
// query parameter is absent.
func TestGetAuditEvents_MissingSubmissionParam(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, Store: newFakeSubmissionLookup(), DeployMode: "onprem"}
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
	h := &api.AuditHandlers{Audit: &fakeAuditSvc{}, Store: newFakeSubmissionLookup(), DeployMode: "onprem"}
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
