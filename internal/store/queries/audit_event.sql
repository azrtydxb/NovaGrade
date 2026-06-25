-- name: InsertAuditEvent :one
INSERT INTO audit_event (
    tenant_id, entity_type, entity_id,
    actor, action, old_value, new_value, reason
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING
    id, tenant_id, entity_type, entity_id,
    actor, action, old_value, new_value, reason,
    created_at;

-- name: ListAuditEventsBySubmission :many
-- Returns all audit_event rows for a specific submission (entity_id) scoped
-- to the given tenant, ordered chronologically (oldest first).
-- Only rows with entity_type = 'submission' are returned.
SELECT
    id, tenant_id, entity_type, entity_id,
    actor, action, old_value, new_value, reason,
    created_at
FROM audit_event
WHERE tenant_id = $1
  AND entity_type = 'submission'
  AND entity_id = $2
ORDER BY created_at ASC;
