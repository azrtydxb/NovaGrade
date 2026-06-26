-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- teacher_review: add Phase-2 per-question override columns
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE teacher_review
    ADD COLUMN IF NOT EXISTS question_no  text             NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS old_marks    double precision NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS new_marks    double precision NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS feedback     text             NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS comment      text             NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS actor        text             NOT NULL DEFAULT '';

-- ─────────────────────────────────────────────────────────────────────────────
-- final_grade: replace Phase-1 stub columns with Phase-2 approval snapshot
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE final_grade
    ADD COLUMN IF NOT EXISTS total        double precision NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_total    double precision NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS score_100    double precision NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS graded_key   text             NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_by  text             NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_at  timestamptz      NOT NULL DEFAULT now();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE final_grade
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS graded_key,
    DROP COLUMN IF EXISTS score_100,
    DROP COLUMN IF EXISTS max_total,
    DROP COLUMN IF EXISTS total;

ALTER TABLE teacher_review
    DROP COLUMN IF EXISTS actor,
    DROP COLUMN IF EXISTS comment,
    DROP COLUMN IF EXISTS feedback,
    DROP COLUMN IF EXISTS new_marks,
    DROP COLUMN IF EXISTS old_marks,
    DROP COLUMN IF EXISTS question_no;

-- +goose StatementEnd
