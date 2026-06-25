package api

// AuditHandlers provides the HTTP handler for the audit trail endpoint.
// It follows the same RBAC + tenant-isolation pattern as the submission
// handlers in handlers.go:
//
//   - Required Action: domain.ActionViewResults (every role that can read
//     submission results can also view the audit trail for those submissions).
//   - Tenant isolation: an actor may only list audit events for submissions
//     that belong to their tenant. Cross-tenant requests return 404 (not 403)
//     to prevent tenant enumeration, matching the GetSubmission convention.
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

// AuditHandlers holds dependencies for the audit HTTP handler.
type AuditHandlers struct {
	Audit      AuditServiceFace
	DeployMode string // "saas" or "onprem"
}

// GetAuditEvents handles GET /v1/audit?submission={id}.
//
// RBAC: requires ActionViewResults.
// Tenant isolation: the submission query is scoped to the principal's tenant;
// cross-tenant queries silently return an empty list (same as 404 for
// submissions — avoids leaking whether a submission exists in another tenant).
// Ordering: events are returned chronologically (oldest first), as produced by
// ListBySubmission → ORDER BY created_at ASC.
func (h *AuditHandlers) GetAuditEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// RBAC check — same action as GetSubmission / GetResult.
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionViewResults, rctx) {
		// Return 403 when the principal is authenticated but lacks permission.
		// (Unlike cross-tenant submissions we don't need 404 here because we
		// have not yet fetched any resource whose existence we must hide.)
		http.Error(w, "forbidden", http.StatusForbidden)
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
