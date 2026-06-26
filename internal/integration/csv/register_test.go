package csv_test

import (
	"testing"

	csvconn "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/internal/integration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCSVRegister(t *testing.T) {
	reg := integration.NewRegistry()
	csvconn.Register(reg)

	t.Run("RosterConnector registered as RosterSource", func(t *testing.T) {
		got, ok := reg.Get(integration.CategoryRoster, "csv")
		require.True(t, ok, "expected (CategoryRoster, 'csv') to be registered")
		_, isSource := got.(integration.RosterSource)
		assert.True(t, isSource, "expected RosterSource, got %T", got)
	})

	t.Run("GradeConnector registered as GradeSink", func(t *testing.T) {
		got, ok := reg.Get(integration.CategorySIS, "csv")
		require.True(t, ok, "expected (CategorySIS, 'csv') to be registered")
		_, isSink := got.(integration.GradeSink)
		assert.True(t, isSink, "expected GradeSink, got %T", got)
	})
}
