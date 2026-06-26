-- name: InsertGuideVersion :one
-- Inserts a new marking_guide version. The version number is computed atomically
-- as COALESCE(MAX(version), 0) + 1 within the (tenant_id, assessment_version_id)
-- scope, so callers never supply it — it is always auto-incremented by the DB.
INSERT INTO marking_guide (
    tenant_id, assessment_version_id, name, content, version
)
SELECT
    $1,
    $2,
    $3,
    $4,
    COALESCE((
        SELECT MAX(version)
        FROM marking_guide
        WHERE tenant_id             = $1
          AND assessment_version_id = $2
    ), 0) + 1
RETURNING
    id, tenant_id, assessment_version_id, version, name,
    content, locked, locked_at, created_at;

-- name: GetLatestGuide :one
-- Returns the highest-version marking_guide row for the given tenant + assessment_version.
SELECT
    id, tenant_id, assessment_version_id, version, name,
    content, locked, locked_at, created_at
FROM marking_guide
WHERE tenant_id             = $1
  AND assessment_version_id = $2
ORDER BY version DESC
LIMIT 1;

-- name: ListGuideVersions :many
-- Returns all marking_guide versions for a tenant + assessment_version, newest first.
SELECT
    id, tenant_id, assessment_version_id, version, name,
    content, locked, locked_at, created_at
FROM marking_guide
WHERE tenant_id             = $1
  AND assessment_version_id = $2
ORDER BY version DESC;

-- name: LockGuide :execrows
-- Sets locked=true and locked_at=now() for the given guide. Tenant-scoped.
UPDATE marking_guide
   SET locked    = true,
       locked_at = COALESCE(locked_at, now())
 WHERE tenant_id = $1
   AND id        = $2;
