package api

// curriculum_handler.go — HTTP handlers for curriculum outcomes and the mapping
// of assessment-version questions to those outcomes.
//
// Endpoints:
//   POST /v1/outcomes                                         — create an outcome
//   GET  /v1/outcomes                                         — list tenant outcomes
//   POST /v1/assessment-versions/{avid}/question-outcomes     — map a question→outcome
//   GET  /v1/assessment-versions/{avid}/question-outcomes     — list mappings for AV
//
// RBAC:
//   All four endpoints require ActionEditTunables.
//   Roles that hold this action: operator, group_admin, school_admin.
//   Teacher, reviewer and scanner are denied → 404 (404-not-403 to avoid
//   role/tenant enumeration, consistent with the rest of the API).
//
// Tenant-scoping approach:
//   The caller's tenant is derived from the principal. All store calls are
//   scoped by that tenant. For the mapping endpoint two extra checks apply:
//     - The target outcome must belong to the caller's tenant (GetOutcome →
//       404 on miss / cross-tenant).
//     - If the AVID already exists, it must belong to the caller's tenant
//       (GetAssessmentVersionTenantID → 404 on mismatch). If the AVID does not
//       exist yet (ErrNotFound) the mapping is still allowed, since mappings may
//       be authored ahead of the assessment version row materialising.

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
)

// CurriculumStore is the minimal store interface required by CurriculumHandlers.
type CurriculumStore interface {
	CreateOutcome(ctx context.Context, p store.CreateOutcomeParams) (store.CurriculumOutcome, error)
	ListOutcomes(ctx context.Context, tenantID uuid.UUID) ([]store.CurriculumOutcome, error)
	GetOutcome(ctx context.Context, tenantID, id uuid.UUID) (store.CurriculumOutcome, error)
	MapQuestionOutcome(ctx context.Context, p store.MapQuestionOutcomeParams) (store.QuestionOutcome, error)
	ListQuestionOutcomes(ctx context.Context, tenantID, assessmentVersionID uuid.UUID) ([]store.QuestionOutcome, error)
	GetAssessmentVersionTenantID(ctx context.Context, avid uuid.UUID) (uuid.UUID, error)
}

// CurriculumHandlers holds dependencies for the curriculum API handlers.
type CurriculumHandlers struct {
	Store      CurriculumStore
	DeployMode string // "saas" or "onprem"
}

// curriculumAuthz extracts and validates the principal and checks
// ActionEditTunables. On denial it writes an HTTP response and returns
// (zero, false). Auth is purely principal-scoped: we only need the caller's
// own tenant.
func (h *CurriculumHandlers) curriculumAuthz(w http.ResponseWriter, r *http.Request) (auth.Principal, uuid.UUID, bool) {
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
	if !domain.Can(p.Roles, domain.ActionEditTunables, rctx) {
		// 404, not 403 — prevents role/tenant enumeration.
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
// JSON shapes
// ─────────────────────────────────────────────────────────────────────────────

type createOutcomeRequest struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Subject     string `json:"subject"`
}

type outcomeResponse struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Description string `json:"description"`
	Subject     string `json:"subject"`
	CreatedAt   string `json:"created_at"`
}

type mapQuestionOutcomeRequest struct {
	QuestionNo string `json:"question_no"`
	OutcomeID  string `json:"outcome_id"`
}

type questionOutcomeResponse struct {
	ID                  string `json:"id"`
	AssessmentVersionID string `json:"assessment_version_id"`
	QuestionNo          string `json:"question_no"`
	OutcomeID           string `json:"outcome_id"`
	CreatedAt           string `json:"created_at"`
}

const timeFmt = "2006-01-02T15:04:05Z07:00"

func toOutcomeResponse(o store.CurriculumOutcome) outcomeResponse {
	return outcomeResponse{
		ID:          o.ID.String(),
		Code:        o.Code,
		Description: o.Description,
		Subject:     o.Subject,
		CreatedAt:   o.CreatedAt.Format(timeFmt),
	}
}

func toQuestionOutcomeResponse(q store.QuestionOutcome) questionOutcomeResponse {
	return questionOutcomeResponse{
		ID:                  q.ID.String(),
		AssessmentVersionID: q.AssessmentVersionID.String(),
		QuestionNo:          q.QuestionNo,
		OutcomeID:           q.OutcomeID.String(),
		CreatedAt:           q.CreatedAt.Format(timeFmt),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Handlers
// ─────────────────────────────────────────────────────────────────────────────

// CreateOutcome handles POST /v1/outcomes.
func (h *CurriculumHandlers) CreateOutcome(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.curriculumAuthz(w, r)
	if !ok {
		return
	}

	var req createOutcomeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Code == "" || req.Description == "" || req.Subject == "" {
		http.Error(w, "code, description and subject are required", http.StatusBadRequest)
		return
	}

	o, err := h.Store.CreateOutcome(r.Context(), store.CreateOutcomeParams{
		TenantID:    tenantID,
		Code:        req.Code,
		Description: req.Description,
		Subject:     req.Subject,
	})
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toOutcomeResponse(o))
}

// ListOutcomes handles GET /v1/outcomes.
func (h *CurriculumHandlers) ListOutcomes(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.curriculumAuthz(w, r)
	if !ok {
		return
	}

	outcomes, err := h.Store.ListOutcomes(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	items := make([]outcomeResponse, 0, len(outcomes))
	for _, o := range outcomes {
		items = append(items, toOutcomeResponse(o))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// MapQuestionOutcome handles POST /v1/assessment-versions/{avid}/question-outcomes.
func (h *CurriculumHandlers) MapQuestionOutcome(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.curriculumAuthz(w, r)
	if !ok {
		return
	}

	avid, ok := h.parseAndCheckAVID(w, r, tenantID)
	if !ok {
		return
	}

	var req mapQuestionOutcomeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.QuestionNo == "" {
		http.Error(w, "question_no is required", http.StatusBadRequest)
		return
	}
	outcomeID, err := uuid.Parse(req.OutcomeID)
	if err != nil {
		http.Error(w, "invalid outcome_id", http.StatusBadRequest)
		return
	}

	// The outcome must belong to the caller's tenant.
	if _, err := h.Store.GetOutcome(r.Context(), tenantID, outcomeID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	q, err := h.Store.MapQuestionOutcome(r.Context(), store.MapQuestionOutcomeParams{
		TenantID:            tenantID,
		AssessmentVersionID: avid,
		QuestionNo:          req.QuestionNo,
		OutcomeID:           outcomeID,
	})
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toQuestionOutcomeResponse(q))
}

// ListQuestionOutcomes handles GET /v1/assessment-versions/{avid}/question-outcomes.
func (h *CurriculumHandlers) ListQuestionOutcomes(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.curriculumAuthz(w, r)
	if !ok {
		return
	}

	avid, ok := h.parseAndCheckAVID(w, r, tenantID)
	if !ok {
		return
	}

	mappings, err := h.Store.ListQuestionOutcomes(r.Context(), tenantID, avid)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	items := make([]questionOutcomeResponse, 0, len(mappings))
	for _, q := range mappings {
		items = append(items, toQuestionOutcomeResponse(q))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// parseAndCheckAVID parses {avid} from the URL and ensures it does not belong to
// a different tenant. If the AVID exists and is owned by another tenant → 404.
// If the AVID does not exist yet (ErrNotFound) the request proceeds.
func (h *CurriculumHandlers) parseAndCheckAVID(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) (uuid.UUID, bool) {
	avidStr := chi.URLParam(r, "avid")
	avid, err := uuid.Parse(avidStr)
	if err != nil {
		http.Error(w, "invalid assessment_version_id", http.StatusBadRequest)
		return uuid.UUID{}, false
	}

	ownerTenantID, err := h.Store.GetAssessmentVersionTenantID(r.Context(), avid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// AVID does not exist yet — allowed (mapping authored ahead of time).
			return avid, true
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return uuid.UUID{}, false
	}
	if ownerTenantID != tenantID {
		http.Error(w, "not found", http.StatusNotFound)
		return uuid.UUID{}, false
	}
	return avid, true
}
