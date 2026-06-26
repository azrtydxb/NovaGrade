-- name: GetAssessmentVersionTenantID :one
SELECT tenant_id FROM assessment_version WHERE id = $1;
