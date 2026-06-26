package api

// ApprovalHandlers provides the HTTP handlers for the teacher approval endpoints:
//
//   POST /v1/submissions/{id}/approve
//   POST /v1/submissions/{id}/publish
//   POST /v1/submissions/{id}/export
//
// Design notes:
//   - RBAC: ActionReviewFixApprove required for all three endpoints.
//   - Tenant isolation: the handler first fetches the submission to verify it
//     exists and belongs to the caller's tenant. Cross-tenant and no-permission
//     both return 404 to prevent tenant enumeration.
//   - State guards: 409 if state != expected (teacher_review/approved/published).
//   - Audit-first ordering: InsertAuditEvent is called BEFORE InsertFinalGrade
//     and before publishing the command. If InsertAuditEvent fails → 500, no
//     other writes are performed.
//   - The api-svc does NOT write submission STATE — it publishes the command;
//     the orchestrator advances state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ApprovalStore is the subset of store.Store required by ApprovalHandlers.
type ApprovalStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	GetFinalGrade(ctx context.Context, tenantID uuid.UUID, submissionID uuid.UUID) (store.FinalGrade, error)
	InsertAuditEvent(ctx context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error)
	InsertFinalGrade(ctx context.Context, p store.InsertFinalGradeParams) (store.FinalGrade, error)
	ListTeacherReviews(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.TeacherReview, error)
}

// ApprovalHandlers holds dependencies for the approve/publish/export HTTP handlers.
type ApprovalHandlers struct {
	Store      ApprovalStore
	Objects    ObjectStore // same interface as Handlers.Objects / ReviewHandlers.Objects
	Bus        CommandBus  // same interface as Handlers.Bus
	DeployMode string      // "saas" or "onprem"
}

// ─────────────────────────────────────────────────────────────────────────────
// Approve — POST /v1/submissions/{id}/approve
// ─────────────────────────────────────────────────────────────────────────────

// Approve handles POST /v1/submissions/{id}/approve.
//
// Steps (audit-first):
//  1. Auth + RBAC + tenant isolation (404 on failure).
//  2. 409 if sub.State != teacher_review.
//  3. GetFinalGrade — if EXISTS, take idempotent path (re-publish only).
//  4. Load graded.v1.json + overlay teacher overrides → effective GradedPaper (500 if missing).
//  5. InsertAuditEvent(action="approve") — if this fails → 500, nothing else persisted/published.
//  6. PutObject(graded.final.json) — the snapshot artifact.
//  7. InsertFinalGrade(snapshot with overrides applied, approved_by=principal).
//  8. Publish Envelope{Stage: contracts.StageApprove} to commands.q.
//  9. Return 200 + FinalGrade JSON.
func (h *ApprovalHandlers) Approve(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionReviewFixApprove, h.DeployMode)
	if !ok {
		return
	}

	if sub.State != contracts.StateTeacherReview {
		http.Error(w, "submission is not in teacher_review state", http.StatusConflict)
		return
	}

	// Idempotent path: if a FinalGrade already exists, re-publish and return it.
	existingFG, err := h.Store.GetFinalGrade(r.Context(), tenantID, sub.ID)
	if err == nil {
		// FinalGrade already exists — idempotent retry: re-publish and return existing.
		env := contracts.Envelope{
			TenantID:      sub.TenantID.String(),
			Principal:     p.ID,
			SubmissionID:  sub.ID.String(),
			Stage:         contracts.StageApprove,
			CorrelationID: uuid.New().String(),
		}
		if err := h.Bus.Publish(r.Context(), "commands.q", env); err != nil {
			http.Error(w, "bus error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(existingFG)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		http.Error(w, "store error (get_final_grade)", http.StatusInternalServerError)
		return
	}

	// Load graded.v1.json. A missing artifact while in teacher_review is a server-side
	// error (the pipeline should have written it), not a client conflict.
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

	// Overlay teacher overrides to compute effective paper.
	reviews, err := h.Store.ListTeacherReviews(r.Context(), tenantID, sub.ID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	effective := overlayReviews(paper, reviews)

	// Compute totals from effective paper.
	var total, maxTotal float64
	for _, q := range effective.Questions {
		total += q.AwardedMarks
		maxTotal += q.MaxMarks
	}
	effective.Total = total
	effective.MaxTotal = maxTotal
	if maxTotal > 0 {
		effective.Score100 = (total / maxTotal) * 100
	}

	// Audit-first: write audit event before any artifact/DB write.
	subID := sub.ID
	_, err = h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subID,
		Actor:      p.ID,
		Action:     "approve",
		Reason:     "teacher approved graded submission",
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	// Write graded.final.json to object store (after audit succeeds).
	finalKey := fmt.Sprintf("%s/%s/graded.final.json", sub.TenantID, sub.ID)
	finalData, err := json.Marshal(effective)
	if err != nil {
		http.Error(w, "failed to marshal final grade", http.StatusInternalServerError)
		return
	}
	if err := h.Objects.PutObject(r.Context(), finalKey, finalData); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	// Persist the immutable final grade snapshot.
	now := time.Now()
	fg, err := h.Store.InsertFinalGrade(r.Context(), store.InsertFinalGradeParams{
		TenantID:     tenantID,
		SubmissionID: sub.ID,
		Total:        effective.Total,
		MaxTotal:     effective.MaxTotal,
		Score100:     effective.Score100,
		GradedKey:    finalKey,
		ApprovedBy:   p.ID,
		ApprovedAt:   now,
	})
	if err != nil {
		http.Error(w, "store error (final_grade)", http.StatusInternalServerError)
		return
	}

	// Publish ApproveByTeacher command to orchestrator.
	env := contracts.Envelope{
		TenantID:      sub.TenantID.String(),
		Principal:     p.ID,
		SubmissionID:  sub.ID.String(),
		Stage:         contracts.StageApprove,
		CorrelationID: uuid.New().String(),
	}
	if err := h.Bus.Publish(r.Context(), "commands.q", env); err != nil {
		// Command publish failure after all DB writes. Log in production;
		// the orchestrator can reconcile state on resync or manual replay.
		// Return 500 to signal to the caller that the full operation did not complete.
		http.Error(w, "bus error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fg)
}

// ─────────────────────────────────────────────────────────────────────────────
// Publish — POST /v1/submissions/{id}/publish
// ─────────────────────────────────────────────────────────────────────────────

// Publish handles POST /v1/submissions/{id}/publish.
//
// Steps (audit-first):
//  1. Auth + RBAC + tenant isolation (404 on failure).
//  2. 409 if sub.State != approved.
//  3. InsertAuditEvent(action="publish") — if this fails → 500, no command published.
//  4. Publish Envelope{Stage: contracts.StagePublish} to commands.q.
//  5. Return 200.
func (h *ApprovalHandlers) Publish(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionReviewFixApprove, h.DeployMode)
	if !ok {
		return
	}

	if sub.State != contracts.StateApproved {
		http.Error(w, "submission is not in approved state", http.StatusConflict)
		return
	}

	// Audit-first.
	subID := sub.ID
	_, err := h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subID,
		Actor:      p.ID,
		Action:     "publish",
		Reason:     "teacher published approved submission",
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	env := contracts.Envelope{
		TenantID:      sub.TenantID.String(),
		Principal:     p.ID,
		SubmissionID:  sub.ID.String(),
		Stage:         contracts.StagePublish,
		CorrelationID: uuid.New().String(),
	}
	if err := h.Bus.Publish(r.Context(), "commands.q", env); err != nil {
		http.Error(w, "bus error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ─────────────────────────────────────────────────────────────────────────────
// Export — POST /v1/submissions/{id}/export
// ─────────────────────────────────────────────────────────────────────────────

// Export handles POST /v1/submissions/{id}/export.
//
// Steps (audit-first):
//  1. Auth + RBAC + tenant isolation (404 on failure).
//  2. 409 if sub.State != published.
//  3. InsertAuditEvent(action="export") — if this fails → 500, no command published.
//  4. Publish Envelope{Stage: contracts.StageExport} to commands.q.
//  5. Return 200.
func (h *ApprovalHandlers) Export(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionReviewFixApprove, h.DeployMode)
	if !ok {
		return
	}

	if sub.State != contracts.StatePublished {
		http.Error(w, "submission is not in published state", http.StatusConflict)
		return
	}

	// Audit-first.
	subID := sub.ID
	_, err := h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subID,
		Actor:      p.ID,
		Action:     "export",
		Reason:     "teacher exported published submission",
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	env := contracts.Envelope{
		TenantID:      sub.TenantID.String(),
		Principal:     p.ID,
		SubmissionID:  sub.ID.String(),
		Stage:         contracts.StageExport,
		CorrelationID: uuid.New().String(),
	}
	if err := h.Bus.Publish(r.Context(), "commands.q", env); err != nil {
		http.Error(w, "bus error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

