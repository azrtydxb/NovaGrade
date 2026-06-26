package store

// moderation_store.go — Store methods for the sampled second-marker workflow.
//
// Design notes:
//
//   - Sampling is DETERMINISTIC: submissions are picked ORDER BY id LIMIT
//     sample_size. UUID primary keys order lexicographically, so the same set
//     of submissions always yields the same sample, which makes store tests
//     predictable without random-seed shenanigans.
//
//   - A moderation mark is RECORDED FOR COMPARISON ONLY. It NEVER changes the
//     final grade. Discrepancies are actioned via the normal override/approve
//     path.
//
//   - All operations are tenant-scoped: every query filters on tenant_id and
//     returns ErrNotFound rather than a cross-tenant row.

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

// ModerationSession is the public domain representation of a moderation_session row.
type ModerationSession struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	AssessmentVersionID uuid.UUID
	CreatedBy           string
	SampleSize          int
	Status              string
	CreatedAt           time.Time
}

// ModerationMark is the public domain representation of a moderation_mark row.
// It records a second marker's per-question awarded marks for comparison only.
type ModerationMark struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	SessionID      uuid.UUID
	SubmissionID   uuid.UUID
	QuestionNo     string
	ModeratorMarks float64
	Moderator      string
	CreatedAt      time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// Params
// ─────────────────────────────────────────────────────────────────────────────

// CreateModerationSessionParams carries the fields needed to start a new
// moderation session.
type CreateModerationSessionParams struct {
	TenantID            uuid.UUID
	AssessmentVersionID uuid.UUID
	CreatedBy           string
	SampleSize          int
}

// RecordModerationMarkParams carries the fields needed to append a moderator mark.
type RecordModerationMarkParams struct {
	TenantID       uuid.UUID
	SessionID      uuid.UUID
	SubmissionID   uuid.UUID
	QuestionNo     string
	ModeratorMarks float64
	Moderator      string
}

// ─────────────────────────────────────────────────────────────────────────────
// Store methods
// ─────────────────────────────────────────────────────────────────────────────

// CreateModerationSession inserts a moderation_session and deterministically
// samples up to SampleSize submissions from the assessment version
// (ORDER BY submission.id LIMIT sample_size — reproducible, testable).
// It inserts a moderation_session_submission row for each sampled submission.
// Returns the session and the sampled submission IDs.
func (s *Store) CreateModerationSession(ctx context.Context, p CreateModerationSessionParams) (ModerationSession, []uuid.UUID, error) {
	// 1. Insert the session row.
	sess, err := s.queries.InsertModerationSession(ctx, db.InsertModerationSessionParams{
		TenantID:            p.TenantID,
		AssessmentVersionID: p.AssessmentVersionID,
		CreatedBy:           p.CreatedBy,
		SampleSize:          int32(p.SampleSize),
	})
	if err != nil {
		return ModerationSession{}, nil, fmt.Errorf("store: CreateModerationSession insert session: %w", err)
	}

	// 2. Sample submissions deterministically (ORDER BY id LIMIT).
	sampledIDs, err := s.queries.SampleSubmissionsByAssessmentVersion(ctx, db.SampleSubmissionsByAssessmentVersionParams{
		TenantID:            p.TenantID,
		AssessmentVersionID: uuid.NullUUID{UUID: p.AssessmentVersionID, Valid: true},
		Limit:               int32(p.SampleSize),
	})
	if err != nil {
		return ModerationSession{}, nil, fmt.Errorf("store: CreateModerationSession sample submissions: %w", err)
	}

	// 3. Insert moderation_session_submission rows.
	for _, subID := range sampledIDs {
		if err := s.queries.InsertModerationSessionSubmission(ctx, db.InsertModerationSessionSubmissionParams{
			SessionID:    sess.ID,
			SubmissionID: subID,
			TenantID:     p.TenantID,
		}); err != nil {
			return ModerationSession{}, nil, fmt.Errorf("store: CreateModerationSession link submission %s: %w", subID, err)
		}
	}

	return moderationSessionFromDB(sess), sampledIDs, nil
}

// RecordModerationMark appends a moderator mark for a sampled submission.
// The mark is append-only and NEVER mutates the final grade; it is stored
// for comparison purposes only.
//
// Note: This method does not validate that submission_id is in the session's
// sample — callers (the API handler) are expected to verify tenant ownership
// of the session before calling, and the session_id FK enforces session
// existence. Cross-session submission validation is left to business logic in
// the handler if stricter enforcement is desired.
func (s *Store) RecordModerationMark(ctx context.Context, p RecordModerationMarkParams) (ModerationMark, error) {
	row, err := s.queries.InsertModerationMark(ctx, db.InsertModerationMarkParams{
		TenantID:       p.TenantID,
		SessionID:      p.SessionID,
		SubmissionID:   p.SubmissionID,
		QuestionNo:     p.QuestionNo,
		ModeratorMarks: p.ModeratorMarks,
		Moderator:      p.Moderator,
	})
	if err != nil {
		return ModerationMark{}, fmt.Errorf("store: RecordModerationMark: %w", err)
	}
	return moderationMarkFromDB(row), nil
}

// GetModerationSession retrieves a moderation session by id, filtered by
// tenant. Returns ErrNotFound when no such session exists for the tenant.
func (s *Store) GetModerationSession(ctx context.Context, tenantID, id uuid.UUID) (ModerationSession, error) {
	row, err := s.queries.GetModerationSession(ctx, db.GetModerationSessionParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ModerationSession{}, fmt.Errorf("GetModerationSession %s: %w", id, ErrNotFound)
		}
		return ModerationSession{}, fmt.Errorf("store: GetModerationSession: %w", err)
	}
	return moderationSessionFromDB(row), nil
}

// ListModerationSubmissions returns the sampled submission IDs for a session,
// filtered by tenant. Returns an empty slice (not an error) if none exist.
func (s *Store) ListModerationSubmissions(ctx context.Context, tenantID, sessionID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := s.queries.ListModerationSessionSubmissions(ctx, db.ListModerationSessionSubmissionsParams{
		SessionID: sessionID,
		TenantID:  tenantID,
	})
	if err != nil {
		return nil, fmt.Errorf("store: ListModerationSubmissions: %w", err)
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return ids, nil
}

// ListModerationMarks returns all moderation marks for a session, ordered
// chronologically (oldest first). Filtered by tenant.
func (s *Store) ListModerationMarks(ctx context.Context, tenantID, sessionID uuid.UUID) ([]ModerationMark, error) {
	rows, err := s.queries.ListModerationMarks(ctx, db.ListModerationMarksParams{
		SessionID: sessionID,
		TenantID:  tenantID,
	})
	if err != nil {
		return nil, fmt.Errorf("store: ListModerationMarks: %w", err)
	}
	result := make([]ModerationMark, 0, len(rows))
	for _, r := range rows {
		result = append(result, moderationMarkFromDB(r))
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Mappers
// ─────────────────────────────────────────────────────────────────────────────

func moderationSessionFromDB(r db.ModerationSession) ModerationSession {
	return ModerationSession{
		ID:                  r.ID,
		TenantID:            r.TenantID,
		AssessmentVersionID: r.AssessmentVersionID,
		CreatedBy:           r.CreatedBy,
		SampleSize:          int(r.SampleSize),
		Status:              r.Status,
		CreatedAt:           r.CreatedAt.Time,
	}
}

func moderationMarkFromDB(r db.ModerationMark) ModerationMark {
	return ModerationMark{
		ID:             r.ID,
		TenantID:       r.TenantID,
		SessionID:      r.SessionID,
		SubmissionID:   r.SubmissionID,
		QuestionNo:     r.QuestionNo,
		ModeratorMarks: r.ModeratorMarks,
		Moderator:      r.Moderator,
		CreatedAt:      r.CreatedAt.Time,
	}
}
