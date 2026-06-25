// Package integration provides end-to-end integration tests for the NovaGrade
// pipeline. Each test scenario spins up real infrastructure via testcontainers
// (Postgres, RabbitMQ, MinIO), wires in a fake AI provider via httptest.Server
// (so no live GPU/model is needed), and runs the orchestrator plus stage workers
// in-process as goroutines.
//
// # Gate
//
// All tests are skipped when SKIP_DOCKER_TESTS is set or -short is passed.
//
// # Architecture
//
// Workers are driven in-process by calling the same internal packages that
// the cmd/* binaries use (pipeline, pipeline/grade, providers, store, queue).
// The orchestrator is driven via the exported internal/orchestrator package.
//
// # Fake AI Provider
//
// An httptest.Server returns canned OpenAI-compatible responses keyed on the
// model name in the request body. The happy-path canned data is CLEAN
// (no low-confidence reads, no blank answers, no checksum mismatch), so the
// happy-path test proceeds from transcribing → grading → teacher_review without
// triggering any quality gates.
//
// # State Walk (happy path)
//
//	uploaded → queued → splitting_pages → extracting_metadata →
//	transcribing → grading → teacher_review
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chi_middleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/orchestrator"
	"github.com/azrtydxb/novagrade/internal/pipeline"
	pipelineGrade "github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/queue"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Infrastructure helpers
// ─────────────────────────────────────────────────────────────────────────────

// testInfra holds the three real infrastructure components for a test.
type testInfra struct {
	pgStore  *store.Store
	objStore *store.ObjStore
	bus      *queue.Bus
	bucket   string
	amqpURL  string
	dbCfg    store.DBConfig // for raw SQL helpers
}

// startInfra spins up Postgres, RabbitMQ, and MinIO testcontainers; runs
// migrations; declares queue topology; and ensures the bucket exists.
// All containers are terminated at t.Cleanup.
func startInfra(t *testing.T) *testInfra {
	t.Helper()
	if os.Getenv("SKIP_DOCKER_TESTS") != "" || testing.Short() {
		t.Skip("requires Docker (set SKIP_DOCKER_TESTS to skip, or use -short flag)")
	}
	ctx := context.Background()

	// ── Postgres ──────────────────────────────────────────────────────────────
	pgContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:16-alpine",
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     "novagrade",
				"POSTGRES_PASSWORD": "novagrade",
				"POSTGRES_DB":       "novagrade_test",
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start postgres")
	t.Cleanup(func() { _ = pgContainer.Terminate(context.Background()) })

	pgHost, err := pgContainer.Host(ctx)
	require.NoError(t, err)
	pgPortNat, err := pgContainer.MappedPort(ctx, "5432")
	require.NoError(t, err)
	pgPortNum, err := strconv.Atoi(pgPortNat.Port())
	require.NoError(t, err)

	pgCfg := store.DBConfig{
		Host:     pgHost,
		Port:     pgPortNum,
		User:     "novagrade",
		Password: "novagrade",
		Database: "novagrade_test",
		SSLMode:  "disable",
	}
	pgStore, err := store.NewStore(ctx, pgCfg)
	require.NoError(t, err, "connect to postgres")
	t.Cleanup(pgStore.Close)
	require.NoError(t, pgStore.MigrateUp(ctx), "run migrations")

	// ── RabbitMQ ─────────────────────────────────────────────────────────────
	rabbitContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "rabbitmq:3.13",
			ExposedPorts: []string{"5672/tcp"},
			WaitingFor:   wait.ForListeningPort("5672/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start rabbitmq")
	t.Cleanup(func() { _ = rabbitContainer.Terminate(context.Background()) })

	rabbitHost, err := rabbitContainer.Host(ctx)
	require.NoError(t, err)
	rabbitPort, err := rabbitContainer.MappedPort(ctx, "5672")
	require.NoError(t, err)
	amqpURL := fmt.Sprintf("amqp://guest:guest@%s:%s/", rabbitHost, rabbitPort.Port())

	bus, err := queue.Connect(amqpURL)
	require.NoError(t, err, "connect to rabbitmq")
	t.Cleanup(func() { _ = bus.Close() })
	require.NoError(t, bus.DeclareTopology(), "declare topology")

	// ── MinIO ─────────────────────────────────────────────────────────────────
	minioContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio",
			Cmd:          []string{"server", "/data"},
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     "minioadmin",
				"MINIO_ROOT_PASSWORD": "minioadmin",
			},
			WaitingFor: wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start minio")
	t.Cleanup(func() { _ = minioContainer.Terminate(context.Background()) })

	minioHost, err := minioContainer.Host(ctx)
	require.NoError(t, err)
	minioPort, err := minioContainer.MappedPort(ctx, "9000")
	require.NoError(t, err)

	objStore, err := store.New(store.Config{
		Endpoint:  fmt.Sprintf("%s:%s", minioHost, minioPort.Port()),
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		UseSSL:    false,
	})
	require.NoError(t, err, "connect to minio")

	const bucket = "novagrade-test"
	require.NoError(t, objStore.EnsureBucket(ctx, bucket), "ensure bucket")

	return &testInfra{
		pgStore:  pgStore,
		objStore: objStore,
		bus:      bus,
		bucket:   bucket,
		amqpURL:  amqpURL,
		dbCfg:    pgCfg,
	}
}

// ensureTenant inserts a school row for tenantID (if it doesn't already exist).
// The submissions table has tenant_id FK → school.id, so the tenant must exist
// before submitting.
func ensureTenant(t *testing.T, inf *testInfra, tenantID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	// Open a raw pgx connection to run the INSERT.
	conn, err := pgxConnect(ctx, inf.dbCfg)
	require.NoError(t, err, "open pgx connection for ensureTenant")
	defer func() { _ = conn.Close(ctx) }()

	// INSERT ... ON CONFLICT DO NOTHING so it's idempotent.
	_, err = conn.Exec(ctx,
		`INSERT INTO school (id, name, slug, country) VALUES ($1, $2, $3, '')
         ON CONFLICT (id) DO NOTHING`,
		tenantID, "Integration Test Tenant", tenantID.String(),
	)
	require.NoError(t, err, "insert school for tenant %s", tenantID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Fake AI Provider
// ─────────────────────────────────────────────────────────────────────────────

// newFakeAIServer builds an httptest.Server that returns canned OpenAI-compatible
// responses keyed on the "model" field in the request body.
//
// The canned transcript is CLEAN (no low-confidence, no blank answers, checksum OK
// with no expected_total), so the happy-path test progresses all the way to
// teacher_review without triggering any quality gates.
func newFakeAIServer(t *testing.T) *httptest.Server {
	t.Helper()

	// Canned responses per model.
	responses := map[string]string{
		// OCR: faithful page text describing exam content.
		"dots.ocr": "Question 1a (2 marks)\nWhat is 2+2?\n\nQuestion 1b (3 marks)\nExplain gravity.",

		// Reason (structure): JSON array of questions with NO expected_total
		// so no checksum gate is possible.
		"qwen3": `[{"section":null,"question_no":"1a","max_marks":2,"question_text":"What is 2+2?"},{"section":null,"question_no":"1b","max_marks":3,"question_text":"Explain gravity."}]`,

		// VLM (answers): non-blank answers to pass blank-answer gate.
		"qwen3-vl": `[{"question_no":"1a","student_answer":"4"},{"question_no":"1b","student_answer":"Objects attract each other due to gravitational force."}]`,

		// Grade model: valid grade decision.
		"grade-model": `{"awarded_marks":2,"justification":"Correct answer","grade_confidence":0.98}`,
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

// newFakeAIProvider returns a VLLMProvider configured to hit fakeServer.
func newFakeAIProvider(fakeServer *httptest.Server) *providers.VLLMProvider {
	return providers.NewVLLMProvider(providers.VLLMConfig{
		BaseURL:    fakeServer.URL,
		MaxRetries: 1,
		Timeout:    30 * time.Second,
	})
}

// newFailing500AIServer returns an httptest.Server that always returns HTTP 500.
// Used for TestForcedTechnicalFailure to force the DLQ/failed path.
func newFailing500AIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ─────────────────────────────────────────────────────────────────────────────
// JWT / auth helpers
// ─────────────────────────────────────────────────────────────────────────────

// testSigningKey is the HMAC key used to mint JWTs in integration tests.
// NEVER use in production.
const testSigningKey = "novagrade-integration-test-key-not-for-production"

// testTenantID is the fixed tenant UUID for all integration tests.
var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// mintTestJWT issues a short-lived JWT for the test tenant.
// It sets JWT_SIGNING_KEY in the test environment via t.Setenv.
func mintTestJWT(t *testing.T) string {
	t.Helper()
	t.Setenv("JWT_SIGNING_KEY", testSigningKey)
	tok, err := auth.IssueToken(auth.Principal{
		ID:       "test-scanner-001",
		TenantID: testTenantID.String(),
		Roles:    []domain.Role{domain.RoleScanner, domain.RoleTeacher},
	}, 1*time.Hour)
	require.NoError(t, err, "issue test JWT")
	return tok
}

// ─────────────────────────────────────────────────────────────────────────────
// API server helper
// ─────────────────────────────────────────────────────────────────────────────

// objStoreAdapter adapts *store.ObjStore to api.ObjectStore.
type objStoreAdapter struct {
	s      *store.ObjStore
	bucket string
}

func (a *objStoreAdapter) PutObject(ctx context.Context, key string, data []byte) error {
	return a.s.Put(ctx, a.bucket, key, data, "application/pdf")
}

func (a *objStoreAdapter) GetObject(ctx context.Context, key string) ([]byte, error) {
	return a.s.Get(ctx, a.bucket, key)
}

// newAPIServer builds an httptest.Server running api.Handlers wired to inf.
func newAPIServer(t *testing.T, inf *testInfra) *httptest.Server {
	t.Helper()
	if os.Getenv("JWT_SIGNING_KEY") == "" {
		t.Setenv("JWT_SIGNING_KEY", testSigningKey)
	}

	h := &api.Handlers{
		Store:      inf.pgStore,
		Bus:        inf.bus,
		Objects:    &objStoreAdapter{s: inf.objStore, bucket: inf.bucket},
		DeployMode: "onprem",
	}

	r := chi.NewRouter()
	r.Use(chi_middleware.Recoverer)
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(auth.NewAPIKeyResolver()))
		r.Post("/submissions", h.PostSubmission)
		r.Get("/submissions/{id}", h.GetSubmission)
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// ─────────────────────────────────────────────────────────────────────────────
// Orchestrator bus adapter (satisfies orchestrator.MessageBus)
// ─────────────────────────────────────────────────────────────────────────────

// busAdapter adapts *queue.Bus to orchestrator.MessageBus.
type busAdapter struct {
	b *queue.Bus
}

func (a *busAdapter) Publish(ctx context.Context, q string, env contracts.Envelope) error {
	return a.b.Publish(ctx, q, env)
}

func (a *busAdapter) Consume(ctx context.Context, q string, handler func(contracts.Envelope) error) error {
	return a.b.Consume(ctx, q, handler)
}

func (a *busAdapter) MaxAttempts() int { return a.b.MaxAttempts }

// ─────────────────────────────────────────────────────────────────────────────
// In-process stage worker handlers
// ─────────────────────────────────────────────────────────────────────────────

// renderHandler is an in-process implementation of the render stage worker.
// It mirrors cmd/render/main.go handleEnvelope: download PDF from object store,
// render pages via pipeline.RenderPDF (requires pdftoppm/poppler), upload PNGs
// + render-result.json sidecar, publish render.result to results.q.
func renderHandler(ctx context.Context, env contracts.Envelope, obj *store.ObjStore, bus *queue.Bus, bucket string) error {
	// 1. Download the source PDF.
	pdfData, err := obj.Get(ctx, bucket, env.PayloadRef)
	if err != nil {
		return fmt.Errorf("render[test]: get PDF %q: %w", env.PayloadRef, err)
	}

	// 2. Render to a temp directory.
	tmpDir, err := os.MkdirTemp("", "render-int-*")
	if err != nil {
		return fmt.Errorf("render[test]: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	pdfFile := filepath.Join(tmpDir, "source.pdf")
	if err := os.WriteFile(pdfFile, pdfData, 0o600); err != nil {
		return fmt.Errorf("render[test]: write temp PDF: %w", err)
	}
	outDir := filepath.Join(tmpDir, "pages")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("render[test]: mkdir pages: %w", err)
	}

	pages, err := pipeline.RenderPDF(ctx, pdfFile, outDir)
	if err != nil {
		return fmt.Errorf("render[test]: RenderPDF: %w", err)
	}
	if len(pages) == 0 {
		return fmt.Errorf("render[test]: zero content pages — PDF may be blank")
	}

	// 3. Upload page PNGs.
	for i, pngPath := range pages {
		data, err := os.ReadFile(pngPath)
		if err != nil {
			return fmt.Errorf("render[test]: read page %d: %w", i+1, err)
		}
		key := fmt.Sprintf("%s/%s/pages/%d.png", env.TenantID, env.SubmissionID, i+1)
		if err := obj.Put(ctx, bucket, key, data, "image/png"); err != nil {
			return fmt.Errorf("render[test]: upload page %d: %w", i+1, err)
		}
	}

	// 4. Write render-result.json sidecar.
	type renderResult struct {
		PageCount int `json:"page_count"`
	}
	sidecarKey := fmt.Sprintf("%s/%s/render-result.json", env.TenantID, env.SubmissionID)
	sidecar, _ := json.Marshal(renderResult{PageCount: len(pages)})
	if err := obj.Put(ctx, bucket, sidecarKey, sidecar, "application/json"); err != nil {
		return fmt.Errorf("render[test]: upload sidecar: %w", err)
	}

	// 5. Publish render.result to results.q.
	return bus.Publish(ctx, "results.q", contracts.Envelope{
		TenantID:      env.TenantID,
		Principal:     env.Principal,
		SubmissionID:  env.SubmissionID,
		BatchID:       env.BatchID,
		Stage:         contracts.StageRenderResult,
		Attempt:       env.Attempt,
		CorrelationID: env.CorrelationID,
		PayloadRef:    sidecarKey,
	})
}

// transcribeHandler is an in-process implementation of the transcribe stage worker.
// It mirrors cmd/transcribe/main.go handleEnvelope with the supplied AI provider.
//
// The quality flags sidecar it produces is used by the orchestrator to evaluate
// gates. Set clean=true for a clean transcript (no gates tripped); clean=false
// injects a checksum mismatch to force transcription_review_required.
func transcribeHandler(
	ctx context.Context,
	env contracts.Envelope,
	obj *store.ObjStore,
	bus *queue.Bus,
	prov providers.AIProvider,
	bucket string,
	forceGate bool, // when true, injects checksum_ok=false
) error {
	// 1. Read render sidecar for page count.
	type renderResult struct {
		PageCount int `json:"page_count"`
	}
	sidecarData, err := obj.Get(ctx, bucket, env.PayloadRef)
	if err != nil {
		return fmt.Errorf("transcribe[test]: get render sidecar %q: %w", env.PayloadRef, err)
	}
	var rr renderResult
	if err := json.Unmarshal(sidecarData, &rr); err != nil {
		return fmt.Errorf("transcribe[test]: parse render sidecar: %w", err)
	}
	if rr.PageCount <= 0 {
		return fmt.Errorf("transcribe[test]: no pages to transcribe")
	}

	// 2. Download page PNGs.
	pages := make([][]byte, 0, rr.PageCount)
	for n := 1; n <= rr.PageCount; n++ {
		key := fmt.Sprintf("%s/%s/pages/%d.png", env.TenantID, env.SubmissionID, n)
		data, err := obj.Get(ctx, bucket, key)
		if err != nil {
			return fmt.Errorf("transcribe[test]: get page %d: %w", n, err)
		}
		pages = append(pages, data)
	}

	// 3. Transcribe (calls the fake AI provider).
	paper, err := pipeline.Transcribe(ctx, prov, pages, "Mathematics")
	if err != nil {
		return fmt.Errorf("transcribe[test]: pipeline: %w", err)
	}
	paper.SourcePDF = env.PayloadRef

	// 4. Persist transcript.v1.json.
	transcriptKey := fmt.Sprintf("%s/%s/transcript.v1.json", env.TenantID, env.SubmissionID)
	tJSON, _ := json.Marshal(paper)
	if err := obj.Put(ctx, bucket, transcriptKey, tJSON, "application/json"); err != nil {
		return fmt.Errorf("transcribe[test]: upload transcript: %w", err)
	}

	// 5. Build quality flags sidecar.
	type transcribeFlags struct {
		QuestionCount       int      `json:"question_count"`
		LowReadConfidence   int      `json:"low_read_confidence"`
		BlankAnswers        int      `json:"blank_answers"`
		DetectedTotal       float64  `json:"detected_total"`
		ExpectedTotal       *float64 `json:"expected_total"`
		ChecksumOK          bool     `json:"checksum_ok"`
		ChecksumDifference  *float64 `json:"checksum_difference"`
		TranscriptObjectKey string   `json:"transcript_object_key"`
	}

	var detected float64
	lowReads, blanks := 0, 0
	for _, q := range paper.Questions {
		detected += q.MaxMarks
		if q.ReadConfidence < 0.5 {
			lowReads++
		}
		if q.StudentAnswer == "" {
			blanks++
		}
	}

	flags := transcribeFlags{
		QuestionCount:       len(paper.Questions),
		LowReadConfidence:   lowReads,
		BlankAnswers:        blanks,
		DetectedTotal:       detected,
		ExpectedTotal:       paper.ExpectedTotal,
		ChecksumOK:          true,
		TranscriptObjectKey: transcriptKey,
	}
	if paper.ExpectedTotal != nil {
		diff := detected - *paper.ExpectedTotal
		flags.ChecksumDifference = &diff
		flags.ChecksumOK = diff == 0
	}

	if forceGate {
		// Inject a checksum mismatch: declare an expected total that doesn't match
		// detected, so FlagChecksumMismatch is raised and the orchestrator routes
		// the submission to transcription_review_required.
		expectedTotal := detected + 10.0
		diff := detected - expectedTotal
		flags.ExpectedTotal = &expectedTotal
		flags.ChecksumDifference = &diff
		flags.ChecksumOK = false
	}

	flagsJSON, _ := json.Marshal(flags)
	flagsKey := fmt.Sprintf("%s/%s/transcribe-result.json", env.TenantID, env.SubmissionID)
	if err := obj.Put(ctx, bucket, flagsKey, flagsJSON, "application/json"); err != nil {
		return fmt.Errorf("transcribe[test]: upload flags: %w", err)
	}

	// 6. Publish transcribe.result to results.q.
	return bus.Publish(ctx, "results.q", contracts.Envelope{
		TenantID:      env.TenantID,
		Principal:     env.Principal,
		SubmissionID:  env.SubmissionID,
		BatchID:       env.BatchID,
		Stage:         contracts.StageTranscribeResult,
		Attempt:       env.Attempt,
		CorrelationID: env.CorrelationID,
		PayloadRef:    flagsKey,
	})
}

// gradeHandler is an in-process implementation of the grade stage worker.
// Mirrors cmd/grade/main.go handleEnvelope with the supplied AI provider.
func gradeHandler(ctx context.Context, env contracts.Envelope, obj *store.ObjStore, bus *queue.Bus, prov providers.AIProvider, bucket, gradeModel string) error {
	// 1. Load transcript.
	transcriptKey := fmt.Sprintf("%s/%s/transcript.v1.json", env.TenantID, env.SubmissionID)
	tData, err := obj.Get(ctx, bucket, transcriptKey)
	if err != nil {
		return fmt.Errorf("grade[test]: get transcript: %w", err)
	}
	var paper contracts.TranscribedPaper
	if err := json.Unmarshal(tData, &paper); err != nil {
		return fmt.Errorf("grade[test]: parse transcript: %w", err)
	}

	// 2. Grade using LLMJudge (no marking guide in test infra).
	gradedPaper, err := pipelineGrade.GradePaper(ctx, pipelineGrade.NewLLMJudge(prov, gradeModel), paper)
	if err != nil {
		return fmt.Errorf("grade[test]: grade paper: %w", err)
	}

	// 3. Persist graded.v1.json.
	gradedKey := fmt.Sprintf("%s/%s/graded.v1.json", env.TenantID, env.SubmissionID)
	gJSON, _ := json.Marshal(gradedPaper)
	if err := obj.Put(ctx, bucket, gradedKey, gJSON, "application/json"); err != nil {
		return fmt.Errorf("grade[test]: upload graded: %w", err)
	}

	// 4. Persist grade-result.json summary.
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
		return fmt.Errorf("grade[test]: upload summary: %w", err)
	}

	// 5. Publish grade.result to results.q.
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
// Pipeline wiring helper
// ─────────────────────────────────────────────────────────────────────────────

// pipelineConfig bundles options for startPipeline.
type pipelineConfig struct {
	// forceTranscribeGate: when true, the transcribe worker injects a checksum
	// mismatch to force transcription_review_required.
	forceTranscribeGate bool

	// transcribeProvider: the AI provider used by the transcribe worker.
	// If nil, the transcribe worker is not started.
	transcribeProvider providers.AIProvider

	// gradeProvider: the AI provider used by the grade worker.
	// If nil, the grade worker is not started.
	gradeProvider providers.AIProvider

	// gradeModel: model name passed to the grade worker's LLMJudge.
	gradeModel string

	// maxAttempts: override bus.MaxAttempts (0 = use default 3).
	maxAttempts int
}

// startPipeline wires the orchestrator and all stage workers in-process.
// Each worker and the orchestrator get their own AMQP connection (as in prod).
// Returns a cancel function that stops all goroutines.
func startPipeline(ctx context.Context, t *testing.T, inf *testInfra, cfg pipelineConfig) context.CancelFunc {
	t.Helper()
	workerCtx, cancel := context.WithCancel(ctx)

	if cfg.maxAttempts > 0 {
		inf.bus.MaxAttempts = cfg.maxAttempts
	}

	// ── Orchestrator ─────────────────────────────────────────────────────────
	orch := orchestrator.New(inf.pgStore, &busAdapter{inf.bus}, inf.objStore, inf.bucket)
	go func() {
		if err := orch.Start(workerCtx); err != nil && err != context.Canceled {
			t.Logf("orchestrator stopped: %v", err)
		}
	}()

	// ── Render worker ────────────────────────────────────────────────────────
	renderBus := mustConnectBus(t, inf.amqpURL, cfg.maxAttempts)
	require.NoError(t, renderBus.Consume(workerCtx, "render.q", func(env contracts.Envelope) error {
		return renderHandler(workerCtx, env, inf.objStore, renderBus, inf.bucket)
	}), "start render consumer")

	// ── Transcribe worker ────────────────────────────────────────────────────
	if cfg.transcribeProvider != nil {
		transcribeBus := mustConnectBus(t, inf.amqpURL, cfg.maxAttempts)
		forceGate := cfg.forceTranscribeGate
		require.NoError(t, transcribeBus.Consume(workerCtx, "transcribe.q", func(env contracts.Envelope) error {
			return transcribeHandler(workerCtx, env, inf.objStore, transcribeBus, cfg.transcribeProvider, inf.bucket, forceGate)
		}), "start transcribe consumer")
	}

	// ── Grade worker ─────────────────────────────────────────────────────────
	if cfg.gradeProvider != nil {
		gradeBus := mustConnectBus(t, inf.amqpURL, cfg.maxAttempts)
		gradeModel := cfg.gradeModel
		if gradeModel == "" {
			gradeModel = "grade-model"
		}
		require.NoError(t, gradeBus.Consume(workerCtx, "grade.q", func(env contracts.Envelope) error {
			return gradeHandler(workerCtx, env, inf.objStore, gradeBus, cfg.gradeProvider, inf.bucket, gradeModel)
		}), "start grade consumer")
	}

	return cancel
}

// mustConnectBus connects a fresh AMQP connection + declares topology.
// Returns the Bus and registers cleanup.
func mustConnectBus(t *testing.T, amqpURL string, maxAttempts int) *queue.Bus {
	t.Helper()
	b, err := queue.Connect(amqpURL)
	require.NoError(t, err, "connect worker bus")
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.DeclareTopology(), "worker declare topology")
	if maxAttempts > 0 {
		b.MaxAttempts = maxAttempts
	}
	return b
}

// ─────────────────────────────────────────────────────────────────────────────
// Polling helper
// ─────────────────────────────────────────────────────────────────────────────

// pollSubmission polls pgStore until submission id reaches one of targetStates
// or the timeout elapses. Uses exponential backoff (200ms → 2s cap) to avoid
// tight-loop flakiness. Fails the test on timeout.
func pollSubmission(
	ctx context.Context,
	t *testing.T,
	st *store.Store,
	id uuid.UUID,
	targetStates []contracts.SubmissionState,
	timeout time.Duration,
) contracts.SubmissionState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	target := make(map[contracts.SubmissionState]bool, len(targetStates))
	for _, s := range targetStates {
		target[s] = true
	}

	backoff := 200 * time.Millisecond
	for time.Now().Before(deadline) {
		sub, err := st.GetSubmission(ctx, id)
		if err == nil && target[sub.State] {
			return sub.State
		}
		if err == nil {
			t.Logf("poll[%s]: state=%s (waiting for %v)", id, sub.State, targetStates)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled polling submission %s", id)
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
	sub, err := st.GetSubmission(ctx, id)
	if err == nil {
		t.Fatalf("timeout: submission %s stuck in %q (wanted one of %v)", id, sub.State, targetStates)
	} else {
		t.Fatalf("timeout: could not get submission %s: %v", id, err)
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// API submission helper
// ─────────────────────────────────────────────────────────────────────────────

// postSubmission POSTs the PDF bytes as multipart/form-data to apiURL and
// returns the created submission UUID.
func postSubmission(t *testing.T, apiURL, jwtToken string, pdfData []byte) uuid.UUID {
	t.Helper()

	// Build a multipart body with the MIME type set on the file part (the API
	// handler checks the file part's Content-Type header for "application/pdf").
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="pdf"; filename="test.pdf"`}
	h["Content-Type"] = []string{"application/pdf"}
	part, err := mw.CreatePart(h)
	require.NoError(t, err, "create multipart part")
	_, err = part.Write(pdfData)
	require.NoError(t, err, "write pdf to part")
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, apiURL+"/v1/submissions", &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+jwtToken)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode,
		"POST /v1/submissions expected 201, got %d: %s", resp.StatusCode, body)

	var result struct {
		SubmissionID string `json:"submission_id"`
	}
	require.NoError(t, json.Unmarshal(body, &result), "parse 201 response: %s", body)
	id, err := uuid.Parse(result.SubmissionID)
	require.NoError(t, err, "parse submission_id")
	return id
}

// ─────────────────────────────────────────────────────────────────────────────
// Sample PDF helper
// ─────────────────────────────────────────────────────────────────────────────

// pgxConnect opens a single pgx.Conn using cfg. Callers must Close it.
func pgxConnect(ctx context.Context, cfg store.DBConfig) (*pgx.Conn, error) {
	return pgx.Connect(ctx, cfg.DSN())
}

// samplePDFPath returns the path to the sample1.pdf test fixture, searching
// relative to this test file's directory and the module root.
func samplePDFPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile = .../test/integration/pipeline_test.go
	// Module root = two directories up.
	moduleRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	candidates := []string{
		filepath.Join(moduleRoot, "internal", "pipeline", "testdata", "sample1.pdf"),
		filepath.Join(moduleRoot, "tests", "sample1.pdf"),
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Fatalf("sample1.pdf not found; searched: %v", candidates)
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: Happy Path
// ─────────────────────────────────────────────────────────────────────────────

// TestHappyPath exercises the full pipeline end-to-end:
//
//  1. Submits a real PDF via the API.
//  2. The orchestrator dispatches to render.q (state: queued).
//  3. Render worker processes the PDF, uploads PNGs + sidecar, publishes render.result.
//  4. Orchestrator advances through splitting_pages → extracting_metadata → transcribing.
//  5. Transcribe worker calls the fake AI provider, produces a CLEAN transcript
//     (no quality flags), publishes transcribe.result.
//  6. Orchestrator advances to grading, dispatches to grade.q.
//  7. Grade worker grades the paper, uploads graded.v1.json, publishes grade.result.
//  8. Orchestrator advances to teacher_review (terminal auto-state).
//
// Assertions:
//   - Final state == teacher_review.
//   - graded.v1.json exists in object storage and is valid JSON.
func TestHappyPath(t *testing.T) {
	inf := startInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tok := mintTestJWT(t)
	fakeAI := newFakeAIServer(t)
	aiProv := newFakeAIProvider(fakeAI)

	stopPipeline := startPipeline(ctx, t, inf, pipelineConfig{
		transcribeProvider: aiProv,
		gradeProvider:      aiProv,
		gradeModel:         "grade-model",
	})
	defer stopPipeline()

	// Ensure the test tenant exists in the school table (FK constraint).
	ensureTenant(t, inf, testTenantID)

	apiSrv := newAPIServer(t, inf)
	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err, "read sample PDF")

	subID := postSubmission(t, apiSrv.URL, tok, pdfBytes)
	t.Logf("happy path: submission_id=%s", subID)

	// Poll until teacher_review (or failed so the test fails fast).
	finalState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{
			contracts.StateTeacherReview,
			contracts.StateFailed,
		},
		4*time.Minute,
	)

	assert.Equal(t, contracts.StateTeacherReview, finalState,
		"submission should reach teacher_review after grading")

	// Assert graded.v1.json artifact exists and is parseable.
	gradedKey := fmt.Sprintf("%s/%s/graded.v1.json", testTenantID, subID)
	gradedData, err := inf.objStore.Get(ctx, inf.bucket, gradedKey)
	require.NoError(t, err, "graded.v1.json must exist in object store at %q", gradedKey)

	var gradedPaper contracts.GradedPaper
	require.NoError(t, json.Unmarshal(gradedData, &gradedPaper),
		"graded.v1.json must be a valid GradedPaper")
	assert.NotEmpty(t, gradedPaper.Questions,
		"graded paper must contain at least one question")
	t.Logf("happy path: graded paper — questions=%d score=%.1f/%.1f (%.1f%%)",
		len(gradedPaper.Questions), gradedPaper.Total, gradedPaper.MaxTotal, gradedPaper.Score100)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: Forced Review Gate
// ─────────────────────────────────────────────────────────────────────────────

// TestForcedReviewGate verifies that a transcribe result whose sidecar carries
// a quality flag (checksum mismatch) routes the submission to
// transcription_review_required and does NOT dispatch grading.
func TestForcedReviewGate(t *testing.T) {
	inf := startInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tok := mintTestJWT(t)
	fakeAI := newFakeAIServer(t)
	aiProv := newFakeAIProvider(fakeAI)

	// Track grade worker invocations.
	var gradeCallCount int64

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	// Orchestrator.
	orch := orchestrator.New(inf.pgStore, &busAdapter{inf.bus}, inf.objStore, inf.bucket)
	go func() {
		if err := orch.Start(workerCtx); err != nil && err != context.Canceled {
			t.Logf("orchestrator error: %v", err)
		}
	}()

	// Render (normal).
	renderBus := mustConnectBus(t, inf.amqpURL, 0)
	require.NoError(t, renderBus.Consume(workerCtx, "render.q", func(env contracts.Envelope) error {
		return renderHandler(workerCtx, env, inf.objStore, renderBus, inf.bucket)
	}))

	// Transcribe with forced gate (checksum mismatch).
	transcribeBus := mustConnectBus(t, inf.amqpURL, 0)
	require.NoError(t, transcribeBus.Consume(workerCtx, "transcribe.q", func(env contracts.Envelope) error {
		return transcribeHandler(workerCtx, env, inf.objStore, transcribeBus, aiProv, inf.bucket, true /* forceGate */)
	}))

	// Grade worker — should NOT be called.
	gradeBus := mustConnectBus(t, inf.amqpURL, 0)
	require.NoError(t, gradeBus.Consume(workerCtx, "grade.q", func(env contracts.Envelope) error {
		atomic.AddInt64(&gradeCallCount, 1)
		return nil // ack without grading
	}))

	ensureTenant(t, inf, testTenantID)
	apiSrv := newAPIServer(t, inf)
	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err)

	subID := postSubmission(t, apiSrv.URL, tok, pdfBytes)
	t.Logf("forced gate: submission_id=%s", subID)

	finalState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{
			contracts.StateTranscriptionReviewRequired,
			contracts.StateFailed,
		},
		2*time.Minute,
	)

	assert.Equal(t, contracts.StateTranscriptionReviewRequired, finalState,
		"checksum mismatch must route to transcription_review_required")

	// Brief pause to ensure any erroneous grade dispatch would have been consumed.
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, int64(0), atomic.LoadInt64(&gradeCallCount),
		"grade worker must not be invoked when transcript is flagged")

	t.Logf("forced gate: correctly landed in transcription_review_required, grade_calls=%d",
		atomic.LoadInt64(&gradeCallCount))
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: Forced Technical Failure → DLQ → failed
// ─────────────────────────────────────────────────────────────────────────────

// TestForcedTechnicalFailure makes the transcribe stage fail persistently by
// pointing it at a 500-returning fake server. After MaxAttempts retries (set to
// 2 for speed), the message dead-letters to transcribe.q.dlq. The orchestrator
// consumes transcribe.q.dlq and transitions the submission to "failed" with
// error_detail containing the stage name.
//
// This exercises the broker-native DLQ path (x-delivery-count + x-dead-letter
// routing) without requiring real model failures.
func TestForcedTechnicalFailure(t *testing.T) {
	inf := startInfra(t)

	// Use MaxAttempts=2 for speed: 2 delivery attempts before dead-lettering.
	const maxAttempts = 2

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tok := mintTestJWT(t)

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	// Orchestrator (with MaxAttempts=2 for DLQ trigger alignment).
	orchBus := mustConnectBus(t, inf.amqpURL, maxAttempts)
	orch := orchestrator.New(inf.pgStore, &busAdapter{orchBus}, inf.objStore, inf.bucket)
	go func() {
		if err := orch.Start(workerCtx); err != nil && err != context.Canceled {
			t.Logf("orchestrator error: %v", err)
		}
	}()

	// Render (uses real pdftoppm — doesn't touch AI, so succeeds normally).
	renderBus := mustConnectBus(t, inf.amqpURL, maxAttempts)
	require.NoError(t, renderBus.Consume(workerCtx, "render.q", func(env contracts.Envelope) error {
		return renderHandler(workerCtx, env, inf.objStore, renderBus, inf.bucket)
	}))

	// Transcribe — always returns a hard error to force DLQ after maxAttempts.
	// We simulate a persistent infrastructure failure (e.g. AI gateway unavailable)
	// by returning an explicit error without touching AI at all. This is the most
	// reliable way to exercise the DLQ path: the broker sees repeated nacks and
	// routes to transcribe.q.dlq after maxAttempts deliveries.
	transcribeBus := mustConnectBus(t, inf.amqpURL, maxAttempts)
	require.NoError(t, transcribeBus.Consume(workerCtx, "transcribe.q", func(env contracts.Envelope) error {
		return fmt.Errorf("transcribe[dlq-test]: simulated persistent AI gateway failure for submission %s", env.SubmissionID)
	}))

	// Grade — should never be reached.
	gradeBus := mustConnectBus(t, inf.amqpURL, maxAttempts)
	require.NoError(t, gradeBus.Consume(workerCtx, "grade.q", func(env contracts.Envelope) error {
		t.Errorf("grade worker must not be dispatched after transcribe failure")
		return nil
	}))

	ensureTenant(t, inf, testTenantID)
	apiSrv := newAPIServer(t, inf)
	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err)

	subID := postSubmission(t, apiSrv.URL, tok, pdfBytes)
	t.Logf("forced failure: submission_id=%s", subID)

	// Poll until failed.
	finalState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateFailed},
		2*time.Minute,
	)

	assert.Equal(t, contracts.StateFailed, finalState,
		"submission must reach failed after transcribe DLQ")

	// Assert error_detail is set and mentions transcribe.
	sub, err := inf.pgStore.GetSubmission(ctx, subID)
	require.NoError(t, err)
	require.NotNil(t, sub.ErrorDetail,
		"error_detail must be set on failed submission")
	assert.True(t, strings.Contains(*sub.ErrorDetail, "transcribe"),
		"error_detail must mention the transcribe stage, got: %q", *sub.ErrorDetail)

	t.Logf("forced failure: submission %s -> failed, error_detail=%q", subID, *sub.ErrorDetail)
}
