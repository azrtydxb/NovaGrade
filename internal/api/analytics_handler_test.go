package api_test

// Tests for:
//   GET /v1/assessment-versions/{avid}/analytics
//   GET /v1/assessment-versions/{avid}/override-stats
//
// TDD RED phase — written before implementation.

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
// AnalyticsFakeStore — extends ClassResultsFakeStore with ListAuditEventsBySubmission
// ─────────────────────────────────────────────────────────────────────────────

type AnalyticsFakeStore struct {
	*ClassResultsFakeStore
	mu          sync.Mutex
	auditEvents map[uuid.UUID][]store.AuditEvent // submissionID → events
}

func newAnalyticsFakeStore() *AnalyticsFakeStore {
	return &AnalyticsFakeStore{
		ClassResultsFakeStore: newClassResultsFakeStore(),
		auditEvents:           make(map[uuid.UUID][]store.AuditEvent),
	}
}

func (f *AnalyticsFakeStore) ListAuditEventsBySubmission(_ context.Context, _, submissionID uuid.UUID) ([]store.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auditEvents[submissionID], nil
}

// seedAuditOverride adds an override_question audit event for a submission.
func (f *AnalyticsFakeStore) seedAuditOverride(submissionID uuid.UUID, qno string, oldMarks, newMarks float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	oldVal, _ := json.Marshal(map[string]interface{}{"question_no": qno, "awarded_marks": oldMarks})
	newVal, _ := json.Marshal(map[string]interface{}{"question_no": qno, "awarded_marks": newMarks})
	f.auditEvents[submissionID] = append(f.auditEvents[submissionID], store.AuditEvent{
		ID:         uuid.New(),
		TenantID:   uuid.UUID{},
		EntityType: "submission",
		EntityID:   &submissionID,
		Actor:      "teacher-1",
		Action:     "override_question",
		OldValue:   oldVal,
		NewValue:   newVal,
		CreatedAt:  time.Now(),
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildAnalyticsRouter(t *testing.T, h *api.AnalyticsHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Get("/v1/assessment-versions/{avid}/analytics", h.GetAnalytics)
	r.Get("/v1/assessment-versions/{avid}/override-stats", h.GetOverrideStats)
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Analytics endpoint tests
// ─────────────────────────────────────────────────────────────────────────────

// TestGetAnalytics_OK: 3 graded + 1 ungraded submissions → correct JSON response.
func TestGetAnalytics_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newAnalyticsFakeStore()
	fakeObjects := NewFakeObjectStore()

	// Seed 3 graded submissions.
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Bob")
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Carol")
	// 1 ungraded — should be counted in total_count but not in graded_count.
	fakeStore.seedUngradedSubmission(tenantID, avid)

	h := &api.AnalyticsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/analytics", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildAnalyticsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp struct {
		GradedCount     int                      `json:"graded_count"`
		TotalCount      int                      `json:"total_count"`
		ItemAnalysis    []map[string]interface{} `json:"item_analysis"`
		Distribution    map[string]interface{}   `json:"distribution"`
		Hardest         []map[string]interface{} `json:"hardest"`
		FlagFrequencies map[string]int           `json:"flag_frequencies"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, 3, resp.GradedCount, "three graded submissions")
	assert.Equal(t, 4, resp.TotalCount, "four total (3 graded + 1 ungraded)")
	assert.NotEmpty(t, resp.ItemAnalysis, "item_analysis must be populated")
	assert.NotNil(t, resp.Distribution, "distribution must be present")
	distCount, _ := resp.Distribution["count"].(float64)
	assert.Equal(t, float64(3), distCount, "distribution.count must equal graded_count")
	assert.NotNil(t, resp.FlagFrequencies, "flag_frequencies must be non-nil")
}

// TestGetAnalytics_CrossTenant: teacher from tenantB requests avid owned by tenantA → 404.
func TestGetAnalytics_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	avid := uuid.New()
	fakeStore := newAnalyticsFakeStore()
	fakeObjects := NewFakeObjectStore()

	// Seed a graded submission for tenantA's assessment version.
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantA, avid, "Alice")

	h := &api.AnalyticsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	// Request as tenantB.
	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/analytics", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildAnalyticsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant must return 404, body: %s", rec.Body.String())
}

// TestGetAnalytics_ScannerForbidden: scanner role → 404 (lacks ActionViewResults).
func TestGetAnalytics_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newAnalyticsFakeStore()
	fakeObjects := NewFakeObjectStore()

	h := &api.AnalyticsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("scanner-key", scanner)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/analytics", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	router := buildAnalyticsRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "scanner must not access analytics")
}

// ─────────────────────────────────────────────────────────────────────────────
// Override-stats endpoint tests
// ─────────────────────────────────────────────────────────────────────────────

// TestGetOverrideStats_OK: 3 submissions, 2 have override audit events.
// total_graded_questions = 3 subs × 2 questions = 6
// overridden_questions = 2
// override_rate = 2/6
// mean_abs_delta = (|8-5| + |7-3|) / 2 = (3 + 4) / 2 = 3.5
func TestGetOverrideStats_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newAnalyticsFakeStore()
	fakeObjects := NewFakeObjectStore()

	// 3 graded submissions.
	sub1 := fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")
	sub2 := fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Bob")
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Carol") // no override

	// Sub1: override question "1" from 5 → 8 (delta = 3)
	fakeStore.seedAuditOverride(sub1.ID, "1", 5.0, 8.0)
	// Sub2: override question "1" from 3 → 7 (delta = 4)
	fakeStore.seedAuditOverride(sub2.ID, "1", 3.0, 7.0)

	h := &api.AnalyticsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/override-stats", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildAnalyticsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp struct {
		TotalGradedQuestions int     `json:"total_graded_questions"`
		OverriddenQuestions  int     `json:"overridden_questions"`
		OverrideRate         float64 `json:"override_rate"`
		MeanAbsDelta         float64 `json:"mean_abs_delta"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// makeGradedPaperWithFlags produces 2 questions per paper → 3 × 2 = 6 total.
	assert.Equal(t, 6, resp.TotalGradedQuestions, "total graded questions")
	assert.Equal(t, 2, resp.OverriddenQuestions, "two overridden questions")
	assert.InDelta(t, float64(2)/float64(6), resp.OverrideRate, 1e-9, "override_rate")
	assert.InDelta(t, 3.5, resp.MeanAbsDelta, 1e-9, "mean_abs_delta = (3+4)/2")
}

// TestGetOverrideStats_ZeroOverrides: no audit events → rate=0, delta=0.
func TestGetOverrideStats_ZeroOverrides(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newAnalyticsFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Bob")
	// No audit events seeded.

	h := &api.AnalyticsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/override-stats", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildAnalyticsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		TotalGradedQuestions int     `json:"total_graded_questions"`
		OverriddenQuestions  int     `json:"overridden_questions"`
		OverrideRate         float64 `json:"override_rate"`
		MeanAbsDelta         float64 `json:"mean_abs_delta"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, 4, resp.TotalGradedQuestions, "2 subs × 2 questions")
	assert.Equal(t, 0, resp.OverriddenQuestions)
	assert.Equal(t, float64(0), resp.OverrideRate)
	assert.Equal(t, float64(0), resp.MeanAbsDelta)
}

// TestGetOverrideStats_CrossTenant: cross-tenant → 404.
func TestGetOverrideStats_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	avid := uuid.New()
	fakeStore := newAnalyticsFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantA, avid, "Alice")

	h := &api.AnalyticsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/override-stats", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildAnalyticsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant must return 404, body: %s", rec.Body.String())
}

