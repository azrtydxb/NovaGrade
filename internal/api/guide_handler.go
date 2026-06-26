package api

// guide_handler.go — HTTP handlers for the marking-guide API.
//
// Endpoints:
//   POST /v1/assessment-versions/{avid}/guides       — import guide (validate + insert)
//   GET  /v1/assessment-versions/{avid}/guides       — list all versions (metadata only)
//   GET  /v1/assessment-versions/{avid}/guides/latest — latest guide with content
//   POST /v1/guides/{id}/lock                        — lock a guide version
//
// RBAC:
//   All four endpoints require ActionEditTunables.
//   Roles that hold this action: operator, group_admin, school_admin.
//   Teacher and scanner are denied → 404 (404-not-403 to avoid role/tenant
//   enumeration, consistent with fetchAndAuthorize in authz.go).
//
// Tenant-scoping approach:
//   Guide store calls always use p.TenantID (the caller's tenant) as the tenant
//   filter. An attacker from tenant B who supplies tenant A's assessment_version_id
//   will simply query guides scoped to tenant B, finding nothing for that avid.
//   There is no per-resource tenant fetch — the guide is tenant-isolated by the
//   principal alone. This is safe because:
//     - The assessment_version_id is opaque; guessing it yields 404 for the wrong tenant.
//     - No data about tenant A leaks to tenant B.
//   This approach is documented in the task brief and is consistent with the
//   guidance: "derive tenant from the principal and scope ALL guide store calls by
//   that tenant."
//
// Lock-doesn't-block-new-version:
//   Locking marks a specific guide version as immutable. A subsequent POST to
//   /guides creates a NEW version (version = max+1). The API layer never prevents
//   a new import; the store enforces immutability of content on the locked row only.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
	"github.com/azrtydxb/novagrade/internal/store"
)

// GuideStore is the minimal store interface required by GuideHandlers.
type GuideStore interface {
	InsertGuideVersion(ctx context.Context, p store.InsertGuideVersionParams) (store.MarkingGuide, error)
	GetLatestGuide(ctx context.Context, tenantID, assessmentVersionID uuid.UUID) (store.MarkingGuide, error)
	ListGuideVersions(ctx context.Context, tenantID, assessmentVersionID uuid.UUID) ([]store.MarkingGuide, error)
	LockGuide(ctx context.Context, tenantID, guideID uuid.UUID) error
}

// GuideHandlers holds dependencies for the guide API handlers.
type GuideHandlers struct {
	Store      GuideStore
	DeployMode string // "saas" or "onprem"
}

// guideAuthz extracts and validates the principal and checks ActionEditTunables.
// On denial it writes an HTTP response and returns (zero, false).
// Unlike fetchAndAuthorize (which is per-submission), guide auth is purely
// principal-scoped: we only need the caller's own tenant.
func (h *GuideHandlers) guideAuthz(w http.ResponseWriter, r *http.Request) (auth.Principal, uuid.UUID, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return auth.Principal{}, uuid.UUID{}, false
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID, // guide is scoped to the caller's own tenant
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionEditTunables, rctx) {
		// 404, not 403 — prevents role/tenant enumeration (matches authz.go convention).
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

// guideImportResponse is the JSON shape returned by ImportGuide (201).
type guideImportResponse struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Name    string `json:"name"`
	Locked  bool   `json:"locked"`
}

// guideListItem is the JSON shape for each entry in the ListGuides response.
type guideListItem struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	Name      string `json:"name"`
	Locked    bool   `json:"locked"`
	CreatedAt string `json:"created_at"`
}

// guideLatestResponse is the JSON shape returned by GetLatestGuide.
type guideLatestResponse struct {
	ID        string          `json:"id"`
	Version   int             `json:"version"`
	Name      string          `json:"name"`
	Locked    bool            `json:"locked"`
	CreatedAt string          `json:"created_at"`
	Content   json.RawMessage `json:"content"`
}

const maxGuideBodySize = 5 << 20 // 5 MB

// ImportGuide handles POST /v1/assessment-versions/{avid}/guides.
//
// Steps:
//  1. Auth + RBAC (ActionEditTunables). 401/403 on failure.
//  2. Parse {avid} URL param. 400 on bad UUID.
//  3. Read + parse body JSON.
//  4. Validate via grade.ValidateGuide → 400 with detail if invalid.
//  5. InsertGuideVersion (TenantID=principal's tenant, AssessmentVersionID={avid},
//     Name from ?name query param or body field, Content=raw JSON).
//  6. Return 201 + {id, version, name, locked}.
func (h *GuideHandlers) ImportGuide(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.guideAuthz(w, r)
	if !ok {
		return
	}

	avidStr := chi.URLParam(r, "avid")
	avid, err := uuid.Parse(avidStr)
	if err != nil {
		http.Error(w, "invalid assessment_version_id", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxGuideBodySize+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if int64(len(body)) > maxGuideBodySize {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Parse into Guide for validation.
	g, err := grade.LoadGuideFromJSON(body)
	if err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate every entry.
	if err := grade.ValidateGuide(g); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Name: prefer ?name query param, fall back to empty.
	name := r.URL.Query().Get("name")

	mg, err := h.Store.InsertGuideVersion(r.Context(), store.InsertGuideVersionParams{
		TenantID:            tenantID,
		AssessmentVersionID: avid,
		Name:                name,
		Content:             body,
	})
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(guideImportResponse{
		ID:      mg.ID.String(),
		Version: mg.Version,
		Name:    mg.Name,
		Locked:  mg.Locked,
	})
}

// ListGuides handles GET /v1/assessment-versions/{avid}/guides.
//
// Returns metadata for all versions (no content) for the caller's tenant.
func (h *GuideHandlers) ListGuides(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.guideAuthz(w, r)
	if !ok {
		return
	}

	avidStr := chi.URLParam(r, "avid")
	avid, err := uuid.Parse(avidStr)
	if err != nil {
		http.Error(w, "invalid assessment_version_id", http.StatusBadRequest)
		return
	}

	guides, err := h.Store.ListGuideVersions(r.Context(), tenantID, avid)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	items := make([]guideListItem, 0, len(guides))
	for _, g := range guides {
		items = append(items, guideListItem{
			ID:        g.ID.String(),
			Version:   g.Version,
			Name:      g.Name,
			Locked:    g.Locked,
			CreatedAt: g.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// GetLatestGuide handles GET /v1/assessment-versions/{avid}/guides/latest.
//
// Returns the highest-version guide with its content. 404 if none.
func (h *GuideHandlers) GetLatestGuide(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.guideAuthz(w, r)
	if !ok {
		return
	}

	avidStr := chi.URLParam(r, "avid")
	avid, err := uuid.Parse(avidStr)
	if err != nil {
		http.Error(w, "invalid assessment_version_id", http.StatusBadRequest)
		return
	}

	mg, err := h.Store.GetLatestGuide(r.Context(), tenantID, avid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(guideLatestResponse{
		ID:        mg.ID.String(),
		Version:   mg.Version,
		Name:      mg.Name,
		Locked:    mg.Locked,
		CreatedAt: mg.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Content:   json.RawMessage(mg.Content),
	})
}

// LockGuide handles POST /v1/guides/{id}/lock.
//
// Locks the specified guide version (idempotent). Returns 200 on success,
// 404 if not found (or belongs to a different tenant).
// Locking does NOT prevent new versions from being imported.
func (h *GuideHandlers) LockGuide(w http.ResponseWriter, r *http.Request) {
	_, tenantID, ok := h.guideAuthz(w, r)
	if !ok {
		return
	}

	idStr := chi.URLParam(r, "id")
	guideID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid guide id", http.StatusBadRequest)
		return
	}

	if err := h.Store.LockGuide(r.Context(), tenantID, guideID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
