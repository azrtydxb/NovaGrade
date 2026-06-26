-- name: InsertCurriculumOutcome :one
INSERT INTO curriculum_outcome (tenant_id, code, description, subject)
VALUES ($1, $2, $3, $4)
RETURNING id, tenant_id, code, description, subject, created_at;

-- name: ListCurriculumOutcomes :many
SELECT id, tenant_id, code, description, subject, created_at
FROM curriculum_outcome
WHERE tenant_id = $1
ORDER BY code;

-- name: GetCurriculumOutcome :one
SELECT id, tenant_id, code, description, subject, created_at
FROM curriculum_outcome
WHERE id = $1 AND tenant_id = $2;

-- name: InsertQuestionOutcome :one
INSERT INTO question_outcome (tenant_id, assessment_version_id, question_no, outcome_id)
VALUES ($1, $2, $3, $4)
RETURNING id, tenant_id, assessment_version_id, question_no, outcome_id, created_at;

-- name: ListQuestionOutcomes :many
SELECT id, tenant_id, assessment_version_id, question_no, outcome_id, created_at
FROM question_outcome
WHERE tenant_id = $1 AND assessment_version_id = $2
ORDER BY question_no;
