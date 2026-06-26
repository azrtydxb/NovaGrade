-- name: CreateWebhookSubscription :one
INSERT INTO webhook_subscription (tenant_id, event, url, secret)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListWebhookSubscriptions :many
SELECT id, tenant_id, event, url, active, created_at
FROM webhook_subscription
WHERE tenant_id = $1
ORDER BY created_at;

-- name: GetActiveWebhooksForEvent :many
SELECT id, tenant_id, event, url, secret, active, created_at
FROM webhook_subscription
WHERE tenant_id = $1
  AND event = $2
  AND active = true;

-- name: DeleteWebhookSubscription :execrows
DELETE FROM webhook_subscription
WHERE id = $1
  AND tenant_id = $2;
