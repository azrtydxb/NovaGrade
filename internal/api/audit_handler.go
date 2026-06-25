package api

// AuditHandlers provides the HTTP handler for the audit trail endpoint.
// It follows the same RBAC + tenant-isolation pattern as the submission
// handlers in handlers.go:
//
//   - Required Action: domain.ActionViewResults (every role that can read
//     submission results can also view the audit trail for those submissions).
//   - Tenant isolation: the handler first fetches the submission to verify it
//     exists AND belongs to the caller's tenant. Cross-tenant requests return
//     404 (not 403) to prevent tenant enumeration, exactly matching the
//     GetSubmission convention: fetch → RBAC+tenant check using the
//     submission's own TenantID → list events.
//
// The endpoint is append-only by design: the handler calls ONLY
// AuditService.ListBySubmission (read) — there is no create/update/delete
// path exposed here. The write path (Record) is intended for internal use
// by pipeline stages, not for HTTP callers.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
)

// AuditServiceFace is the subset of domain.AuditServiceInterface used by the
// handler. This decouples the handler from the concrete type and makes it
// testable with a fake.
type AuditServiceFace interface {
	ListBySubmission(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.AuditEvent, error)
}

// SubmissionLookup is the subset of store.Store used by AuditHandlers to
// verify submission existence and ownership before listing audit events.
type SubmissionLookup interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
}

// AuditHandlers holds dependencies for the audit HTTP handler.
type AuditHandlers struct {
	Audit      AuditServiceFace
	Store      SubmissionLookup
	DeployMode string // "saas" or "onprem"
}

// GetAuditEvents handles GET /v1/audit?submission={id}.
//
// RBAC: requires ActionViewResults.
// Tenant isolation: the handler fetches the submission first to verify it
// exists; if not found → 404. It then performs the RBAC + tenant check using
// the submission's own TenantID as ResourceTenantID (same pattern as
// GetSubmission). A cross-tenant request — or a caller who lacks
// ActionViewResults — receives 404 (not 403) to prevent tenant enumeration.
// Only after both checks pass are the audit events fetched and returned.
// Ordering: events are returned chronologically (oldest first), as produced by
// ListBySubmission → ORDER BY created_at ASC.
func (h *AuditHandlers) GetAuditEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	subIDStr := r.URL.Query().Get("submission")
	if subIDStr == "" {
		http.Error(w, "missing query parameter: submission", http.StatusBadRequest)
		return
	}
	submissionID, err := uuid.Parse(subIDStr)
	if err != nil {
		http.Error(w, "invalid submission id", http.StatusBadRequest)
		return
	}

	// Fetch the submission to verify existence and get its TenantID.
	sub, err := h.Store.GetSubmission(r.Context(), submissionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// RBAC + tenant check — use the submission's own TenantID as ResourceTenantID,
	// exactly like GetSubmission. Cross-tenant and lacking-permission both return
	// 404 to avoid leaking whether a submission exists in another tenant.
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: sub.TenantID.String(),
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionViewResults, rctx) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	events, err := h.Audit.ListBySubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// Normalise nil → empty array for consistent JSON output.
	if events == nil {
		events = []store.AuditEvent{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(events)
}
