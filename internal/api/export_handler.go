package api

// ExportHandlers provides the HTTP handler for the CSV export endpoint:
//
//   GET /v1/submissions/{id}/export.csv
//
// Design notes:
//   - RBAC: ActionViewResults required (teachers, reviewers, admins).
//   - Tenant isolation: the handler first fetches the submission to verify it
//     exists and belongs to the caller's tenant. Cross-tenant and no-permission
//     both return 404 to prevent tenant enumeration.
//   - Effective result selection:
//       • If a FinalGrade record exists, use its GradedKey (graded.final.json) —
//         this artifact already has teacher overrides baked in from the Approve flow.
//       • Otherwise load graded.v1.json and overlay ListTeacherReviews to compute
//         the current effective paper (reuses overlayReviews from review_handler.go).
//   - If no graded artifact exists at all → 404 "not graded yet".
//   - CSV header: question_no,section,max_marks,awarded_marks,grade_confidence,feedback,flags
//   - Content-Type: text/csv; charset=utf-8
//   - Content-Disposition: attachment; filename="submission-<id>.csv"
//   - Flags are joined by ";" within a single CSV cell.
//   - Section is empty string when nil (optional field in GradedQuestion).

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ExportStore is the subset of store.Store required by ExportHandlers.
type ExportStore interface {
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	GetFinalGrade(ctx context.Context, tenantID uuid.UUID, submissionID uuid.UUID) (store.FinalGrade, error)
	ListTeacherReviews(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.TeacherReview, error)
}

// ExportHandlers holds dependencies for the CSV export HTTP handler.
type ExportHandlers struct {
	Store      ExportStore
	Objects    ObjectStore // same interface as Handlers.Objects / ReviewHandlers.Objects
	DeployMode string      // "saas" or "onprem"
}

// ExportCSV handles GET /v1/submissions/{id}/export.csv.
//
// Effective result selection:
//   - If GetFinalGrade succeeds → load graded.final.json via fg.GradedKey.
//     Overrides are already baked into the snapshot; skip ListTeacherReviews.
//   - Otherwise → load graded.v1.json + overlay ListTeacherReviews.
//   - If no graded artifact found → 404 (not graded yet).
//
// The CSV is streamed directly to the response writer with:
//
//	Content-Type: text/csv; charset=utf-8
//	Content-Disposition: attachment; filename="submission-<id>.csv"
func (h *ExportHandlers) ExportCSV(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sub, tenantID, ok := fetchAndAuthorize(w, r, p, h.Store, actionViewResults, h.DeployMode)
	if !ok {
		return
	}

	// Determine effective GradedPaper.
	paper, err := h.effectivePaper(r.Context(), tenantID, sub)
	if err != nil {
		if errors.Is(err, errNotGradedYet) {
			http.Error(w, "not graded yet", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Stream CSV response.
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="submission-%s.csv"`, sub.ID))

	cw := csv.NewWriter(w)

	// Header row — matches the spec:
	//   question_no, section, max_marks, awarded_marks, grade_confidence, feedback, flags
	_ = cw.Write([]string{
		"question_no",
		"section",
		"max_marks",
		"awarded_marks",
		"grade_confidence",
		"feedback",
		"flags",
	})

	for _, q := range paper.Questions {
		section := ""
		if q.Section != nil {
			section = *q.Section
		}
		flagsCell := strings.Join(q.Flags, ";")

		_ = cw.Write([]string{
			q.QuestionNo,
			section,
			formatFloat(q.MaxMarks),
			formatFloat(q.AwardedMarks),
			formatFloat(q.GradeConfidence),
			q.Justification,
			flagsCell,
		})
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Printf("export: csv write/flush error for submission %s: %v", sub.ID, err)
	}
}

// errNotGradedYet is a sentinel returned by effectivePaper when no graded
// artifact exists in the object store.
var errNotGradedYet = errors.New("not graded yet")

// effectivePaper resolves the effective GradedPaper for a submission:
//   - FinalGrade exists → load the baked-in artifact (graded.final.json).
//   - No FinalGrade → load graded.v1.json + overlay ListTeacherReviews.
//   - Artifact missing from object store → errNotGradedYet.
func (h *ExportHandlers) effectivePaper(
	ctx context.Context,
	tenantID uuid.UUID,
	sub store.Submission,
) (contracts.GradedPaper, error) {
	// Try final_grade first (approved/published/exported path).
	fg, err := h.Store.GetFinalGrade(ctx, tenantID, sub.ID)
	if err == nil {
		// FinalGrade exists — load the baked-in snapshot.
		data, err := h.Objects.GetObject(ctx, fg.GradedKey)
		if err != nil {
			return contracts.GradedPaper{}, errNotGradedYet
		}
		var paper contracts.GradedPaper
		if err := json.Unmarshal(data, &paper); err != nil {
			return contracts.GradedPaper{}, fmt.Errorf("corrupt graded.final artifact: %w", err)
		}
		return paper, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return contracts.GradedPaper{}, fmt.Errorf("store error (get_final_grade): %w", err)
	}

	// No FinalGrade — fall back to graded.v1.json + overlayReviews.
	objectKey := fmt.Sprintf("%s/%s/graded.v1.json", sub.TenantID, sub.ID)
	data, err := h.Objects.GetObject(ctx, objectKey)
	if err != nil {
		return contracts.GradedPaper{}, errNotGradedYet
	}

	var paper contracts.GradedPaper
	if err := json.Unmarshal(data, &paper); err != nil {
		return contracts.GradedPaper{}, fmt.Errorf("corrupt graded.v1 artifact: %w", err)
	}

	reviews, err := h.Store.ListTeacherReviews(ctx, tenantID, sub.ID)
	if err != nil {
		return contracts.GradedPaper{}, fmt.Errorf("store error (list_teacher_reviews): %w", err)
	}

	return overlayReviews(paper, reviews), nil
}

// formatFloat formats a float64 for CSV output. If the value has no fractional
// part it is rendered as an integer (e.g. 9 → "9", 9.5 → "9.5").
func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
