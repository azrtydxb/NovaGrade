-- name: UpsertStudent :one
INSERT INTO student (id, tenant_id, email, full_name)
VALUES (gen_random_uuid(), $1, $2, $3)
ON CONFLICT (tenant_id, email)
DO UPDATE SET full_name = EXCLUDED.full_name
RETURNING id, tenant_id, email, full_name, created_at;

-- name: GetStudent :one
SELECT id, tenant_id, email, full_name, created_at
FROM student
WHERE tenant_id = $1 AND id = $2;
