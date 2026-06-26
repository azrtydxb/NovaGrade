package integration

// regrade_finalize_test.go — Integration test proving the full regrade→re-approve loop.
//
// TestRegradeFinalizes proves end-to-end that:
//
//  1. A submission is graded (via real gradeworker.HandleEnvelope), approved, and
//     published (original grade captured).
//  2. An appeal is filed and a regrade is triggered.
//  3. The regrade produces a DIFFERENT (higher) grade via a stateful fake AI server
//     that returns a higher awarded_marks on the second grade pass.
//  4. Re-approving the regraded submission finalizes the NEW grade:
//     (a) GetFinalGrade returns the NEW total (different from original).
//     (b) Exactly one final_grade row exists in the database (UPSERT invariant).
//     (c) graded.archive.1.json exists in object storage holding the original graded bytes
//         (T1 archive-before-overwrite).
//     (d) The audit trail preserves an "approve" event recording the ORIGINAL total
//         (first approve is in the append-only audit_event table, survives the UPSERT).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chi_middleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/gradeworker"
	"github.com/azrtydxb/novagrade/internal/orchestrator"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Stateful fake AI server
// ─────────────────────────────────────────────────────────────────────────────

// newStatefulFakeAIServer returns an httptest.Server identical to newFakeAIServer
// except that the grade-model handler is stateful: it counts calls globally and
// returns awarded_marks=2 on the FIRST grade pass (calls 1–4) and awarded_marks=4
// on the SECOND grade pass (calls 5+). This ensures the regrade produces a
// different total so the test can assert GetFinalGrade returns a NEW value after
// re-approve.
//
// With 2 questions × 2 page renders = 4 grade-model calls per pass:
//
//	First  grade pass (calls 1–4): awarded_marks=2 → 1a capped at 2, 1b capped at 2 → total = 8.00
//	Second grade pass (calls 5+):  awarded_marks=4 → 1a capped at max, 1b capped at max → total = 10.00
//
// origGrade.Total (8.00) != newGrade.Total (10.00), which is what the test requires.
//
// The returned *int64 pointer is the shared gradeCallCount for logging.
func newStatefulFakeAIServer(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()

	var gradeCallCount int64
	// 2 questions × 2 page renders = 4 grade-model calls per first pass;
	// calls > 4 come from the regrade pass and return higher marks.
	const passThreshold int64 = 4

	responses := map[string]func() string{
		"dots.ocr": func() string {
			return "Question 1a (2 marks)\nWhat is 2+2?\n\nQuestion 1b (3 marks)\nExplain gravity."
		},
		"qwen3": func() string {
			return `[{"section":null,"question_no":"1a","max_marks":2,"question_text":"What is 2+2?"},{"section":null,"question_no":"1b","max_marks":3,"question_text":"Explain gravity."}]`
		},
		"qwen3-vl": func() string {
			return `[{"question_no":"1a","student_answer":"4"},{"question_no":"1b","student_answer":"Objects attract each other due to gravitational force."}]`
		},
		// Grade model: stateful — first pass awards 2, second pass awards 4 (capped at max_marks per question).
		"grade-model": func() string {
			n := atomic.AddInt64(&gradeCallCount, 1)
			if n <= passThreshold {
				return `{"awarded_marks":2,"justification":"Correct answer","grade_confidence":0.98}`
			}
			return `{"awarded_marks":4,"justification":"Re-graded: improved marks","grade_confidence":0.99}`
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		fn, ok := responses[req.Model]
		var content string
		if ok {
			content = fn()
		} else {
			content = "no content"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": content}},
			},
			"usage": map[string]int{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &gradeCallCount
}

// ─────────────────────────────────────────────────────────────────────────────
// newRegradeAPIServer
// ─────────────────────────────────────────────────────────────────────────────

// newRegradeAPIServer builds an httptest.Server wiring the Phase-1 submission
// handlers, the Phase-2 review/approval handlers, and the Phase-5 appeal handlers.
// It does NOT wire analytics or moderation — only what this test needs.
func newRegradeAPIServer(t *testing.T, inf *testInfra) string {
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
	aph := &api.AppealHandlers{
		Store:      inf.pgStore,
		Bus:        inf.bus,
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
// TestRegradeFinalizes — end-to-end regrade → re-approve loop
// ─────────────────────────────────────────────────────────────────────────────

// TestRegradeFinalizes proves the complete regrade→re-approve finalization loop
// end-to-end on real infrastructure.
func TestRegradeFinalizes(t *testing.T) {
	if os.Getenv("SKIP_DOCKER_TESTS") != "" {
		t.Skip("SKIP_DOCKER_TESTS set")
	}
	inf := startInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	adminJWT := mintAdminJWT(t)

	// Build a STATEFUL fake AI server so the regrade produces a higher total.
	fakeAI, gradeCallCountPtr := newStatefulFakeAIServer(t)
	aiProv := newFakeAIProvider(fakeAI)

	// Wire the pipeline manually (like marking_guide_test.go) so we can use the
	// REAL gradeworker.HandleEnvelope, which includes the archive-before-overwrite
	// logic (T1). startPipeline uses a simplified inline gradeHandler that skips
	// archiving, so we cannot use it here.
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	orch := orchestrator.New(inf.pgStore, &busAdapter{inf.bus}, inf.objStore, inf.bucket)
	go func() {
		if err := orch.Start(workerCtx); err != nil && err != context.Canceled {
			t.Logf("orchestrator stopped: %v", err)
		}
	}()

	renderBus := mustConnectBus(t, inf.amqpURL, 0)
	require.NoError(t, renderBus.Consume(workerCtx, "render.q", func(env contracts.Envelope) error {
		return renderHandler(workerCtx, env, inf.objStore, renderBus, inf.bucket)
	}), "start render consumer")

	transcribeBus := mustConnectBus(t, inf.amqpURL, 0)
	require.NoError(t, transcribeBus.Consume(workerCtx, "transcribe.q", func(env contracts.Envelope) error {
		return transcribeHandler(workerCtx, env, inf.objStore, transcribeBus, aiProv, inf.bucket, false)
	}), "start transcribe consumer")

	// Grade worker uses the REAL gradeworker.HandleEnvelope (same as cmd/grade/main.go).
	// This is the code path that performs archive-before-overwrite on regrade (T1).
	gradeBus := mustConnectBus(t, inf.amqpURL, 0)
	gradeDeps := gradeworker.Deps{
		ObjStore:   inf.objStore,
		Store:      inf.pgStore,
		Provider:   aiProv,
		Bus:        gradeBus,
		Bucket:     inf.bucket,
		GradeModel: "grade-model",
	}
	require.NoError(t, gradeBus.Consume(workerCtx, "grade.q", func(env contracts.Envelope) error {
		return gradeworker.HandleEnvelope(workerCtx, gradeDeps, env)
	}), "start grade consumer")

	ensureTenant(t, inf, testTenantID)
	avid := ensureAssessmentAndVersion(t, inf, testTenantID)
	t.Logf("regrade_finalize: assessment_version_id=%s", avid)

	apiURL := newRegradeAPIServer(t, inf)
	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err, "read sample PDF")

	// ── Step 1: Drive submission to teacher_review → approve → publish ──────────
	subID := driveToTeacherReview(t, ctx, inf, avid, apiURL, adminJWT, pdfBytes)
	t.Logf("regrade_finalize: submission %s reached teacher_review", subID)

	// First approval.
	approveResp1 := doPost(t, apiURL, adminJWT, fmt.Sprintf("/v1/submissions/%s/approve", subID))
	require.Equal(t, http.StatusOK, approveResp1.status,
		"POST /approve (1st) expected 200, got %d: %s", approveResp1.status, approveResp1.body)
	require.Equal(t, contracts.StateApproved, pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateApproved, contracts.StateFailed}, 90*time.Second),
		"submission %s must reach approved after first approve", subID)

	publishResp1 := doPost(t, apiURL, adminJWT, fmt.Sprintf("/v1/submissions/%s/publish", subID))
	require.Equal(t, http.StatusOK, publishResp1.status,
		"POST /publish expected 200, got %d: %s", publishResp1.status, publishResp1.body)
	require.Equal(t, contracts.StatePublished, pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StatePublished, contracts.StateFailed}, 90*time.Second),
		"submission %s must reach published", subID)

	// ── Step 2: Capture original grade state ────────────────────────────────────
	origGrade, err := inf.pgStore.GetFinalGrade(ctx, testTenantID, subID)
	require.NoError(t, err, "GetFinalGrade must return a row after first approval")
	t.Logf("regrade_finalize: original final grade total=%.2f max=%.2f score=%.2f",
		origGrade.Total, origGrade.MaxTotal, origGrade.Score100)

	// Capture the original graded.v1.json bytes before the regrade overwrites it.
	gradedKey := fmt.Sprintf("%s/%s/graded.v1.json", testTenantID, subID)
	originalGradedBytes, err := inf.objStore.Get(ctx, inf.bucket, gradedKey)
	require.NoError(t, err, "graded.v1.json must exist before regrade at %q", gradedKey)
	t.Logf("regrade_finalize: captured original graded.v1.json (%d bytes)", len(originalGradedBytes))
	t.Logf("regrade_finalize: grade call count after first pass = %d", atomic.LoadInt64(gradeCallCountPtr))

	// ── Step 3: File appeal → trigger regrade ───────────────────────────────────
	fileResp := postJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/submissions/%s/appeals", subID),
		map[string]any{"reason": "student disputes the grade on question 1b"})
	require.Equal(t, http.StatusCreated, fileResp.status,
		"POST /appeals expected 201, got %d: %s", fileResp.status, fileResp.body)
	var appeal store.Appeal
	require.NoError(t, json.Unmarshal(fileResp.body, &appeal),
		"decode appeal response: %s", fileResp.body)
	require.NotEqual(t, uuid.Nil, appeal.ID, "appeal ID must be non-nil")
	assert.Equal(t, "open", appeal.Status, "new appeal must have status 'open'")
	t.Logf("regrade_finalize: filed appeal id=%s", appeal.ID)

	// Trigger the regrade: orchestrator routes published → grading → grade.q → teacher_review.
	regradeResp := doPost(t, apiURL, adminJWT, fmt.Sprintf("/v1/appeals/%s/regrade", appeal.ID))
	require.Equal(t, http.StatusOK, regradeResp.status,
		"POST /regrade expected 200, got %d: %s", regradeResp.status, regradeResp.body)

	// Poll: the submission must re-open to teacher_review after the regrade.
	reopened := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateTeacherReview, contracts.StateFailed},
		3*time.Minute,
	)
	require.Equal(t, contracts.StateTeacherReview, reopened,
		"regrade must re-open submission %s to teacher_review", subID)
	t.Logf("regrade_finalize: submission %s re-opened to %s after regrade", subID, reopened)
	t.Logf("regrade_finalize: grade call count after second pass = %d", atomic.LoadInt64(gradeCallCountPtr))

	// ── Step 4: Re-approve to finalize the new grade ─────────────────────────────
	approveResp2 := doPost(t, apiURL, adminJWT, fmt.Sprintf("/v1/submissions/%s/approve", subID))
	require.Equal(t, http.StatusOK, approveResp2.status,
		"POST /approve (2nd, re-approve after regrade) expected 200, got %d: %s",
		approveResp2.status, approveResp2.body)
	require.Equal(t, contracts.StateApproved, pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateApproved, contracts.StateFailed}, 90*time.Second),
		"submission %s must reach approved after re-approve", subID)

	// Resolve the appeal now that the regrade has been approved.
	resolveResp := postJSON(t, apiURL, adminJWT, fmt.Sprintf("/v1/appeals/%s/resolve", appeal.ID),
		map[string]any{"status": "resolved", "resolution": "regrade completed and approved"})
	require.Equal(t, http.StatusOK, resolveResp.status,
		"POST /appeals/%s/resolve expected 200, got %d: %s", appeal.ID, resolveResp.status, resolveResp.body)
	t.Logf("regrade_finalize: appeal %s resolved", appeal.ID)

	// ── Step 5: Assertions ────────────────────────────────────────────────────────

	// (a) GetFinalGrade returns the NEW total — different from the original.
	newGrade, err := inf.pgStore.GetFinalGrade(ctx, testTenantID, subID)
	require.NoError(t, err, "GetFinalGrade must return a row after re-approve")
	t.Logf("regrade_finalize: new final grade total=%.2f max=%.2f score=%.2f",
		newGrade.Total, newGrade.MaxTotal, newGrade.Score100)
	assert.NotEqual(t, origGrade.Total, newGrade.Total,
		"new final grade total must differ from original (regrade produced a different score): orig=%.2f new=%.2f",
		origGrade.Total, newGrade.Total)
	assert.Greater(t, newGrade.Total, origGrade.Total,
		"new total must be greater than original (second grade pass awards higher marks)")

	// (b) Exactly ONE final_grade row must exist per submission (UPSERT, not INSERT).
	conn, err := pgxConnect(ctx, inf.dbCfg)
	require.NoError(t, err, "open pgx connection for final_grade count")
	defer func() { _ = conn.Close(ctx) }()
	var finalGradeCount int
	err = conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM final_grade WHERE tenant_id = $1 AND submission_id = $2`,
		testTenantID, subID,
	).Scan(&finalGradeCount)
	require.NoError(t, err, "count final_grade rows")
	assert.Equal(t, 1, finalGradeCount,
		"UPSERT invariant: must be exactly one final_grade row per submission after re-approve")

	// (c) graded.archive.1.json exists and matches the original graded.v1.json bytes.
	// This is produced by T1 (archive-before-overwrite in gradeworker.HandleEnvelope).
	archiveKey := fmt.Sprintf("%s/%s/graded.archive.1.json", testTenantID, subID)
	archivedBytes, err := inf.objStore.Get(ctx, inf.bucket, archiveKey)
	require.NoError(t, err,
		"T1 invariant: graded.archive.1.json must exist after regrade at %q (archive-before-overwrite)", archiveKey)
	require.NotEmpty(t, archivedBytes, "graded.archive.1.json must not be empty")
	assert.Equal(t, originalGradedBytes, archivedBytes,
		"T1 invariant: archived artifact must be byte-for-byte identical to the original graded.v1.json captured before regrade")
	t.Logf("regrade_finalize: T1 archive artifact verified at %q (%d bytes)", archiveKey, len(archivedBytes))

	// (d) Audit trail: the FIRST "approve" event must record the ORIGINAL total.
	// The audit_event table is append-only; InsertAuditEvent is called before
	// InsertFinalGrade (audit-first), so the original total is preserved even after
	// the UPSERT overwrites the final_grade row.
	auditEvents, err := inf.pgStore.ListAuditEventsBySubmission(ctx, testTenantID, subID)
	require.NoError(t, err, "ListAuditEventsBySubmission must succeed")
	require.NotEmpty(t, auditEvents, "must have at least one audit event")

	type approveAuditValue struct {
		Total    float64 `json:"total"`
		MaxTotal float64 `json:"max_total"`
		Score100 float64 `json:"score_100"`
	}

	var approveEvents []approveAuditValue
	for _, ev := range auditEvents {
		if ev.Action != "approve" {
			continue
		}
		var val approveAuditValue
		if jsonErr := json.Unmarshal(ev.NewValue, &val); jsonErr != nil {
			continue
		}
		approveEvents = append(approveEvents, val)
		t.Logf("regrade_finalize: audit approve event: total=%.2f max=%.2f score=%.2f (event_id=%s)",
			val.Total, val.MaxTotal, val.Score100, ev.ID)
	}

	// Must have at least 2 approve events (one for each approve).
	require.GreaterOrEqual(t, len(approveEvents), 2,
		"must have at least 2 'approve' audit events (first approve + re-approve after regrade)")

	// The FIRST approve event must record the original total.
	firstApprove := approveEvents[0]
	assert.InDelta(t, origGrade.Total, firstApprove.Total, 1e-6,
		"first approve audit event must record the original total (%.2f), got %.2f",
		origGrade.Total, firstApprove.Total)

	// The SECOND approve event (re-approve) must record the new total.
	secondApprove := approveEvents[1]
	assert.InDelta(t, newGrade.Total, secondApprove.Total, 1e-6,
		"second approve audit event (re-approve) must record the new total (%.2f), got %.2f",
		newGrade.Total, secondApprove.Total)

	t.Logf("regrade_finalize: PASSED — orig_total=%.2f new_total=%.2f, final_grade_rows=%d, archive=%q verified, audit_preserved_original=%.2f",
		origGrade.Total, newGrade.Total, finalGradeCount, archiveKey, firstApprove.Total)
}
