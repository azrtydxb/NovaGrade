-- name: InsertFinalGrade :one
INSERT INTO final_grade (
    tenant_id, submission_id,
    total, max_total, score_100,
    graded_key, approved_by, approved_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
ON CONFLICT (tenant_id, submission_id) DO UPDATE SET
    total        = EXCLUDED.total,
    max_total    = EXCLUDED.max_total,
    score_100    = EXCLUDED.score_100,
    graded_key   = EXCLUDED.graded_key,
    approved_by  = EXCLUDED.approved_by,
    approved_at  = EXCLUDED.approved_at,
    updated_at   = now()
RETURNING
    id, tenant_id, submission_id,
    total, max_total, score_100,
    graded_key, approved_by, approved_at,
    created_at;

-- name: GetFinalGrade :one
SELECT
    id, tenant_id, submission_id,
    total, max_total, score_100,
    graded_key, approved_by, approved_at,
    created_at
FROM final_grade
WHERE tenant_id     = $1
  AND submission_id = $2;
