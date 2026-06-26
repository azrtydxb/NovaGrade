package providers

import (
	"context"

	"github.com/google/uuid"
)

// ProviderConfig holds decrypted config for building a provider.
// APIKey is already DECRYPTED by the ConfigSource — the registry never handles
// encryption or encrypted bytes.
type ProviderConfig struct {
	ProviderType string
	BaseURL      string
	Model        string
	APIKey       string // DECRYPTED
}

// ConfigSource resolves the default provider config for a tenant.
// Implementations must decrypt the api_key before returning it.
type ConfigSource interface {
	DefaultConfig(ctx context.Context, tenantID uuid.UUID) (ProviderConfig, error)
}

// Registry resolves the correct AIProvider for a tenant, falling back to the
// env-configured default when no tenant config exists.
//
// All four provider_types (openai, azure_openai, vllm, self_hosted) speak the
// OpenAI-compatible HTTP format, so every tenant config builds a VLLMProvider.
type Registry struct {
	Source        ConfigSource
	Fallback      AIProvider
	FallbackModel string
	PriceTable    map[string]ModelPrice
	LogSink       func(AICallLog)
}

// Resolve returns the AIProvider and model string for the given tenant.
//
// If Source is nil, returns an error, or has no config for the tenant, the
// (Fallback, FallbackModel) pair is returned. Resolve never panics.
func (r *Registry) Resolve(ctx context.Context, tenantID uuid.UUID) (AIProvider, string) {
	if r.Source == nil {
		return r.Fallback, r.FallbackModel
	}
	cfg, err := r.Source.DefaultConfig(ctx, tenantID)
	if err != nil {
		return r.Fallback, r.FallbackModel
	}

	prov := NewVLLMProvider(VLLMConfig{
		BaseURL:    cfg.BaseURL,
		APIKey:     cfg.APIKey,
		PriceTable: r.PriceTable,
		LogSink:    r.LogSink,
	})
	return prov, cfg.Model
}
