package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
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
// Fakes
// ─────────────────────────────────────────────────────────────────────────────

type FakeStore struct {
	mu          sync.Mutex
	submissions map[uuid.UUID]store.Submission
}

func NewFakeStore() *FakeStore {
	return &FakeStore{submissions: make(map[uuid.UUID]store.Submission)}
}

func (f *FakeStore) CreateSubmission(_ context.Context, p store.CreateSubmissionParams) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := store.Submission{
		ID:                  uuid.New(),
		TenantID:            p.TenantID,
		AssessmentVersionID: p.AssessmentVersionID,
		StudentID:           p.StudentID,
		State:               contracts.StateUploaded,
		SourcePDFKey:        p.SourcePDFKey,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	f.submissions[sub.ID] = sub
	return sub, nil
}

func (f *FakeStore) GetSubmission(_ context.Context, id uuid.UUID) (store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.submissions[id]
	if !ok {
		return store.Submission{}, fmt.Errorf("GetSubmission %s: %w", id, store.ErrNotFound)
	}
	return sub, nil
}

func (f *FakeStore) ListSubmissionsByState(_ context.Context, tenantID uuid.UUID, state contracts.SubmissionState) ([]store.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []store.Submission
	for _, sub := range f.submissions {
		if sub.TenantID != tenantID {
			continue
		}
		if state != "" && sub.State != state {
			continue
		}
		result = append(result, sub)
	}
	return result, nil
}

// SetState is a test helper to set a submission's state directly.
func (f *FakeStore) SetState(id uuid.UUID, state contracts.SubmissionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := f.submissions[id]
	sub.State = state
	f.submissions[id] = sub
}

type FakeBus struct {
	mu       sync.Mutex
	commands []contracts.Envelope
}

func (f *FakeBus) Publish(_ context.Context, _ string, env contracts.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, env)
	return nil
}

func (f *FakeBus) Commands() []contracts.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]contracts.Envelope, len(f.commands))
	copy(out, f.commands)
	return out
}

type FakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func NewFakeObjectStore() *FakeObjectStore {
	return &FakeObjectStore{objects: make(map[string][]byte)}
}

func (f *FakeObjectStore) PutObject(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
	return nil
}

func (f *FakeObjectStore) GetObject(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return data, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

func buildRouter(t *testing.T, h *api.Handlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/submissions", h.PostSubmission)
	r.Get("/v1/submissions", h.ListSubmissions)
	r.Get("/v1/submissions/{id}", h.GetSubmission)
	r.Get("/v1/submissions/{id}/result", h.GetResult)
	return r
}

func makePDFBody(t *testing.T, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	// Set Content-Type: application/pdf on the file part header
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="pdf"; filename="%s"`, filename))
	h.Set("Content-Type", "application/pdf")
	part, err := writer.CreatePart(h)
	require.NoError(t, err)
	part.Write(content)
	writer.Close()
	return body, writer.FormDataContentType()
}

// issueToken issues a JWT for the given principal with JWT_SIGNING_KEY set.
func issueToken(t *testing.T, p auth.Principal) string {
	t.Helper()
	tok, err := auth.IssueToken(p, time.Hour)
	require.NoError(t, err)
	return tok
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestPostSubmission_JWT(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := NewFakeStore()
	fakeBus := &FakeBus{}
	fakeObjects := NewFakeObjectStore()
	h := &api.Handlers{
		Store:      fakeStore,
		Bus:        fakeBus,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	principal := auth.Principal{
		ID:       "user-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body, ct := makePDFBody(t, "exam.pdf", []byte("%PDF-1.4 fake pdf content"))
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["submission_id"])

	// Verify bus got a command
	cmds := fakeBus.Commands()
	require.Len(t, cmds, 1)
	assert.Equal(t, contracts.StageSubmitExam, cmds[0].Stage)
	assert.Equal(t, tenantID.String(), cmds[0].TenantID)
}

func TestPostSubmission_APIKey(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := NewFakeStore()
	fakeBus := &FakeBus{}
	fakeObjects := NewFakeObjectStore()
	h := &api.Handlers{
		Store:      fakeStore,
		Bus:        fakeBus,
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	scanner := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("scanner-api-key", scanner)
	router := buildRouter(t, h, resolver)

	body, ct := makePDFBody(t, "exam.pdf", []byte("%PDF-1.4 fake pdf content"))
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions", body)
	req.Header.Set("X-API-Key", "scanner-api-key")
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestPostSubmission_Unauthorized(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	h := &api.Handlers{
		Store:      NewFakeStore(),
		Bus:        &FakeBus{},
		Objects:    NewFakeObjectStore(),
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	body, ct := makePDFBody(t, "exam.pdf", []byte("%PDF-1.4"))
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestPostSubmission_Forbidden_Reviewer(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.Handlers{
		Store:      NewFakeStore(),
		Bus:        &FakeBus{},
		Objects:    NewFakeObjectStore(),
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	// Reviewer cannot submit exam
	principal := auth.Principal{
		ID:       "reviewer-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleReviewer},
	}
	tok := issueToken(t, principal)

	body, ct := makePDFBody(t, "exam.pdf", []byte("%PDF-1.4"))
	req := httptest.NewRequest(http.MethodPost, "/v1/submissions", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestGetSubmission_Found(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := NewFakeStore()
	h := &api.Handlers{
		Store:      fakeStore,
		Bus:        &FakeBus{},
		Objects:    NewFakeObjectStore(),
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	// Pre-create a submission in the store
	sub, err := fakeStore.CreateSubmission(context.Background(), store.CreateSubmissionParams{
		TenantID: tenantID,
	})
	require.NoError(t, err)

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetSubmission_NotFound(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.Handlers{
		Store:      NewFakeStore(),
		Bus:        &FakeBus{},
		Objects:    NewFakeObjectStore(),
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetSubmission_CrossTenant(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := NewFakeStore()
	h := &api.Handlers{
		Store:      fakeStore,
		Bus:        &FakeBus{},
		Objects:    NewFakeObjectStore(),
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	// Submission belongs to tenantA
	sub, err := fakeStore.CreateSubmission(context.Background(), store.CreateSubmissionParams{
		TenantID: tenantA,
	})
	require.NoError(t, err)

	// Request from tenantB
	principal := auth.Principal{
		ID:       "teacher-b",
		TenantID: tenantB.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// Should return 404 (not 403) to avoid tenant enumeration
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListSubmissions(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := NewFakeStore()
	h := &api.Handlers{
		Store:      fakeStore,
		Bus:        &FakeBus{},
		Objects:    NewFakeObjectStore(),
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	// Create two submissions
	_, err := fakeStore.CreateSubmission(context.Background(), store.CreateSubmissionParams{TenantID: tenantID})
	require.NoError(t, err)
	_, err = fakeStore.CreateSubmission(context.Background(), store.CreateSubmissionParams{TenantID: tenantID})
	require.NoError(t, err)

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var subs []store.Submission
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &subs))
	assert.Len(t, subs, 2)
}

func TestGetResult_NotReady(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := NewFakeStore()
	h := &api.Handlers{
		Store:      fakeStore,
		Bus:        &FakeBus{},
		Objects:    NewFakeObjectStore(),
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	sub, err := fakeStore.CreateSubmission(context.Background(), store.CreateSubmissionParams{TenantID: tenantID})
	require.NoError(t, err)
	// State is uploaded — result not ready

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/result", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestGetResult_Ready(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := NewFakeStore()
	fakeObjects := NewFakeObjectStore()
	h := &api.Handlers{
		Store:      fakeStore,
		Bus:        &FakeBus{},
		Objects:    fakeObjects,
		DeployMode: "onprem",
	}
	resolver := auth.NewAPIKeyResolver()
	router := buildRouter(t, h, resolver)

	sub, err := fakeStore.CreateSubmission(context.Background(), store.CreateSubmissionParams{TenantID: tenantID})
	require.NoError(t, err)

	// Set state to approved so result is available
	fakeStore.SetState(sub.ID, contracts.StateApproved)

	// Pre-store the graded result
	gradedKey := fmt.Sprintf("%s/%s/graded.v1.json", tenantID, sub.ID)
	resultData := []byte(`{"score": 95}`)
	require.NoError(t, fakeObjects.PutObject(context.Background(), gradedKey, resultData))

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/submissions/"+sub.ID.String()+"/result", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, string(resultData), rec.Body.String())
}

func TestJWT_RoundTrip(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	original := auth.Principal{
		ID:       "user-42",
		TenantID: uuid.New().String(),
		Roles:    []domain.Role{domain.RoleTeacher, domain.RoleReviewer},
	}
	tok, err := auth.IssueToken(original, time.Hour)
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	got, err := auth.VerifyToken(tok)
	require.NoError(t, err)
	assert.Equal(t, original.ID, got.ID)
	assert.Equal(t, original.TenantID, got.TenantID)
	assert.Equal(t, original.Roles, got.Roles)
}

func TestJWT_Expired(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	p := auth.Principal{
		ID:       "user-1",
		TenantID: uuid.New().String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	tok, err := auth.IssueToken(p, -time.Second) // already expired
	require.NoError(t, err)

	_, err = auth.VerifyToken(tok)
	assert.Error(t, err)
}

func TestAPIKey_Resolve(t *testing.T) {
	resolver := auth.NewAPIKeyResolver()
	tenantID := uuid.New().String()
	scanner := auth.Principal{
		ID:       "scanner-device-1",
		TenantID: tenantID,
		Roles:    []domain.Role{domain.RoleScanner},
	}
	resolver.Register("my-secret-key", scanner)

	got, err := resolver.Resolve("my-secret-key")
	require.NoError(t, err)
	assert.Equal(t, scanner, got)

	_, err = resolver.Resolve("wrong-key")
	assert.ErrorIs(t, err, auth.ErrUnauthorized)
}
