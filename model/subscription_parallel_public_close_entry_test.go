// Both declared payer geometries use the actual public close and registered
// financial/debit workers, with one reproducibly jittered 2048-caller burst.
package model_test

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

func TestParallelPublicClose64Payers(t *testing.T) {
	counts := make([]int, 64)
	for index := range counts {
		counts[index] = 32
	}
	model.TestingParallelPublicCloseSharedPayers(t, counts,
		task.NewTaskTargetWithPost(work.FlushLegacySettlements, work.FlushLegacySettlementsPost),
		task.NewTaskTargetWithPost(work.FlushTransferDebits, work.FlushTransferDebitsPost),
		work.ScheduleFlushLegacySettlements, work.ScheduleFlushTransferDebits, server.MaintenancePgVaultResourceName)
}

func TestParallelPublicClose8Payers(t *testing.T) {
	counts := make([]int, 8)
	for index := range counts {
		counts[index] = 256
	}
	model.TestingParallelPublicCloseSharedPayers(t, counts,
		task.NewTaskTargetWithPost(work.FlushLegacySettlements, work.FlushLegacySettlementsPost),
		task.NewTaskTargetWithPost(work.FlushTransferDebits, work.FlushTransferDebitsPost),
		work.ScheduleFlushLegacySettlements, work.ScheduleFlushTransferDebits, server.MaintenancePgVaultResourceName)
}
