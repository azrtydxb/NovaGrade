// Package api implements the HTTP API layer for NovaGrade.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/store"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// validSubmissionStates is the set of known SubmissionState values for input validation.
var validSubmissionStates = map[contracts.SubmissionState]bool{
	contracts.StateUploaded:                    true,
	contracts.StateQueued:                      true,
	contracts.StateSplittingPages:              true,
	contracts.StateExtractingMetadata:          true,
	contracts.StateTranscribing:                true,
	contracts.StateTranscriptionReviewRequired: true,
	contracts.StateGrading:                     true,
	contracts.StateGradingReviewRequired:       true,
	contracts.StateTeacherReview:               true,
	contracts.StateApproved:                    true,
	contracts.StatePublished:                   true,
	contracts.StateExported:                    true,
	contracts.StateArchived:                    true,
	contracts.StateFailed:                      true,
}

// SubmissionStore is the subset of store.Store used by the API handlers.
type SubmissionStore interface {
	CreateSubmission(ctx context.Context, p store.CreateSubmissionParams) (store.Submission, error)
	GetSubmission(ctx context.Context, id uuid.UUID) (store.Submission, error)
	ListSubmissionsByState(ctx context.Context, tenantID uuid.UUID, state contracts.SubmissionState) ([]store.Submission, error)
}

// CommandBus publishes envelopes to the command queue.
type CommandBus interface {
	Publish(ctx context.Context, queue string, env contracts.Envelope) error
}

// ObjectStore stores and retrieves objects by key.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, data []byte) error
	GetObject(ctx context.Context, key string) ([]byte, error)
}

// Handlers holds dependencies for all HTTP handlers.
type Handlers struct {
	Store      SubmissionStore
	Bus        CommandBus
	Objects    ObjectStore
	DeployMode string // "saas" or "onprem"
}

const maxPDFSize = 50 << 20 // 50 MB

// PostSubmission handles POST /v1/submissions.
// Accepts multipart/form-data with a "pdf" file field.
func (h *Handlers) PostSubmission(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionSubmitExam, rctx) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := r.ParseMultipartForm(maxPDFSize); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("pdf")
	if err != nil {
		http.Error(w, "missing pdf field", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if ct := header.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/pdf") {
		http.Error(w, "pdf content-type required", http.StatusBadRequest)
		return
	}

	pdfData, err := io.ReadAll(io.LimitReader(file, maxPDFSize+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if int64(len(pdfData)) > maxPDFSize {
		http.Error(w, "pdf too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Build submission params (without SourcePDFKey — we don't know sub.ID yet).
	params := store.CreateSubmissionParams{
		TenantID: tenantID,
	}
	// Optional form fields
	if avid := r.FormValue("assessment_version_id"); avid != "" {
		if id, err := uuid.Parse(avid); err == nil {
			params.AssessmentVersionID = &id
		}
	}
	if sid := r.FormValue("student_id"); sid != "" {
		if id, err := uuid.Parse(sid); err == nil {
			params.StudentID = &id
		}
	}

	// Create the submission record first so the DB generates the canonical UUID.
	sub, err := h.Store.CreateSubmission(r.Context(), params)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	// Store the PDF at {tenantID}/{sub.ID}/source.pdf so the path matches the
	// canonical submission UUID returned in the 201 response.
	objectKey := fmt.Sprintf("%s/%s/source.pdf", p.TenantID, sub.ID)
	if err := h.Objects.PutObject(r.Context(), objectKey, pdfData); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	env := contracts.Envelope{
		TenantID:      p.TenantID,
		Principal:     p.ID,
		SubmissionID:  sub.ID.String(),
		Stage:         contracts.StageSubmitExam,
		CorrelationID: uuid.New().String(),
	}
	// Best-effort publish; submission is already created. Log on failure.
	if err := h.Bus.Publish(r.Context(), "commands.q", env); err != nil {
		log.Printf("warn: failed to publish SubmitExam command for submission %s: %v", sub.ID, err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"submission_id": sub.ID.String()})
}

// GetSubmission handles GET /v1/submissions/{id}.
func (h *Handlers) GetSubmission(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sub, err := h.Store.GetSubmission(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: sub.TenantID.String(),
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionViewResults, rctx) {
		// Return 404 for cross-tenant (not 403) to avoid tenant enumeration
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sub)
}

// ListSubmissions handles GET /v1/submissions?state=...
func (h *Handlers) ListSubmissions(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: p.TenantID,
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionViewResults, rctx) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	stateParam := contracts.SubmissionState(r.URL.Query().Get("state"))
	if stateParam != "" && !validSubmissionStates[stateParam] {
		http.Error(w, "invalid state value", http.StatusBadRequest)
		return
	}
	subs, err := h.Store.ListSubmissionsByState(r.Context(), tenantID, stateParam)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if subs == nil {
		subs = []store.Submission{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(subs)
}

// GetResult handles GET /v1/submissions/{id}/result.
func (h *Handlers) GetResult(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sub, err := h.Store.GetSubmission(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	rctx := domain.ResourceCtx{
		PrincipalTenants: []string{p.TenantID},
		ResourceTenantID: sub.TenantID.String(),
		DeployMode:       h.DeployMode,
	}
	if !domain.Can(p.Roles, domain.ActionViewResults, rctx) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Check if graded — result is available in approved/published/exported/archived states
	gradedStates := map[contracts.SubmissionState]bool{
		contracts.StateApproved:  true,
		contracts.StatePublished: true,
		contracts.StateExported:  true,
		contracts.StateArchived:  true,
	}
	if !gradedStates[sub.State] {
		http.Error(w, "result not yet available", http.StatusConflict)
		return
	}
	objectKey := fmt.Sprintf("%s/%s/graded.v1.json", sub.TenantID, sub.ID)
	data, err := h.Objects.GetObject(r.Context(), objectKey)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
