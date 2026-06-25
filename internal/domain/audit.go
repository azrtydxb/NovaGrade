// Package domain contains the business logic for NovaGrade.
// This file defines the audit domain layer: a thin, append-only service
// wrapping the store's InsertAuditEvent + ListAuditEventsBySubmission.
//
// # Design decisions
//
//   - Action choice: the audit endpoint reuses domain.ActionViewResults.
//     Rationale: every role that legitimately reads submission results (Teacher,
//     Reviewer, SchoolAdmin, GroupAdmin, Operator) also has the right to view
//     the audit trail of those submissions. A separate AuditView action would
//     duplicate the permission matrix without adding security value.
//
//   - Append-only guarantee: AuditStore exposes ONLY InsertAuditEvent (write)
//     and ListAuditEventsBySubmission (read). There is no Update or Delete
//     method. The SQL query layer (sqlc-generated) also has no UPDATE/DELETE
//     statement for audit_event. The service therefore cannot mutate or remove
//     existing rows.
//
//   - Ordering: ListBySubmission returns events oldest-first (ORDER BY
//     created_at ASC). This matches the chronological narrative of an audit
//     trail and is consistent with how audit logs are typically consumed.
package domain

import (
	"context"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Interfaces
// ─────────────────────────────────────────────────────────────────────────────

// AuditStore is the subset of store.Store used by AuditService.
// It deliberately exposes ONLY insert (write) and list (read): no update,
// no delete — enforcing the append-only contract at the interface boundary.
type AuditStore interface {
	InsertAuditEvent(ctx context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error)
	ListAuditEventsBySubmission(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.AuditEvent, error)
}

// AuditServiceInterface is the public surface of AuditService. It is
// intentionally narrow: Record (insert-only) + ListBySubmission (read-only).
// The compiler enforces that no Update or Delete method is added without
// updating this interface.
type AuditServiceInterface interface {
	Record(ctx context.Context, p RecordParams) (store.AuditEvent, error)
	ListBySubmission(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.AuditEvent, error)
}

// Compile-time assertion: *AuditService must satisfy AuditServiceInterface.
var _ AuditServiceInterface = (*AuditService)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Service
// ─────────────────────────────────────────────────────────────────────────────

// AuditService is the domain-layer service for the append-only audit trail.
type AuditService struct {
	store AuditStore
}

// NewAuditService constructs an AuditService backed by the given AuditStore.
func NewAuditService(s AuditStore) *AuditService {
	return &AuditService{store: s}
}

// RecordParams carries the values for a single audit event.
// It mirrors store.InsertAuditEventParams but lives in the domain package so
// callers do not need to import internal/store directly.
type RecordParams struct {
	TenantID   uuid.UUID
	EntityType string     // e.g. "submission"
	EntityID   *uuid.UUID // optional; use the submission UUID for submission events
	Actor      string     // who performed the action (user ID or email)
	Action     string     // what happened (e.g. "override", "approve", "publish", "retry")
	OldValue   []byte     // raw JSON snapshot before the change (nil if not applicable)
	NewValue   []byte     // raw JSON snapshot after the change (nil if not applicable)
	Reason     string     // human-readable justification
}

// Record appends a new immutable event to the audit trail.
// It is the ONLY write path — there is no corresponding Update or Delete.
func (s *AuditService) Record(ctx context.Context, p RecordParams) (store.AuditEvent, error) {
	return s.store.InsertAuditEvent(ctx, store.InsertAuditEventParams{
		TenantID:   p.TenantID,
		EntityType: p.EntityType,
		EntityID:   p.EntityID,
		Actor:      p.Actor,
		Action:     p.Action,
		OldValue:   p.OldValue,
		NewValue:   p.NewValue,
		Reason:     p.Reason,
	})
}

// ListBySubmission returns all audit events for a submission, scoped to the
// given tenant, in chronological order (oldest first).
// Cross-tenant queries always return an empty slice — the store filters by
// both tenant_id AND entity_id, so no cross-tenant leakage is possible.
func (s *AuditService) ListBySubmission(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.AuditEvent, error) {
	return s.store.ListAuditEventsBySubmission(ctx, tenantID, submissionID)
}
