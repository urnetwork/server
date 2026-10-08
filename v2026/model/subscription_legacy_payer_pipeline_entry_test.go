// External tests invoke the actual worker package without reversing model's
// production dependencies. The internal bridge supplies only fixture oracles.
package model_test

import (
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

func TestLegacyPayerPipelineSamePayerFullWork(t *testing.T) {
	model.TestingLegacyPayerPipeline(t, []int{1024},
		legacyPayerPipelineShardTarget(), work.ScheduleFlushLegacySettlements)
}

func TestLegacyPayerPipelineSharedProviderFullWork(t *testing.T) {
	model.TestingLegacyPayerPipeline(t, []int{512, 128, 64, 64, 64, 64, 64, 64},
		legacyPayerPipelineShardTarget(), work.ScheduleFlushLegacySettlements)
}
