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

	var students []contracts.RosterStudent

	for _, row := range records[1:] {
		if len(row) == 0 {
			continue
		}

		role := ""
		if roleIdx < len(row) {
			role = strings.ToLower(strings.TrimSpace(row[roleIdx]))
		}
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
		if hasEmail {
			if emailIdx := colIndex["email"]; emailIdx < len(row) {
				email = strings.TrimSpace(row[emailIdx])
			}
		}
		if email == "" && hasUsername {
			if usernameIdx := colIndex["username"]; usernameIdx < len(row) {
				email = strings.TrimSpace(row[usernameIdx])
			}
		}

		students = append(students, contracts.RosterStudent{
			Email:      email,
			FullName:   givenName + " " + familyName,
			ExternalID: sourcedID,
			ClassLabel: "",
		})
	}

	return students, nil
}
