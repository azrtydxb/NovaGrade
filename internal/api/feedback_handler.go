package api

// feedback_handler.go — POST /v1/submissions/{id}/feedback/regenerate
//
// Design notes:
//   - RBAC: ActionReviewFixApprove required.
//   - Tenant isolation: cross-tenant and no-permission both return 404.
//   - State guard: 409 if submission is approved, published, or exported
//     (must not mutate a finalised snapshot). Proceeds on teacher_review
//     and any other pre-approval state.
//   - Provider resolution: per-tenant via Registry.Resolve; falls back to
//     the env-configured provider if no tenant config exists.
//   - Clear+redraft: existing Feedback and Revision are cleared before
//     drafting so the regeneration is a true full refresh, not idempotent skip.
//   - Audit-first: InsertAuditEvent is called BEFORE PutObject. If audit
//     fails → 500, no artifact is written.
//   - Marks invariant: the regeneration path NEVER changes awarded_marks,
//     max_marks, Total, MaxTotal, Score100, or SectionTotals.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/pipeline"
	"github.com/azrtydxb/novagrade/internal/providers"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// FeedbackStore is the subset of store.Store required by FeedbackHandlers.
type FeedbackStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	InsertAuditEvent(ctx context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error)
}

// FeedbackHandlers holds dependencies for the feedback-regeneration HTTP handler.
type FeedbackHandlers struct {
	Store      FeedbackStore
	Objects    ObjectStore // same interface as Handlers.Objects
	Registry   *providers.Registry
	DeployMode string
}

// Regenerate handles POST /v1/submissions/{id}/feedback/regenerate.
//
// Steps (audit-first):
//  1. Auth + RBAC + tenant isolation (404 on failure).
//  2. 409 if sub.State is approved, published, or exported (immutable).
//  3. Resolve provider via Registry.Resolve(ctx, tenantID).
//  4. Load graded.v1.json → GradedPaper.
//  5. Clear Feedback and Revision on every question (true regenerate).
//  6. DraftFeedback(ctx, prov, model, paper) → updated paper.
//  7. DraftRevisionSuggestions(ctx, prov, model, paper) → updated paper.
//  8. InsertAuditEvent(action="regenerate_feedback") — audit-first, before PutObject.
//  9. PutObject updated graded.v1.json.
//  10. Return 200 + updated paper JSON; marks UNCHANGED.
func (h *FeedbackHandlers) Regenerate(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionReviewFixApprove, h.DeployMode)
	if !ok {
		return
	}

	// 409 if the submission is in a finalised state (approved, published, exported).
	// We only allow regeneration on pre-approval working artifacts.
	switch sub.State {
	case contracts.StateApproved, contracts.StatePublished, contracts.StateExported:
		http.Error(w, "submission is finalised; regeneration is not permitted", http.StatusConflict)
		return
	}

	// Resolve the per-tenant AI provider.
	prov, model := h.Registry.Resolve(r.Context(), tenantID)

	// Load the graded artifact.
	objectKey := fmt.Sprintf("%s/%s/graded.v1.json", sub.TenantID, sub.ID)
	data, err := h.Objects.GetObject(r.Context(), objectKey)
	if err != nil {
		http.Error(w, "graded artifact not available (server error)", http.StatusInternalServerError)
		return
	}

	var paper contracts.GradedPaper
	if err := json.Unmarshal(data, &paper); err != nil {
		http.Error(w, "corrupt graded artifact", http.StatusInternalServerError)
		return
	}

	// Clear existing Feedback and Revision on every question so that the
	// DraftFeedback and DraftRevisionSuggestions idempotency checks do not
	// skip them — this is a true full regeneration.
	cleared := make([]contracts.GradedQuestion, len(paper.Questions))
	copy(cleared, paper.Questions)
	for i := range cleared {
		cleared[i].Feedback = ""
		cleared[i].Revision = ""
	}
	paper.Questions = cleared

	// Draft fresh feedback.
	paper, err = pipeline.DraftFeedback(r.Context(), prov, model, paper)
	if err != nil {
		http.Error(w, "feedback generation error", http.StatusInternalServerError)
		return
	}

	// Draft fresh revision suggestions.
	paper, err = pipeline.DraftRevisionSuggestions(r.Context(), prov, model, paper)
	if err != nil {
		http.Error(w, "revision generation error", http.StatusInternalServerError)
		return
	}

	// Audit-first: write audit event BEFORE the object store write.
	subID := sub.ID
	_, err = h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subID,
		Actor:      p.ID,
		Action:     "regenerate_feedback",
		NewValue:   nil,
		Reason:     "teacher regenerated feedback",
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	// Write the updated paper back to the object store.
	updated, err := json.Marshal(paper)
	if err != nil {
		http.Error(w, "failed to marshal updated paper", http.StatusInternalServerError)
		return
	}
	if err := h.Objects.PutObject(r.Context(), objectKey, updated); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(paper)
}
