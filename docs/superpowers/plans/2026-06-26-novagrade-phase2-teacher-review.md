# NovaGrade Phase 2 — Teacher Review & Approval — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add the human-in-the-loop teacher workflow on top of the Phase-1 spine: review/override graded questions, edit feedback, approve (locking the grade), publish, export — every action audited; no grade final until a teacher approves.

**Architecture:** New api-svc endpoints publish the existing approval commands (`EventApproveByTeacher`/`EventPublish`/`EventExport`) to `commands.q`; the orchestrator (Phase 1) already drives the state machine. api-svc remains the edge; the orchestrator stays the sole submission-state writer. Overrides + approvals are persisted to the `teacher_review` / `final_grade` tables (footings already in the schema) and every action writes an append-only `audit_event` via the Phase-1 audit `Record` path.

**Tech Stack:** Same locked stack as Phase 1 — Go, chi, pgx+sqlc+goose, amqp091-go, minio-go, golang-jwt, testify+testcontainers.

## Global Constraints
- Module `github.com/azrtydxb/novagrade`; build on Phase 1 (merged to main). Branch `feat/phase2-teacher-review`.
- **No grade final until a teacher approves.** A submission's state IS its lock: overrides allowed ONLY when `state == teacher_review`; `approve` only from `teacher_review`; `publish` only from `approved`; `export` only from `published`. Reject out-of-order actions with 409.
- **api-svc is NOT a state writer** — it publishes commands; the orchestrator owns `SetSubmissionState`. api-svc MAY write the `teacher_review`/`final_grade`/audit tables (those are not the submission-state row).
- **Every review/override/approve/publish/export writes an append-only `audit_event`** (use the Phase-1 `audit.Record`/`Store.InsertAuditEvent`; never an update/delete).
- Multi-tenancy + RBAC enforced on every new endpoint (action `ReviewFixApprove` for review/override/approve/publish; `ViewResults` for export/read), tenant-isolated (404 cross-tenant), exactly like the Phase-1 handlers.
- Locked stack: any new query is sqlc-generated (no raw pgx). Regenerate at sqlc v1.31.1.
- TDD throughout; no hardcoded secrets.

---

## Task 1: Review/approval store + domain (teacher_review, final_grade, lock)

**Files:**
- Modify: `internal/store/queries/*.sql` (+ regenerate `internal/store/db/`), `internal/store/store.go`
- Modify: `internal/store/migrations/0001_core.sql` ONLY if the footing columns are insufficient (prefer adding a new migration `0002_phase2.sql` rather than editing 0001)
- Test: `internal/store/store_test.go`

**Interfaces (later tasks depend on these):**
- `Store.InsertTeacherReview(ctx, InsertTeacherReviewParams) (TeacherReview, error)` — params: tenant_id, submission_id, question_no, old_marks, new_marks, feedback, comment, actor; append a review row.
- `Store.ListTeacherReviews(ctx, tenant, submissionID) ([]TeacherReview, error)` — overrides for a submission (latest per question wins when merging).
- `Store.InsertFinalGrade(ctx, InsertFinalGradeParams) (FinalGrade, error)` — immutable snapshot: tenant_id, submission_id, total, max_total, score_100, graded_key (the graded artifact + overrides applied), approved_by, approved_at. NO update/delete query for final_grade.
- `Store.GetFinalGrade(ctx, tenant, submissionID) (FinalGrade, error)` → `ErrNotFound` if not approved.

- [ ] **Step 1: Migration** — add `0002_phase2.sql` (goose up/down) ensuring `teacher_review` has columns (id, tenant_id, submission_id, question_no, old_marks, new_marks, feedback, comment, actor, created_at) and `final_grade` has (id, tenant_id, submission_id, total, max_total, score_100, graded_key, approved_by, approved_at, created_at) — add any missing columns to the footings. Down drops the added columns/constraints.
- [ ] **Step 2: Write the failing repo test** — testcontainers Postgres; create school+submission; `InsertTeacherReview` then `ListTeacherReviews` returns it; `InsertFinalGrade` then `GetFinalGrade` round-trips; `GetFinalGrade` on a non-approved submission → `ErrNotFound`. Docker-skip guard.
- [ ] **Step 3: Run (FAIL)** — `go test ./internal/store/...`
- [ ] **Step 4: sqlc queries + regenerate + implement the Store methods.** `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`; commit generated code.
- [ ] **Step 5: Run (PASS) + commit** — `feat(store): teacher_review + final_grade repositories`

---

## Task 2: Review/override API + audit

**Files:**
- Create: `internal/api/review_handler.go`, `internal/api/review_handler_test.go`
- Modify: `cmd/api/main.go` (route registration)

**Interfaces:**
- Consumes: `Store` (Task 1), `domain.Can`, auth middleware, `ObjStore` (read `graded.v1.json`), `audit.Record`/`Store.InsertAuditEvent`.
- Produces:
  - `GET /v1/submissions/{id}/review` — `ReviewFixApprove`, tenant-isolated. Returns the graded result with any teacher overrides merged (latest override per question wins) + a `locked` flag (true unless state==teacher_review).
  - `PATCH /v1/submissions/{id}/questions/{qno}` — body `{awarded_marks?, feedback?, comment?}`. `ReviewFixApprove`, tenant-isolated. **409 if state != teacher_review** (locked). On success: clamp awarded to [0, question max], `InsertTeacherReview` (old from current effective value, new from body), `InsertAuditEvent` (entity=submission, action="override_question", old/new JSON, actor, reason=comment). Returns the updated effective question.

- [ ] **Step 1: Write failing handler tests** (httptest + fake Store/ObjStore + fake bus): a teacher overrides a question's marks → 200, a teacher_review row recorded, an audit_event recorded; override on a submission NOT in teacher_review → 409; cross-tenant → 404; a Scanner (no ReviewFixApprove) → 403/404; GET /review merges overrides.
- [ ] **Step 2: Run (FAIL).**
- [ ] **Step 3: Implement** `review_handler.go` (merge logic: load graded.v1.json, overlay latest ListTeacherReviews per question_no); register routes in cmd/api.
- [ ] **Step 4: Run (PASS).**
- [ ] **Step 5: Commit** — `feat(api): per-question override + review view, audited`

---

## Task 3: Approve / publish / export API + audit + lock

**Files:**
- Create: `internal/api/approval_handler.go`, `internal/api/approval_handler_test.go`
- Modify: `cmd/api/main.go`

**Interfaces:**
- Consumes: `Store`, `domain.Can`, auth, `ObjStore`, the queue `Bus` (publish commands), `audit`.
- Produces (all `ReviewFixApprove`, tenant-isolated):
  - `POST /v1/submissions/{id}/approve` — **409 unless state==teacher_review**. Builds the effective graded result (graded.v1 + overrides), writes it to `graded.final.json`, `InsertFinalGrade` (immutable snapshot, approved_by=principal), `InsertAuditEvent(action="approve")`, and publishes an `EventApproveByTeacher` command envelope to `commands.q` (orchestrator advances teacher_review→approved). Returns the FinalGrade. **After approve, Task-2 override returns 409** (state no longer teacher_review — already enforced by the state guard).
  - `POST /v1/submissions/{id}/publish` — **409 unless state==approved**. `InsertAuditEvent(action="publish")` + publish `EventPublish`. 
  - `POST /v1/submissions/{id}/export` — **409 unless state==published**. `InsertAuditEvent(action="export")` + publish `EventExport`.

- [ ] **Step 1: Write failing tests** — approve from teacher_review → 200 + FinalGrade persisted + audit "approve" + an `ApproveByTeacher` command published (capture via fake bus); approve when NOT teacher_review → 409; publish before approve → 409; publish from approved → 200 + command; export before published → 409; cross-tenant → 404; no ReviewFixApprove → 403. Assert final_grade is the immutable snapshot (overrides applied).
- [ ] **Step 2: Run (FAIL).**
- [ ] **Step 3: Implement** `approval_handler.go`; register routes. (The command envelope's `Stage` carries the event verb the orchestrator's `StageToEvent` maps to `EventApproveByTeacher`/`EventPublish`/`EventExport` — read internal/orchestrator + internal/domain to use the exact verbs/constants; add command-stage constants to pkg/contracts if missing.)
- [ ] **Step 4: Run (PASS).**
- [ ] **Step 5: Commit** — `feat(api): approve/publish/export with lock + audit, drives approval commands`

---

## Task 4: CSV export endpoint

**Files:**
- Create: `internal/api/export_handler.go`, `internal/api/export_handler_test.go`
- Modify: `cmd/api/main.go`

**Interfaces:**
- Produces: `GET /v1/submissions/{id}/export.csv` — `ViewResults`, tenant-isolated. Streams CSV with header `question_no,section,max_marks,awarded_marks,feedback,flags` over the effective graded result (final_grade snapshot if approved, else graded.v1 + overrides). `Content-Type: text/csv`, `Content-Disposition: attachment`.

- [ ] **Step 1: Write failing test** — request CSV for a graded submission → 200, `text/csv`, header row + one row per question with correct awarded marks (reflecting an override); cross-tenant → 404; missing grade → 409/404.
- [ ] **Step 2: Run (FAIL).**
- [ ] **Step 3: Implement** with `encoding/csv`; register route.
- [ ] **Step 4: Run (PASS).**
- [ ] **Step 5: Commit** — `feat(api): CSV export of graded results`

---

## Task 5: feedback-svc (basic) — separate AI feedback stage

**Files:**
- Create: `internal/pipeline/feedback.go`, `internal/pipeline/feedback_test.go`, `cmd/feedback/main.go`

**Interfaces:**
- Consumes: `feedback.q` envelopes, `AIProvider`, `ObjStore`.
- Produces: per-question draft feedback written into the graded artifact's `feedback` field (or a `feedback.v1.json` sidecar), via a `pipeline.DraftFeedback(ctx, prov, model, graded contracts.GradedPaper) (contracts.GradedPaper, error)` (one provider call per question lacking feedback, schema-validated; isolation per question). Keep grading and feedback SEPARATE — feedback never changes a mark.

- [ ] **Step 1: Write failing test** with a mocked `AIProvider` returning canned feedback; assert each question gets non-empty drafted feedback and `awarded_marks` is UNCHANGED.
- [ ] **Step 2: Run (FAIL) → implement → PASS.**
- [ ] **Step 3: feedback service** — `cmd/feedback/main.go` consumes `feedback.q`, drafts, persists, publishes a result. (Wiring the orchestrator to dispatch feedback before teacher_review is a small orchestrator change; include it only if straightforward, else note as a follow-up — the endpoint/stage existing is the deliverable.)
- [ ] **Step 4: Commit** — `feat(feedback): basic AI feedback drafting stage (mark-independent)`

---

## Task 6: Phase-2 integration test (review → approve → publish → export, audited & locked)

**Files:**
- Create: `test/integration/teacher_review_test.go`

- [ ] **Step 1: Write the integration test** (real infra + fake provider + in-process api/orchestrator, reusing the Phase-1 harness in `test/integration`): drive a submission to `teacher_review`; PATCH-override a question; GET /review shows the override; POST /approve → state advances to `approved`, final_grade persisted (override applied), audit "approve" present; a second override now → 409 (locked); POST /publish → `published`; POST /export → `exported`; GET export.csv reflects the override. Assert audit_event rows for override+approve+publish+export.
- [ ] **Step 2: Run (PASS) with Docker.**
- [ ] **Step 3: Commit** — `test(integration): teacher review→approve→publish→export flow`

---

## Self-review (run after writing)
- Coverage: review/override (T2), approval gate wiring + lock (T3), audit on every action (T2/T3), CSV export (T4), feedback-svc (T5), end-to-end (T6). The Phase-1 follow-up (wire the approval commands + audit Record) is delivered by T3.
- Locked-stack: every new query sqlc-generated at v1.31.1.
- State lock: enforced by the `state==X` guards in T2/T3, verified by 409 tests + the T6 locked-override assertion.
