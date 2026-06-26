# NovaGrade Phase 5 — Analytics & Moderation (backend) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Give teachers and school leaders insight and quality control — item analysis (difficulty/discrimination), grade distribution, override & grading-drift metrics, a second-marker/sampled moderation workflow, and an appeals/regrade workflow. The school-admin **dashboard UI is deferred** (wireframes first); this phase builds the data/backend it will consume.

**Architecture:** Analytics are computed from stored graded results via pure functions over `[]contracts.GradedPaper` (reusing the Phase-4 `effectiveGradedPaper` gather), exposed as read-only API endpoints. Moderation and appeals are new tenant-scoped tables + workflow APIs. Override-rate and AI-vs-teacher drift are derived from `audit_event` (override actions) + `final_grade`. Everything is HERMETIC and TDD'd.

**Tech Stack:** Same locked stack — Go, chi, pgx+sqlc+goose (v1.31.1), amqp091-go, minio-go, golang-jwt, testify+testcontainers.

## Global Constraints
- Module `github.com/azrtydxb/novagrade`; branch `feat/phase5-analytics` (off main, P1–P4 merged).
- **No grade is mutated by analytics/moderation reads.** Analytics + comparison are read-only. A moderation second-mark or a regrade NEVER silently overwrites the teacher's final grade — it is recorded separately; any change to a final grade goes through the existing approval/override path with audit.
- RBAC + tenant isolation on every endpoint (analytics/override-stats = `ActionViewResults`; moderation = `ActionReviewFixApprove`; appeals file = ViewResults, resolve/regrade = ReviewFixApprove); 404-on-denial; principal-tenant scoped (no client-supplied tenant) via the shared `fetchAndAuthorize`/principal pattern.
- Locked stack: new queries sqlc-generated at v1.31.1 (no raw pgx); migrations additive + reversible, never editing earlier ones. NO git worktrees (commit on the branch). TDD throughout. No hardcoded secrets.
- Reuse, don't duplicate: the `effectiveGradedPaper` helper (Phase 4) for gathering per-submission effective grades; `ListSubmissionsByAssessmentVersion`, `GetFinalGrade`, `ListTeacherReviews`, `ListAuditEventsBySubmission`, `GetStudent`.

---

## Task 1: Analytics engine (pure) + store gather

**Files:**
- Create: `internal/analytics/analytics.go` (+ test)
- Possibly modify: `internal/api/export_handler.go` only if `effectiveGradedPaper` needs to be exported/shared (prefer reusing as-is).

**Interfaces (T2/T5 depend on these):**
- `analytics.ItemAnalysis(papers []contracts.GradedPaper) []QuestionStat` — per `question_no`: `Responses int`, `MeanAwarded float64`, `MaxMarks float64`, `Difficulty float64` (= MeanAwarded/MaxMarks, 0..1), `PctFullMarks float64`, `PctZero float64`, `Discrimination float64` (point-biserial-style: correlation of this question's score with the total score across students; document the formula; guard small N / zero variance → 0).
- `analytics.GradeDistribution(papers) Distribution` — histogram of `Score100` into buckets (e.g. 0-9,10-19,…,90-100) + mean/median/stddev/min/max/count.
- `analytics.HardestQuestions(stats []QuestionStat, n int) []QuestionStat` — lowest difficulty first.
- `analytics.FlagFrequencies(papers) map[string]int` — count of each flag across all graded questions (common-issue signal).
All PURE (no I/O); handle empty input gracefully (zero-value results, no panic/divide-by-zero).

- [ ] **Step 1: Write failing table-driven tests** — small fixed `[]GradedPaper` with known per-question marks → assert exact ItemAnalysis (difficulty, pctFull, discrimination on a hand-computable case), GradeDistribution (buckets + mean/median), HardestQuestions ordering, FlagFrequencies counts; empty input → zero values, no panic.
- [ ] **Step 2: Run (FAIL) → implement analytics.go → PASS.**
- [ ] **Step 3: Commit** — `feat(analytics): item analysis, grade distribution, hardest questions, flag frequencies (pure)`

---

## Task 2: Analytics + override/drift API

**Files:**
- Create: `internal/api/analytics_handler.go` (+ test); Modify: `cmd/api/main.go`

**Interfaces (RBAC `ActionViewResults`, tenant-isolated 404):**
- `GET /v1/assessment-versions/{avid}/analytics` — gather submissions (ListSubmissionsByAssessmentVersion) → effective graded papers (reuse `effectiveGradedPaper`, skip ungraded) → returns `{item_analysis:[...], distribution:{...}, hardest:[...], flag_frequencies:{...}, graded_count, total_count}`.
- `GET /v1/assessment-versions/{avid}/override-stats` — derive from audit_event + final_grade across the assessment version's submissions: `override_rate` (fraction of graded questions a teacher overrode, from `action="override_question"` audit events), `mean_abs_delta` (mean |teacher_final − ai_awarded| over overridden questions), `ai_vs_final_drift` summary. Document the exact derivation. (Add a store method if needed, e.g. `ListAuditEventsByAssessmentVersion` or iterate submissions calling ListAuditEventsBySubmission.)

- [ ] **Step 1: Write failing handler tests** (httptest + fakes): an avid with 3 graded submissions → analytics JSON with correct item_analysis/distribution; override-stats reflects N overrides → correct override_rate + mean delta; ungraded submissions skipped; cross-tenant → 404; lacking ViewResults → 404.
- [ ] **Step 2: Run FAIL → implement analytics_handler.go (+ any store method) → PASS; register routes.**
- [ ] **Step 3: Commit** — `feat(api): analytics + override/drift endpoints`

---

## Task 3: Second-marker / sampled moderation workflow

**Files:**
- Create: `internal/store/migrations/0006_moderation.sql`, `internal/store/queries/moderation.sql` (+ regen db), `internal/store/moderation_store.go` (+ test), `internal/api/moderation_handler.go` (+ test); Modify: `cmd/api/main.go`

**Migration 0006:** `moderation_session` (id, tenant_id, assessment_version_id, created_by, sample_size, status, created_at) + `moderation_mark` (id, tenant_id, session_id, submission_id, question_no, moderator_marks float, moderator text, created_at). Tenant-scoped; goose up/down.

**Interfaces:**
- `Store.CreateModerationSession(ctx, params)` — picks a SAMPLE of submissions for the assessment version (e.g. random or first-N; document) → returns the session + the sampled submission ids.
- `Store.RecordModerationMark(ctx, params)` (session, submission, question_no, marks, moderator) — append-only record of the second marker's mark.
- `Store.GetModerationComparison(ctx, tenant, sessionID)` — for each moderated question: the AI mark, the teacher final mark, the moderator mark, and the deltas; plus session-level agreement/override-rate.
- API (RBAC `ActionReviewFixApprove`, tenant-isolated): `POST /v1/assessment-versions/{avid}/moderation` (start a session, sample_size in body); `POST /v1/moderation/{id}/marks` (submit a moderator mark); `GET /v1/moderation/{id}` (the comparison report). A moderation mark does NOT change the final grade — it's recorded for comparison only (document; a discrepancy would be actioned via the normal override/approve path).

- [ ] **Step 1: Migration 0006 + sqlc + store methods** — TDD store tests (testcontainers): create session samples submissions; record marks; comparison returns AI/teacher/moderator deltas.
- [ ] **Step 2: API handler tests** (httptest): start session → sample returned; submit marks; GET comparison; cross-tenant 404; lacking ReviewFixApprove → 404. Assert the final grade is NOT mutated.
- [ ] **Step 3: Implement → PASS; routes; sqlc v1.31.1.**
- [ ] **Step 4: Commit** — `feat(moderation): sampled second-marker sessions + AI/teacher/moderator comparison`

---

## Task 4: Appeals / regrade workflow

**Files:**
- Create: `internal/store/migrations/0007_appeals.sql`, `internal/store/queries/appeal.sql` (+ regen db), `internal/store/appeal_store.go` (+ test), `internal/api/appeal_handler.go` (+ test); Modify: `cmd/api/main.go`

**Migration 0007:** `appeal` (id, tenant_id, submission_id, status [open|under_review|resolved|rejected], reason, requested_by, resolution, created_at, updated_at). Tenant-scoped; goose up/down.

**Interfaces:**
- `Store.CreateAppeal`, `Store.ListAppeals(tenant[, status])`, `Store.GetAppeal`, `Store.UpdateAppealStatus(ctx, tenant, id, status, resolution)`.
- API: `POST /v1/submissions/{id}/appeals` (ViewResults — a teacher/reviewer files on behalf of a student; reason) → 201 open appeal; `GET /v1/appeals?status=` (ViewResults, tenant worklist); `POST /v1/appeals/{id}/resolve` (ReviewFixApprove — body status+resolution); `POST /v1/appeals/{id}/regrade` (ReviewFixApprove) — triggers a re-grade by publishing a `RetryStage(grade)` command for the submission (the original graded artifact is preserved — versions retained, per the Phase-1 artifact model) and sets the appeal under_review; audit the action.
- Appeals never silently change a grade; a regrade re-runs grading (which still requires teacher approval to become final).

- [ ] **Step 1: Migration 0007 + sqlc + store methods** — TDD store tests.
- [ ] **Step 2: API handler tests** (httptest): file appeal → open; list by status; resolve; regrade publishes a RetryStage(grade) command (capture via fake bus) + audits + sets under_review; cross-tenant 404; RBAC.
- [ ] **Step 3: Implement → PASS; routes; sqlc v1.31.1.**
- [ ] **Step 4: Commit** — `feat(appeals): appeal/regrade workflow (regrade preserves original, re-requires approval)`

---

## Task 5: Phase-5 hermetic integration test

**Files:** Create `test/integration/analytics_moderation_test.go`

- [ ] **Step 1:** Reuse the harness. `TestAnalyticsModerationFlow`: drive multiple submissions for one assessment_version through grade→approve→publish (reuse Phase-2/3 helpers); override a mark on one (Phase-2 PATCH) so override-stats is non-trivial; GET /analytics → assert item_analysis + distribution reflect the known marks; GET /override-stats → assert override_rate; start a moderation session, submit a moderator mark, GET comparison → assert AI/teacher/moderator deltas and that the FINAL GRADE is unchanged; file an appeal → regrade (assert a RetryStage(grade) command is published and the original graded artifact still exists) → resolve. Poll (no fixed sleeps); SKIP_DOCKER_TESTS gate.
- [ ] **Step 2:** Run with Docker → PASS.
- [ ] **Step 3: Commit** — `test(integration): analytics + moderation + appeals end-to-end`

---

## Explicitly DEFERRED
- The school-admin **dashboard UI** (consumes these endpoints) — built after the wireframe prototype.
- Cross-assessment / longitudinal student-progress analytics and curriculum-outcome rollups beyond per-assessment (Phase 6 / later — `outcome_tag` modeling is already in the schema footings).

## Self-review (after writing)
- Coverage: analytics engine (T1), analytics+override/drift API (T2), moderation (T3), appeals/regrade (T4), e2e (T5). Dashboard UI deferred.
- Read-only safety: analytics/moderation/comparison never mutate a final grade; regrade re-runs grading and still requires approval. Locked stack sqlc v1.31.1; migrations 0006/0007 additive+reversible. Hermetic.
