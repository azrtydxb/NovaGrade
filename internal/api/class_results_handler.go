package api

// class_results_handler.go — GET /v1/assessment-versions/{avid}/results.csv
//
// Streams a CSV of all graded submissions for an assessment version.
// Uses the same effective-grade logic as ExportCSV (via the package-level
// effectiveGradedPaper helper). Ungraded submissions are skipped.
//
// RBAC: ActionViewResults required.
// Content-Type: text/csv

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	integrationcsv "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ClassResultsStore is the store interface required by ClassResultsHandlers.
type ClassResultsStore interface {
	ExportStore // GetSubmission, GetFinalGrade, ListTeacherReviews
	ListSubmissionsByAssessmentVersion(ctx context.Context, tenantID, avid uuid.UUID) ([]store.Submission, error)
	GetStudent(ctx context.Context, tenantID, id uuid.UUID) (store.Student, error)
}

// ClassResultsHandlers holds dependencies for the class-results CSV handler.
type ClassResultsHandlers struct {
	Store      ClassResultsStore
	Objects    ObjectStore
	DeployMode string
}

// ClassResultsCSV handles GET /v1/assessment-versions/{avid}/results.csv.
func (h *ClassResultsHandlers) ClassResultsCSV(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	avidStr := chi.URLParam(r, "avid")
	avid, err := uuid.Parse(avidStr)
	if err != nil {
		http.Error(w, "invalid assessment_version_id", http.StatusBadRequest)
		return
	}

	subs, err := h.Store.ListSubmissionsByAssessmentVersion(r.Context(), tenantID, avid)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	var rows []contracts.GradeRow
	for _, sub := range subs {
		paper, err := effectiveGradedPaper(r.Context(), h.Store, h.Objects, tenantID, sub)
		if err != nil {
			// Skip submissions with no graded artifact.
			continue
		}

		// Resolve student name.
		studentName := ""
		if sub.StudentID != nil {
			if st, err := h.Store.GetStudent(r.Context(), tenantID, *sub.StudentID); err == nil {
				studentName = st.FullName
			}
		}

		for _, q := range paper.Questions {
			rows = append(rows, contracts.GradeRow{
				StudentName: studentName,
				QuestionNo:  q.QuestionNo,
				MaxMarks:    q.MaxMarks,
				Awarded:     q.AwardedMarks,
				Feedback:    q.Feedback,
			})
		}
	}

	w.Header().Set("Content-Type", "text/csv")
	connector := integrationcsv.GradeConnector{}
	_ = connector.ExportGrades(r.Context(), w, rows)
}
