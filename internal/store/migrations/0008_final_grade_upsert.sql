-- +goose Up
-- +goose StatementBegin

-- Add updated_at to final_grade to support UPSERT tracking.
ALTER TABLE final_grade
    ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE final_grade
    DROP COLUMN IF EXISTS updated_at;

-- +goose StatementEnd
