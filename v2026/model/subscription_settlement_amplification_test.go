// Pins legacy empty-escrow settlement work independently of machine timing.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// A fully reserved legacy grant adds no payout but must still become terminal.
func TestEscrowSettlementLegacyRowsUseOneStatement(t *testing.T) {
	sweepPayouts := map[server.Id]sweepPayout{}
	for range 45 {
		sweepPayouts[server.NewId()] = sweepPayout{}
	}
	sweepPayouts[server.NewId()] = sweepPayout{
		escrowBalanceByteCount: 1024,
		payoutByteCount:        128,
		returnByteCount:        896,
	}
	batch := &pgx.Batch{}
	contractId := server.NewId()
	settleTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	queueEscrowSettlementUpdates(batch, contractId, settleTime, sweepPayouts)
	if batch.Len() != 1 {
		t.Fatalf("legacy settlement statements=%d, want one for all %d captured rows", batch.Len(), len(sweepPayouts))
	}
	args := batch.QueuedQueries[0].Arguments
	if len(args) != 4 || args[0] != contractId || args[1] != settleTime {
		t.Fatal("bulk settlement lost its contract or timestamp boundary")
	}
	balanceIds, payouts := args[2].([]server.Id), args[3].([]ByteCount)
	if len(balanceIds) != len(sweepPayouts) || len(payouts) != len(balanceIds) {
		t.Fatal("bulk settlement dropped a captured zero-byte row")
	}
	for index, balanceId := range balanceIds {
		payout, found := sweepPayouts[balanceId]
		if !found || payouts[index] != payout.payoutByteCount ||
			(index > 0 && balanceIds[index-1].Cmp(balanceId) >= 0) {
			t.Fatal("bulk settlement changed an amount, duplicated a row, or lost stable ordering")
		}
	}
}

// An empty captured set cannot become a contract-wide update.
func TestEscrowSettlementEmptyBatchHasNoStatement(t *testing.T) {
	batch := &pgx.Batch{}
	queueEscrowSettlementUpdates(batch, server.NewId(), time.Time{}, nil)
	if batch.Len() != 0 {
		t.Fatal("empty settlement queued a statement")
	}
}

// A real legacy cohort keeps all terminal rows, exact money, and sibling scope.
func TestEscrowSettlementLegacyRowsPreserveAccounting(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		const used = ByteCount(128)
		fixture := newForceCloseDisputeFixture(t, ctx, true, true, used, used, 1024)
		zeroBalanceIds := addLegacyEmptySettlementEscrows(t, ctx, fixture, 45)
		var sourceNetworkId, sourceId, destinationNetworkId, destinationId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT source_network_id, source_id, destination_network_id, destination_id
				FROM transfer_contract WHERE contract_id=$1`, fixture.contractId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("synthetic sibling source missing")
				}
				server.Raise(rows.Scan(&sourceNetworkId, &sourceId, &destinationNetworkId, &destinationId))
			})
		})
		// A second live contract shares the funded balance. Its reservation and
		// escrow marker must survive the first contract's set-based update.
		siblingId, _, err := CreateContract(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024)
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, netEscrowKey(zeroBalanceIds[0]), 512, 0).Err())
		})
		selected, err := ForceCloseOpenContractIds(ctx, fixture.cutoff, 10, 1, 1, 0)
		if selected != 1 || err != nil {
			t.Fatalf("legacy close selected=%d error=%v", selected, err)
		}
		assertSettlementEscrowState(t, ctx, fixture, 46, used, forceCloseDisputeInitialBalance-used)
		proof, snapshot := readContractExpiryTestSnapshot(t, ctx, fixture.contractId)
		if snapshot.ByteCount != used || snapshot.Expiry == nil || len(snapshot.Providers) != 1 ||
			snapshot.Providers[0].ClientId != destinationId || snapshot.Providers[0].NetworkId != fixture.providerNetworkId {
			t.Fatalf("batched settlement changed completed provider usage: %s", proof)
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT settled, coalesce(payout_byte_count,0)
				FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2`, siblingId, fixture.balanceId)
			server.WithPgResult(rows, err, func() {
				var settled bool
				var payout ByteCount
				if !rows.Next() {
					t.Fatal("synthetic sibling did not share the funded balance")
				}
				server.Raise(rows.Scan(&settled, &payout))
				if settled || payout != 0 {
					t.Fatal("set-based settlement crossed its contract boundary")
				}
			})
		})
		server.Redis(ctx, func(r server.RedisClient) {
			if value := r.Get(ctx, netEscrowKey(fixture.balanceId)).Val(); value != "1024" {
				t.Fatalf("sibling reservation=%q, want 1024", value)
			}
			if value := r.Get(ctx, accountBalanceNetPayoutByteCountKey(fixture.providerNetworkId)).Val(); value != "128" {
				t.Fatalf("provider payout=%q, want 128", value)
			}
			if r.Get(ctx, netEscrowKey(zeroBalanceIds[0])).Val() != "512" || r.TTL(ctx, netEscrowKey(zeroBalanceIds[0])).Val() != -1 {
				t.Fatal("zero reservation changed an unrelated mirror value or TTL")
			}
			for _, balanceId := range zeroBalanceIds[1:] {
				if !errors.Is(r.Get(ctx, netEscrowKey(balanceId)).Err(), redis.Nil) {
					t.Fatal("zero reservation created a Redis key")
				}
			}
		})
		if err := SettleEscrow(ctx, fixture.contractId, ContractOutcomeSettled); err != nil {
			t.Fatal(err)
		}
		assertSettlementEscrowState(t, ctx, fixture, 46, used, forceCloseDisputeInitialBalance-used)
		replayedProof, _ := readContractExpiryTestSnapshot(t, ctx, fixture.contractId)
		if !bytes.Equal(proof, replayedProof) {
			t.Fatal("duplicate settlement changed its retained usage proof")
		}
		usages, err := GetStEpochProviderUsage(ctx, start, server.NowUtc().Add(time.Hour))
		if err != nil || len(usages) != 1 || usages[0].ClientId != destinationId ||
			usages[0].NetworkId != fixture.providerNetworkId || usages[0].PayoutByteCount != int64(used) {
			t.Fatalf("legacy zero rows or duplicate settlement changed epoch usage: %+v, %v", usages, err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if r.Get(ctx, netEscrowKey(fixture.balanceId)).Val() != "1024" ||
				r.Get(ctx, accountBalanceNetPayoutByteCountKey(fixture.providerNetworkId)).Val() != "128" {
				t.Fatal("repeat settlement repeated a reservation or provider post")
			}
		})
	})
}

// Zero use has no debit; a zero grant additionally has no mirror release.
func TestEscrowSettlementZeroUseOmitsEmptyPosts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, test := range []struct {
			grant ByteCount
			posts int
		}{
			{grant: 0, posts: 1},
			{grant: 1024, posts: 2},
		} {
			fixture := newForceCloseDisputeFixture(t, ctx, true, true, 0, 0, test.grant)
			addLegacyEmptySettlementEscrows(t, ctx, fixture, 45)
			var beforeMirror string
			var beforeMirrorErr error
			server.Redis(ctx, func(r server.RedisClient) {
				beforeMirror, beforeMirrorErr = r.Get(ctx, netEscrowKey(fixture.balanceId)).Result()
			})
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=false WHERE contract_id=$1`, fixture.contractId))
				var closed bool
				var err error
				posts, closed, err = settleEscrowInTx(ctx, tx, fixture.contractId, ContractOutcomeSettled)
				if err != nil || !closed {
					t.Fatalf("zero-use settlement closed=%t error=%v", closed, err)
				}
			}, server.TxReadCommitted)
			if len(posts) != test.posts {
				t.Fatalf("grant=%d zero-use posts=%d, want %d without empty debit/release work", test.grant, len(posts), test.posts)
			}
			server.RunPosts(ctx, posts...)
			assertSettlementEscrowState(t, ctx, fixture, 46, 0, forceCloseDisputeInitialBalance)
			server.Redis(ctx, func(r server.RedisClient) {
				mirror, mirrorErr := r.Get(ctx, netEscrowKey(fixture.balanceId)).Result()
				if test.grant == 0 {
					if mirror != beforeMirror || !errors.Is(mirrorErr, beforeMirrorErr) {
						t.Fatal("zero grant changed its existing mirror marker")
					}
				} else if !errors.Is(mirrorErr, redis.Nil) {
					t.Fatal("zero use retained a positive reservation")
				}
				if !errors.Is(r.Get(ctx, accountBalanceNetPayoutByteCountKey(fixture.providerNetworkId)).Err(), redis.Nil) {
					t.Fatal("zero-use settlement paid the provider")
				}
			})
		}
	})
}

// Rejected financial work must never reach either batched marks or releases.
func TestEscrowSettlementLegacyRejectionKeepsReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const grant = ByteCount(32 * 1024 * 1024)
		fixture := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*grant, grant)
		before := fixture.state(t, ctx)
		addLegacyEmptySettlementEscrows(t, ctx, fixture, 45)
		selected, err := ForceCloseOpenContractIds(ctx, fixture.cutoff, 10, 1, 1, 0)
		if selected != 1 || !errors.Is(err, errContractInsufficientEscrow) {
			t.Fatal("legacy underfunded dispute lost the accounting guard")
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT count(*), count(*) FILTER(WHERE settled), coalesce(sum(payout_byte_count),0),
				(SELECT dispute AND outcome IS NULL FROM transfer_contract WHERE contract_id=$1),
				(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)
				FROM transfer_escrow WHERE contract_id=$1`, fixture.contractId, fixture.balanceId)
			server.WithPgResult(rows, err, func() {
				var count, settled, payout, balance int64
				var unresolvedDispute bool
				if !rows.Next() {
					t.Fatal("synthetic rejection state missing")
				}
				server.Raise(rows.Scan(&count, &settled, &payout, &unresolvedDispute, &balance))
				if count != 46 || settled != 0 || payout != 0 || !unresolvedDispute || balance != before.payerBalanceByteCount {
					t.Fatal("rejection marked or paid legacy escrow")
				}
			})
		})
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.Get(ctx, netEscrowKey(fixture.balanceId)).Int64()
			if err != nil || value != before.netEscrowByteCount ||
				!errors.Is(r.Get(ctx, accountBalanceNetPayoutByteCountKey(fixture.providerNetworkId)).Err(), redis.Nil) {
				t.Fatal("rejection changed reservation or provider payout")
			}
		})
	})
}

// Quarantine shares the no-op rule but does not mark escrow paid or settled.
func TestEscrowSettlementZeroQuarantinePreservesUnrelatedMirror(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newForceCloseDisputeFixture(t, ctx, true, true, 0, 0, 0)
		zeroBalanceIds := addLegacyEmptySettlementEscrows(t, ctx, fixture, 45)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, netEscrowKey(zeroBalanceIds[0]), 512, 0).Err())
		})
		server.Tx(ctx, func(tx server.PgTx) {
			// Quarantine retains the original checkpoint proof before its
			// terminal claim, just as the real expiry owner does.
			state, err := prepareContractExpiryInTx(ctx, tx, fixture.contractId, fixture.cutoff)
			if err != nil || state == nil {
				t.Fatalf("synthetic quarantine did not retain expiry proof: %v", err)
			}
			if closed, err := claimContractOutcomeInTx(ctx, tx, fixture.contractId, ContractOutcomeSettled); err != nil || !closed {
				t.Fatalf("synthetic quarantine did not own its claim: %v", err)
			}
		}, server.TxReadCommitted)
		releaseNetEscrowForContract(ctx, fixture.contractId)
		server.Redis(ctx, func(r server.RedisClient) {
			if r.Get(ctx, netEscrowKey(zeroBalanceIds[0])).Val() != "512" || r.TTL(ctx, netEscrowKey(zeroBalanceIds[0])).Val() != -1 {
				t.Fatal("zero quarantine changed an unrelated mirror or its TTL")
			}
		})
	})
}

// Seeds generated legacy rows without creating real reservations for zero bytes.
func addLegacyEmptySettlementEscrows(t testing.TB, ctx context.Context, fixture *forceCloseDisputeFixture, count int) []server.Id {
	t.Helper()
	balanceIds := make([]server.Id, count)
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for index := range balanceIds {
				balanceIds[index] = server.NewId()
				batch.Queue(`INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time,
					start_balance_byte_count, balance_byte_count, net_revenue_nano_cents, purchase_token,
					subsidy_net_revenue_nano_cents, pro)
					SELECT $2, network_id, start_time, end_time, start_balance_byte_count, 0,
						net_revenue_nano_cents, purchase_token, subsidy_net_revenue_nano_cents, pro
					FROM transfer_balance WHERE balance_id=$1`, fixture.balanceId, balanceIds[index])
				batch.Queue(`INSERT INTO transfer_escrow(contract_id, balance_id, balance_byte_count)
					VALUES($1,$2,0)`, fixture.contractId, balanceIds[index])
			}
		})
	}, server.TxReadCommitted)
	return balanceIds
}

// Verifies durable terminal marks and the funded balance separately from Redis.
func assertSettlementEscrowState(t testing.TB, ctx context.Context, fixture *forceCloseDisputeFixture, wantRows int64, wantPayout, wantBalance ByteCount) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT count(*), count(*) FILTER(WHERE settled AND settle_time IS NOT NULL),
			coalesce(sum(payout_byte_count),0), count(*) FILTER(WHERE balance_byte_count=0 AND payout_byte_count<>0),
			(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)
			FROM transfer_escrow WHERE contract_id=$1`, fixture.contractId, fixture.balanceId)
		server.WithPgResult(rows, err, func() {
			var count, settled, payout, invalidZero, balance int64
			if !rows.Next() {
				t.Fatal("synthetic settlement state missing")
			}
			server.Raise(rows.Scan(&count, &settled, &payout, &invalidZero, &balance))
			if count != wantRows || settled != wantRows || payout != wantPayout || invalidZero != 0 || balance != wantBalance {
				t.Fatalf("settlement rows=%d settled=%d payout=%d invalid_zero=%d balance=%d", count, settled, payout, invalidZero, balance)
			}
		})
	})
}
