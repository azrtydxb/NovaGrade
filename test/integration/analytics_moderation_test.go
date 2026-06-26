package integration

// analytics_moderation_test.go — Phase-5 hermetic integration test.
//
// TestAnalyticsModerationFlow proves the full Phase-5 feature set end-to-end on
// real infrastructure (Postgres, RabbitMQ, MinIO via testcontainers), the
// in-process orchestrator + stage workers, and the real Phase-2 + Phase-5 HTTP
// handlers:
//
//  1. startInfra (inherited SKIP_DOCKER_TESTS gate) + startPipeline (real
//     orchestrator + render/transcribe/grade workers driven by a fake AI provider).
//  2. Create an assessment_version; submit ≥3 submissions linked to it; drive each
//     through grade → teacher_review → approve → publish. On ONE submission, PATCH
//     a question's marks (override) before approving so override-stats is non-trivial.
//  3. Analytics: GET …/analytics — assert item_analysis present (expected MaxMarks /
//     Responses), distribution.count == graded submissions, graded_count/total_count.
//  4. Override-stats: GET …/override-stats — overridden_questions ≥1, override_rate > 0.
//  5. Moderation: POST …/moderation {sample_size} → session + sampled ids; POST a
//     moderator mark for a sampled submission+question; GET /moderation/{id} → assert
//     the comparison carries AI/teacher_final/moderator + deltas; assert the sampled
//     submission's FINAL GRADE is UNCHANGED (moderation is read-only).
//  6. Appeals/regrade: POST …/appeals {reason} on a published submission → 201 open;
//     GET /appeals?status=open → listed; POST /appeals/{id}/regrade → poll until the
//     submission re-opens (grading → teacher_review, NOT still published); assert the
//     appeal is now under_review; assert the original graded artifact still exists.
//     POST /appeals/{id}/resolve {status:"resolved"} → appeal resolved.
//  7. All async transitions awaited via pollSubmission (no fixed sleeps). Hermetic.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chi_middleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/analytics"
	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Phase-5 in-process API server
// ─────────────────────────────────────────────────────────────────────────────

// newPhase5APIServer builds an httptest.Server wiring the Phase-1 submission
// handlers, the Phase-2 review/approval handlers (needed to drive
// grade→approve→publish + the PATCH override), and the Phase-5 Analytics /
// Moderation / Appeal handlers. Routing mirrors cmd/api/main.go exactly.
func newPhase5APIServer(t *testing.T, inf *testInfra) string {
	t.Helper()
	if os.Getenv("JWT_SIGNING_KEY") == "" {
		t.Setenv("JWT_SIGNING_KEY", testSigningKey)
	}

	objAdapter := &objStoreAdapter{s: inf.objStore, bucket: inf.bucket}

	// Phase-1 submission handlers.
	h := &api.Handlers{
		Store:      inf.pgStore,
		Bus:        inf.bus,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}
	// Phase-2 review + approval handlers (PATCH override, approve, publish).
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
	// Phase-5 handlers — same constructors as cmd/api/main.go.
	anah := &api.AnalyticsHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}
	moh := &api.ModerationHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}
	aph := &api.AppealHandlers{
		Store:      inf.pgStore,
		Bus:        inf.bus,
		DeployMode: "onprem",
	}

	r := chi.NewRouter()
	r.Use(chi_middleware.Recoverer)
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(auth.NewAPIKeyResolver()))
		// Phase-1/2: submission lifecycle.
		r.Post("/submissions", h.PostSubmission)
		r.Get("/submissions/{id}", h.GetSubmission)
		r.Get("/submissions/{id}/review", rh.GetReview)
		r.Patch("/submissions/{id}/questions/{qno}", rh.PatchQuestion)
		r.Post("/submissions/{id}/approve", ah.Approve)
		r.Post("/submissions/{id}/publish", ah.Publish)
		// Phase-5: analytics.
		r.Get("/assessment-versions/{avid}/analytics", anah.GetAnalytics)
		r.Get("/assessment-versions/{avid}/override-stats", anah.GetOverrideStats)
		// Phase-5: moderation.
		r.Post("/assessment-versions/{avid}/moderation", moh.StartSession)
		r.Post("/moderation/{id}/marks", moh.RecordMark)
		r.Get("/moderation/{id}", moh.GetComparison)
		// Phase-5: appeals / regrade.
		r.Post("/submissions/{id}/appeals", aph.FileAppeal)
		r.Get("/appeals", aph.ListAppeals)
		r.Post("/appeals/{id}/resolve", aph.ResolveAppeal)
		r.Post("/appeals/{id}/regrade", aph.RegradeAppeal)
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase-5 response shapes (mirror the handler JSON)
// ─────────────────────────────────────────────────────────────────────────────

type analyticsResp struct {
	ItemAnalysis    []analytics.QuestionStat `json:"item_analysis"`
	Distribution    analytics.Distribution   `json:"distribution"`
	Hardest         []analytics.QuestionStat `json:"hardest"`
	FlagFrequencies map[string]int           `json:"flag_frequencies"`
	GradedCount     int                      `json:"graded_count"`
	TotalCount      int                      `json:"total_count"`
}

type overrideStatsResp struct {
	TotalGradedQuestions int     `json:"total_graded_questions"`
	OverriddenQuestions  int     `json:"overridden_questions"`
	OverrideRate         float64 `json:"override_rate"`
	MeanAbsDelta         float64 `json:"mean_abs_delta"`
}

type startModResp struct {
	SessionID            string   `json:"session_id"`
	SampledSubmissionIDs []string `json:"sampled_submission_ids"`
	SampleSize           int      `json:"sample_size"`
	Status               string   `json:"status"`
}

type modMarkEntry struct {
	SubmissionID    string  `json:"submission_id"`
	QuestionNo      string  `json:"question_no"`
	AI              float64 `json:"ai"`
	TeacherFinal    float64 `json:"teacher_final"`
	Moderator       float64 `json:"moderator"`
	DeltaModTeacher float64 `json:"delta_mod_teacher"`
	DeltaModAI      float64 `json:"delta_mod_ai"`
}

type comparisonResp struct {
	Session struct {
		ID                  string `json:"id"`
		AssessmentVersionID string `json:"assessment_version_id"`
		SampleSize          int    `json:"sample_size"`
		Status              string `json:"status"`
	} `json:"session"`
	Marks   []modMarkEntry `json:"marks"`
	Summary struct {
		MeanAbsModTeacherDelta float64 `json:"mean_abs_mod_teacher_delta"`
		Count                  int     `json:"count"`
	} `json:"summary"`
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP helpers (scoped to the Phase-5 test)
// ─────────────────────────────────────────────────────────────────────────────

// getJSON issues an authenticated GET and decodes the JSON body into out,
// asserting a 200 status.
func getJSON(t *testing.T, apiURL, tok, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, apiURL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s expected 200: %s", path, body)
	require.NoError(t, json.Unmarshal(body, out), "decode %s: %s", path, body)
}

// postJSON issues an authenticated POST with a JSON body and returns the raw result.
func postJSON(t *testing.T, apiURL, tok, path string, payload any) httpResult {
	t.Helper()
	var rdr io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, apiURL+path, rdr)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: body}
}

// driveToTeacherReview submits a PDF linked to avid and polls until the
// submission reaches teacher_review.
func driveToTeacherReview(t *testing.T, ctx context.Context, inf *testInfra, avid uuid.UUID, apiURL, jwt string, pdf []byte) uuid.UUID {
	t.Helper()
	subID := postSubmissionWithAVID(t, inf, testTenantID, avid, apiURL, jwt, pdf)
	state := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateTeacherReview, contracts.StateFailed},
		4*time.Minute,
	)
	require.Equal(t, contracts.StateTeacherReview, state,
		"submission %s must reach teacher_review", subID)
	return subID
}

// approveAndPublish drives a teacher_review submission through approve → publish.
func approveAndPublish(t *testing.T, ctx context.Context, inf *testInfra, apiURL, jwt string, subID uuid.UUID) {
	t.Helper()
	approveResp := doPost(t, apiURL, jwt, fmt.Sprintf("/v1/submissions/%s/approve", subID))
	require.Equal(t, http.StatusOK, approveResp.status,
		"POST /approve expected 200, got %d: %s", approveResp.status, approveResp.body)
	require.Equal(t, contracts.StateApproved, pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateApproved, contracts.StateFailed}, 90*time.Second),
		"submission %s must advance to approved", subID)

	publishResp := doPost(t, apiURL, jwt, fmt.Sprintf("/v1/submissions/%s/publish", subID))
	require.Equal(t, http.StatusOK, publishResp.status,
		"POST /publish expected 200, got %d: %s", publishResp.status, publishResp.body)
	require.Equal(t, contracts.StatePublished, pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StatePublished, contracts.StateFailed}, 90*time.Second),
		"submission %s must advance to published", subID)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAnalyticsModerationFlow — Phase-5 end-to-end hermetic integration test
// ─────────────────────────────────────────────────────────────────────────────

func TestAnalyticsModerationFlow(t *testing.T) {
	inf := startInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Admin JWT: school_admin has BOTH ViewResults (analytics/appeals) and
	// ReviewFixApprove (moderation/resolve/regrade) + EditTunables.
	adminJWT := mintAdminJWT(t)
	// Teacher JWT for submission ingestion + review/override (RoleTeacher).
	teacherJWT := mintTestJWT(t)

	fakeAI := newFakeAIServer(t)
	aiProv := newFakeAIProvider(fakeAI)

	// Start the orchestrator + render/transcribe/grade workers. The orchestrator
	// also consumes commands.q (approve/publish) and the regrade command, and
	// re-dispatches grade.q on EventRegrade → grading.
	stopPipeline := startPipeline(ctx, t, inf, pipelineConfig{
		transcribeProvider: aiProv,
		gradeProvider:      aiProv,
		gradeModel:         "grade-model",
	})
	defer stopPipeline()

	ensureTenant(t, inf, testTenantID)
	avid := ensureAssessmentAndVersion(t, inf, testTenantID)
	t.Logf("phase5: assessment_version_id=%s", avid)

	apiURL := newPhase5APIServer(t, inf)
	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err, "read sample PDF")

	// ── Step 1: submit 3 submissions linked to the assessment_version ─────────
	const nSubs = 3
	subIDs := make([]uuid.UUID, 0, nSubs)
	for i := 0; i < nSubs; i++ {
		subID := driveToTeacherReview(t, ctx, inf, avid, apiURL, teacherJWT, pdfBytes)
		subIDs = append(subIDs, subID)
		t.Logf("phase5: submission[%d]=%s reached teacher_review", i, subID)
	}

	// The fake transcript yields question "1a" (max 2) and "1b" (max 3); the
	// grade model awards 2 marks per question. On the FIRST submission we override
	// 1a down to 1.0 so override-stats is non-trivial.
	const (
		overrideQno   = "1a"
		overrideMarks = 1.0
		aiMarks1a     = 2.0 // raw AI award for 1a before override
	)
	overriddenSub := subIDs[0]

	patchResp := patchQuestionMarks(t, apiURL, teacherJWT, overriddenSub, overrideQno, overrideMarks, "phase5 override on 1a")
	require.Equal(t, http.StatusOK, patchResp.status,
		"PATCH override expected 200, got %d: %s", patchResp.status, patchResp.body)
	t.Logf("phase5: overrode %s on submission %s → %.1f", overrideQno, overriddenSub, overrideMarks)

	// Approve + publish ALL submissions (override baked into the first).
	for _, subID := range subIDs {
		approveAndPublish(t, ctx, inf, apiURL, teacherJWT, subID)
	}

	// ── Step 2: Analytics — GET …/analytics ──────────────────────────────────
	var an analyticsResp
	getJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/assessment-versions/%s/analytics", avid), &an)
	t.Logf("phase5: analytics graded=%d total=%d items=%d dist.count=%d",
		an.GradedCount, an.TotalCount, len(an.ItemAnalysis), an.Distribution.Count)

	require.NotEmpty(t, an.ItemAnalysis, "item_analysis must contain at least one question")
	assert.Equal(t, nSubs, an.GradedCount, "graded_count must equal the number of graded submissions")
	assert.Equal(t, nSubs, an.TotalCount, "total_count must equal the number of submissions for the AVID")
	assert.Equal(t, nSubs, an.Distribution.Count,
		"distribution.count must equal the number of graded submissions")

	// Locate the 1a stat and assert MaxMarks + Responses reflect the known marks.
	var stat1a *analytics.QuestionStat
	for i := range an.ItemAnalysis {
		if an.ItemAnalysis[i].QuestionNo == overrideQno {
			stat1a = &an.ItemAnalysis[i]
			break
		}
	}
	require.NotNil(t, stat1a, "item_analysis must include question %s", overrideQno)
	assert.Equal(t, 2.0, stat1a.MaxMarks, "question %s MaxMarks must be 2", overrideQno)
	assert.Equal(t, nSubs, stat1a.Responses,
		"question %s must have a response in every graded submission", overrideQno)

	// ── Step 3: Override-stats — GET …/override-stats ────────────────────────
	var os1 overrideStatsResp
	getJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/assessment-versions/%s/override-stats", avid), &os1)
	t.Logf("phase5: override-stats total=%d overridden=%d rate=%.4f meanAbsDelta=%.2f",
		os1.TotalGradedQuestions, os1.OverriddenQuestions, os1.OverrideRate, os1.MeanAbsDelta)

	assert.GreaterOrEqual(t, os1.OverriddenQuestions, 1, "at least one overridden question expected")
	assert.Greater(t, os1.OverrideRate, 0.0, "override_rate must be > 0")
	// We overrode 1a from AI's 2.0 down to 1.0 → abs delta 1.0.
	assert.Greater(t, os1.MeanAbsDelta, 0.0, "mean_abs_delta must be > 0 after a real override")

	// ── Step 4: Moderation — start session, record a mark, compare ───────────
	startResp := postJSON(t, apiURL, adminJWT,
		fmt.Sprintf("/v1/assessment-versions/%s/moderation", avid),
		map[string]any{"sample_size": 2})
	require.Equal(t, http.StatusCreated, startResp.status,
		"POST /moderation expected 201, got %d: %s", startResp.status, startResp.body)

	var mod startModResp
	require.NoError(t, json.Unmarshal(startResp.body, &mod), "decode moderation session: %s", startResp.body)
	sessionID, err := uuid.Parse(mod.SessionID)
	require.NoError(t, err, "parse session id")
	require.NotEmpty(t, mod.SampledSubmissionIDs, "moderation must sample at least one submission")
	assert.LessOrEqual(t, len(mod.SampledSubmissionIDs), 2, "sample must not exceed sample_size")
	t.Logf("phase5: moderation session=%s sampled=%v", sessionID, mod.SampledSubmissionIDs)

	// Pick a sampled submission to moderate. Capture its FINAL GRADE first so we
	// can prove moderation does NOT mutate it.
	sampledSubID, err := uuid.Parse(mod.SampledSubmissionIDs[0])
	require.NoError(t, err, "parse sampled submission id")
	fgBefore, err := inf.pgStore.GetFinalGrade(ctx, testTenantID, sampledSubID)
	require.NoError(t, err, "final_grade must exist for the published sampled submission")

	// Record a moderator mark for question 1b on the sampled submission. The
	// moderator awards 1.0 (vs AI/teacher 2.0) so deltas are non-zero.
	const (
		modQno   = "1b"
		modMarks = 1.0
	)
	markResp := postJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/moderation/%s/marks", sessionID),
		map[string]any{
			"submission_id":   sampledSubID.String(),
			"question_no":     modQno,
			"moderator_marks": modMarks,
		})
	require.Equal(t, http.StatusCreated, markResp.status,
		"POST moderator mark expected 201, got %d: %s", markResp.status, markResp.body)

	// GET comparison: AI/teacher_final/moderator + deltas.
	var cmp comparisonResp
	getJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/moderation/%s", sessionID), &cmp)
	require.Len(t, cmp.Marks, 1, "comparison must contain the single recorded mark")
	entry := cmp.Marks[0]
	t.Logf("phase5: comparison %s/%s ai=%.1f teacher=%.1f mod=%.1f Δmt=%.1f Δma=%.1f",
		entry.SubmissionID, entry.QuestionNo, entry.AI, entry.TeacherFinal, entry.Moderator,
		entry.DeltaModTeacher, entry.DeltaModAI)
	assert.Equal(t, sampledSubID.String(), entry.SubmissionID)
	assert.Equal(t, modQno, entry.QuestionNo)
	assert.Equal(t, modMarks, entry.Moderator, "moderator mark must be surfaced")
	// 1b was NOT overridden on any submission → AI == teacher_final == 2.0.
	assert.Equal(t, 2.0, entry.AI, "AI mark for %s must be the raw 2.0", modQno)
	assert.Equal(t, 2.0, entry.TeacherFinal, "teacher_final for %s must be 2.0 (no override)", modQno)
	assert.Equal(t, modMarks-entry.TeacherFinal, entry.DeltaModTeacher, "delta_mod_teacher must be moderator − teacher_final")
	assert.Equal(t, modMarks-entry.AI, entry.DeltaModAI, "delta_mod_ai must be moderator − ai")
	assert.InDelta(t, -1.0, entry.DeltaModTeacher, 1e-9, "moderator 1.0 vs teacher 2.0 → −1.0")

	// Assert the FINAL GRADE is UNCHANGED by moderation (read-only invariant).
	fgAfter, err := inf.pgStore.GetFinalGrade(ctx, testTenantID, sampledSubID)
	require.NoError(t, err, "final_grade must still exist after moderation")
	assert.InDelta(t, fgBefore.Total, fgAfter.Total, 1e-9,
		"moderation must NOT change the final grade total")
	assert.InDelta(t, fgBefore.MaxTotal, fgAfter.MaxTotal, 1e-9,
		"moderation must NOT change the final grade max_total")
	require.Equal(t, contracts.StatePublished,
		mustSubState(t, ctx, inf.pgStore, sampledSubID),
		"moderation must NOT change submission state")

	// ── Step 5: Appeals / regrade ────────────────────────────────────────────
	// File an appeal on a published submission that is NOT the moderated one, so
	// the regrade state-transition assertions are independent.
	appealSubID := subIDs[1]
	require.Equal(t, contracts.StatePublished, mustSubState(t, ctx, inf.pgStore, appealSubID),
		"appeal target must be published before regrade")

	// Assert the original graded artifact exists before regrade.
	gradedKey := fmt.Sprintf("%s/%s/graded.v1.json", testTenantID, appealSubID)
	_, err = inf.objStore.Get(ctx, inf.bucket, gradedKey)
	require.NoError(t, err, "original graded.v1.json must exist before regrade at %q", gradedKey)

	fileResp := postJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/submissions/%s/appeals", appealSubID),
		map[string]any{"reason": "student disputes question 1b marking"})
	require.Equal(t, http.StatusCreated, fileResp.status,
		"POST /appeals expected 201, got %d: %s", fileResp.status, fileResp.body)
	var appeal store.Appeal
	require.NoError(t, json.Unmarshal(fileResp.body, &appeal), "decode appeal: %s", fileResp.body)
	require.NotEqual(t, uuid.Nil, appeal.ID, "appeal id must be non-nil")
	assert.Equal(t, "open", appeal.Status, "new appeal status must be 'open'")
	t.Logf("phase5: filed appeal id=%s status=%s", appeal.ID, appeal.Status)

	// GET /appeals?status=open → the appeal must be listed.
	var openAppeals []store.Appeal
	getJSON(t, apiURL, adminJWT, "/v1/appeals?status=open", &openAppeals)
	foundOpen := false
	for _, a := range openAppeals {
		if a.ID == appeal.ID {
			foundOpen = true
		}
	}
	assert.True(t, foundOpen, "open appeal %s must appear in GET /appeals?status=open", appeal.ID)

	// POST regrade → orchestrator drives published → grading → (grade.q) → teacher_review.
	regradeResp := doPost(t, apiURL, adminJWT, fmt.Sprintf("/v1/appeals/%s/regrade", appeal.ID))
	require.Equal(t, http.StatusOK, regradeResp.status,
		"POST /regrade expected 200, got %d: %s", regradeResp.status, regradeResp.body)

	// Poll: the submission must re-open — i.e. leave the published terminal state
	// and land back in teacher_review after re-grading (NOT still published).
	reopened := pollSubmission(ctx, t, inf.pgStore, appealSubID,
		[]contracts.SubmissionState{contracts.StateTeacherReview, contracts.StateFailed},
		3*time.Minute,
	)
	require.Equal(t, contracts.StateTeacherReview, reopened,
		"regrade must re-open the submission to teacher_review (not leave it published)")
	assert.NotEqual(t, contracts.StatePublished, reopened, "submission must NOT still be published after regrade")
	t.Logf("phase5: regrade re-opened submission %s to %s", appealSubID, reopened)

	// The original graded artifact must still exist (regrade never deletes it).
	_, err = inf.objStore.Get(ctx, inf.bucket, gradedKey)
	require.NoError(t, err, "graded.v1.json must still exist after regrade at %q", gradedKey)

	// The appeal must now be under_review.
	appealAfter, err := inf.pgStore.GetAppeal(ctx, testTenantID, appeal.ID)
	require.NoError(t, err, "get appeal after regrade")
	assert.Equal(t, "under_review", appealAfter.Status,
		"appeal must be 'under_review' after regrade")

	// Resolve the appeal.
	resolveResp := postJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/appeals/%s/resolve", appeal.ID),
		map[string]any{"status": "resolved", "resolution": "regraded; marks confirmed"})
	require.Equal(t, http.StatusOK, resolveResp.status,
		"POST /resolve expected 200, got %d: %s", resolveResp.status, resolveResp.body)

	appealResolved, err := inf.pgStore.GetAppeal(ctx, testTenantID, appeal.ID)
	require.NoError(t, err, "get appeal after resolve")
	assert.Equal(t, "resolved", appealResolved.Status, "appeal must be 'resolved' after resolve")

	t.Logf("phase5: PASSED — analytics(%d graded), override_rate=%.4f, moderation read-only verified, appeal regrade→teacher_review→resolved",
		an.GradedCount, os1.OverrideRate)
}

// mustSubState fetches the current state of a submission, failing the test on error.
func mustSubState(t *testing.T, ctx context.Context, st *store.Store, id uuid.UUID) contracts.SubmissionState {
	t.Helper()
	sub, err := st.GetSubmission(ctx, id)
	require.NoError(t, err, "get submission %s", id)
	return sub.State
}
