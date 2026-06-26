# NovaGrade Phase 6 — Advanced AI (backend) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add the AI-assisted pedagogical layer on top of the graded results: a curriculum/outcome model, outcome-mastery & learning-gap analytics, a per-tenant multi-provider AI gateway (so each school can point at OpenAI / Azure / a self-hosted vLLM endpoint), and an AI revision-suggestions assistant (student-facing "how to improve" + teacher regenerate). Dashboard UI remains deferred (wireframes first); this builds the data/backend it consumes.

**Architecture:** Reuse the existing footing — the `providers.AIProvider` (`Complete`) abstraction + the OpenAI-format `VLLMProvider` (injectable `BaseURL`/`APIKey`), the `pipeline.DraftFeedback` per-question-isolated additive stage, the `secrets.Encrypt/Decrypt` (AES-256-GCM) credential pattern already used by `integration_store`, and the Phase-5 `internal/analytics` pure-function + `effectiveGradedPaper` gather. New: curriculum/outcome tables + mapping; outcome-mastery/gap analytics; a per-tenant `providers.Registry` resolving an `AIProvider` from encrypted per-tenant config; a `DraftRevisionSuggestions` stage + a teacher regenerate endpoint. Everything HERMETIC and TDD'd; the AI surface is the OpenAI-compatible chat-completions format (covers OpenAI, Azure OpenAI, vLLM, self-hosted, OpenRouter). Live provider credentials are supplied by the operator; native Anthropic/Gemini adapters are a documented follow-up.

**Tech Stack:** Same locked stack — Go, chi, pgx+sqlc+goose (v1.31.1), amqp091-go, minio-go, golang-jwt, testify+testcontainers.

## Global Constraints
- Module `github.com/azrtydxb/novagrade`; branch `feat/phase6-advanced-ai` (off main, P1–P5 + regrade-finalize merged). NO git worktrees (commit on the branch; `.claude/worktrees/` is gitignored).
- INVARIANTS THAT MUST NOT BREAK: AI feedback/revision is STRICTLY ADDITIVE — it NEVER changes `awarded_marks`/`max_marks`/`Total`/`Score100` (same guarantee as `DraftFeedback`). The teacher-approval gate is untouched (no grade final without approval; regenerate is pre-approval only — 409 if already approved, preserving the finalized snapshot). api-svc writes no submission STATE. Tenant isolation + RBAC on every endpoint (404-on-denial; principal-tenant scoped, no client-supplied tenant) via the shared `fetchAndAuthorize`/principal pattern. Provider API keys: encrypted at rest with `secrets.Encrypt` (key from `INTEGRATION_ENC_KEY`), NEVER logged, NEVER returned in any API response (write-only; show-once at creation if at all). Analytics are read-only (never mutate a grade).
- Locked stack: new queries sqlc-generated at v1.31.1 (regenerate with `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`, commit db); migrations 0009/0010 additive + reversible, never editing earlier ones. TDD throughout. No hardcoded secrets; no live external AI calls in tests (fake provider).
- Reuse, don't duplicate: `effectiveGradedPaper` (Phase 4 gather), `internal/analytics` (Phase 5), `ListSubmissionsByAssessmentVersion`, `secrets.Encrypt/Decrypt`, the `integration_store` encrypted-cred pattern, `providers.VLLMProvider`, `pipeline.DraftFeedback`.

---

## Task 1: Curriculum / outcome data model + mapping API

**Files:**
- Create: `internal/store/migrations/0009_curriculum.sql`, `internal/store/queries/curriculum.sql` (+ regen db), `internal/store/curriculum_store.go` (+ test), `internal/api/curriculum_handler.go` (+ test); Modify: `cmd/api/main.go`

**Migration 0009 (tenant-scoped, goose up/down):**
- `curriculum_outcome` (id uuid pk, tenant_id uuid not null, code text not null, description text not null, subject text not null, created_at timestamptz default now(), UNIQUE(tenant_id, code)).
- `question_outcome` (id uuid pk, tenant_id uuid not null, assessment_version_id uuid not null, question_no text not null, outcome_id uuid not null references curriculum_outcome(id), created_at timestamptz default now(), UNIQUE(tenant_id, assessment_version_id, question_no, outcome_id)). Indexed by (tenant_id, assessment_version_id).

**Interfaces (sqlc store methods, all tenant-scoped):**
- `CreateOutcome(ctx, params{TenantID, Code, Description, Subject}) (CurriculumOutcome, error)`
- `ListOutcomes(ctx, tenantID) ([]CurriculumOutcome, error)`
- `MapQuestionOutcome(ctx, params{TenantID, AssessmentVersionID, QuestionNo, OutcomeID}) (QuestionOutcome, error)`
- `ListQuestionOutcomes(ctx, tenantID, assessmentVersionID) ([]QuestionOutcome, error)`

**API (RBAC: `ActionEditTunables` for create/map — outcomes are configuration; tenant-isolated 404):**
- `POST /v1/outcomes` (body code/description/subject) → 201 outcome.
- `GET /v1/outcomes` → tenant's outcomes.
- `POST /v1/assessment-versions/{avid}/question-outcomes` (body question_no + outcome_id) → 201 mapping; validate the outcome belongs to the tenant (404 if not).
- `GET /v1/assessment-versions/{avid}/question-outcomes` → mappings for the version.

- [ ] **Step 1:** Migration 0009 + sqlc + store methods; TDD store tests (testcontainers): create outcome; duplicate code → unique violation surfaced; map a question; list mappings; cross-tenant list returns nothing.
- [ ] **Step 2:** API handler tests (httptest + fakes): create outcome; list; map question→outcome; mapping with a foreign-tenant outcome → 404; lacking EditTunables → 404; cross-tenant avid → 404.
- [ ] **Step 3:** Implement → PASS; register routes; sqlc v1.31.1.
- [ ] **Step 4: Commit** — `feat(curriculum): outcome model + question→outcome mapping (tenant-scoped, RBAC)`

---

## Task 2: Outcome-mastery & learning-gap analytics

**Files:**
- Create: `internal/analytics/outcomes.go` (+ test); Modify: `internal/api/analytics_handler.go` (+ test) to add the endpoint; `cmd/api/main.go` only if a new store dep is needed.

**Interfaces (PURE, no I/O; in `internal/analytics`):**
- Types: `OutcomeStat{ OutcomeID string; Code string; Description string; MappedQuestions int; Responses int; MeanPct float64; Mastery string }` where `Mastery` ∈ {"secure","developing","emerging"} by `MeanPct` thresholds (≥0.75 secure, ≥0.5 developing, else emerging — document).
- `OutcomeMastery(papers []contracts.GradedPaper, mapping map[string][]string) []OutcomeStat` — `mapping` is question_no → []outcome key (code or id). For each outcome: gather the mapped questions' awarded/max across all papers, `MeanPct` = Σawarded/Σmax (guard Σmax==0 → 0), `Responses` = count. Stable order by Code. Empty input / no mapping → empty slice, no panic.
- `LearningGaps(stats []OutcomeStat, n int) []OutcomeStat` — the n weakest outcomes (lowest MeanPct first; ties by Code) among those with Responses>0.

**API (RBAC `ActionViewResults`, tenant-isolated 404):**
- `GET /v1/assessment-versions/{avid}/outcome-mastery` → gather submissions → effective graded papers (reuse `effectiveGradedPaper`, skip ungraded) + the question→outcome mapping (ListQuestionOutcomes + ListOutcomes for codes/descriptions) → `{ outcomes:[OutcomeStat...], gaps:[OutcomeStat...], graded_count }`. `gaps` = `LearningGaps(outcomes, 5)`.

- [ ] **Step 1:** Failing table-driven analytics tests — a small fixed `[]GradedPaper` + a known mapping → assert exact `OutcomeMastery` (MeanPct + Mastery bucket on a hand-computable case) and `LearningGaps` ordering; empty → empty, no panic/divide-by-zero.
- [ ] **Step 2:** Run FAIL → implement `outcomes.go` → PASS.
- [ ] **Step 3:** Failing handler test (httptest + fakes): an avid with graded submissions + mappings → outcome-mastery JSON with correct per-outcome MeanPct + gaps; ungraded skipped; cross-tenant → 404; lacking ViewResults → 404.
- [ ] **Step 4:** Run FAIL → implement the endpoint (reuse the analytics gather pattern) → PASS; register route.
- [ ] **Step 5: Commit** — `feat(analytics): outcome mastery + learning-gap analysis`

---

## Task 3: Per-tenant multi-provider AI gateway (registry + config)

**Files:**
- Create: `internal/store/migrations/0010_ai_provider.sql`, `internal/store/queries/ai_provider.sql` (+ regen db), `internal/store/ai_provider_store.go` (+ test), `internal/providers/registry.go` (+ test), `internal/api/ai_provider_handler.go` (+ test); Modify: `cmd/api/main.go`.

**Migration 0010 (tenant-scoped, goose up/down):**
- `ai_provider_config` (id uuid pk, tenant_id uuid not null, name text not null, provider_type text not null CHECK in ('openai','azure_openai','vllm','self_hosted'), base_url text not null, model text not null, api_key_enc bytea, is_default boolean not null default false, created_at timestamptz default now(), UNIQUE(tenant_id, name)). Partial unique index enforcing at most one default per tenant: `CREATE UNIQUE INDEX ai_provider_one_default ON ai_provider_config(tenant_id) WHERE is_default;`.

**Interfaces:**
- Store (api_key_enc is ALREADY-ENCRYPTED bytes — caller encrypts with `secrets.Encrypt`, mirroring `integration_store`): `CreateAIProviderConfig(ctx, params)`, `ListAIProviderConfigs(ctx, tenantID)` (returns rows WITHOUT the key bytes for listing — or a row type whose key field callers must not serialize), `GetDefaultAIProviderConfigWithKey(ctx, tenantID) (row incl. api_key_enc, error)`, `SetDefaultAIProviderConfig(ctx, tenantID, id)` (clears the prior default in a tx).
- `providers.Registry` (in `internal/providers/registry.go`):
  - `type ConfigSource interface { DefaultConfig(ctx, tenantID uuid.UUID) (ProviderConfig, error) }` where `ProviderConfig{ ProviderType, BaseURL, Model string; APIKey string }` (APIKey already decrypted by the source).
  - `type Registry struct { Source ConfigSource; Fallback AIProvider; FallbackModel string; PriceTable map[string]ModelPrice; LogSink func(AICallLog) }`.
  - `func (r *Registry) Resolve(ctx, tenantID uuid.UUID) (AIProvider, string)` — looks up the tenant's default config; builds a `VLLMProvider` (OpenAI-format) from BaseURL/APIKey + returns its model; on no-config / error, returns `r.Fallback`, `r.FallbackModel` (the env vLLM default). Cache per tenant is optional (document if added).
- API (RBAC `ActionEditTunables`; tenant-isolated 404; the encrypted key is provided at create and NEVER returned):
  - `POST /v1/ai-providers` (body name/provider_type/base_url/model/api_key) → encrypt api_key via `secrets.Encrypt(encKey, ...)` → 201 with the config MINUS the key.
  - `GET /v1/ai-providers` → tenant's configs, each WITHOUT api_key (and without api_key_enc).
  - `POST /v1/ai-providers/{id}/default` → set as the tenant default.
- Self-hosted/local AI = a config with `provider_type=self_hosted` and a local `base_url` (e.g. an on-prem vLLM); no code path differs — document in README that this is how a school runs fully local.

- [ ] **Step 1:** Migration 0010 + sqlc + store methods; TDD store tests (testcontainers): create config; one-default-per-tenant enforced (second default insert / SetDefault clears prior); GetDefaultWithKey returns the encrypted bytes; ListConfigs does not expose the key.
- [ ] **Step 2:** `Registry.Resolve` tests (no DB — a fake ConfigSource): tenant with a config → a VLLMProvider built with that BaseURL + the config's model; tenant with no config (source returns not-found) → Fallback + FallbackModel. (Assert via a ConfigSource spy + that the returned model matches; do NOT make a real HTTP call.)
- [ ] **Step 3:** API handler tests (httptest + fakes): create provider (assert the stored bytes are ENCRYPTED, not plaintext; assert the response JSON has NO api_key field); list (no keys); set-default; lacking EditTunables → 404; cross-tenant → 404.
- [ ] **Step 4:** Implement → PASS; register routes; wire a `Registry` in `cmd/api/main.go` (Source backed by the store + `secrets.Decrypt`, Fallback = the env vLLM provider). sqlc v1.31.1. Keys never logged.
- [ ] **Step 5: Commit** — `feat(ai-gateway): per-tenant provider config + registry (encrypted keys, env fallback)`

---

## Task 4: AI revision-suggestions assistant + teacher regenerate

**Files:**
- Modify: `pkg/contracts/schemas.go` (add `Revision string` to `GradedQuestion`); Create: `internal/pipeline/revision.go` (+ test), `internal/api/feedback_handler.go` (+ test); Modify: `internal/pipeline/feedback.go` wiring or the feedback worker `cmd/feedback`/`internal/pipeline` to also draft revisions; `cmd/api/main.go`.

**Behavior:**
- Add `Revision string `json:"revision,omitempty"`` to `contracts.GradedQuestion` — student-facing "how to improve" guidance. STRICTLY ADDITIVE (never affects marks). Verify nothing that marshals GradedQuestion breaks (it's an additive optional field).
- `pipeline.DraftRevisionSuggestions(ctx, prov providers.AIProvider, model string, graded contracts.GradedPaper) (contracts.GradedPaper, error)` — mirrors `DraftFeedback` EXACTLY in contract: per-question isolation (one provider error → that question's Revision stays empty, continue, return nil), idempotent (skip questions with non-empty Revision), NEVER mutates marks/totals, returns a new value (no input mutation). Distinct prompt (`revision-v1`) focused on actionable next steps for the student given their answer + the awarded vs max.
- Teacher regenerate endpoint (RBAC `ActionReviewFixApprove`; tenant-isolated 404): `POST /v1/submissions/{id}/feedback/regenerate` — 409 if the submission is `approved`/`published`/`exported` (do NOT mutate a finalized snapshot); otherwise load `graded.v1.json`, run `DraftFeedback` + `DraftRevisionSuggestions` using the per-tenant provider resolved via the Phase-6 `Registry` (clearing existing Feedback/Revision first so it truly regenerates), write the updated `graded.v1.json` back, audit `action="regenerate_feedback"`, return the updated paper. Marks unchanged (assert in test). This rewrites the PRE-approval working artifact only.

- [ ] **Step 1:** Add the `Revision` field + a schemas test that round-trips it and asserts marks fields untouched. Failing `DraftRevisionSuggestions` test (fake provider): every question gets a Revision; a provider error on one question leaves that Revision empty and others populated; marks/totals identical before/after; idempotent skip.
- [ ] **Step 2:** Run FAIL → implement `revision.go` → PASS.
- [ ] **Step 3:** Failing handler test (httptest + fakes + a fake Registry/provider + object store): regenerate on a `teacher_review` submission → graded.v1.json now has fresh Feedback + Revision, marks unchanged, an audit `regenerate_feedback` written; regenerate on an `approved` submission → 409; cross-tenant → 404; lacking ReviewFixApprove → 404.
- [ ] **Step 4:** Run FAIL → implement the handler (resolve provider via Registry; clear+redraft; rewrite artifact; audit-first) → PASS; register route. Optionally wire `DraftRevisionSuggestions` into the feedback worker so revisions are drafted in the normal pipeline too (additive; per-question isolation) — if so, add/extend a feedback worker test.
- [ ] **Step 5: Commit** — `feat(feedback): student revision suggestions + teacher regenerate (additive, pre-approval, per-tenant provider)`

---

## Task 5: Phase-6 hermetic integration test

**Files:** Create `test/integration/advanced_ai_test.go`.

- [ ] **Step 1:** Reuse the harness (startInfra/startPipeline/newPhase5APIServer or the Phase-6 superset incl. curriculum/outcome/ai-provider/feedback handlers + a fake AI server). `TestAdvancedAIFlow`:
  - Create 2 outcomes; submit ≥2 submissions for one assessment_version; map questions→outcomes; drive grade→teacher_review (reuse helpers); feedback+revision present (fake provider).
  - `GET /outcome-mastery` → assert per-outcome MeanPct reflects the known marks + `gaps` names the weakest outcome.
  - Create a per-tenant `ai-provider` config (assert the stored key is encrypted, response has no key); `POST .../default`; assert `Registry.Resolve` (exercised indirectly via regenerate) uses it.
  - `POST /submissions/{id}/feedback/regenerate` on a teacher_review submission → graded.v1.json Feedback+Revision refreshed, marks UNCHANGED, audit `regenerate_feedback`; then approve → final grade equals the pre-regenerate marks (feedback/revision never touched the grade).
  - Poll for async (no fixed sleeps); SKIP_DOCKER_TESTS gate; only the AI provider is faked.
- [ ] **Step 2:** Run with Docker → PASS.
- [ ] **Step 3: Commit** — `test(integration): advanced-AI — outcomes, mastery/gaps, per-tenant provider, regenerate`

---

## Explicitly DEFERRED
- The school-admin **dashboard UI** (consumes these endpoints) — built after the wireframe prototype.
- Native **Anthropic / Gemini** provider adapters (their non-OpenAI request formats) — the registry currently builds an OpenAI-format `VLLMProvider`, which covers OpenAI, Azure OpenAI, vLLM, self-hosted, and OpenRouter-fronted Claude/Gemini. Native adapters are an additive `AIProvider` implementation + a `provider_type` switch in the registry.
- Live provider credentials + connectivity — supplied/operated by the deployer (hermetic tests use a fake provider).
- Longitudinal / cross-assessment student-progress rollups beyond per-assessment outcome mastery.

## Self-review (after writing)
- Coverage: curriculum/outcome model (T1), outcome-mastery + gaps (T2), per-tenant multi-provider gateway (T3), revision suggestions + regenerate (T4), e2e (T5). Dashboard UI + native Claude/Gemini adapters + live creds deferred.
- Invariants: AI output strictly additive (never changes marks); regenerate pre-approval only (409 on approved → snapshot immutable); gate untouched; tenant/RBAC/404-on-denial everywhere; provider keys encrypted at rest, never returned/logged; analytics read-only. sqlc v1.31.1; migrations 0009/0010 additive+reversible. Hermetic.
