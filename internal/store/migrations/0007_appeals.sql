-- +goose Up
-- +goose StatementBegin
CREATE TABLE appeal (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES school(id),
    submission_id  uuid        NOT NULL,
    status         text        NOT NULL DEFAULT 'open' CHECK (status IN ('open','under_review','resolved','rejected')),
    reason         text        NOT NULL,
    requested_by   text        NOT NULL,
    resolution     text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS appeal;
-- +goose StatementEnd
