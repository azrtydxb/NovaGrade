package api_test

// Tests for:
//   POST /v1/submissions/{id}/approve
//   POST /v1/submissions/{id}/publish
//   POST /v1/submissions/{id}/export
//
// TDD RED phase — written before the handler is implemented.
// Follows the pattern established by review_handler_test.go.

import (
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
// ApprovalFakeStore — implements api.ApprovalStore
// ─────────────────────────────────────────────────────────────────────────────

type ApprovalFakeStore struct {
	mu          sync.Mutex
	submissions map[uuid.UUID]store.Submission
	auditEvents []store.AuditEvent
	finalGrades []store.FinalGrade
	reviews     []store.TeacherReview
	// failAudit forces InsertAuditEvent to return an error.
	failAudit bool
	// preloadedFinalGrades holds existing FinalGrade rows for GetFinalGrade to return.
	preloadedFinalGrades map[uuid.UUID]store.FinalGrade
}

func newApprovalFakeStore() *ApprovalFakeStore {
	return &ApprovalFakeStore{
		submissions:          make(map[uuid.UUID]store.Submission),
		preloadedFinalGrades: make(map[uuid.UUID]store.FinalGrade),
	}
}

func (f *ApprovalFakeStore) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.submissions[id]
	if !ok {
		return store.Submission{}, fmt.Errorf("GetSubmission %s: %w", id, store.ErrNotFound)
	}
	return sub, nil
}

func (f *ApprovalFakeStore) GetFinalGrade(_ context.Context, tenantID, submissionID uuid.UUID) (store.FinalGrade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fg, ok := f.preloadedFinalGrades[submissionID]; ok && fg.TenantID == tenantID {
		return fg, nil
	}
	return store.FinalGrade{}, fmt.Errorf("GetFinalGrade %s: %w", submissionID, store.ErrNotFound)
}

func (f *ApprovalFakeStore) InsertAuditEvent(_ context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAudit {
		return store.AuditEvent{}, fmt.Errorf("simulated audit insert failure")
	}
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

func (f *ApprovalFakeStore) InsertFinalGrade(_ context.Context, p store.InsertFinalGradeParams) (store.FinalGrade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fg := store.FinalGrade{
		ID:           uuid.New(),
		TenantID:     p.TenantID,
		SubmissionID: p.SubmissionID,
		Total:        p.Total,
		MaxTotal:     p.MaxTotal,
		Score100:     p.Score100,
		GradedKey:    p.GradedKey,
		ApprovedBy:   p.ApprovedBy,
		ApprovedAt:   p.ApprovedAt,
		CreatedAt:    time.Now(),
	}
	f.finalGrades = append(f.finalGrades, fg)
	return fg, nil
}

func (f *ApprovalFakeStore) ListTeacherReviews(_ context.Context, tenantID, submissionID uuid.UUID) ([]store.TeacherReview, error) {
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
func (f *ApprovalFakeStore) seedSubmission(tenantID uuid.UUID, state contracts.SubmissionState) store.Submission {
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

func (f *ApprovalFakeStore) auditCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.auditEvents)
}

func (f *ApprovalFakeStore) finalGradeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.finalGrades)
}

func (f *ApprovalFakeStore) latestAuditAction() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auditEvents) == 0 {
		return ""
	}
	return f.auditEvents[len(f.auditEvents)-1].Action
}

func (f *ApprovalFakeStore) latestFinalGrade() store.FinalGrade {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finalGrades[len(f.finalGrades)-1]
}

// ─────────────────────────────────────────────────────────────────────────────
// ApprovalFakeBus — records published envelopes with the target queue
// ─────────────────────────────────────────────────────────────────────────────

type ApprovalFakeBus struct {
	mu       sync.Mutex
	messages []approvalMsg
}

type approvalMsg struct {
	queue string
	env   contracts.Envelope
}

func (b *ApprovalFakeBus) Publish(_ context.Context, queue string, env contracts.Envelope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, approvalMsg{queue: queue, env: env})
	return nil
}

func (b *ApprovalFakeBus) publishedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messages)
}

func (b *ApprovalFakeBus) latestMessage() approvalMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messages[len(b.messages)-1]
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper for approval endpoints
// ─────────────────────────────────────────────────────────────────────────────

func buildApprovalRouter(t *testing.T, h *api.ApprovalHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/submissions/{id}/approve", h.Approve)
	r.Post("/v1/submissions/{id}/publish", h.Publish)
	r.Post("/v1/submissions/{id}/export", h.Export)
	return r
}

// makeGradedPaperForApproval stores a graded paper artifact and returns it.
func makeGradedPaperForApproval(t *testing.T, fakeObjects *FakeObjectStore, tenantID, subID uuid.UUID) contracts.GradedPaper {
	t.Helper()
	paper := contracts.GradedPaper{
		Subject:   "Physics",
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
// Tests: POST /v1/submissions/{id}/approve
// ─────────────────────────────────────────────────────────────────────────────

// TestApprove_HappyPath verifies that approving a submission in teacher_review:
//   - returns 200 + FinalGrade JSON
//   - writes graded.final.json to object store
//   - InsertAuditEvent with action="approve" is recorded
//   - InsertFinalGrade snapshot is persisted (with overrides applied)
//   - publishes Envelope{Stage: contracts.StageApprove} to commands.q
func TestApprove_HappyPath(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	paper := makeGradedPaperForApproval(t, fakeObjects, tenantID, sub.ID)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Response should be FinalGrade JSON
	var fg store.FinalGrade
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fg))
	assert.Equal(t, sub.ID, fg.SubmissionID)
	assert.Equal(t, paper.Total, fg.Total)
	assert.Equal(t, paper.MaxTotal, fg.MaxTotal)
	assert.Equal(t, "teacher-1", fg.ApprovedBy)

	// Audit event must be recorded with action="approve" BEFORE FinalGrade
	require.Equal(t, 1, fakeApprovalStore.auditCount())
	assert.Equal(t, "approve", fakeApprovalStore.latestAuditAction())

	// FinalGrade snapshot must be persisted
	require.Equal(t, 1, fakeApprovalStore.finalGradeCount())
	stored := fakeApprovalStore.latestFinalGrade()
	assert.Equal(t, sub.ID, stored.SubmissionID)
	assert.Equal(t, "teacher-1", stored.ApprovedBy)

	// graded.final.json must be stored in the object store
	finalKey := fmt.Sprintf("%s/%s/graded.final.json", tenantID, sub.ID)
	finalData, err := fakeObjects.GetObject(context.Background(), finalKey)
	require.NoError(t, err)
	var finalPaper contracts.GradedPaper
	require.NoError(t, json.Unmarshal(finalData, &finalPaper))
	// Total should match the graded paper (no overrides in this test)
	assert.Equal(t, paper.Total, finalPaper.Total)

	// Envelope with Stage=approve must be published to commands.q
	require.Equal(t, 1, fakeBus.publishedCount())
	msg := fakeBus.latestMessage()
	assert.Equal(t, "commands.q", msg.queue)
	assert.Equal(t, contracts.StageApprove, msg.env.Stage)
	assert.Equal(t, tenantID.String(), msg.env.TenantID)
	assert.Equal(t, sub.ID.String(), msg.env.SubmissionID)
}

// TestApprove_WithOverrides verifies that the effective graded paper (overrides
// applied) is used for the FinalGrade snapshot (not the raw graded.v1.json).
func TestApprove_WithOverrides(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaperForApproval(t, fakeObjects, tenantID, sub.ID)

	// Seed a teacher override: Q1 was 7, override to 9
	fakeApprovalStore.reviews = append(fakeApprovalStore.reviews, store.TeacherReview{
		ID:           uuid.New(),
		TenantID:     tenantID,
		SubmissionID: sub.ID,
		QuestionNo:   "1",
		OldMarks:     7.0,
		NewMarks:     9.0,
		Feedback:     "Reconsidered",
		Actor:        "teacher-1",
		CreatedAt:    time.Now(),
	})

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// FinalGrade total should reflect the override (9 + 3 = 12, not original 7 + 3 = 10)
	fg := fakeApprovalStore.latestFinalGrade()
	assert.Equal(t, 12.0, fg.Total, "total should reflect override (9+3=12)")

	// The stored graded.final.json should also reflect the override
	finalKey := fmt.Sprintf("%s/%s/graded.final.json", tenantID, sub.ID)
	finalData, err := fakeObjects.GetObject(context.Background(), finalKey)
	require.NoError(t, err)
	var finalPaper contracts.GradedPaper
	require.NoError(t, json.Unmarshal(finalData, &finalPaper))
	// Q1 should have overridden mark of 9
	for _, q := range finalPaper.Questions {
		if q.QuestionNo == "1" {
			assert.Equal(t, 9.0, q.AwardedMarks, "Q1 should be overridden to 9")
		}
	}
}

// TestApprove_NotInTeacherReview verifies 409 when submission is NOT in teacher_review.
func TestApprove_NotInTeacherReview(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	for _, state := range []contracts.SubmissionState{
		contracts.StateGrading,
		contracts.StateApproved,
		contracts.StatePublished,
		contracts.StateExported,
	} {
		t.Run(string(state), func(t *testing.T) {
			tenantID := uuid.New()
			fakeApprovalStore := newApprovalFakeStore()
			fakeObjects := NewFakeObjectStore()
			fakeBus := &ApprovalFakeBus{}

			sub := fakeApprovalStore.seedSubmission(tenantID, state)
			makeGradedPaperForApproval(t, fakeObjects, tenantID, sub.ID)

			h := &api.ApprovalHandlers{
				Store:      fakeApprovalStore,
				Objects:    fakeObjects,
				Bus:        fakeBus,
				DeployMode: "onprem",
			}

			principal := auth.Principal{
				ID:       "teacher-1",
				TenantID: tenantID.String(),
				Roles:    []domain.Role{domain.RoleTeacher},
			}
			tok := issueToken(t, principal)

			req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/approve", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()

			router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusConflict, rec.Code, "state=%s should return 409", state)
			assert.Equal(t, 0, fakeApprovalStore.auditCount())
			assert.Equal(t, 0, fakeApprovalStore.finalGradeCount())
			assert.Equal(t, 0, fakeBus.publishedCount())
		})
	}
}

// TestApprove_AuditFailurePreventsAll verifies audit-first ordering:
// if InsertAuditEvent fails → 500, no FinalGrade persisted, no command published.
func TestApprove_AuditFailurePreventsAll(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeApprovalStore.failAudit = true
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaperForApproval(t, fakeObjects, tenantID, sub.ID)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, 0, fakeApprovalStore.auditCount())
	assert.Equal(t, 0, fakeApprovalStore.finalGradeCount())
	assert.Equal(t, 0, fakeBus.publishedCount())
}

// TestApprove_CrossTenant verifies 404 for cross-tenant access.
func TestApprove_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantA, contracts.StateTeacherReview)
	makeGradedPaperForApproval(t, fakeObjects, tenantA, sub.ID)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 0, fakeBus.publishedCount())
}

// TestApprove_ScannerForbidden verifies 404 when caller lacks ReviewFixApprove.
func TestApprove_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaperForApproval(t, fakeObjects, tenantID, sub.ID)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("scanner-key", scanner)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/approve", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, resolver)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 0, fakeBus.publishedCount())
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: POST /v1/submissions/{id}/publish
// ─────────────────────────────────────────────────────────────────────────────

// TestPublish_HappyPath verifies publish from approved → 200 + audit + command.
func TestPublish_HappyPath(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StateApproved)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/publish", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Audit event with action="publish"
	require.Equal(t, 1, fakeApprovalStore.auditCount())
	assert.Equal(t, "publish", fakeApprovalStore.latestAuditAction())

	// Envelope with Stage=publish published to commands.q
	require.Equal(t, 1, fakeBus.publishedCount())
	msg := fakeBus.latestMessage()
	assert.Equal(t, "commands.q", msg.queue)
	assert.Equal(t, contracts.StagePublish, msg.env.Stage)
	assert.Equal(t, sub.ID.String(), msg.env.SubmissionID)
}

// TestPublish_NotApproved verifies 409 when submission is not in approved state.
func TestPublish_NotApproved(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	for _, state := range []contracts.SubmissionState{
		contracts.StateTeacherReview,
		contracts.StateGrading,
		contracts.StatePublished,
		contracts.StateExported,
	} {
		t.Run(string(state), func(t *testing.T) {
			tenantID := uuid.New()
			fakeApprovalStore := newApprovalFakeStore()
			fakeObjects := NewFakeObjectStore()
			fakeBus := &ApprovalFakeBus{}

			sub := fakeApprovalStore.seedSubmission(tenantID, state)

			h := &api.ApprovalHandlers{
				Store:      fakeApprovalStore,
				Objects:    fakeObjects,
				Bus:        fakeBus,
				DeployMode: "onprem",
			}

			principal := auth.Principal{
				ID:       "teacher-1",
				TenantID: tenantID.String(),
				Roles:    []domain.Role{domain.RoleTeacher},
			}
			tok := issueToken(t, principal)

			req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/publish", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()

			router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusConflict, rec.Code, "state=%s should return 409", state)
			assert.Equal(t, 0, fakeApprovalStore.auditCount())
			assert.Equal(t, 0, fakeBus.publishedCount())
		})
	}
}

// TestPublish_AuditFailurePreventsCommand verifies audit-first: if InsertAuditEvent
// fails → 500 and no command published.
func TestPublish_AuditFailurePreventsCommand(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeApprovalStore.failAudit = true
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StateApproved)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/publish", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, 0, fakeBus.publishedCount())
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: POST /v1/submissions/{id}/export
// ─────────────────────────────────────────────────────────────────────────────

// TestExport_HappyPath verifies export from published → 200 + audit + command.
func TestExport_HappyPath(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StatePublished)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Audit event with action="export"
	require.Equal(t, 1, fakeApprovalStore.auditCount())
	assert.Equal(t, "export", fakeApprovalStore.latestAuditAction())

	// Envelope with Stage=export published to commands.q
	require.Equal(t, 1, fakeBus.publishedCount())
	msg := fakeBus.latestMessage()
	assert.Equal(t, "commands.q", msg.queue)
	assert.Equal(t, contracts.StageExport, msg.env.Stage)
	assert.Equal(t, sub.ID.String(), msg.env.SubmissionID)
}

// TestExport_NotPublished verifies 409 when submission is not in published state.
func TestExport_NotPublished(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	for _, state := range []contracts.SubmissionState{
		contracts.StateTeacherReview,
		contracts.StateApproved,
		contracts.StateGrading,
		contracts.StateExported,
	} {
		t.Run(string(state), func(t *testing.T) {
			tenantID := uuid.New()
			fakeApprovalStore := newApprovalFakeStore()
			fakeObjects := NewFakeObjectStore()
			fakeBus := &ApprovalFakeBus{}

			sub := fakeApprovalStore.seedSubmission(tenantID, state)

			h := &api.ApprovalHandlers{
				Store:      fakeApprovalStore,
				Objects:    fakeObjects,
				Bus:        fakeBus,
				DeployMode: "onprem",
			}

			principal := auth.Principal{
				ID:       "teacher-1",
				TenantID: tenantID.String(),
				Roles:    []domain.Role{domain.RoleTeacher},
			}
			tok := issueToken(t, principal)

			req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/export", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()

			router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusConflict, rec.Code, "state=%s should return 409", state)
			assert.Equal(t, 0, fakeApprovalStore.auditCount())
			assert.Equal(t, 0, fakeBus.publishedCount())
		})
	}
}

// TestExport_AuditFailurePreventsCommand verifies audit-first: if InsertAuditEvent
// fails → 500 and no command published.
func TestExport_AuditFailurePreventsCommand(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeApprovalStore.failAudit = true
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StatePublished)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, 0, fakeBus.publishedCount())
}

// TestExport_CrossTenant verifies 404 for cross-tenant access to export.
func TestExport_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantA, contracts.StatePublished)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 0, fakeBus.publishedCount())
}

// TestApprove_Idempotent verifies that a second approve call on a submission that
// already has a FinalGrade does NOT write a second FinalGrade or audit event, but
// DOES re-publish the approve command and returns 200 with the existing FinalGrade.
func TestApprove_Idempotent(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	// Submission still in teacher_review (orchestrator hasn't advanced it yet).
	sub := fakeApprovalStore.seedSubmission(tenantID, contracts.StateTeacherReview)
	makeGradedPaperForApproval(t, fakeObjects, tenantID, sub.ID)

	// Pre-seed an existing FinalGrade (simulates a previous approve that completed
	// DB writes but the client never received the 200).
	existingFG := store.FinalGrade{
		ID:           uuid.New(),
		TenantID:     tenantID,
		SubmissionID: sub.ID,
		Total:        10,
		MaxTotal:     15,
		Score100:     66.7,
		GradedKey:    fmt.Sprintf("%s/%s/graded.final.json", tenantID, sub.ID),
		ApprovedBy:   "teacher-1",
		ApprovedAt:   time.Now(),
		CreatedAt:    time.Now(),
	}
	fakeApprovalStore.preloadedFinalGrades[sub.ID] = existingFG

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Idempotent path: one audit event (approve re-publish) but no second FinalGrade.
	assert.Equal(t, 1, fakeApprovalStore.auditCount(), "idempotent path must write one audit event")
	assert.Equal(t, 0, fakeApprovalStore.finalGradeCount(), "idempotent path must not write a second FinalGrade")

	// The approve command MUST be re-published.
	require.Equal(t, 1, fakeBus.publishedCount())
	msg := fakeBus.latestMessage()
	assert.Equal(t, "commands.q", msg.queue)
	assert.Equal(t, contracts.StageApprove, msg.env.Stage)
	assert.Equal(t, sub.ID.String(), msg.env.SubmissionID)

	// The response must be the existing FinalGrade.
	var fg store.FinalGrade
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fg))
	assert.Equal(t, existingFG.ID, fg.ID)
	assert.Equal(t, existingFG.SubmissionID, fg.SubmissionID)
}

// TestPublish_CrossTenant verifies 404 for cross-tenant access to publish.
func TestPublish_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeApprovalStore := newApprovalFakeStore()
	fakeObjects := NewFakeObjectStore()
	fakeBus := &ApprovalFakeBus{}

	sub := fakeApprovalStore.seedSubmission(tenantA, contracts.StateApproved)

	h := &api.ApprovalHandlers{
		Store:      fakeApprovalStore,
		Objects:    fakeObjects,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/publish", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildApprovalRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 0, fakeBus.publishedCount())
}
