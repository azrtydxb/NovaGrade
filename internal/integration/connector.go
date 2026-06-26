package integration

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/google/uuid"
)

// RosterImportError is returned by RosterSource.ImportRoster when one or more
// rows were skipped. It carries the count of skipped rows and a detail message
// per row. The partial student list is still returned alongside this error.
// Callers should use errors.As to extract structured skipped-row information.
type RosterImportError struct {
	Skipped int
	Details []string
}

func (e *RosterImportError) Error() string {
	return fmt.Sprintf("roster import: skipped %d row(s): %s", e.Skipped, strings.Join(e.Details, "; "))
}

// Category identifies the functional role of an integration.
type Category string

const (
	CategoryLMS     Category = "lms"
	CategorySIS     Category = "sis"
	CategoryRoster  Category = "roster"
	CategoryStorage Category = "storage"
)

// Connection is the domain model for a saved integration connection.
type Connection struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Category  Category
	Provider  string
	Status    string
	Config    map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RosterSource can import a student roster from an io.Reader.
type RosterSource interface {
	ImportRoster(ctx context.Context, r io.Reader) ([]contracts.RosterStudent, error)
}

// GradeSink can export grade rows to an io.Writer.
type GradeSink interface {
	ExportGrades(ctx context.Context, w io.Writer, rows []contracts.GradeRow) error
}
