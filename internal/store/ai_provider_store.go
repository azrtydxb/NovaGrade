package store

// ai_provider_store.go — Store methods for per-tenant AI provider configuration.
//
// CALLER ENCRYPTS: api_key_enc bytes are ALREADY ENCRYPTED by the caller; the
// store has no knowledge of encryption keys. Callers MUST encrypt the API key
// with secrets.Encrypt before passing it here and decrypt after retrieval with
// secrets.Decrypt. This mirrors the discipline in integration_store.go.
//
// api_key_enc is NEVER logged and is NEVER returned in List operations — the
// AIProviderConfig domain type has no key field, so list/create responses
// cannot leak it. Only GetDefaultAIProviderConfigWithKey returns the encrypted
// bytes, and only to a trusted in-process caller (the provider Registry adapter).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/azrtydxb/novagrade/internal/store/db"
)

// ─────────────────────────────────────────────────────────────────────────────
// Domain types
// ─────────────────────────────────────────────────────────────────────────────

// AIProviderConfig is the public domain representation of an ai_provider_config
// row WITHOUT the api_key_enc bytes. It is used for create and list responses.
type AIProviderConfig struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Name         string
	ProviderType string
	BaseURL      string
	Model        string
	IsDefault    bool
	CreatedAt    time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// Params
// ─────────────────────────────────────────────────────────────────────────────

// CreateAIProviderConfigParams carries the fields needed to register a provider.
// APIKeyEnc must be ALREADY ENCRYPTED by the caller; pass nil for no key.
type CreateAIProviderConfigParams struct {
	TenantID     uuid.UUID
	Name         string
	ProviderType string
	BaseURL      string
	Model        string
	APIKeyEnc    []byte // ALREADY ENCRYPTED
}

// ─────────────────────────────────────────────────────────────────────────────
// Store methods
// ─────────────────────────────────────────────────────────────────────────────

// CreateAIProviderConfig inserts a new provider config and returns it (without
// the encrypted key bytes). A duplicate (tenant_id, name) returns ErrDuplicate.
func (s *Store) CreateAIProviderConfig(ctx context.Context, p CreateAIProviderConfigParams) (AIProviderConfig, error) {
	row, err := s.queries.InsertAIProviderConfig(ctx, db.InsertAIProviderConfigParams{
		TenantID:     p.TenantID,
		Name:         p.Name,
		ProviderType: p.ProviderType,
		BaseUrl:      p.BaseURL,
		Model:        p.Model,
		ApiKeyEnc:    p.APIKeyEnc,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return AIProviderConfig{}, fmt.Errorf("store: CreateAIProviderConfig: duplicate name %q: %w", p.Name, ErrDuplicate)
		}
		return AIProviderConfig{}, fmt.Errorf("store: CreateAIProviderConfig: %w", err)
	}
	return aiProviderFromInsertRow(row), nil
}

// ListAIProviderConfigs returns all provider configs for the tenant, ordered by
// name. The result NEVER includes the encrypted key bytes.
func (s *Store) ListAIProviderConfigs(ctx context.Context, tenantID uuid.UUID) ([]AIProviderConfig, error) {
	rows, err := s.queries.ListAIProviderConfigs(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: ListAIProviderConfigs: %w", err)
	}
	out := make([]AIProviderConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, AIProviderConfig{
			ID:           r.ID,
			TenantID:     r.TenantID,
			Name:         r.Name,
			ProviderType: r.ProviderType,
			BaseURL:      r.BaseUrl,
			Model:        r.Model,
			IsDefault:    r.IsDefault,
			CreatedAt:    r.CreatedAt.Time,
		})
	}
	return out, nil
}

// GetDefaultAIProviderConfigWithKey returns the tenant's default provider config
// together with its ALREADY-ENCRYPTED api_key_enc bytes. The caller is
// responsible for decrypting the bytes with secrets.Decrypt. Returns ErrNotFound
// when the tenant has no default provider configured.
func (s *Store) GetDefaultAIProviderConfigWithKey(ctx context.Context, tenantID uuid.UUID) (AIProviderConfig, []byte, error) {
	row, err := s.queries.GetDefaultAIProviderConfigWithKey(ctx, tenantID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AIProviderConfig{}, nil, fmt.Errorf("GetDefaultAIProviderConfigWithKey %s: %w", tenantID, ErrNotFound)
		}
		return AIProviderConfig{}, nil, fmt.Errorf("store: GetDefaultAIProviderConfigWithKey: %w", err)
	}
	return aiProviderFromRow(row), row.ApiKeyEnc, nil
}

// SetDefaultAIProviderConfig makes the given config the tenant's default,
// clearing any prior default in the same transaction. The partial unique index
// guarantees at most one default per tenant. Returns ErrNotFound when the id
// does not belong to the tenant (or does not exist).
func (s *Store) SetDefaultAIProviderConfig(ctx context.Context, tenantID, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: SetDefaultAIProviderConfig: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := db.New(tx)
	if err := q.ClearTenantDefaultAIProvider(ctx, tenantID); err != nil {
		return fmt.Errorf("store: SetDefaultAIProviderConfig: clear default: %w", err)
	}
	n, err := q.SetAIProviderDefault(ctx, db.SetAIProviderDefaultParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		return fmt.Errorf("store: SetDefaultAIProviderConfig: set default: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("SetDefaultAIProviderConfig %s: %w", id, ErrNotFound)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: SetDefaultAIProviderConfig: commit: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Mappers
// ─────────────────────────────────────────────────────────────────────────────

func aiProviderFromRow(r db.AiProviderConfig) AIProviderConfig {
	return AIProviderConfig{
		ID:           r.ID,
		TenantID:     r.TenantID,
		Name:         r.Name,
		ProviderType: r.ProviderType,
		BaseURL:      r.BaseUrl,
		Model:        r.Model,
		IsDefault:    r.IsDefault,
		CreatedAt:    r.CreatedAt.Time,
	}
}

func aiProviderFromInsertRow(r db.AiProviderConfig) AIProviderConfig {
	return aiProviderFromRow(r)
}
