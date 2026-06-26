-- name: CreateSubmission :one
INSERT INTO submission (
    tenant_id, assessment_version_id, student_id,
    state, attempt, source_pdf_key
) VALUES (
    $1, $2, $3,
    $4, 0, $5
)
RETURNING
    id, tenant_id, assessment_version_id, student_id,
    state, current_stage, attempt, error_detail,
    source_pdf_key, transcript_key, graded_key,
    created_at, updated_at;

-- name: SetSubmissionState :execrows
UPDATE submission
   SET state      = $1,
       updated_at = now()
 WHERE id = $2;

-- name: FailSubmission :execrows
UPDATE submission
   SET state         = 'failed',
       current_stage = $2,
       error_detail  = $3,
       updated_at    = now()
 WHERE id = $1;

-- name: GetSubmission :one
SELECT
    id, tenant_id, assessment_version_id, student_id,
    state, current_stage, attempt, error_detail,
    source_pdf_key, transcript_key, graded_key,
    created_at, updated_at
FROM submission
WHERE id = $1;

-- name: ListSubmissionsByState :many
-- When $2 is the empty string, no state filter is applied (all states match).
SELECT
    id, tenant_id, assessment_version_id, student_id,
    state, current_stage, attempt, error_detail,
    source_pdf_key, transcript_key, graded_key,
    created_at, updated_at
FROM submission
WHERE tenant_id = $1
  AND ($2 = '' OR state = $2)
ORDER BY created_at DESC;

-- name: SetSourcePDFKey :execrows
UPDATE submission
   SET source_pdf_key = $2,
       updated_at     = now()
 WHERE id = $1;

-- name: ListSubmissionsByAssessmentVersion :many
SELECT id, tenant_id, assessment_version_id, student_id,
    state, current_stage, attempt, error_detail,
    source_pdf_key, transcript_key, graded_key,
    created_at, updated_at
FROM submission
WHERE tenant_id = $1
  AND assessment_version_id = $2
ORDER BY created_at DESC;
