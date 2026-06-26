package api_test

// Tests for:
//   POST /v1/assessment-versions/{avid}/guides       — import guide
//   GET  /v1/assessment-versions/{avid}/guides       — list guide versions
//   GET  /v1/assessment-versions/{avid}/guides/latest — get latest guide
//   POST /v1/guides/{id}/lock                        — lock a guide version
//
// TDD RED → GREEN pattern: tests are written first; the handler is implemented
// to make them pass.
//
// Tenant-scoping approach: all guide store calls are scoped by the principal's
// tenant (from p.TenantID). A guide can only ever be created/read under the
// caller's own tenant, so a principal from tenant B simply operates under its
// own tenant namespace and can never see tenant A's guides even when supplying
// the same assessment_version_id.

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
// GuideFakeStore — behavioural in-memory implementation of api.GuideStore
// ─────────────────────────────────────────────────────────────────────────────

type GuideFakeStore struct {
	mu     sync.Mutex
	guides []store.MarkingGuide
}

func newGuideFakeStore() *GuideFakeStore {
	return &GuideFakeStore{}
}

func (f *GuideFakeStore) InsertGuideVersion(_ context.Context, p store.InsertGuideVersionParams) (store.MarkingGuide, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Compute next version for this (tenant, avid) pair.
	maxVersion := 0
	for _, g := range f.guides {
		if g.TenantID == p.TenantID && g.AssessmentVersionID == p.AssessmentVersionID {
			if g.Version > maxVersion {
				maxVersion = g.Version
			}
		}
	}
	mg := store.MarkingGuide{
		ID:                  uuid.New(),
		TenantID:            p.TenantID,
		AssessmentVersionID: p.AssessmentVersionID,
		Version:             maxVersion + 1,
		Name:                p.Name,
		Content:             p.Content,
		Locked:              false,
		CreatedAt:           time.Now(),
	}
	f.guides = append(f.guides, mg)
	return mg, nil
}

func (f *GuideFakeStore) GetLatestGuide(_ context.Context, tenantID, assessmentVersionID uuid.UUID) (store.MarkingGuide, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var latest store.MarkingGuide
	found := false
	for _, g := range f.guides {
		if g.TenantID == tenantID && g.AssessmentVersionID == assessmentVersionID {
			if !found || g.Version > latest.Version {
				latest = g
				found = true
			}
		}
	}
	if !found {
		return store.MarkingGuide{}, fmt.Errorf("GetLatestGuide: %w", store.ErrNotFound)
	}
	return latest, nil
}

func (f *GuideFakeStore) ListGuideVersions(_ context.Context, tenantID, assessmentVersionID uuid.UUID) ([]store.MarkingGuide, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var result []store.MarkingGuide
	for _, g := range f.guides {
		if g.TenantID == tenantID && g.AssessmentVersionID == assessmentVersionID {
			result = append(result, g)
		}
	}
	return result, nil
}

func (f *GuideFakeStore) LockGuide(_ context.Context, tenantID, guideID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i, g := range f.guides {
		if g.TenantID == tenantID && g.ID == guideID {
			now := time.Now()
			f.guides[i].Locked = true
			f.guides[i].LockedAt = &now
			return nil
		}
	}
	return fmt.Errorf("LockGuide: %w", store.ErrNotFound)
}

// GuideCount returns the total number of guides stored (for assertion).
func (f *GuideFakeStore) GuideCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.guides)
}

// ─────────────────────────────────────────────────────────────────────────────
// Router helper
// ─────────────────────────────────────────────────────────────────────────────

func buildGuideRouter(t *testing.T, h *api.GuideHandlers, resolver *auth.APIKeyResolver) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/assessment-versions/{avid}/guides", h.ImportGuide)
	r.Get("/v1/assessment-versions/{avid}/guides", h.ListGuides)
	r.Get("/v1/assessment-versions/{avid}/guides/latest", h.GetLatestGuide)
	r.Post("/v1/guides/{id}/lock", h.LockGuide)
	return r
}

// adminPrincipal returns a group_admin principal for the given tenant.
func adminPrincipal(tenantID string) auth.Principal {
	return auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID,
		Roles:    []domain.Role{domain.RoleGroupAdmin},
	}
}

// validGuideJSON returns valid guide JSON with Phase-3 match types.
var validGuideJSON = []byte(`{
	"Q1": {"max_marks": 2, "match": "exact", "answer": "Paris"},
	"Q2": {"max_marks": 3, "match": "numeric", "numeric_answer": 9.81},
	"Q3": {
		"max_marks": 4,
		"match": "multi_step",
		"steps": [
			{"match": "exact", "answer": "KE=0.5mv2", "marks": 2},
			{"match": "numeric", "numeric_answer": 50, "marks": 2}
		]
	}
}`)

var invalidGuideJSON_UnknownMatch = []byte(`{
	"Q1": {"max_marks": 2, "match": "fuzzy", "answer": "Paris"}
}`)

var invalidGuideJSON_NumericMissingAnswer = []byte(`{
	"Q5": {"max_marks": 3, "match": "set"}
}`)

// ─────────────────────────────────────────────────────────────────────────────
// Tests: Import (POST /v1/assessment-versions/{avid}/guides)
// ─────────────────────────────────────────────────────────────────────────────

func TestImportGuide_Valid_201(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v1",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["id"])
	assert.Equal(t, float64(1), resp["version"])
	assert.Equal(t, "v1", resp["name"])
	assert.Equal(t, false, resp["locked"])

	// Verify persisted.
	assert.Equal(t, 1, fakeGuideStore.GuideCount())
}

func TestImportGuide_SecondImport_Version2(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	// Import first version.
	req1 := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v1",
		bytes.NewReader(validGuideJSON))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	// Import second version.
	req2 := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v2",
		bytes.NewReader(validGuideJSON))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusCreated, rec2.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Equal(t, float64(2), resp["version"])
	assert.Equal(t, 2, fakeGuideStore.GuideCount())
}

func TestImportGuide_InvalidMatch_400(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides",
		bytes.NewReader(invalidGuideJSON_UnknownMatch))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Q1")  // validation detail
	// Nothing persisted.
	assert.Equal(t, 0, fakeGuideStore.GuideCount())
}

func TestImportGuide_InvalidGuide_SetNoAccept_400(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides",
		bytes.NewReader(invalidGuideJSON_NumericMissingAnswer))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, fakeGuideStore.GuideCount())
}

func TestImportGuide_Unauthenticated_401(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	avid := uuid.New()
	h := &api.GuideHandlers{Store: newGuideFakeStore(), DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestImportGuide_LackingEditTunables_403(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	h := &api.GuideHandlers{Store: newGuideFakeStore(), DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	// scanner does NOT have EditTunables
	scannerP := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner},
	}
	tok := issueToken(t, scannerP)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// 403 on explicit access denial (not a resource lookup — no submission to 404 on)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: List (GET /v1/assessment-versions/{avid}/guides)
// ─────────────────────────────────────────────────────────────────────────────

func TestListGuides_ReturnsVersions(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	// Import two guides.
	for _, name := range []string{"v1", "v2"} {
		req := httptest.NewRequest(http.MethodPost,
			"/v1/assessment-versions/"+avid.String()+"/guides?name="+name,
			bytes.NewReader(validGuideJSON))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code)
	}

	// List them.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/assessment-versions/"+avid.String()+"/guides", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var guides []map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &guides))
	assert.Len(t, guides, 2)
}

func TestListGuides_Empty_ReturnsEmptyArray(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	h := &api.GuideHandlers{Store: newGuideFakeStore(), DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/assessment-versions/"+avid.String()+"/guides", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var guides []interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &guides))
	assert.Len(t, guides, 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: GetLatest (GET /v1/assessment-versions/{avid}/guides/latest)
// ─────────────────────────────────────────────────────────────────────────────

func TestGetLatestGuide_ReturnsContent(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	// Import a guide.
	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v1",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Get latest.
	req2 := httptest.NewRequest(http.MethodGet,
		"/v1/assessment-versions/"+avid.String()+"/guides/latest", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	require.Equal(t, http.StatusOK, rec2.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Equal(t, float64(1), resp["version"])
	assert.Equal(t, "v1", resp["name"])
	// content field should be present
	assert.NotNil(t, resp["content"])
}

func TestGetLatestGuide_None_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	h := &api.GuideHandlers{Store: newGuideFakeStore(), DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/assessment-versions/"+avid.String()+"/guides/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests: Lock (POST /v1/guides/{id}/lock)
// ─────────────────────────────────────────────────────────────────────────────

func TestLockGuide_200(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	// Import a guide to get its id.
	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v1",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var importResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &importResp))
	guideID := importResp["id"].(string)

	// Lock it.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/guides/"+guideID+"/lock", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusOK, rec2.Code)
}

func TestLockGuide_NotFound_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.GuideHandlers{Store: newGuideFakeStore(), DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/guides/"+uuid.New().String()+"/lock", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLockGuide_DoesNotBlockNewVersion(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	// Import v1.
	req1 := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v1",
		bytes.NewReader(validGuideJSON))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	var importResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &importResp))
	guideID := importResp["id"].(string)

	// Lock v1.
	lockReq := httptest.NewRequest(http.MethodPost, "/v1/guides/"+guideID+"/lock", nil)
	lockReq.Header.Set("Authorization", "Bearer "+tok)
	lockRec := httptest.NewRecorder()
	router.ServeHTTP(lockRec, lockReq)
	require.Equal(t, http.StatusOK, lockRec.Code)

	// Import v2 — lock does NOT block new version.
	req2 := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v2",
		bytes.NewReader(validGuideJSON))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusCreated, rec2.Code)

	var resp2 map[string]interface{}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, float64(2), resp2["version"])
	assert.Equal(t, 2, fakeGuideStore.GuideCount())
}

func TestLockGuide_LackingEditTunables_403(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	// First import a guide as an admin.
	adminP := adminPrincipal(tenantID.String())
	adminTok := issueToken(t, adminP)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v1",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+adminTok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var importResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &importResp))
	guideID := importResp["id"].(string)

	// Teacher does NOT have EditTunables.
	teacherP := auth.Principal{
		ID:       "teacher-1",
		TenantID: tenantID.String(),
		Roles:    []domain.Role{domain.RoleTeacher},
	}
	teacherTok := issueToken(t, teacherP)

	lockReq := httptest.NewRequest(http.MethodPost, "/v1/guides/"+guideID+"/lock", nil)
	lockReq.Header.Set("Authorization", "Bearer "+teacherTok)
	lockRec := httptest.NewRecorder()
	router.ServeHTTP(lockRec, lockReq)

	assert.Equal(t, http.StatusForbidden, lockRec.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Cross-tenant isolation
// ─────────────────────────────────────────────────────────────────────────────

func TestGuide_CrossTenant_B_Cannot_Read_A(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantA := uuid.New()
	tenantB := uuid.New()
	avid := uuid.New()                    // same avid for both tenants
	fakeGuideStore := newGuideFakeStore() // shared store (like real DB)
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	// Tenant A imports a guide.
	adminA := auth.Principal{ID: "admin-a", TenantID: tenantA.String(), Roles: []domain.Role{domain.RoleGroupAdmin}}
	tokA := issueToken(t, adminA)
	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=v1",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+tokA)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Tenant B tries to get the latest guide for the same avid — must 404 (scoped by B's tenant).
	adminB := auth.Principal{ID: "admin-b", TenantID: tenantB.String(), Roles: []domain.Role{domain.RoleGroupAdmin}}
	tokB := issueToken(t, adminB)
	req2 := httptest.NewRequest(http.MethodGet,
		"/v1/assessment-versions/"+avid.String()+"/guides/latest", nil)
	req2.Header.Set("Authorization", "Bearer "+tokB)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusNotFound, rec2.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Metadata field helpers used in GetLatest response
// ─────────────────────────────────────────────────────────────────────────────

// TestGetLatestGuide_ResponseFields checks that created_at and locked are present.
func TestGetLatestGuide_ResponseFields(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	avid := uuid.New()
	fakeGuideStore := newGuideFakeStore()
	h := &api.GuideHandlers{Store: fakeGuideStore, DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/"+avid.String()+"/guides?name=exam1",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	req2 := httptest.NewRequest(http.MethodGet,
		"/v1/assessment-versions/"+avid.String()+"/guides/latest", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Contains(t, resp, "created_at")
	assert.Equal(t, false, resp["locked"])
}

// TestImportGuide_InvalidAVID_400 tests that a non-UUID avid returns 400.
func TestImportGuide_InvalidAVID_400(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := uuid.New()
	h := &api.GuideHandlers{Store: newGuideFakeStore(), DeployMode: "onprem"}
	resolver := auth.NewAPIKeyResolver()
	router := buildGuideRouter(t, h, resolver)

	p := adminPrincipal(tenantID.String())
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/assessment-versions/not-a-uuid/guides",
		bytes.NewReader(validGuideJSON))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Ensure GuideFakeStore satisfies api.GuideStore at compile time.
var _ api.GuideStore = (*GuideFakeStore)(nil)
