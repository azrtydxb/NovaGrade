# NovaGrade — Regrade Finalize Fix — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Close the Phase-5 follow-up so an appeal regrade can FINALIZE: (1) preserve the original AI graded artifact across a regrade (don't overwrite it away), and (2) make re-approval after a regrade update the `final_grade` to the new grade (today the idempotent re-approve returns the stale one).

**Architecture:** Two contained changes plus a test. The grade worker archives the prior `graded.v1.json` before overwriting on a regrade (readers keep reading `graded.v1.json` = latest). `InsertFinalGrade` becomes an UPSERT; the approve handler always recomputes the effective paper and upserts (idempotent-in-effect), so a post-regrade re-approve finalizes the new grade while a plain retry rewrites identical values.

**Tech Stack:** Same locked stack — Go, chi, pgx+sqlc+goose (v1.31.1), amqp091-go, minio-go, testify+testcontainers.

## Global Constraints
- Module `github.com/azrtydxb/novagrade`; branch `fix/regrade-finalize` (off main, P1–P5 merged). NO git worktrees (commit on the branch; `.claude/worktrees/` is gitignored).
- INVARIANTS THAT MUST NOT BREAK: the teacher-approval gate (no grade final without `EventApproveByTeacher` from `teacher_review`); api-svc writes no submission STATE (approve publishes the `StageApprove` command; the orchestrator owns state); tenant isolation + RBAC unchanged; audit-first ordering on approve; per-question grading isolation. The `final_grade` row becomes the LATEST approved grade; prior values are preserved in the append-only `audit_event` (approve audit must record the final total).
- Locked stack: sqlc v1.31.1 (regenerate with `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`, commit db). TDD. No hardcoded secrets.

---

## Task 1: Preserve the prior graded artifact across a regrade

**Files:**
- Modify: `internal/gradeworker/gradeworker.go` (+ possibly `internal/store/objstore.go` for a copy/exists helper); Test: `internal/gradeworker/gradeworker_test.go`

**Behavior:**
- In `HandleEnvelope`, BEFORE writing `{tenant}/{submission}/graded.v1.json`, check whether that object already exists (a prior grade — i.e. a regrade). If it exists, COPY the existing bytes to an archive key `{tenant}/{submission}/graded.archive.{N}.json` where N is the next free archive index (count existing `graded.archive.*` or use a monotonic index; document). Then write the NEW grade to `graded.v1.json` (latest). On the FIRST grade (no existing artifact), no archive is made.
- Use the ObjStore (`Get`/`Put`; add a small `Exists`/`List`-prefix helper on ObjStore if needed, or attempt `Get` and treat not-found as "no prior"). Keep the worker's per-question isolation + the rest unchanged. NEVER delete the latest artifact.

- [ ] **Step 1: Write the failing test** — `TestHandleEnvelope_ArchivesPriorGradedArtifact` (testcontainers MinIO + the worker deps, OR a focused test of the archive logic with a fake ObjStore): grade a submission (writes graded.v1.json); grade it AGAIN with a different canned result (regrade) → assert graded.v1.json now holds the NEW result AND a `graded.archive.*.json` exists holding the FIRST result. First-grade case → no archive key.
- [ ] **Step 2: Run (FAIL) → implement the archive-before-overwrite in HandleEnvelope (+ any ObjStore helper) → PASS.**
- [ ] **Step 3: Verify** `go build ./...`, `go vet ./...`, `go test ./internal/gradeworker/...`, and that the Phase-3 marking-guide + Phase-5 integration tests still pass (the archive only triggers on a real second grade). Commit — `fix(gradeworker): archive prior graded.v1.json before a regrade overwrites it`

---

## Task 2: final_grade UPSERT + approve always finalizes (regrade re-approve updates)

**Files:**
- Modify: `internal/store/queries/final_grade.sql` (InsertFinalGrade → UPSERT) + regenerate db; `internal/api/approval_handler.go` (remove the early return-existing; always recompute + upsert + audit-with-total); Tests: `internal/api/approval_handler_test.go`, `internal/store/store_test.go`

**Behavior:**
- `InsertFinalGrade` query → `INSERT ... VALUES (...) ON CONFLICT (tenant_id, submission_id) DO UPDATE SET total = EXCLUDED.total, max_total = EXCLUDED.max_total, score_100 = EXCLUDED.score_100, graded_key = EXCLUDED.graded_key, approved_by = EXCLUDED.approved_by, approved_at = EXCLUDED.approved_at, updated_at = now() RETURNING ...`. (Keep the method name + signature stable; it now upserts.)
- `Approve` handler: REMOVE the early "GetFinalGrade exists → audit idempotent + return existing" short-circuit. ALWAYS: 409-unless-teacher_review (unchanged) → load `graded.v1.json` + overlay overrides → effective GradedPaper → `InsertAuditEvent(action="approve", new_value=JSON{total, max_total, score_100})` (audit-first; records the grade so history survives the upsert) → `PutObject(graded.final.json)` → `InsertFinalGrade` (now UPSERT) → `Publish(StageApprove)` → 200 + FinalGrade. This is idempotent-in-effect: a plain retry recomputes identical values and upserts the same row; a post-regrade re-approve (graded.v1.json now has the new grade) upserts the NEW final grade.
- Do NOT change the gate (still requires `teacher_review` + the orchestrator's `EventApproveByTeacher`), tenant isolation, or RBAC.

- [ ] **Step 1: Write/adjust failing tests:**
  - store: `TestInsertFinalGrade_UpsertsOnConflict` (testcontainers) — InsertFinalGrade for a (tenant,submission); InsertFinalGrade AGAIN with DIFFERENT total/graded_key → no error, GetFinalGrade returns the NEW values (upsert), still one row.
  - api: `TestApprove_AfterRegrade_UpdatesFinalGrade` — submission in teacher_review with an existing final_grade (simulating a re-opened+regraded submission whose graded.v1.json now holds a NEW grade) → Approve returns 200 with the NEW final grade, the audit records the new total, the StageApprove command is published. Update the existing `TestApprove_Idempotent` to the new semantic: a plain re-approve (same graded.v1) returns the SAME values (idempotent-in-effect), no error, command re-published.
- [ ] **Step 2: Run (FAIL) → change the InsertFinalGrade query + regenerate sqlc (commit db) + rewrite the Approve handler → PASS.**
- [ ] **Step 3: Verify** build/vet clean; `go test ./internal/store/... ./internal/api/...` pass (Docker for store); `SKIP_DOCKER_TESTS=1 go test ./...` green; sqlc v1.31.1. Commit — `fix(approve): UPSERT final_grade + always recompute so a post-regrade re-approve finalizes the new grade`

---

## Task 3: Integration test — regrade → re-grade → re-approve → finalized

**Files:** Modify `test/integration/analytics_moderation_test.go` (extend the appeal flow) OR create `test/integration/regrade_finalize_test.go`.

- [ ] **Step 1:** Reuse the harness. Drive a submission grade→approve→publish (capture the original final grade total). File an appeal → regrade → poll the submission re-opens to `teacher_review`. Make the fake AI provider return a DIFFERENT grade for the re-grade pass (so the new effective result differs). Re-approve (real ApprovalHandlers.Approve) → poll to `approved` → assert: (a) `GetFinalGrade` now returns the NEW total (different from the original), (b) a `graded.archive.*.json` exists in object storage holding the original grade (prior preserved), (c) the original final-grade value is recoverable from the audit trail (an approve audit with the original total). Resolve the appeal. Poll (no fixed sleeps); SKIP_DOCKER_TESTS gate.
- [ ] **Step 2:** Run with Docker → PASS.
- [ ] **Step 3: Commit** — `test(integration): regrade re-approve finalizes the new grade + original artifact archived`

---

## Self-review (after writing)
- The regrade loop is now end-to-end: re-open (existing) → re-grade (Task 1 preserves the prior) → re-approve (Task 2 finalizes the new) → audit/artifact retain the original.
- Invariants intact: gate unchanged (teacher_review + EventApproveByTeacher), api-svc writes no submission state, tenant/RBAC/audit-first unchanged, prior grade preserved in audit + archive. sqlc v1.31.1.
