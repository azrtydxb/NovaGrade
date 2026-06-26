package api

// guide_preview_handler.go — HTTP handler for the guide preview endpoint.
//
// Endpoint:
//   POST /v1/guides/preview
//
// Purpose:
//   Lets a teacher validate a marking guide by running it against sample answers
//   BEFORE using it for real grading — deterministically, without persisting
//   anything and without calling any AI model.
//
// RBAC:
//   Requires ActionEditTunables (same as guide management). Roles: operator,
//   group_admin, school_admin. Teacher and scanner are denied → 404.
//
// Stateless guarantees:
//   - No store reads or writes.
//   - No AI provider calls.
//   - rubric entries and unknown/absent match types return Previewable=false
//     with a justification; no model is invoked.

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/pipeline/grade"
)

// GuidePreviewHandlers holds dependencies for the guide preview handler.
// It is intentionally separate from GuideHandlers because it requires no store
// (the preview is stateless).
type GuidePreviewHandlers struct {
	DeployMode string // "saas" or "onprem"
}

// previewRequest is the JSON body for POST /v1/guides/preview.
type previewRequest struct {
	Guide   grade.Guide           `json:"guide"`
	Samples []grade.PreviewSample `json:"samples"`
}

// previewResponse is the JSON body returned by POST /v1/guides/preview.
type previewResponse struct {
	Results []grade.PreviewResult `json:"results"`
}

const maxPreviewBodySize = 5 << 20 // 5 MB

// previewAuthz extracts and validates the principal and checks ActionEditTunables.
// On denial it writes an HTTP response and returns (zero, false).
// Mirrors guideAuthz in guide_handler.go (same 404-on-denial convention).
func (h *GuidePreviewHandlers) previewAuthz(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return auth.Principal{}, false
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionEditTunables, rctx) {
		// 404, not 403 — prevents role/tenant enumeration (matches guide_handler.go convention).
		http.Error(w, "not found", http.StatusNotFound)
		return auth.Principal{}, false
	}
	return p, true
}

// Preview handles POST /v1/guides/preview.
//
// Steps:
//  1. Auth + RBAC (ActionEditTunables). 401/404 on failure.
//  2. Read + parse body JSON → 400 on parse error.
//  3. Validate guide via grade.ValidateGuide → 400 with detail if invalid.
//  4. Run grade.PreviewGuide (pure, no provider, no persistence).
//  5. Return 200 + {results: [...]}.
func (h *GuidePreviewHandlers) Preview(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.previewAuthz(w, r); !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPreviewBodySize+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if int64(len(body)) > maxPreviewBodySize {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req previewRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the guide content before running preview.
	if err := grade.ValidateGuide(req.Guide); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	results := grade.PreviewGuide(req.Guide, req.Samples)

	// Guarantee non-nil slice so JSON encodes as [] not null.
	if results == nil {
		results = []grade.PreviewResult{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(previewResponse{Results: results})
}
