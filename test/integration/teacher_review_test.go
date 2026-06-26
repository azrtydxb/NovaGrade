package integration

// teacher_review_test.go exercises the full Phase-2 teacher-review lifecycle
// end-to-end against real infrastructure (Postgres, RabbitMQ, MinIO via
// testcontainers), the in-process orchestrator, and the real Phase-2 HTTP
// handlers (ReviewHandlers, ApprovalHandlers, ExportHandlers).
//
// The flow under test:
//
//	teacher_review
//	  → PATCH a question's marks (override)         → 200; GET /review shows override, locked=false
//	  → POST /approve                               → 200; poll until state==approved
//	      → FinalGrade row persisted (override applied) + graded.final.json artifact
//	  → second PATCH override attempt               → 409 (locked: state != teacher_review)
//	  → POST /publish                               → 200; poll until state==published
//	  → POST /export                                → 200; poll until state==exported
//	  → GET /export.csv                             → 200; CSV reflects the overridden marks
//
// Audit assertions: audit_event rows exist for override_question, approve,
// publish, and export.
//
// All async state transitions are awaited via pollSubmission (no fixed sleeps).
// The test is gated by SKIP_DOCKER_TESTS / -short, inherited from startInfra.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chi_middleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// newPhase2APIServer builds an httptest.Server wiring the Phase-2 handlers
// (ReviewHandlers, ApprovalHandlers, ExportHandlers) plus the Phase-1
// submission handlers, all against the same store + bus + object store as the
// pipeline. Routing mirrors cmd/api/main.go exactly.
func newPhase2APIServer(t *testing.T, inf *testInfra) string {
	t.Helper()
	if os.Getenv("JWT_SIGNING_KEY") == "" {
		t.Setenv("JWT_SIGNING_KEY", testSigningKey)
	}

	objAdapter := &objStoreAdapter{s: inf.objStore, bucket: inf.bucket}

	h := &api.Handlers{
		Store:      inf.pgStore,
		Bus:        inf.bus,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}
	rh := &api.ReviewHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}
	ah := &api.ApprovalHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		Bus:        inf.bus,
		DeployMode: "onprem",
	}
	eh := &api.ExportHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}

	r := chi.NewRouter()
	r.Use(chi_middleware.Recoverer)
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(auth.NewAPIKeyResolver()))
		r.Post("/submissions", h.PostSubmission)
		r.Get("/submissions/{id}", h.GetSubmission)
		r.Get("/submissions/{id}/review", rh.GetReview)
		r.Patch("/submissions/{id}/questions/{qno}", rh.PatchQuestion)
		r.Post("/submissions/{id}/approve", ah.Approve)
		r.Post("/submissions/{id}/publish", ah.Publish)
		r.Post("/submissions/{id}/export", ah.Export)
		r.Get("/submissions/{id}/export.csv", eh.ExportCSV)
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: full teacher-review lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// TestTeacherReviewFlow drives a submission to teacher_review (reusing the
// happy-path pipeline), then exercises the full Phase-2 lifecycle through the
// real HTTP handlers and the orchestrator's command consumer.
func TestTeacherReviewFlow(t *testing.T) {
	inf := startInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tok := mintTestJWT(t) // includes RoleTeacher
	fakeAI := newFakeAIServer(t)
	aiProv := newFakeAIProvider(fakeAI)

	// Start the orchestrator + render/transcribe/grade workers. The orchestrator
	// also consumes commands.q, which is what advances approve→published→exported.
	stopPipeline := startPipeline(ctx, t, inf, pipelineConfig{
		transcribeProvider: aiProv,
		gradeProvider:      aiProv,
		gradeModel:         "grade-model",
	})
	defer stopPipeline()

	ensureTenant(t, inf, testTenantID)

	apiURL := newPhase2APIServer(t, inf)
	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err, "read sample PDF")

	// ── 1. Submit and drive to teacher_review ────────────────────────────────
	subID := postSubmission(t, apiURL, tok, pdfBytes)
	t.Logf("teacher review: submission_id=%s", subID)

	finalState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{
			contracts.StateTeacherReview,
			contracts.StateFailed,
		},
		4*time.Minute,
	)
	require.Equal(t, contracts.StateTeacherReview, finalState,
		"submission must reach teacher_review before review actions")

	// The fake transcript yields questions 1a (max 2) and 1b (max 3); the grade
	// model awards 2 marks per question. We override 1a down to 1.0.
	const (
		overrideQno   = "1a"
		overrideMarks = 1.0
	)

	// ── 2. PATCH override question 1a → 200 ──────────────────────────────────
	patchResp := patchQuestionMarks(t, apiURL, tok, subID, overrideQno, overrideMarks, "marking error on 1a")
	require.Equal(t, http.StatusOK, patchResp.status,
		"PATCH override expected 200, got %d: %s", patchResp.status, patchResp.body)

	var updatedQ contracts.GradedQuestion
	require.NoError(t, json.Unmarshal(patchResp.body, &updatedQ), "parse patch response: %s", patchResp.body)
	assert.Equal(t, overrideQno, updatedQ.QuestionNo)
	assert.Equal(t, overrideMarks, updatedQ.AwardedMarks, "patched question must reflect override")

	// ── 3. GET /review shows the override + locked=false ─────────────────────
	review := getReview(t, apiURL, tok, subID)
	assert.False(t, review.Locked, "review must be unlocked while in teacher_review")
	overlaid := questionByNo(t, review.Paper, overrideQno)
	assert.Equal(t, overrideMarks, overlaid.AwardedMarks,
		"GET /review must surface the override for %s", overrideQno)

	// Compute the expected effective total from the overlaid paper so the
	// FinalGrade / CSV assertions are independent of the fake's exact awards.
	var expectedTotal, expectedMax float64
	for _, q := range review.Paper.Questions {
		expectedTotal += q.AwardedMarks
		expectedMax += q.MaxMarks
	}
	require.Greater(t, expectedMax, float64(0), "max total must be > 0")

	// ── 4. POST /approve → 200; poll until state==approved ───────────────────
	approveResp := doPost(t, apiURL, tok, fmt.Sprintf("/v1/submissions/%s/approve", subID))
	require.Equal(t, http.StatusOK, approveResp.status,
		"POST /approve expected 200, got %d: %s", approveResp.status, approveResp.body)

	approvedState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateApproved, contracts.StateFailed},
		90*time.Second,
	)
	require.Equal(t, contracts.StateApproved, approvedState,
		"submission must advance to approved after approve command")

	// FinalGrade row persisted with the override applied.
	fg, err := inf.pgStore.GetFinalGrade(ctx, testTenantID, subID)
	require.NoError(t, err, "final_grade row must exist after approve")
	assert.InDelta(t, expectedTotal, fg.Total, 1e-9,
		"final grade total must reflect the overridden marks")
	assert.InDelta(t, expectedMax, fg.MaxTotal, 1e-9, "final grade max_total mismatch")

	// graded.final.json artifact written, override baked in.
	finalKey := fmt.Sprintf("%s/%s/graded.final.json", testTenantID, subID)
	finalData, err := inf.objStore.Get(ctx, inf.bucket, finalKey)
	require.NoError(t, err, "graded.final.json must exist at %q", finalKey)
	var finalPaper contracts.GradedPaper
	require.NoError(t, json.Unmarshal(finalData, &finalPaper), "graded.final.json must be valid GradedPaper")
	finalQ := questionByNo(t, finalPaper, overrideQno)
	assert.Equal(t, overrideMarks, finalQ.AwardedMarks,
		"graded.final.json must bake in the override for %s", overrideQno)

	// ── 5. Second override attempt → 409 (locked) ────────────────────────────
	lockedResp := patchQuestionMarks(t, apiURL, tok, subID, overrideQno, 0.5, "too late")
	assert.Equal(t, http.StatusConflict, lockedResp.status,
		"override after approve must be 409 (locked), got %d: %s", lockedResp.status, lockedResp.body)

	// GET /review now reports locked=true.
	lockedReview := getReview(t, apiURL, tok, subID)
	assert.True(t, lockedReview.Locked, "review must be locked once past teacher_review")

	// ── 6. POST /publish → poll until state==published ───────────────────────
	publishResp := doPost(t, apiURL, tok, fmt.Sprintf("/v1/submissions/%s/publish", subID))
	require.Equal(t, http.StatusOK, publishResp.status,
		"POST /publish expected 200, got %d: %s", publishResp.status, publishResp.body)

	publishedState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StatePublished, contracts.StateFailed},
		90*time.Second,
	)
	require.Equal(t, contracts.StatePublished, publishedState,
		"submission must advance to published after publish command")

	// ── 7. POST /export → poll until state==exported ─────────────────────────
	exportResp := doPost(t, apiURL, tok, fmt.Sprintf("/v1/submissions/%s/export", subID))
	require.Equal(t, http.StatusOK, exportResp.status,
		"POST /export expected 200, got %d: %s", exportResp.status, exportResp.body)

	exportedState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateExported, contracts.StateFailed},
		90*time.Second,
	)
	require.Equal(t, contracts.StateExported, exportedState,
		"submission must advance to exported after export command")

	// ── 8. GET /export.csv → 200; CSV reflects the override ──────────────────
	rows := getExportCSV(t, apiURL, tok, subID)
	require.NotEmpty(t, rows, "CSV must contain at least a header + one data row")

	header := rows[0]
	colIdx := func(name string) int {
		for i, c := range header {
			if c == name {
				return i
			}
		}
		t.Fatalf("CSV header missing column %q (header=%v)", name, header)
		return -1
	}
	qnoCol := colIdx("question_no")
	awardedCol := colIdx("awarded_marks")

	var foundOverride bool
	for _, row := range rows[1:] {
		if row[qnoCol] != overrideQno {
			continue
		}
		foundOverride = true
		awarded, perr := strconv.ParseFloat(row[awardedCol], 64)
		require.NoError(t, perr, "parse awarded_marks for %s: %q", overrideQno, row[awardedCol])
		assert.Equal(t, overrideMarks, awarded,
			"export.csv awarded_marks for %s must reflect the override", overrideQno)
	}
	assert.True(t, foundOverride, "export.csv must contain a row for question %s", overrideQno)

	// ── 9. Audit trail: override + approve + publish + export ─────────────────
	events, err := inf.pgStore.ListAuditEventsBySubmission(ctx, testTenantID, subID)
	require.NoError(t, err, "list audit events")
	actions := make(map[string]int)
	for _, e := range events {
		actions[e.Action]++
	}
	t.Logf("teacher review: audit actions=%v", actions)
	assert.GreaterOrEqual(t, actions["override_question"], 1, "expected an override_question audit event")
	assert.GreaterOrEqual(t, actions["approve"], 1, "expected an approve audit event")
	assert.GreaterOrEqual(t, actions["publish"], 1, "expected a publish audit event")
	assert.GreaterOrEqual(t, actions["export"], 1, "expected an export audit event")
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP helpers (scoped to the teacher-review test)
// ─────────────────────────────────────────────────────────────────────────────

// httpResult bundles an HTTP status code and raw response body.
type httpResult struct {
	status int
	body   []byte
}

// reviewBody mirrors the JSON shape returned by GET /v1/submissions/{id}/review.
type reviewBody struct {
	Locked bool                  `json:"locked"`
	Paper  contracts.GradedPaper `json:"paper"`
}

// patchQuestionMarks issues PATCH /v1/submissions/{id}/questions/{qno} with an
// awarded_marks override and an audit comment.
func patchQuestionMarks(t *testing.T, apiURL, tok string, subID uuid.UUID, qno string, marks float64, comment string) httpResult {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"awarded_marks": marks,
		"comment":       comment,
	})
	require.NoError(t, err)

	url := fmt.Sprintf("%s/v1/submissions/%s/questions/%s", apiURL, subID, qno)
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(string(payload)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: body}
}

// getReview issues GET /v1/submissions/{id}/review and decodes the response.
func getReview(t *testing.T, apiURL, tok string, subID uuid.UUID) reviewBody {
	t.Helper()
	url := fmt.Sprintf("%s/v1/submissions/%s/review", apiURL, subID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /review expected 200: %s", body)

	var rb reviewBody
	require.NoError(t, json.Unmarshal(body, &rb), "parse review body: %s", body)
	return rb
}

// doPost issues a bodyless POST to a relative path with the bearer token.
func doPost(t *testing.T, apiURL, tok, path string) httpResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, apiURL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: body}
}

// getExportCSV issues GET /v1/submissions/{id}/export.csv and parses the CSV.
func getExportCSV(t *testing.T, apiURL, tok string, subID uuid.UUID) [][]string {
	t.Helper()
	url := fmt.Sprintf("%s/v1/submissions/%s/export.csv", apiURL, subID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /export.csv expected 200: %s", body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/csv", "export.csv Content-Type")

	records, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	require.NoError(t, err, "parse CSV: %s", body)
	return records
}

// questionByNo returns the question with the given question_no from a paper,
// failing the test if absent.
func questionByNo(t *testing.T, paper contracts.GradedPaper, qno string) contracts.GradedQuestion {
	t.Helper()
	for _, q := range paper.Questions {
		if q.QuestionNo == qno {
			return q
		}
	}
	t.Fatalf("question %q not found in paper", qno)
	return contracts.GradedQuestion{}
}
