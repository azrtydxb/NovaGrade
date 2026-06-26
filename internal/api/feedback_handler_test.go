package api_test

// Tests for:
//   POST /v1/submissions/{id}/feedback/regenerate

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
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// FeedbackFakeStore — implements api.FeedbackStore
// ─────────────────────────────────────────────────────────────────────────────

type FeedbackFakeStore struct {
	mu          sync.Mutex
	submissions map[uuid.UUID]store.Submission
	auditEvents []store.AuditEvent
	failAudit   bool
}

func newFeedbackFakeStore() *FeedbackFakeStore {
	return &FeedbackFakeStore{
		submissions: make(map[uuid.UUID]store.Submission),
	}
}

func (f *FeedbackFakeStore) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.submissions[id]
	if !ok {
		return store.Submission{}, fmt.Errorf("GetSubmission %s: %w", id, store.ErrNotFound)
	}
	return sub, nil
}

func (f *FeedbackFakeStore) InsertAuditEvent(_ context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error) {
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
		Reason:     p.Reason,
		CreatedAt:  time.Now(),
	}
	f.auditEvents = append(f.auditEvents, ev)
	return ev, nil
}

func (f *FeedbackFakeStore) seedSubmission(tenantID uuid.UUID, state contracts.SubmissionState) store.Submission {
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

func (f *FeedbackFakeStore) latestAuditAction() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auditEvents) == 0 {
		return ""
	}
	return f.auditEvents[len(f.auditEvents)-1].Action
}

func (f *FeedbackFakeStore) auditCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.auditEvents)
}

// ─────────────────────────────────────────────────────────────────────────────
// FeedbackFakeObjects — implements api.ObjectStore
// ─────────────────────────────────────────────────────────────────────────────

type FeedbackFakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFeedbackFakeObjects() *FeedbackFakeObjects {
	return &FeedbackFakeObjects{objects: make(map[string][]byte)}
}

func (f *FeedbackFakeObjects) PutObject(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.objects[key] = cp
	return nil
}

func (f *FeedbackFakeObjects) GetObject(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", key)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

func (f *FeedbackFakeObjects) seedGradedPaper(tenantID, subID uuid.UUID, paper contracts.GradedPaper) {
	key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, subID)
	data, _ := json.Marshal(paper)
	f.PutObject(context.Background(), key, data)
}

func (f *FeedbackFakeObjects) readGradedPaper(tenantID, subID uuid.UUID) (contracts.GradedPaper, error) {
	key := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, subID)
	data, err := f.GetObject(context.Background(), key)
	if err != nil {
		return contracts.GradedPaper{}, err
	}
	var p contracts.GradedPaper
	if err := json.Unmarshal(data, &p); err != nil {
		return contracts.GradedPaper{}, err
	}
	return p, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// feedbackFakeAIProvider — fake AIProvider for feedback handler tests
// ─────────────────────────────────────────────────────────────────────────────

type feedbackFakeAIProvider struct {
	mu        sync.Mutex
	callCount int
}

func (f *feedbackFakeAIProvider) Complete(_ context.Context, req providers.CompletionReq) (providers.CompletionResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	// Return canned response based on prompt_version to differentiate feedback vs revision.
	switch req.PromptVersion {
	case "feedback-v1":
		return providers.CompletionResp{Content: "canned feedback", SchemaValid: true}, nil
	case "revision-v1":
		return providers.CompletionResp{Content: "canned revision", SchemaValid: true}, nil
	default:
		return providers.CompletionResp{Content: "canned response", SchemaValid: true}, nil
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fakeFeedbackRegistry — wraps feedbackFakeAIProvider as a providers.Registry-like
// resolver without hitting real infra.
// ─────────────────────────────────────────────────────────────────────────────

// buildFeedbackRegistry builds a *providers.Registry that always returns the
// given fake provider and model string. ConfigSource is left nil so Registry
// falls back to (Fallback, FallbackModel) immediately.
func buildFeedbackRegistry(prov providers.AIProvider, model string) *providers.Registry {
	return &providers.Registry{
		Source:        nil, // nil → always use fallback
		Fallback:      prov,
		FallbackModel: model,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildFeedbackRouter(t *testing.T, h *api.FeedbackHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/submissions/{id}/feedback/regenerate", h.Regenerate)
	return r
}

// buildTestGradedPaperForFeedback creates a small paper with one question,
// optionally with pre-existing feedback and revision.
func buildTestGradedPaperForFeedback(feedback, revision string) contracts.GradedPaper {
	return contracts.GradedPaper{
		Subject:   "Mathematics",
		SourcePDF: "test.pdf",
		Questions: []contracts.GradedQuestion{
			{
				QuestionNo:    "1",
				MaxMarks:      10,
				AwardedMarks:  7,
				StudentAnswer: "Partial answer",
				Justification: "Partially correct",
				Flags:         []string{},
				Feedback:      feedback,
				Revision:      revision,
			},
		},
		SectionTotals: map[string]float64{},
		Total:         7,
		MaxTotal:      10,
		Score100:      70,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestRegenerate_TeacherReview_200 verifies that regenerating on a teacher_review
// submission returns 200, rewrites both Feedback and Revision (clearing prior
// values), writes an "regenerate_feedback" audit event, and leaves marks unchanged.
func TestRegenerate_TeacherReview_200(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newFeedbackFakeStore()
	fakeObjects := newFeedbackFakeObjects()
	fakeProv := &feedbackFakeAIProvider{}
	registry := buildFeedbackRegistry(fakeProv, "test-model")

	sub := fakeStore.seedSubmission(tenantID, contracts.StateTeacherReview)

	// Seed a graded paper with stale feedback and revision.
	paper := buildTestGradedPaperForFeedback("old feedback", "old revision")
	fakeObjects.seedGradedPaper(tenantID, sub.ID, paper)

	h := &api.FeedbackHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		Registry:   registry,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/submissions/"+sub.ID.String()+"/feedback/regenerate", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildFeedbackRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Decode the returned paper.
	var returned contracts.GradedPaper
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &returned))

	// Marks must be unchanged.
	assert.Equal(t, paper.Total, returned.Total, "Total must not change")
	assert.Equal(t, paper.MaxTotal, returned.MaxTotal, "MaxTotal must not change")
	assert.Equal(t, paper.Score100, returned.Score100, "Score100 must not change")
	require.Len(t, returned.Questions, 1)
	assert.Equal(t, paper.Questions[0].AwardedMarks, returned.Questions[0].AwardedMarks, "awarded_marks must not change")
	assert.Equal(t, paper.Questions[0].MaxMarks, returned.Questions[0].MaxMarks, "max_marks must not change")

	// Fresh Feedback and Revision must be non-empty (regenerated).
	assert.NotEmpty(t, returned.Questions[0].Feedback, "Feedback must be non-empty after regeneration")
	assert.NotEmpty(t, returned.Questions[0].Revision, "Revision must be non-empty after regeneration")

	// Feedback and Revision must NOT be the old stale values.
	assert.NotEqual(t, "old feedback", returned.Questions[0].Feedback)
	assert.NotEqual(t, "old revision", returned.Questions[0].Revision)

	// Object store must have the updated paper.
	stored, err := fakeObjects.readGradedPaper(tenantID, sub.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, stored.Questions[0].Feedback, "stored paper must have feedback")
	assert.NotEmpty(t, stored.Questions[0].Revision, "stored paper must have revision")

	// Audit event "regenerate_feedback" must have been written.
	assert.Equal(t, 1, fakeStore.auditCount(), "exactly one audit event expected")
	assert.Equal(t, "regenerate_feedback", fakeStore.latestAuditAction())
}

// TestRegenerate_Approved_409 verifies that regenerating on an approved
// submission returns 409 (cannot mutate a finalised snapshot).
func TestRegenerate_Approved_409(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newFeedbackFakeStore()
	fakeObjects := newFeedbackFakeObjects()
	registry := buildFeedbackRegistry(&feedbackFakeAIProvider{}, "test-model")

	sub := fakeStore.seedSubmission(tenantID, contracts.StateApproved)

	h := &api.FeedbackHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		Registry:   registry,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/submissions/"+sub.ID.String()+"/feedback/regenerate", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildFeedbackRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code, "approved submissions must return 409")
}

// TestRegenerate_Published_409 verifies that regenerating on a published
// submission returns 409.
func TestRegenerate_Published_409(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newFeedbackFakeStore()
	fakeObjects := newFeedbackFakeObjects()
	registry := buildFeedbackRegistry(&feedbackFakeAIProvider{}, "test-model")

	sub := fakeStore.seedSubmission(tenantID, contracts.StatePublished)

	h := &api.FeedbackHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		Registry:   registry,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/submissions/"+sub.ID.String()+"/feedback/regenerate", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildFeedbackRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code, "published submissions must return 409")
}

// TestRegenerate_Exported_409 verifies that regenerating on an exported
// submission returns 409.
func TestRegenerate_Exported_409(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newFeedbackFakeStore()
	fakeObjects := newFeedbackFakeObjects()
	registry := buildFeedbackRegistry(&feedbackFakeAIProvider{}, "test-model")

	sub := fakeStore.seedSubmission(tenantID, contracts.StateExported)

	h := &api.FeedbackHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		Registry:   registry,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/submissions/"+sub.ID.String()+"/feedback/regenerate", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildFeedbackRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code, "exported submissions must return 409")
}

// TestRegenerate_CrossTenant_404 verifies that a teacher from a different tenant
// cannot regenerate feedback for a submission (returns 404 to prevent enumeration).
func TestRegenerate_CrossTenant_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	ownerTenantID := uuid.New()
	otherTenantID := uuid.New()

	fakeStore := newFeedbackFakeStore()
	fakeObjects := newFeedbackFakeObjects()
	registry := buildFeedbackRegistry(&feedbackFakeAIProvider{}, "test-model")

	// Submission belongs to ownerTenantID.
	sub := fakeStore.seedSubmission(ownerTenantID, contracts.StateTeacherReview)

	h := &api.FeedbackHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		Registry:   registry,
		DeployMode: "onprem",
	}

	// Principal is from a different tenant.
	principal := auth.Principal{
		ID:       "teacher-other",
		TenantID: otherTenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/submissions/"+sub.ID.String()+"/feedback/regenerate", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildFeedbackRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "cross-tenant access must return 404")
}

// TestRegenerate_NoReviewFixApprove_404 verifies that a principal without the
// ReviewFixApprove role cannot access the endpoint (returns 404).
func TestRegenerate_NoReviewFixApprove_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newFeedbackFakeStore()
	fakeObjects := newFeedbackFakeObjects()
	registry := buildFeedbackRegistry(&feedbackFakeAIProvider{}, "test-model")

	sub := fakeStore.seedSubmission(tenantID, contracts.StateTeacherReview)

	h := &api.FeedbackHandlers{
		Store:      fakeStore,
		Objects:    fakeObjects,
		Registry:   registry,
		DeployMode: "onprem",
	}

	// Scanner role cannot perform ReviewFixApprove actions.
	principal := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/submissions/"+sub.ID.String()+"/feedback/regenerate", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	router := buildFeedbackRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "principal without ReviewFixApprove must get 404")
}
