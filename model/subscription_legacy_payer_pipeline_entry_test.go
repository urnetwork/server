// External tests invoke the actual worker package without reversing model's
// production dependencies. The internal bridge supplies only fixture oracles.
package model_test

import (
	"testing"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

func TestLegacyPayerPipelineSamePayerFullWork(t *testing.T) {
	model.TestingLegacyPayerPipeline(t, []int{1024},
		task.NewTaskTargetWithPost(work.FlushLegacySettlements, work.FlushLegacySettlementsPost), work.ScheduleFlushLegacySettlements)
}

func TestLegacyPayerPipelineSharedProviderFullWork(t *testing.T) {
	model.TestingLegacyPayerPipeline(t, []int{512, 128, 64, 64, 64, 64, 64, 64},
		task.NewTaskTargetWithPost(work.FlushLegacySettlements, work.FlushLegacySettlementsPost), work.ScheduleFlushLegacySettlements)
}
