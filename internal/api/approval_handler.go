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
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/integration/webhook"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ApprovalStore is the subset of store.Store required by ApprovalHandlers.
type ApprovalStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
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

	// Webhook dispatch (optional — nil means no webhooks fired).
	WebhookSender *webhook.Sender
	WebhookStore  webhook.WebhookStore // subset interface from dispatch.go
	WebhookKey    []byte               // AES-256-GCM key; nil means no webhooks fired
}

// ─────────────────────────────────────────────────────────────────────────────
// Approve — POST /v1/submissions/{id}/approve
// ─────────────────────────────────────────────────────────────────────────────

// Approve handles POST /v1/submissions/{id}/approve.
//
// Steps (audit-first):
//  1. Auth + RBAC + tenant isolation (404 on failure).
//  2. 409 if sub.State != teacher_review.
//  3. Load graded.v1.json + overlay teacher overrides → effective GradedPaper (500 if missing).
//  4. InsertAuditEvent(action="approve", new_value=JSON{total,max_total,score_100}) — audit-first;
//     records the final totals so the prior grade survives in the append-only audit trail across
//     the upsert. If this fails → 500, nothing else persisted/published.
//  5. PutObject(graded.final.json) — the snapshot artifact.
//  6. InsertFinalGrade (UPSERT) — inserts on first approve, updates on post-regrade re-approve.
//  7. Publish Envelope{Stage: contracts.StageApprove} to commands.q.
//  8. Return 200 + FinalGrade JSON.
//
// This is idempotent-in-effect: a plain retry (no regrade between) recomputes identical values
// and upserts the same row (updated_at bumps, graded.final rewritten identically), then
// re-publishes — net effect identical. A post-regrade re-approve (graded.v1.json now holds the
// new grade) upserts the NEW final grade. Prior grades are preserved in the audit_event trail.
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

	// Recompute totals from effective paper (single source of truth shared with review view).
	effective = recomputeEffectiveTotals(effective)

	// Audit-first: write audit event before any artifact/DB write.
	// new_value records the final totals so prior grades survive in the append-only audit trail
	// across the upsert.
	subID := sub.ID
	newValueJSON, err := json.Marshal(map[string]any{
		"total":     effective.Total,
		"max_total": effective.MaxTotal,
		"score_100": effective.Score100,
	})
	if err != nil {
		http.Error(w, "failed to marshal audit new_value", http.StatusInternalServerError)
		return
	}
	_, err = h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subID,
		Actor:      p.ID,
		Action:     "approve",
		NewValue:   newValueJSON,
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

	// Persist the approval snapshot (UPSERT — inserts on first approve, updates on re-approve).
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
	// A publish failure here returns 500 after the DB writes have already succeeded.
	// Because InsertFinalGrade is an UPSERT, a client retry is safe: re-approve
	// recomputes the same values and the orchestrator reconciles via the re-sent
	// StageApprove command.
	if err := h.Bus.Publish(r.Context(), "commands.q", env); err != nil {
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

	// Fire webhook dispatch asynchronously — fire-and-forget; failures are logged.
	if h.WebhookSender != nil && h.WebhookStore != nil && len(h.WebhookKey) == 32 {
		go func() {
			whEvent := webhook.Event{
				Type:         "published",
				TenantID:     sub.TenantID.String(),
				SubmissionID: sub.ID.String(),
				OccurredAt:   time.Now().UTC(),
			}
			webhook.Dispatch(context.Background(), h.WebhookStore, h.WebhookSender, h.WebhookKey, sub.TenantID, whEvent)
		}()
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

