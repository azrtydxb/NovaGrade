package api

// roster_handler.go — HTTP handler for roster import.
//
// Endpoint:
//   POST /v1/rosters/import?provider=csv  — import a roster from CSV (or other providers)
//
// RBAC: ActionEditTunables required.
// The request body is the raw roster file (e.g., a CSV).
// For each imported student, UpsertStudent is called so that re-imports are idempotent.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/azrtydxb/novagrade/internal/auth"
	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/integration"
	integrationcsv "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/internal/store"
)

// RosterStore is the subset of store required by RosterHandlers.
type RosterStore interface {
	UpsertStudent(ctx context.Context, tenantID uuid.UUID, email, fullName string) (store.Student, error)
}

// RosterHandlers holds dependencies for the roster import handler.
type RosterHandlers struct {
	Store      RosterStore
	Registry   *integration.Registry
	DeployMode string
}

// ImportRoster handles POST /v1/rosters/import.
//
// Query param: ?provider=csv (default: csv)
// Body: roster file content (e.g. CSV).
//
// Response: 200 JSON { "imported": N, "errors": [...] }
func (h *RosterHandlers) ImportRoster(w http.ResponseWriter, r *http.Request) {
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
	if !domain.Can(p.Roles, domain.ActionEditTunables, rctx) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		http.Error(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = "csv"
	}

	// Look up roster source from registry, fallback to built-in CSV.
	var source integration.RosterSource
	if h.Registry != nil {
		if connector, ok := h.Registry.Get(integration.CategoryRoster, provider); ok {
			if rs, ok := connector.(integration.RosterSource); ok {
				source = rs
			}
		}
	}
	if source == nil {
		if provider == "csv" {
			source = integrationcsv.RosterConnector{}
		} else {
			http.Error(w, fmt.Sprintf("unknown provider: %s", provider), http.StatusBadRequest)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	var importErrors []string
	students, importErr := source.ImportRoster(r.Context(), bytes.NewReader(body))
	if importErr != nil {
		importErrors = append(importErrors, importErr.Error())
	}

	imported := 0
	for _, s := range students {
		if _, err := h.Store.UpsertStudent(r.Context(), tenantID, s.Email, s.FullName); err != nil {
			importErrors = append(importErrors, fmt.Sprintf("upsert %s: %v", s.Email, err))
			continue
		}
		imported++
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"imported": imported,
		"errors":   importErrors,
	})
}
