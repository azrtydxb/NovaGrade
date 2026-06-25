# NovaGrade Implementation Plan (all phases)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the NovaGrade POC into a production AI-assisted assessment platform — a Go,
queue-driven, multi-tenant system that ingests scanned exams, transcribes, grades against
official marking guides, routes low-confidence work to teachers for review/approval, and exports
results — keeping the POC pipeline as the technical core.

**Architecture:** Go monorepo of small, pure stage-services (render → transcribe → grade →
feedback) coordinated by an orchestrator that owns a per-submission state machine, connected by
RabbitMQ (work queues + DLQs) with Postgres (state/domain) and S3/MinIO (artifacts). All model
calls go through an ai-gateway provider abstraction. RBAC + tenant isolation enforced at the edge
from the first commit.

**Tech Stack:** Go 1.23+ monorepo · chi (HTTP) · pgx + sqlc (DB) · goose (migrations) ·
amqp091-go (RabbitMQ) · minio-go (object store) · golang-jwt (auth) · testify +
testcontainers-go (tests) · poppler/imagemagick (render, shelled out) · docker-compose (on-prem) /
Helm (cloud).

## Global Constraints

- **Language/runtime:** Go ≥ 1.23, single monorepo module `github.com/azrtydxb/novagrade`.
- **Self-hostable only:** every infra dependency runs as one container on-prem AND scales in cloud
  — Postgres, RabbitMQ, MinIO. No managed-cloud-only services.
- **Multi-tenancy is mandatory:** `tenant_id` (School) on every table row and every message
  envelope; tenant isolation enforced **below** RBAC (a valid principal cannot reach another
  tenant's rows). Operator cross-tenant access only when `DEPLOY_MODE=saas`.
- **No grade is final until a teacher approves it.** Marking guides are the default grading path;
  LLM-as-judge is assistive only.
- **No hardcoded secrets / API keys.** All secrets via env/secret manager. Secure file-upload
  validation on every upload path.
- **Every AI call logged** with model, prompt version, cost, token usage, and output schema.
- **TDD throughout:** write the failing test, see it fail, implement minimally, see it pass,
  commit. Frequent commits. Provider/model HTTP calls are mocked in unit tests.
- **POC parity:** the three sample papers must grade through the Go pipeline and match the POC's
  `out/*.results.json` within tolerance (see Task 14).

## Plan structure & tiering

This is the **master** plan. Per the agreed tiering:
- **Phase 1 (Tasks 1–14)** — the production-foundation **spine**, written **step-level** with real
  code: scaffold → infra → contracts → DB → queue → ai-gateway → render → transcribe → grade →
  orchestrator/state-machine → api+RBAC → audit → integration → POC parity. **Build this first.**
- **Phase 1b (Tasks 15–20)** — remaining Phase-1 breadth (batch/split, student mapping, full
  school CRUD, dashboard), written **task-level**; each expands into step-level before its build.
- **Phases 2–6 (§ Later phases)** — **task-level** outlines (files, interfaces, deliverables,
  acceptance). Each becomes its own step-level plan when reached.

Distant-phase tasks are intentionally task-level (not step-level) because their concrete code
depends on decisions that do not yet exist (wireframes, specific LMS/SIS API flows, provider
internals). Expanding them now would mean fictional code — forbidden by the method.

## File structure (Phase 1 spine)

```
go.mod                                  # module github.com/azrtydxb/novagrade
Makefile                                # build, test, lint, infra-up/down
deploy/compose/docker-compose.yml       # postgres, rabbitmq, minio
deploy/compose/.env.example
pkg/contracts/                          # cross-service types + message envelopes
  schemas.go                            # Transcription, TranscribedQuestion, GradedPaper, ...
  envelope.go                           # Message envelope, command/result types
  states.go                             # SubmissionState enum + valid transitions
internal/store/
  migrations/                           # goose .sql files
  queries/                              # sqlc .sql sources
  store.go                              # pgxpool wiring
  objstore.go                           # minio-go wrapper
internal/queue/
  rabbit.go                             # connect, declare topology, publish, consume, DLQ
internal/providers/
  provider.go                           # AIProvider interface + call logging
  vllm.go                               # OpenAI-compatible vLLM provider (ported llm_client.py)
internal/pipeline/
  render.go                             # PDF -> page images (poppler shell-out)
  transcribe.go                         # pages -> Transcription (hybrid read)
  grade/                                # marking-guide engine + MarkScheme
    markscheme.go  llmjudge.go  guide.go
internal/domain/
  statemachine.go                       # submission state machine + gate rules
  rbac.go                               # role/permission matrix + tenant isolation
internal/auth/
  jwt.go  apikey.go  middleware.go
cmd/ai-gateway/main.go
cmd/render/main.go  cmd/transcribe/main.go  cmd/grade/main.go
cmd/orchestrator/main.go  cmd/api/main.go  cmd/audit/main.go
```

---

## Phase 1 — production-foundation spine (step-level)

### Task 1: Scaffold the Go monorepo

**Files:**
- Create: `go.mod`, `Makefile`, `internal/version/version.go`, `internal/version/version_test.go`, `.gitignore`

**Interfaces:**
- Produces: module path `github.com/azrtydxb/novagrade`; `version.Version() string`.

- [ ] **Step 1: Write the failing test**
```go
// internal/version/version_test.go
package version

import "testing"

func TestVersionNotEmpty(t *testing.T) {
	if Version() == "" {
		t.Fatal("Version() returned empty string")
	}
}
```

- [ ] **Step 2: Init module and run the test (expect FAIL)**
```bash
go mod init github.com/azrtydxb/novagrade
go test ./internal/version/...   # FAIL: undefined: Version
```

- [ ] **Step 3: Minimal implementation**
```go
// internal/version/version.go
package version

func Version() string { return "0.1.0-dev" }
```

- [ ] **Step 4: Makefile + .gitignore**
```makefile
# Makefile
.PHONY: build test lint infra-up infra-down
build: ; go build ./...
test: ; go test ./...
lint: ; go vet ./...
infra-up: ; docker compose -f deploy/compose/docker-compose.yml up -d
infra-down: ; docker compose -f deploy/compose/docker-compose.yml down
```
```gitignore
/bin/
*.env
.env
```

- [ ] **Step 5: Verify and commit**
```bash
go build ./... && go test ./...   # PASS
git add go.mod Makefile .gitignore internal/version
git commit -m "chore: scaffold go monorepo + version package"
```

---

### Task 2: Local infrastructure via docker-compose

**Files:**
- Create: `deploy/compose/docker-compose.yml`, `deploy/compose/.env.example`, `internal/store/objstore_test.go` (smoke), `internal/store/objstore.go`

**Interfaces:**
- Produces: running Postgres (`:5432`), RabbitMQ (`:5672` + mgmt `:15672`), MinIO (`:9000`).
  `objstore.New(cfg) (*ObjStore, error)`; `(*ObjStore).EnsureBucket(ctx, name) error`.

- [ ] **Step 1: Compose file**
```yaml
# deploy/compose/docker-compose.yml
services:
  postgres:
    image: postgres:16
    environment: { POSTGRES_USER: nova, POSTGRES_PASSWORD: nova, POSTGRES_DB: novagrade }
    ports: ["5432:5432"]
    healthcheck: { test: ["CMD-SHELL","pg_isready -U nova"], interval: 5s, retries: 10 }
  rabbitmq:
    image: rabbitmq:3.13-management
    ports: ["5672:5672","15672:15672"]
    healthcheck: { test: ["CMD","rabbitmq-diagnostics","ping"], interval: 5s, retries: 10 }
  minio:
    image: minio/minio
    command: server /data --console-address ":9001"
    environment: { MINIO_ROOT_USER: nova, MINIO_ROOT_PASSWORD: nova12345 }
    ports: ["9000:9000","9001:9001"]
    healthcheck: { test: ["CMD","mc","ready","local"], interval: 5s, retries: 10 }
```

- [ ] **Step 2: Write the failing object-store smoke test** (uses testcontainers MinIO)
```go
// internal/store/objstore_test.go
package store
import ("context";"testing";"github.com/stretchr/testify/require")
func TestObjStoreEnsureBucket(t *testing.T){
  s,err:=New(testMinioCfg(t)); require.NoError(t,err)
  require.NoError(t, s.EnsureBucket(context.Background(),"exams"))
}
```
(`testMinioCfg` spins a `minio/minio` testcontainer; helper lives in `objstore_test.go`.)

- [ ] **Step 3: Run (expect FAIL: New undefined)** — `go test ./internal/store/...`

- [ ] **Step 4: Implement `objstore.go`** with minio-go (`New`, `EnsureBucket`, `Put`, `Get`).

- [ ] **Step 5: Verify and commit**
```bash
go test ./internal/store/... && git add deploy/compose internal/store
git commit -m "feat: local compose infra + minio object store wrapper"
```

---

### Task 3: Shared contracts (schemas, envelope, states)

**Files:**
- Create: `pkg/contracts/schemas.go`, `pkg/contracts/envelope.go`, `pkg/contracts/states.go`, `pkg/contracts/states_test.go`

**Interfaces:**
- Produces: `TranscribedQuestion`, `TranscribedPaper`, `GradedQuestion`, `GradedPaper` (ported
  1:1 from POC `schemas.py`); `Envelope{TenantID, Principal, SubmissionID, BatchID, Stage,
  Attempt, CorrelationID, PayloadRef string}`; `SubmissionState string` enum;
  `CanTransition(from, to SubmissionState) bool`.

- [ ] **Step 1: Write the failing transition test**
```go
// pkg/contracts/states_test.go
package contracts
import "testing"
func TestValidTransition(t *testing.T){
  if !CanTransition(StateTranscribing, StateGradingReviewRequired) && !CanTransition(StateTranscribing, StateGrading) {
    t.Fatal("transcribing must be able to advance")
  }
  if CanTransition(StateApproved, StateUploaded) { t.Fatal("approved must not regress to uploaded") }
}
```

- [ ] **Step 2: Run (FAIL)** — `go test ./pkg/contracts/...`

- [ ] **Step 3: Implement `states.go`** — the §3 state constants + a `var transitions map[SubmissionState][]SubmissionState` + `CanTransition`. Implement `schemas.go` (port POC structs with json tags) and `envelope.go`.

- [ ] **Step 4: Round-trip test for schemas** (marshal/unmarshal `GradedPaper`), run, PASS.

- [ ] **Step 5: Commit**
```bash
git add pkg/contracts && git commit -m "feat: shared contracts (schemas, envelope, state machine)"
```

---

### Task 4: Database schema + repositories

**Files:**
- Create: `internal/store/migrations/0001_core.sql`, `internal/store/queries/*.sql`, `sqlc.yaml`, `internal/store/store.go`, `internal/store/store_test.go`

**Interfaces:**
- Produces: tables for the §7 data model (Phase-1 subset live, rest as footings): `school`,
  `principal`, `role`, `api_key`, `assessment`, `assessment_version`, `question`, `marking_guide`,
  `rubric`, `submission` (+ state cols), `page_image`, `transcription`, `ai_grading_result`,
  `teacher_review`, `final_grade`, `feedback`, `audit_event`, `tunables`. Repo:
  `Store.CreateSubmission`, `Store.SetSubmissionState`, `Store.GetSubmission`,
  `Store.InsertAuditEvent`.

- [ ] **Step 1: Write migration `0001_core.sql`** (goose up/down). Every table has
  `tenant_id uuid not null` and FK to `school(id)`. `submission` carries `state text not null`,
  `current_stage text`, `attempt int`, `error_detail text`, artifact ref columns.
  `audit_event` is append-only (no update/delete grants).

- [ ] **Step 2: Write the failing repo test** (testcontainers Postgres, run goose up)
```go
func TestSetSubmissionStateAudited(t *testing.T){
  st := newTestStore(t)            // spins PG container, runs migrations
  id := mustCreateSubmission(t, st)
  require.NoError(t, st.SetSubmissionState(ctx, id, contracts.StateTranscribing))
  got,_ := st.GetSubmission(ctx, id)
  require.Equal(t, contracts.StateTranscribing, got.State)
}
```

- [ ] **Step 3: Run (FAIL)** — `go test ./internal/store/...`

- [ ] **Step 4: Author sqlc queries + `sqlc generate`; implement `store.go`** wrapping pgxpool + generated code.

- [ ] **Step 5: Verify and commit**
```bash
sqlc generate && go test ./internal/store/...
git add internal/store sqlc.yaml && git commit -m "feat: core schema + repositories (goose+sqlc)"
```

---

### Task 5: RabbitMQ queue layer (topology, publish, consume, DLQ)

**Files:**
- Create: `internal/queue/rabbit.go`, `internal/queue/rabbit_test.go`

**Interfaces:**
- Produces: `Bus.Publish(ctx, queue string, env contracts.Envelope) error`;
  `Bus.Consume(ctx, queue string, handler func(contracts.Envelope) error)` with manual ack;
  topology declaring `render.q/transcribe.q/grade.q/feedback.q/commands.q/results.q` each bound to
  a per-queue DLQ (`<q>.dlq`) via `x-dead-letter-exchange`; redelivery up to `maxAttempts` then
  the message lands in the DLQ.

- [ ] **Step 1: Write the failing round-trip test** (testcontainers RabbitMQ): publish an envelope to `render.q`, consume it, assert fields match.

- [ ] **Step 2: Run (FAIL)**.

- [ ] **Step 3: Implement `rabbit.go`** — connect, `declareTopology()` (quorum queues + DLQ args), `Publish` (JSON body), `Consume` (manual ack; on handler error, nack→DLX after attempt count).

- [ ] **Step 4: Add a DLQ test** — a handler that always errors; assert the message ends in `render.q.dlq` after `maxAttempts`.

- [ ] **Step 5: Verify and commit**
```bash
go test ./internal/queue/... && git add internal/queue
git commit -m "feat: rabbitmq bus with per-queue DLQ + manual-ack retry"
```

---

### Task 6: AI gateway — provider abstraction + vLLM provider

**Files:**
- Create: `internal/providers/provider.go`, `internal/providers/vllm.go`, `internal/providers/vllm_test.go`, `cmd/ai-gateway/main.go`

**Interfaces:**
- Produces: `type AIProvider interface { Complete(ctx, req CompletionReq) (CompletionResp, error) }`;
  `CompletionReq{Model, PromptVersion string; Messages []Message; Images [][]byte; Schema json.RawMessage}`;
  `CompletionResp{Content string; Tokens TokenUsage; CostUSD float64}`; every call emits an
  `AICallLog{Model, PromptVersion, Tokens, CostUSD, SchemaValid bool}` (ported retry + JSON
  extraction from POC `llm_client.py`); schema validation rejects/re-asks on invalid output.

- [ ] **Step 1: Write the failing test** with a mocked HTTP server returning an OpenAI-compatible body; assert `Complete` returns parsed content + token usage and logs the call.

- [ ] **Step 2: Run (FAIL)**.

- [ ] **Step 3: Implement `vllm.go`** (httptest-driven): POST to `/v1/chat/completions`, retry with backoff, extract JSON, validate against `Schema` (using `santhosh-tekuri/jsonschema`), populate `CostUSD` from a per-model price table.

- [ ] **Step 4: Add a schema-invalid test** — provider returns malformed JSON; assert one re-ask then error with `SchemaValid=false` logged.

- [ ] **Step 5: ai-gateway service** — `cmd/ai-gateway/main.go` exposes the provider over HTTP/queue for stage services; provider selected per-tenant from `tunables`. Commit.
```bash
git add internal/providers cmd/ai-gateway && git commit -m "feat: ai-gateway provider abstraction + vLLM provider"
```

---

### Task 7: Render stage service

**Files:**
- Create: `internal/pipeline/render.go`, `internal/pipeline/render_test.go`, `cmd/render/main.go`, `testdata/sample1.pdf`

**Interfaces:**
- Consumes: `Envelope` from `render.q` (PayloadRef → `source.pdf` in object store).
- Produces: page PNGs to `{tenant}/{submission}/pages/{n}.png`; result event to `results.q` with
  page count; `pipeline.RenderPDF(ctx, pdfPath, outDir) ([]string, error)` (shells `pdftoppm`,
  drops near-blank pages via `magick` mean, ported from POC `pdf_to_images.py`).

- [ ] **Step 1: Write the failing test** — call `RenderPDF` on `testdata/sample1.pdf`; assert ≥1 PNG produced and near-blank pages dropped.

- [ ] **Step 2: Run (FAIL)**.

- [ ] **Step 3: Implement `render.go`** (exec `pdftoppm -png -r <dpi>`, `magick` mean threshold).

- [ ] **Step 4: Run (PASS)**.

- [ ] **Step 5: render service** — `cmd/render/main.go` consumes `render.q`, downloads PDF, renders, uploads pages, publishes result. Commit.

---

### Task 8: Transcribe stage service

**Files:**
- Create: `internal/pipeline/transcribe.go`, `internal/pipeline/transcribe_test.go`, `cmd/transcribe/main.go`

**Interfaces:**
- Consumes: page images; the `AIProvider` (via ai-gateway).
- Produces: `TranscribedPaper` to `{tenant}/{submission}/transcript.v{N}.json`; per-question
  `read_confidence`; result event with quality flags. `pipeline.Transcribe(ctx, prov, pages,
  subject) (contracts.TranscribedPaper, error)` (ported hybrid `dots_transcriber.py` + `markmap`).

- [ ] **Step 1: Write the failing test** with a mocked `AIProvider` returning canned OCR + answers; assert a `TranscribedPaper` with expected questions + confidence + checksum recorded.

- [ ] **Step 2–4: Run FAIL → implement → PASS.**

- [ ] **Step 5: transcribe service** consumes `transcribe.q`, persists transcript, emits flags. Commit.

---

### Task 9: Grade stage service (MarkScheme + minimal guide engine)

**Files:**
- Create: `internal/pipeline/grade/markscheme.go`, `internal/pipeline/grade/llmjudge.go`, `internal/pipeline/grade/guide.go`, `internal/pipeline/grade/grade_test.go`, `cmd/grade/main.go`

**Interfaces:**
- Consumes: `TranscribedPaper`; `AIProvider`; optional `MarkingGuide`.
- Produces: `GradedPaper` to `graded.v{N}.json`; result event with grade confidence + per-question
  `awarded_by` (`ai`|`human`). `type MarkScheme interface { Grade(ctx, q TranscribedQuestion)
  (GradedQuestion, error) }`; `LLMJudge`, `GuideMarkScheme` (Phase-1 supports `exact`/`exact_ci`/
  `set`; partial/method/tolerance/units/rubric land in Phase 3).

- [ ] **Step 1: Write a deterministic guide test** — `exact_ci` entry, no model call, full marks for a matching answer, zero otherwise.

- [ ] **Step 2: Write a fallback test** — question absent from guide → `LLMJudge` with mocked provider.

- [ ] **Step 3–4: FAIL → implement → PASS.**

- [ ] **Step 5: grade service** consumes `grade.q`, persists, emits. Commit.

---

### Task 10: Orchestrator — submission state machine + gate rules

**Files:**
- Create: `internal/domain/statemachine.go`, `internal/domain/statemachine_test.go`, `internal/domain/gates.go`, `cmd/orchestrator/main.go`

**Interfaces:**
- Consumes: `commands.q` (`SubmitExam`, `ApplyFix`, `RetryStage`, `ApproveForGrading`) + `results.q`.
- Produces: state transitions (sole writer via `Store.SetSubmissionState`), next-stage dispatch,
  DLQ-escalation to `failed_*`. `domain.NextState(cur SubmissionState, ev Event, flags
  []QualityFlag) (SubmissionState, error)`; `domain.EvaluateGates(t TranscribedPaper, tun
  Tunables) []QualityFlag`.

- [ ] **Step 1: Write state-machine tests** — clean transcript → `grading`; any of the four gate
  flags → `transcription_review_required`; technical failure after attempts → `failed`;
  re-delivery of an applied transition is a no-op (idempotent).

- [ ] **Step 2: Write gate tests** — low confidence, checksum mismatch, blank-over-threshold, structural anomaly each set the expected flag (thresholds from `Tunables`).

- [ ] **Step 3–4: FAIL → implement → PASS.**

- [ ] **Step 5: orchestrator service** wires `commands.q`+`results.q` to the state machine and dispatch. Commit.

---

### Task 11: api-svc — auth, RBAC, upload/status/results

**Files:**
- Create: `internal/auth/jwt.go`, `internal/auth/apikey.go`, `internal/auth/middleware.go`, `internal/domain/rbac.go`, `internal/domain/rbac_test.go`, `cmd/api/main.go`, `cmd/api/api_test.go`

**Interfaces:**
- Produces: chi server; `POST /v1/submissions` (upload PDF → object store → `SubmitExam` command),
  `GET /v1/submissions/{id}`, `GET /v1/submissions?state=…` (worklists), `GET
  /v1/submissions/{id}/result`; auth middleware resolving `principal{tenant, roles}` from JWT or
  API key; `domain.Can(roles []Role, action Action, ctx ResourceCtx) bool` enforcing the §11
  matrix + tenant isolation.

- [ ] **Step 1: Write the RBAC matrix test** — table of (role × action) expected allow/deny, plus a tenant-isolation negative test (principal of tenant A cannot read tenant B's submission) and the SaaS-only Operator cross-tenant flag.

- [ ] **Step 2: Write an API test** — Scanner API key can `POST /v1/submissions` but gets 403 on `GET result`; Teacher can read results in-tenant, 404/403 cross-tenant.

- [ ] **Step 3–4: FAIL → implement `rbac.go`, auth, handlers → PASS.**

- [ ] **Step 5: Commit** `feat: api-svc with JWT/API-key auth + RBAC + tenant isolation`.

---

### Task 12: audit-svc — append-only event log

**Files:**
- Create: `cmd/audit/main.go`, `internal/domain/audit.go`, `internal/domain/audit_test.go`

**Interfaces:**
- Produces: `audit.Record(ctx, AuditEvent)` (insert-only); `GET /v1/audit?submission=…`. Every
  override/approve/publish/retry writes an event `{actor, action, old, new, reason, ts}`.

- [ ] **Step 1: Write a test** — recording an override creates an immutable row; an attempted update fails (no update path exposed).
- [ ] **Step 2–4: FAIL → implement → PASS.**
- [ ] **Step 5: Commit.**

---

### Task 13: Wire services + happy-path integration test

**Files:**
- Create: `test/integration/pipeline_test.go`

- [ ] **Step 1: Write the integration test** — `compose up` (testcontainers compose or the
  Makefile stack); `POST` `testdata/sample1.pdf`; poll `GET /v1/submissions/{id}` until `graded`
  (or `*_review_required`); assert a `GradedPaper` artifact exists and the state walk matches §3.
- [ ] **Step 2: Add a forced-flag case** — inject a low-confidence transcript; assert routing to `transcription_review_required`.
- [ ] **Step 3: Add a forced-failure case** — provider errors; assert DLQ → `failed` with error detail.
- [ ] **Step 4: Run the suite (PASS).**
- [ ] **Step 5: Commit** `test: end-to-end pipeline integration`.

---

### Task 14: POC parity check

**Files:**
- Create: `test/parity/parity_test.go`, fixtures from POC `out/*.results.json`

- [ ] **Step 1: Write the parity test** — grade the three sample papers through the Go pipeline
  against the same vLLM endpoints (or recorded fixtures) and compare per-question marks to the
  POC `results.json` within tolerance; assert denominators reconcile where the POC's did.
- [ ] **Step 2: Run; record any divergence as issues.**
- [ ] **Step 3: Commit** `test: POC parity on sample papers`.

**Phase 1 spine done →** an exam can be submitted via API, walks the real state machine through
render/transcribe/grade with review + failure gates, is graded (guide + LLM-assist) and audited,
multi-tenant with RBAC, and matches POC behaviour.

---

## Phase 1b — remaining Phase-1 breadth (task-level)

### Task 15: Batch upload + batch state machine
Files: `internal/domain/batch.go`, `cmd/api` (batch endpoints). Produces: `POST /v1/batches`
(multiple PDFs / one multi-student PDF), batch state machine (§3), per-submission progress,
failed-item retry. Acceptance: a teacher uploads a class batch and sees per-item status.

### Task 16: Document splitting + page-anomaly detection
Files: `internal/pipeline/split.go`, extend `ingestion`. Produces: split multi-student PDF →
N submissions; detect blank/missing/duplicate pages and absent students → flags routing to review.
Acceptance: a multi-student PDF yields one submission per student; anomalies are flagged.

### Task 17: Student mapping
Files: `internal/domain/roster.go`. Produces: OCR name / candidate number → roster fuzzy-match →
`possible_wrong_mapping` flag → teacher confirm; named + anonymous candidate-number modes.
Acceptance: submissions map to roster students; ambiguous matches go to review.

### Task 18: Full school CRUD
Files: `cmd/api` resources for School/AcademicYear/Term/Department/Course/Class/Teacher/Student.
Produces: RBAC-scoped CRUD APIs. Acceptance: a school admin manages users, classes, assessments.

### Task 19: Assessment + marking-guide CRUD (import)
Files: `cmd/api` assessment/guide endpoints; `internal/pipeline/grade/guide.go` import. Produces:
create assessment, upload paper, import marking guide, version + lock-on-grading. Acceptance: an
assessment with a guide grades deterministically.

### Task 20: Teacher dashboard (web) — post-wireframe
Files: `web/` SPA (framework chosen with wireframes). Produces: dashboard, assessment list, batch
status, review queue, per-question review/override/approve wired to the API + SSE status.
Acceptance: criteria 1, 4, 7 from §17. **Gated on the wireframe prototype.**

---

## Later phases (task-level outlines — each becomes its own step-level plan)

### Phase 2 — Teacher review
- T2.1 Per-question review API (accept/override/edit-feedback/comment) → writes `teacher_review` + `audit_event`.
- T2.2 Approve + publish + lock; `final_grade` immutable after approve; publish gating (no publish before approve).
- T2.3 feedback-svc (basic) — separate stage drafting per-question feedback via ai-gateway.
- T2.4 Export CSV + PDF (reporting-svc).
- Acceptance: §17 criteria 3,4,5,6 (CSV).

### Phase 3 — Marking-guide engine (full)
- T3.1 Guide import formats + question→rubric mapping.
- T3.2 Deterministic rules: partial marks, method marks, multi-step, numeric tolerance, unit handling, alternatives/acceptable wording.
- T3.3 Rubric editor backend + versioning + lock-on-grading.
- T3.4 Confidence scoring per transcription + grade; preview + test-against-samples.
- Acceptance: a paper grades fully deterministically where the guide covers it; LLM only for rubric items.

### Phase 4 — Integrations
- T4.1 integration-svc abstraction (Connector interface per category) + `integration_connection` (encrypted creds).
- T4.2 SSO: Google Workspace + Microsoft Entra (OIDC) → identity-svc; api-gateway hardening (rate-limit/quotas).
- T4.3 LMS: Google Classroom + one of Teams/Moodle/Canvas (roster import + grade export).
- T4.4 SIS: CSV + generic OneRoster.
- T4.5 Storage: Google Drive / OneDrive ingestion.
- Acceptance: §17 criterion 6 (≥1 LMS/SIS target); start = CSV + Google Classroom/Microsoft.

### Phase 5 — Analytics & moderation
- T5.1 analytics-svc projections off graded events: item analysis, distributions, hardest questions, common mistakes.
- T5.2 Second-marker workflow + sampled moderation + AI-vs-teacher comparison.
- T5.3 Override-rate + grading-drift tracking; appeals/regrade workflow.
- T5.4 School-admin dashboard (completion, workload, turnaround, usage).

### Phase 6 — Advanced AI
- T6.1 AI feedback assistant + student revision suggestions.
- T6.2 Curriculum mapping (outcome tags) + learning-gap analysis.
- T6.3 Multi-provider gateway (full): OpenAI/Azure/Gemini/Claude + per-school config + cost tracking + retry/fallback + prompt-version registry.
- T6.4 Local/self-hosted-only AI mode (disable external AI).

---

## Continuous track (alongside every phase)
- Packaging: keep `deploy/compose` current; build `deploy/helm` charts; CI (build/test/lint/image push); on-prem upgrade path (gated migrations).
- Observability: structured logs, metrics, OpenTelemetry tracing keyed on `CorrelationID`; DLQ-depth alerts.
- Compliance: retention/auto-purge jobs (`retention_until`), blind-marking mode, DSAR export/delete, backups/DR.
- SaaS: usage metering (papers graded) for billing.

---

## Self-review (done)
- **Spec coverage:** every §1–§17 master-plan section maps to a task — positioning/principles →
  Global Constraints; states §3 → T3/T10; review §4 → T11/T2.x; guide engine §5 → T9/T3.x;
  batch §6 → T15–17; data model §7 → T4; moderation §8 → T5.x; authoring §9 → T19/T3.3; reports
  §10 → T2.4/T5; security §11 → T11/Constraints/Continuous; ai-gateway §12 → T6; components §13 →
  service tasks; integrations §14 → T4.x; APIs §15 → T11/T18/T19; roadmap §16 → phase split;
  acceptance §17 → noted per task.
- **Placeholder scan:** Phase-1 spine steps carry real code/commands; P1b and Phases 2–6 are
  intentionally task-level (their step-level code would be speculative — expanded when reached).
- **Type consistency:** `SubmissionState`/`CanTransition` (T3) reused in T10; `Envelope` (T3) used
  in T5/T7–10; `AIProvider`/`CompletionReq` (T6) used in T8/T9; `MarkScheme` (T9) matches POC;
  `domain.Can` (T11) referenced by audit/CRUD tasks.
