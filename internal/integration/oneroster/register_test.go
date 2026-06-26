package oneroster_test

import (
	"testing"

	"github.com/azrtydxb/novagrade/internal/integration"
	"github.com/azrtydxb/novagrade/internal/integration/oneroster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOneRosterRegister(t *testing.T) {
	reg := integration.NewRegistry()
	oneroster.Register(reg)

	got, ok := reg.Get(integration.CategoryRoster, "oneroster")
	require.True(t, ok, "expected (CategoryRoster, 'oneroster') to be registered")
	_, isSource := got.(integration.RosterSource)
	assert.True(t, isSource, "expected RosterSource, got %T", got)
}
