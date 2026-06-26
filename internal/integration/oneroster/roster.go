package oneroster

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// RosterConnector implements integration.RosterSource by parsing a OneRoster
// v1.1 users.csv file.
// The zero value is ready to use.
type RosterConnector struct{}

// ImportRoster reads a OneRoster v1.1 users.csv from r and returns students.
//
// Required header columns: sourcedId, givenName, familyName, role, and at
// least one of email or username. Rows where role (case-insensitive) is not
// "student" are ignored. When email is blank, username is used as the email
// fallback. ClassLabel is left empty (not present in users.csv).
func (c RosterConnector) ImportRoster(_ context.Context, r io.Reader) ([]contracts.RosterStudent, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("oneroster roster: read: %w", err)
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("oneroster roster: empty file")
	}

	// Parse header.
	header := records[0]
	colIndex := make(map[string]int, len(header))
	for i, h := range header {
		colIndex[strings.TrimSpace(strings.ToLower(h))] = i
	}

	required := []string{"sourcedid", "givenname", "familyname", "role"}
	for _, col := range required {
		if _, ok := colIndex[col]; !ok {
			return nil, fmt.Errorf("oneroster roster: missing required column %q", col)
		}
	}

	_, hasEmail := colIndex["email"]
	_, hasUsername := colIndex["username"]
	if !hasEmail && !hasUsername {
		return nil, fmt.Errorf("oneroster roster: header must contain 'email' or 'username'")
	}

	sourcedIDIdx := colIndex["sourcedid"]
	givenNameIdx := colIndex["givenname"]
	familyNameIdx := colIndex["familyname"]
	roleIdx := colIndex["role"]

	// Resolve optional column indices once before the loop.
	emailIdx := -1
	if hasEmail {
		emailIdx = colIndex["email"]
	}
	usernameIdx := -1
	if hasUsername {
		usernameIdx = colIndex["username"]
	}

	headerLen := len(header)
	var students []contracts.RosterStudent
	var skipped []string

	for rowNum, row := range records[1:] {
		lineNum := rowNum + 2 // 1-based, skip header

		if len(row) == 0 {
			continue
		}
		// Skip malformed rows that are shorter than the header.
		if len(row) < headerLen {
			skipped = append(skipped, fmt.Sprintf("line %d: short row (%d cols)", lineNum, len(row)))
			continue
		}

		role := ""
		if roleIdx < len(row) {
			role = strings.ToLower(strings.TrimSpace(row[roleIdx]))
		}
		// Skip non-student rows (normal filtering, not a malformed-row skip).
		if role != "student" {
			continue
		}

		sourcedID := ""
		if sourcedIDIdx < len(row) {
			sourcedID = strings.TrimSpace(row[sourcedIDIdx])
		}
		givenName := ""
		if givenNameIdx < len(row) {
			givenName = strings.TrimSpace(row[givenNameIdx])
		}
		familyName := ""
		if familyNameIdx < len(row) {
			familyName = strings.TrimSpace(row[familyNameIdx])
		}

		email := ""
		if emailIdx >= 0 && emailIdx < len(row) {
			email = strings.TrimSpace(row[emailIdx])
		}
		if email == "" && usernameIdx >= 0 && usernameIdx < len(row) {
			email = strings.TrimSpace(row[usernameIdx])
		}
		// Per OneRoster v1.1 spec, email is optional; username is also optional.
		// Skip rows where neither provides a usable identifier — there is no safe
		// key to use for grade delivery. Report as malformed-row skip (no silent skip).
		if email == "" {
			skipped = append(skipped, fmt.Sprintf("line %d: student row has no email/username", lineNum))
			continue
		}

		students = append(students, contracts.RosterStudent{
			Email:      email,
			FullName:   strings.TrimSpace(givenName + " " + familyName),
			ExternalID: sourcedID,
			ClassLabel: "",
		})
	}

	var retErr error
	if len(skipped) > 0 {
		retErr = fmt.Errorf("oneroster: skipped %d row(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return students, retErr
}
