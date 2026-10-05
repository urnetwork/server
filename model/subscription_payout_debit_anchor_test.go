// These public companion fixtures separate live zero anchors from pending
// paid consumption, including the zero-debit acknowledgment obligation.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Two independent forward anchors remain byte-identical while the public
// reverse settlement and debit worker consume one exact reservation.
func TestPayoutDebitPublicCompanionPreservesTwoLiveZeroAnchors(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		for range 2 {
			anchor, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 0)
			if err != nil || anchor == nil || anchor.TransferByteCount != 0 {
				t.Fatal("public zero anchor was not created", anchor, err)
			}
		}
		original := readPayoutDebitTestState(t, ctx, f.balanceId)
		if original.anchors != 2 || original.escrows != 2 || original.invalid != 0 || original.pending != 0 || original.anchorRows == "" {
			t.Fatalf("two independent zero anchors were not retained: %+v", original)
		}
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, f.sourceId, 121, time.Hour)
		if err != nil || companion == nil || companion.TransferByteCount != 121 {
			t.Fatal("public reverse reservation was not created", companion, err)
		}
		for _, clientId := range []server.Id{f.destinationId, f.sourceId} {
			if err := CloseContract(ctx, companion.ContractId, clientId, 121, false); err != nil {
				t.Fatal("public reverse settlement failed", err)
			}
		}
		before := readPayoutDebitTestState(t, ctx, f.balanceId)
		if before.anchors != 2 || before.anchorRows != original.anchorRows || before.settledEscrows != 1 || before.escrows != 3 {
			t.Fatalf("companion settlement changed its live forward anchors: before=%+v after=%+v", original, before)
		}
		assertPayoutDebitTestConsumptionAndDrain(t, ctx, f.balanceId, 1000, 121)
	})
}

// A positive reservation settled with zero traffic still has one durable
// journal/token to acknowledge. The live anchor has neither, even though both
// amounts are zero after settlement.
func TestPayoutDebitPublicZeroUseCompanionStillAcknowledgesJournal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		anchor, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 0)
		if err != nil || anchor == nil {
			t.Fatal("public zero anchor was not created", anchor, err)
		}
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, f.sourceId, 17, time.Hour)
		if err != nil || companion == nil || companion.TransferByteCount != 17 {
			t.Fatal("public reverse reservation was not created", companion, err)
		}
		keys := redisContractReservationKeys(f.balanceId)
		var originalExpiry float64
		server.Redis(ctx, func(client server.RedisClient) {
			var err error
			originalExpiry, err = client.ZScore(ctx, keys[2], companion.ContractId.String()).Result()
			server.Raise(err)
		})
		for _, clientId := range []server.Id{f.destinationId, f.sourceId} {
			if err := CloseContract(ctx, companion.ContractId, clientId, 0, false); err != nil {
				t.Fatal("public zero-use settlement failed", err)
			}
		}
		before := readPayoutDebitTestState(t, ctx, f.balanceId)
		if before.anchors != 1 || before.settledEscrows != 1 || before.pending != 1 || before.pendingBytes != 0 || before.reserved != 0 || before.invalid != 0 {
			t.Fatalf("zero-use settlement was confused with an unconsumed anchor: %+v", before)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			amount, err := client.HGet(ctx, keys[1], companion.ContractId.String()).Int64()
			server.Raise(err)
			expiry, err := client.ZScore(ctx, keys[2], companion.ContractId.String()).Result()
			server.Raise(err)
			if amount != 0 || expiry != originalExpiry {
				t.Fatal("zero debit did not retain its original request acknowledgment", amount, expiry, originalExpiry)
			}
		})
		assertPayoutDebitTestConsumptionAndDrain(t, ctx, f.balanceId, 1000, 0)
		server.Redis(ctx, func(client server.RedisClient) {
			if err := client.HGet(ctx, keys[1], companion.ContractId.String()).Err(); err != server.RedisNil {
				t.Fatal("acknowledged zero debit retained its request token", err)
			}
			if err := client.ZScore(ctx, keys[2], companion.ContractId.String()).Err(); err != server.RedisNil {
				t.Fatal("acknowledged zero debit retained its request lease", err)
			}
		})
	})
}

// Relaxing the original all-settled fixture census is allowed only for the
// exact live anchor shape. Financial or lifecycle evidence disqualifies it.
func TestPayoutDebitAnchorCensusRejectsAlteredFinancialAndLifecycleRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, test := range []struct {
			name, statement string
		}{
			{name: "reserved", statement: `UPDATE transfer_escrow SET redis_reserved=true WHERE contract_id=$1`},
			{name: "positive bytes", statement: `UPDATE transfer_escrow SET balance_byte_count=1 WHERE contract_id=$1`},
			{name: "payout", statement: `UPDATE transfer_escrow SET payout_byte_count=0 WHERE contract_id=$1`},
			{name: "settlement time", statement: `UPDATE transfer_escrow SET settle_time=clock_timestamp() WHERE contract_id=$1`},
			{name: "settled", statement: `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`},
			{name: "contract bytes", statement: `UPDATE transfer_contract SET transfer_byte_count=1 WHERE contract_id=$1`},
			{name: "dispute", statement: `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`},
			{name: "zero journal", statement: `INSERT INTO transfer_debit_journal (contract_id,balance_id,shard,debit_byte_count)
				SELECT contract_id,balance_id,get_byte(uuid_send(balance_id),15)%16,0 FROM transfer_escrow WHERE contract_id=$1`},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			anchor, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 0)
			if err != nil || anchor == nil {
				t.Fatal("public zero anchor was not created", anchor, err)
			}
			before := readPayoutDebitTestState(t, ctx, f.balanceId)
			if before.anchors != 1 || before.invalid != 0 {
				t.Fatalf("%s: original anchor was not admitted: %+v", test.name, before)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, test.statement, anchor.ContractId))
			})
			after := readPayoutDebitTestState(t, ctx, f.balanceId)
			if after.anchors != 0 || after.invalid != 1 || after.settledEscrows != 0 || after.escrows != 1 {
				t.Fatalf("%s: altered financial/lifecycle row was accepted as a live zero anchor: %+v", test.name, after)
			}
		}
	})
}
