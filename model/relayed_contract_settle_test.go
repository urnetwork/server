//go:build sidecar_pending

package model

// QUOTA-M2-09: relayed settlement (DESIGN.md 20.6's settle and bound rows), under -tags
// sidecar_pending: one debit per contract_id across retried closes and the deadline close, the
// mean of both reports, the rest returned within the flush, a stream bind returning everything, a
// debit that lands with Redis lost, and recovery's custody of a relayed token.

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The contract's journal rows by balance, each its meter and bytes.
func testingRelayedJournal(t testing.TB, ctx context.Context, contractId server.Id) map[server.Id]ByteCount {
	t.Helper()
	rows := map[server.Id]ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT balance_id, meter, debit_byte_count FROM service_debit_journal WHERE usage_id = $1`, contractId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				var meter string
				var byteCount ByteCount
				server.Raise(result.Scan(&balanceId, &meter, &byteCount))
				if meter != sidecar.QuotaMeterRelay {
					t.Fatalf("a relayed row under meter %q", meter)
				}
				rows[balanceId] = byteCount
			}
		})
	})
	return rows
}

// FI-13(a): the contract's journal never carries more than its settled count.
func testingRelayedJournalTotal(t testing.TB, ctx context.Context, contractId server.Id) ByteCount {
	t.Helper()
	var total ByteCount
	for _, byteCount := range testingRelayedJournal(t, ctx, contractId) {
		total += byteCount
	}
	return total
}

func testingRelayedBalanceByteCount(ctx context.Context, balanceId server.Id) (byteCount ByteCount) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id = $1`, balanceId).Scan(&byteCount))
	})
	return
}

// Both reports settle the mean once, whatever retries, and one side reporting zero halves the other;
// the flush applies the debit and releases the token, returning the rest.
func TestRelayedSettlementDebitsTheMeanOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		for _, c := range []struct {
			source      ByteCount
			destination ByteCount
			settled     ByteCount
		}{
			{source: 1000, destination: 600, settled: 800},
			{source: 1000, destination: 0, settled: 500},
		} {
			network := newTestingRelayedNetwork(t, ctx, 1*Gib)
			contractId, grant, err := network.reserve(ctx, 64*Mib)
			if err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.sourceId, c.source, false))
			connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.destinationId, c.destination, false))
			_, closed := GetContractClose(ctx, contractId)
			connect.AssertEqual(t, closed, true)
			// retried closes of a settled contract are refused and journal nothing more
			connect.AssertEqual(t, CloseContract(ctx, contractId, network.sourceId, c.source, false) != nil, true)
			connect.AssertEqual(t, CloseContract(ctx, contractId, network.destinationId, c.destination, false) != nil, true)
			connect.AssertEqual(t, testingRelayedJournal(t, ctx, contractId), map[server.Id]ByteCount{network.balanceIds[0]: c.settled})
			connect.AssertEqual(t, network.held(ctx), []ByteCount{grant})

			clock := sidecartest.NewFakeClock(server.NowUtc())
			_, err = FlushServiceDebits(ctx, ServiceDebitShard(network.balanceIds[0]), nil, clock)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, network.held(ctx), []ByteCount{0})
			connect.AssertEqual(t, testingRelayedBalanceByteCount(ctx, network.balanceIds[0]), 1*Gib-c.settled)
			connect.AssertEqual(t, len(testingRelayedJournal(t, ctx, contractId)), 0)
		}
	})
}

// A deadline close with one report debits that report, once.
func TestRelayedDeadlineCloseDebitsOneReport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 1*Gib)
		contractId, _, err := network.reserve(ctx, 64*Mib)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.sourceId, 3000, false))
		for range 2 {
			if _, err := ReconcileContractAtDeadline(ctx, contractId, server.NowUtc().Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		connect.AssertEqual(t, testingRelayedJournal(t, ctx, contractId), map[server.Id]ByteCount{network.balanceIds[0]: 3000})
	})
}

// Bytes reported past the grant debit only the grant.
func TestRelayedSettlementCapsAtTheGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 16*Mib)
		contractId, grant, err := network.reserve(ctx, 64*Mib)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, grant, 2*Mib)
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.sourceId, 10*Mib, false))
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.destinationId, 10*Mib, false))
		connect.AssertEqual(t, testingRelayedJournalTotal(t, ctx, contractId), grant)
	})
}

// A contract later bound to a P2P stream returns all of its reservation at the bind and debits
// nothing at its close.
func TestRelayedStreamBindReturnsEverything(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 1*Gib)
		contractId, _, err := network.reserve(ctx, 64*Mib)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, nil, SetContractStream(ctx, contractId, server.NewId(), nil))
		connect.AssertEqual(t, network.held(ctx), []ByteCount{0})
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.sourceId, 5000, false))
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.destinationId, 5000, false))
		connect.AssertEqual(t, testingRelayedJournal(t, ctx, contractId), map[server.Id]ByteCount{network.balanceIds[0]: 0})
	})
}

// With Redis lost before the close the debit still lands, in draw order (FS-45).
func TestRelayedDebitLandsWithRedisLost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 2*Mib, 1*Gib)
		contractId, grant, err := network.reserve(ctx, 64*Mib)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, grant, 64*Mib)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, balanceId := range network.balanceIds {
				server.Raise(r.Del(ctx, redisContractReservationKeys(balanceId)...).Err())
			}
		})
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.sourceId, 3*Mib, false))
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.destinationId, 3*Mib, false))
		// the first balance in draw order pays its room, the next the rest
		connect.AssertEqual(t, testingRelayedJournal(t, ctx, contractId), map[server.Id]ByteCount{
			network.balanceIds[0]: 2 * Mib,
			network.balanceIds[1]: 1 * Mib,
		})
	})
}

// Recovery that enrolls a live relayed token keeps it, forgetting only its marker; once the
// contract is terminal and its debit applied, recovery releases it; while the debit is pending it
// keeps it for the flush.
func TestRelayedRecoveryCustody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 1*Gib)
		contractId, grant, err := network.reserve(ctx, 64*Mib)
		if err != nil {
			t.Fatal(err)
		}
		balanceId := network.balanceIds[0]
		enroll := func() {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.ZAdd(ctx, redisContractReservationKeys(balanceId)[4], redis.Z{Score: 1, Member: contractId.String()}).Err())
			})
		}
		recover := func() (released bool, err error) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, redisContractAdmissionLock(contractId)))
				released, err = recoverRedisReservationWithFenceInTx(ctx, tx, balanceId, redisReservationRecoveryCandidate{contractId: contractId, amount: grant})
			}, server.TxReadCommitted)
			return
		}

		// live: published, the token kept
		enroll()
		released, err := recover()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, released, false)
		connect.AssertEqual(t, network.held(ctx), []ByteCount{grant})

		// terminal with its debit pending: kept for the flush
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.sourceId, 1000, false))
		connect.AssertEqual(t, nil, CloseContract(ctx, contractId, network.destinationId, 1000, false))
		enroll()
		_, err = recover()
		connect.AssertEqual(t, err, errRedisReservationDebitPending)
		connect.AssertEqual(t, network.held(ctx), []ByteCount{grant})

		// terminal with its debit applied but not yet released: released
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE service_debit_journal SET applied = true WHERE usage_id = $1`, contractId))
		})
		released, err = recover()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, released, true)
		connect.AssertEqual(t, network.held(ctx), []ByteCount{0})
		connect.AssertEqual(t, testingRelayedJournalTotal(t, ctx, contractId), ByteCount(1000))
	})
}
