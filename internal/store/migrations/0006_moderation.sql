-- +goose Up
-- +goose StatementBegin

-- moderation_session: one sampling session per assessment version.
CREATE TABLE moderation_session (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES school(id),
    assessment_version_id uuid        NOT NULL,
    created_by            text        NOT NULL,
    sample_size           int         NOT NULL,
    status                text        NOT NULL DEFAULT 'open',
    created_at            timestamptz NOT NULL DEFAULT now()
);

-- moderation_mark: append-only record of each moderator's per-question mark.
-- A moderation mark is RECORDED FOR COMPARISON ONLY — it NEVER changes the
-- final grade. Discrepancies are actioned via the normal override/approve path.
CREATE TABLE moderation_mark (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES school(id),
    session_id      uuid        NOT NULL REFERENCES moderation_session(id),
    submission_id   uuid        NOT NULL,
    question_no     text        NOT NULL,
    moderator_marks float8      NOT NULL,
    moderator       text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- moderation_session_submission: the sampled submissions for a session.
CREATE TABLE moderation_session_submission (
    session_id    uuid NOT NULL REFERENCES moderation_session(id),
    submission_id uuid NOT NULL,
    tenant_id     uuid NOT NULL REFERENCES school(id),
    PRIMARY KEY (session_id, submission_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS moderation_session_submission;
DROP TABLE IF EXISTS moderation_mark;
DROP TABLE IF EXISTS moderation_session;
-- +goose StatementEnd
