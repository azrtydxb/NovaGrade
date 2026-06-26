# NovaGrade Phase 4 — Integrations (hermetic layer) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Build the integration layer so schools don't re-enter data — an integration abstraction with per-tenant **encrypted** connections, **CSV/OneRoster** roster-import + class-results export, **outbound webhooks**, and an **LMS connector** (Google Classroom) proven against a mocked API. Everything here is HERMETIC and fully testable; **live OAuth/credential wiring to real Google/Microsoft accounts is explicitly DEFERRED** to the operator (it needs their OAuth app + sandbox tokens).

**Architecture:** A `internal/integration` package defines category interfaces (RosterSource, GradeSink) and a provider registry. Connections are rows in a new `integration_connection` table with credentials **encrypted at rest** (AES-GCM, key from env). CSV/OneRoster connectors are pure parsers/formatters. Webhooks deliver HMAC-signed POSTs on pipeline events. The Google Classroom connector talks to an injectable base URL (real API in prod, httptest mock in tests). api-svc exposes integration management + roster-import + class-export endpoints (RBAC + tenant-isolated). No external network calls in any test.

**Tech Stack:** Same locked stack — Go, chi, pgx+sqlc+goose (v1.31.1), amqp091-go, minio-go, golang-jwt, testify+testcontainers; stdlib `crypto/aes`+`crypto/cipher`, `encoding/csv`, `crypto/hmac`.

## Global Constraints
- Module `github.com/azrtydxb/novagrade`; branch `feat/phase4-integrations` (off main, P1+P2+P3 merged).
- **No secrets at rest in plaintext:** integration credentials are encrypted (AES-256-GCM) with a key from env (`INTEGRATION_ENC_KEY`, base64 32 bytes); the service refuses to start if it's required and absent. Never log decrypted credentials.
- **HERMETIC:** every test runs offline — CSV via fixtures, LMS via an httptest mock server (injected base URL), webhooks via an httptest receiver. NO real external API calls, NO OAuth-app registration in code or tests. The live-credential path is config-gated and documented as operator-supplied.
- Multi-tenancy + RBAC on every new endpoint (integration authoring = `ActionEditTunables`; roster import = EditTunables/SchoolAdmin; export = `ActionViewResults`); tenant-isolated (404 cross-tenant) via the shared `fetchAndAuthorize`/principal-tenant pattern.
- Locked stack: new queries sqlc-generated at v1.31.1 (no raw pgx). TDD throughout. api-svc writes no submission state.

---

## Task 1: Integration abstraction + encrypted connection store

**Files:**
- Create: `internal/integration/connector.go` (interfaces + registry), `internal/integration/registry.go`, `internal/secrets/aesgcm.go` (+ test), `internal/store/migrations/0004_integrations.sql`, `internal/store/queries/integration.sql` (+ regen db), `internal/store/integration_store.go`
- Test: `internal/secrets/aesgcm_test.go`, `internal/store/integration_store_test.go`

**Interfaces (later tasks depend on these):**
- `secrets.Encrypt(key, plaintext []byte) ([]byte, error)` / `secrets.Decrypt(key, ciphertext []byte) ([]byte, error)` (AES-256-GCM, random nonce prepended); `secrets.KeyFromEnv(name string) ([]byte, error)`.
- `integration.Category` (`"lms"|"sis"|"roster"|"storage"`), `integration.Connection{ID, TenantID, Category, Provider, Config map[string]any, …}` (credentials NOT in this struct — fetched decrypted separately), and category interfaces:
  - `type RosterSource interface { ImportRoster(ctx, r io.Reader) ([]contracts.RosterStudent, error) }`
  - `type GradeSink interface { ExportGrades(ctx, w io.Writer, rows []contracts.GradeRow) error }`
  - A `Registry` mapping `(category, provider)` → factory; `Register`/`Get`.
- `Store.UpsertConnection(ctx, UpsertConnectionParams) (Connection, error)` — encrypts `credentials` before insert (UNIQUE per tenant+category+provider; upsert). `Store.GetConnection(ctx, tenant, id) (Connection, decryptedCreds []byte, error)`. `Store.ListConnections(ctx, tenant) ([]Connection, error)` (no creds). `Store.DeleteConnection(ctx, tenant, id) error`.

- [ ] **Step 1: secrets AES-GCM** — TDD `aesgcm_test.go` (encrypt→decrypt round-trip; wrong key fails; tampered ciphertext fails). Implement `aesgcm.go`.
- [ ] **Step 2: migration 0004** — `integration_connection` (id, tenant_id, category, provider, config jsonb, credentials bytea, status text, created_at, updated_at, UNIQUE(tenant_id, category, provider)); `webhook_subscription` table too (id, tenant_id, event text, url text, secret bytea encrypted, active bool, created_at) for Task 4. goose up/down.
- [ ] **Step 3: integration interfaces + registry** (`connector.go`/`registry.go`) — pure; TDD a registry get/register test.
- [ ] **Step 4: store + sqlc** — TDD `integration_store_test.go` (testcontainers): Upsert encrypts (assert the stored `credentials` column != plaintext), GetConnection decrypts to original, List omits creds, Delete + ErrNotFound. Regenerate sqlc v1.31.1; commit db.
- [ ] **Step 5: Commit** — `feat(integration): abstraction + AES-GCM encrypted connection store`

---

## Task 2: CSV + OneRoster connectors (roster import + class-results export)

**Files:**
- Create: `internal/integration/csv/roster.go` (RosterSource), `internal/integration/csv/grades.go` (GradeSink), `internal/integration/oneroster/roster.go`, tests + `testdata/*.csv`
- Modify: `pkg/contracts` (add `RosterStudent{Email,FullName,ExternalID,ClassLabel}` and `GradeRow{StudentName,QuestionNo,Awarded,MaxMarks,Feedback}` if not present)

**Interfaces:**
- `csv.RosterConnector` implements `RosterSource` — parse a roster CSV (`email,full_name[,class]`) → `[]RosterStudent`; validate headers; skip/return errors on malformed rows (configurable).
- `oneroster.RosterConnector` implements `RosterSource` — parse the OneRoster CSV `users.csv` (sourcedId, username/email, givenName, familyName, role=student) → `[]RosterStudent`.
- `csv.GradeConnector` implements `GradeSink` — write class results (`student,question_no,max_marks,awarded,feedback`) to a writer.
- Register all three in the registry under `("roster","csv")`, `("roster","oneroster")`, `("sis","csv")`.

- [ ] **Step 1: TDD roster import** — fixture `testdata/roster.csv` + `testdata/oneroster_users.csv`; assert parsed `[]RosterStudent` (count, fields, role filtering for OneRoster); malformed row handling.
- [ ] **Step 2: TDD grade export** — given rows, assert exact CSV output (header + rows, escaping).
- [ ] **Step 3: Implement → PASS; register in the registry.**
- [ ] **Step 4: Commit** — `feat(integration): CSV + OneRoster roster import & CSV grade export connectors`

---

## Task 3: Integration management + roster-import + class-export API

**Files:**
- Create: `internal/api/integration_handler.go`, `internal/api/roster_handler.go` (+ tests); Modify: `cmd/api/main.go`; Store: roster upsert + class-results read methods (sqlc) if needed.

**Interfaces (all RBAC + tenant-isolated):**
- `POST /v1/integrations` (ActionEditTunables) — body `{category, provider, config, credentials}`; validates `(category,provider)` is a registered connector; `UpsertConnection` (credentials encrypted). 201 + connection (no creds).
- `GET /v1/integrations` (EditTunables) — list (no creds). `DELETE /v1/integrations/{id}`.
- `POST /v1/rosters/import` (EditTunables) — multipart CSV upload + `?provider=csv|oneroster`; runs the RosterSource connector → upserts `student` rows (+ class enrollment if class given) for the principal's tenant; returns a summary `{imported, skipped, errors[]}`. Idempotent upsert by (tenant, email).
- `GET /v1/assessment-versions/{avid}/results.csv` (ViewResults) — class-results export: gather every graded submission for that assessment version (effective/final grades), run the `GradeSink` CSV connector. 200 text/csv.

- [ ] **Step 1: TDD handler tests** (httptest + fakes): create an integration (creds stored encrypted — assert via fake store), list omits creds, unknown `(category,provider)` → 400; roster import of a fixture CSV → students upserted (idempotent on re-import); class-results CSV reflects grades; cross-tenant → 404; lacking EditTunables/ViewResults → 404/403.
- [ ] **Step 2: Implement → PASS; register routes.**
- [ ] **Step 3: Commit** — `feat(api): integration config + roster import + class-results CSV export`

---

## Task 4: Outbound webhooks (3rd-party push on pipeline events)

**Files:**
- Create: `internal/integration/webhook/sender.go` (+ test), `cmd/webhook/main.go` (or fold into an existing svc — document), `internal/api/webhook_handler.go` (subscribe/list/delete) (+ test); Store: webhook_subscription methods (sqlc, table from Task 1 migration).

**Interfaces:**
- `webhook.Sender.Deliver(ctx, sub Subscription, event Event) error` — POST JSON to `sub.URL` with header `X-NovaGrade-Signature: sha256=<hmac>` (HMAC-SHA256 of the body using the sub's secret); ret(n) with backoff on 5xx/transport error; give up → log (the orchestrator/grade events that trigger this are consumed from `results.q` or a webhook queue — wire delivery on `published`/`graded` events; document the trigger).
- API: `POST /v1/webhooks` (EditTunables) `{event, url}` → stores a subscription with a generated secret (returned ONCE, then encrypted at rest); `GET /v1/webhooks`; `DELETE /v1/webhooks/{id}`.

- [ ] **Step 1: TDD sender** — an httptest receiver; assert it receives the POST with a VALID HMAC signature (verify with the secret) and correct JSON body; a 500 receiver → retried then gives up (assert attempt count); secret never logged.
- [ ] **Step 2: TDD subscribe API** — create (secret returned once), list (no secret), delete; cross-tenant 404.
- [ ] **Step 3: Wire delivery** on a pipeline event (e.g. a consumer of `results.q` for `published`/`graded`, or a small webhook dispatcher). Keep it isolated; document the trigger + that failures don't block grading.
- [ ] **Step 4: Commit** — `feat(integration): HMAC-signed outbound webhooks + subscription API`

---

## Task 5: LMS connector — Google Classroom (mock-tested; live creds DEFERRED)

**Files:**
- Create: `internal/integration/lms/googleclassroom.go` (+ test with httptest mock)

**Interfaces:**
- A `googleclassroom.Connector` implementing `RosterSource` (list course students → `[]RosterStudent`) and `GradeSink` (post grades to a coursework) against the Google Classroom REST API, with an **injectable base URL** (`BaseURL` field; defaults to the real Google endpoint in prod, set to the httptest mock in tests) and a bearer token read from the connection's **decrypted credentials** (`{access_token: ...}`). Register under `("lms","google_classroom")`.
- **DEFERRED + documented:** obtaining/refreshing the OAuth access token (the operator registers a Google OAuth app and supplies tokens via `POST /v1/integrations`); no OAuth flow or real Google call is built or tested here. A `// DEFERRED(phase4-live):` comment marks the token-acquisition seam.

- [ ] **Step 1: TDD against an httptest mock** of the Classroom API: `ImportRoster` parses the mocked `students.list` JSON → `[]RosterStudent`; `ExportGrades` issues the expected PATCH/POST to the mocked coursework endpoint with the bearer token; a 401 from the mock → a clear auth error. NO real network.
- [ ] **Step 2: Implement → PASS; register.**
- [ ] **Step 3: Commit** — `feat(integration): Google Classroom LMS connector (mock-tested; live OAuth deferred)`

---

## Task 6: Phase-4 hermetic integration test

**Files:** Create `test/integration/integrations_test.go`

- [ ] **Step 1:** Reuse the harness (startInfra + the in-process API). `TestIntegrationsFlow`: (a) configure a CSV roster connection via the real API (creds encrypted); (b) import a roster CSV via `/v1/rosters/import` → students upserted; (c) subscribe a webhook to `published` pointing at an httptest receiver; (d) drive a submission through grade→approve→publish (reuse Phase-2 helpers); (e) assert the webhook receiver got a valid HMAC-signed `published` event; (f) export class-results CSV and assert it contains the graded marks. All hermetic (no external calls). SKIP_DOCKER_TESTS gate.
- [ ] **Step 2:** Run with Docker → PASS.
- [ ] **Step 3: Commit** — `test(integration): roster import + webhook delivery + class export end-to-end`

---

## Explicitly DEFERRED to the operator (NOT built/tested here)
- Real OAuth app registration + token acquisition/refresh for Google Classroom / Microsoft (the connector consumes operator-supplied tokens via encrypted connections).
- Live Microsoft Teams/Education, Moodle, Canvas, PowerSchool/Infinite Campus/Skyward connectors (the abstraction + registry make these additive later).
- SSO/OIDC login federation (that's Phase-C/identity, separate).

## Self-review (after writing)
- Coverage: abstraction + encrypted store (T1), CSV/OneRoster (T2), management+roster+export API (T3), webhooks (T4), Google Classroom mock-tested (T5), e2e (T6). Live creds deferred + documented.
- Hermetic: every test offline (fixtures/httptest mocks). No plaintext secrets at rest. Locked stack sqlc v1.31.1.
