-- name: UpsertIntegrationConnection :one
INSERT INTO integration_connection (tenant_id, category, provider, config, credentials, status)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, category, provider)
DO UPDATE SET
    config      = EXCLUDED.config,
    credentials = EXCLUDED.credentials,
    status      = EXCLUDED.status,
    updated_at  = now()
RETURNING *;

-- name: GetIntegrationConnectionWithCreds :one
SELECT *
FROM integration_connection
WHERE id = $1
  AND tenant_id = $2;

-- name: ListIntegrationConnections :many
SELECT id, tenant_id, category, provider, config, status, created_at, updated_at
FROM integration_connection
WHERE tenant_id = $1
ORDER BY created_at;

-- name: DeleteIntegrationConnection :execrows
DELETE FROM integration_connection
WHERE id = $1
  AND tenant_id = $2;
