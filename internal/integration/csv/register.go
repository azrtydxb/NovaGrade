package csv

import "github.com/azrtydxb/novagrade/internal/integration"

// Register wires the CSV connectors into reg:
//
//   - (CategoryRoster, "csv")  → RosterConnector
//   - (CategorySIS,    "csv")  → GradeConnector
func Register(reg *integration.Registry) {
	reg.Register(integration.CategoryRoster, "csv", func() any {
		return RosterConnector{}
	})
	reg.Register(integration.CategorySIS, "csv", func() any {
		return GradeConnector{}
	})
}
