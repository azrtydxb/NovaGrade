package api_test

// Tests for the class-results CSV export endpoint:
//   GET /v1/assessment-versions/{avid}/results.csv

import (
	"context"
	"encoding/csv"
	"fmt"
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
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// ClassResultsFakeStore — implements api.ClassResultsStore (embeds ExportStore)
// ─────────────────────────────────────────────────────────────────────────────

type ClassResultsFakeStore struct {
	*ExportFakeStore
	mu         sync.Mutex
	byAVID     map[uuid.UUID][]store.Submission
	students   map[uuid.UUID]store.Student
	avidOwners map[uuid.UUID]uuid.UUID // avid → owning tenantID
}

func newClassResultsFakeStore() *ClassResultsFakeStore {
	return &ClassResultsFakeStore{
		ExportFakeStore: newExportFakeStore(),
		byAVID:          make(map[uuid.UUID][]store.Submission),
		students:        make(map[uuid.UUID]store.Student),
		avidOwners:      make(map[uuid.UUID]uuid.UUID),
	}
}

func (f *ClassResultsFakeStore) ListSubmissionsByAssessmentVersion(_ context.Context, tenantID, avid uuid.UUID) ([]store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Submission
	for _, s := range f.byAVID[avid] {
		if s.TenantID == tenantID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *ClassResultsFakeStore) GetStudent(_ context.Context, tenantID, id uuid.UUID) (store.Student, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.students[id]
	if !ok || s.TenantID != tenantID {
		return store.Student{}, store.ErrNotFound
	}
	return s, nil
}

func (f *ClassResultsFakeStore) GetAssessmentVersionTenantID(_ context.Context, avid uuid.UUID) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tid, ok := f.avidOwners[avid]
	if !ok {
		return uuid.UUID{}, store.ErrNotFound
	}
	return tid, nil
}

// seedGradedSubmission seeds a submission with a graded.v1.json artifact and a
// student, attaches it to avid, and returns it.
func (f *ClassResultsFakeStore) seedGradedSubmission(t *testing.T, fakeObjects *FakeObjectStore, tenantID, avid uuid.UUID, studentName string) store.Submission {
	t.Helper()
	studentID := uuid.New()
	f.mu.Lock()
	f.students[studentID] = store.Student{
		ID:        studentID,
		TenantID:  tenantID,
		Email:     studentName + "@example.com",
		FullName:  studentName,
		CreatedAt: time.Now(),
	}
	f.mu.Unlock()

	sub := store.Submission{
		ID:                  uuid.New(),
		TenantID:            tenantID,
		AssessmentVersionID: &avid,
		StudentID:           &studentID,
		State:               contracts.StateTeacherReview,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	f.ExportFakeStore.mu.Lock()
	f.ExportFakeStore.submissions[sub.ID] = sub
	f.ExportFakeStore.mu.Unlock()

	v1Key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, sub.ID)
	makeGradedPaperWithFlags(t, fakeObjects, v1Key)

	f.mu.Lock()
	f.byAVID[avid] = append(f.byAVID[avid], sub)
	f.avidOwners[avid] = tenantID
	f.mu.Unlock()
	return sub
}

// seedUngradedSubmission seeds a submission attached to avid WITHOUT any graded
// artifact, so it must be skipped by the handler.
func (f *ClassResultsFakeStore) seedUngradedSubmission(tenantID, avid uuid.UUID) store.Submission {
	sub := store.Submission{
		ID:                  uuid.New(),
		TenantID:            tenantID,
		AssessmentVersionID: &avid,
		State:               contracts.StateGrading,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	f.ExportFakeStore.mu.Lock()
	f.ExportFakeStore.submissions[sub.ID] = sub
	f.ExportFakeStore.mu.Unlock()
	f.mu.Lock()
	f.byAVID[avid] = append(f.byAVID[avid], sub)
	f.mu.Unlock()
	return sub
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildClassResultsRouter(t *testing.T, h *api.ClassResultsHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Get("/v1/assessment-versions/{avid}/results.csv", h.ClassResultsCSV)
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestClassResultsCSV_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newClassResultsFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Bob")

	h := &api.ClassResultsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/results.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildClassResultsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "text/csv", rec.Header().Get("Content-Type"))

	r := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := r.ReadAll()
	require.NoError(t, err)

	// Header + (2 students * 2 questions) = 5 rows.
	require.Len(t, rows, 5, "header + 4 question rows")
	assert.Equal(t, []string{"student", "question_no", "max_marks", "awarded", "feedback"}, rows[0])

	// Student names must appear in the first column.
	names := map[string]bool{}
	for _, row := range rows[1:] {
		names[row[0]] = true
	}
	assert.True(t, names["Alice"])
	assert.True(t, names["Bob"])
}

func TestClassResultsCSV_Unauthorized(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	avid := uuid.New()
	fakeStore := newClassResultsFakeStore()
	fakeObjects := NewFakeObjectStore()
	h := &api.ClassResultsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/results.csv", nil)
	rec := httptest.NewRecorder()

	router := buildClassResultsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestClassResultsCSV_NoSubmissions(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newClassResultsFakeStore()
	fakeObjects := NewFakeObjectStore()
	h := &api.ClassResultsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/results.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildClassResultsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	r := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := r.ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 1, "header only")
	assert.Equal(t, []string{"student", "question_no", "max_marks", "awarded", "feedback"}, rows[0])
}

// TestClassResultsCSV_SkipsUngraded verifies that submissions without a graded
// artifact are skipped while graded ones are still exported.
func TestClassResultsCSV_SkipsUngraded(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newClassResultsFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")
	fakeStore.seedUngradedSubmission(tenantID, avid)

	h := &api.ClassResultsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/results.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildClassResultsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	r := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := r.ReadAll()
	require.NoError(t, err)
	// Only Alice's 2 questions + header. Ungraded submission contributes nothing.
	require.Len(t, rows, 3, "header + 2 questions from the single graded submission")
}

// TestClassResults_CrossTenant verifies that a request for an assessment version
// belonging to TenantA, made by TenantB, returns HTTP 404 (no data leakage).
func TestClassResults_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	avid := uuid.New()
	fakeStore := newClassResultsFakeStore()
	fakeObjects := NewFakeObjectStore()

	// Seed a graded submission for TenantA's assessment version.
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantA, avid, "Alice")

	h := &api.ClassResultsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	// Request as TenantB — the store filters by tenant so TenantB sees no submissions.
	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/results.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildClassResultsRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	// The handler must return 404 — the AVID belongs to TenantA, so TenantB must
	// not receive any data (not even an empty CSV) to prevent tenant enumeration.
	require.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant request must return 404, body: %s", rec.Body.String())
}

// TestClassResults_Forbidden verifies that a user without ActionViewResults
// (e.g., scanner role) receives 404 when accessing class results.
func TestClassResults_Forbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newClassResultsFakeStore()
	fakeObjects := NewFakeObjectStore()
	h := &api.ClassResultsHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}

	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("scanner-key", scanner)

	req := httptest.NewRequest(http.MethodGet, "/v1/assessment-versions/"+avid.String()+"/results.csv", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	router := buildClassResultsRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	// Scanner lacks ActionViewResults → handler returns 404 (not 403, avoids enumeration).
	assert.Equal(t, http.StatusNotFound, rec.Code, "scanner must not access class results")
}
