// Package store provides storage adapters for NovaGrade. This file contains
// the Postgres database store backed by pgx/v5 connection pool and goose
// migrations.
//
// # Design notes
//
// sqlc was considered but not used because it is a code-generator that must be
// installed as a binary at development time and the binary was not available in
// this environment. Instead, the query layer is hand-written with pgx/v5 typed
// scan helpers, giving equivalent type safety without a build-time tool
// dependency. All generated code is committed so the repository builds with a
// plain `go build ./...`.
//
// Migrations are embedded with //go:embed so they travel with the binary.
package store

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// migrationFS holds the embedded SQL migration files.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// DBConfig holds connection settings for the Postgres database.
// No secrets are hardcoded; the caller always supplies them.
type DBConfig struct {
	Host     string // Postgres host name or IP
	Port     int    // Postgres port (default 5432)
	User     string // database user
	Password string // database password
	Database string // database name
	SSLMode  string // "disable", "require", "verify-full", …
}

// DSN formats a libpq-style connection string from the config.
func (c DBConfig) DSN() string {
	ssl := c.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	port := c.Port
	if port == 0 {
		port = 5432
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, port, c.User, c.Password, c.Database, ssl,
	)
}

// Store wraps a pgxpool.Pool and exposes typed repository methods for the
// NovaGrade data model. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore opens a pgxpool connection to Postgres using cfg and returns a
// *Store. It pings the database to verify connectivity before returning.
// Callers are responsible for calling Close when the store is no longer needed.
func NewStore(ctx context.Context, cfg DBConfig) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases all connections in the underlying pool.
func (s *Store) Close() {
	s.pool.Close()
}

// MigrateUp runs all pending goose migrations against the database that Store
// is connected to. Migration SQL files are embedded in the binary.
// It uses pgx's stdlib adapter to obtain a *sql.DB that goose can drive.
func (s *Store) MigrateUp(ctx context.Context) error {
	// Open a stdlib *sql.DB backed by the same pool config.
	sqlDB := stdlib.OpenDBFromPool(s.pool)
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: goose set dialect: %w", err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		return fmt.Errorf("store: goose up: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Domain types
// ─────────────────────────────────────────────────────────────────────────────

// Submission is the Go representation of the submission row.
type Submission struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	AssessmentVersionID *uuid.UUID // nullable
	StudentID           *uuid.UUID // nullable
	State               contracts.SubmissionState
	CurrentStage        *string
	Attempt             int
	ErrorDetail         *string
	SourcePDFKey        *string
	TranscriptKey       *string
	GradedKey           *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// AuditEvent is the Go representation of the audit_event row.
type AuditEvent struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	EntityType string
	EntityID   *uuid.UUID // nullable
	Actor      string
	Action     string
	OldValue   []byte // raw JSON, nil when absent
	NewValue   []byte // raw JSON, nil when absent
	Reason     string
	CreatedAt  time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// CreateSubmission params
// ─────────────────────────────────────────────────────────────────────────────

// CreateSubmissionParams carries the values needed to insert a new submission.
type CreateSubmissionParams struct {
	TenantID            uuid.UUID
	AssessmentVersionID *uuid.UUID // optional
	StudentID           *uuid.UUID // optional
	SourcePDFKey        *string    // optional
}

// CreateSubmission inserts a new submission in the "uploaded" state and returns
// the full persisted row.
func (s *Store) CreateSubmission(ctx context.Context, p CreateSubmissionParams) (Submission, error) {
	const q = `
INSERT INTO submission (
    tenant_id, assessment_version_id, student_id,
    state, attempt, source_pdf_key
) VALUES (
    $1, $2, $3,
    $4, 0, $5
)
RETURNING
    id, tenant_id, assessment_version_id, student_id,
    state, current_stage, attempt, error_detail,
    source_pdf_key, transcript_key, graded_key,
    created_at, updated_at`

	row := s.pool.QueryRow(ctx, q,
		p.TenantID,
		p.AssessmentVersionID,
		p.StudentID,
		string(contracts.StateUploaded),
		p.SourcePDFKey,
	)
	return scanSubmission(row)
}

// SetSubmissionState updates the state column (and bumps updated_at) for the
// submission with the given id.
func (s *Store) SetSubmissionState(ctx context.Context, id uuid.UUID, state contracts.SubmissionState) error {
	const q = `
UPDATE submission
   SET state      = $1,
       updated_at = now()
 WHERE id = $2`

	tag, err := s.pool.Exec(ctx, q, string(state), id)
	if err != nil {
		return fmt.Errorf("store: SetSubmissionState: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: SetSubmissionState: submission %s not found", id)
	}
	return nil
}

// GetSubmission retrieves a single submission by primary key.
func (s *Store) GetSubmission(ctx context.Context, id uuid.UUID) (Submission, error) {
	const q = `
SELECT
    id, tenant_id, assessment_version_id, student_id,
    state, current_stage, attempt, error_detail,
    source_pdf_key, transcript_key, graded_key,
    created_at, updated_at
FROM submission
WHERE id = $1`

	row := s.pool.QueryRow(ctx, q, id)
	sub, err := scanSubmission(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Submission{}, fmt.Errorf("store: GetSubmission: not found: %s", id)
		}
		return Submission{}, fmt.Errorf("store: GetSubmission: %w", err)
	}
	return sub, nil
}

// scanSubmission reads a pgx.Row into a Submission value.
func scanSubmission(row pgx.Row) (Submission, error) {
	var sub Submission
	var stateStr string
	err := row.Scan(
		&sub.ID,
		&sub.TenantID,
		&sub.AssessmentVersionID,
		&sub.StudentID,
		&stateStr,
		&sub.CurrentStage,
		&sub.Attempt,
		&sub.ErrorDetail,
		&sub.SourcePDFKey,
		&sub.TranscriptKey,
		&sub.GradedKey,
		&sub.CreatedAt,
		&sub.UpdatedAt,
	)
	if err != nil {
		return Submission{}, err
	}
	sub.State = contracts.SubmissionState(stateStr)
	return sub, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// InsertAuditEvent
// ─────────────────────────────────────────────────────────────────────────────

// InsertAuditEventParams carries the values for a new audit_event row.
type InsertAuditEventParams struct {
	TenantID   uuid.UUID
	EntityType string
	EntityID   *uuid.UUID // optional
	Actor      string
	Action     string
	OldValue   []byte // raw JSON or nil
	NewValue   []byte // raw JSON or nil
	Reason     string
}

// InsertAuditEvent appends a new row to the append-only audit_event table.
// There are no update or delete methods for audit_event.
func (s *Store) InsertAuditEvent(ctx context.Context, p InsertAuditEventParams) (AuditEvent, error) {
	const q = `
INSERT INTO audit_event (
    tenant_id, entity_type, entity_id,
    actor, action, old_value, new_value, reason
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING
    id, tenant_id, entity_type, entity_id,
    actor, action, old_value, new_value, reason,
    created_at`

	var ev AuditEvent
	var stateStr string
	_ = stateStr

	row := s.pool.QueryRow(ctx, q,
		p.TenantID,
		p.EntityType,
		p.EntityID,
		p.Actor,
		p.Action,
		nullJSON(p.OldValue),
		nullJSON(p.NewValue),
		p.Reason,
	)
	err := row.Scan(
		&ev.ID,
		&ev.TenantID,
		&ev.EntityType,
		&ev.EntityID,
		&ev.Actor,
		&ev.Action,
		&ev.OldValue,
		&ev.NewValue,
		&ev.Reason,
		&ev.CreatedAt,
	)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: InsertAuditEvent: %w", err)
	}
	return ev, nil
}

// nullJSON returns nil when b is empty so pgx sends a SQL NULL instead of an
// empty byte slice (which would fail the jsonb cast).
func nullJSON(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}
