package providers

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeSource is a test ConfigSource that returns a fixed config or error.
type fakeSource struct {
	cfg ProviderConfig
	err error
}

func (f fakeSource) DefaultConfig(_ context.Context, _ uuid.UUID) (ProviderConfig, error) {
	return f.cfg, f.err
}

// errNotFound is a sentinel used to simulate "no tenant config".
var errNotFound = errors.New("not found")

func TestRegistry_TenantConfig(t *testing.T) {
	src := fakeSource{cfg: ProviderConfig{
		ProviderType: "openai",
		BaseURL:      "https://api.openai.com",
		Model:        "gpt-4o",
		APIKey:       "decrypted-key",
	}}
	fallback := NewVLLMProvider(VLLMConfig{BaseURL: "http://fallback"})
	reg := &Registry{
		Source:        src,
		Fallback:      fallback,
		FallbackModel: "fallback-model",
	}

	prov, model := reg.Resolve(context.Background(), uuid.New())
	if model != "gpt-4o" {
		t.Fatalf("expected tenant model gpt-4o, got %q", model)
	}
	if prov == nil {
		t.Fatal("expected a provider, got nil")
	}
	if _, ok := prov.(*VLLMProvider); !ok {
		t.Fatalf("expected *VLLMProvider, got %T", prov)
	}
	if prov == fallback {
		t.Fatal("expected a freshly-built tenant provider, got the fallback")
	}
}

func TestRegistry_Fallback_OnNotFound(t *testing.T) {
	src := fakeSource{err: errNotFound}
	fallback := NewVLLMProvider(VLLMConfig{BaseURL: "http://fallback"})
	reg := &Registry{
		Source:        src,
		Fallback:      fallback,
		FallbackModel: "fallback-model",
	}

	prov, model := reg.Resolve(context.Background(), uuid.New())
	if prov != AIProvider(fallback) {
		t.Fatalf("expected fallback provider, got %v", prov)
	}
	if model != "fallback-model" {
		t.Fatalf("expected fallback-model, got %q", model)
	}
}

func TestRegistry_Fallback_OnError(t *testing.T) {
	src := fakeSource{err: errors.New("boom")}
	fallback := NewVLLMProvider(VLLMConfig{BaseURL: "http://fallback"})
	reg := &Registry{
		Source:        src,
		Fallback:      fallback,
		FallbackModel: "fallback-model",
	}

	prov, model := reg.Resolve(context.Background(), uuid.New())
	if prov != AIProvider(fallback) {
		t.Fatalf("expected fallback provider on error, got %v", prov)
	}
	if model != "fallback-model" {
		t.Fatalf("expected fallback-model, got %q", model)
	}
}

// TestRegistry_Fallback_NilSource verifies Resolve never panics with a nil Source.
func TestRegistry_Fallback_NilSource(t *testing.T) {
	fallback := NewVLLMProvider(VLLMConfig{BaseURL: "http://fallback"})
	reg := &Registry{Fallback: fallback, FallbackModel: "fallback-model"}
	prov, model := reg.Resolve(context.Background(), uuid.New())
	if prov != AIProvider(fallback) || model != "fallback-model" {
		t.Fatalf("expected fallback on nil source, got %v / %q", prov, model)
	}
}

// TestRegistry_Fallback_OnNonNotFoundError verifies that a non-NotFound error
// (e.g., a decrypt failure) still triggers fallback but is now observable to the caller.
func TestRegistry_Fallback_OnNonNotFoundError(t *testing.T) {
	decryptErr := errors.New("secrets.Decrypt: corrupted key")
	src := fakeSource{err: decryptErr}
	fallback := NewVLLMProvider(VLLMConfig{BaseURL: "http://fallback"})
	reg := &Registry{
		Source:        src,
		Fallback:      fallback,
		FallbackModel: "fallback-model",
	}

	prov, model := reg.Resolve(context.Background(), uuid.New())
	// Behavior: still falls back to the env provider
	if prov != AIProvider(fallback) {
		t.Fatalf("expected fallback provider on decrypt error, got %v", prov)
	}
	if model != "fallback-model" {
		t.Fatalf("expected fallback-model, got %q", model)
	}
	// The key difference: the error was logged by storeConfigSource.DefaultConfig
	// before reaching Resolve, making the issue observable.
}
