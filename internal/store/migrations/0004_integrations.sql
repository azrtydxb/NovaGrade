-- +goose Up
CREATE TABLE integration_connection (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    category    text        NOT NULL,
    provider    text        NOT NULL,
    config      jsonb       NOT NULL DEFAULT '{}',
    credentials bytea,
    status      text        NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, category, provider)
);

CREATE TABLE webhook_subscription (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    event       text        NOT NULL,
    url         text        NOT NULL,
    secret      bytea       NOT NULL,
    active      bool        NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS webhook_subscription;
DROP TABLE IF EXISTS integration_connection;
