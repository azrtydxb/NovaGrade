package csv

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"

	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// GradeConnector implements integration.GradeSink by writing grade rows as CSV.
// The zero value is ready to use.
type GradeConnector struct{}

// ExportGrades writes rows to w as a CSV with header:
//
//	student,question_no,max_marks,awarded,feedback
//
// Floats are formatted with strconv.FormatFloat(v, 'f', -1, 64).
// The encoding/csv writer handles quoting/escaping automatically.
func (c GradeConnector) ExportGrades(_ context.Context, w io.Writer, rows []contracts.GradeRow) error {
	writer := csv.NewWriter(w)

	if err := writer.Write([]string{"student", "question_no", "max_marks", "awarded", "feedback"}); err != nil {
		return fmt.Errorf("csv grades: write header: %w", err)
	}

	for i, row := range rows {
		record := []string{
			row.StudentName,
			row.QuestionNo,
			strconv.FormatFloat(row.MaxMarks, 'f', -1, 64),
			strconv.FormatFloat(row.Awarded, 'f', -1, 64),
			row.Feedback,
		}
		if err := writer.Write(record); err != nil {
			return fmt.Errorf("csv grades: write row %d: %w", i, err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("csv grades: flush: %w", err)
	}
	return nil
}
