-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- school: tenant root — every other table's tenant_id FKs here
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE school (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    slug        text        NOT NULL UNIQUE,
    country     text        NOT NULL DEFAULT '',
    timezone    text        NOT NULL DEFAULT 'UTC',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Phase-1 footings (minimal: id, tenant_id, key cols, timestamps)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE academic_year (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    label       text        NOT NULL,
    start_date  date        NOT NULL,
    end_date    date        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE term (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES school(id),
    academic_year_id uuid       NOT NULL REFERENCES academic_year(id),
    label           text        NOT NULL,
    start_date      date        NOT NULL,
    end_date        date        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE department (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    name        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE course (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES school(id),
    department_id uuid        REFERENCES department(id),
    code          text        NOT NULL,
    name          text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE class (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    course_id   uuid        REFERENCES course(id),
    label       text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE teacher (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    email       text        NOT NULL,
    full_name   text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE student (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    email       text        NOT NULL,
    full_name   text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principal (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    email       text        NOT NULL UNIQUE,
    full_name   text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE role (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    name        text        NOT NULL,
    permissions jsonb       NOT NULL DEFAULT '[]',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_key (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    key_hash    text        NOT NULL UNIQUE,
    description text        NOT NULL DEFAULT '',
    expires_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- assessment chain
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE assessment (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    course_id   uuid        REFERENCES course(id),
    title       text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE assessment_version (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES school(id),
    assessment_id  uuid        NOT NULL REFERENCES assessment(id),
    version_number int         NOT NULL DEFAULT 1,
    published_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE question (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES school(id),
    assessment_version_id uuid        NOT NULL REFERENCES assessment_version(id),
    sequence              int         NOT NULL,
    stem                  text        NOT NULL DEFAULT '',
    max_marks             numeric     NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE marking_guide (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES school(id),
    assessment_version_id uuid        NOT NULL REFERENCES assessment_version(id),
    content               jsonb       NOT NULL DEFAULT '{}',
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE rubric (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES school(id),
    assessment_version_id uuid        NOT NULL REFERENCES assessment_version(id),
    content               jsonb       NOT NULL DEFAULT '{}',
    created_at            timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- submission — fully exercised in Phase 1
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE submission (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES school(id),
    assessment_version_id uuid        REFERENCES assessment_version(id),
    student_id            uuid        REFERENCES student(id),
    -- pipeline state
    state                 text        NOT NULL DEFAULT 'uploaded',
    current_stage         text,
    attempt               int         NOT NULL DEFAULT 0,
    error_detail          text,
    -- artifact refs (S3/MinIO keys)
    source_pdf_key        text,
    transcript_key        text,
    graded_key            text,
    -- timestamps
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- downstream pipeline tables (footings)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE page_image (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES school(id),
    submission_id uuid        NOT NULL REFERENCES submission(id),
    page_number   int         NOT NULL,
    storage_key   text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transcription (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES school(id),
    submission_id uuid        NOT NULL REFERENCES submission(id),
    content       jsonb       NOT NULL DEFAULT '{}',
    model         text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ai_grading_result (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES school(id),
    submission_id uuid        NOT NULL REFERENCES submission(id),
    content       jsonb       NOT NULL DEFAULT '{}',
    model         text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE teacher_review (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES school(id),
    submission_id uuid        NOT NULL REFERENCES submission(id),
    teacher_id    uuid        REFERENCES teacher(id),
    notes         text        NOT NULL DEFAULT '',
    reviewed_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE final_grade (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES school(id),
    submission_id uuid        NOT NULL REFERENCES submission(id),
    marks         numeric     NOT NULL DEFAULT 0,
    percentage    numeric,
    grade_letter  text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE feedback (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES school(id),
    submission_id uuid        NOT NULL REFERENCES submission(id),
    content       text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_event — append-only
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE audit_event (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    entity_type text        NOT NULL DEFAULT '',
    entity_id   uuid,
    actor       text        NOT NULL DEFAULT '',
    action      text        NOT NULL DEFAULT '',
    old_value   jsonb,
    new_value   jsonb,
    reason      text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- tunables: per-tenant key/value configuration knobs
CREATE TABLE tunables (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES school(id),
    key         text        NOT NULL,
    value       jsonb       NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, key)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS tunables;
DROP TABLE IF EXISTS audit_event;
DROP TABLE IF EXISTS feedback;
DROP TABLE IF EXISTS final_grade;
DROP TABLE IF EXISTS teacher_review;
DROP TABLE IF EXISTS ai_grading_result;
DROP TABLE IF EXISTS transcription;
DROP TABLE IF EXISTS page_image;
DROP TABLE IF EXISTS submission;
DROP TABLE IF EXISTS rubric;
DROP TABLE IF EXISTS marking_guide;
DROP TABLE IF EXISTS question;
DROP TABLE IF EXISTS assessment_version;
DROP TABLE IF EXISTS assessment;
DROP TABLE IF EXISTS api_key;
DROP TABLE IF EXISTS role;
DROP TABLE IF EXISTS principal;
DROP TABLE IF EXISTS student;
DROP TABLE IF EXISTS teacher;
DROP TABLE IF EXISTS class;
DROP TABLE IF EXISTS course;
DROP TABLE IF EXISTS department;
DROP TABLE IF EXISTS term;
DROP TABLE IF EXISTS academic_year;
DROP TABLE IF EXISTS school;
-- +goose StatementEnd
