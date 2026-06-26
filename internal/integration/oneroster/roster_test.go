package oneroster_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/azrtydxb/novagrade/internal/integration/oneroster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOneRosterConnector_ImportRoster_StudentsOnly(t *testing.T) {
	f, err := os.Open("testdata/oneroster_users.csv")
	require.NoError(t, err)
	defer f.Close()

	conn := oneroster.RosterConnector{}
	students, err := conn.ImportRoster(context.Background(), f)
	require.NoError(t, err)
	require.Len(t, students, 3, "only 3 students expected (teacher and admin filtered out)")

	// alice (s001)
	assert.Equal(t, "alice@school.edu", students[0].Email)
	assert.Equal(t, "Alice Smith", students[0].FullName)
	assert.Equal(t, "s001", students[0].ExternalID)
	assert.Empty(t, students[0].ClassLabel)

	// carol (s002) — role "Student" (mixed case)
	assert.Equal(t, "carol@school.edu", students[1].Email)
	assert.Equal(t, "Carol White", students[1].FullName)
	assert.Equal(t, "s002", students[1].ExternalID)

	// dave (s003) — empty email, falls back to username
	assert.Equal(t, "dave_b", students[2].Email)
	assert.Equal(t, "Dave Brown", students[2].FullName)
	assert.Equal(t, "s003", students[2].ExternalID)
}

func TestOneRosterConnector_ImportRoster_MissingRequiredHeader(t *testing.T) {
	csv := "username,givenName,familyName\nalice,Alice,Smith\n"
	conn := oneroster.RosterConnector{}
	students, err := conn.ImportRoster(context.Background(), strings.NewReader(csv))

	require.Error(t, err)
	assert.Nil(t, students)
	assert.Contains(t, err.Error(), "missing required column")
}
