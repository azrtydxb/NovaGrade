# Task 2 Report: Review/Override API + Audit

Status: COMPLETE. Committed as `5bd1b98` on branch `feat/phase2-teacher-review`.

---

## Endpoints

| Method | Path | Action | Notes |
|--------|------|--------|-------|
| GET  | /v1/submissions/{id}/review | ActionReviewFixApprove | Returns merged paper + `locked` flag |
| PATCH | /v1/submissions/{id}/questions/{qno} | ActionReviewFixApprove | Override marks/feedback; 409 if locked |

Both endpoints are registered in `cmd/api/main.go` under `r.Route("/v1", ...)` with `auth.Middleware`.

## 409-Lock Rule

`PATCH` returns **409 Conflict** immediately after the RBAC check when `sub.State != contracts.StateTeacherReview`. No DB write occurs. `GET /review` still works (returns the paper with `"locked": true`) so teachers can see the final merged state after approval.

## Tenant Isolation

Handler fetches the submission first (`Store.GetSubmission`), then performs `domain.Can(p.Roles, ActionReviewFixApprove, ResourceCtx{ResourceTenantID: sub.TenantID})`. Cross-tenant and no-permission both return **404** (no enumeration), matching the Phase-1 `GetSubmission` convention.

## Override-Merge Logic (Latest-per-question)

`overlayReviews(paper, reviews)` in `review_handler.go`:
- `ListTeacherReviews` returns rows ordered by `created_at ASC`.
- We iterate in order; each row overwrites the question's `AwardedMarks` and `Justification` — so the **last** row per `question_no` wins.
- Rows for unknown `question_no` are silently skipped.

The PATCH handler loads graded.v1.json, overlays existing reviews, and uses the **effective** (post-overlay) value as `OldMarks` when writing the new `teacher_review` row. This ensures a second PATCH sees the first override's value as its baseline, not the original graded value.

## Audit Event Shape

Every successful `PATCH` writes one `audit_event` row via `Store.InsertAuditEvent`:

```json
{
  "tenant_id": "<uuid>",
  "entity_type": "submission",
  "entity_id": "<submission uuid>",
  "actor": "<principal id>",
  "action": "override_question",
  "old_value": { "question_no": "1", "awarded_marks": 7.0, "justification": "..." },
  "new_value": { "question_no": "1", "awarded_marks": 8.0, "justification": "..." },
  "reason": "<comment from body>"
}
```

## Marks Clamp

`newMarks` is clamped to `[0, question.MaxMarks]` before insertion. Both `teacher_review.NewMarks` and the returned `GradedQuestion.AwardedMarks` reflect the clamped value.

## Fakes Extended

`ReviewFakeStore` in `review_handler_test.go` implements `api.ReviewStore`:
- `GetSubmission` — in-memory map
- `InsertTeacherReview` — appends to `[]store.TeacherReview` slice
- `ListTeacherReviews` — filters by tenant+submission (returns in insertion order = creation-time order in tests)
- `InsertAuditEvent` — appends to `[]store.AuditEvent` slice

The existing `FakeObjectStore` (from `api_test.go`) is reused for graded artifact reads.

## TDD Evidence

**RED** (before implementation): `go test ./internal/api/...` → build failed with 10+ `undefined: api.ReviewHandlers` errors.

**GREEN** (after `review_handler.go`): `go test ./internal/api/...` → `ok  github.com/azrtydxb/novagrade/internal/api 1.018s`.

Full suite: `SKIP_DOCKER_TESTS=1 go test ./...` → all packages pass, no regressions.

## Files Changed

- `internal/api/review_handler.go` — new: ReviewHandlers, ReviewStore interface, GetReview, PatchQuestion, overlayReviews
- `internal/api/review_handler_test.go` — new: 13 test cases covering all required scenarios
- `cmd/api/main.go` — modified: instantiate ReviewHandlers, register 2 new routes

## Concerns / Notes

1. **Feedback overlay**: the `GET /review` overlay sets `Justification = teacher.Feedback` when feedback is non-empty. If a teacher sets an empty string feedback, the original justification is preserved. This is a conservative default; could be made explicit with a nullable feedback field in future.
2. **Audit failure is fatal**: `InsertAuditEvent` failure returns 500 (the override is already in teacher_review at that point). A production system should use a transaction or idempotent retry, but the current store interface doesn't expose transactions.
3. **No worktree used**: all work done directly on `feat/phase2-teacher-review`.

---

<!-- Previous task-2 report content preserved below for reference -->
# (Previous) Task 2 Report: Local Compose Infra + MinIO Object Store Wrapper

Status: COMPLETE. Committed as `73018ea` on branch `main`.

## What was implemented

1. **Local dev infrastructure** under `deploy/compose/`:
   - `docker-compose.yml` with three services: `postgres:16`, `rabbitmq:3.13-management`,
     and `minio/minio`. Each has a healthcheck. Credential/DB values are parameterized
     via `${VAR:-default}` so the file works both with and without an `.env` file.
   - `.env.example` providing dev defaults (copy to `.env` to use).

2. **Object store wrapper** in `internal/store/`:
   - `objstore.go` — `Config`, `ObjStore`, `New`, `EnsureBucket`, `Put`, `Get`,
     backed by `github.com/minio/minio-go/v7`.
   - `objstore_test.go` — hermetic tests using `testcontainers-go`; each test spins up a
     real `minio/minio` container and tears it down via `t.Cleanup`.

Package is `package store` (not `_test`) so unexported members are testable.

## Config field names and method signatures

```go
type Config struct {
    Endpoint  string // host:port of the MinIO/S3 endpoint
    AccessKey string // access key ID
    SecretKey string // secret access key
    UseSSL    bool   // whether to connect over TLS
}

type ObjStore struct { /* wraps *minio.Client */ }

func New(cfg Config) (*ObjStore, error)
func (s *ObjStore) EnsureBucket(ctx context.Context, name string) error
func (s *ObjStore) Put(ctx context.Context, bucket, key string, data []byte, contentType string) error
func (s *ObjStore) Get(ctx context.Context, bucket, key string) ([]byte, error)
```

Implementation notes:
- `New` uses `credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")` and `Secure: cfg.UseSSL`.
- `EnsureBucket` = `BucketExists` then `MakeBucket` only if absent (idempotent).
- `Put` = `PutObject` with `bytes.NewReader(data)`, length `int64(len(data))`, content type passed through.
- `Get` = `GetObject` + `io.ReadAll`, object closed via deferred `Close`.
- All errors are wrapped with `%w` and a `store:` prefix for context.

## TDD evidence

### RED (before implementation existed)
```
# github.com/azrtydxb/novagrade/internal/store
internal/store/objstore_test.go:9:2: no required module provides package github.com/stretchr/testify/require; to add it:
	go get github.com/stretchr/testify/require
FAIL	github.com/azrtydxb/novagrade/internal/store [setup failed]
FAIL
```
(Test written first; failed to compile because deps + `New`/methods did not exist.)

### GREEN (after `objstore.go` + deps)
```
--- PASS: TestObjStoreEnsureBucket (10.61s)
--- PASS: TestObjStorePutGet (0.53s)
PASS
ok  	github.com/azrtydxb/novagrade/internal/store	12.162s
```
Real `minio/minio` containers were created, exercised, and terminated within each test.
`go build ./...` exits 0; `go vet ./internal/store/...` exits 0.
`docker compose ... config -q` validates the compose file.

## Files changed (committed)

- `deploy/compose/docker-compose.yml` (new)
- `deploy/compose/.env.example` (new)
- `internal/store/objstore.go` (new)
- `internal/store/objstore_test.go` (new)
- `go.mod` (modified — added minio-go/v7, testcontainers-go, testify)
- `go.sum` (new — was absent because the module previously had no external deps)

The Makefile was NOT modified. Staging was restricted to exactly the four paths above
(`deploy/compose/`, `internal/store/`, `go.mod`, `go.sum`).

## Self-review findings

- No hardcoded secrets in Go source: credentials flow only through `Config` into `New`. The
  test's `minioadmin` literals live in test code, not production source. Correct per constraint.
- Tests are hermetic: container lifecycle is fully owned by the test via `t.Cleanup` — no
  external compose stack required to run them.
- `GetObject` in minio-go is lazy; the actual fetch/error surfaces during `io.ReadAll`. The
  current code wraps both the `GetObject` call and the read, so a missing object still returns
  a non-nil error from `Get`. Verified the happy path via test; the lazy-error nuance is handled
  by the read-path error wrap.
- Compose file parameterizes credentials with sane defaults, so `docker compose up` works with
  zero setup while still honoring an `.env` override (Postgres healthcheck uses the same
  `${POSTGRES_USER:-nova}` so it stays consistent with the configured user).

## Concerns / follow-ups (non-blocking)

- `go.mod` pins `go 1.26.3`. `go mod tidy`, build, and tests all pass on the local toolchain, so
  it is supported here; just flagging the unusually high version pin.
- Tests require Docker and pull `minio/minio`, `testcontainers/ryuk`, etc. Not suitable for
  no-Docker environments; consider a build tag or `-short` skip if a Docker-less CI lane is added.
- `Get` reads the entire object into memory via `io.ReadAll`. Fine for exam pages/JSON; if large
  binaries are stored later, a streaming `GetReader`-style method may be warranted.
- No `DeleteObject`/`List`/presigned-URL helpers yet — add when downstream tasks need them.

## Fix: Docker-skip guard

Added graceful test skip when Docker is unavailable.

### Change

In `internal/store/objstore_test.go`:
- Added `os` import
- Added skip guard at the top of `testMinioCfg(t)`:
```go
if os.Getenv("SKIP_DOCKER_TESTS") != "" || testing.Short() {
    t.Skip("requires Docker (set SKIP_DOCKER_TESTS to skip, or omit -short)")
}
```

### Test Results

**1. Skip run** (`SKIP_DOCKER_TESTS=1 go test ./internal/store/... -run TestObjStore -v`):
```
=== RUN   TestObjStoreEnsureBucket
    objstore_test.go:52: requires Docker (set SKIP_DOCKER_TESTS to skip, or omit -short)
--- SKIP: TestObjStoreEnsureBucket (0.00s)
=== RUN   TestObjStorePutGet
    objstore_test.go:58: requires Docker (set SKIP_DOCKER_TESTS to skip, or omit -short)
--- SKIP: TestObjStorePutGet (0.00s)
PASS
ok  	github.com/azrtydxb/novagrade/internal/store	0.937s
```

**2. Pass run** (`go test ./internal/store/... -run TestObjStore -v`):
```
--- PASS: TestObjStoreEnsureBucket (1.97s)
--- PASS: TestObjStorePutGet (0.52s)
PASS
ok  	github.com/azrtydxb/novagrade/internal/store	2.814s
```

### Commit

- **SHA**: `1c78d40`
- **Message**: `test: skip object-store container tests when Docker unavailable`
