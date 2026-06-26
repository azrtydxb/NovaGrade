package contracts

// TranscribedQuestion mirrors the POC's TranscribedQuestion Pydantic model.
// Optional field Section uses *string (nil == Python None).
// ReadConfidence is constrained to [0,1] in the POC; here it is a plain float64
// (validation is left to the caller or a dedicated validator layer).
type TranscribedQuestion struct {
	Section        *string `json:"section"`
	QuestionNo     string  `json:"question_no"`
	MaxMarks       float64 `json:"max_marks"`
	QuestionText   string  `json:"question_text"`
	StudentAnswer  string  `json:"student_answer"`
	ReadConfidence float64 `json:"read_confidence"`
}

// TranscribedPaper mirrors the POC's TranscribedPaper Pydantic model.
// Optional field ExpectedTotal uses *float64 (nil == Python None).
type TranscribedPaper struct {
	Subject       string                `json:"subject"`
	SourcePDF     string                `json:"source_pdf"`
	Questions     []TranscribedQuestion `json:"questions"`
	ExpectedTotal *float64              `json:"expected_total"`
}

// GradedQuestion mirrors the POC's GradedQuestion Pydantic model.
// Optional field Section uses *string (nil == Python None).
// GradeConfidence is constrained to [0,1] in the POC; validation left to caller.
// Flags defaults to an empty slice (mirrors Python default_factory=list).
// Feedback holds the AI-drafted per-question feedback text (added in Phase 2,
// Task 5). It is separate from Justification (the grader's mark-scheme rationale)
// and never affects awarded_marks or max_marks.
// Revision holds the AI-drafted "how to improve" guidance for the student
// (added in Phase 6, Task 4). Like Feedback, it is strictly additive and never
// affects awarded_marks, max_marks, or any paper-level totals.
type GradedQuestion struct {
	QuestionNo      string   `json:"question_no"`
	Section         *string  `json:"section"`
	MaxMarks        float64  `json:"max_marks"`
	AwardedMarks    float64  `json:"awarded_marks"`
	StudentAnswer   string   `json:"student_answer"`
	Justification   string   `json:"justification"`
	GradeConfidence float64  `json:"grade_confidence"`
	Flags           []string `json:"flags"`
	Feedback        string   `json:"feedback"`
	Revision        string   `json:"revision,omitempty"`
}

// GradedPaper mirrors the POC's GradedPaper Pydantic model.
// Optional field ExpectedTotal uses *float64 (nil == Python None).
// Score100 defaults to 0.0 (mirrors Python default).
type GradedPaper struct {
	Subject       string             `json:"subject"`
	SourcePDF     string             `json:"source_pdf"`
	Questions     []GradedQuestion   `json:"questions"`
	SectionTotals map[string]float64 `json:"section_totals"`
	Total         float64            `json:"total"`
	MaxTotal      float64            `json:"max_total"`
	Score100      float64            `json:"score_100"`
	ExpectedTotal *float64           `json:"expected_total"`
}

// RosterStudent represents a student record imported from a roster source.
type RosterStudent struct {
	Email      string `json:"email"`
	FullName   string `json:"full_name"`
	ExternalID string `json:"external_id"`
	ClassLabel string `json:"class_label"`
}

// GradeRow represents a single row in a grade export.
type GradeRow struct {
	StudentName string  `json:"student_name"`
	QuestionNo  string  `json:"question_no"`
	Awarded     float64 `json:"awarded"`
	MaxMarks    float64 `json:"max_marks"`
	Feedback    string  `json:"feedback"`
}
