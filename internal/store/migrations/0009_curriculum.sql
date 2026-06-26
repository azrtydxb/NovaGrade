-- +goose Up
-- +goose StatementBegin
CREATE TABLE curriculum_outcome (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL,
    code        text        NOT NULL,
    description text        NOT NULL,
    subject     text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE(tenant_id, code)
);

CREATE TABLE question_outcome (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL,
    assessment_version_id uuid        NOT NULL,
    question_no           text        NOT NULL,
    outcome_id            uuid        NOT NULL REFERENCES curriculum_outcome(id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE(tenant_id, assessment_version_id, question_no, outcome_id)
);

CREATE INDEX ON question_outcome (tenant_id, assessment_version_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS question_outcome;
DROP TABLE IF EXISTS curriculum_outcome;
-- +goose StatementEnd
