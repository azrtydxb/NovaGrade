-- +goose Up
-- +goose StatementBegin
CREATE TABLE ai_provider_config (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL,
    name          text        NOT NULL,
    provider_type text        NOT NULL CHECK (provider_type IN ('openai','azure_openai','vllm','self_hosted')),
    base_url      text        NOT NULL,
    model         text        NOT NULL,
    api_key_enc   bytea,
    is_default    boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE(tenant_id, name)
);
CREATE UNIQUE INDEX ai_provider_one_default ON ai_provider_config(tenant_id) WHERE is_default;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS ai_provider_one_default;
DROP TABLE IF EXISTS ai_provider_config;
-- +goose StatementEnd
