-- name: InsertAppeal :one
INSERT INTO appeal (tenant_id, submission_id, reason, requested_by)
VALUES ($1, $2, $3, $4)
RETURNING id, tenant_id, submission_id, status, reason, requested_by, resolution, created_at, updated_at;

-- name: ListAppeals :many
SELECT id, tenant_id, submission_id, status, reason, requested_by, resolution, created_at, updated_at
FROM appeal
WHERE tenant_id = $1
  AND ($2 = '' OR status = $2)
ORDER BY created_at DESC;

-- name: GetAppeal :one
SELECT id, tenant_id, submission_id, status, reason, requested_by, resolution, created_at, updated_at
FROM appeal
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateAppealStatus :execrows
UPDATE appeal
SET status = $3, resolution = $4, updated_at = now()
WHERE id = $1 AND tenant_id = $2;
