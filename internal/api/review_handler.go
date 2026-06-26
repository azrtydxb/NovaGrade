package api

// ReviewHandlers provides the HTTP handlers for the teacher review endpoints:
//
//   GET  /v1/submissions/{id}/review
//   PATCH /v1/submissions/{id}/questions/{qno}
//
// Design notes:
//   - RBAC: ActionReviewFixApprove required for both endpoints.
//   - Tenant isolation: the handler first fetches the submission to verify it
//     exists and belongs to the caller's tenant. Cross-tenant and no-permission
//     both return 404 to prevent tenant enumeration.
//   - Override allowed ONLY when sub.State == StateTeacherReview; otherwise 409.
//   - Every override writes an append-only audit_event via Store.InsertAuditEvent.
//   - Merge logic: ListTeacherReviews returns all overrides in created_at ASC
//     order; iterating in order so the last row per question_no wins.
//   - AwardedMarks is clamped to [0, question.MaxMarks] before persisting.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ReviewStore is the subset of store.Store required by ReviewHandlers.
type ReviewStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	InsertTeacherReview(ctx context.Context, p store.InsertTeacherReviewParams) (store.TeacherReview, error)
	ListTeacherReviews(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.TeacherReview, error)
	InsertAuditEvent(ctx context.Context, p store.InsertAuditEventParams) (store.AuditEvent, error)
}

// ReviewHandlers holds dependencies for the review HTTP handlers.
type ReviewHandlers struct {
	Store      ReviewStore
	Objects    ObjectStore // PutObject/GetObject — same interface as Handlers.Objects
	DeployMode string      // "saas" or "onprem"
}

// patchQuestionBody is the JSON body accepted by PATCH /v1/submissions/{id}/questions/{qno}.
type patchQuestionBody struct {
	AwardedMarks *float64 `json:"awarded_marks"`
	Feedback     *string  `json:"feedback"`
	Comment      *string  `json:"comment"`
}

// reviewResponse is returned by GET /v1/submissions/{id}/review.
type reviewResponse struct {
	Locked bool                  `json:"locked"`
	Paper  contracts.GradedPaper `json:"paper"`
}

// GetReview handles GET /v1/submissions/{id}/review.
//
// Loads graded.v1.json from the object store, overlays the latest teacher
// override per question_no (from ListTeacherReviews — latest by created_at
// wins), and returns the effective GradedPaper plus a top-level "locked" flag.
// locked == true means sub.State != StateTeacherReview and further overrides
// are not accepted.
//
// 409 when no graded artifact exists yet.
// 404 cross-tenant or lacking ReviewFixApprove.
func (h *ReviewHandlers) GetReview(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionReviewFixApprove, h.DeployMode)
	if !ok {
		return
	}

	// Load the graded artifact.
	objectKey := fmt.Sprintf("%s/%s/graded.v1.json", sub.TenantID, sub.ID)
	data, err := h.Objects.GetObject(r.Context(), objectKey)
	if err != nil {
		// No graded artifact yet → 409 (submission not graded).
		http.Error(w, "graded artifact not available", http.StatusConflict)
		return
	}

	var paper contracts.GradedPaper
	if err := json.Unmarshal(data, &paper); err != nil {
		http.Error(w, "corrupt graded artifact", http.StatusInternalServerError)
		return
	}

	// Load all teacher overrides and compute the latest per question_no.
	reviews, err := h.Store.ListTeacherReviews(r.Context(), tenantID, sub.ID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	paper = overlayReviews(paper, reviews)

	locked := sub.State != contracts.StateTeacherReview

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reviewResponse{
		Locked: locked,
		Paper:  paper,
	})
}

// PatchQuestion handles PATCH /v1/submissions/{id}/questions/{qno}.
//
// Body (all fields optional):
//
//	{ "awarded_marks": <float>, "feedback": <string>, "comment": <string> }
//
// Steps:
//  1. RBAC + tenant isolation (same pattern as GetReview).
//  2. 409 if sub.State != StateTeacherReview.
//  3. Load graded.v1.json, overlay existing reviews → find the current effective
//     value of the requested question. 404 if question_no absent.
//  4. Clamp awarded_marks to [0, question.MaxMarks].
//  5. InsertAuditEvent FIRST (entity_type="submission", action="override_question",
//     old_value/new_value JSON, actor=principal id, reason=comment).
//     On error → 500; no teacher_review row is written (audit gates the override).
//  6. InsertTeacherReview (OldMarks = current effective, NewMarks = new).
//  7. Return the updated effective question (200).
func (h *ReviewHandlers) PatchQuestion(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	qno := chi.URLParam(r, "qno")
	if qno == "" {
		http.Error(w, "missing question number", http.StatusBadRequest)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionReviewFixApprove, h.DeployMode)
	if !ok {
		return
	}

	// Locked check: only override in teacher_review state (404 before 409 via fetchAndAuthorize).
	if sub.State != contracts.StateTeacherReview {
		http.Error(w, "submission is locked (not in teacher_review)", http.StatusConflict)
		return
	}

	// Parse request body.
	var body patchQuestionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Load graded artifact.
	objectKey := fmt.Sprintf("%s/%s/graded.v1.json", sub.TenantID, sub.ID)
	data, err := h.Objects.GetObject(r.Context(), objectKey)
	if err != nil {
		http.Error(w, "graded artifact not available", http.StatusConflict)
		return
	}

	var paper contracts.GradedPaper
	if err := json.Unmarshal(data, &paper); err != nil {
		http.Error(w, "corrupt graded artifact", http.StatusInternalServerError)
		return
	}

	// Load existing overrides and overlay.
	reviews, err := h.Store.ListTeacherReviews(r.Context(), tenantID, sub.ID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	effective := overlayReviews(paper, reviews)

	// Find the effective question.
	qIdx := -1
	for i, q := range effective.Questions {
		if q.QuestionNo == qno {
			qIdx = i
			break
		}
	}
	if qIdx < 0 {
		http.Error(w, "question not found", http.StatusNotFound)
		return
	}

	currentQ := effective.Questions[qIdx]
	oldMarks := currentQ.AwardedMarks

	// Compute new awarded marks (default: keep current).
	newMarks := oldMarks
	if body.AwardedMarks != nil {
		newMarks = *body.AwardedMarks
		// Clamp to [0, MaxMarks].
		if newMarks < 0 {
			newMarks = 0
		}
		if newMarks > currentQ.MaxMarks {
			newMarks = currentQ.MaxMarks
		}
	}

	// Determine feedback string (operates on Feedback field; Justification is immutable).
	newFeedback := currentQ.Feedback
	if body.Feedback != nil {
		newFeedback = *body.Feedback
	}

	comment := ""
	if body.Comment != nil {
		comment = *body.Comment
	}

	// Serialize old/new for audit trail (feedback field, not justification).
	oldValueJSON, _ := json.Marshal(map[string]interface{}{
		"question_no":   qno,
		"awarded_marks": oldMarks,
		"feedback":      currentQ.Feedback,
	})
	newValueJSON, _ := json.Marshal(map[string]interface{}{
		"question_no":   qno,
		"awarded_marks": newMarks,
		"feedback":      newFeedback,
	})

	// Write audit event FIRST — it gates the override.
	// If the audit write fails we return 500 and do NOT persist the teacher_review
	// row, so no un-audited mark change can ever reach the database.
	subID := sub.ID
	_, err = h.Store.InsertAuditEvent(r.Context(), store.InsertAuditEventParams{
		TenantID:   tenantID,
		EntityType: "submission",
		EntityID:   &subID,
		Actor:      p.ID,
		Action:     "override_question",
		OldValue:   oldValueJSON,
		NewValue:   newValueJSON,
		Reason:     comment,
	})
	if err != nil {
		http.Error(w, "store error (audit)", http.StatusInternalServerError)
		return
	}

	// Audit recorded — now persist the override row.
	// If this fails the result is an audit event for an override that didn't
	// persist, which is benign and far safer than the reverse ordering.
	_, err = h.Store.InsertTeacherReview(r.Context(), store.InsertTeacherReviewParams{
		TenantID:     tenantID,
		SubmissionID: sub.ID,
		QuestionNo:   qno,
		OldMarks:     oldMarks,
		NewMarks:     newMarks,
		Feedback:     newFeedback,
		Comment:      comment,
		Actor:        p.ID,
	})
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// Build the returned effective question (Feedback updated; Justification unchanged).
	updatedQ := currentQ
	updatedQ.AwardedMarks = newMarks
	updatedQ.Feedback = newFeedback

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updatedQ)
}

// overlayReviews applies teacher overrides onto a GradedPaper and recomputes
// the paper-level totals so that Total, MaxTotal, and Score100 are consistent
// with the overlaid per-question AwardedMarks.
//
// reviews must be in created_at ASC order (as returned by ListTeacherReviews).
// The last row per question_no wins (latest-write-wins semantics).
func overlayReviews(paper contracts.GradedPaper, reviews []store.TeacherReview) contracts.GradedPaper {
	if len(reviews) == 0 {
		return paper
	}

	// Build index: question_no → position in paper.Questions.
	idx := make(map[string]int, len(paper.Questions))
	for i, q := range paper.Questions {
		idx[q.QuestionNo] = i
	}

	// Apply overrides in order; later entries overwrite earlier ones.
	// Marks overlay onto AwardedMarks; feedback overlay onto Feedback (NOT Justification).
	// Justification (grader rationale) is left immutable by the override path.
	for _, r := range reviews {
		i, ok := idx[r.QuestionNo]
		if !ok {
			continue // unknown question — skip
		}
		paper.Questions[i].AwardedMarks = r.NewMarks
		if r.Feedback != "" {
			paper.Questions[i].Feedback = r.Feedback
		}
	}

	// Recompute paper-level totals from the overlaid questions so that the
	// response is internally consistent (Total/Score100 match per-question marks).
	paper = recomputeEffectiveTotals(paper)
	return paper
}

// recomputeEffectiveTotals rebuilds Total, MaxTotal, and Score100 from the
// current per-question AwardedMarks/MaxMarks. It is the single source of truth
// for effective-paper totals used by both the review view and the approval path.
//
// Score100 = roundTo1(100 * Total / MaxTotal); 0 when MaxTotal == 0.
// This matches the grade-pipeline rounding (internal/pipeline/grade/grader.go).
func recomputeEffectiveTotals(paper contracts.GradedPaper) contracts.GradedPaper {
	var total, maxTotal float64
	for _, q := range paper.Questions {
		total += q.AwardedMarks
		maxTotal += q.MaxMarks
	}
	paper.Total = total
	paper.MaxTotal = maxTotal
	if maxTotal > 0 {
		paper.Score100 = reviewRoundTo1(100 * total / maxTotal)
	} else {
		paper.Score100 = 0
	}
	return paper
}

// reviewRoundTo1 rounds f to one decimal place.
// Mirrors the rounding in internal/pipeline/grade/grader.go (roundTo1) so that
// the review view, the approval snapshot, and the grade stage all agree.
func reviewRoundTo1(f float64) float64 {
	shifted := f * 10
	rounded := float64(int64(shifted+0.5)) / 10
	return rounded
}
