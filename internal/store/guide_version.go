package store

// guide_version.go — versioned, lockable marking_guide repository methods.
//
// Design notes:
//   - Version auto-increment is handled atomically in SQL via
//     COALESCE(MAX(version), 0)+1 within (tenant_id, assessment_version_id).
//     Callers never supply a version number.
//   - A locked guide is never content-mutated at this layer; callers that need
//     to edit content must create a new version (enforced at the API layer).
//   - All methods are tenant-scoped: every WHERE clause includes tenant_id.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/azrtydxb/novagrade/internal/store/db"
)

// MarkingGuide is the public domain representation of a marking_guide row.
// Content holds the raw JSONB bytes from Postgres.
type MarkingGuide struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	AssessmentVersionID uuid.UUID
	Version             int
	Name                string
	Content             []byte    // raw JSONB
	Locked              bool
	LockedAt            *time.Time
	CreatedAt           time.Time
}

// InsertGuideVersionParams carries the values needed to insert a new marking_guide version.
// The Version field is intentionally absent — it is computed atomically by the database.
type InsertGuideVersionParams struct {
	TenantID            uuid.UUID
	AssessmentVersionID uuid.UUID
	Name                string
	Content             []byte // raw JSON / jsonb
}

// InsertGuideVersion inserts a new marking_guide version for the given
// (TenantID, AssessmentVersionID). The version number is auto-incremented by
// the database (COALESCE(MAX(version),0)+1), so concurrent inserts are safe
// under the UNIQUE(tenant_id, assessment_version_id, version) constraint.
// The new row is always unlocked.
func (s *Store) InsertGuideVersion(ctx context.Context, p InsertGuideVersionParams) (MarkingGuide, error) {
	row, err := s.queries.InsertGuideVersion(ctx, db.InsertGuideVersionParams{
		TenantID:            p.TenantID,
		AssessmentVersionID: p.AssessmentVersionID,
		Name:                pgtype.Text{String: p.Name, Valid: p.Name != ""},
		Content:             nullJSON(p.Content),
	})
	if err != nil {
		return MarkingGuide{}, fmt.Errorf("store: InsertGuideVersion: %w", err)
	}
	return markingGuideFromInsertRow(row), nil
}

// GetLatestGuide returns the highest-version marking_guide row for the given
// (tenantID, assessmentVersionID). Returns ErrNotFound when no row exists.
func (s *Store) GetLatestGuide(ctx context.Context, tenantID, assessmentVersionID uuid.UUID) (MarkingGuide, error) {
	row, err := s.queries.GetLatestGuide(ctx, db.GetLatestGuideParams{
		TenantID:            tenantID,
		AssessmentVersionID: assessmentVersionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MarkingGuide{}, fmt.Errorf("GetLatestGuide tenant=%s av=%s: %w", tenantID, assessmentVersionID, ErrNotFound)
		}
		return MarkingGuide{}, fmt.Errorf("store: GetLatestGuide: %w", err)
	}
	return markingGuideFromRow(row.ID, row.TenantID, row.AssessmentVersionID, row.Version, row.Name, row.Content, row.Locked, row.LockedAt, row.CreatedAt), nil
}

// ListGuideVersions returns all marking_guide versions for the given
// (tenantID, assessmentVersionID), ordered by version DESC (newest first).
func (s *Store) ListGuideVersions(ctx context.Context, tenantID, assessmentVersionID uuid.UUID) ([]MarkingGuide, error) {
	rows, err := s.queries.ListGuideVersions(ctx, db.ListGuideVersionsParams{
		TenantID:            tenantID,
		AssessmentVersionID: assessmentVersionID,
	})
	if err != nil {
		return nil, fmt.Errorf("store: ListGuideVersions: %w", err)
	}
	result := make([]MarkingGuide, 0, len(rows))
	for _, r := range rows {
		result = append(result, markingGuideFromRow(r.ID, r.TenantID, r.AssessmentVersionID, r.Version, r.Name, r.Content, r.Locked, r.LockedAt, r.CreatedAt))
	}
	return result, nil
}

// LockGuide sets locked=true and locked_at=now() for the given guide row.
// It is tenant-scoped (filters by both tenantID and guideID).
// Returns ErrNotFound if no row was updated (guide does not exist or belongs
// to a different tenant). Re-locking an already-locked guide is idempotent.
func (s *Store) LockGuide(ctx context.Context, tenantID, guideID uuid.UUID) error {
	n, err := s.queries.LockGuide(ctx, db.LockGuideParams{
		TenantID: tenantID,
		ID:       guideID,
	})
	if err != nil {
		return fmt.Errorf("store: LockGuide: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store: LockGuide %s: %w", guideID, ErrNotFound)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Mapping helpers
// ─────────────────────────────────────────────────────────────────────────────

func markingGuideFromInsertRow(r db.InsertGuideVersionRow) MarkingGuide {
	return markingGuideFromRow(
		r.ID, r.TenantID, r.AssessmentVersionID,
		r.Version, r.Name, r.Content, r.Locked, r.LockedAt, r.CreatedAt,
	)
}

func markingGuideFromRow(
	id, tenantID, assessmentVersionID uuid.UUID,
	version int32,
	name pgtype.Text,
	content []byte,
	locked bool,
	lockedAt pgtype.Timestamptz,
	createdAt pgtype.Timestamptz,
) MarkingGuide {
	var lat *time.Time
	if lockedAt.Valid {
		t := lockedAt.Time
		lat = &t
	}
	nameStr := ""
	if name.Valid {
		nameStr = name.String
	}
	return MarkingGuide{
		ID:                  id,
		TenantID:            tenantID,
		AssessmentVersionID: assessmentVersionID,
		Version:             int(version),
		Name:                nameStr,
		Content:             content,
		Locked:              locked,
		LockedAt:            lat,
		CreatedAt:           createdAt.Time,
	}
}
