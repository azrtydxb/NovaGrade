package oneroster_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/azrtydxb/novagrade/internal/integration/oneroster"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRosterConnector_ImportRoster(t *testing.T) {
	tests := []struct {
		name         string
		readerFn     func(t *testing.T) io.Reader
		wantErr      bool
		errContains  string
		wantLen      int
		wantStudents []contracts.RosterStudent
	}{
		{
			name: "students only — role filtering and email fallback",
			readerFn: func(t *testing.T) io.Reader {
				f, err := os.Open("testdata/oneroster_users.csv")
				require.NoError(t, err)
				t.Cleanup(func() { f.Close() })
				return f
			},
			wantLen: 3,
			// only 3 students expected (teacher and admin filtered out)
			wantStudents: []contracts.RosterStudent{
				{Email: "alice@school.edu", FullName: "Alice Smith", ExternalID: "s001", ClassLabel: ""},
				{Email: "carol@school.edu", FullName: "Carol White", ExternalID: "s002"},
				// dave (s003) — empty email, falls back to username
				{Email: "dave_b", FullName: "Dave Brown", ExternalID: "s003"},
			},
		},
		{
			name: "missing required header",
			readerFn: func(t *testing.T) io.Reader {
				return strings.NewReader("username,givenName,familyName\nalice,Alice,Smith\n")
			},
			wantErr:     true,
			errContains: "missing required column",
		},
		{
			// Provides only name and group — missing sourcedId, givenName, familyName, role.
			// Expects an error and no students returned.
			name: "missing required columns — only name and group present",
			readerFn: func(t *testing.T) io.Reader {
				return strings.NewReader("name,group\nalice,10A\n")
			},
			wantErr:     true,
			errContains: "missing required column",
		},
		{
			// Explicit test that a student with blank email falls back to username.
			name: "empty email falls back to username",
			readerFn: func(t *testing.T) io.Reader {
				return strings.NewReader(
					"sourcedId,username,givenName,familyName,email,role\n" +
						"s003,dave_b,Dave,Brown,,student\n",
				)
			},
			wantLen: 1,
			wantStudents: []contracts.RosterStudent{
				{Email: "dave_b", FullName: "Dave Brown", ExternalID: "s003"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := oneroster.RosterConnector{}
			students, err := conn.ImportRoster(context.Background(), tc.readerFn(t))

			if tc.wantErr {
				require.Error(t, err)
				if tc.errContains != "" {
					assert.Contains(t, err.Error(), tc.errContains)
				}
				assert.Nil(t, students)
				return
			}

			require.NoError(t, err)
			if tc.wantLen > 0 {
				require.Len(t, students, tc.wantLen)
			}

			for i, want := range tc.wantStudents {
				if i >= len(students) {
					break
				}
				assert.Equal(t, want.Email, students[i].Email)
				assert.Equal(t, want.FullName, students[i].FullName)
				assert.Equal(t, want.ExternalID, students[i].ExternalID)
				if want.ClassLabel != "" {
					assert.Equal(t, want.ClassLabel, students[i].ClassLabel)
				}
			}
		})
	}
}
