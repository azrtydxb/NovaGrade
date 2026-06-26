package integration

import (
	"context"
	"io"
	"time"

	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/google/uuid"
)

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
