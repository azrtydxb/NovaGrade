package orchestrator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azrtydxb/novagrade/internal/domain"
	"github.com/azrtydxb/novagrade/internal/orchestrator"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

// TestStageToEvent_Regrade verifies that the "regrade" stage maps to EventRegrade.
func TestStageToEvent_Regrade(t *testing.T) {
	ev, err := orchestrator.StageToEvent(contracts.StageRegrade)
	require.NoError(t, err)
	assert.Equal(t, domain.EventRegrade, ev)
}

// TestStageToEvent_KnownStages verifies that all existing known stages still map correctly.
func TestStageToEvent_KnownStages(t *testing.T) {
	cases := []struct {
		stage string
		want  domain.Event
	}{
		{contracts.StageRenderResult, domain.EventStageSucceeded},
		{contracts.StageTranscribeResult, domain.EventStageSucceeded},
		{contracts.StageGradeResult, domain.EventStageSucceeded},
		{contracts.StageSubmitExam, domain.EventSubmitExam},
		{contracts.StageApprove, domain.EventApproveByTeacher},
		{contracts.StagePublish, domain.EventPublish},
		{contracts.StageExport, domain.EventExport},
		{contracts.StageRegrade, domain.EventRegrade},
	}
	for _, c := range cases {
		t.Run(c.stage, func(t *testing.T) {
			ev, err := orchestrator.StageToEvent(c.stage)
			require.NoError(t, err)
			assert.Equal(t, c.want, ev)
		})
	}
}

// TestStageToEvent_UnknownStage verifies that an unknown stage returns an error.
func TestStageToEvent_UnknownStage(t *testing.T) {
	_, err := orchestrator.StageToEvent("totally_unknown_stage")
	assert.Error(t, err)
}
