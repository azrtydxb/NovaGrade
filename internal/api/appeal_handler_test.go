package api_test

// Tests for:
//   POST /v1/submissions/{id}/appeals
//   GET  /v1/appeals?status=
//   POST /v1/appeals/{id}/resolve
//   POST /v1/appeals/{id}/regrade

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
// AppealFakeStore — implements api.AppealStore
// ─────────────────────────────────────────────────────────────────────────────

type AppealFakeStore struct {
	mu          sync.Mutex
	submissions map[uuid.UUID]store.Submission
	appeals     map[uuid.UUID]store.Appeal
	auditEvents []store.AuditEvent
	failAudit   bool
}

func newAppealFakeStore() *AppealFakeStore {
	return &AppealFakeStore{
		submissions: make(map[uuid.UUID]store.Submission),
		appeals:     make(map[uuid.UUID]store.Appeal),
	}
}

func (f *AppealFakeStore) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.submissions[id]
	if !ok {
		return store.Submission{}, fmt.Errorf("GetSubmission %s: %w", id, store.ErrNotFound)
	}
	return sub, nil
}

func (f *AppealFakeStore) InsertAuditEvent(_ context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error) {
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

func (f *AppealFakeStore) CreateAppeal(_ context.Context, p store.CreateAppealParams) (store.Appeal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := store.Appeal{
		ID:           uuid.New(),
		TenantID:     p.TenantID,
		SubmissionID: p.SubmissionID,
		Status:       "open",
		Reason:       p.Reason,
		RequestedBy:  p.RequestedBy,
		Resolution:   "",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	f.appeals[a.ID] = a
	return a, nil
}

func (f *AppealFakeStore) ListAppeals(_ context.Context, tenantID uuid.UUID, status string) ([]store.Appeal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []store.Appeal
	for _, a := range f.appeals {
		if a.TenantID != tenantID {
			continue
		}
		if status != "" && a.Status != status {
			continue
		}
		result = append(result, a)
	}
	return result, nil
}

func (f *AppealFakeStore) GetAppeal(_ context.Context, tenantID, id uuid.UUID) (store.Appeal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.appeals[id]
	if !ok || a.TenantID != tenantID {
		return store.Appeal{}, fmt.Errorf("GetAppeal %s: %w", id, store.ErrNotFound)
	}
	return a, nil
}

func (f *AppealFakeStore) UpdateAppealStatus(_ context.Context, tenantID, id uuid.UUID, status, resolution string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.appeals[id]
	if !ok || a.TenantID != tenantID {
		return fmt.Errorf("UpdateAppealStatus %s: %w", id, store.ErrNotFound)
	}
	a.Status = status
	a.Resolution = resolution
	a.UpdatedAt = time.Now()
	f.appeals[id] = a
	return nil
}

// seedSubmission inserts a submission and returns it.
func (f *AppealFakeStore) seedSubmission(tenantID uuid.UUID, state contracts.SubmissionState) store.Submission {
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

// seedAppeal inserts an appeal directly and returns it.
func (f *AppealFakeStore) seedAppeal(tenantID, submissionID uuid.UUID, status string) store.Appeal {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := store.Appeal{
		ID:           uuid.New(),
		TenantID:     tenantID,
		SubmissionID: submissionID,
		Status:       status,
		Reason:       "Test reason",
		RequestedBy:  "student@school.com",
		Resolution:   "",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	f.appeals[a.ID] = a
	return a
}

func (f *AppealFakeStore) latestAuditAction() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auditEvents) == 0 {
		return ""
	}
	return f.auditEvents[len(f.auditEvents)-1].Action
}

func (f *AppealFakeStore) auditCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.auditEvents)
}

func (f *AppealFakeStore) getAppealStatus(id uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appeals[id].Status
}

// ─────────────────────────────────────────────────────────────────────────────
// AppealFakeBus — records published envelopes
// ─────────────────────────────────────────────────────────────────────────────

type AppealFakeBus struct {
	mu       sync.Mutex
	messages []appealMsg
}

type appealMsg struct {
	queue string
	env   contracts.Envelope
}

func (b *AppealFakeBus) Publish(_ context.Context, queue string, env contracts.Envelope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, appealMsg{queue: queue, env: env})
	return nil
}

func (b *AppealFakeBus) publishedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messages)
}

func (b *AppealFakeBus) latestMessage() appealMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messages[len(b.messages)-1]
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper for appeal endpoints
// ─────────────────────────────────────────────────────────────────────────────

func buildAppealRouter(t *testing.T, h *api.AppealHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/submissions/{id}/appeals", h.FileAppeal)
	r.Get("/v1/appeals", h.ListAppeals)
	r.Post("/v1/appeals/{id}/resolve", h.ResolveAppeal)
	r.Post("/v1/appeals/{id}/regrade", h.RegradeAppeal)
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: POST /v1/submissions/{id}/appeals
// ─────────────────────────────────────────────────────────────────────────────

// TestFileAppeal_CreatesOpen verifies that filing an appeal creates it with status "open".
func TestFileAppeal_CreatesOpen(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAppealFakeStore()
	fakeBus := &AppealFakeBus{}

	sub := fakeStore.seedSubmission(tenantID, contracts.StateApproved)

	h := &api.AppealHandlers{
		Store:      fakeStore,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "student-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body := `{"reason":"My score looks wrong"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/appeals",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router := buildAppealRouter(t, h, auth.NewAPIKeyResolver())
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var appeal store.Appeal
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &appeal))
	assert.Equal(t, "open", appeal.Status, "newly filed appeal must be open")
	assert.Equal(t, sub.ID, appeal.SubmissionID)
	assert.Equal(t, tenantID, appeal.TenantID)
	assert.Equal(t, "My score looks wrong", appeal.Reason)
	assert.Equal(t, "student-1", appeal.RequestedBy)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: GET /v1/appeals?status=
// ─────────────────────────────────────────────────────────────────────────────

// TestListAppeals_ByStatus verifies that listing by status filters correctly.
func TestListAppeals_ByStatus(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAppealFakeStore()
	fakeBus := &AppealFakeBus{}
	subID := uuid.New()

	// Seed two appeals: one open, one resolved.
	fakeStore.seedAppeal(tenantID, subID, "open")
	fakeStore.seedAppeal(tenantID, subID, "resolved")

	h := &api.AppealHandlers{
		Store:      fakeStore,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	// Filter by open.
	req := httptest.NewRequest(http.MethodGet, "/v1/appeals?status=open", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	buildAppealRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var openAppeals []store.Appeal
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &openAppeals))
	assert.Len(t, openAppeals, 1, "should return only 1 open appeal")
	if len(openAppeals) > 0 {
		assert.Equal(t, "open", openAppeals[0].Status)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: POST /v1/appeals/{id}/resolve
// ─────────────────────────────────────────────────────────────────────────────

// TestResolveAppeal_UpdatesStatus verifies resolve → 200, audit action="appeal_resolve",
// UpdateAppealStatus called.
func TestResolveAppeal_UpdatesStatus(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAppealFakeStore()
	fakeBus := &AppealFakeBus{}
	subID := uuid.New()

	appeal := fakeStore.seedAppeal(tenantID, subID, "open")

	h := &api.AppealHandlers{
		Store:      fakeStore,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body := `{"status":"resolved","resolution":"Marks were correct"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/appeals/"+appeal.ID.String()+"/resolve",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildAppealRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, fakeStore.auditCount(), "should have one audit event")
	assert.Equal(t, "appeal_resolve", fakeStore.latestAuditAction())
	assert.Equal(t, "resolved", fakeStore.getAppealStatus(appeal.ID))
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: POST /v1/appeals/{id}/regrade
// ─────────────────────────────────────────────────────────────────────────────

// TestRegradeAppeal_PublishesCommand verifies:
//   - Returns 200
//   - Publishes command to commands.q with Stage="regrade"
//   - Audit action="appeal_regrade"
//   - Appeal status updated to "under_review"
func TestRegradeAppeal_PublishesCommand(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAppealFakeStore()
	fakeBus := &AppealFakeBus{}
	subID := uuid.New()

	appeal := fakeStore.seedAppeal(tenantID, subID, "open")

	h := &api.AppealHandlers{
		Store:      fakeStore,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodPost, "/v1/appeals/"+appeal.ID.String()+"/regrade", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildAppealRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Command must be published with Stage="regrade".
	require.Equal(t, 1, fakeBus.publishedCount(), "should publish one regrade command")
	msg := fakeBus.latestMessage()
	assert.Equal(t, "commands.q", msg.queue)
	assert.Equal(t, contracts.StageRegrade, msg.env.Stage)
	assert.Equal(t, subID.String(), msg.env.SubmissionID)
	assert.Equal(t, tenantID.String(), msg.env.TenantID)

	// Audit event must be appeal_regrade.
	assert.Equal(t, 1, fakeStore.auditCount())
	assert.Equal(t, "appeal_regrade", fakeStore.latestAuditAction())

	// Appeal status must be "under_review".
	assert.Equal(t, "under_review", fakeStore.getAppealStatus(appeal.ID))
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: cross-tenant isolation
// ─────────────────────────────────────────────────────────────────────────────

// TestGetAppeal_CrossTenant_404 verifies that an appeal for tenantA cannot be
// accessed with tenantB credentials.
func TestGetAppeal_CrossTenant_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := newAppealFakeStore()
	fakeBus := &AppealFakeBus{}

	// Seed appeal for tenantA.
	fakeStore.seedAppeal(tenantA, uuid.New(), "open")

	// Seed submission for tenantA in fakeStore so the FileAppeal route can look it up.
	// For this test we test the resolve endpoint with tenantB.
	appeal := fakeStore.seedAppeal(tenantA, uuid.New(), "open")

	h := &api.AppealHandlers{
		Store:      fakeStore,
		Bus:        fakeBus,
		DeployMode: "onprem",
	}

	// Access with tenantB.
	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body := `{"status":"resolved","resolution":"Nope"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/appeals/"+appeal.ID.String()+"/resolve",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildAppealRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: RBAC
// ─────────────────────────────────────────────────────────────────────────────

// TestRBACFileAppeal_ScannerForbidden verifies that a scanner (lacks ActionViewResults
// on submissions) gets 404 when filing an appeal.
func TestRBACFileAppeal_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAppealFakeStore()
	fakeBus := &AppealFakeBus{}

	sub := fakeStore.seedSubmission(tenantID, contracts.StateApproved)

	h := &api.AppealHandlers{
		Store:      fakeStore,
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

	body := `{"reason":"I want a regrade"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions/"+sub.ID.String()+"/appeals",
		bytes.NewBufferString(body))
	req.Header.Set("X-API-Key", "scanner-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildAppealRouter(t, h, resolver).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "scanner must not file appeals")
}

// TestRBACRegrade_ScannerForbidden verifies that a scanner (lacks ActionReviewFixApprove)
// gets 404 when requesting a regrade.
func TestRBACRegrade_ScannerForbidden(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newAppealFakeStore()
	fakeBus := &AppealFakeBus{}

	appeal := fakeStore.seedAppeal(tenantID, uuid.New(), "open")

	h := &api.AppealHandlers{
		Store:      fakeStore,
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

	req := httptest.NewRequest(http.MethodPost, "/v1/appeals/"+appeal.ID.String()+"/regrade", nil)
	req.Header.Set("X-API-Key", "scanner-key")
	rec := httptest.NewRecorder()

	buildAppealRouter(t, h, resolver).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "scanner must not trigger regrade")
	assert.Equal(t, 0, fakeBus.publishedCount(), "no command must be published")
}
