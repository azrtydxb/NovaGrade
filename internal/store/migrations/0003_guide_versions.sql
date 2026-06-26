-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- marking_guide: add versioning and locking columns
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE marking_guide
    ADD COLUMN IF NOT EXISTS version    int         NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS name       text,
    ADD COLUMN IF NOT EXISTS locked     bool        NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS locked_at  timestamptz;

ALTER TABLE marking_guide
    ADD CONSTRAINT marking_guide_tenant_av_version_unique
        UNIQUE (tenant_id, assessment_version_id, version);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE marking_guide
    DROP CONSTRAINT IF EXISTS marking_guide_tenant_av_version_unique;

ALTER TABLE marking_guide
    DROP COLUMN IF EXISTS locked_at,
    DROP COLUMN IF EXISTS locked,
    DROP COLUMN IF EXISTS name,
    DROP COLUMN IF EXISTS version;

-- +goose StatementEnd
