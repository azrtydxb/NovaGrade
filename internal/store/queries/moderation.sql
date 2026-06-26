-- name: InsertModerationSession :one
INSERT INTO moderation_session (
    tenant_id, assessment_version_id, created_by, sample_size, status
) VALUES (
    $1, $2, $3, $4, 'open'
)
RETURNING id, tenant_id, assessment_version_id, created_by, sample_size, status, created_at;

-- name: InsertModerationSessionSubmission :exec
INSERT INTO moderation_session_submission (session_id, submission_id, tenant_id)
VALUES ($1, $2, $3);

-- name: GetModerationSession :one
SELECT id, tenant_id, assessment_version_id, created_by, sample_size, status, created_at
FROM moderation_session
WHERE id        = $1
  AND tenant_id = $2;

-- name: ListModerationSessionSubmissions :many
SELECT submission_id
FROM moderation_session_submission
WHERE session_id = $1
  AND tenant_id  = $2
ORDER BY submission_id;

-- name: InsertModerationMark :one
INSERT INTO moderation_mark (
    tenant_id, session_id, submission_id, question_no, moderator_marks, moderator
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING id, tenant_id, session_id, submission_id, question_no, moderator_marks, moderator, created_at;

-- name: ListModerationMarks :many
SELECT id, tenant_id, session_id, submission_id, question_no, moderator_marks, moderator, created_at
FROM moderation_mark
WHERE session_id = $1
  AND tenant_id  = $2
ORDER BY created_at;

-- name: SampleSubmissionsByAssessmentVersion :many
-- Deterministic sampling: ORDER BY id LIMIT sample_size.
-- Ordered by id (uuid primary key) for reproducibility — tests can pre-create
-- submissions and predict which are sampled. NOT random, enabling deterministic tests.
SELECT id
FROM submission
WHERE tenant_id              = $1
  AND assessment_version_id  = $2
ORDER BY id
LIMIT $3;
