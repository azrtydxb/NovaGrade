package api

// analytics_handler.go — analytics + override-stats endpoints.
//
//   GET /v1/assessment-versions/{avid}/analytics
//   GET /v1/assessment-versions/{avid}/override-stats
//
// RBAC: ActionViewResults required.
// Tenant isolation: same pattern as ClassResultsHandlers — cross-tenant returns 404.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/analytics"
	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// AnalyticsStore is the store interface for both analytics endpoints.
type AnalyticsStore interface {
	ExportStore // GetSubmission, GetFinalGrade, ListTeacherReviews
	ListSubmissionsByAssessmentVersion(ctx context.Context, tenantID, avid uuid.UUID) ([]store.Submission, error)
	GetAssessmentVersionTenantID(ctx context.Context, avid uuid.UUID) (uuid.UUID, error)
	ListAuditEventsBySubmission(ctx context.Context, tenantID, submissionID uuid.UUID) ([]store.AuditEvent, error)
}

// AnalyticsHandlers holds dependencies for both analytics endpoints.
type AnalyticsHandlers struct {
	Store      AnalyticsStore
	Objects    ObjectStore
	DeployMode string
}

// analyticsResponse is the JSON shape for GET …/analytics.
type analyticsResponse struct {
	ItemAnalysis    []analytics.QuestionStat `json:"item_analysis"`
	Distribution    analytics.Distribution   `json:"distribution"`
	Hardest         []analytics.QuestionStat `json:"hardest"`
	FlagFrequencies map[string]int           `json:"flag_frequencies"`
	GradedCount     int                      `json:"graded_count"`
	TotalCount      int                      `json:"total_count"`
}

// overrideStatsResponse is the JSON shape for GET …/override-stats.
type overrideStatsResponse struct {
	TotalGradedQuestions int     `json:"total_graded_questions"`
	OverriddenQuestions  int     `json:"overridden_questions"`
	OverrideRate         float64 `json:"override_rate"`
	MeanAbsDelta         float64 `json:"mean_abs_delta"`
}

// GetAnalytics handles GET /v1/assessment-versions/{avid}/analytics.
func (h *AnalyticsHandlers) GetAnalytics(w http.ResponseWriter, r *http.Request) {
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

	// Cross-tenant 404 check: if no submissions found, confirm the AVID doesn't
	// belong to another tenant.
	if len(subs) == 0 {
		ownerTenantID, lookupErr := h.Store.GetAssessmentVersionTenantID(r.Context(), avid)
		if lookupErr == nil && ownerTenantID != tenantID {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	// Collect effective graded papers; skip submissions without a graded artifact.
	var papers []contracts.GradedPaper
	for _, sub := range subs {
		paper, err := effectiveGradedPaper(r.Context(), h.Store, h.Objects, tenantID, sub)
		if err != nil {
			if errors.Is(err, errNotGradedYet) {
				continue
			}
			// Unexpected error — skip this submission (best-effort analytics).
			continue
		}
		papers = append(papers, paper)
	}

	stats := analytics.ItemAnalysis(papers)
	dist := analytics.GradeDistribution(papers)
	hardest := analytics.HardestQuestions(stats, 5)
	flags := analytics.FlagFrequencies(papers)

	// Ensure non-nil slices/maps in the response.
	if stats == nil {
		stats = []analytics.QuestionStat{}
	}
	if hardest == nil {
		hardest = []analytics.QuestionStat{}
	}
	if flags == nil {
		flags = map[string]int{}
	}

	resp := analyticsResponse{
		ItemAnalysis:    stats,
		Distribution:    dist,
		Hardest:         hardest,
		FlagFrequencies: flags,
		GradedCount:     len(papers),
		TotalCount:      len(subs),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// GetOverrideStats handles GET /v1/assessment-versions/{avid}/override-stats.
func (h *AnalyticsHandlers) GetOverrideStats(w http.ResponseWriter, r *http.Request) {
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

	// Cross-tenant 404 check.
	if len(subs) == 0 {
		ownerTenantID, lookupErr := h.Store.GetAssessmentVersionTenantID(r.Context(), avid)
		if lookupErr == nil && ownerTenantID != tenantID {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	var totalGradedQuestions int
	var overriddenQuestions int
	var absDeltas []float64

	for _, sub := range subs {
		paper, err := effectiveGradedPaper(r.Context(), h.Store, h.Objects, tenantID, sub)
		if err != nil {
			// Skip ungraded or error submissions.
			continue
		}
		totalGradedQuestions += len(paper.Questions)

		// Fetch audit events for this submission.
		events, err := h.Store.ListAuditEventsBySubmission(r.Context(), tenantID, sub.ID)
		if err != nil {
			continue
		}

		for _, ev := range events {
			if ev.Action != "override_question" {
				continue
			}
			oldMarks, okOld := parseAwardedMarks(ev.OldValue)
			newMarks, okNew := parseAwardedMarks(ev.NewValue)
			if !okOld || !okNew {
				continue
			}
			overriddenQuestions++
			absDeltas = append(absDeltas, math.Abs(newMarks-oldMarks))
		}
	}

	var overrideRate float64
	if totalGradedQuestions > 0 {
		overrideRate = float64(overriddenQuestions) / float64(totalGradedQuestions)
	}

	var meanAbsDelta float64
	if len(absDeltas) > 0 {
		sum := 0.0
		for _, d := range absDeltas {
			sum += d
		}
		meanAbsDelta = sum / float64(len(absDeltas))
	}

	resp := overrideStatsResponse{
		TotalGradedQuestions: totalGradedQuestions,
		OverriddenQuestions:  overriddenQuestions,
		OverrideRate:         overrideRate,
		MeanAbsDelta:         meanAbsDelta,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// parseAwardedMarks unmarshals raw JSON bytes from an audit event's OldValue or
// NewValue field and extracts the "awarded_marks" key as float64.
func parseAwardedMarks(raw []byte) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false
	}
	v, ok := m["awarded_marks"]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}
