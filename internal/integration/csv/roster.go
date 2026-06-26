package csv

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// RosterConnector implements integration.RosterSource by parsing a CSV roster.
// The zero value is ready to use.
type RosterConnector struct{}

// ImportRoster reads a CSV from r and returns a slice of RosterStudent.
//
// The header row must contain at least "email" and "full_name" columns; a
// "class" column is optional. Blank lines are skipped. Rows with the wrong
// column count or an empty email (after trimming) are collected as malformed;
// the method returns the partial valid result plus a single error listing all
// skipped rows. If no rows are malformed, the error is nil.
func (c RosterConnector) ImportRoster(_ context.Context, r io.Reader) ([]contracts.RosterStudent, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // allow variable; we validate ourselves

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("csv roster: read: %w", err)
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("csv roster: empty file")
	}

	// Parse header.
	header := records[0]
	colIndex := make(map[string]int, len(header))
	for i, h := range header {
		colIndex[strings.TrimSpace(strings.ToLower(h))] = i
	}

	emailIdx, hasEmail := colIndex["email"]
	nameIdx, hasName := colIndex["full_name"]
	if !hasEmail || !hasName {
		return nil, fmt.Errorf("csv roster: header must contain 'email' and 'full_name' columns")
	}
	classIdx, hasClass := colIndex["class"]

	var students []contracts.RosterStudent
	var malformed []string

	for rowNum, row := range records[1:] {
		lineNum := rowNum + 2 // 1-based, skip header

		// Skip truly blank lines (single empty field or empty slice).
		if len(row) == 0 || (len(row) == 1 && strings.TrimSpace(row[0]) == "") {
			continue
		}

		// Must have at least enough columns to read email and full_name.
		// class is optional — do NOT include classIdx here; it is guarded
		// separately below with "hasClass && classIdx < len(row)".
		maxRequired := emailIdx
		if nameIdx > maxRequired {
			maxRequired = nameIdx
		}
		if len(row) <= maxRequired {
			malformed = append(malformed, fmt.Sprintf("line %d: wrong column count (%d)", lineNum, len(row)))
			continue
		}

		email := strings.TrimSpace(row[emailIdx])
		if email == "" {
			malformed = append(malformed, fmt.Sprintf("line %d: empty email", lineNum))
			continue
		}

		s := contracts.RosterStudent{
			Email:    email,
			FullName: strings.TrimSpace(row[nameIdx]),
		}
		if hasClass && classIdx < len(row) {
			s.ClassLabel = strings.TrimSpace(row[classIdx])
		}
		students = append(students, s)
	}

	var retErr error
	if len(malformed) > 0 {
		retErr = fmt.Errorf("csv roster: skipped %d malformed row(s): %s", len(malformed), strings.Join(malformed, "; "))
	}
	return students, retErr
}
