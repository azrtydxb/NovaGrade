package integration

// integrations_test.go — Phase-4 hermetic integration test.
//
// TestIntegrationsFlow proves the full Phase-4 feature set end-to-end on real
// infrastructure (Postgres, RabbitMQ, MinIO via testcontainers):
//
//  1. startInfra + SKIP_DOCKER_TESTS gate (inherited from pipeline_test.go).
//  2. Stand up all Phase-2/4 API handlers in-process using the same pattern as
//     teacher_review_test.go and marking_guide_test.go.
//  3. Integration connection config (csv/roster) with encrypted credentials.
//  4. Roster import via POST /v1/rosters/import — idempotent re-import.
//  5. Webhook subscription via POST /v1/webhooks pointing at an httptest receiver.
//  6. Drive a submission to published (reusing Phase-2 pipeline + handlers).
//  7. Assert the webhook fired: poll the httptest receiver (no fixed sleep),
//     verify HMAC-SHA256 signature, assert body type=="published".
//  8. Class-results CSV export via GET /v1/assessment-versions/{avid}/results.csv.
//
// All hermetic: no external network except the in-test httptest webhook receiver.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chi_middleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/api"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/integration"
	integrationcsv "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/internal/integration/oneroster"
	"github.com/azrtydxb/novagrade/internal/integration/webhook"
	"github.com/azrtydxb/novagrade/internal/secrets"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Phase-4 in-process API server
// ─────────────────────────────────────────────────────────────────────────────

// phase4APIServer bundles the server URL and enc key for callers.
type phase4APIServer struct {
	url    string
	encKey []byte
}

// newPhase4APIServer builds an httptest.Server wiring all Phase-2 + Phase-4
// handlers in-process. Routes mirror cmd/api/main.go exactly.
//
// The 32-byte encKey is passed by the caller (generated in-test from crypto/rand)
// and set via t.Setenv("INTEGRATION_ENC_KEY", ...) so that IntegrationHandlers
// can call secrets.KeyFromEnv during its own handler path.
func newPhase4APIServer(t *testing.T, inf *testInfra, encKey []byte) *phase4APIServer {
	t.Helper()
	if os.Getenv("JWT_SIGNING_KEY") == "" {
		t.Setenv("JWT_SIGNING_KEY", testSigningKey)
	}

	// Set the enc key in the environment so the integration handler can read it.
	b64Key := base64.StdEncoding.EncodeToString(encKey)
	t.Setenv("INTEGRATION_ENC_KEY", b64Key)

	objAdapter := &objStoreAdapter{s: inf.objStore, bucket: inf.bucket}

	// Phase-1 handlers.
	h := &api.Handlers{
		Store:      inf.pgStore,
		Bus:        inf.bus,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}

	// Phase-2 review + export handlers.
	rh := &api.ReviewHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}
	eh := &api.ExportHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}

	// Phase-4 webhook handler (needs encKey directly).
	wh := &api.WebhookHandlers{
		Store:      inf.pgStore,
		EncKey:     encKey,
		DeployMode: "onprem",
	}

	// Phase-2 approval handler wired with webhook dispatch.
	webhookSender := webhook.NewSender(10*time.Second, 3)
	ah := &api.ApprovalHandlers{
		Store:         inf.pgStore,
		Objects:       objAdapter,
		Bus:           inf.bus,
		DeployMode:    "onprem",
		WebhookSender: webhookSender,
		WebhookStore:  inf.pgStore,
		WebhookKey:    encKey,
	}

	// Phase-4 integration registry (csv + oneroster built-ins).
	reg := integration.NewRegistry()
	integrationcsv.Register(reg)
	oneroster.Register(reg)

	ih := &api.IntegrationHandlers{
		Store:      inf.pgStore,
		Registry:   reg,
		DeployMode: "onprem",
	}

	roh := &api.RosterHandlers{
		Store:      inf.pgStore,
		Registry:   reg,
		DeployMode: "onprem",
	}

	crh := &api.ClassResultsHandlers{
		Store:      inf.pgStore,
		Objects:    objAdapter,
		DeployMode: "onprem",
	}

	r := chi.NewRouter()
	r.Use(chi_middleware.Recoverer)
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(auth.NewAPIKeyResolver()))
		// Phase-1: submission ingestion.
		r.Post("/submissions", h.PostSubmission)
		r.Get("/submissions/{id}", h.GetSubmission)
		// Phase-2: review + approval.
		r.Get("/submissions/{id}/review", rh.GetReview)
		r.Patch("/submissions/{id}/questions/{qno}", rh.PatchQuestion)
		r.Post("/submissions/{id}/approve", ah.Approve)
		r.Post("/submissions/{id}/publish", ah.Publish)
		r.Post("/submissions/{id}/export", ah.Export)
		r.Get("/submissions/{id}/export.csv", eh.ExportCSV)
		// Phase-4: integration connections.
		r.Post("/integrations", ih.UpsertIntegration)
		r.Get("/integrations", ih.ListIntegrations)
		r.Delete("/integrations/{id}", ih.DeleteIntegration)
		// Phase-4: webhook subscriptions.
		r.Post("/webhooks", wh.Create)
		r.Get("/webhooks", wh.List)
		r.Delete("/webhooks/{id}", wh.Delete)
		// Phase-4: roster import.
		r.Post("/rosters/import", roh.ImportRoster)
		// Phase-4: class-results CSV.
		r.Get("/assessment-versions/{avid}/results.csv", crh.ClassResultsCSV)
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &phase4APIServer{url: srv.URL, encKey: encKey}
}

// ─────────────────────────────────────────────────────────────────────────────
// Webhook receiver — captures the incoming request for assertion
// ─────────────────────────────────────────────────────────────────────────────

// webhookCapture records the body and signature of a single received POST.
type webhookCapture struct {
	mu   sync.Mutex
	body []byte
	sig  string
	got  bool
}

// received reports whether a POST has been captured.
func (w *webhookCapture) received() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.got
}

// snapshot returns a copy of (body, sig) after received() == true.
func (w *webhookCapture) snapshot() ([]byte, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body, w.sig
}

// newWebhookReceiver stands up an httptest.Server that records the first
// inbound POST and returns 200 OK. Subsequent POSTs are also accepted (idempotent).
func newWebhookReceiver(t *testing.T) (*httptest.Server, *webhookCapture) {
	t.Helper()
	whCap := &webhookCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig := r.Header.Get("X-NovaGrade-Signature")
		whCap.mu.Lock()
		whCap.body = body
		whCap.sig = sig
		whCap.got = true
		whCap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, whCap
}

// pollWebhookReceiver polls cap.received() with exponential backoff until
// the receiver has been called or timeout elapses.
func pollWebhookReceiver(t *testing.T, cap *webhookCapture, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	backoff := 100 * time.Millisecond
	for time.Now().Before(deadline) {
		if cap.received() {
			return
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
	t.Fatalf("timeout: webhook receiver did not receive a POST within %s", timeout)
}

// ─────────────────────────────────────────────────────────────────────────────
// HMAC verification helper
// ─────────────────────────────────────────────────────────────────────────────

// verifyWebhookHMAC validates that the X-NovaGrade-Signature header matches
// HMAC-SHA256(body, secret). sigHeader is the raw header value ("sha256=<hex>").
// secret is the base64-decoded plaintext secret returned by POST /v1/webhooks.
func verifyWebhookHMAC(t *testing.T, body []byte, sigHeader string, secret []byte) {
	t.Helper()
	const prefix = "sha256="
	require.True(t, strings.HasPrefix(sigHeader, prefix),
		"X-NovaGrade-Signature must start with %q, got %q", prefix, sigHeader)
	gotHex := strings.TrimPrefix(sigHeader, prefix)

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	wantHex := hex.EncodeToString(mac.Sum(nil))

	assert.Equal(t, wantHex, gotHex, "webhook HMAC-SHA256 signature mismatch")
}

// ─────────────────────────────────────────────────────────────────────────────
// Phase-4 HTTP helpers
// ─────────────────────────────────────────────────────────────────────────────

// postIntegration POSTs a new integration connection and returns the created
// connection (without credentials).
func postIntegration(t *testing.T, apiURL, adminJWT, category, provider string, credentials json.RawMessage) integration.Connection {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"category":    category,
		"provider":    provider,
		"credentials": credentials,
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, apiURL+"/v1/integrations", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminJWT)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode,
		"POST /v1/integrations expected 201, got %d: %s", resp.StatusCode, body)

	var conn integration.Connection
	require.NoError(t, json.Unmarshal(body, &conn), "parse integration response: %s", body)
	return conn
}

// createWebhookSub creates a webhook subscription and returns (id, plaintextSecret).
func createWebhookSub(t *testing.T, apiURL, adminJWT, event, receiverURL string) (uuid.UUID, []byte) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"event": event,
		"url":   receiverURL,
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, apiURL+"/v1/webhooks", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminJWT)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode,
		"POST /v1/webhooks expected 201, got %d: %s", resp.StatusCode, body)

	var result struct {
		ID     uuid.UUID `json:"id"`
		Secret string    `json:"secret"` // base64-encoded plaintext
	}
	require.NoError(t, json.Unmarshal(body, &result), "parse webhook response: %s", body)
	require.NotEqual(t, uuid.Nil, result.ID, "webhook id must be non-zero")
	require.NotEmpty(t, result.Secret, "webhook secret must be returned once on create")

	plainSecret, err := base64.StdEncoding.DecodeString(result.Secret)
	require.NoError(t, err, "decode webhook secret from base64")
	require.Len(t, plainSecret, 32, "webhook secret must be 32 bytes")

	return result.ID, plainSecret
}

// importRosterCSV sends a multipart POST to /v1/rosters/import with the given
// CSV bytes and returns the parsed response.
func importRosterCSV(t *testing.T, apiURL, adminJWT string, csvData []byte) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "roster.csv")
	require.NoError(t, err)
	_, err = fw.Write(csvData)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, apiURL+"/v1/rosters/import?provider=csv", &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+adminJWT)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"POST /v1/rosters/import expected 200, got %d: %s", resp.StatusCode, body)

	var result map[string]any
	require.NoError(t, json.Unmarshal(body, &result), "parse roster import response: %s", body)
	return result
}

// getClassResultsCSV issues GET /v1/assessment-versions/{avid}/results.csv and
// returns the parsed CSV records.
func getClassResultsCSV(t *testing.T, apiURL, adminJWT string, avid uuid.UUID) [][]string {
	t.Helper()
	url := fmt.Sprintf("%s/v1/assessment-versions/%s/results.csv", apiURL, avid)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+adminJWT)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /results.csv expected 200, got %d: %s", resp.StatusCode, body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/csv")

	records, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	require.NoError(t, err, "parse CSV body: %s", body)
	return records
}

// ─────────────────────────────────────────────────────────────────────────────
// TestIntegrationsFlow — Phase-4 end-to-end hermetic integration test
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegrationsFlow proves the Phase-4 integration feature set end-to-end:
//
//  1. startInfra (inherited SKIP_DOCKER_TESTS gate).
//  2. Generate a random 32-byte enc key; expose as INTEGRATION_ENC_KEY env var.
//  3. Stand up all Phase-2/4 handlers in-process via newPhase4APIServer.
//  4. Create an integration connection (csv/roster) with encrypted credentials.
//  5. Import a CSV roster (2 students), assert count; idempotent re-import.
//  6. Subscribe a webhook to "published" pointing at an httptest receiver.
//  7. Drive a submission to teacher_review → approve → publish via real handlers.
//  8. Poll the webhook receiver (no fixed sleep); verify HMAC signature.
//  9. GET class-results CSV and assert it contains graded marks rows.
func TestIntegrationsFlow(t *testing.T) {
	inf := startInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	// ── Step 1: Generate enc key (32 bytes) ──────────────────────────────────
	var encKeyRaw [32]byte
	_, err := rand.Read(encKeyRaw[:])
	require.NoError(t, err, "generate enc key")
	encKey := encKeyRaw[:]

	// ── Step 2: Stand up full Phase-2/4 API server ───────────────────────────
	srv := newPhase4APIServer(t, inf, encKey)
	apiURL := srv.url

	// Admin JWT (school_admin has ActionEditTunables + ActionViewResults).
	adminJWT := mintAdminJWT(t)
	// Teacher JWT used for submission + review/approve/publish.
	teacherJWT := mintTestJWT(t)

	ensureTenant(t, inf, testTenantID)
	avid := ensureAssessmentAndVersion(t, inf, testTenantID)
	t.Logf("integrations: assessment_version_id=%s", avid)

	// ── Step 3: Integration config — POST /v1/integrations ───────────────────
	// Store a CSV roster connection with fake credentials; assert 201 + encrypted.
	fakeCreds := json.RawMessage(`{"path":"/data/roster.csv","delimiter":","}`)
	conn := postIntegration(t, apiURL, adminJWT, "roster", "csv", fakeCreds)
	require.NotEqual(t, uuid.Nil, conn.ID, "integration connection must have a non-nil ID")
	assert.Equal(t, "roster", string(conn.Category))
	assert.Equal(t, "csv", conn.Provider)
	t.Logf("integrations: connection id=%s category=%s provider=%s", conn.ID, conn.Category, conn.Provider)

	// Assert the raw column in the DB is ENCRYPTED (not equal to fakeCreds).
	_, rawEncBytes, err := inf.pgStore.GetConnectionWithCreds(ctx, testTenantID, conn.ID)
	require.NoError(t, err, "GetConnectionWithCreds must succeed for the new connection")
	// The stored bytes are the AES-GCM ciphertext (nonce||ct); they are NOT the plaintext.
	assert.NotEqual(t, []byte(fakeCreds), rawEncBytes,
		"stored credentials must not be plaintext")
	// But decrypting must recover the original JSON.
	decrypted, err := secrets.Decrypt(encKey, rawEncBytes)
	require.NoError(t, err, "decrypt stored credentials")
	assert.Equal(t, []byte(fakeCreds), decrypted,
		"decrypted credentials must match the original")
	t.Logf("integrations: credentials encrypted at rest (raw %d bytes → plaintext %d bytes)", len(rawEncBytes), len(decrypted))

	// ── Step 4: Roster import — POST /v1/rosters/import ──────────────────────
	const rosterCSV = "email,full_name,class\nalice@example.com,Alice Smith,9A\nbob@example.com,Bob Jones,9B\n"
	importResult := importRosterCSV(t, apiURL, adminJWT, []byte(rosterCSV))
	t.Logf("integrations: roster import result: %v", importResult)

	imported, ok := importResult["imported"].(float64)
	require.True(t, ok, "imported field must be a number")
	assert.Equal(t, float64(2), imported, "should import 2 students")

	// Idempotent re-import: same roster again must not error.
	reimportResult := importRosterCSV(t, apiURL, adminJWT, []byte(rosterCSV))
	t.Logf("integrations: idempotent re-import result: %v", reimportResult)
	imported2, _ := reimportResult["imported"].(float64)
	assert.Equal(t, float64(2), imported2, "idempotent re-import must still report 2")

	// Assert at the DB level: student table must contain exactly 2 students for this tenant.
	// Query directly to prove idempotency is enforced at the database level, not just the API.
	dbConn, err := pgxConnect(ctx, inf.dbCfg)
	require.NoError(t, err, "open pgx connection to verify DB state")
	defer func() { _ = dbConn.Close(ctx) }()

	var studentCount int
	err = dbConn.QueryRow(ctx, "SELECT count(*) FROM student WHERE tenant_id=$1", testTenantID).Scan(&studentCount)
	require.NoError(t, err, "count students in DB for tenant")
	assert.Equal(t, 2, studentCount, "DB must contain exactly 2 students after idempotent re-import (not 4)")

	// ── Step 5: Webhook subscription ─────────────────────────────────────────
	// Stand up the httptest receiver FIRST, then subscribe.
	receiverSrv, cap := newWebhookReceiver(t)
	whID, plainSecret := createWebhookSub(t, apiURL, adminJWT, "published", receiverSrv.URL)
	t.Logf("integrations: webhook subscription id=%s receiver=%s", whID, receiverSrv.URL)

	// ── Step 6: Drive submission to teacher_review ───────────────────────────
	fakeAI := newFakeAIServer(t)
	aiProv := newFakeAIProvider(fakeAI)

	stopPipeline := startPipeline(ctx, t, inf, pipelineConfig{
		transcribeProvider: aiProv,
		gradeProvider:      aiProv,
		gradeModel:         "grade-model",
	})
	defer stopPipeline()

	pdfBytes, err := os.ReadFile(samplePDFPath(t))
	require.NoError(t, err, "read sample PDF")

	// Create submission, patch its assessment_version_id, wait for teacher_review.
	subID := postSubmissionWithAVID(t, inf, testTenantID, avid, apiURL, teacherJWT, pdfBytes)
	t.Logf("integrations: submission_id=%s", subID)

	finalState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateTeacherReview, contracts.StateFailed},
		4*time.Minute,
	)
	require.Equal(t, contracts.StateTeacherReview, finalState,
		"submission must reach teacher_review before approval actions")

	// ── Step 7: Approve + Publish via real handlers ───────────────────────────
	approveResp := doPost(t, apiURL, adminJWT, fmt.Sprintf("/v1/submissions/%s/approve", subID))
	require.Equal(t, http.StatusOK, approveResp.status,
		"POST /approve expected 200, got %d: %s", approveResp.status, approveResp.body)

	approvedState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StateApproved, contracts.StateFailed},
		90*time.Second,
	)
	require.Equal(t, contracts.StateApproved, approvedState,
		"submission must advance to approved")

	publishResp := doPost(t, apiURL, adminJWT, fmt.Sprintf("/v1/submissions/%s/publish", subID))
	require.Equal(t, http.StatusOK, publishResp.status,
		"POST /publish expected 200, got %d: %s", publishResp.status, publishResp.body)

	publishedState := pollSubmission(ctx, t, inf.pgStore, subID,
		[]contracts.SubmissionState{contracts.StatePublished, contracts.StateFailed},
		90*time.Second,
	)
	require.Equal(t, contracts.StatePublished, publishedState,
		"submission must advance to published (triggers webhook dispatch)")

	// ── Step 8: Assert webhook fired + HMAC verified ─────────────────────────
	// The webhook dispatch fires asynchronously in a goroutine inside ah.Publish.
	// Poll until the httptest receiver captures the POST (no fixed sleep).
	pollWebhookReceiver(t, cap, 30*time.Second)

	capturedBody, capturedSig := cap.snapshot()
	require.NotEmpty(t, capturedBody, "webhook body must not be empty")
	require.NotEmpty(t, capturedSig, "X-NovaGrade-Signature must be present")
	t.Logf("integrations: webhook body: %s", capturedBody)
	t.Logf("integrations: webhook sig: %s", capturedSig)

	// Verify HMAC-SHA256 signature.
	verifyWebhookHMAC(t, capturedBody, capturedSig, plainSecret)

	// Decode the event body and assert type == "published" + submission id.
	var whEvent map[string]any
	require.NoError(t, json.Unmarshal(capturedBody, &whEvent), "parse webhook event body")
	assert.Equal(t, "published", whEvent["type"], "webhook event type must be 'published'")
	assert.Equal(t, subID.String(), whEvent["submission_id"],
		"webhook event submission_id must match the published submission")
	assert.Equal(t, testTenantID.String(), whEvent["tenant_id"],
		"webhook event tenant_id must match")
	t.Logf("integrations: webhook fired — type=%s submission_id=%s HMAC OK", whEvent["type"], whEvent["submission_id"])

	// ── Step 9: Class-results CSV ─────────────────────────────────────────────
	rows := getClassResultsCSV(t, apiURL, adminJWT, avid)
	require.NotEmpty(t, rows, "class-results CSV must contain at least a header row")
	t.Logf("integrations: class-results CSV rows: %d", len(rows))

	// Find the header and locate column indices.
	header := rows[0]
	colIdx := func(name string) int {
		for i, c := range header {
			if c == name {
				return i
			}
		}
		t.Fatalf("class-results CSV header missing column %q (header=%v)", name, header)
		return -1
	}
	_ = colIdx("question_no") // assert column exists
	awardedMarksIdx := colIdx("awarded")

	// At least one data row must exist (the graded submission).
	require.Greater(t, len(rows), 1, "class-results CSV must have at least one data row (the graded submission)")

	// Assert that at least one data row has a non-empty awarded cell
	// and that it parses as a float >= 0 (proof of actual mark values, not blank export).
	var foundNonEmptyMark bool
	for i := 1; i < len(rows); i++ {
		if awardedMarksIdx < len(rows[i]) && rows[i][awardedMarksIdx] != "" {
			markStr := rows[i][awardedMarksIdx]
			var mark float64
			_, err := fmt.Sscanf(markStr, "%f", &mark)
			if err == nil && mark >= 0 {
				foundNonEmptyMark = true
				break
			}
		}
	}
	assert.True(t, foundNonEmptyMark, "class-results CSV must contain at least one row with a non-empty, parseable float awarded >= 0")

	t.Logf("integrations: PASSED — enc creds verified, %d students imported, webhook HMAC OK, CSV %d rows",
		int(imported), len(rows))
}
