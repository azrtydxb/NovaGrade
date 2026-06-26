package csv_test

import (
	"bytes"
	"context"
	"testing"

	csvconn "github.com/azrtydxb/novagrade/internal/integration/csv"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGradeConnector_ExportGrades(t *testing.T) {
	tests := []struct {
		name     string
		rows     []contracts.GradeRow
		expected string
	}{
		{
			name:     "empty rows",
			rows:     nil,
			expected: "student,question_no,max_marks,awarded,feedback\n",
		},
		{
			name: "multi-row happy path",
			rows: []contracts.GradeRow{
				{StudentName: "Alice Smith", QuestionNo: "Q1", Awarded: 4, MaxMarks: 5, Feedback: "Good"},
				{StudentName: "Bob Jones", QuestionNo: "Q2", Awarded: 2.5, MaxMarks: 10, Feedback: "Needs improvement"},
			},
			expected: "student,question_no,max_marks,awarded,feedback\n" +
				"Alice Smith,Q1,5,4,Good\n" +
				"Bob Jones,Q2,10,2.5,Needs improvement\n",
		},
		{
			name: "comma in feedback",
			rows: []contracts.GradeRow{
				{StudentName: "Alice", QuestionNo: "Q1", Awarded: 3, MaxMarks: 5, Feedback: "Good, well done"},
			},
			// encoding/csv wraps fields containing commas in double quotes.
			expected: "student,question_no,max_marks,awarded,feedback\n" +
				"Alice,Q1,5,3,\"Good, well done\"\n",
		},
		{
			name: "quote in feedback",
			rows: []contracts.GradeRow{
				{StudentName: "Bob", QuestionNo: "Q2", Awarded: 1, MaxMarks: 4, Feedback: `He said "well done"`},
			},
			// encoding/csv escapes double quotes by doubling them and wrapping in quotes.
			expected: "student,question_no,max_marks,awarded,feedback\n" +
				"Bob,Q2,4,1,\"He said \"\"well done\"\"\"\n",
		},
		{
			name: "float shortest representation",
			rows: []contracts.GradeRow{
				{StudentName: "Carol", QuestionNo: "Q3", Awarded: 3, MaxMarks: 3, Feedback: ""},
			},
			// FormatFloat with 'f' and -1 prec gives shortest representation: "3" not "3.0".
			expected: "student,question_no,max_marks,awarded,feedback\n" +
				"Carol,Q3,3,3,\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			conn := csvconn.GradeConnector{}
			err := conn.ExportGrades(context.Background(), &buf, tc.rows)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, buf.String())
		})
	}
}
