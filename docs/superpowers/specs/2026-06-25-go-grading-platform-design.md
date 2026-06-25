# Design — Go grading platform (full product, phased)

**Date:** 2026-06-25
**Branch:** `feat/go-platform` (POC preserved at tag `poc-v0.1` / branch `poc-baseline`)
**Status:** Approved design. **Phase A+B** is planned for implementation now; **phases C–E and
the continuous track** are modeled here as the target architecture (designed, not yet planned).

This document turns the `wael-exames` POC — a Python orchestration of local vLLM models that
grades scanned NESA exam PDFs — into a real product: a multi-tenant, horizontally scalable,
queue-driven grading platform written in Go, with a human-in-the-loop review workflow, a
teacher/admin frontend, third-party integrations, and RBAC enforced end-to-end. It is modeled
**whole**, then sliced into shippable phases.

---

## 1. Context & goals

### Where we start
The POC is **thin orchestration**: render a PDF, fan out HTTP calls to three vLLM model servers
(dots.ocr, qwen3-vl, qwen3.6-35b), merge typed results across the `TranscribedPaper →
GradedPaper` boundary behind a `MarkScheme` interface, and write a report. The heavy ML work
lives in the model servers, **not** in Python — so porting to Go is a low-risk rewrite of glue
code, not an ML rewrite. The model fleet stays Python/vLLM throughout every phase.

### Product targets (fixed across all phases)
- Ships as **both** multi-tenant SaaS (hosted, smaller schools) **and** on-prem, from **one
  codebase**. A *school group* can span multiple tenants in either mode, so **multi-tenancy is a
  core first-class concept**, not a SaaS-only add-on. SaaS-vs-on-prem is a packaging concern.
- **On-prem forces self-hostable infrastructure only**: every dependency must run as a single
  container on-prem *and* scale out in cloud. No managed-cloud-only services.
- **RBAC and tenant isolation are enforced from the first commit** and in every phase — never
  retrofitted. The data is children's exam work (PII); authorization is non-negotiable.

---

## 2. Phased roadmap

| Phase | Builds | Status |
|---|---|---|
| **0 — Preserve** | POC baseline tag `poc-v0.1` / branch `poc-baseline` | ✅ done |
| **A+B — Core engine + control plane** | Go stage-services, RabbitMQ + DLQs, orchestrator/state machine, human-in-the-loop review + failure gates, multi-tenancy, RBAC enforcement, per-tenant tunables — plus the **structural domain model for the whole product** | **← implement now** |
| **C — Identity & gateway** | API gateway hardening, identity-svc: SSO/OIDC federation, user/tenant management, API-key lifecycle, self-serve signup, MFA, rate limiting/quotas | modeled |
| **D — Teacher/admin frontend** | Wireframe prototype → SPA: dashboard, intake, review-queue UX, gradebook, guide authoring, override + moderation; the feature backends those screens need | modeled |
| **E — Insight & integrations** | analytics-svc (item analysis, NESA outcome reporting, progress), feedback drafting, OneRoster/LMS-SIS sync, mobile capture, webhooks | modeled |
| **Continuous** | Packaging (compose/Helm), CI/CD, observability, compliance (retention/purge, blind marking, DSAR), backups/DR, SaaS billing/metering | modeled |

The guiding discipline: **A+B builds the pipeline + control plane and *models* the rest** (§9),
so phases C–E are additive — new services and screens over a domain model that already has their
footings poured.

---

## 3. Target architecture (all phases)

The complete platform. Services introduced after A+B are marked with their phase.

```
                                    ┌─────────────────────────── Phase C ────────────────────────┐
  teacher / admin (browser, D) ──┐  │  identity-svc: SSO/OIDC, users, tenants, API keys, MFA       │
  3rd-party scanner / mobile (E) ─┼─►│                                                              │
  LMS / SIS (E)                  ─┘  │  api-gateway: TLS, authn, RBAC enforce, rate-limit, quotas   │
                                     └───────────────┬──────────────────────────────────────────────┘
                                                     │ commands / queries
                                     ┌───────────────▼───────────────┐
                                     │  api-svc (A+B): edge, worklists│
                                     └───────┬───────────────▲────────┘
                          commands ──────────┘               │ status/results/analytics
                                     ┌────────▼──────────┐    │
                                     │ orchestrator-svc  │    │
                                     │ (A+B): state       │   │
                                     │  machine, gates,   │   │
                                     │  retries           │   │
                                     └───┬───────────┬────┘   │
                            dispatch     │           │ result events
                  render.q ─► render-svc │           │        │
                  transcribe.q ─► transcribe-svc ◄───┘        │
                  grade.q ─► grade-svc ──► results.q ─────────┘
                  feedback.q ─► feedback-svc (E)               │
                                                               ▼
                       ┌──────────── graded events (E) ──► analytics-svc (E) ──► read models
                       │
   Infra:  RabbitMQ (queues + DLQs) · Postgres (state, RBAC, audit) · S3/MinIO (artifacts)
   Model fleet (external, unchanged): vLLM dots.ocr + qwen3-vl + qwen3.6-35b
   Integrations (E): integration-svc ↔ OneRoster / Google Classroom / Canvas / Microsoft
```

Stage workers are always **pure** (input artifact → output artifact + result event); the
**orchestrator** is always the sole owner of exam state; the **api/gateway** layer is always the
only place RBAC is enforced. Those three invariants hold in every phase.

---

## 4. Phase A+B — core engine + control plane (implement now)

### 4.1 Services & infrastructure

| Service (Go) | Role | Scales by |
|---|---|---|
| **api-svc** | HTTP edge. Authn (JWT users / API-key service accounts) + RBAC enforcement. Accepts exam uploads, serves job status/results and review/failure **worklists**, accepts teacher *fix / retry / approve* actions. Publishes **commands**; owns **no** state transitions. | replicas behind LB |
| **orchestrator-svc** | The brain. Sole writer of exam state. Consumes commands + stage-result events, runs the state machine, applies gate rules, dispatches stages, manages retries/DLQ escalation. | 1–N (§4.5) |
| **render-svc** | Pure worker: PDF → page images. Shells out to poppler/imagemagick (bundled) to preserve the POC's exact render + near-blank-page detection. | replicas |
| **transcribe-svc** | Pure worker: pages → `TranscribedPaper` (hybrid dots.ocr + qwen3-vl + qwen3.6-35b structuring/merge + mark-map reconciliation). GPU-bound bottleneck. | replicas (gated by GPU fleet) |
| **grade-svc** | Pure worker: `TranscribedPaper` → `GradedPaper` + report (the `MarkScheme` grading). | replicas |

**Infra (single-container on-prem, clustered in cloud):** RabbitMQ (per-stage work queues + a
command queue + per-stage DLQs), Postgres (state, tenancy, RBAC, tunables, audit, worklists),
MinIO/S3 (all artifacts).

### 4.2 Coordination: command / event over RabbitMQ (CQRS-style)

- **api-svc publishes commands** to `commands.q`: `SubmitExam`, `ApplyFix`, `RetryStage`,
  `ApproveForGrading`. It never mutates exam state directly.
- **orchestrator-svc is the sole writer of exam state.** It consumes `commands.q` + stage-result
  events, runs the state machine (§4.4), and dispatches the next stage to `render.q` /
  `transcribe.q` / `grade.q`.
- **Stage workers are pure.** Consume an input-artifact reference, do the work, write the output
  artifact, publish a **result event** to `results.q` (`ok` + quality flags, or `failed` +
  error). No exam state held.

**Message envelope (every message):**
`{ tenant_id, principal, exam_id, assessment_id, stage, attempt, correlation_id, payload_ref }`
— `tenant_id` and `principal` travel everywhere, so isolation and auditability hold across the
whole pipeline, not just at the edge.

**Two kinds of "queue" (never conflated):**
1. **Work queues (RabbitMQ)** drive automated stages and carry **technical failures** to a
   per-stage **DLQ**: transient errors auto-retry with backoff up to `max_attempts` (tunable),
   then the orchestrator moves the exam to a `Failed_*` state.
2. **Human worklists (Postgres-backed)** — the "review queue" and "failure queue" — are **views
   over exam state** (`status = needs_review` / `failed_*`), served via API (and the GUI in D).
   Not AMQP queues. A teacher action emits a command that re-dispatches a stage.

**Why the split:** a hard POC lesson — re-running a *bad transcription* reproduces the same
systematic error. So **quality** failures go to a **human** (review queue), never an auto-retry
loop; only **technical** failures auto-retry via the DLQ.

### 4.3 Artifacts & free re-runs
Each stage writes an **immutable** artifact keyed by `(tenant_id, exam_id, stage, version)`:
```
{tenant}/{exam}/source.pdf · /pages/{n}.png · /transcript.v{N}.json · /graded.v{N}.json
```
Re-running a stage = the orchestrator re-dispatches it against the saved **upstream** artifact,
writing a new downstream version (v1 retained for audit). Generalizes the POC's
`--from-transcript`: a grade re-run never re-pays for OCR.

### 4.4 Exam state machine (orchestrator-owned)
```
SubmitExam → Intake → Rendering → Transcribing → [gate rules] ─clean→ Transcribed → Grading → Graded
                          │             │              ╲flagged→ NeedsReview ⇄ (ApplyFix/RetryStage)
                   Failed_Render   Failed_Transcribe                              Failed_Grade
                   (technical fail after attempts exhausted → human worklist → RetryStage)
```

**Gate rules (transcribe → grade), all tunable per tenant.** Route to **NeedsReview** if **any**:
1. **Low read confidence** — any question flagged `low_read_confidence`.
2. **Marks checksum mismatch** — detected total fails to reconcile with the stated total, or a
   per-section budget is off (POC `markmap` / `section_reconcile`).
3. **Missing/blank answers over `blank_threshold`** — signals dropped pages / transcription gaps.
4. **Structural anomalies** — question count off, a section absent, or a page that failed.
Otherwise → **Transcribed** → auto-dispatch grading.

**What a "fix" is (both supported):**
- **(a) Direct correction** — teacher edits the transcription → `transcript.v{N+1}` →
  `ApproveForGrading`.
- **(b) Re-tune & re-run** — teacher adjusts tunables → `RetryStage(transcribe)` from saved
  page images. Grade failures use the same mechanism (`Failed_Grade` → `RetryStage(grade)`).

### 4.5 Concurrency, scaling & isolation
- Stage workers scale horizontally; transcribe throughput is bounded by the shared GPU fleet
  (POC measured ~2× before saturation), so transcribe replicas track fleet capacity.
- **Per-item isolation** (from the POC): a failed page/question fails *that exam*, never a batch;
  RabbitMQ redelivery handles worker crashes.
- Orchestrator is a single active writer per tenant partition (serialized transitions). One
  instance suffices for A+B; the design allows partitioning state ownership by `tenant_id` later
  (leader lease / consumer groups) without touching workers.
- **Idempotency**: every command/dispatch carries `(exam_id, stage, attempt)`; re-delivery of an
  applied transition is a no-op (orchestrator checks current state before transitioning).

### 4.6 Multi-tenancy, RBAC & tunables (enforced now)
`tenant_id` on every row and message. Tenant isolation enforced **below** RBAC: a valid
principal still cannot reach another tenant's rows. **Operator cross-tenant access exists only in
SaaS mode** (deployment flag); on-prem it is disabled.

`principal` = **user** (JWT bearer) or **service account** (API key → tenant + Scanner role).
api-svc middleware resolves `(tenant, roles)` and enforces:

| Action | Operator | Group-admin | School-admin | Teacher | Reviewer | Scanner |
|---|---|---|---|---|---|---|
| Manage tenants / platform | ✅ cross-tenant | — | — | — | — | — |
| Manage users & API keys | ✅ | ✅ (group) | ✅ (tenant) | — | — | — |
| Edit tunables | ✅ | ✅ | ✅ | — | — | — |
| Submit exam | ✅ | ✅ | ✅ | ✅ | — | ✅ |
| View results | ✅ | ✅ (group) | ✅ (tenant) | ✅ | ✅ | — |
| Review: fix / retry / approve | ✅ | ✅ | ✅ | ✅ | ✅ | — |

Operator is the only cross-tenant role (SaaS only); Group-admin is scoped to its group's tenant
set; Teacher and Reviewer share review/fix powers in A+B; Scanner is submit-only.
Teacher/subject/class scoping is modeled (§9) but enforced at tenant level in A+B.

**Tunables (per-tenant config — the seam the GUI edits in D):** gate thresholds, retry
`max_attempts` per stage, render DPI, stage concurrency, model endpoints, data-retention period.

---

## 5. Phase C — identity & gateway

**Goal:** the full identity surface and a hardened public edge. RBAC *enforcement* already lands
in A+B; C provides *who the principal is* and *how the edge is protected*.

### Services
- **identity-svc** — user & tenant management, **SSO/OIDC federation** (Google Workspace,
  Microsoft Entra — what schools already use), self-serve tenant signup (SaaS), invite flows,
  password reset, **MFA**, session/refresh-token management, **API-key lifecycle** for scanners
  (issue/rotate/revoke, scoped to Scanner role).
- **api-gateway** — TLS termination, request validation, **rate limiting & per-tenant quotas**,
  authn token verification offloaded from api-svc, access logging. In A+B api-svc verifies tokens
  itself; C inserts the gateway in front and api-svc trusts the verified principal it forwards.

### Data added
`User` auth records, per-tenant `IdentityProvider` config, `Session`, `Invite`, MFA secrets,
API-key metadata (the A+B `ApiKey`/`Principal`/`Role` tables extend; they are not replaced).

### Key decisions deferred to C's own spec
OIDC library choice, gateway product (self-hosted: Traefik/Envoy/KrakenD vs a Go gateway in the
monorepo), token format/rotation policy, MFA method.

---

## 6. Phase D — teacher/admin frontend

**Goal:** the human surface over the A+B control plane and the feature backends its screens need.
**Wireframe prototype first** (agreed), then build.

### Frontend (SPA)
- **Dashboard** — tenant/class overview, queue depths, exams awaiting review.
- **Intake** — upload papers (single or class set), assign to an `Assessment`, attach a `Guide`.
- **Review-queue UX** — the high-value screen: side-by-side **scan vs transcript**, keyboard-
  driven quick-fix, apply correction or re-tune & retry. Speed here drives trust in the AI.
- **Gradebook** — class-set view (one `Assessment` × N `Submissions`), per-student results,
  exports.
- **Guide authoring** — build/version marking guides (the POC `scaffold_guide` flow as UI),
  reuse across classes, a tenant subject library.
- **Override & moderation** — change any AI mark (writes `grade_audit`), optional double-marking
  view comparing two markers and flagging disagreement.
- **Real-time** — exam-status updates via SSE/WebSocket from api-svc.

### Feature backends built in D (over the model A+B already poured)
Gradebook queries, guide CRUD + versioning, override endpoints (append `grade_audit`, set
`awarded_by=human`), moderation workflow (`moderation_status`), scan→student roster matching UI +
endpoint.

### Key decisions deferred to D's own spec
SPA framework (React/Svelte/…), real-time transport, design system. Wireframes precede this spec.

---

## 7. Phase E — insight & integrations

**Goal:** the value-add layer and the outward connections.

### Services
- **analytics-svc** — builds **read models/projections** off `graded` events (CQRS read side):
  **item analysis** (per-question class performance, difficulty/discrimination), **NESA outcome
  reporting** (rollups over each question's `outcome_tag`), **student progress** across
  assessments over time.
- **feedback-svc** — a pipeline stage (`feedback.q`) that drafts per-question student feedback
  (an LLM call), populating the `feedback` field; teacher edits/approves in D.
- **integration-svc** — **OneRoster** roster import; grade **export to LMS/SIS** (Google
  Classroom, Canvas, Microsoft); **webhooks** to 3rd parties on job completion; **mobile capture**
  ingestion (photograph papers → `Submission`, reusing the scanner API path).

### Data added
Analytics read models / projections, curriculum outcome taxonomies per tenant, webhook
subscriptions, integration credentials (per-tenant, encrypted).

### Key decisions deferred to E's own spec
Projection store (Postgres materialized views vs separate read DB), feedback model/prompting,
each integration's auth and sync cadence.

---

## 8. Continuous track — packaging, ops, compliance

Runs alongside every phase, not a discrete slice.

- **Packaging & release** — `deploy/compose` (on-prem: one container each for RabbitMQ, Postgres,
  MinIO, and the services; school points model endpoints at their own GPU box) and `deploy/helm`
  (SaaS: same images, clustered infra, replica-scaled, you host the GPU fleet). CI/CD, image
  registry, versioned releases with an on-prem **upgrade/migration path** (DB migrations gated).
- **Observability** — structured logging, metrics, **distributed tracing** (OpenTelemetry across
  services; the `correlation_id` in the envelope ties a trace to an exam). Queue-depth and
  stage-latency dashboards; alerting on DLQ growth.
- **Compliance (children's data)** — **data retention/auto-purge** jobs honoring
  `retention_until`; **blind marking** (hide student identity during grading/review to reduce
  bias *and* protect PII); **data residency** (on-prem is itself a selling point here);
  GDPR/child-data **DSAR** (export & delete); backups/DR.
- **SaaS billing/metering** — per-tenant usage (papers graded) metering feeding billing; on-prem
  licensing. Not present on-prem.

---

## 9. Cross-cutting domain model — build the pipeline, model the product

A+B's **services** only do the pipeline; A+B's **contracts/tables** anticipate every phase so
later phases are pure additions.

### Built and exercised in A+B
- `Tenant`, `Principal` (user / service-account), `Role`, `ApiKey`
- `Assessment` — a paper definition (subject, term, expected structure, `guide_id?`)
- `Submission` — one student's scan (today's "exam"); the unit the pipeline processes
- `Exam` lifecycle state row (status, current stage, attempt, artifact versions)
- `TranscribedPaper`/`TranscribedQuestion`, `GradedPaper`/`GradedQuestion` (ported from POC)
- `Guide` — a **versioned, tenant-scoped** marking-guide resource referenced by `Assessment`
- `grade_audit` — **append-only**: `(exam_id, question_no, old, new, awarded_by, actor, reason,
  ts)`; every mark carries `awarded_by` (`ai` | `human`)
- per-tenant `Tunables`

### Modeled now, populated/used later
| Field/entity | Modeled in | Used by |
|---|---|---|
| `Student`, `Roster`, `submission.student_id` (scan→student match) | A+B | D gradebook, E |
| `moderation_status` on a graded result | A+B | D double-marking |
| `outcome_tag` / `standard` on each question | A+B | E outcome reporting |
| `feedback` on `GradedQuestion` | A+B | E feedback drafting |
| `retention_until` on `Submission` | A+B | continuous purge |
| `IdentityProvider`, `Session`, `Invite`, MFA | C | C |
| webhook subscriptions, integration creds, outcome taxonomies, projections | E | E |

The `MarkScheme` interface, schemas, and the typed `TranscribedPaper → GradedPaper` boundary
carry over from the POC unchanged — exactly what makes the stage-per-service split and the future
swaps safe.

---

## 10. Repository layout (Go monorepo, grows by phase)

```
cmd/
  api/ orchestrator/ render/ transcribe/ grade/      # A+B
  identity/ gateway/                                  # C
  feedback/ analytics/ integration/                  # E
internal/
  pipeline/   # ported POC stages (render, transcribe/markmap, grade/MarkScheme)
  domain/     # state machine, gate rules, RBAC matrix, tenancy
  queue/      # RabbitMQ publish/consume, DLQ, envelope
  store/      # Postgres repositories + object-store client
  llm/        # OpenAI-compatible vLLM client (ported llm_client.py)
pkg/
  contracts/  # shared schemas + message envelopes (ported schemas.py + full domain model)
web/          # SPA (D)
deploy/
  compose/    # on-prem      helm/   # cloud
```

**POC → Go mapping:** `pdf_to_images.py`→render · `dots_transcriber.py`+`markmap.py`→transcribe ·
`grader.py`+`MarkScheme`→grade · `schemas.py`→`pkg/contracts` · `llm_client.py`→`internal/llm` ·
`parallel.py`→goroutines/worker pools · `report.py`→grade report writer · `cli.py` orchestration
→ `cmd/orchestrator` (state machine) + `cmd/api` (edge).

---

## 11. Testing strategy

- **Pure-stage unit tests** with the vLLM client mocked (mirrors the POC's offline tests).
- **State-machine tests**: every transition, gate-rule routing, retry/DLQ escalation, idempotent
  re-delivery.
- **RBAC matrix tests**: each role × action + tenant-isolation negative tests + the SaaS-only
  Operator cross-tenant flag.
- **Contract tests** on `pkg/contracts` envelopes between services.
- **Integration test**: compose up RabbitMQ + Postgres + MinIO + services; submit a sample PDF;
  assert Intake → … → Graded; assert a forced quality flag routes to NeedsReview and a forced
  technical error routes via DLQ to Failed_*.
- **End-to-end parity check**: grade the three sample papers through the Go pipeline and compare
  against the POC's `out/*.results.json` to confirm the port preserves behaviour.
- (Per phase: C auth/identity tests, D frontend + feature-backend tests, E projection/integration
  tests — detailed in each phase's own spec.)

---

## 12. Deployment

One codebase, two packagings (see §8). On-prem disables Operator cross-tenant and SaaS billing;
the school supplies its own GPU box for the model fleet. SaaS clusters the same images and hosts
the fleet. The model fleet (vLLM: dots.ocr, qwen3-vl, qwen3.6-35b) is external to every phase and
unchanged.

---

## 13. Scope note & next step

**This spec models the whole product; only Phase A+B is planned for implementation now.** Phases
C, D, E and the continuous track are designed here to keep A+B's domain model and service
invariants forward-compatible, and each will get its **own** spec → plan → implementation cycle
when its turn comes (D is gated on a wireframe prototype first).

The immediate next step is an **implementation plan for Phase A+B only**.

### Open questions for the A+B implementation plan
- RabbitMQ topology: exchange types, routing keys, prefetch per worker, DLQ TTL/backoff curve.
- Postgres migration tooling and exact DDL for the state row + audit log.
- JWT issuance for A+B (a minimal internal issuer) given full identity is Phase C.
- Go HTTP router/framework choice and object-store client library.

These are implementation details, not design blockers.
