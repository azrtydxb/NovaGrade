package domain

// TranscribeFlags mirrors the payload published by the transcribe worker.
// This is a domain copy so the gates package does not import cmd/transcribe.
type TranscribeFlags struct {
	QuestionCount       int      `json:"question_count"`
	LowReadConfidence   int      `json:"low_read_confidence"` // questions with read_confidence below threshold
	BlankAnswers        int      `json:"blank_answers"`
	DetectedTotal       float64  `json:"detected_total"`
	ExpectedTotal       *float64 `json:"expected_total"`
	ChecksumOK          bool     `json:"checksum_ok"`
	ChecksumDifference  *float64 `json:"checksum_difference"`
	TranscriptObjectKey string   `json:"transcript_object_key"`
}

// Tunables holds configurable thresholds for gate evaluation.
type Tunables struct {
	// BlankThreshold is the maximum number of blank answers before flagging.
	// Default 0 means any blank answer triggers FlagBlankOverThreshold.
	BlankThreshold int

	// ExpectedQuestions is the expected number of questions. 0 means unknown
	// and disables structural anomaly checking.
	ExpectedQuestions int

	// QuestionCountTol is the tolerance for question count deviation. A
	// question count outside [Expected-Tol, Expected+Tol] raises FlagStructuralAnomaly.
	QuestionCountTol int
}

// DefaultTunables returns sensible default thresholds.
func DefaultTunables() Tunables {
	return Tunables{
		BlankThreshold:    0, // any blank → flag
		ExpectedQuestions: 0, // unknown
		QuestionCountTol:  2, // ±2 questions tolerance
	}
}

// EvaluateGates inspects transcription flags and returns any quality flags raised.
//
// Rules:
//   - LowReadConfidence > 0 → FlagLowReadConfidence
//   - ChecksumOK == false OR (ChecksumDifference != nil && *ChecksumDifference != 0) → FlagChecksumMismatch
//   - BlankAnswers > BlankThreshold → FlagBlankOverThreshold
//   - QuestionCount outside [ExpectedQuestions-Tol, ExpectedQuestions+Tol] (when ExpectedQuestions > 0) → FlagStructuralAnomaly
func EvaluateGates(tf TranscribeFlags, tun Tunables) []QualityFlag {
	var flags []QualityFlag

	if tf.LowReadConfidence > 0 {
		flags = append(flags, FlagLowReadConfidence)
	}

	if !tf.ChecksumOK || (tf.ChecksumDifference != nil && *tf.ChecksumDifference != 0) {
		flags = append(flags, FlagChecksumMismatch)
	}

	if tf.BlankAnswers > tun.BlankThreshold {
		flags = append(flags, FlagBlankOverThreshold)
	}

	if tun.ExpectedQuestions > 0 {
		diff := tf.QuestionCount - tun.ExpectedQuestions
		if diff < 0 {
			diff = -diff
		}
		if diff > tun.QuestionCountTol {
			flags = append(flags, FlagStructuralAnomaly)
		}
	}

	return flags
}
