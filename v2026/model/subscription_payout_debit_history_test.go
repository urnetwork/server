// Repeated payout assertions preserve materialized consumption from earlier
// debit passes while new native reservations keep their independent journals.
package model

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The zero-use first pass restores admission; later passes mix positive applied
// history with pending usage. The final invocation proves an empty exact replay.
func TestPayoutDebitCensusPreservesAppliedHistoryAcrossNewSettlement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		var consumed ByteCount
		for index, test := range []struct {
			reserved, used ByteCount
		}{
			{reserved: 600, used: 0},
			{reserved: 600, used: 300},
			{reserved: 200, used: 137},
		} {
			escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, test.reserved)
			if err != nil || escrow == nil || escrow.TransferByteCount != test.reserved {
				t.Fatalf("pass %d could not reuse the exact released grant: escrow=%v err=%v", index, escrow, err)
			}
			for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
				if err := CloseContract(ctx, escrow.ContractId, clientId, test.used, false); err != nil {
					t.Fatal("close repeated native payout reservation", err)
				}
			}
			before := readPayoutDebitTestState(t, ctx, f.balanceId)
			if before.credit != 1000-consumed || before.settled != consumed || before.settledEscrows != index ||
				before.pending != 1 || before.pendingBytes != test.used || before.reserved != test.reserved || before.invalid != 0 {
				t.Fatalf("pass %d changed prior applied consumption or pending reservation: %+v", index, before)
			}
			consumed += test.used
			assertPayoutDebitTestConsumptionAndDrain(t, ctx, f.balanceId, 1000, consumed)
			after := readPayoutDebitTestState(t, ctx, f.balanceId)
			if after.credit != 1000-consumed || after.settled != consumed || after.settledEscrows != index+1 ||
				after.pending != 0 || after.pendingBytes != 0 || after.reserved != 0 || after.invalid != 0 {
				t.Fatalf("pass %d lost cumulative applied consumption: %+v", index, after)
			}
		}
		assertPayoutDebitTestConsumptionAndDrain(t, ctx, f.balanceId, 1000, consumed)
	})
}
