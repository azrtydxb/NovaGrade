package api

// appeal_handler.go — HTTP handlers for the appeal / regrade workflow.
//
//   POST /v1/submissions/{id}/appeals       — file an appeal (actionViewResults)
//   GET  /v1/appeals?status=               — list appeals for tenant (actionViewResults)
//   POST /v1/appeals/{id}/resolve          — resolve/reject an appeal (actionReviewFixApprove)
//   POST /v1/appeals/{id}/regrade          — trigger regrade (actionReviewFixApprove)
//
// Design notes:
//   - RBAC: FileAppeal and ListAppeals require ActionViewResults.
//           ResolveAppeal and RegradeAppeal require ActionReviewFixApprove.
//   - Tenant isolation: every operation is scoped to the caller's tenant.
//     Cross-tenant and no-permission both return 404 to prevent enumeration.
//   - Audit-first ordering: InsertAuditEvent is called before any state mutation
//     (UpdateAppealStatus, Bus.Publish). If audit fails → 500, nothing else written.
//   - The regrade endpoint publishes a command; it does NOT write submission state.
//     The orchestrator advances state via EventRegrade. NOTE: a regrade currently
//     OVERWRITES graded.v1.json (the grade worker does not yet version artifacts),
//     so the original AI graded pass is not retained across a regrade. The approved
//     final_grade row IS immutable. A regrade still requires teacher approval to
//     become final — EventRegrade targets StateGrading, and the grading stage then
//     advances to StateTeacherReview which is the hard-stop gate. FOLLOW-UP: version
//     graded artifacts (graded.v{N}) and make re-approve after a regrade UPSERT the
//     final_grade (today the idempotent-approve path returns the original final_grade,
//     so a regraded result does not become final).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// AppealStore is the subset of store.Store required by AppealHandlers.
type AppealStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	InsertAuditEvent(ctx context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error)
	CreateAppeal(ctx context.Context, p store.CreateAppealParams) (store.Appeal, error)
	ListAppeals(ctx context.Context, tenantID uuid.UUID, status string) ([]store.Appeal, error)
	GetAppeal(ctx context.Context, tenantID, id uuid.UUID) (store.Appeal, error)
	UpdateAppealStatus(ctx context.Context, tenantID, id uuid.UUID, status, resolution string) error
}

// AppealHandlers holds dependencies for the appeal / regrade HTTP handlers.
type AppealHandlers struct {
	Store      AppealStore
	Bus        CommandBus
	DeployMode string
}

// ─────────────────────────────────────────────────────────────────────────────
// FileAppeal — POST /v1/submissions/{id}/appeals
// ─────────────────────────────────────────────────────────────────────────────

// fileAppealBody is the JSON body for POST /v1/submissions/{id}/appeals.
type fileAppealBody struct {
	Reason string `json:"reason"`
}

// FileAppeal handles POST /v1/submissions/{id}/appeals.
//
// Steps:
//  1. Auth + RBAC (ActionViewResults) + tenant isolation.
//  2. Decode body {reason}.
//  3. CreateAppeal — status defaults to "open".
//  4. Return 201 + JSON appeal.
func (h *AppealHandlers) FileAppeal(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionViewResults, h.DeployMode)
	if !ok {
		return
	}

	var body fileAppealBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Reason == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}

	// Audit-first: write audit event before creating the appeal.
	subIDPtr := sub.ID
	_, err := h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subIDPtr,
		Actor:      p.ID,
		Action:     "appeal_file",
		Reason:     body.Reason,
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	a, err := h.Store.CreateAppeal(r.Context(), store.CreateAppealParams{
		TenantID:     tenantID,
		SubmissionID: sub.ID,
		Reason:       body.Reason,
		RequestedBy:  p.ID,
	})
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(a)
}

// ─────────────────────────────────────────────────────────────────────────────
// ListAppeals — GET /v1/appeals?status=
// ─────────────────────────────────────────────────────────────────────────────

// ListAppeals handles GET /v1/appeals?status=<status>.
//
// Steps:
//  1. Auth + RBAC (ActionViewResults). Tenant from principal (no submission lookup).
//  2. ListAppeals(tenantID, status) — empty status returns all.
//  3. Return 200 + JSON array.
func (h *AppealHandlers) ListAppeals(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionViewResults, rctx) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	status := r.URL.Query().Get("status")
	appeals, err := h.Store.ListAppeals(r.Context(), tenantID, status)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if appeals == nil {
		appeals = []store.Appeal{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(appeals)
}

// ─────────────────────────────────────────────────────────────────────────────
// ResolveAppeal — POST /v1/appeals/{id}/resolve
// ─────────────────────────────────────────────────────────────────────────────

// resolveAppealBody is the JSON body for POST /v1/appeals/{id}/resolve.
type resolveAppealBody struct {
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
}

// ResolveAppeal handles POST /v1/appeals/{id}/resolve.
//
// Steps (audit-first):
//  1. Auth + RBAC (ActionReviewFixApprove) + parse tenantID from principal.
//  2. Parse appeal {id} URL param.
//  3. GetAppeal — 404 if ErrNotFound.
//  4. InsertAuditEvent(action="appeal_resolve") — 500 if fails, nothing else written.
//  5. UpdateAppealStatus.
//  6. Return 200.
func (h *AppealHandlers) ResolveAppeal(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	tenantID, ok := h.authorizeReviewFixApprove(w, p)
	if !ok {
		return
	}

	appealID, ok := parseAppealID(w, r)
	if !ok {
		return
	}

	appeal, err := h.Store.GetAppeal(r.Context(), tenantID, appealID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	var body resolveAppealBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Status != "resolved" && body.Status != "rejected" {
		http.Error(w, "status must be 'resolved' or 'rejected'", http.StatusBadRequest)
		return
	}

	// Audit-first.
	appealIDPtr := appealID
	_, err = h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "appeal",
		EntityID:   &appealIDPtr,
		Actor:      p.ID,
		Action:     "appeal_resolve",
		Reason:     "appeal resolved by reviewer",
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	if err := h.Store.UpdateAppealStatus(r.Context(), tenantID, appeal.ID, body.Status, body.Resolution); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ─────────────────────────────────────────────────────────────────────────────
// RegradeAppeal — POST /v1/appeals/{id}/regrade
// ─────────────────────────────────────────────────────────────────────────────

// RegradeAppeal handles POST /v1/appeals/{id}/regrade.
//
// Steps (audit-first):
//  1. Auth + RBAC (ActionReviewFixApprove) + parse tenantID from principal.
//  2. Parse appeal {id} URL param.
//  3. GetAppeal — 404 if ErrNotFound.
//  4. InsertAuditEvent(action="appeal_regrade") — 500 if fails, nothing else written.
//  5. Publish Envelope{Stage: contracts.StageRegrade, SubmissionID: ...} to commands.q.
//  6. UpdateAppealStatus(status="under_review", resolution="").
//  7. Return 200.
//
// The handler does NOT modify submission state directly. The orchestrator advances
// the submission state via EventRegrade (approved/published/exported → grading).
// NOTE: a regrade currently OVERWRITES graded.v1.json (the grade worker does not yet
// version artifacts), so the original AI graded pass is not retained across a regrade.
// The approved final_grade row IS immutable. FOLLOW-UP: version graded artifacts
// (graded.v{N}) and make re-approve after a regrade UPSERT the final_grade (today the
// idempotent-approve path returns the original final_grade, so a regraded result does
// not become final). After regrading, the result must still pass through the teacher-approval gate.
func (h *AppealHandlers) RegradeAppeal(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	tenantID, ok := h.authorizeReviewFixApprove(w, p)
	if !ok {
		return
	}

	appealID, ok := parseAppealID(w, r)
	if !ok {
		return
	}

	appeal, err := h.Store.GetAppeal(r.Context(), tenantID, appealID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// Audit-first: write audit event before any command or state mutation.
	appealIDPtr := appealID
	_, err = h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "appeal",
		EntityID:   &appealIDPtr,
		Actor:      p.ID,
		Action:     "appeal_regrade",
		Reason:     "regrade requested via appeal",
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	// Publish regrade command — the orchestrator will drive EventRegrade through
	// the state machine (approved/published/exported → grading). It does NOT bypass
	// the teacher-approval gate: grading → teacher_review still requires explicit approval.
	env := contracts.Envelope{
		TenantID:      tenantID.String(),
		Principal:     p.ID,
		SubmissionID:  appeal.SubmissionID.String(),
		Stage:         contracts.StageRegrade,
		CorrelationID: uuid.New().String(),
	}
	if err := h.Bus.Publish(r.Context(), "commands.q", env); err != nil {
		http.Error(w, "bus error", http.StatusInternalServerError)
		return
	}

	// Mark the appeal as under review.
	if err := h.Store.UpdateAppealStatus(r.Context(), tenantID, appeal.ID, "under_review", ""); err != nil {
		// Log but don't fail the request — the command was already published.
		// A retry will find the appeal in its old status and may re-publish; the
		// orchestrator's idempotency guard makes that safe.
		http.Error(w, "store error (update appeal status)", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// authorizeReviewFixApprove extracts the principal's tenantID and validates
// that the caller has ActionReviewFixApprove. Returns (tenantID, true) on
// success; writes the response and returns (zero, false) on failure.
func (h *AppealHandlers) authorizeReviewFixApprove(w http.ResponseWriter, p auth.Principal) (uuid.UUID, bool) {
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return uuid.UUID{}, false
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionReviewFixApprove, rctx) {
		http.Error(w, "not found", http.StatusNotFound)
		return uuid.UUID{}, false
	}
	return tenantID, true
}

// parseAppealID extracts and parses the {id} URL parameter.
// Writes 400 and returns false on failure.
func parseAppealID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid appeal id", http.StatusBadRequest)
		return uuid.UUID{}, false
	}
	return id, true
}
