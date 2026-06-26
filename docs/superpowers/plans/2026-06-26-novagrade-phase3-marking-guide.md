# NovaGrade Phase 3 — Full Marking-Guide Engine — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make official marking guides the authoritative, expressive grading path — extend the deterministic engine with partial/method/multi-step/numeric-tolerance/unit/alternative match types and per-question confidence, and make guides a versioned, tenant-scoped, lockable resource that teachers import, test against samples, and attach to assessments.

**Architecture:** Extends the Phase-1 `internal/pipeline/grade` engine (the `MarkScheme`/`GuideMarkScheme` already does exact/exact_ci/set/rubric + LLMJudge fallback). New deterministic match types are pure functions (no LLM). Guides become rows in the existing `marking_guide` table (now versioned + lockable); a new guide-management API (api-svc edge, RBAC + tenant-isolated) imports/validates/versions/locks guides and previews them against sample answers without persisting.

**Tech Stack:** Same locked stack — Go, chi, pgx+sqlc+goose (v1.31.1), amqp091-go, minio-go, golang-jwt, testify+testcontainers.

## Global Constraints
- Module `github.com/azrtydxb/novagrade`; branch `feat/phase3-marking-guide` (off main, Phases 1+2 merged).
- **Marking guides are the production-default grading path; LLM-as-judge stays assistive (rubric + fallback only).** New match types are DETERMINISTIC (no model call) and reproducible run-to-run.
- Per-question **confidence**: deterministic matches → 1.0; rubric/LLM → the model/heuristic confidence. Surface it on `GradedQuestion.GradeConfidence`.
- Guides are **versioned + tenant-scoped + lockable**: a guide is locked once grading starts; a locked guide cannot be edited (new version required). `marking_guide` already has `content jsonb`, `assessment_version_id`, `tenant_id`.
- api-svc edge: RBAC on every guide endpoint (guide authoring = `EditTunables`-class, i.e. school_admin/group_admin/operator; teachers may import for their own assessments — pick and document); tenant-isolated (404 cross-tenant) via the shared `fetchAndAuthorize`.
- Locked stack: new queries sqlc-generated at v1.31.1; no raw pgx. TDD throughout. No hardcoded secrets.
- Backward compatibility: existing guides (exact/exact_ci/set/rubric) must keep grading identically; new match types are additive.

---

## Task 1: Deterministic match-type engine (the accuracy core)

**Files:**
- Modify: `internal/pipeline/grade/guide.go` (GuideEntry + dispatch), Create: `internal/pipeline/grade/matchers.go`
- Test: `internal/pipeline/grade/matchers_test.go`

**Interfaces / capabilities (extend `GuideEntry`; document the final JSON schema in the report):**
Add these `match` types, each graded DETERMINISTICALLY (no provider call) and clamped to `[0, max_marks]`:
- **`numeric`** — parse the student answer as a number; award full marks if within tolerance of `answer`. Fields: `answer` (number), `tolerance` (number, default 0), `tolerance_type` ("abs"|"pct", default "abs"). Strip a trailing unit before parsing if `unit` set.
- **`unit`** (or a `unit` modifier on `numeric`) — require/normalize a unit; optionally award `unit_marks` for the correct unit separately from the value. Document whether unit is a standalone type or a numeric modifier.
- **`multi_step`** — award per-step marks (this also delivers **method marks**: a correct step earns its marks even if the final answer is wrong). Field: `steps: [{accept: [...] | answer, marks, match}]`; sum awarded step marks, clamp to max_marks.
- **`partial`** — award marks per matched sub-criterion/keyword. Field: `criteria: [{accept: [...], marks}]`; sum marks for matched criteria, clamp.
- **alternatives / acceptable wording** — enhance `set`/`exact` matching with optional normalization (`normalize: true` → casefold + collapse whitespace + strip punctuation) so acceptable wordings match; keep existing behavior when `normalize` is absent.

Each matcher returns `(awarded float64, confidence float64, justification string)`. Confidence = 1.0 for a clean deterministic decision; you MAY lower it for partial/ambiguous numeric parses (document the rule). `GuideMarkScheme.Grade` dispatches on `match` to the new matchers; unknown types still fall back to the existing `fallback` (LLMJudge). `Flags` stays non-nil (`[]string{}`). Existing match types unchanged.

- [ ] **Step 1: Write failing matcher tests** (table-driven, pure, no provider): numeric within/outside abs and pct tolerance; numeric with unit stripped; multi_step awarding method marks when the final answer is wrong but a step is right; partial summing matched criteria and clamping at max; `set`/exact with `normalize` accepting alternate wording/case/punctuation; each returns the expected awarded + confidence. Assert NO provider is invoked for any of these.
- [ ] **Step 2: Run (FAIL).**
- [ ] **Step 3: Implement `matchers.go`** + extend `GuideEntry` + the dispatch in `guide.go`. Keep deterministic (no LLM) for all the above.
- [ ] **Step 4: Run (PASS); confirm existing grade tests + parity (hermetic) unaffected.**
- [ ] **Step 5: Commit** — `feat(grade): deterministic match types (numeric/tolerance/unit/multi_step/partial/normalize) + per-question confidence`

---

## Task 2: Guide as a versioned, lockable, tenant-scoped resource (store)

**Files:**
- Create: `internal/store/migrations/0003_guide_versions.sql`, `internal/store/queries/marking_guide.sql` (+ regenerate db)
- Modify: `internal/store/store.go`; Test: `internal/store/store_test.go`

**Migration:** add to `marking_guide`: `version int NOT NULL DEFAULT 1`, `name text`, `locked bool NOT NULL DEFAULT false`, `locked_at timestamptz`, and a `UNIQUE(tenant_id, assessment_version_id, version)`. (goose up/down.)

**Interfaces (Tasks 3/4/5 depend on these):**
- `Store.InsertGuideVersion(ctx, InsertGuideVersionParams) (MarkingGuide, error)` — params: TenantID, AssessmentVersionID, Name, Content (json.RawMessage), Version (caller supplies next version, or the query computes max+1 — document). Inserts a new version (unlocked).
- `Store.GetLatestGuide(ctx, tenantID, assessmentVersionID uuid.UUID) (MarkingGuide, error)` — highest version; `ErrNotFound` if none.
- `Store.ListGuideVersions(ctx, tenantID, assessmentVersionID uuid.UUID) ([]MarkingGuide, error)` — version DESC.
- `Store.LockGuide(ctx, tenantID, guideID uuid.UUID) error` — sets locked=true, locked_at=now; `ErrNotFound` if missing. (A locked guide row is never content-mutated; edits create a new version.)

- [ ] **Step 1: Migration 0003** (do not edit 0001/0002).
- [ ] **Step 2: Write failing repo tests** (testcontainers + Docker-skip): insert v1 + v2 → GetLatestGuide returns v2; ListGuideVersions DESC; LockGuide sets locked; GetLatestGuide→ErrNotFound when none.
- [ ] **Step 3: Run FAIL → sqlc queries + regenerate (commit db) + Store methods + MarkingGuide struct → PASS.**
- [ ] **Step 4: Commit** — `feat(store): versioned, lockable marking_guide repository`

---

## Task 3: Guide-management API (import / list / get / lock) + assessment linkage

**Files:**
- Create: `internal/api/guide_handler.go`, `internal/api/guide_handler_test.go`; Modify: `cmd/api/main.go`

**Interfaces (all RBAC + tenant-isolated via `fetchAndAuthorize`; pick `ActionEditTunables` for authoring — document):**
- `POST /v1/assessment-versions/{avid}/guides` — import a guide JSON for an assessment version. **VALIDATE** the guide with the Task-1 engine (every entry has a known `match` + required fields; reject 400 with details on invalid). Inserts a new version (max+1). 201 + the version.
- `GET /v1/assessment-versions/{avid}/guides` — list versions (metadata: version, name, locked, created_at).
- `GET /v1/assessment-versions/{avid}/guides/latest` — the latest guide content.
- `POST /v1/guides/{id}/lock` — lock a guide version (no further edits; a new import creates a new version).
- **Lock-on-grading:** when grading starts for a submission of this assessment version, the guide in use is locked. For Phase 3, enforce at import/edit time: a `POST .../guides` is allowed (new version) but editing happens by NEW version only; document that grade-svc/orchestrator will call `LockGuide` when it first grades against a guide (wire this into grade-svc loading the guide if straightforward; else expose the lock endpoint + note the orchestrator wiring as the mechanism).

- [ ] **Step 1: Write failing handler tests** (httptest + fakes): import a valid guide → 201 + version persisted; import an INVALID guide (bad match type / missing field) → 400 with validation detail and nothing persisted; list/get/lock happy paths; lock then import → new version created (lock doesn't block a new version); cross-tenant → 404; lacking EditTunables → 403/404.
- [ ] **Step 2: Run FAIL → implement guide_handler.go (reuse fetchAndAuthorize + the Task-1 validator) + routes → PASS.**
- [ ] **Step 3: Commit** — `feat(api): marking-guide import/list/get/lock with validation`

---

## Task 4: Preview / test-a-guide-against-sample-answers

**Files:**
- Create: `internal/api/guide_preview_handler.go`, `internal/api/guide_preview_handler_test.go`; Modify: `cmd/api/main.go`; maybe `internal/pipeline/grade` (a pure preview helper)

**Interface:**
- `POST /v1/guides/preview` — body `{guide: <guide JSON>, samples: [{question_no, student_answer}, ...]}`. RBAC `ActionEditTunables` (or ViewResults — document), tenant-scoped (no resource fetch needed; it's stateless). Runs the Task-1 deterministic engine over the samples WITHOUT persisting anything and WITHOUT calling the LLM (rubric/fallback entries return a "needs LLM / not previewable deterministically" marker rather than a live model call — keep preview hermetic/cheap). Returns per-sample `{question_no, awarded, max_marks, match_type, confidence, justification}`. This lets a teacher validate a guide before using it.

- [ ] **Step 1: Write failing tests**: preview a guide with numeric/partial/multi_step/set entries over sample answers → correct awarded marks per sample; a rubric entry → returns the not-deterministically-previewable marker (no model call). Invalid guide → 400.
- [ ] **Step 2: Run FAIL → implement (a pure `grade.PreviewGuide(guide, samples)` helper + handler) → PASS.**
- [ ] **Step 3: Commit** — `feat(api): guide preview / test-against-samples (deterministic, no persistence)`

---

## Task 5: Phase-3 integration test (import → grade with rich match types → version/lock)

**Files:**
- Create: `test/integration/marking_guide_test.go`

- [ ] **Step 1: Write the integration test** (reuse the Phase-1/2 harness): import a guide (via the real API) containing numeric+tolerance, multi_step, and partial entries for an assessment version; drive a submission whose transcript exercises those questions through grading (the grade stage loads the guide); assert the GradedPaper awards marks deterministically per the new match types (exact expected values, run-to-run stable), the guide-covered questions made NO LLM call, and a second import creates v2 while v1 can be locked. Use `pollSubmission`; SKIP_DOCKER_TESTS gate.
- [ ] **Step 2: Run (PASS) with Docker.**
- [ ] **Step 3: Commit** — `test(integration): marking-guide import + deterministic rich-match grading`

---

## Self-review (run after writing)
- Coverage: match-type engine (T1 — the accuracy core + confidence), versioned/lockable guide store (T2), management API + validation + lock (T3), preview/test (T4), end-to-end (T5). Rubric editing = import-new-version (T3). 
- Determinism: all new match types are LLM-free and reproducible; preview is hermetic.
- Locked stack: new queries sqlc v1.31.1; backward compatibility (old match types unchanged) asserted by existing grade/parity tests staying green.
