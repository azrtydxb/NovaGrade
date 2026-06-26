package integration_test

import (
	"testing"

	"github.com/azrtydxb/novagrade/internal/integration"
)

type fakeConnector struct{ name string }

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := integration.NewRegistry()
	reg.Register(integration.CategoryLMS, "canvas", func() any {
		return &fakeConnector{name: "canvas"}
	})

	got, ok := reg.Get(integration.CategoryLMS, "canvas")
	if !ok {
		t.Fatal("expected to find registered provider")
	}
	c, ok2 := got.(*fakeConnector)
	if !ok2 || c.name != "canvas" {
		t.Fatalf("unexpected connector: %v", got)
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	reg := integration.NewRegistry()
	got, ok := reg.Get(integration.CategoryLMS, "unknown-provider")
	if ok || got != nil {
		t.Fatalf("expected (nil, false) for unknown provider, got (%v, %v)", got, ok)
	}
}
