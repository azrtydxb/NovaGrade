package api_test

// Tests for:
//   GET  /v1/submissions/{id}/review
//   PATCH /v1/submissions/{id}/questions/{qno}
//
// Follows the Phase-1 test pattern (httptest + behavioural fakes).

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
// Extended fake store that also handles TeacherReview and InsertAuditEvent
// ─────────────────────────────────────────────────────────────────────────────

// ReviewFakeStore extends FakeStore with TeacherReview and AuditEvent support.
// It implements api.ReviewStore.
type ReviewFakeStore struct {
	mu           sync.Mutex
	submissions  map[uuid.UUID]store.Submission
	reviews      []store.TeacherReview
	auditEvents  []store.AuditEvent
}

func newReviewFakeStore() *ReviewFakeStore {
	return &ReviewFakeStore{
		submissions: make(map[uuid.UUID]store.Submission),
	}
}

func (f *ReviewFakeStore) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.submissions[id]
	if !ok {
		return store.Submission{}, fmt.Errorf("GetSubmission %s: %w", id, store.ErrNotFound)
	}
	return sub, nil
}

func (f *ReviewFakeStore) InsertTeacherReview(_ context.Context, p store.InsertTeacherReviewParams) (store.TeacherReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr := store.TeacherReview{
		ID:           uuid.New(),
		TenantID:     p.TenantID,
		SubmissionID: p.SubmissionID,
		QuestionNo:   p.QuestionNo,
		OldMarks:     p.OldMarks,
		NewMarks:     p.NewMarks,
		Feedback:     p.Feedback,
		Comment:      p.Comment,
		Actor:        p.Actor,
		CreatedAt:    time.Now(),
	}
	f.reviews = append(f.reviews, tr)
	return tr, nil
}

func (f *ReviewFakeStore) ListTeacherReviews(_ context.Context, tenantID, submissionID uuid.UUID) ([]store.TeacherReview, error) {
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

func (f *ReviewFakeStore) InsertAuditEvent(_ context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev := store.AuditEvent{
		ID:         uuid.New(),
		TenantID:   p.TenantID,
		EntityType: p.EntityType,
		EntityID:   p.EntityID,
		Actor:      p.Actor,
		Action:     p.Action,
		OldValue:   p.OldValue,
		NewValue:   p.NewValue,
		Reason:     p.Reason,
		CreatedAt:  time.Now(),
	}
	f.auditEvents = append(f.auditEvents, ev)
	return ev, nil
}

// seedSubmission inserts a submission into the fake store and returns it.
func (f *ReviewFakeStore) seedSubmission(tenantID uuid.UUID, state contracts.SubmissionState) store.Submission {
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

// reviewCount returns the number of teacher_review rows in the store.
func (f *ReviewFakeStore) reviewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reviews)
}

// auditCount returns the number of audit_event rows in the store.
func (f *ReviewFakeStore) auditCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.auditEvents)
}

// latestAuditEvent returns the most recently inserted audit event.
func (f *ReviewFakeStore) latestAuditEvent() store.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auditEvents[len(f.auditEvents)-1]
}

// latestTeacherReview returns the most recently inserted teacher_review row.
func (f *ReviewFakeStore) latestTeacherReview() store.TeacherReview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reviews[len(f.reviews)-1]
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper for review endpoints
// ─────────────────────────────────────────────────────────────────────────────

func buildReviewRouter(t *testing.T, h *api.ReviewHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Get("/v1/submissions/{id}/review", h.GetReview)
	r.Patch("/v1/submissions/{id}/questions/{qno}", h.PatchQuestion)
	return r
}

// makeGradedPaper builds a simple GradedPaper JSON and stores it in fakeObjects.
func makeGradedPaper(t *testing.T, fakeObjects *FakeObjectStore, tenantID, subID uuid.UUID) contracts.GradedPaper {
	t.Helper()
	paper := contracts.GradedPaper{
		Subject:   "Maths",
		SourcePDF: "source.pdf",
		Questions: []contracts.GradedQuestion{
			{QuestionNo: "1", MaxMarks: 10, AwardedMarks: 7, Justification: "Good", GradeConfidence: 0.9, Flags: []string{}},
			{QuestionNo: "2", MaxMarks: 5, AwardedMarks: 3, Justification: "Partial", GradeConfidence: 0.8, Flags: []string{}},
		},
		Total:    10,
		MaxTotal: 15,
		Score100: 66.7,
	}
	key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, subID)
	data, err := json.Marshal(paper)
	require.NoError(t, err)
	require.NoError(t, fakeObjects.PutObject(context.Background(), key, data))
	return paper
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: PATCH /v1/submissions/{id}/questions/{qno}
// ─────────────────────────────────────────────────────────────────────────────

// TestPatchQuestion_Override verifies a teacher can override a question's marks
// while the submission is in teacher_review state.
// Expectations:
//   - 200 response with updated effective question
//   - awarded_marks clamped and recorded in teacher_review
//   - audit_event written with action "override_question", old/new values, actor
func TestPatchQuestion_Override(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body, _ := json.Marshal(map[string]interface{}{
		"awarded_marks": 8.0,
		"feedback":      "Good work",
		"comment":       "Reconsidered",
	})
	req := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/1",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	resolver := auth.NewAPIKeyResolver()
	router := buildReviewRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Returned question should reflect the new marks.
	var q contracts.GradedQuestion
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &q))
	assert.Equal(t, "1", q.QuestionNo)
	assert.Equal(t, 8.0, q.AwardedMarks)

	// teacher_review row must be recorded.
	require.Equal(t, 1, fakeReviewStore.reviewCount())
	tr := fakeReviewStore.latestTeacherReview()
	assert.Equal(t, sub.ID, tr.SubmissionID)
	assert.Equal(t, "1", tr.QuestionNo)
	assert.Equal(t, 7.0, tr.OldMarks)  // original from graded.v1.json
	assert.Equal(t, 8.0, tr.NewMarks)
	assert.Equal(t, "teacher-1", tr.Actor)

	// audit_event must be recorded.
	require.Equal(t, 1, fakeReviewStore.auditCount())
	ev := fakeReviewStore.latestAuditEvent()
	assert.Equal(t, "submission", ev.EntityType)
	assert.Equal(t, sub.ID, *ev.EntityID)
	assert.Equal(t, "override_question", ev.Action)
	assert.Equal(t, "teacher-1", ev.Actor)
	assert.Equal(t, "Reconsidered", ev.Reason)

	// old_value and new_value should be non-empty JSON.
	assert.NotEmpty(t, ev.OldValue)
	assert.NotEmpty(t, ev.NewValue)

	// Verify old_value contains old awarded marks.
	var oldVal map[string]interface{}
	require.NoError(t, json.Unmarshal(ev.OldValue, &oldVal))
	assert.Equal(t, 7.0, oldVal["awarded_marks"])

	var newVal map[string]interface{}
	require.NoError(t, json.Unmarshal(ev.NewValue, &newVal))
	assert.Equal(t, 8.0, newVal["awarded_marks"])
}

// TestPatchQuestion_Locked verifies that 409 is returned when the submission is
// not in teacher_review state.
func TestPatchQuestion_Locked(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	for _, state := range []contracts.SubmissionState{
		contracts.StateGrading,
		contracts.StateApproved,
		contracts.StatePublished,
	} {
		t.Run(string(state), func(t *testing.T) {
			tenantID := uuid.New()
			fakeReviewStore := newReviewFakeStore()
			fakeObjects := NewFakeObjectStore()

			sub := fakeReviewStore.seedSubmission(tenantID, state)
			makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

			h := &api.ReviewHandlers{
				Store:      fakeReviewStore,
				Objects:    fakeObjects,
				DeployMode: "onprem",
			}

			principal := auth.Principal{
				ID:       "teacher-1",
				TenantID: tenantID.String(),
				Roles:    []domain.Role{domain.RoleTeacher},
			}
			tok := issueToken(t, principal)

			body, _ := json.Marshal(map[string]interface{}{"awarded_marks": 5.0})
			req := httptest.NewRequest(http.MethodPatch,
				"/v1/submissions/"+sub.ID.String()+"/questions/1",
				bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			resolver := auth.NewAPIKeyResolver()
			router := buildReviewRouter(t, h, resolver)
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusConflict, rec.Code)
			assert.Equal(t, 0, fakeReviewStore.reviewCount())
			assert.Equal(t, 0, fakeReviewStore.auditCount())
		})
	}
}

// TestPatchQuestion_ClampMaxMarks verifies that awarded_marks above MaxMarks is
// clamped to MaxMarks (10 in the test paper, question 1).
func TestPatchQuestion_ClampMaxMarks(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	// Request 999 marks for a question with MaxMarks=10 → must be clamped to 10.
	body, _ := json.Marshal(map[string]interface{}{"awarded_marks": 999.0})
	req := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/1",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	resolver := auth.NewAPIKeyResolver()
	router := buildReviewRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var q contracts.GradedQuestion
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &q))
	assert.Equal(t, 10.0, q.AwardedMarks) // clamped to MaxMarks

	tr := fakeReviewStore.latestTeacherReview()
	assert.Equal(t, 10.0, tr.NewMarks) // persisted clamped value
}

// TestPatchQuestion_ClampZero verifies that negative awarded_marks is clamped to 0.
func TestPatchQuestion_ClampZero(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body, _ := json.Marshal(map[string]interface{}{"awarded_marks": -5.0})
	req := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/1",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var q contracts.GradedQuestion
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &q))
	assert.Equal(t, 0.0, q.AwardedMarks) // clamped to 0
}

// TestPatchQuestion_UnknownQno verifies 404 when the question_no does not exist
// in the graded paper.
func TestPatchQuestion_UnknownQno(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body, _ := json.Marshal(map[string]interface{}{"awarded_marks": 5.0})
	req := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/99",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestPatchQuestion_CrossTenant verifies that a teacher from a different tenant
// gets 404 (no enumeration).
func TestPatchQuestion_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantA, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantA, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	// Caller belongs to tenantB
	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body, _ := json.Marshal(map[string]interface{}{"awarded_marks": 5.0})
	req := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/1",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestPatchQuestion_ScannerForbidden verifies that a scanner (lacks
// ReviewFixApprove) receives 404 (no-enumeration convention).
func TestPatchQuestion_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
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

	body, _ := json.Marshal(map[string]interface{}{"awarded_marks": 5.0})
	req := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/1",
		bytes.NewReader(body))
	req.Header.Set("X-API-Key", "scanner-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: GET /v1/submissions/{id}/review
// ─────────────────────────────────────────────────────────────────────────────

// TestGetReview_NoOverrides verifies that GET /review loads graded.v1.json,
// returns locked=false when in teacher_review, and all questions come from the
// base graded paper when no overrides exist.
func TestGetReview_NoOverrides(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	paper := makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/review", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// locked should be false (in teacher_review).
	var locked bool
	require.NoError(t, json.Unmarshal(resp["locked"], &locked))
	assert.False(t, locked)

	// paper should contain questions from the base graded paper.
	var respPaper contracts.GradedPaper
	require.NoError(t, json.Unmarshal(resp["paper"], &respPaper))
	require.Len(t, respPaper.Questions, 2)
	assert.Equal(t, paper.Questions[0].AwardedMarks, respPaper.Questions[0].AwardedMarks)
}

// TestGetReview_OverlaysLatestOverride verifies that GET /review overlays the
// LATEST teacher override for each question_no (last write wins).
func TestGetReview_OverlaysLatestOverride(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	// Insert two overrides for question "1" — the second should win.
	_, err := fakeReviewStore.InsertTeacherReview(context.Background(), store.InsertTeacherReviewParams{
		TenantID:     tenantID,
		SubmissionID: sub.ID,
		QuestionNo:   "1",
		OldMarks:     7.0,
		NewMarks:     8.0,
		Feedback:     "First override",
		Comment:      "round1",
		Actor:        "teacher-1",
	})
	require.NoError(t, err)

	_, err = fakeReviewStore.InsertTeacherReview(context.Background(), store.InsertTeacherReviewParams{
		TenantID:     tenantID,
		SubmissionID: sub.ID,
		QuestionNo:   "1",
		OldMarks:     8.0,
		NewMarks:     9.0,
		Feedback:     "Second override",
		Comment:      "round2",
		Actor:        "teacher-2",
	})
	require.NoError(t, err)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/review", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var respPaper contracts.GradedPaper
	require.NoError(t, json.Unmarshal(resp["paper"], &respPaper))

	// question "1" should have the second override (9.0), not the first (8.0).
	var q1 *contracts.GradedQuestion
	for i := range respPaper.Questions {
		if respPaper.Questions[i].QuestionNo == "1" {
			q1 = &respPaper.Questions[i]
			break
		}
	}
	require.NotNil(t, q1)
	assert.Equal(t, 9.0, q1.AwardedMarks)
	assert.Equal(t, "Second override", q1.Justification)

	// question "2" should still have the original marks.
	var q2 *contracts.GradedQuestion
	for i := range respPaper.Questions {
		if respPaper.Questions[i].QuestionNo == "2" {
			q2 = &respPaper.Questions[i]
			break
		}
	}
	require.NotNil(t, q2)
	assert.Equal(t, 3.0, q2.AwardedMarks)
}

// TestGetReview_Locked verifies that locked=true when the submission is NOT
// in teacher_review (e.g. approved).
func TestGetReview_Locked(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateApproved)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/review", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var locked bool
	require.NoError(t, json.Unmarshal(resp["locked"], &locked))
	assert.True(t, locked)
}

// TestGetReview_CrossTenant verifies 404 for cross-tenant access.
func TestGetReview_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantA, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantA, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/review", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestGetReview_ScannerForbidden verifies 404 when the caller lacks ReviewFixApprove.
func TestGetReview_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
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

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/review", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestGetReview_NoGradedArtifact verifies 409 when no graded.v1.json exists yet.
func TestGetReview_NoGradedArtifact(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore() // empty — no graded artifact

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/review", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildReviewRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

// TestPatchQuestion_SecondOverrideUsesLatestEffective verifies that when a second
// PATCH is made, old_marks in teacher_review reflects the EFFECTIVE value after
// the first override (not the original graded.v1.json value).
func TestPatchQuestion_SecondOverrideUsesLatestEffective(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeReviewStore := newReviewFakeStore()
	fakeObjects := NewFakeObjectStore()

	sub := fakeReviewStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaper(t, fakeObjects, tenantID, sub.ID)

	h := &api.ReviewHandlers{
		Store:      fakeReviewStore,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)
	resolver := auth.NewAPIKeyResolver()
	router := buildReviewRouter(t, h, resolver)

	// First override: 7→8
	body1, _ := json.Marshal(map[string]interface{}{"awarded_marks": 8.0})
	req1 := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/1",
		bytes.NewReader(body1))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Second override: effective is 8, new should be 6; old_marks in DB should be 8.
	body2, _ := json.Marshal(map[string]interface{}{"awarded_marks": 6.0})
	req2 := httptest.NewRequest(http.MethodPatch,
		"/v1/submissions/"+sub.ID.String()+"/questions/1",
		bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)

	require.Equal(t, 2, fakeReviewStore.reviewCount())
	tr2 := fakeReviewStore.latestTeacherReview()
	assert.Equal(t, 8.0, tr2.OldMarks, "old_marks should be effective value (8) not original (7)")
	assert.Equal(t, 6.0, tr2.NewMarks)
}
