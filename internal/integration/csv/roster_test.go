package csv_test

import (
	"context"
	"os"
	"testing"

	csvconn "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRosterConnector_ImportRoster(t *testing.T) {
	tests := []struct {
		name          string
		file          string
		wantErr       bool
		errContains   string
		wantStudents  []contracts.RosterStudent
		wantLen       int
	}{
		{
			name:    "happy path",
			file:    "testdata/roster.csv",
			wantLen: 3,
			wantStudents: []contracts.RosterStudent{
				{Email: "alice@example.com", FullName: "Alice Smith", ClassLabel: "10A"},
				{Email: "bob@example.com", FullName: "Bob Jones", ClassLabel: "10B"},
				{Email: "carol@example.com", FullName: "Carol White", ClassLabel: "11A"},
			},
		},
		{
			name:        "malformed rows",
			file:        "testdata/roster_malformed.csv",
			wantErr:     true,
			errContains: "skipped",
			wantLen:     2,
			wantStudents: []contracts.RosterStudent{
				{Email: "good@example.com"},
				{Email: "another@example.com"},
			},
		},
		{
			name:        "missing header",
			file:        "testdata/roster_noheader.csv",
			wantErr:     true,
			errContains: "header",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open(tc.file)
			require.NoError(t, err)
			defer f.Close()

			conn := csvconn.RosterConnector{}
			students, err := conn.ImportRoster(context.Background(), f)

			if tc.wantErr {
				require.Error(t, err)
				if tc.errContains != "" {
					assert.Contains(t, err.Error(), tc.errContains)
				}
			} else {
				require.NoError(t, err)
			}

			if tc.wantStudents == nil && !tc.wantErr {
				assert.Nil(t, students)
				return
			}

			if tc.wantLen > 0 {
				require.Len(t, students, tc.wantLen)
			}

			for i, want := range tc.wantStudents {
				if i >= len(students) {
					break
				}
				if want.Email != "" {
					assert.Equal(t, want.Email, students[i].Email)
				}
				if want.FullName != "" {
					assert.Equal(t, want.FullName, students[i].FullName)
				}
				if want.ClassLabel != "" {
					assert.Equal(t, want.ClassLabel, students[i].ClassLabel)
				}
			}
		})
	}
}
