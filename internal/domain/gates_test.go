package domain_test

import (
	"testing"

	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluateGates_Clean(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     20,
		LowReadConfidence: 0,
		BlankAnswers:      0,
		DetectedTotal:     100.0,
		ChecksumOK:        true,
	}
	tun := domain.DefaultTunables()
	flags := domain.EvaluateGates(tf, tun)
	assert.Empty(t, flags)
}

func TestEvaluateGates_LowReadConfidence(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     20,
		LowReadConfidence: 3,
		BlankAnswers:      0,
		DetectedTotal:     100.0,
		ChecksumOK:        true,
	}
	tun := domain.DefaultTunables()
	flags := domain.EvaluateGates(tf, tun)
	require.Contains(t, flags, domain.FlagLowReadConfidence)
}

func TestEvaluateGates_ChecksumFailed(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     20,
		LowReadConfidence: 0,
		BlankAnswers:      0,
		DetectedTotal:     95.0,
		ChecksumOK:        false,
	}
	tun := domain.DefaultTunables()
	flags := domain.EvaluateGates(tf, tun)
	require.Contains(t, flags, domain.FlagChecksumMismatch)
}

func TestEvaluateGates_ChecksumDifferenceNonZero(t *testing.T) {
	diff := 5.0
	tf := domain.TranscribeFlags{
		QuestionCount:      20,
		LowReadConfidence:  0,
		BlankAnswers:       0,
		DetectedTotal:      95.0,
		ChecksumOK:         true,
		ChecksumDifference: &diff,
	}
	tun := domain.DefaultTunables()
	flags := domain.EvaluateGates(tf, tun)
	require.Contains(t, flags, domain.FlagChecksumMismatch)
}

func TestEvaluateGates_BlankOverThreshold(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     20,
		LowReadConfidence: 0,
		BlankAnswers:      2,
		DetectedTotal:     100.0,
		ChecksumOK:        true,
	}
	// Default threshold is 0, so any blank triggers flag
	tun := domain.DefaultTunables()
	flags := domain.EvaluateGates(tf, tun)
	require.Contains(t, flags, domain.FlagBlankOverThreshold)
}

func TestEvaluateGates_BlankExactlyAtThreshold_NoFlag(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     20,
		LowReadConfidence: 0,
		BlankAnswers:      0,
		DetectedTotal:     100.0,
		ChecksumOK:        true,
	}
	tun := domain.Tunables{BlankThreshold: 0, ExpectedQuestions: 0, QuestionCountTol: 2}
	flags := domain.EvaluateGates(tf, tun)
	assert.NotContains(t, flags, domain.FlagBlankOverThreshold)
}

func TestEvaluateGates_StructuralAnomaly_QuestionCountFarFromExpected(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     25,
		LowReadConfidence: 0,
		BlankAnswers:      0,
		DetectedTotal:     100.0,
		ChecksumOK:        true,
	}
	tun := domain.Tunables{
		BlankThreshold:    0,
		ExpectedQuestions: 20,
		QuestionCountTol:  2,
	}
	flags := domain.EvaluateGates(tf, tun)
	require.Contains(t, flags, domain.FlagStructuralAnomaly)
}

func TestEvaluateGates_StructuralAnomaly_WithinTolerance_NoFlag(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     21,
		LowReadConfidence: 0,
		BlankAnswers:      0,
		DetectedTotal:     100.0,
		ChecksumOK:        true,
	}
	tun := domain.Tunables{
		BlankThreshold:    0,
		ExpectedQuestions: 20,
		QuestionCountTol:  2,
	}
	flags := domain.EvaluateGates(tf, tun)
	assert.NotContains(t, flags, domain.FlagStructuralAnomaly)
}

func TestEvaluateGates_StructuralAnomaly_ExpectedZero_NoFlag(t *testing.T) {
	tf := domain.TranscribeFlags{
		QuestionCount:     25,
		LowReadConfidence: 0,
		BlankAnswers:      0,
		DetectedTotal:     100.0,
		ChecksumOK:        true,
	}
	// ExpectedQuestions == 0 means unknown → no structural anomaly check
	tun := domain.DefaultTunables()
	flags := domain.EvaluateGates(tf, tun)
	assert.NotContains(t, flags, domain.FlagStructuralAnomaly)
}

func TestEvaluateGates_MultipleFlags(t *testing.T) {
	diff := 10.0
	tf := domain.TranscribeFlags{
		QuestionCount:      30,
		LowReadConfidence:  2,
		BlankAnswers:       5,
		DetectedTotal:      80.0,
		ChecksumOK:         false,
		ChecksumDifference: &diff,
	}
	tun := domain.Tunables{
		BlankThreshold:    2,
		ExpectedQuestions: 20,
		QuestionCountTol:  2,
	}
	flags := domain.EvaluateGates(tf, tun)
	assert.Contains(t, flags, domain.FlagLowReadConfidence)
	assert.Contains(t, flags, domain.FlagChecksumMismatch)
	assert.Contains(t, flags, domain.FlagBlankOverThreshold)
	assert.Contains(t, flags, domain.FlagStructuralAnomaly)
}
