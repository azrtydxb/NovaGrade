-- +goose Up
ALTER TABLE student ADD CONSTRAINT student_tenant_email_uq UNIQUE (tenant_id, email);

-- +goose Down
ALTER TABLE student DROP CONSTRAINT student_tenant_email_uq;
