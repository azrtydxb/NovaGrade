// Package integration — Phase-3 marking-guide integration test.
//
// TestMarkingGuideFlow proves the full import→deterministic-grading pipeline:
//  1. Imports a guide (via the real guide API) with numeric, multi_step, and partial entries.
//  2. Creates a submission linked to the assessment_version.
//  3. Drives the submission through the pipeline with a grade worker that loads
//     the guide from the DB store (the real Part A wiring, mirrored in-process).
//  4. Asserts deterministic marks (no LLM for guide-covered questions), lock-on-grading,
//     and that a second import creates v2 without being blocked by the v1 lock.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/orchestrator"
	pipelineGrade "github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Guide content + expected deterministic marks
//
// Q1: numeric, expected=9.8, tolerance=0.1 abs, max_marks=3
//     Student answer: "9.75" → |9.75-9.8| = 0.05 ≤ 0.1 → awarded 3
//
// Q2: multi_step, 2 steps, max_marks=4
//     Step1: exact_ci "F=ma",  marks=2  → student writes "F=ma"
//     Step2: numeric 20, tol=0 abs, marks=2 → student writes "20"
//     Both match → awarded 4
//
// Q3: partial, max_marks=3
//     Criterion1: accept ["photosynthesis"], marks=2
//     Criterion2: accept ["chlorophyll"],    marks=2  (clamped to max_marks=3)
//     Student answer contains both → total=4, clamped to 3 → awarded 3
//
// Q4: NOT in guide → falls through to LLMJudge.
//     Fake AI returns {"awarded_marks":2,...}
// ─────────────────────────────────────────────────────────────────────────────

const (
	mgQ1ExpectedMarks = 3.0
	mgQ2ExpectedMarks = 4.0
	mgQ3ExpectedMarks = 3.0
	// Q4 is LLM-judged; fake returns 2.
	mgQ4ExpectedMarks = 2.0
)

// guideJSON is the marking guide imported via the API for the Phase-3 test.
// It deliberately uses all three new deterministic match types.
const guideJSON = `{
  "Q1": {
    "max_marks": 3,
    "match": "numeric",
    "numeric_answer": 9.8,
    "tolerance": 0.1,
    "tolerance_type": "abs"
  },
  "Q2": {
    "max_marks": 4,
    "match": "multi_step",
    "steps": [
      {"match": "exact_ci", "answer": "F=ma", "marks": 2},
      {"match": "numeric", "numeric_answer": 20, "tolerance": 0, "tolerance_type": "abs", "marks": 2}
    ]
  },
  "Q3": {
    "max_marks": 3,
    "match": "partial",
    "criteria": [
      {"accept": ["photosynthesis"], "marks": 2},
      {"accept": ["chlorophyll"],    "marks": 2}
    ]
  }
}`

// ─────────────────────────────────────────────────────────────────────────────
// Fake AI server for the marking guide test.
//
// The qwen3-vl student answers are crafted to trigger deterministic matchers:
//   Q1: "9.75"                        → numeric 9.8 ±0.1 → 3 marks
//   Q2: "F=ma\n20"                    → step1 exact_ci + step2 numeric 20 → 4 marks
//   Q3: "photosynthesis chlorophyll"  → both partial criteria → 4→clamped 3 marks
//   Q4: real answer text              → falls to LLM → fake returns 2 marks
// ─────────────────────────────────────────────────────────────────────────────

// newMarkingGuideAIServer returns a fake AI server whose transcript is wired to
// the guideJSON above. It counts "grade-model" calls so the test can assert
// guide-covered questions did NOT hit the LLM.
func newMarkingGuideAIServer(t *testing.T, gradeCallCounter *int64) *httptest.Server {
	t.Helper()

	// qwen3 structure: Q1 (3 marks), Q2 (4 marks), Q3 (3 marks), Q4 (5 marks, not in guide).
	qwen3Response := `[{"section":null,"question_no":"Q1","max_marks":3,"question_text":"What is the acceleration due to gravity?"},{"section":null,"question_no":"Q2","max_marks":4,"question_text":"Apply Newton second law. Find force for 2kg at 10 m/s^2."},{"section":null,"question_no":"Q3","max_marks":3,"question_text":"Describe the role of chlorophyll in plants."},{"section":null,"question_no":"Q4","max_marks":5,"question_text":"What is osmosis?"}]`

	// qwen3-vl answers crafted to fire deterministic matchers for Q1-Q3.
	qwen3VlResponse := `[{"question_no":"Q1","student_answer":"9.75"},{"question_no":"Q2","student_answer":"F=ma\n20"},{"question_no":"Q3","student_answer":"photosynthesis chlorophyll"},{"question_no":"Q4","student_answer":"Osmosis is the movement of water across a semipermeable membrane."}]`

	responses := map[string]string{
		"dots.ocr": "Q1 (3 marks)\nQ2 (4 marks)\nQ3 (3 marks)\nQ4 (5 marks)",
		"qwen3":    qwen3Response,
		"qwen3-vl": qwen3VlResponse,
		// grade-model: only called for Q4 (not in guide).
		"grade-model": `{"awarded_marks":2,"justification":"Correct definition","grade_confidence":0.97}`,
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

		// Track LLM grade model calls.
		if req.Model == "grade-model" {
			atomic.AddInt64(gradeCallCounter, 1)
		}

		content, ok := responses[req.Model]
		if !ok {
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
	return srv
}

// ─────────────────────────────────────────────────────────────────────────────
// gradeHandlerWithStore is an in-process grade worker that uses the DB store
// to load guides — mirroring the Part A wiring in cmd/grade/main.go.
// This is used by TestMarkingGuideFlow so the REAL guide-loading + locking
// logic runs in the test without launching a separate process.
// ─────────────────────────────────────────────────────────────────────────────
func gradeHandlerWithStore(
	ctx context.Context,
	env contracts.Envelope,
	obj *store.ObjStore,
	st *store.Store,
	bus *queue.Bus,
	prov providers.AIProvider,
	bucket, gradeModel string,
) error {
	// 1. Load transcript.
	transcriptKey := fmt.Sprintf("%s/%s/transcript.v1.json", env.TenantID, env.SubmissionID)
	tData, err := obj.Get(ctx, bucket, transcriptKey)
	if err != nil {
		return fmt.Errorf("grade[mg-test]: get transcript: %w", err)
	}
	var paper contracts.TranscribedPaper
	if err := json.Unmarshal(tData, &paper); err != nil {
		return fmt.Errorf("grade[mg-test]: parse transcript: %w", err)
	}

	// 2. Build mark scheme — mirrors cmd/grade/main.go handleEnvelope Part A logic.
	llmJudge := pipelineGrade.NewLLMJudge(prov, gradeModel)
	var scheme pipelineGrade.MarkScheme = llmJudge
	var guideLoaded bool

	if st != nil {
		submissionUID, parseErr := uuid.Parse(env.SubmissionID)
		if parseErr == nil {
			sub, subErr := st.GetSubmission(ctx, submissionUID)
			if subErr != nil && !errors.Is(subErr, store.ErrNotFound) {
				return fmt.Errorf("grade[mg-test]: get submission %s: %w", env.SubmissionID, subErr)
			}
			if subErr == nil && sub.AssessmentVersionID != nil {
				avid := *sub.AssessmentVersionID
				tenantUID, tenantParseErr := uuid.Parse(env.TenantID)
				if tenantParseErr == nil {
					mg, guideErr := st.GetLatestGuide(ctx, tenantUID, avid)
					if guideErr == nil {
						g, guideParseErr := pipelineGrade.LoadGuideFromJSON(mg.Content)
						if guideParseErr == nil {
							scheme = pipelineGrade.NewGuideMarkScheme(g, llmJudge, prov, gradeModel)
							guideLoaded = true
							// Lock-on-grading-start (idempotent — preserves locked_at if already locked).
							_ = st.LockGuide(ctx, tenantUID, mg.ID)
						}
					} else if !errors.Is(guideErr, store.ErrNotFound) {
						return fmt.Errorf("grade[mg-test]: GetLatestGuide: %w", guideErr)
					}
				}
			}
		}
	}

	// 2b. Fallback: obj-store guide (not expected in this test but kept for completeness).
	if !guideLoaded {
		guideKey := fmt.Sprintf("%s/%s/guide.v1.json", env.TenantID, env.SubmissionID)
		guideData, guideErr := obj.Get(ctx, bucket, guideKey)
		if guideErr == nil {
			g, parseErr := pipelineGrade.LoadGuideFromJSON(guideData)
			if parseErr == nil {
				scheme = pipelineGrade.NewGuideMarkScheme(g, llmJudge, prov, gradeModel)
				guideLoaded = true
			}
		} else if !errors.Is(guideErr, store.ErrNotFound) {
			return fmt.Errorf("grade[mg-test]: get obj-store guide: %w", guideErr)
		}
	}
	_ = guideLoaded

	// 3. Grade.
	gradedPaper, err := pipelineGrade.GradePaper(ctx, scheme, paper)
	if err != nil {
		return fmt.Errorf("grade[mg-test]: grade paper: %w", err)
	}

	// 4. Persist graded.v1.json.
	gradedKey := fmt.Sprintf("%s/%s/graded.v1.json", env.TenantID, env.SubmissionID)
	gJSON, _ := json.Marshal(gradedPaper)
	if err := obj.Put(ctx, bucket, gradedKey, gJSON, "application/json"); err != nil {
		return fmt.Errorf("grade[mg-test]: upload graded: %w", err)
	}

	// 5. Persist grade-result.json summary.
	type summary struct {
		QuestionCount int     `json:"question_count"`
		TotalMarks    float64 `json:"total_marks"`
		MaxMarks      float64 `json:"max_marks"`
		Score100      float64 `json:"score_100"`
		GradedKey     string  `json:"graded_key"`
	}
	summaryJSON, _ := json.Marshal(summary{
		QuestionCount: len(gradedPaper.Questions),
		TotalMarks:    gradedPaper.Total,
		MaxMarks:      gradedPaper.MaxTotal,
		Score100:      gradedPaper.Score100,
		GradedKey:     gradedKey,
	})
	summaryKey := fmt.Sprintf("%s/%s/grade-result.json", env.TenantID, env.SubmissionID)
	if err := obj.Put(ctx, bucket, summaryKey, summaryJSON, "application/json"); err != nil {
		return fmt.Errorf("grade[mg-test]: upload summary: %w", err)
	}

	// 6. Publish grade.result.
	return bus.Publish(ctx, "results.q", contracts.Envelope{
		TenantID:      env.TenantID,
		Principal:     env.Principal,
		SubmissionID:  env.SubmissionID,
		BatchID:       env.BatchID,
		Stage:         contracts.StageGradeResult,
		Attempt:       env.Attempt,
		CorrelationID: env.CorrelationID,
		PayloadRef:    summaryKey,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Guide API server helper (for the marking guide test)
// ─────────────────────────────────────────────────────────────────────────────

// newGuideAPIServer stands up an httptest.Server with only the guide endpoints
// wired to inf.pgStore. The JWT must carry a role that has ActionEditTunables
// (operator, group_admin, or school_admin).
func newGuideAPIServer(t *testing.T, inf *testInfra) *httptest.Server {
	t.Helper()
	if os.Getenv("JWT_SIGNING_KEY") == "" {
		t.Setenv("JWT_SIGNING_KEY", testSigningKey)
	}

	gh := &api.GuideHandlers{
		Store:      inf.pgStore,
		DeployMode: "onprem",
	}

	r := chi.NewRouter()
	r.Use(chi_middleware.Recoverer)
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(auth.NewAPIKeyResolver()))
		r.Post("/assessment-versions/{avid}/guides", gh.ImportGuide)
		r.Get("/assessment-versions/{avid}/guides", gh.ListGuides)
		r.Get("/assessment-versions/{avid}/guides/latest", gh.GetLatestGuide)
		r.Post("/guides/{id}/lock", gh.LockGuide)
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// mintAdminJWT issues a JWT with school_admin role (has ActionEditTunables).
func mintAdminJWT(t *testing.T) string {
	t.Helper()
	t.Setenv("JWT_SIGNING_KEY", testSigningKey)
	tok, err := auth.IssueToken(auth.Principal{
		ID:       "test-admin-001",
		TenantID: testTenantID.String(),
		Roles:    []domain.Role{domain.RoleSchoolAdmin},
	}, 1*time.Hour)
	require.NoError(t, err, "issue admin JWT")
	return tok
}

// importGuide POSTs guideBody to the guide API and returns the guide ID and version.
func importGuide(t *testing.T, guideAPIURL, adminJWT, avid, guideName string, guideBody []byte) (guideID uuid.UUID, version int) {
	t.Helper()

	url := fmt.Sprintf("%s/v1/assessment-versions/%s/guides", guideAPIURL, avid)
	if guideName != "" {
		url += "?name=" + guideName
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(guideBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminJWT)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode,
		"POST guide expected 201, got %d: %s", resp.StatusCode, string(body))

	var result struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		Name    string `json:"name"`
		Locked  bool   `json:"locked"`
	}
	require.NoError(t, json.Unmarshal(body, &result), "parse guide import response: %s", body)

	id, err := uuid.Parse(result.ID)
	require.NoError(t, err, "parse guide id")
	return id, result.Version
}

// ensureAssessmentAndVersion inserts an assessment + assessment_version row
// for the test tenant. Returns the assessment_version UUID.
func ensureAssessmentAndVersion(t *testing.T, inf *testInfra, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	conn, err := pgxConnect(ctx, inf.dbCfg)
	require.NoError(t, err, "open pgx connection for ensureAssessmentAndVersion")
	defer func() { _ = conn.Close(ctx) }()

	// Insert assessment.
	var assessmentID uuid.UUID
	err = conn.QueryRow(ctx,
		`INSERT INTO assessment (tenant_id, title)
		 VALUES ($1, 'Phase-3 Integration Test Assessment')
		 RETURNING id`,
		tenantID,
	).Scan(&assessmentID)
	require.NoError(t, err, "insert assessment")

	// Insert assessment_version.
	var avID uuid.UUID
	err = conn.QueryRow(ctx,
		`INSERT INTO assessment_version (tenant_id, assessment_id, version_number)
		 VALUES ($1, $2, 1)
		 RETURNING id`,
		tenantID, assessmentID,
	).Scan(&avID)
	require.NoError(t, err, "insert assessment_version")

	return avID
}

// postSubmissionWithAVID creates a submission via the API and immediately patches
// its assessment_version_id in the DB (the API does not expose this field yet).
// The patch happens synchronously before the pipeline can run the grade stage.
func postSubmissionWithAVID(
	t *testing.T,
	inf *testInfra,
	tenantID uuid.UUID,
	avid uuid.UUID,
	apiURL, jwtToken string,
	pdfData []byte,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	// Create submission via API to trigger the pipeline start.
	subID := postSubmission(t, apiURL, jwtToken, pdfData)

	// Patch the assessment_version_id before grading starts.
	// Because grading is async (triggered after transcribing), there is enough
	// time between submission creation and grade dispatch for this update.
	conn, err := pgxConnect(ctx, inf.dbCfg)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	_, err = conn.Exec(ctx,
		`UPDATE submission SET assessment_version_id = $1 WHERE id = $2 AND tenant_id = $3`,
		avid, subID, tenantID,
	)
	require.NoError(t, err, "patch assessment_version_id on submission %s", subID)

	return subID
}

// ─────────────────────────────────────────────────────────────────────────────
// TestMarkingGuideFlow — Phase-3 end-to-end integration test
// ─────────────────────────────────────────────────────────────────────────────

// TestMarkingGuideFlow proves:
//  1. Guide is importable via the API (validates + stores in DB).
//  2. A submission linked to the assessment_version is graded using the DB guide.
//  3. Deterministic marks (numeric, multi_step, partial) are exact and stable.
//  4. Guide-covered questions did NOT invoke the LLM grade model (only Q4 did).
//  5. Guide v1 is locked after grading (lock-on-grading-start).
//  6. A second import creates v2 (lock did not block new import).
func TestMarkingGuideFlow(t *testing.T) {
	inf := startInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ensureTenant(t, inf, testTenantID)

	// ── Step 1: Create assessment + assessment_version ──────────────────────
	avid := ensureAssessmentAndVersion(t, inf, testTenantID)
	t.Logf("marking-guide: assessment_version_id=%s", avid)

	// ── Step 2: Import guide v1 via the real guide API ──────────────────────
	adminJWT := mintAdminJWT(t)
	guideAPISrv := newGuideAPIServer(t, inf)

	guideV1ID, guideV1Version := importGuide(t, guideAPISrv.URL, adminJWT, avid.String(), "Phase3 Guide v1", []byte(guideJSON))
	t.Logf("marking-guide: imported guide v1 id=%s version=%d", guideV1ID, guideV1Version)
	assert.Equal(t, 1, guideV1Version, "first import must be version 1")

	// Verify the guide is not yet locked before grading.
	guideBeforeGrading, err := inf.pgStore.GetLatestGuide(ctx, testTenantID, avid)
	require.NoError(t, err)
	assert.False(t, guideBeforeGrading.Locked, "guide must not be locked before grading")

	// ── Step 3: Set up fake AI with grade call counter ───────────────────────
	var gradeCallCounter int64
	fakeAI := newMarkingGuideAIServer(t, &gradeCallCounter)
	aiProv := newFakeAIProvider(fakeAI)

	// ── Step 4: Wire pipeline with the DB-guide grade worker ─────────────────
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

	// Grade worker uses DB store — the real Part A guide-loading + locking logic.
	gradeBus := mustConnectBus(t, inf.amqpURL, 0)
	require.NoError(t, gradeBus.Consume(workerCtx, "grade.q", func(env contracts.Envelope) error {
		return gradeHandlerWithStore(workerCtx, env, inf.objStore, inf.pgStore, gradeBus, aiProv, inf.bucket, "grade-model")
	}), "start grade consumer")

	// ── Step 5: Submit PDF + patch assessment_version_id ────────────────────
	scannerJWT := mintTestJWT(t)
	apiSrv := newAPIServer(t, inf)

	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err, "read sample PDF")

	subID := postSubmissionWithAVID(t, inf, testTenantID, avid, apiSrv.URL, scannerJWT, pdfBytes)
	t.Logf("marking-guide: submission_id=%s", subID)

	// ── Step 6: Poll until teacher_review or failed ──────────────────────────
	finalState := pollSubmission(ctx, t, inf.pgStore,
		subID,
		[]contracts.SubmissionState{
			contracts.StateTeacherReview,
			contracts.StateFailed,
		},
		4*time.Minute,
	)
	require.Equal(t, contracts.StateTeacherReview, finalState,
		"submission must reach teacher_review after guide-based grading")

	// ── Step 7: Assert deterministic marks in GradedPaper ───────────────────
	gradedKey := fmt.Sprintf("%s/%s/graded.v1.json", testTenantID, subID)
	gradedData, err := inf.objStore.Get(ctx, inf.bucket, gradedKey)
	require.NoError(t, err, "graded.v1.json must exist at %q", gradedKey)

	var gradedPaper contracts.GradedPaper
	require.NoError(t, json.Unmarshal(gradedData, &gradedPaper), "parse graded paper")
	require.NotEmpty(t, gradedPaper.Questions, "graded paper must have questions")

	// Build a lookup map by question_no for easy assertion.
	marksByQ := map[string]float64{}
	justByQ := map[string]string{}
	for _, q := range gradedPaper.Questions {
		marksByQ[q.QuestionNo] = q.AwardedMarks
		justByQ[q.QuestionNo] = q.Justification
		t.Logf("  question %s: awarded=%.1f max=%.1f justification=%q",
			q.QuestionNo, q.AwardedMarks, q.MaxMarks, q.Justification)
	}

	// Deterministic marks must be exact and stable (no LLM variance).
	assert.Equal(t, mgQ1ExpectedMarks, marksByQ["Q1"],
		"Q1 (numeric ±0.1): 9.75 should award %.1f marks", mgQ1ExpectedMarks)
	assert.Equal(t, mgQ2ExpectedMarks, marksByQ["Q2"],
		"Q2 (multi_step): both steps should award %.1f marks", mgQ2ExpectedMarks)
	assert.Equal(t, mgQ3ExpectedMarks, marksByQ["Q3"],
		"Q3 (partial clamped): two criteria clamped should award %.1f marks", mgQ3ExpectedMarks)

	// Q4 is LLM-judged; fake model returns 2.
	assert.Equal(t, mgQ4ExpectedMarks, marksByQ["Q4"],
		"Q4 (LLM-judged): fake model should award %.1f marks", mgQ4ExpectedMarks)

	// ── Step 8: Assert only Q4 used LLM (1 call total) ──────────────────────
	// Q1, Q2, Q3 are guide-covered with deterministic match types → zero LLM calls.
	// Q4 is absent from the guide → 1 LLM call.
	gradeModelCalls := atomic.LoadInt64(&gradeCallCounter)
	assert.Equal(t, int64(1), gradeModelCalls,
		"only Q4 should invoke the grade LLM model (got %d calls)", gradeModelCalls)
	t.Logf("marking-guide: grade model LLM calls = %d (expected 1 for Q4 only)", gradeModelCalls)

	// ── Step 9: Assert guide v1 is now LOCKED ────────────────────────────────
	lockedGuide, err := inf.pgStore.GetLatestGuide(ctx, testTenantID, avid)
	require.NoError(t, err, "must be able to read guide after grading")
	assert.True(t, lockedGuide.Locked, "guide v1 must be locked after lock-on-grading-start")
	assert.Equal(t, 1, lockedGuide.Version, "locked guide must be v1")
	t.Logf("marking-guide: guide v1 locked=%v lockedAt=%v", lockedGuide.Locked, lockedGuide.LockedAt)

	// ── Step 10: Second import creates v2 (lock did not block) ───────────────
	guideV2ID, guideV2Version := importGuide(t, guideAPISrv.URL, adminJWT, avid.String(), "Phase3 Guide v2", []byte(guideJSON))
	t.Logf("marking-guide: imported guide v2 id=%s version=%d", guideV2ID, guideV2Version)
	assert.Equal(t, 2, guideV2Version, "second import must create version 2")
	assert.NotEqual(t, guideV1ID, guideV2ID, "v1 and v2 must have different IDs")

	// The new v2 guide must not be locked.
	allVersions, err := inf.pgStore.ListGuideVersions(ctx, testTenantID, avid)
	require.NoError(t, err)
	require.Len(t, allVersions, 2, "must have exactly 2 guide versions")

	// Versions are ordered DESC (newest first).
	assert.Equal(t, 2, allVersions[0].Version, "first in list must be v2")
	assert.False(t, allVersions[0].Locked, "v2 must not be locked")
	assert.Equal(t, 1, allVersions[1].Version, "second in list must be v1")
	assert.True(t, allVersions[1].Locked, "v1 must remain locked")

	t.Logf("marking-guide: PASSED — deterministic marks Q1=%.1f Q2=%.1f Q3=%.1f Q4=%.1f, LLM calls=%d, v1 locked, v2 not locked",
		marksByQ["Q1"], marksByQ["Q2"], marksByQ["Q3"], marksByQ["Q4"], gradeModelCalls)

	// Log the justifications for guide-covered questions to confirm deterministic path.
	t.Logf("marking-guide: Q1 justification: %q", justByQ["Q1"])
	t.Logf("marking-guide: Q2 justification: %q", justByQ["Q2"])
	t.Logf("marking-guide: Q3 justification: %q", justByQ["Q3"])
	t.Logf("marking-guide: Q4 justification: %q (LLM path)", justByQ["Q4"])
}
