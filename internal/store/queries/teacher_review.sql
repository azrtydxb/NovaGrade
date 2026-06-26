-- name: InsertTeacherReview :one
INSERT INTO teacher_review (
    tenant_id, submission_id,
    question_no, old_marks, new_marks,
    feedback, comment, actor
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING
    id, tenant_id, submission_id,
    question_no, old_marks, new_marks,
    feedback, comment, actor,
    created_at;

-- name: ListTeacherReviews :many
SELECT
    id, tenant_id, submission_id,
    question_no, old_marks, new_marks,
    feedback, comment, actor,
    created_at
FROM teacher_review
WHERE tenant_id     = $1
  AND submission_id = $2
ORDER BY created_at ASC, id ASC;
