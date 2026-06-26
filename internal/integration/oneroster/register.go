package oneroster

import "github.com/azrtydxb/novagrade/internal/integration"

// Register wires the OneRoster connectors into reg:
//
//   - (CategoryRoster, "oneroster") → RosterConnector
func Register(reg *integration.Registry) {
	reg.Register(integration.CategoryRoster, "oneroster", func() any {
		return RosterConnector{}
	})
}
