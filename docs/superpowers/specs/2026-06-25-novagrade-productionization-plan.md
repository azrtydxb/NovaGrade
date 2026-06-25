# NovaGrade — productionization plan (master design)

**Date:** 2026-06-25 · **Repo:** github.com/azrtydxb/NovaGrade · **Branch:** `feat/go-platform`
**POC preserved at:** tag `poc-v0.1` / branch `poc-baseline`
**Status:** Approved direction. **Phase 1** is the next implementation target; phases 2–6 are
modeled here as the target architecture (each gets its own spec → plan cycle).

This is the master plan that turns the `wael-exames`/NovaGrade POC — a Python orchestration of
local vLLM models that grades scanned NESA exam PDFs — into a **production-ready, AI-assisted
assessment workflow for schools and teachers**. The design principle (§18) is firm: **keep the
POC OCR/transcription/grading pipeline as the technical core and wrap it in production
workflows** — we do not rebuild the pipeline unless needed. The missing production work is
teacher workflow, classroom/batch handling, deterministic marking-guide grading, review/approval,
integrations, auditability, reporting, and multi-tenant security.

---

## 1. Product positioning

NovaGrade is **not** a script that grades scans. It is an AI-assisted assessment workflow whose
production value is the end-to-end loop:

> ingest scanned exams or digital submissions → identify student/class/assessment → transcribe
> answers → apply the **official marking guide deterministically** where possible → **AI-assist**
> rubric evaluation only where needed → teacher **reviews, overrides, approves, publishes** →
> **export** grades and feedback back into school systems.

Two non-negotiable principles run through the whole design:
- **No grade is final until a teacher approves it.** AI proposes; the teacher is accountable.
- **Official marking guides are the default grading path.** LLM-as-judge is an *assistive* mode,
  not the production default (a deliberate flip from the POC).

---

## 2. Target architecture (services)

Go services connected by RabbitMQ, backed by Postgres (state/domain) and S3-compatible object
storage (artifacts). Three invariants hold everywhere: **stage workers are pure** (input artifact
→ output artifact + result event), the **orchestrator solely owns submission state**, and **RBAC
+ tenant isolation are enforced only at the edge, from the first commit**.

```
 Teachers/Admins (web app) ─┐
 3rd-party scanner / mobile ─┼─► api-gateway ──► api-svc ──(commands)──► orchestrator-svc
 LMS / SIS / SSO            ─┘   (TLS, authn,        │  ▲                  (submission + batch
                                  RBAC, rate-limit)  │  │ status/results    state machines,
                                                      │  │                   gates, retries)
   ingestion-svc ◄── upload ──────┘                  │  │                        │ dispatch
        │ split/detect                               │  │                        ▼
        ▼                                            │  │   split.q ─► ingestion-svc
   render-svc ─► transcribe-svc ─► grade-svc ─► feedback-svc ─► results.q ─► orchestrator
        │             │                │              │                          │
        └─── all stages call ──► ai-gateway-svc ◄─────┘                   graded events
                                 (provider abstraction:                          │
                                  OpenAI/Azure/Gemini/Claude/                     ▼
                                  local vLLM; cost/token/prompt           analytics-svc ─► read models
                                  tracking; schema validation)            reporting-svc ─► PDF/CSV
                                                                          integration-svc ◄─► LMS/SIS/SSO/storage/calendar
                                                                          audit-svc (append-only event log)

 Infra: RabbitMQ (work queues + DLQs + command queue) · Postgres · S3/MinIO
 Model providers (external): local vLLM (dots.ocr + qwen3-vl + qwen3.6-35b) and/or cloud LLMs
```

| Service | Responsibility | Phase introduced |
|---|---|---|
| **api-gateway** | TLS, authn token verification, RBAC enforcement, rate-limit/quotas | 1 (basic) → 4 (hardened) |
| **api-svc** | HTTP API (§15), publishes commands, serves status/results/worklists; owns no state | 1 |
| **orchestrator-svc** | Submission + batch **state machines** (§3), gate rules, stage dispatch, retries/DLQ | 1 |
| **ingestion-svc** | Document ingestion: split multi-student PDFs, detect blank/missing/duplicate pages, metadata extraction, student mapping | 1 |
| **render-svc** | PDF/page → page images (POC poppler/imagemagick, bundled) | 1 |
| **transcribe-svc** | pages → `Transcription` (POC hybrid read) via ai-gateway | 1 |
| **grade-svc** | `Transcription` → `AIGradingResult` via the **deterministic marking-guide engine** (§5); LLM-assist only where the guide requires it | 1 (LLM-assist) → 3 (full guide engine) |
| **feedback-svc** | grade → `Feedback` (separate stage; AI-assisted drafting) | 2 (basic) → 6 (assistant) |
| **ai-gateway-svc** | **Provider abstraction** (§12): routing, cost/token tracking, prompt versioning, schema validation, retry/fallback | 1 (vLLM) → 6 (multi-provider) |
| **reporting-svc** | Teacher/student/admin reports + PDF/CSV export (§10) | 2 (export) → 5 (analytics) |
| **analytics-svc** | Read-model projections off graded events: item analysis, distributions, drift (§10) | 5 |
| **integration-svc** | The integration abstraction (§14): SSO, LMS, SIS, storage, calendar | 4 |
| **audit-svc** | Append-only audit event log + query (§8, §11) | 1 |

> **UI note:** the teacher/admin **web app** (Phase 1 dashboard) is built only after a **wireframe
> prototype** (agreed earlier). Phase 1 includes the dashboard; its build is gated on wireframes.

---

## 3. Workflow states

Canonical **submission** state machine (orchestrator-owned). Every submission and batch exposes
its state + error detail via API.

```
uploaded → queued → splitting_pages → extracting_metadata → transcribing
   → transcription_review_required? ──(teacher fix)──┐
   → grading → grading_review_required? ──(teacher)──┤
   → teacher_review → approved → published → exported
                                   archived
   (any stage → failed, with error detail; recoverable via retry)
```

| State | Meaning |
|---|---|
| `uploaded` | File received, persisted to object store |
| `queued` | Accepted into a batch, awaiting processing |
| `splitting_pages` | ingestion-svc splitting a multi-student PDF / detecting page anomalies |
| `extracting_metadata` | Detecting student/class/assessment, mapping to roster |
| `transcribing` | transcribe-svc reading answers |
| `transcription_review_required` | Quality gate tripped → human review queue (held) |
| `grading` | grade-svc applying marking guide (+ AI-assist) |
| `grading_review_required` | Grading quality/low-confidence → human review queue (held) |
| `teacher_review` | Awaiting teacher accept/override/approve |
| `approved` | Teacher approved; grade locked |
| `published` | Grades/feedback released |
| `exported` | Pushed to LMS/SIS/CSV target |
| `failed` | Technical failure (after retries); error detail attached |
| `archived` | Retired/retained per retention policy |

**Batch** state machine (parent over N submissions): `uploaded → splitting → processing →
partially_complete → complete → exported`, with per-submission progress and failed-item retry.

---

## 4. Teacher review & approval workflow

The human surface over the pipeline. **No grade is final until a teacher approves.**

**Dashboard & lists:** teacher dashboard; list of uploaded assessments; per-class grading status;
per-student grading status; per-question confidence score; the **review queue** (Postgres-backed
view over `*_review_required` states).

**Per-item actions:** accept AI mark · override AI mark · edit feedback · re-run grading for one
question · re-run grading for one student · add teacher comments · approve final grade · publish
grades · **lock** final result after approval.

**Audit (always):** every action writes an `AuditEvent` capturing AI mark, teacher override,
actor, timestamp, and reason. A `FinalGrade` references the originating `AIGradingResult` and the
`TeacherReview` chain.

**Re-run semantics** reuse the POC's artifact model: re-running a question/student re-dispatches
the relevant stage against the saved upstream artifact, producing a new version while prior
versions are retained for audit and appeals.

---

## 5. Deterministic grading model (marking-guide engine — the production default)

LLM-as-judge is **not** the primary path. grade-svc prioritizes the **official marking guide**;
the LLM is invoked only for entries that explicitly need it (open-ended rubric) and always under
schema validation via the ai-gateway.

**Clear separation of concerns (three distinct stages/services):** `transcribe-svc` (what the
student wrote) → `grade-svc` (the mark) → `feedback-svc` (the explanation). Never blended.

**Marking-guide engine capabilities (extends the POC `MarkScheme`/`GuideMarkScheme`):**

| Capability | Behaviour |
|---|---|
| exact-answer | deterministic string compare (case modes) — no LLM |
| alternative correct answers | `accept: [...]` set match (synonyms, spellings) |
| teacher-defined acceptable wording | per-question accepted phrasings / patterns |
| partial marks | award a fraction per matched sub-criterion |
| method marks | credit a correct method even if the final answer is wrong (multi-step) |
| multi-step answers | step-by-step mark allocation with per-step rules |
| numeric tolerance | accept within ±tolerance (absolute or %) |
| unit handling | require/normalize units; mark unit separately |
| open-ended rubric scoring | LLM awards marks **bounded by the rubric** (assistive, schema-validated) |
| AI-assisted feedback generation | separate stage; never affects the mark |

Each entry maps a `Question` → `Rubric`, carries the authoritative `max_marks` (fixing the POC's
denominator drift), and records whether the mark was deterministic or AI-assisted. Confidence is
recorded per transcription and per grade (§8).

---

## 6. Batch & classroom workflow

Expand from one paper to a real classroom.

**Ingestion:** upload one PDF containing many students · upload multiple PDFs · **split papers by
student** · detect blank pages · detect missing pages · detect duplicate uploads · detect absent
students.

**Student mapping:** map submission → student (OCR name / candidate number → roster fuzzy-match →
teacher confirm) · support **anonymous candidate numbers** and **named students** · scope to
class/section/course. A possible-wrong-mapping flag (§8) routes to review.

**Batch operations:** show batch progress · retry failed papers · continue after partial failure
(per-item isolation, from the POC) · export class results · export per-student reports · export
per-question analysis.

---

## 7. School data model (entities)

Built across phases; **Phase 1 lays the structural footings for all of them** so later phases are
additive. `tenant_id (School)` on every row.

`School` · `AcademicYear` · `Term` · `Department` · `Course` · `Class` · `Teacher` · `Student` ·
`Parent/Guardian` *(later)* · `Assessment` · `AssessmentVersion` · `Question` · `MarkingGuide` ·
`Rubric` · `Submission` · `PageImage` · `Transcription` · `AIGradingResult` · `TeacherReview` ·
`FinalGrade` · `Feedback` · `AuditEvent` · `IntegrationConnection`.

Key relationships: `Assessment 1—* AssessmentVersion 1—* Question 1—1 Rubric`; `MarkingGuide`
versioned and **locked once grading starts**; `Submission *—1 Student`, `*—1 AssessmentVersion`;
`Submission 1—* PageImage`, `1—1 Transcription`, `1—* AIGradingResult` (versions), `1—*
TeacherReview`, `1—1 FinalGrade`, `1—* Feedback`; `AuditEvent` append-only, references any
entity; `IntegrationConnection` per School, encrypted credentials.

---

## 8. Moderation & quality control

Confidence and moderation, not only automation.

**Confidence & flags:** confidence per transcription · confidence per grade · flag low-confidence
answers · flag unreadable handwriting · flag answer mismatch · flag possible wrong student mapping
· flag possible missing page. Flags route the submission to the appropriate `*_review_required`
state (the gate rules; thresholds are per-tenant tunables).

**Moderation:** second-marker workflow · sampled moderation · compare AI mark vs teacher final
mark · track average **override rate** · track **grading drift** over time · **appeals/regrade**
workflow · preserve the **original submission and all grading versions** (immutable artifacts).

---

## 9. Assessment authoring & marking-guide tools

Teacher-facing authoring (UI in Phase 3; backend contracts modeled earlier):

create assessment · upload exam paper · upload official marking guide · define questions · define
max marks · define rubrics · define acceptable answers · define partial marks · define common
mistakes · define feedback templates · **preview grading behaviour** · **test the marking guide
against sample answers** · version marking guides · **lock guide after grading starts** · clone
previous assessments.

---

## 10. Reports & analytics

**Teacher:** class average · grade distribution · per-question performance · hardest questions ·
common mistakes · students needing help · rubric breakdown · improvement over time · export
PDF/CSV.

**Student:** final score · marks per question · teacher feedback · AI-assisted explanation ·
strengths · weaknesses · recommended revision topics.

**School/admin:** assessment completion · teacher workload · moderation status · grading
turnaround time · grade distribution by class/course · override rates · system usage.

Built as **read-model projections** (analytics-svc) off graded events (CQRS read side); export via
reporting-svc.

---

## 11. Security, privacy & compliance

multi-tenant **school isolation** (enforced below RBAC) · **RBAC** roles: school admin · teacher ·
reviewer/moderator · student *(optional)* · parent *(optional, later)* — plus platform **Operator**
and **Group-admin** (a group spanning multiple schools/tenants) from the earlier design ·
encryption **at rest** and **in transit** · **audit logs** (audit-svc) · **retention policy** ·
**delete/export student data** (DSAR) · **data residency** configuration · **anonymized grading
mode** (blind marking) · **configurable AI provider** · **option to disable external AI** ·
**self-hosted deployment mode** · **no hardcoded API keys** · **secret management** · **secure
file-upload validation**.

---

## 12. AI provider abstraction (ai-gateway-svc)

No hardwired LLM provider. All model calls (transcription, rubric grading, feedback) go through the
ai-gateway.

**Providers:** OpenAI · Azure OpenAI · Gemini · Claude · local **Ollama/vLLM** (the POC fleet
becomes one provider; *later* for full multi-cloud) · **per-school provider configuration**.

**Controls:** cost tracking per assessment · token-usage tracking · retry/fallback model support ·
**prompt/version tracking** · **deterministic output-schema validation** (reject/re-ask on
schema-invalid output) · option to **disable external AI** (self-hosted-only). Every AI call is
logged with model, prompt version, cost, and output schema (acceptance criterion §17).

---

## 13. Architecture stack (components)

Frontend web app (teachers/admins) · backend API · background job queue · document ingestion
service · OCR/VLM transcription service · grading service · feedback service · reporting service ·
integration service · audit service · storage service · database · object storage · event/job
status tracking. Built on the current repo's pipeline as the core, with strict separation of
**ingestion · transcription · grading · review · publishing · export**.

**Repository layout (Go monorepo, grows by phase):**
```
cmd/  api/ orchestrator/ ingestion/ render/ transcribe/ grade/ feedback/        # P1-2
      ai-gateway/ audit/ reporting/                                            # P1-2
      gateway/ identity/ integration/ analytics/                              # P4-5
internal/  pipeline/ domain/ queue/ store/ guide/ providers/                   # guide=engine §5, providers=§12
pkg/  contracts/                                                               # schemas + envelopes + full data model §7
web/                                                                          # SPA (after wireframes)
deploy/  compose/  helm/
```

POC → Go: `pdf_to_images.py`→render · `dots_transcriber.py`+`markmap.py`→transcribe ·
`grader.py`+`MarkScheme`→grade/guide engine · `schemas.py`→`pkg/contracts` · `llm_client.py`→
`internal/providers` (behind ai-gateway) · `report.py`→reporting.

---

## 14. Integration layer

An **integration abstraction** so schools never re-enter data. Each category is an interface with
pluggable providers; **start with CSV + Google Classroom / Microsoft**, add the rest later.

| Category | Targets (start → later) |
|---|---|
| Identity / SSO | Google Workspace for Education, Microsoft Entra ID / M365 Education, SAML, OAuth/OIDC → LDAP later |
| LMS | Google Classroom, Microsoft Teams for Education, Moodle, Canvas → Blackboard/Anthology, Brightspace later |
| SIS / gradebook | CSV import/export (fallback), generic **OneRoster** → PowerSchool, Infinite Campus, Skyward, OpenSIS |
| Storage | Google Drive, OneDrive, SharePoint, local upload, S3-compatible (self-host) |
| Calendar / deadlines | Google Calendar, Outlook Calendar, iCal export |

`IntegrationConnection` per School holds encrypted credentials + sync config.

---

## 15. APIs

api-svc exposes (RBAC-enforced, tenant-scoped):
create assessment · upload papers · upload marking guides · list batches · list submissions · read
transcription results · review grades · override marks · approve grades · publish results · export
results · integration sync · audit history. Plus status/worklist endpoints driving the dashboard
and review queue. Full OpenAPI spec is produced in the Phase 1 plan.

---

## 16. Roadmap (authoritative — 6 phases)

| Phase | Scope |
|---|---|
| **1 — Production foundation** | DB schema; auth; school/class/student model; batch upload; job queue; persistent storage; teacher dashboard (post-wireframe); existing OCR/grading pipeline wired to the UI; audit-svc; ai-gateway (vLLM provider) |
| **2 — Teacher review** | per-question review; override marks; edit feedback; approve/publish; lock; audit trail; export CSV/PDF; feedback-svc (basic) |
| **3 — Marking-guide engine** | marking-guide import; deterministic grading rules (§5 full); rubric editor; versioning + lock; confidence scoring; preview/test-against-samples |
| **4 — Integrations** | Google Classroom; Microsoft Teams/Education; Moodle or Canvas; CSV/OneRoster; Google Drive/OneDrive; api-gateway hardening; SSO/identity-svc |
| **5 — Analytics & moderation** | class analytics; question analytics; second marking; sampled moderation; override analytics; grading drift; school-admin dashboard |
| **6 — Advanced AI** | AI feedback assistant; student revision suggestions; curriculum mapping; learning-gap analysis; multi-provider AI gateway (full); local/self-hosted AI option |

Each phase gets its own spec → plan → implementation cycle. The earlier "A+B" core engine work is
absorbed into **Phase 1** (the orchestrator, queues, stage services, multi-tenancy, and RBAC are
the foundation Phase 1 builds).

---

## 17. Acceptance criteria (per the requirements)

- A teacher can upload a full class exam batch and see processing status.
- The system can split submissions and match them to students.
- Each question has transcription, proposed marks, confidence, and feedback.
- A teacher can override any mark and the override is audited.
- A final grade cannot be published until approved.
- Grades can be exported to CSV and at least one LMS/SIS integration target.
- A school admin can manage users, classes, and assessments.
- Low-confidence answers are automatically placed in the review queue.
- All AI calls are logged with model, prompt version, cost, and output schema.
- The system supports tenant isolation between schools.

These map to phases: criteria 1–2, 7–8, 10 → P1; 3 → P1–2; 4–5 → P2; 6 → P2/P4; 9 → P1 (ai-gateway).

---

## 18. Design principle (do not violate)

Keep the existing POC pipeline as the **technical core**; wrap it in production workflows. **Do not
rebuild the OCR/transcription pipeline unless needed.** The production work is: teacher workflow ·
classroom/batch handling · deterministic marking-guide support · review/approval · integrations ·
auditability · reporting · multi-tenant security.

---

## 19. Implementation backlog (Phase 1 — next plan)

The Phase 1 implementation plan (next step, via writing-plans) will detail:

1. **Scaffold** the Go monorepo (`cmd/`, `internal/`, `pkg/contracts`, `deploy/compose`).
2. **Domain model & migrations** — Postgres DDL for the §7 entities (full schema, footings for all
   phases) + the submission/batch state rows + append-only `audit_event`.
3. **Contracts** — port `schemas.py`; add envelopes; define the §15 API (OpenAPI).
4. **Queue layer** — RabbitMQ exchanges/routing/DLQs + the command/result envelope.
5. **ai-gateway-svc (vLLM provider)** — provider interface, schema validation, cost/token/prompt
   logging; POC `llm_client` behind it.
6. **Pipeline stage services** — ingestion (split + page/anomaly detection + student mapping),
   render, transcribe, grade (LLM-assist now; full guide engine in P3).
7. **orchestrator-svc** — submission + batch state machines, gate rules, retries/DLQ.
8. **api-svc + basic api-gateway** — auth (minimal internal JWT issuer + API keys), RBAC
   enforcement, upload/status/worklist/results endpoints.
9. **audit-svc** — write + query.
10. **Teacher dashboard (web)** — *after wireframe prototype*; lists, batch status, review queue
    skeleton wired to the API.
11. **Tests** — pure-stage units (provider mocked), state-machine, RBAC matrix + tenant isolation,
    integration (compose up; submit a class-batch PDF; walk states), POC parity check on the three
    sample papers.
12. **Packaging** — `deploy/compose` for on-prem; `deploy/helm` skeleton for cloud.

### Open questions for the Phase 1 plan
- Go HTTP router/framework + object-store + migration tooling choices.
- RabbitMQ topology specifics (exchange types, prefetch, DLQ backoff curve).
- Minimal JWT issuer for P1 vs deferring more identity to P4.
- SPA framework (decided alongside the wireframe prototype).
- Student-mapping matching strategy (name OCR vs candidate-number-first).
