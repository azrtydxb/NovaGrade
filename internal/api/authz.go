package api

// authz.go — package-level fetch+authorize helper shared across handlers.
//
// fetchAndAuthorize centralises the RBAC + tenant-isolation + uuid-parse
// sequence that every submission handler must perform:
//
//  1. Parse the {id} URL param (400 on bad UUID).
//  2. GetSubmission — 404 on ErrNotFound, 500 on other errors.
//  3. Build domain.ResourceCtx and call domain.Can with the supplied action.
//     Returns 404 on deny (404-not-403 to prevent tenant enumeration).
//  4. Parse p.TenantID to uuid.UUID (400 on bad value).
//
// On success it returns (sub, tenantUUID, true); on any failure it writes the
// response and returns (_, _, false).  The caller must check ok before using
// the returned values.
//
// Security invariants (must not be changed without a security review):
//   - Cross-tenant access returns 404, not 403, to avoid enumeration.
//   - The action passed by the caller determines which RBAC rule is evaluated;
//     callers must supply the tightest action their endpoint requires.

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
)

// submissionGetter is the minimal store interface required by fetchAndAuthorize.
// Both ExportStore and ApprovalStore satisfy this interface.
type submissionGetter interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
}

// authzAction is a thin wrapper so callers can pass either ActionViewResults
// or ActionReviewFixApprove without importing domain directly into every
// handler file.
type authzAction = domain.Action

// Convenience aliases used by each handler to avoid repeating the domain
// package path at every call site.
const (
	actionViewResults      authzAction = domain.ActionViewResults
	actionReviewFixApprove authzAction = domain.ActionReviewFixApprove
)

// fetchAndAuthorize fetches the submission identified by the {id} URL param,
// enforces RBAC and tenant isolation, and parses the principal's tenant UUID.
//
// Parameters:
//   - w, r   — HTTP request/response (used to write error responses on failure).
//   - p      — authenticated principal from context.
//   - getter — store that exposes GetSubmission (e.g. ExportStore, ApprovalStore).
//   - action — the domain action to authorise (ActionViewResults or ActionReviewFixApprove).
//   - deployMode — "saas" or "onprem", forwarded to domain.ResourceCtx.
//
// Returns (sub, tenantUUID, true) on success; on any failure it writes the
// appropriate HTTP error and returns (zero, zero, false).
func fetchAndAuthorize(
	w http.ResponseWriter,
	r *http.Request,
	p auth.Principal,
	getter submissionGetter,
	action authzAction,
	deployMode string,
) (store.Submission, uuid.UUID, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return store.Submission{}, uuid.UUID{}, false
	}

	sub, err := getter.GetSubmission(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return store.Submission{}, uuid.UUID{}, false
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return store.Submission{}, uuid.UUID{}, false
	}

	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: sub.TenantID.String(),
		DeployMode:       deployMode,
	}
	if !domain.Can(p.Roles, action, rctx) {
		// 404, not 403 — prevents tenant enumeration.
		http.Error(w, "not found", http.StatusNotFound)
		return store.Submission{}, uuid.UUID{}, false
	}

	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return store.Submission{}, uuid.UUID{}, false
	}

	return sub, tenantID, true
}
