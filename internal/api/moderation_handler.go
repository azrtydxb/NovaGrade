package api

// moderation_handler.go — second-marker / sampled moderation workflow.
//
// Endpoints:
//
//	POST /v1/assessment-versions/{avid}/moderation  — start a session
//	POST /v1/moderation/{id}/marks                  — record a moderator mark
//	GET  /v1/moderation/{id}                        — comparison report
//
// RBAC: ActionReviewFixApprove required for all three endpoints.
// Tenant isolation: 404 on cross-tenant access (prevents enumeration).
//
// Design invariant: a moderation mark is RECORDED FOR COMPARISON ONLY.
// It NEVER changes the final grade. Discrepancies are actioned via the normal
// override/approve path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// ─────────────────────────────────────────────────────────────────────────────
// Store interface
// ─────────────────────────────────────────────────────────────────────────────

// ModerationStore is the store interface required by ModerationHandlers.
type ModerationStore interface {
	ExportStore // GetSubmission, GetFinalGrade, ListTeacherReviews
	GetAssessmentVersionTenantID(ctx context.Context, avid uuid.UUID) (uuid.UUID, error)
	ListSubmissionsByAssessmentVersion(ctx context.Context, tenantID, avid uuid.UUID) ([]store.Submission, error)

	CreateModerationSession(ctx context.Context, p store.CreateModerationSessionParams) (store.ModerationSession, []uuid.UUID, error)
	RecordModerationMark(ctx context.Context, p store.RecordModerationMarkParams) (store.ModerationMark, error)
	GetModerationSession(ctx context.Context, tenantID, id uuid.UUID) (store.ModerationSession, error)
	ListModerationSubmissions(ctx context.Context, tenantID, sessionID uuid.UUID) ([]uuid.UUID, error)
	ListModerationMarks(ctx context.Context, tenantID, sessionID uuid.UUID) ([]store.ModerationMark, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Handler
// ─────────────────────────────────────────────────────────────────────────────

// ModerationHandlers holds dependencies for the moderation endpoints.
type ModerationHandlers struct {
	Store      ModerationStore
	Objects    ObjectStore
	DeployMode string
}

// ─────────────────────────────────────────────────────────────────────────────
// Request / response shapes
// ─────────────────────────────────────────────────────────────────────────────

type startModerationRequest struct {
	SampleSize int `json:"sample_size"`
}

type startModerationResponse struct {
	SessionID            string   `json:"session_id"`
	SampledSubmissionIDs []string `json:"sampled_submission_ids"`
	SampleSize           int      `json:"sample_size"`
	Status               string   `json:"status"`
}

type recordMarkRequest struct {
	SubmissionID   string  `json:"submission_id"`
	QuestionNo     string  `json:"question_no"`
	ModeratorMarks float64 `json:"moderator_marks"`
}

type markComparisonEntry struct {
	SubmissionID    string  `json:"submission_id"`
	QuestionNo      string  `json:"question_no"`
	AI              float64 `json:"ai"`
	TeacherFinal    float64 `json:"teacher_final"`
	Moderator       float64 `json:"moderator"`
	DeltaModTeacher float64 `json:"delta_mod_teacher"`
	DeltaModAI      float64 `json:"delta_mod_ai"`
}

type comparisonSummary struct {
	MeanAbsModTeacherDelta float64 `json:"mean_abs_mod_teacher_delta"`
	Count                  int     `json:"count"`
}

type comparisonSessionInfo struct {
	ID                  string `json:"id"`
	AssessmentVersionID string `json:"assessment_version_id"`
	CreatedBy           string `json:"created_by"`
	SampleSize          int    `json:"sample_size"`
	Status              string `json:"status"`
}

type comparisonResponse struct {
	Session comparisonSessionInfo `json:"session"`
	Marks   []markComparisonEntry `json:"marks"`
	Summary comparisonSummary     `json:"summary"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth helper
// ─────────────────────────────────────────────────────────────────────────────

// resolveModPrincipal extracts and validates the principal and enforces
// ActionReviewFixApprove RBAC. Returns (principal, tenantUUID, true) or writes
// the error response and returns (_, _, false).
func (h *ModerationHandlers) resolveModPrincipal(w http.ResponseWriter, r *http.Request) (auth.Principal, uuid.UUID, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return auth.Principal{}, uuid.UUID{}, false
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionReviewFixApprove, rctx) {
		http.Error(w, "not found", http.StatusNotFound)
		return auth.Principal{}, uuid.UUID{}, false
	}
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return auth.Principal{}, uuid.UUID{}, false
	}
	return p, tenantID, true
}

// ─────────────────────────────────────────────────────────────────────────────
// Endpoints
// ─────────────────────────────────────────────────────────────────────────────

// StartSession handles POST /v1/assessment-versions/{avid}/moderation.
// Creates a moderation session and deterministically samples submissions.
func (h *ModerationHandlers) StartSession(w http.ResponseWriter, r *http.Request) {
	p, tenantID, ok := h.resolveModPrincipal(w, r)
	if !ok {
		return
	}

	avidStr := chi.URLParam(r, "avid")
	avid, err := uuid.Parse(avidStr)
	if err != nil {
		http.Error(w, "invalid assessment_version_id", http.StatusBadRequest)
		return
	}

	// Verify the AVID belongs to this tenant.
	ownerTenantID, err := h.Store.GetAssessmentVersionTenantID(r.Context(), avid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if ownerTenantID != tenantID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var req startModerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SampleSize <= 0 {
		http.Error(w, "invalid request body: sample_size must be positive", http.StatusBadRequest)
		return
	}

	sess, sampledIDs, err := h.Store.CreateModerationSession(r.Context(), store.CreateModerationSessionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avid,
		CreatedBy:           p.ID,
		SampleSize:          req.SampleSize,
	})
	if err != nil {
		log.Printf("moderation: CreateModerationSession error: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	sampledStrIDs := make([]string, len(sampledIDs))
	for i, id := range sampledIDs {
		sampledStrIDs[i] = id.String()
	}

	resp := startModerationResponse{
		SessionID:            sess.ID.String(),
		SampledSubmissionIDs: sampledStrIDs,
		SampleSize:           sess.SampleSize,
		Status:               sess.Status,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// RecordMark handles POST /v1/moderation/{id}/marks.
// Appends a moderator mark to the session. Does NOT change any final grade.
func (h *ModerationHandlers) RecordMark(w http.ResponseWriter, r *http.Request) {
	p, tenantID, ok := h.resolveModPrincipal(w, r)
	if !ok {
		return
	}

	sessionIDStr := chi.URLParam(r, "id")
	sessionID, err := uuid.Parse(sessionIDStr)
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}

	// Verify the session belongs to this tenant (tenant isolation).
	if _, err := h.Store.GetModerationSession(r.Context(), tenantID, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	var req recordMarkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	submissionID, err := uuid.Parse(req.SubmissionID)
	if err != nil {
		http.Error(w, "invalid submission_id", http.StatusBadRequest)
		return
	}
	if req.QuestionNo == "" {
		http.Error(w, "question_no is required", http.StatusBadRequest)
		return
	}
	// Fix 4: reject negative moderator_marks.
	if req.ModeratorMarks < 0 {
		http.Error(w, "moderator_marks must be non-negative", http.StatusBadRequest)
		return
	}

	// Fix 1: validate that submission_id is in the session's sampled submissions.
	sampledIDs, err := h.Store.ListModerationSubmissions(r.Context(), tenantID, sessionID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	inSample := false
	for _, id := range sampledIDs {
		if id == submissionID {
			inSample = true
			break
		}
	}
	if !inSample {
		http.Error(w, "submission_id is not in the session's sampled submissions", http.StatusUnprocessableEntity)
		return
	}

	// The moderator is the authenticated principal.
	mark, err := h.Store.RecordModerationMark(r.Context(), store.RecordModerationMarkParams{
		TenantID:       tenantID,
		SessionID:      sessionID,
		SubmissionID:   submissionID,
		QuestionNo:     req.QuestionNo,
		ModeratorMarks: req.ModeratorMarks,
		Moderator:      p.ID,
	})
	if err != nil {
		log.Printf("moderation: RecordModerationMark error: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":            mark.ID.String(),
		"session_id":    mark.SessionID.String(),
		"submission_id": mark.SubmissionID.String(),
		"question_no":   mark.QuestionNo,
	})
}

// GetComparison handles GET /v1/moderation/{id}.
//
// For each moderation_mark this builds a comparison row containing:
//   - ai: graded.v1.json AwardedMarks for that question (raw AI, unmodified)
//   - teacher_final: effective/final AwardedMarks for that question (after
//     teacher overrides, or from final_grade artifact if approved)
//   - moderator: the moderator's mark
//   - delta_mod_teacher: moderator − teacher_final
//   - delta_mod_ai: moderator − ai
//
// NOTE: This handler reads grades for comparison only. It NEVER mutates any
// grade, final_grade row, or submission state.
func (h *ModerationHandlers) GetComparison(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.resolveModPrincipal(w, r)
	if !ok {
		return
	}

	sessionIDStr := chi.URLParam(r, "id")
	sessionID, err := uuid.Parse(sessionIDStr)
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}

	sess, err := h.Store.GetModerationSession(r.Context(), tenantID, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	marks, err := h.Store.ListModerationMarks(r.Context(), tenantID, sessionID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// Build per-submission AI and teacher-final caches to avoid re-fetching.
	// aiPaper: raw graded.v1.json (no overlays) per submission
	// effectivePaper: effective grades per submission (with overlays/final)
	type paperCache struct {
		ai        map[string]float64 // questionNo → awarded
		effective map[string]float64 // questionNo → effective awarded
	}
	cache := make(map[uuid.UUID]*paperCache)

	loadCache := func(subID uuid.UUID) *paperCache {
		if c, ok := cache[subID]; ok {
			return c
		}
		c := &paperCache{
			ai:        make(map[string]float64),
			effective: make(map[string]float64),
		}
		cache[subID] = c

		sub, err := h.Store.GetSubmission(r.Context(), subID)
		if err != nil {
			return c // empty — marks will show 0
		}
		// Fix 2: belt-and-suspenders tenant assertion. GetSubmission fetches by ID
		// only; if somehow a foreign-tenant submission ID leaked through, skip it
		// rather than building an object key from a different tenant's path.
		if sub.TenantID != tenantID {
			return c // skip foreign-tenant submission silently
		}

		// AI mark: always from graded.v1.json (raw, before any overrides).
		v1Key := fmt.Sprintf("%s/%s/graded.v1.json", sub.TenantID, sub.ID)
		if data, err := h.Objects.GetObject(r.Context(), v1Key); err == nil {
			var paper contracts.GradedPaper
			if err := json.Unmarshal(data, &paper); err == nil {
				for _, q := range paper.Questions {
					c.ai[q.QuestionNo] = q.AwardedMarks
				}
			}
		}

		// Teacher-final mark: effectiveGradedPaper (FinalGrade artifact OR v1 + overlays).
		// effectiveGradedPaper is read-only and NEVER mutates any grade.
		effective, err := effectiveGradedPaper(r.Context(), h.Store, h.Objects, tenantID, sub)
		if err == nil {
			for _, q := range effective.Questions {
				c.effective[q.QuestionNo] = q.AwardedMarks
			}
		} else {
			// If no graded paper yet, fall back to AI marks for effective too.
			for k, v := range c.ai {
				c.effective[k] = v
			}
		}

		return c
	}

	entries := make([]markComparisonEntry, 0, len(marks))
	var absModTeacherDeltas []float64

	for _, m := range marks {
		pc := loadCache(m.SubmissionID)
		aiMark := pc.ai[m.QuestionNo]
		teacherFinal := pc.effective[m.QuestionNo]
		deltaModTeacher := m.ModeratorMarks - teacherFinal
		deltaModAI := m.ModeratorMarks - aiMark

		entries = append(entries, markComparisonEntry{
			SubmissionID:    m.SubmissionID.String(),
			QuestionNo:      m.QuestionNo,
			AI:              aiMark,
			TeacherFinal:    teacherFinal,
			Moderator:       m.ModeratorMarks,
			DeltaModTeacher: deltaModTeacher,
			DeltaModAI:      deltaModAI,
		})
		absModTeacherDeltas = append(absModTeacherDeltas, math.Abs(deltaModTeacher))
	}

	var meanAbsDelta float64
	if len(absModTeacherDeltas) > 0 {
		sum := 0.0
		for _, d := range absModTeacherDeltas {
			sum += d
		}
		meanAbsDelta = sum / float64(len(absModTeacherDeltas))
	}

	resp := comparisonResponse{
		Session: comparisonSessionInfo{
			ID:                  sess.ID.String(),
			AssessmentVersionID: sess.AssessmentVersionID.String(),
			CreatedBy:           sess.CreatedBy,
			SampleSize:          sess.SampleSize,
			Status:              sess.Status,
		},
		Marks: entries,
		Summary: comparisonSummary{
			MeanAbsModTeacherDelta: meanAbsDelta,
			Count:                  len(entries),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
