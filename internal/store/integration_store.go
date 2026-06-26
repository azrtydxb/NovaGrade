// Package store — integration_store.go
//
// CALLER ENCRYPTS: This file persists integration connection credentials as raw []byte.
// The store has no knowledge of encryption keys. Callers MUST encrypt credentials with
// secrets.Encrypt before passing them here, and decrypt after retrieval with secrets.Decrypt.
// Credentials are NEVER logged or returned in list operations.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/azrtydxb/novagrade/internal/integration"
	"github.com/azrtydxb/novagrade/internal/store/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UpsertConnectionParams are the inputs for creating or updating an integration connection.
// Credentials must be ALREADY ENCRYPTED by the caller via secrets.Encrypt; pass nil if no creds.
type UpsertConnectionParams struct {
	TenantID    uuid.UUID
	Category    integration.Category
	Provider    string
	Config      json.RawMessage
	Credentials []byte // ALREADY ENCRYPTED; nil means no credentials
	Status      string // optional; defaults to "active" if empty
}

func domainConnection(row db.IntegrationConnection) integration.Connection {
	var cfg map[string]any
	if row.Config != nil {
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			log.Printf("store: integration_connection %s: bad config json: %v", row.ID, err)
		}
	}
	return integration.Connection{
		ID:        row.ID,
		TenantID:  row.TenantID,
		Category:  integration.Category(row.Category),
		Provider:  row.Provider,
		Status:    row.Status,
		Config:    cfg,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}

// UpsertConnection inserts or updates an integration connection.
func (s *Store) UpsertConnection(ctx context.Context, p UpsertConnectionParams) (integration.Connection, error) {
	status := p.Status
	if status == "" {
		status = "active"
	}
	cfg := p.Config
	if cfg == nil {
		cfg = json.RawMessage("{}")
	}
	row, err := s.queries.UpsertIntegrationConnection(ctx, db.UpsertIntegrationConnectionParams{
		TenantID:    p.TenantID,
		Category:    string(p.Category),
		Provider:    p.Provider,
		Config:      cfg,
		Credentials: p.Credentials,
		Status:      status,
	})
	if err != nil {
		return integration.Connection{}, fmt.Errorf("store: upsert connection: %w", err)
	}
	return domainConnection(row), nil
}

// GetConnectionWithCreds retrieves a connection including its encrypted credential bytes.
func (s *Store) GetConnectionWithCreds(ctx context.Context, tenantID, id uuid.UUID) (integration.Connection, []byte, error) {
	row, err := s.queries.GetIntegrationConnectionWithCreds(ctx, db.GetIntegrationConnectionWithCredsParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return integration.Connection{}, nil, ErrNotFound
		}
		return integration.Connection{}, nil, fmt.Errorf("store: get connection with creds: %w", err)
	}
	return domainConnection(row), row.Credentials, nil
}

// ListConnections returns all connections for a tenant WITHOUT credentials.
func (s *Store) ListConnections(ctx context.Context, tenantID uuid.UUID) ([]integration.Connection, error) {
	rows, err := s.queries.ListIntegrationConnections(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list connections: %w", err)
	}
	out := make([]integration.Connection, 0, len(rows))
	for _, r := range rows {
		var cfg map[string]any
		if r.Config != nil {
			if err := json.Unmarshal(r.Config, &cfg); err != nil {
				log.Printf("store: integration_connection %s: bad config json: %v", r.ID, err)
			}
		}
		out = append(out, integration.Connection{
			ID:        r.ID,
			TenantID:  r.TenantID,
			Category:  integration.Category(r.Category),
			Provider:  r.Provider,
			Status:    r.Status,
			Config:    cfg,
			CreatedAt: r.CreatedAt.Time,
			UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// DeleteConnection deletes a connection by id and tenantID. Returns ErrNotFound if 0 rows affected.
func (s *Store) DeleteConnection(ctx context.Context, tenantID, id uuid.UUID) error {
	n, err := s.queries.DeleteIntegrationConnection(ctx, db.DeleteIntegrationConnectionParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		return fmt.Errorf("store: delete connection: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
