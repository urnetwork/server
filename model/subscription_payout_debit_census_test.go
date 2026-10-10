// The payout fixture follows native consumption through journal, retained
// reservation, metadata writeback, and acknowledged replay boundaries.
package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// A native close retains nullable metadata and its original reservation even
// when final usage is partial or zero. The preimage census rejects this state.
func TestPayoutDebitCensusTracksDeferredNativeMetadata(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, consumed := range []ByteCount{0, 37, 121} {
			balanceId, _ := newContractPayoutDebitReadTestFixture(t, ctx, false, consumed)
			before := readPayoutDebitTestState(t, ctx, balanceId)
			if before.initial != 121 || before.credit != 121 || before.pending != 1 || before.pendingBytes != consumed ||
				before.applied != 0 || before.escrows != 1 || before.invalid != 0 || before.settled != 0 || before.settledEscrows != 0 ||
				before.anchors != 0 || before.legacy != 0 || before.reserved != 121 {
				t.Fatalf("usage %d lost its pending native metadata/reservation boundary: %+v", consumed, before)
			}
			assertPayoutDebitTestConsumptionAndDrain(t, ctx, balanceId, 121, consumed)
			after := readPayoutDebitTestState(t, ctx, balanceId)
			if after.credit != 121-consumed || after.pending != 0 || after.pendingBytes != 0 || after.applied != 0 ||
				after.settled != consumed || after.settledEscrows != 1 || after.invalid != 0 || after.reserved != 0 {
				t.Fatalf("usage %d lost its materialized debit boundary: %+v", consumed, after)
			}
		}
	})
}

// Nullable metadata is accepted only with a complete native pending journal;
// partial projections and missing financial authority remain invalid rows.
func TestPayoutDebitCensusRejectsIncompletePendingMetadata(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, test := range []struct {
			name, statement string
		}{
			{name: "missing journal", statement: `DELETE FROM transfer_debit_journal WHERE contract_id=$1`},
			{name: "applied journal without metadata", statement: `UPDATE transfer_debit_journal SET applied=true WHERE contract_id=$1`},
			{name: "nonnative pending reservation", statement: `UPDATE transfer_escrow SET redis_reserved=false WHERE contract_id=$1`},
			{name: "settled flag without metadata", statement: `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`},
			{name: "timestamp without amount", statement: `UPDATE transfer_escrow SET settle_time=clock_timestamp() WHERE contract_id=$1`},
			{name: "amount without settlement", statement: `UPDATE transfer_escrow SET payout_byte_count=121 WHERE contract_id=$1`},
			{name: "unapplied materialized metadata", statement: `UPDATE transfer_escrow SET settled=true,settle_time=clock_timestamp(),payout_byte_count=121 WHERE contract_id=$1`},
			{name: "debit exceeds reservation", statement: `UPDATE transfer_debit_journal SET debit_byte_count=122 WHERE contract_id=$1`},
		} {
			balanceId, contractId := newContractPayoutDebitReadTestFixture(t, ctx, false, 121)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, test.statement, contractId))
			})
			if state := readPayoutDebitTestState(t, ctx, balanceId); state.invalid != 1 || state.anchors != 0 || state.settledEscrows != 0 {
				t.Fatalf("%s was accepted by the pending payout census: %+v", test.name, state)
			}
		}
	})
}
