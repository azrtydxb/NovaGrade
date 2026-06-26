package api_test

// Tests for:
//   POST /v1/outcomes
//   GET  /v1/outcomes
//   POST /v1/assessment-versions/{avid}/question-outcomes
//   GET  /v1/assessment-versions/{avid}/question-outcomes

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
)

// ─────────────────────────────────────────────────────────────────────────────
// CurriculumFakeStore — implements api.CurriculumStore
// ─────────────────────────────────────────────────────────────────────────────

type CurriculumFakeStore struct {
	mu        sync.Mutex
	outcomes  map[uuid.UUID]store.CurriculumOutcome
	mappings  map[uuid.UUID]store.QuestionOutcome
	avTenants map[uuid.UUID]uuid.UUID // avid → owning tenant
}

func newCurriculumFakeStore() *CurriculumFakeStore {
	return &CurriculumFakeStore{
		outcomes:  make(map[uuid.UUID]store.CurriculumOutcome),
		mappings:  make(map[uuid.UUID]store.QuestionOutcome),
		avTenants: make(map[uuid.UUID]uuid.UUID),
	}
}

func (f *CurriculumFakeStore) CreateOutcome(_ context.Context, p store.CreateOutcomeParams) (store.CurriculumOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.outcomes {
		if o.TenantID == p.TenantID && o.Code == p.Code {
			return store.CurriculumOutcome{}, fmt.Errorf("duplicate outcome code %q: %w", p.Code, store.ErrDuplicate)
		}
	}
	o := store.CurriculumOutcome{
		ID:          uuid.New(),
		TenantID:    p.TenantID,
		Code:        p.Code,
		Description: p.Description,
		Subject:     p.Subject,
		CreatedAt:   time.Now(),
	}
	f.outcomes[o.ID] = o
	return o, nil
}

func (f *CurriculumFakeStore) ListOutcomes(_ context.Context, tenantID uuid.UUID) ([]store.CurriculumOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]store.CurriculumOutcome, 0)
	for _, o := range f.outcomes {
		if o.TenantID == tenantID {
			result = append(result, o)
		}
	}
	return result, nil
}

func (f *CurriculumFakeStore) GetOutcome(_ context.Context, tenantID, id uuid.UUID) (store.CurriculumOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.outcomes[id]
	if !ok || o.TenantID != tenantID {
		return store.CurriculumOutcome{}, fmt.Errorf("GetOutcome %s: %w", id, store.ErrNotFound)
	}
	return o, nil
}

func (f *CurriculumFakeStore) MapQuestionOutcome(_ context.Context, p store.MapQuestionOutcomeParams) (store.QuestionOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.mappings {
		if m.TenantID == p.TenantID && m.AssessmentVersionID == p.AssessmentVersionID &&
			m.QuestionNo == p.QuestionNo && m.OutcomeID == p.OutcomeID {
			return store.QuestionOutcome{}, fmt.Errorf("duplicate mapping for question %q: %w", p.QuestionNo, store.ErrDuplicate)
		}
	}
	q := store.QuestionOutcome{
		ID:                  uuid.New(),
		TenantID:            p.TenantID,
		AssessmentVersionID: p.AssessmentVersionID,
		QuestionNo:          p.QuestionNo,
		OutcomeID:           p.OutcomeID,
		CreatedAt:           time.Now(),
	}
	f.mappings[q.ID] = q
	return q, nil
}

func (f *CurriculumFakeStore) ListQuestionOutcomes(_ context.Context, tenantID, avid uuid.UUID) ([]store.QuestionOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]store.QuestionOutcome, 0)
	for _, q := range f.mappings {
		if q.TenantID == tenantID && q.AssessmentVersionID == avid {
			result = append(result, q)
		}
	}
	return result, nil
}

func (f *CurriculumFakeStore) GetAssessmentVersionTenantID(_ context.Context, avid uuid.UUID) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.avTenants[avid]
	if !ok {
		return uuid.UUID{}, fmt.Errorf("GetAssessmentVersionTenantID %s: %w", avid, store.ErrNotFound)
	}
	return t, nil
}

// seedOutcome inserts an outcome directly and returns it.
func (f *CurriculumFakeStore) seedOutcome(tenantID uuid.UUID, code string) store.CurriculumOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := store.CurriculumOutcome{
		ID:          uuid.New(),
		TenantID:    tenantID,
		Code:        code,
		Description: "desc " + code,
		Subject:     "math",
		CreatedAt:   time.Now(),
	}
	f.outcomes[o.ID] = o
	return o
}

// seedAV registers an avid as owned by the given tenant.
func (f *CurriculumFakeStore) seedAV(tenantID uuid.UUID) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	avid := uuid.New()
	f.avTenants[avid] = tenantID
	return avid
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildCurriculumRouter(t *testing.T, h *api.CurriculumHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/outcomes", h.CreateOutcome)
	r.Get("/v1/outcomes", h.ListOutcomes)
	r.Post("/v1/assessment-versions/{avid}/question-outcomes", h.MapQuestionOutcome)
	r.Get("/v1/assessment-versions/{avid}/question-outcomes", h.ListQuestionOutcomes)
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCurriculumCreateOutcome_Created verifies a role with ActionEditTunables
// can create an outcome → 201.
func TestCurriculumCreateOutcome_Created(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	body := `{"code":"MA1.1","description":"Add integers","subject":"math"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/outcomes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "MA1.1", resp["code"])
	assert.Equal(t, "Add integers", resp["description"])
	assert.Equal(t, "math", resp["subject"])
	assert.NotEmpty(t, resp["id"])
}

// TestCurriculumListOutcomes_Empty verifies GET /v1/outcomes returns 200 with an
// empty JSON array when there are no outcomes.
func TestCurriculumListOutcomes_Empty(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleOperator},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/outcomes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.JSONEq(t, `[]`, rec.Body.String())
}

// TestCurriculumCreateOutcome_NoPermission verifies a teacher (lacks
// ActionEditTunables) gets 404.
func TestCurriculumCreateOutcome_NoPermission(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	principal := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	tok := issueToken(t, principal)

	body := `{"code":"MA1.1","description":"Add integers","subject":"math"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/outcomes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "teacher must not create outcomes")
}

// TestCurriculumMapQuestion_Created verifies mapping a question to an outcome
// (both owned by the caller's tenant) → 201.
func TestCurriculumMapQuestion_Created(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	outcome := fakeStore.seedOutcome(tenantID, "MA1.1")
	avid := fakeStore.seedAV(tenantID)

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	body := fmt.Sprintf(`{"question_no":"1a","outcome_id":%q}`, outcome.ID.String())
	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/question-outcomes",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "1a", resp["question_no"])
	assert.Equal(t, outcome.ID.String(), resp["outcome_id"])
	assert.Equal(t, avid.String(), resp["assessment_version_id"])
}

// TestCurriculumMapQuestion_ForeignOutcome verifies that mapping to an outcome
// owned by tenant B (caller is tenant A) → 404.
func TestCurriculumMapQuestion_ForeignOutcome(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	foreignOutcome := fakeStore.seedOutcome(tenantB, "MA1.1")
	avid := fakeStore.seedAV(tenantA)

	principal := auth.Principal{
		ID:       "admin-a",
		TenantID: tenantA.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	body := fmt.Sprintf(`{"question_no":"1a","outcome_id":%q}`, foreignOutcome.ID.String())
	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/question-outcomes",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "must not map to another tenant's outcome")
}

// TestCurriculumListOutcomes_WithData verifies GET /v1/outcomes returns the
// seeded outcome in the response body (non-empty list).
func TestCurriculumListOutcomes_WithData(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	seeded := fakeStore.seedOutcome(tenantID, "SC2.3")

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	req := httptest.NewRequest(http.MethodGet, "/v1/outcomes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "expected exactly one outcome")
	assert.Equal(t, seeded.Code, resp[0]["code"])
	assert.Equal(t, seeded.Description, resp[0]["description"])
}

// TestCurriculumListQuestionOutcomes_WithMapping verifies that
// GET /v1/assessment-versions/{avid}/question-outcomes returns the mapping
// that was previously created via POST.
func TestCurriculumListQuestionOutcomes_WithMapping(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	outcome := fakeStore.seedOutcome(tenantID, "MA3.1")
	avid := fakeStore.seedAV(tenantID)

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	// Seed a mapping via POST.
	body := fmt.Sprintf(`{"question_no":"2b","outcome_id":%q}`, outcome.ID.String())
	postReq := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/question-outcomes",
		bytes.NewBufferString(body))
	postReq.Header.Set("Authorization", "Bearer "+tok)
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(postRec, postReq)
	require.Equal(t, http.StatusCreated, postRec.Code, "POST body: %s", postRec.Body.String())

	// Now GET the list.
	getReq := httptest.NewRequest(http.MethodGet,
		"/v1/assessment-versions/"+avid.String()+"/question-outcomes",
		nil)
	getReq.Header.Set("Authorization", "Bearer "+tok)
	getRec := httptest.NewRecorder()
	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(getRec, getReq)

	require.Equal(t, http.StatusOK, getRec.Code, "GET body: %s", getRec.Body.String())

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "expected exactly one mapping")
	assert.Equal(t, "2b", resp[0]["question_no"])
	assert.Equal(t, outcome.ID.String(), resp[0]["outcome_id"])
}

// TestCurriculumMapQuestion_CrossTenantAVID verifies that mapping against an
// avid owned by tenant B (caller is tenant A) → 404.
func TestCurriculumMapQuestion_CrossTenantAVID(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	outcome := fakeStore.seedOutcome(tenantA, "MA1.1")
	foreignAVID := fakeStore.seedAV(tenantB)

	principal := auth.Principal{
		ID:       "admin-a",
		TenantID: tenantA.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	body := fmt.Sprintf(`{"question_no":"1a","outcome_id":%q}`, outcome.ID.String())
	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+foreignAVID.String()+"/question-outcomes",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "must not map against another tenant's AVID")
}

// TestCurriculumMapQuestion_DuplicateMapping_409 verifies that posting the same
// (question_no, outcome_id) pair for the same assessment version twice returns
// HTTP 409 Conflict on the second request.
func TestCurriculumMapQuestion_DuplicateMapping_409(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	fakeStore := newCurriculumFakeStore()
	h := &api.CurriculumHandlers{Store: fakeStore, DeployMode: "onprem"}

	outcome := fakeStore.seedOutcome(tenantID, "MA2.1")
	avid := fakeStore.seedAV(tenantID)

	principal := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}
	tok := issueToken(t, principal)

	body := fmt.Sprintf(`{"question_no":"3c","outcome_id":%q}`, outcome.ID.String())

	doPost := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost,
			"/v1/assessment-versions/"+avid.String()+"/question-outcomes",
			bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		buildCurriculumRouter(t, h, auth.NewAPIKeyResolver()).ServeHTTP(rec, req)
		return rec
	}

	// First request must succeed.
	rec1 := doPost()
	require.Equal(t, http.StatusCreated, rec1.Code, "first mapping should succeed, body: %s", rec1.Body.String())

	// Second identical request must return 409 Conflict.
	rec2 := doPost()
	assert.Equal(t, http.StatusConflict, rec2.Code, "duplicate mapping must return 409, body: %s", rec2.Body.String())
}
