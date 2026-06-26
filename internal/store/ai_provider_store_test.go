package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateAIProviderConfig verifies a basic create returns the domain config
// (which carries no api_key_enc bytes).
func TestCreateAIProviderConfig(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := mustCreateSchool(t, st)

	cfg, err := st.CreateAIProviderConfig(ctx, CreateAIProviderConfigParams{
		TenantID:     tenantID,
		Name:         "primary",
		ProviderType: "openai",
		BaseURL:      "https://api.openai.com",
		Model:        "gpt-4o",
		APIKeyEnc:    []byte("encrypted-bytes"),
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, cfg.ID)
	assert.Equal(t, tenantID, cfg.TenantID)
	assert.Equal(t, "primary", cfg.Name)
	assert.Equal(t, "openai", cfg.ProviderType)
	assert.Equal(t, "https://api.openai.com", cfg.BaseURL)
	assert.Equal(t, "gpt-4o", cfg.Model)
	assert.False(t, cfg.IsDefault)
}

// TestListAIProviderConfigs_NoKey verifies the list result contains entries but
// never carries api_key_enc (the AIProviderConfig domain type has no such field,
// so leakage is structurally impossible).
func TestListAIProviderConfigs_NoKey(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := mustCreateSchool(t, st)

	_, err := st.CreateAIProviderConfig(ctx, CreateAIProviderConfigParams{
		TenantID: tenantID, Name: "a", ProviderType: "vllm",
		BaseURL: "http://localhost:8000", Model: "default", APIKeyEnc: []byte("secret-a"),
	})
	require.NoError(t, err)
	_, err = st.CreateAIProviderConfig(ctx, CreateAIProviderConfigParams{
		TenantID: tenantID, Name: "b", ProviderType: "azure_openai",
		BaseURL: "https://x.openai.azure.com", Model: "gpt-4o", APIKeyEnc: []byte("secret-b"),
	})
	require.NoError(t, err)

	list, err := st.ListAIProviderConfigs(ctx, tenantID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	// Ordered by name.
	assert.Equal(t, "a", list[0].Name)
	assert.Equal(t, "b", list[1].Name)
}

// TestGetDefaultAIProviderConfigWithKey_ReturnsEncryptedBytes verifies that the
// exact encrypted bytes stored are returned by the default getter.
func TestGetDefaultAIProviderConfigWithKey_ReturnsEncryptedBytes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := mustCreateSchool(t, st)

	known := []byte{0x00, 0x01, 0x02, 0xde, 0xad, 0xbe, 0xef}
	cfg, err := st.CreateAIProviderConfig(ctx, CreateAIProviderConfigParams{
		TenantID: tenantID, Name: "primary", ProviderType: "openai",
		BaseURL: "https://api.openai.com", Model: "gpt-4o", APIKeyEnc: known,
	})
	require.NoError(t, err)

	require.NoError(t, st.SetDefaultAIProviderConfig(ctx, tenantID, cfg.ID))

	got, encBytes, err := st.GetDefaultAIProviderConfigWithKey(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, cfg.ID, got.ID)
	assert.True(t, got.IsDefault)
	assert.Equal(t, known, encBytes, "exact encrypted bytes must round-trip")
}

// TestGetDefaultAIProviderConfigWithKey_NotFound verifies ErrNotFound when no
// default exists for the tenant.
func TestGetDefaultAIProviderConfigWithKey_NotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := mustCreateSchool(t, st)

	_, _, err := st.GetDefaultAIProviderConfigWithKey(ctx, tenantID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

// TestSetDefaultAIProviderConfig_ClearsPrior verifies that setting a new default
// clears the previously-default row (only one default per tenant).
func TestSetDefaultAIProviderConfig_ClearsPrior(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := mustCreateSchool(t, st)

	first, err := st.CreateAIProviderConfig(ctx, CreateAIProviderConfigParams{
		TenantID: tenantID, Name: "first", ProviderType: "vllm",
		BaseURL: "http://a", Model: "m1", APIKeyEnc: []byte("k1"),
	})
	require.NoError(t, err)
	second, err := st.CreateAIProviderConfig(ctx, CreateAIProviderConfigParams{
		TenantID: tenantID, Name: "second", ProviderType: "vllm",
		BaseURL: "http://b", Model: "m2", APIKeyEnc: []byte("k2"),
	})
	require.NoError(t, err)

	require.NoError(t, st.SetDefaultAIProviderConfig(ctx, tenantID, first.ID))
	got, _, err := st.GetDefaultAIProviderConfigWithKey(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, first.ID, got.ID)

	// Switch the default to the second config — first must be cleared.
	require.NoError(t, st.SetDefaultAIProviderConfig(ctx, tenantID, second.ID))
	got, _, err = st.GetDefaultAIProviderConfigWithKey(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, got.ID, "second config must now be the only default")
}

// TestSetDefaultAIProviderConfig_NotFound verifies ErrNotFound when the id does
// not belong to the tenant (cross-tenant or missing).
func TestSetDefaultAIProviderConfig_NotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := mustCreateSchool(t, st)

	err := st.SetDefaultAIProviderConfig(ctx, tenantID, uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

// TestPartialIndex_OnlyOneDefault verifies the partial unique index prevents two
// rows with is_default=true for the same tenant.
func TestPartialIndex_OnlyOneDefault(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := mustCreateSchool(t, st)

	_, err := st.pool.Exec(ctx,
		`INSERT INTO ai_provider_config (tenant_id, name, provider_type, base_url, model, is_default)
		 VALUES ($1, 'd1', 'vllm', 'http://a', 'm', true)`, tenantID)
	require.NoError(t, err)

	// Second default for the same tenant must violate the partial unique index.
	_, err = st.pool.Exec(ctx,
		`INSERT INTO ai_provider_config (tenant_id, name, provider_type, base_url, model, is_default)
		 VALUES ($1, 'd2', 'vllm', 'http://b', 'm', true)`, tenantID)
	require.Error(t, err, "second is_default=true row must be rejected")
	assert.Contains(t, err.Error(), "ai_provider_one_default")
}
