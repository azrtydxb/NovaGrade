package csv_test

import (
	"context"
	"os"
	"testing"

	csvconn "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRosterConnector_ImportRoster_HappyPath(t *testing.T) {
	f, err := os.Open("testdata/roster.csv")
	require.NoError(t, err)
	defer f.Close()

	conn := csvconn.RosterConnector{}
	students, err := conn.ImportRoster(context.Background(), f)
	require.NoError(t, err)
	require.Len(t, students, 3)

	assert.Equal(t, "alice@example.com", students[0].Email)
	assert.Equal(t, "Alice Smith", students[0].FullName)
	assert.Equal(t, "10A", students[0].ClassLabel)

	assert.Equal(t, "bob@example.com", students[1].Email)
	assert.Equal(t, "Bob Jones", students[1].FullName)
	assert.Equal(t, "10B", students[1].ClassLabel)

	assert.Equal(t, "carol@example.com", students[2].Email)
	assert.Equal(t, "Carol White", students[2].FullName)
	assert.Equal(t, "11A", students[2].ClassLabel)
}

func TestRosterConnector_ImportRoster_MalformedRows(t *testing.T) {
	f, err := os.Open("testdata/roster_malformed.csv")
	require.NoError(t, err)
	defer f.Close()

	conn := csvconn.RosterConnector{}
	students, err := conn.ImportRoster(context.Background(), f)

	// Should return 2 valid students and a non-nil error.
	require.Error(t, err)
	require.Len(t, students, 2)

	assert.Equal(t, "good@example.com", students[0].Email)
	assert.Equal(t, "another@example.com", students[1].Email)

	// Error should mention the skipped rows.
	assert.Contains(t, err.Error(), "skipped")
}

func TestRosterConnector_ImportRoster_MissingHeader(t *testing.T) {
	f, err := os.Open("testdata/roster_noheader.csv")
	require.NoError(t, err)
	defer f.Close()

	conn := csvconn.RosterConnector{}
	students, err := conn.ImportRoster(context.Background(), f)

	require.Error(t, err)
	assert.Nil(t, students)
	assert.Contains(t, err.Error(), "header")
}
