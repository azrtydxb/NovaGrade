package api_test

// Tests for POST /v1/guides/preview
// TDD RED → GREEN: tests written first, then handler implemented.
//
// The preview endpoint is stateless and hermetic:
//   - No store reads/writes.
//   - No AI provider calls.
//   - RBAC: ActionEditTunables (same as guide management).
//   - Returns per-sample PreviewResult for deterministic match types.
//   - rubric entries and missing question_nos → Previewable=false in results.
//   - Invalid guide → 400.
//   - Unauthenticated → 401.
//   - Lacking EditTunables → 404.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// Router helper (stateless — no store needed)
// ─────────────────────────────────────────────────────────────────────────────

func buildPreviewRouter(t *testing.T, resolver *auth.APIKeyResolver, deployMode string) http.Handler {
	t.Helper()
	ph := &api.GuidePreviewHandlers{DeployMode: deployMode}
	r := chi.NewRouter()
	r.Use(auth.Middleware(resolver))
	r.Post("/v1/guides/preview", ph.Preview)
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Request payloads
// ─────────────────────────────────────────────────────────────────────────────

var previewValidBody = []byte(`{
	"guide": {
		"Q1": {"max_marks": 2, "match": "exact", "answer": "Paris"},
		"Q2": {"max_marks": 4, "match": "numeric", "numeric_answer": 9.81, "tolerance": 0.05},
		"Q3": {"max_marks": 3, "match": "partial", "criteria": [
			{"accept": ["photosynthesis"], "marks": 1},
			{"accept": ["light"], "marks": 1},
			{"accept": ["chlorophyll"], "marks": 1}
		]},
		"Q4": {"max_marks": 2, "match": "multi_step", "steps": [
			{"match": "exact", "answer": "F=ma", "marks": 1},
			{"match": "exact_ci", "answer": "newton", "marks": 1}
		]},
		"Q5": {"max_marks": 5, "match": "rubric", "rubric": "Award marks for quality."}
	},
	"samples": [
		{"question_no": "Q1", "student_answer": "Paris"},
		{"question_no": "Q1", "student_answer": "Lyon"},
		{"question_no": "Q2", "student_answer": "9.82"},
		{"question_no": "Q3", "student_answer": "Plants use photosynthesis and need light."},
		{"question_no": "Q4", "student_answer": "F=ma\nnewton"},
		{"question_no": "Q5", "student_answer": "Some essay."},
		{"question_no": "Q99", "student_answer": "absent question"}
	]
}`)

var previewInvalidGuideBody = []byte(`{
	"guide": {
		"Q1": {"max_marks": 2, "match": "fuzzy", "answer": "Paris"}
	},
	"samples": [
		{"question_no": "Q1", "student_answer": "Paris"}
	]
}`)

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestPreview_ValidGuide_200(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := "00000000-0000-0000-0000-000000000001"
	resolver := auth.NewAPIKeyResolver()
	router := buildPreviewRouter(t, resolver, "onprem")

	p := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID,
		Roles:    []domain.Role{domain.RoleGroupAdmin},
	}
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/guides/preview",
		bytes.NewReader(previewValidBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		Results []map[string]interface{} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 7, "expect one result per sample")

	// Q1 Paris → match → awarded=2
	r0 := resp.Results[0]
	assert.Equal(t, "Q1", r0["question_no"])
	assert.Equal(t, float64(2), r0["awarded"])
	assert.Equal(t, true, r0["previewable"])

	// Q1 Lyon → no match → awarded=0
	r1 := resp.Results[1]
	assert.Equal(t, float64(0), r1["awarded"])
	assert.Equal(t, true, r1["previewable"])

	// Q2 9.82 within tol of 9.81 → awarded=4
	r2 := resp.Results[2]
	assert.Equal(t, float64(4), r2["awarded"])
	assert.Equal(t, true, r2["previewable"])

	// Q3 partial → 2 criteria hit
	r3 := resp.Results[3]
	assert.Equal(t, float64(2), r3["awarded"])
	assert.Equal(t, true, r3["previewable"])

	// Q4 multi_step → both steps → awarded=2
	r4 := resp.Results[4]
	assert.Equal(t, float64(2), r4["awarded"])
	assert.Equal(t, true, r4["previewable"])

	// Q5 rubric → not previewable
	r5 := resp.Results[5]
	assert.Equal(t, "Q5", r5["question_no"])
	assert.Equal(t, false, r5["previewable"])
	assert.Equal(t, float64(0), r5["awarded"])

	// Q99 absent → not previewable
	r6 := resp.Results[6]
	assert.Equal(t, "Q99", r6["question_no"])
	assert.Equal(t, false, r6["previewable"])
}

func TestPreview_InvalidGuide_400(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := "00000000-0000-0000-0000-000000000001"
	resolver := auth.NewAPIKeyResolver()
	router := buildPreviewRouter(t, resolver, "onprem")

	p := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID,
		Roles:    []domain.Role{domain.RoleGroupAdmin},
	}
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/guides/preview",
		bytes.NewReader(previewInvalidGuideBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPreview_Unauthenticated_401(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	resolver := auth.NewAPIKeyResolver()
	router := buildPreviewRouter(t, resolver, "onprem")

	req := httptest.NewRequest(http.MethodPost, "/v1/guides/preview",
		bytes.NewReader(previewValidBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestPreview_LackingEditTunables_404(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := "00000000-0000-0000-0000-000000000001"
	resolver := auth.NewAPIKeyResolver()
	router := buildPreviewRouter(t, resolver, "onprem")

	// scanner does NOT have EditTunables
	p := auth.Principal{
		ID:       "scanner-1",
		TenantID: tenantID,
		Roles:    []domain.Role{domain.RoleScanner},
	}
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/guides/preview",
		bytes.NewReader(previewValidBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// 404, not 403 — prevents role/tenant enumeration (matches guide_handler convention)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPreview_MalformedJSON_400(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := "00000000-0000-0000-0000-000000000001"
	resolver := auth.NewAPIKeyResolver()
	router := buildPreviewRouter(t, resolver, "onprem")

	p := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID,
		Roles:    []domain.Role{domain.RoleGroupAdmin},
	}
	tok := issueToken(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/guides/preview",
		bytes.NewReader([]byte(`not json`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPreview_NoSamples_200EmptyResults(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", "test-secret-key")

	tenantID := "00000000-0000-0000-0000-000000000001"
	resolver := auth.NewAPIKeyResolver()
	router := buildPreviewRouter(t, resolver, "onprem")

	p := auth.Principal{
		ID:       "admin-1",
		TenantID: tenantID,
		Roles:    []domain.Role{domain.RoleGroupAdmin},
	}
	tok := issueToken(t, p)

	body := []byte(`{
		"guide": {"Q1": {"max_marks": 2, "match": "exact", "answer": "Paris"}},
		"samples": []
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/guides/preview",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Results []map[string]interface{} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp.Results, 0)
}
