package store

// appeal_store.go — Store methods for the appeal / regrade workflow.
//
// Design notes:
//
//   - All operations are tenant-scoped: every query filters on tenant_id and
//     returns ErrNotFound rather than a cross-tenant row.
//
//   - ListAppeals accepts an empty status string to return all appeals for the
//     tenant. The generated query uses ($2 = '' OR status = $2) so an empty
//     string is a passthrough.
//
//   - UpdateAppealStatus returns ErrNotFound if zero rows were updated (the
//     appeal does not exist for the given tenant).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/azrtydxb/novagrade/internal/store/db"
)

// ─────────────────────────────────────────────────────────────────────────────
// Domain types
// ─────────────────────────────────────────────────────────────────────────────

// Appeal is the public domain representation of an appeal row.
type Appeal struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	SubmissionID uuid.UUID
	Status       string
	Reason       string
	RequestedBy  string
	Resolution   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// Params
// ─────────────────────────────────────────────────────────────────────────────

// CreateAppealParams carries the fields needed to open a new appeal.
type CreateAppealParams struct {
	TenantID     uuid.UUID
	SubmissionID uuid.UUID
	Reason       string
	RequestedBy  string
}

// ─────────────────────────────────────────────────────────────────────────────
// Store methods
// ─────────────────────────────────────────────────────────────────────────────

// CreateAppeal inserts a new appeal row with status "open" and returns it.
func (s *Store) CreateAppeal(ctx context.Context, p CreateAppealParams) (Appeal, error) {
	row, err := s.queries.InsertAppeal(ctx, db.InsertAppealParams{
		TenantID:     p.TenantID,
		SubmissionID: p.SubmissionID,
		Reason:       p.Reason,
		RequestedBy:  p.RequestedBy,
	})
	if err != nil {
		return Appeal{}, fmt.Errorf("store: CreateAppeal: %w", err)
	}
	return appealFromDB(row), nil
}

// ListAppeals returns all appeals for the tenant, optionally filtered by status.
// Pass an empty string for status to return all appeals regardless of status.
func (s *Store) ListAppeals(ctx context.Context, tenantID uuid.UUID, status string) ([]Appeal, error) {
	rows, err := s.queries.ListAppeals(ctx, db.ListAppealsParams{
		TenantID: tenantID,
		Column2:  status,
	})
	if err != nil {
		return nil, fmt.Errorf("store: ListAppeals: %w", err)
	}
	result := make([]Appeal, 0, len(rows))
	for _, r := range rows {
		result = append(result, appealFromDB(r))
	}
	return result, nil
}

// GetAppeal retrieves a single appeal by id, filtered by tenant.
// Returns ErrNotFound if no such appeal exists for the given tenant.
func (s *Store) GetAppeal(ctx context.Context, tenantID, id uuid.UUID) (Appeal, error) {
	row, err := s.queries.GetAppeal(ctx, db.GetAppealParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Appeal{}, fmt.Errorf("GetAppeal %s: %w", id, ErrNotFound)
		}
		return Appeal{}, fmt.Errorf("store: GetAppeal: %w", err)
	}
	return appealFromDB(row), nil
}

// UpdateAppealStatus sets status and resolution for the given appeal (scoped to
// tenant). Returns ErrNotFound if zero rows were updated.
func (s *Store) UpdateAppealStatus(ctx context.Context, tenantID, id uuid.UUID, status, resolution string) error {
	n, err := s.queries.UpdateAppealStatus(ctx, db.UpdateAppealStatusParams{
		ID:         id,
		TenantID:   tenantID,
		Status:     status,
		Resolution: resolution,
	})
	if err != nil {
		return fmt.Errorf("store: UpdateAppealStatus: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("UpdateAppealStatus %s: %w", id, ErrNotFound)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Mapper
// ─────────────────────────────────────────────────────────────────────────────

func appealFromDB(r db.Appeal) Appeal {
	return Appeal{
		ID:           r.ID,
		TenantID:     r.TenantID,
		SubmissionID: r.SubmissionID,
		Status:       r.Status,
		Reason:       r.Reason,
		RequestedBy:  r.RequestedBy,
		Resolution:   r.Resolution,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}
