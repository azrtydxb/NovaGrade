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
