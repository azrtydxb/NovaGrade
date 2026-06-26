-- name: InsertAIProviderConfig :one
INSERT INTO ai_provider_config (tenant_id, name, provider_type, base_url, model, api_key_enc)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, tenant_id, name, provider_type, base_url, model, api_key_enc, is_default, created_at;

-- name: ListAIProviderConfigs :many
SELECT id, tenant_id, name, provider_type, base_url, model, is_default, created_at
FROM ai_provider_config
WHERE tenant_id = $1
ORDER BY name;

-- name: GetDefaultAIProviderConfigWithKey :one
SELECT id, tenant_id, name, provider_type, base_url, model, api_key_enc, is_default, created_at
FROM ai_provider_config
WHERE tenant_id = $1 AND is_default = true;

-- name: ClearTenantDefaultAIProvider :exec
UPDATE ai_provider_config
SET is_default = false
WHERE tenant_id = $1 AND is_default = true;

-- name: SetAIProviderDefault :execrows
UPDATE ai_provider_config
SET is_default = true
WHERE id = $1 AND tenant_id = $2;
