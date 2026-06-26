package api_test

// Tests for:
//   GET /v1/submissions/{id}/export.csv
//
// TDD RED phase — written before the handler is implemented.
// Follows the pattern established by approval_handler_test.go.

import (
	"context"
	"encoding/csv"
	"encoding/json"
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
// ExportFakeStore — implements api.ExportStore
// ─────────────────────────────────────────────────────────────────────────────

type ExportFakeStore struct {
	mu                   sync.Mutex
	submissions          map[uuid.UUID]store.Submission
	reviews              []store.TeacherReview
	preloadedFinalGrades map[uuid.UUID]store.FinalGrade
}

func newExportFakeStore() *ExportFakeStore {
	return &ExportFakeStore{
		submissions:          make(map[uuid.UUID]store.Submission),
		preloadedFinalGrades: make(map[uuid.UUID]store.FinalGrade),
	}
}

func (f *ExportFakeStore) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.submissions[id]
	if !ok {
		return store.Submission{}, fmt.Errorf("GetSubmission %s: %w", id, store.ErrNotFound)
	}
	return sub, nil
}

func (f *ExportFakeStore) GetFinalGrade(_ context.Context, tenantID, submissionID uuid.UUID) (store.FinalGrade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fg, ok := f.preloadedFinalGrades[submissionID]; ok && fg.TenantID == tenantID {
		return fg, nil
	}
	return store.FinalGrade{}, fmt.Errorf("GetFinalGrade %s: %w", submissionID, store.ErrNotFound)
}

func (f *ExportFakeStore) ListTeacherReviews(_ context.Context, tenantID, submissionID uuid.UUID) ([]store.TeacherReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []store.TeacherReview
	for _, r := range f.reviews {
		if r.TenantID == tenantID && r.SubmissionID == submissionID {
			result = append(result, r)
		}
	}
	return result, nil
}

// seedSubmission inserts a submission into the fake store and returns it.
func (f *ExportFakeStore) seedSubmission(tenantID uuid.UUID, state contracts.SubmissionState) store.Submission {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := store.Submission{
		ID:        uuid.New(),
		TenantID:  tenantID,
		State:     state,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	f.submissions[sub.ID] = sub
	return sub
}

// seedFinalGrade registers a FinalGrade for the given submission.
func (f *ExportFakeStore) seedFinalGrade(tenantID, submissionID uuid.UUID, gradedKey string) store.FinalGrade {
	f.mu.Lock()
	defer f.mu.Unlock()
	fg := store.FinalGrade{
		ID:           uuid.New(),
		TenantID:     tenantID,
		SubmissionID: submissionID,
		GradedKey:    gradedKey,
		ApprovedBy:   "teacher-1",
		ApprovedAt:   time.Now(),
		CreatedAt:    time.Now(),
	}
	f.preloadedFinalGrades[submissionID] = fg
	return fg
}

// seedReview inserts a teacher override.
func (f *ExportFakeStore) seedReview(tenantID, submissionID uuid.UUID, qno string, newMarks float64, feedback string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews = append(f.reviews, store.TeacherReview{
		ID:           uuid.New(),
		TenantID:     tenantID,
		SubmissionID: submissionID,
		QuestionNo:   qno,
		OldMarks:     0,
		NewMarks:     newMarks,
		Feedback:     feedback,
		Comment:      "test override",
		Actor:        "teacher-1",
		CreatedAt:    time.Now(),
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper for export endpoint
// ─────────────────────────────────────────────────────────────────────────────

func buildExportRouter(t *testing.T, h *api.ExportHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Get("/v1/submissions/{id}/export.csv", h.ExportCSV)
	return r
}

// makeGradedPaperWithFlags builds a GradedPaper with flags and stores it in fakeObjects.
func makeGradedPaperWithFlags(t *testing.T, fakeObjects *FakeObjectStore, key string) contracts.GradedPaper {
	t.Helper()
	section := "algebra"
	paper := contracts.GradedPaper{
		Subject:   "Maths",
		SourcePDF: "source.pdf",
		Questions: []contracts.GradedQuestion{
			{
				QuestionNo:      "1",
				Section:         &section,
				MaxMarks:        10,
				AwardedMarks:    7,
				Justification:   "Good work",
				Feedback:        "AI feedback q1",
				GradeConfidence: 0.9,
				Flags:           []string{"partial", "check"},
			},
			{
				QuestionNo:      "2",
				Section:         nil,
				MaxMarks:        5,
				AwardedMarks:    3,
				Justification:   "Partial",
				Feedback:        "AI feedback q2",
				GradeConfidence: 0.8,
				Flags:           []string{},
			},
		},
		Total:    10,
		MaxTotal: 15,
		Score100: 66.7,
	}
	data, err := json.Marshal(paper)
	require.NoError(t, err)
	require.NoError(t, fakeObjects.PutObject(context.Background(), key, data))
	return paper
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestExportCSV_LiveGradedWithOverride verifies that a submission in
// teacher_review state (no final_grade) returns graded.v1 + overlaid overrides.
// The overridden question shows the new awarded_marks value in the CSV.
func TestExportCSV_LiveGradedWithOverride(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newExportFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeStore.seedSubmission(tenantID, contracts.StateTeacherReview)

	// Store graded.v1.json
	v1Key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, sub.ID)
	makeGradedPaperWithFlags(t, fakeObjects, v1Key)

	// Add a teacher override: question "1" → 9.0 marks, new feedback
	fakeStore.seedReview(tenantID, sub.ID, "1", 9.0, "Revised: excellent")

	h := &api.ExportHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildExportRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Check Content-Type and Content-Disposition.
	assert.Equal(t, "text/csv; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t,
		fmt.Sprintf(`attachment; filename="submission-%s.csv"`, sub.ID),
		rec.Header().Get("Content-Disposition"),
	)

	// Parse CSV.
	r := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := r.ReadAll()
	require.NoError(t, err, "CSV must parse cleanly")

	// Header row.
	require.GreaterOrEqual(t, len(rows), 1, "must have at least a header row")
	assert.Equal(t,
		[]string{"question_no", "section", "max_marks", "awarded_marks", "grade_confidence", "feedback", "flags"},
		rows[0],
	)

	// Two data rows.
	require.Len(t, rows, 3, "header + 2 questions")

	// Question 1: override applied → 9.0 marks, feedback column reflects Feedback field.
	q1 := rows[1]
	assert.Equal(t, "1", q1[0])                // question_no
	assert.Equal(t, "algebra", q1[1])           // section
	assert.Equal(t, "10", q1[2])                // max_marks
	assert.Equal(t, "9", q1[3])                 // awarded_marks (overridden)
	assert.Equal(t, "Revised: excellent", q1[5]) // feedback column reflects Feedback (not Justification)
	assert.Equal(t, "partial;check", q1[6])     // flags (index 6)

	// Question 2: no override → original 3.0.
	q2 := rows[2]
	assert.Equal(t, "2", q2[0])  // question_no
	assert.Equal(t, "", q2[1])   // section (nil → empty)
	assert.Equal(t, "3", q2[3])  // awarded_marks original
	assert.Equal(t, "", q2[6])   // flags empty → empty string
}

// TestExportCSV_ApprovedUsesFinalSnapshot verifies that an approved submission
// reads the graded.final.json artifact (baked-in overrides), not graded.v1.json.
func TestExportCSV_ApprovedUsesFinalSnapshot(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newExportFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeStore.seedSubmission(tenantID, contracts.StateApproved)

	// Store graded.final.json with baked-in override (awarded_marks=10 for q1).
	finalKey := fmt.Sprintf("%s/%s/graded.final.json", tenantID, sub.ID)
	makeGradedPaperWithFlags(t, fakeObjects, finalKey)
	// Overwrite q1 awarded_marks in the final artifact.
	section := "algebra"
	finalPaper := contracts.GradedPaper{
		Subject:   "Maths",
		SourcePDF: "source.pdf",
		Questions: []contracts.GradedQuestion{
			{
				QuestionNo:      "1",
				Section:         &section,
				MaxMarks:        10,
				AwardedMarks:    10, // baked-in override
				Justification:   "Perfect",
				GradeConfidence: 0.95,
				Flags:           []string{"approved"},
			},
		},
		Total:    10,
		MaxTotal: 10,
		Score100: 100,
	}
	finalData, err := json.Marshal(finalPaper)
	require.NoError(t, err)
	require.NoError(t, fakeObjects.PutObject(context.Background(), finalKey, finalData))

	// Register FinalGrade pointing at graded.final.json.
	fakeStore.seedFinalGrade(tenantID, sub.ID, finalKey)

	// Also store graded.v1.json (should NOT be used).
	v1Key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, sub.ID)
	makeGradedPaperWithFlags(t, fakeObjects, v1Key)

	h := &api.ExportHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildExportRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	r := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := r.ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 2) // header + 1 question

	// Question 1 awarded_marks must be 10 from final snapshot, not 7 from v1.
	assert.Equal(t, "10", rows[1][3], "approved snapshot awarded_marks must be 10")
	assert.Equal(t, "approved", rows[1][6], "flags must come from final snapshot")
}

// TestExportCSV_CrossTenant verifies 404 for cross-tenant access.
func TestExportCSV_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := newExportFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeStore.seedSubmission(tenantA, contracts.StateTeacherReview)
	v1Key := fmt.Sprintf("%s/%s/graded.v1.json", tenantA, sub.ID)
	makeGradedPaperWithFlags(t, fakeObjects, v1Key)

	h := &api.ExportHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	// Caller belongs to tenantB.
	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildExportRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestExportCSV_ScannerForbidden verifies 404 when the caller lacks ViewResults.
func TestExportCSV_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newExportFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	v1Key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, sub.ID)
	makeGradedPaperWithFlags(t, fakeObjects, v1Key)

	h := &api.ExportHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("scanner-key", scanner)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/export.csv", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	router := buildExportRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestExportCSV_NotGradedYet verifies 404 when no graded artifact exists.
func TestExportCSV_NotGradedYet(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newExportFakeStore()
	fakeObjects := NewFakeObjectStore() // empty — no graded artifact

	sub := fakeStore.seedSubmission(tenantID, contracts.StateGrading)

	h := &api.ExportHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildExportRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	// Not graded yet → 404.
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestExportCSV_FlagsJoinedBySemicolon verifies that multiple flags in a
// question are joined by ";" in the flags CSV column.
func TestExportCSV_FlagsJoinedBySemicolon(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newExportFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	v1Key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, sub.ID)
	makeGradedPaperWithFlags(t, fakeObjects, v1Key) // question 1 has flags: ["partial","check"]

	h := &api.ExportHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildExportRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	r := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := r.ReadAll()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 2)

	// Question 1 flags (index 6): "partial;check"
	assert.Equal(t, "partial;check", rows[1][6])
	// Question 2 flags: empty string (empty slice)
	assert.Equal(t, "", rows[2][6])
}
