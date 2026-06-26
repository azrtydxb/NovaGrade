package store

// curriculum_store.go — Store methods for curriculum outcomes and the mapping
// of assessment-version questions to those outcomes.
//
// Design notes:
//
//   - All operations are tenant-scoped: every query filters on tenant_id and
//     GetOutcome returns ErrNotFound rather than a cross-tenant row.
//
//   - Unique violations (Postgres SQLSTATE 23505) are surfaced as a wrapped
//     error so callers can see the constraint that was hit. No special sentinel
//     is used — duplicate codes and duplicate question→outcome mappings are
//     simply rejected by the database UNIQUE constraints.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/azrtydxb/novagrade/internal/store/db"
)

// ─────────────────────────────────────────────────────────────────────────────
// Domain types
// ─────────────────────────────────────────────────────────────────────────────

// CurriculumOutcome is the public domain representation of a curriculum_outcome
// row: a learning outcome / standard a tenant tracks.
type CurriculumOutcome struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Code        string
	Description string
	Subject     string
	CreatedAt   time.Time
}

// QuestionOutcome maps a question (by question_no, within an assessment version)
// to a curriculum outcome.
type QuestionOutcome struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	AssessmentVersionID uuid.UUID
	QuestionNo          string
	OutcomeID           uuid.UUID
	CreatedAt           time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// Params
// ─────────────────────────────────────────────────────────────────────────────

// CreateOutcomeParams carries the fields needed to create a curriculum outcome.
type CreateOutcomeParams struct {
	TenantID    uuid.UUID
	Code        string
	Description string
	Subject     string
}

// MapQuestionOutcomeParams carries the fields needed to map a question to an
// outcome within an assessment version.
type MapQuestionOutcomeParams struct {
	TenantID            uuid.UUID
	AssessmentVersionID uuid.UUID
	QuestionNo          string
	OutcomeID           uuid.UUID
}

// ─────────────────────────────────────────────────────────────────────────────
// Store methods
// ─────────────────────────────────────────────────────────────────────────────

// CreateOutcome inserts a new curriculum outcome and returns it.
// A duplicate (tenant_id, code) returns a wrapped unique-violation error.
func (s *Store) CreateOutcome(ctx context.Context, p CreateOutcomeParams) (CurriculumOutcome, error) {
	row, err := s.queries.InsertCurriculumOutcome(ctx, db.InsertCurriculumOutcomeParams{
		TenantID:    p.TenantID,
		Code:        p.Code,
		Description: p.Description,
		Subject:     p.Subject,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return CurriculumOutcome{}, fmt.Errorf("store: CreateOutcome: duplicate outcome code %q: %w", p.Code, err)
		}
		return CurriculumOutcome{}, fmt.Errorf("store: CreateOutcome: %w", err)
	}
	return outcomeFromDB(row), nil
}

// ListOutcomes returns all curriculum outcomes for the tenant, ordered by code.
func (s *Store) ListOutcomes(ctx context.Context, tenantID uuid.UUID) ([]CurriculumOutcome, error) {
	rows, err := s.queries.ListCurriculumOutcomes(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: ListOutcomes: %w", err)
	}
	result := make([]CurriculumOutcome, 0, len(rows))
	for _, r := range rows {
		result = append(result, outcomeFromDB(r))
	}
	return result, nil
}

// GetOutcome retrieves a single outcome by id, filtered by tenant.
// Returns ErrNotFound if no such outcome exists for the given tenant.
func (s *Store) GetOutcome(ctx context.Context, tenantID, id uuid.UUID) (CurriculumOutcome, error) {
	row, err := s.queries.GetCurriculumOutcome(ctx, db.GetCurriculumOutcomeParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CurriculumOutcome{}, fmt.Errorf("GetOutcome %s: %w", id, ErrNotFound)
		}
		return CurriculumOutcome{}, fmt.Errorf("store: GetOutcome: %w", err)
	}
	return outcomeFromDB(row), nil
}

// MapQuestionOutcome maps a question to an outcome and returns the mapping.
// A duplicate (tenant_id, assessment_version_id, question_no, outcome_id)
// returns a wrapped unique-violation error.
func (s *Store) MapQuestionOutcome(ctx context.Context, p MapQuestionOutcomeParams) (QuestionOutcome, error) {
	row, err := s.queries.InsertQuestionOutcome(ctx, db.InsertQuestionOutcomeParams{
		TenantID:            p.TenantID,
		AssessmentVersionID: p.AssessmentVersionID,
		QuestionNo:          p.QuestionNo,
		OutcomeID:           p.OutcomeID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return QuestionOutcome{}, fmt.Errorf("store: MapQuestionOutcome: duplicate mapping for question %q: %w", p.QuestionNo, err)
		}
		return QuestionOutcome{}, fmt.Errorf("store: MapQuestionOutcome: %w", err)
	}
	return questionOutcomeFromDB(row), nil
}

// ListQuestionOutcomes returns all question→outcome mappings for the given
// assessment version, scoped to the tenant.
func (s *Store) ListQuestionOutcomes(ctx context.Context, tenantID, assessmentVersionID uuid.UUID) ([]QuestionOutcome, error) {
	rows, err := s.queries.ListQuestionOutcomes(ctx, db.ListQuestionOutcomesParams{
		TenantID:            tenantID,
		AssessmentVersionID: assessmentVersionID,
	})
	if err != nil {
		return nil, fmt.Errorf("store: ListQuestionOutcomes: %w", err)
	}
	result := make([]QuestionOutcome, 0, len(rows))
	for _, r := range rows {
		result = append(result, questionOutcomeFromDB(r))
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Mappers
// ─────────────────────────────────────────────────────────────────────────────

func outcomeFromDB(r db.CurriculumOutcome) CurriculumOutcome {
	return CurriculumOutcome{
		ID:          r.ID,
		TenantID:    r.TenantID,
		Code:        r.Code,
		Description: r.Description,
		Subject:     r.Subject,
		CreatedAt:   r.CreatedAt.Time,
	}
}

func questionOutcomeFromDB(r db.QuestionOutcome) QuestionOutcome {
	return QuestionOutcome{
		ID:                  r.ID,
		TenantID:            r.TenantID,
		AssessmentVersionID: r.AssessmentVersionID,
		QuestionNo:          r.QuestionNo,
		OutcomeID:           r.OutcomeID,
		CreatedAt:           r.CreatedAt.Time,
	}
}
