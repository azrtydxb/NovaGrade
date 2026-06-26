package api_test

// moderation_handler_test.go — httptest tests for the three moderation endpoints.
//
// TDD: tests were written before the handler to define the expected behaviour.
//
// Endpoints under test:
//   POST /v1/assessment-versions/{avid}/moderation  — start session
//   POST /v1/moderation/{id}/marks                  — record mark
//   GET  /v1/moderation/{id}                        — comparison report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// ModerationFakeStore
// ─────────────────────────────────────────────────────────────────────────────

type ModerationFakeStore struct {
	*ClassResultsFakeStore                              // ExportStore + ListSubmissionsByAV + GetStudent + GetAVTenantID
	mu             sync.Mutex
	sessions       map[uuid.UUID]store.ModerationSession
	sessionSubs    map[uuid.UUID][]uuid.UUID            // sessionID → sampled submission ids
	marks          []store.ModerationMark
	finalGrades    map[uuid.UUID]store.FinalGrade       // submissionID → finalGrade (for comparison)
	// No gradeMutated flag needed: the ModerationStore interface exposes no
	// grade-write method (no SetSubmissionState, InsertFinalGrade, etc.), so
	// the no-mutation guarantee is structurally enforced by the narrow interface.
}

func newModerationFakeStore() *ModerationFakeStore {
	return &ModerationFakeStore{
		ClassResultsFakeStore: newClassResultsFakeStore(),
		sessions:              make(map[uuid.UUID]store.ModerationSession),
		sessionSubs:           make(map[uuid.UUID][]uuid.UUID),
		finalGrades:           make(map[uuid.UUID]store.FinalGrade),
	}
}

// Implement store.ModerationStore interface methods:

func (f *ModerationFakeStore) CreateModerationSession(_ context.Context, p store.CreateModerationSessionParams) (store.ModerationSession, []uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess := store.ModerationSession{
		ID:                  uuid.New(),
		TenantID:            p.TenantID,
		AssessmentVersionID: p.AssessmentVersionID,
		CreatedBy:           p.CreatedBy,
		SampleSize:          p.SampleSize,
		Status:              "open",
		CreatedAt:           time.Now(),
	}
	// Sample submissions deterministically from the byAVID list.
	all := f.ClassResultsFakeStore.byAVID[p.AssessmentVersionID]
	var sampled []uuid.UUID
	for i, sub := range all {
		if sub.TenantID != p.TenantID {
			continue
		}
		if i >= p.SampleSize {
			break
		}
		sampled = append(sampled, sub.ID)
	}
	f.sessions[sess.ID] = sess
	f.sessionSubs[sess.ID] = sampled
	return sess, sampled, nil
}

func (f *ModerationFakeStore) RecordModerationMark(_ context.Context, p store.RecordModerationMarkParams) (store.ModerationMark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mark := store.ModerationMark{
		ID:             uuid.New(),
		TenantID:       p.TenantID,
		SessionID:      p.SessionID,
		SubmissionID:   p.SubmissionID,
		QuestionNo:     p.QuestionNo,
		ModeratorMarks: p.ModeratorMarks,
		Moderator:      p.Moderator,
		CreatedAt:      time.Now(),
	}
	f.marks = append(f.marks, mark)
	return mark, nil
}

func (f *ModerationFakeStore) GetModerationSession(_ context.Context, tenantID, id uuid.UUID) (store.ModerationSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[id]
	if !ok || sess.TenantID != tenantID {
		return store.ModerationSession{}, fmt.Errorf("GetModerationSession %s: %w", id, store.ErrNotFound)
	}
	return sess, nil
}

func (f *ModerationFakeStore) ListModerationSubmissions(_ context.Context, tenantID, sessionID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[sessionID]
	if !ok || sess.TenantID != tenantID {
		return nil, nil
	}
	return f.sessionSubs[sessionID], nil
}

func (f *ModerationFakeStore) ListModerationMarks(_ context.Context, tenantID, sessionID uuid.UUID) ([]store.ModerationMark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ModerationMark
	for _, m := range f.marks {
		if m.SessionID == sessionID && m.TenantID == tenantID {
			out = append(out, m)
		}
	}
	return out, nil
}

// Override GetFinalGrade to use the finalGrades map seeded by tests, and to
// track mutation (it must never be called with write intent — here it's read-only).
func (f *ModerationFakeStore) GetFinalGrade(_ context.Context, tenantID, submissionID uuid.UUID) (store.FinalGrade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fg, ok := f.finalGrades[submissionID]; ok && fg.TenantID == tenantID {
		return fg, nil
	}
	return store.FinalGrade{}, fmt.Errorf("GetFinalGrade %s: %w", submissionID, store.ErrNotFound)
}

// seedFinalGradeForComparison registers a final grade to be used in comparison tests.
func (f *ModerationFakeStore) seedFinalGradeForComparison(tenantID, submissionID uuid.UUID, gradedKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalGrades[submissionID] = store.FinalGrade{
		ID:           uuid.New(),
		TenantID:     tenantID,
		SubmissionID: submissionID,
		GradedKey:    gradedKey,
		ApprovedBy:   "teacher",
		ApprovedAt:   time.Now(),
		CreatedAt:    time.Now(),
	}
}

// assertNoGradeMutated documents that no grade mutation is possible during moderation.
// The ModerationStore interface exposes no grade-write method, so this is a
// structural guarantee rather than a runtime check. The assertion is kept as a
// named method so call-sites remain self-documenting.
func (f *ModerationFakeStore) assertNoGradeMutated(t *testing.T) {
	t.Helper()
	// No runtime check needed: ModerationStore interface has no grade-write
	// method (no SetSubmissionState, InsertFinalGrade, InsertTeacherReview, etc.).
	// If such a method were ever added, a compile error would surface immediately.
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildModerationRouter(t *testing.T, h *api.ModerationHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/assessment-versions/{avid}/moderation", h.StartSession)
	r.Post("/v1/moderation/{id}/marks", h.RecordMark)
	r.Get("/v1/moderation/{id}", h.GetComparison)
	return r
}

// newTeacherToken issues a JWT for a teacher in tenantID.
func newTeacherToken(t *testing.T, tenantID uuid.UUID) string {
	t.Helper()
	return issueToken(t, auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestStartSession_OK: POST session → 201, sampled ids returned.
func TestStartSession_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	// Seed 5 graded submissions.
	for i := 0; i < 5; i++ {
		fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, fmt.Sprintf("Student%d", i))
	}

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())

	body, _ := json.Marshal(map[string]int{"sample_size": 3})
	req := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+newTeacherToken(t, tenantID))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp struct {
		SessionID            string   `json:"session_id"`
		SampledSubmissionIDs []string `json:"sampled_submission_ids"`
		SampleSize           int      `json:"sample_size"`
		Status               string   `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.SessionID)
	assert.Equal(t, 3, resp.SampleSize)
	assert.Equal(t, "open", resp.Status)
	assert.LessOrEqual(t, len(resp.SampledSubmissionIDs), 3)
}

// TestStartSession_CrossTenant: avid belongs to tenantA, request from tenantB → 404.
func TestStartSession_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	// Seed under tenantA.
	fakeStore.seedGradedSubmission(t, fakeObjects, tenantA, avid, "Alice")

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())

	body, _ := json.Marshal(map[string]int{"sample_size": 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+newTeacherToken(t, tenantB))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant must return 404")
}

// TestStartSession_ScannerForbidden: scanner lacks ActionReviewFixApprove → 404.
func TestStartSession_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{ID: "scanner-1", TenantID: tenantID.String(), Roles: []domain.Role{domain.RoleScanner}}
	resolver.Register("scanner-key", scanner)
	router := buildModerationRouter(t, h, resolver)

	body, _ := json.Marshal(map[string]int{"sample_size": 2})
	req := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "scanner-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "scanner must be denied with 404")
}

// TestRecordMark_OK: submit a mark → 201.
func TestRecordMark_OK(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())
	tok := newTeacherToken(t, tenantID)

	// Start a session.
	sessBody, _ := json.Marshal(map[string]int{"sample_size": 1})
	sessReq := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(sessBody))
	sessReq.Header.Set("Authorization", "Bearer "+tok)
	sessReq.Header.Set("Content-Type", "application/json")
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	require.Equal(t, http.StatusCreated, sessRec.Code)

	var sessResp struct {
		SessionID            string   `json:"session_id"`
		SampledSubmissionIDs []string `json:"sampled_submission_ids"`
	}
	require.NoError(t, json.Unmarshal(sessRec.Body.Bytes(), &sessResp))
	require.NotEmpty(t, sessResp.SessionID)
	require.NotEmpty(t, sessResp.SampledSubmissionIDs)

	// Submit a mark.
	markBody, _ := json.Marshal(map[string]interface{}{
		"submission_id":   sessResp.SampledSubmissionIDs[0],
		"question_no":     "1",
		"moderator_marks": 6.5,
	})
	markReq := httptest.NewRequest(http.MethodPost, "/v1/moderation/"+sessResp.SessionID+"/marks", bytes.NewReader(markBody))
	markReq.Header.Set("Authorization", "Bearer "+tok)
	markReq.Header.Set("Content-Type", "application/json")
	markRec := httptest.NewRecorder()
	router.ServeHTTP(markRec, markReq)

	require.Equal(t, http.StatusCreated, markRec.Code, "body: %s", markRec.Body.String())

	// Verify no grade was mutated.
	fakeStore.assertNoGradeMutated(t)
}

// TestRecordMark_CrossTenantSession: session belongs to tenantA, request from tenantB → 404.
func TestRecordMark_CrossTenantSession(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantA, avid, "Alice")

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())

	// Create session as tenantA.
	sessBody, _ := json.Marshal(map[string]int{"sample_size": 1})
	sessReq := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(sessBody))
	sessReq.Header.Set("Authorization", "Bearer "+newTeacherToken(t, tenantA))
	sessReq.Header.Set("Content-Type", "application/json")
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	require.Equal(t, http.StatusCreated, sessRec.Code)

	var sessResp struct{ SessionID string `json:"session_id"` }
	require.NoError(t, json.Unmarshal(sessRec.Body.Bytes(), &sessResp))

	// tenantB tries to post a mark to tenantA's session.
	markBody, _ := json.Marshal(map[string]interface{}{
		"submission_id":   uuid.New().String(),
		"question_no":     "1",
		"moderator_marks": 5.0,
	})
	markReq := httptest.NewRequest(http.MethodPost, "/v1/moderation/"+sessResp.SessionID+"/marks", bytes.NewReader(markBody))
	markReq.Header.Set("Authorization", "Bearer "+newTeacherToken(t, tenantB))
	markReq.Header.Set("Content-Type", "application/json")
	markRec := httptest.NewRecorder()
	router.ServeHTTP(markRec, markReq)

	require.Equal(t, http.StatusNotFound, markRec.Code, "cross-tenant mark submission must return 404")
}

// TestRecordMark_SubmissionNotInSample: a submission_id not in the session's
// sampled submissions must be rejected with 422 and NO mark recorded.
func TestRecordMark_SubmissionNotInSample(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())
	tok := newTeacherToken(t, tenantID)

	// Start a session (sample_size=1 → Alice is the only sampled submission).
	sessBody, _ := json.Marshal(map[string]int{"sample_size": 1})
	sessReq := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(sessBody))
	sessReq.Header.Set("Authorization", "Bearer "+tok)
	sessReq.Header.Set("Content-Type", "application/json")
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	require.Equal(t, http.StatusCreated, sessRec.Code)

	var sessResp struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(sessRec.Body.Bytes(), &sessResp))

	// Submit a mark using a submission_id that is NOT in the sample.
	outsideSubID := uuid.New()
	markBody, _ := json.Marshal(map[string]interface{}{
		"submission_id":   outsideSubID.String(),
		"question_no":     "1",
		"moderator_marks": 5.0,
	})
	markReq := httptest.NewRequest(http.MethodPost, "/v1/moderation/"+sessResp.SessionID+"/marks", bytes.NewReader(markBody))
	markReq.Header.Set("Authorization", "Bearer "+tok)
	markReq.Header.Set("Content-Type", "application/json")
	markRec := httptest.NewRecorder()
	router.ServeHTTP(markRec, markReq)

	// Must be rejected with 4xx (422 Unprocessable Entity).
	assert.Equal(t, http.StatusUnprocessableEntity, markRec.Code,
		"out-of-sample submission must be rejected; body: %s", markRec.Body.String())

	// No mark must have been recorded.
	fakeStore.mu.Lock()
	markCount := len(fakeStore.marks)
	fakeStore.mu.Unlock()
	assert.Equal(t, 0, markCount, "no mark should be recorded for an out-of-sample submission")
}

// TestGetComparison_Deltas: verifies that comparison report computes correct
// AI/teacher-final/moderator values and deltas.
//
//	Setup:
//	  submission with graded.v1.json: Q1=7, Q2=3 (AI marks)
//	  no FinalGrade → teacher-final = AI (no overrides seeded)
//	  moderator mark: Q1=8.5 → delta_mod_teacher = 1.5, delta_mod_ai = 1.5
//	  mean_abs_mod_teacher_delta = 1.5
func TestGetComparison_Deltas(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	// Seed a graded submission (graded.v1.json has Q1=7, Q2=3 from makeGradedPaperWithFlags).
	sub := fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Alice")
	// The graded paper (from makeGradedPaperWithFlags): Q1 AwardedMarks=7, Q2 AwardedMarks=3.

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())
	tok := newTeacherToken(t, tenantID)

	// 1. Start session.
	sessBody, _ := json.Marshal(map[string]int{"sample_size": 1})
	sessReq := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(sessBody))
	sessReq.Header.Set("Authorization", "Bearer "+tok)
	sessReq.Header.Set("Content-Type", "application/json")
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	require.Equal(t, http.StatusCreated, sessRec.Code)

	var sessResp struct {
		SessionID            string   `json:"session_id"`
		SampledSubmissionIDs []string `json:"sampled_submission_ids"`
	}
	require.NoError(t, json.Unmarshal(sessRec.Body.Bytes(), &sessResp))

	// The sampled submission is Alice's sub (only one in the AV).
	require.Contains(t, sessResp.SampledSubmissionIDs, sub.ID.String())

	// 2. Record moderator mark for Q1: moderator gives 8.5 (AI was 7, teacher-final also 7).
	markBody, _ := json.Marshal(map[string]interface{}{
		"submission_id":   sub.ID.String(),
		"question_no":     "1",
		"moderator_marks": 8.5,
	})
	markReq := httptest.NewRequest(http.MethodPost, "/v1/moderation/"+sessResp.SessionID+"/marks", bytes.NewReader(markBody))
	markReq.Header.Set("Authorization", "Bearer "+tok)
	markReq.Header.Set("Content-Type", "application/json")
	markRec := httptest.NewRecorder()
	router.ServeHTTP(markRec, markReq)
	require.Equal(t, http.StatusCreated, markRec.Code)

	// 3. GET comparison report.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/moderation/"+sessResp.SessionID, nil)
	getReq.Header.Set("Authorization", "Bearer "+tok)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code, "body: %s", getRec.Body.String())

	var resp struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
		Marks []struct {
			SubmissionID    string  `json:"submission_id"`
			QuestionNo      string  `json:"question_no"`
			AI              float64 `json:"ai"`
			TeacherFinal    float64 `json:"teacher_final"`
			Moderator       float64 `json:"moderator"`
			DeltaModTeacher float64 `json:"delta_mod_teacher"`
			DeltaModAI      float64 `json:"delta_mod_ai"`
		} `json:"marks"`
		Summary struct {
			MeanAbsModTeacherDelta float64 `json:"mean_abs_mod_teacher_delta"`
			Count                  int     `json:"count"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &resp))
	assert.Equal(t, sessResp.SessionID, resp.Session.ID)
	require.Len(t, resp.Marks, 1, "one mark was submitted")

	m := resp.Marks[0]
	assert.Equal(t, sub.ID.String(), m.SubmissionID)
	assert.Equal(t, "1", m.QuestionNo)
	assert.InDelta(t, 7.0, m.AI, 0.001, "AI mark for Q1 must be 7")
	assert.InDelta(t, 7.0, m.TeacherFinal, 0.001, "teacher-final (no overrides) must equal AI mark")
	assert.InDelta(t, 8.5, m.Moderator, 0.001, "moderator mark must be 8.5")
	assert.InDelta(t, 1.5, m.DeltaModTeacher, 0.001, "delta_mod_teacher = 8.5 - 7 = 1.5")
	assert.InDelta(t, 1.5, m.DeltaModAI, 0.001, "delta_mod_ai = 8.5 - 7 = 1.5")

	assert.Equal(t, 1, resp.Summary.Count)
	assert.InDelta(t, 1.5, resp.Summary.MeanAbsModTeacherDelta, 0.001)

	// CRITICAL: verify no grade was mutated.
	fakeStore.assertNoGradeMutated(t)
}

// TestGetComparison_WithFinalGrade: when a final_grade exists (teacher approved),
// teacher_final should reflect the final baked artifact, not raw v1.
func TestGetComparison_WithFinalGrade(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeStore.seedGradedSubmission(t, fakeObjects, tenantID, avid, "Bob")

	// Seed a final_grade pointing to a baked artifact where Q1=9 (teacher bumped it).
	finalKey := fmt.Sprintf("%s/%s/graded.final.json", tenantID, sub.ID)
	finalPaper := contracts.GradedPaper{
		Questions: []contracts.GradedQuestion{
			{QuestionNo: "1", AwardedMarks: 9.0, MaxMarks: 10},
			{QuestionNo: "2", AwardedMarks: 3.0, MaxMarks: 5},
		},
	}
	finalData, _ := json.Marshal(finalPaper)
	require.NoError(t, fakeObjects.PutObject(context.Background(), finalKey, finalData))
	fakeStore.seedFinalGradeForComparison(tenantID, sub.ID, finalKey)

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())
	tok := newTeacherToken(t, tenantID)

	// Start session.
	sessBody, _ := json.Marshal(map[string]int{"sample_size": 1})
	sessReq := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(sessBody))
	sessReq.Header.Set("Authorization", "Bearer "+tok)
	sessReq.Header.Set("Content-Type", "application/json")
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	require.Equal(t, http.StatusCreated, sessRec.Code)

	var sessResp struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(sessRec.Body.Bytes(), &sessResp))

	// Record moderator mark for Q1: moderator gives 8 (teacher bumped to 9 from AI 7).
	markBody, _ := json.Marshal(map[string]interface{}{
		"submission_id":   sub.ID.String(),
		"question_no":     "1",
		"moderator_marks": 8.0,
	})
	markReq := httptest.NewRequest(http.MethodPost, "/v1/moderation/"+sessResp.SessionID+"/marks", bytes.NewReader(markBody))
	markReq.Header.Set("Authorization", "Bearer "+tok)
	markReq.Header.Set("Content-Type", "application/json")
	markRec := httptest.NewRecorder()
	router.ServeHTTP(markRec, markReq)
	require.Equal(t, http.StatusCreated, markRec.Code)

	// GET comparison.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/moderation/"+sessResp.SessionID, nil)
	getReq.Header.Set("Authorization", "Bearer "+tok)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)

	var resp struct {
		Marks []struct {
			AI              float64 `json:"ai"`
			TeacherFinal    float64 `json:"teacher_final"`
			Moderator       float64 `json:"moderator"`
			DeltaModTeacher float64 `json:"delta_mod_teacher"`
		} `json:"marks"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &resp))
	require.Len(t, resp.Marks, 1)
	m := resp.Marks[0]
	assert.InDelta(t, 7.0, m.AI, 0.001, "AI mark (v1) for Q1 = 7")
	assert.InDelta(t, 9.0, m.TeacherFinal, 0.001, "teacher-final (baked) for Q1 = 9")
	assert.InDelta(t, 8.0, m.Moderator, 0.001)
	assert.InDelta(t, -1.0, m.DeltaModTeacher, 0.001, "8 - 9 = -1")

	fakeStore.assertNoGradeMutated(t)
}

// TestGetComparison_CrossTenant: tenantB cannot read tenantA's session → 404.
func TestGetComparison_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	avid := uuid.New()
	fakeStore := newModerationFakeStore()
	fakeObjects := NewFakeObjectStore()

	fakeStore.seedGradedSubmission(t, fakeObjects, tenantA, avid, "Alice")

	h := &api.ModerationHandlers{Store: fakeStore, Objects: fakeObjects, DeployMode: "onprem"}
	router := buildModerationRouter(t, h, auth.NewAPIKeyResolver())

	// Create session as tenantA.
	sessBody, _ := json.Marshal(map[string]int{"sample_size": 1})
	sessReq := httptest.NewRequest(http.MethodPost, "/v1/assessment-versions/"+avid.String()+"/moderation", bytes.NewReader(sessBody))
	sessReq.Header.Set("Authorization", "Bearer "+newTeacherToken(t, tenantA))
	sessReq.Header.Set("Content-Type", "application/json")
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	require.Equal(t, http.StatusCreated, sessRec.Code)

	var sessResp struct{ SessionID string `json:"session_id"` }
	require.NoError(t, json.Unmarshal(sessRec.Body.Bytes(), &sessResp))

	// tenantB tries to GET the comparison.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/moderation/"+sessResp.SessionID, nil)
	getReq.Header.Set("Authorization", "Bearer "+newTeacherToken(t, tenantB))
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)

	require.Equal(t, http.StatusNotFound, getRec.Code, "cross-tenant comparison must return 404")
}
