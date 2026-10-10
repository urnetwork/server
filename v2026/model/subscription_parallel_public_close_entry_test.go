// Both declared payer geometries use the actual public close and registered
// financial/debit workers, with one reproducibly jittered 2048-caller burst.
package model_test

import (
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

func TestParallelPublicClose64Payers(t *testing.T) {
	counts := make([]int, 64)
	for index := range counts {
		counts[index] = 32
	}
	model.TestingParallelPublicCloseSharedPayers(t, counts,
		work.NewLegacySettlementDispatcherTaskTarget(),
		work.NewTransferDebitTaskTarget(),
		work.ScheduleFlushLegacySettlements, work.ScheduleFlushTransferDebits, server.MaintenancePgVaultResourceName)
}

func TestParallelPublicClose8Payers(t *testing.T) {
	counts := make([]int, 8)
	for index := range counts {
		counts[index] = 256
	}
	model.TestingParallelPublicCloseSharedPayers(t, counts,
		work.NewLegacySettlementDispatcherTaskTarget(),
		work.NewTransferDebitTaskTarget(),
		work.ScheduleFlushLegacySettlements, work.ScheduleFlushTransferDebits, server.MaintenancePgVaultResourceName)
}
