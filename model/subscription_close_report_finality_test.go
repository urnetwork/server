// Rolling report receipts must only describe work the party actually accepted.
package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// The old owner inserted a receipt even when its guarded increment affected
// zero rows after a party final. That turned refused work into replay authority.
func TestCloseReportLateIdentifiedCheckpointDoesNotCreateReceipt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		server.Raise(CloseContract(f.ctx, f.contractId, f.sourceId, 20, false))
		reportId := server.NewId()
		for range 2 {
			applied, err := CloseContractReport(f.ctx, f.contractId, f.sourceId, 30, true, reportId)
			if err != nil || applied {
				t.Fatal("late rolling checkpoint changed a finalized party", applied, err)
			}
			requireCloseReportState(t, f.ctx, f.contractId, 0, 20)
		}
	})
}
