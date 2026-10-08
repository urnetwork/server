// Same-key refusals happen before financial SQL while independent payers work.
package model

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// Hold the actual native debit entry after direct-backend admission. A positive
// ownership refusal, unchanged money and independent completion establish the
// model gate; the full public stress test separately holds its real grant query.
func TestTransferBalanceOwnerCoversNativeLegacyAdmissionAndCache(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first := newLegacyPayerTestIntent(t, ctx, f, server.NewId(), 100, 11)
		second := newLegacyPayerTestIntent(t, ctx, f, server.NewId(), 100, 11)
		native := createRedisAdmissionTest(ctx, f, 100)
		_ = createRedisAdmissionTest(ctx, f, 200)
		server.Raise(CloseContract(ctx, native.ContractId, f.sourceId, 7, false))
		server.Raise(CloseContract(ctx, native.ContractId, f.destinationId, 7, false))
		other := newNetEscrowOrderingTestFixture(t, ctx)
		otherId := newLegacyPayerTestIntent(t, ctx, other, server.NewId(), 100, 11)
		key := server.NewPgOwnershipKey("transfer_balance", f.balanceId)
		entered := make(chan server.PgOwnershipEvent, 1)
		release := make(chan struct{})
		var heldOnce atomic.Bool
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		nativeCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted && !event.TransactionScoped && slices.Contains(event.Keys, key) && heldOnce.CompareAndSwap(false, true) {
				entered <- event
				select {
				case <-release:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}
		})
		type nativeResult struct {
			result TransferDebitFlushResult
			err    error
		}
		done := make(chan nativeResult, 1)
		go func() {
			result, err := FlushTransferDebits(nativeCtx, transferDebitShard(f.balanceId), nil, 1)
			done <- nativeResult{result: result, err: err}
		}()
		joined := false
		defer func() {
			unblock()
			if !joined {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("native balance owner failed bounded cleanup")
				}
			}
		}()
		var holder server.PgOwnershipEvent
		select {
		case holder = <-entered:
		case <-ctx.Done():
			t.Fatal("actual native debit never acquired its balance owner", ctx.Err())
		}
		refused := map[string]int{}
		observeRefusal := func(name string) context.Context {
			return server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
				if event.Kind == server.PgOwnershipRefused && event.TransactionScoped && slices.Contains(event.Keys, key) {
					refused[name]++
				}
			})
		}
		complete, busy, gate, err := flushLegacySettlement(observeRefusal("single"), first)
		if err != nil || complete || !busy || gate != legacySettlementBusyAdmission || refused["single"] != 1 {
			t.Fatal("legacy single entered an admitted native balance", complete, busy, gate, refused, err)
		}
		attempts, err := flushLegacySettlementCohort(observeRefusal("cohort"), []server.Id{first, second})
		if err != nil || len(attempts) != 2 || refused["cohort"] != 1 {
			t.Fatal("legacy cohort lost common ownership", attempts, refused, err)
		}
		for _, attempt := range attempts {
			if attempt.completed || !attempt.busy || attempt.fallback || attempt.busyGate != legacySettlementBusyAdmission {
				t.Fatal("refused cohort attempted individual financial fallback", attempt)
			}
		}
		admissionCtx := observeRefusal("admission")
		var admitted *TransferEscrow
		err = server.HandleError(func() {
			server.Tx(admissionCtx, func(tx server.PgTx) {
				var err error
				admitted, _, err = createTransferEscrowInTx(admissionCtx, tx,
					f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 13, nil)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if !errors.Is(err, errTransferBalanceOwnershipBusy) || admitted != nil || refused["admission"] != 1 {
			t.Fatal("legacy reservation escaped the common grant owner", admitted, refused, err)
		}
		var snapshots map[server.Id]netEscrowSnapshot
		server.Db(ctx, func(conn server.PgConn) {
			snapshots = readNetEscrowSnapshots(ctx, conn, []server.Id{f.balanceId})
		})
		cacheCommittedNetEscrowSnapshots(observeRefusal("cache"), snapshots)
		if refused["cache"] != 1 {
			t.Fatal("optional snapshot writer bypassed balance ownership", refused)
		}
		// The ordinary Redis reservation path remains independent of the PG
		// financial owner and retains its conservative unpublished debt.
		_ = createRedisAdmissionTest(ctx, f, 13)
		complete, busy, _, err = flushLegacySettlement(ctx, otherId)
		if err != nil || !complete || busy {
			t.Fatal("held native owner blocked another payer", complete, busy, err)
		}
		requireLegacySettlementTestState(t, ctx, other, otherId, false, true, 989, 0)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000
 AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($3))=2
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$4 AND settled)
 AND (SELECT count(*) FROM transfer_debit_journal WHERE balance_id=$2 AND NOT applied)=1`,
				holder.BackendPid, f.balanceId, []server.Id{first, second}, native.ContractId).Scan(&exact))
			if !exact || Testing_NetEscrowByteCount(ctx, f.balanceId) != 513 {
				t.Fatal("refused writers changed financial custody before native admission released")
			}
		})
		unblock()
		select {
		case result := <-done:
			joined = true
			if result.err != nil || result.result.Applied != 1 || result.result.Released != 1 || result.result.Busy != 0 || result.result.Failed != 0 {
				t.Fatal("actual native debit did not finish once", result)
			}
		case <-ctx.Done():
			t.Fatal("native financial owner did not finish", ctx.Err())
		}
		attempts, err = flushLegacySettlementCohort(ctx, []server.Id{first, second})
		if err != nil || len(attempts) != 2 || !attempts[0].completed || !attempts[1].completed {
			t.Fatal("retained legacy work did not resume under the same owner", attempts, err)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 971 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 213 || reruns.Load() != 0 {
			t.Fatal("mixed owners lost exact debit, neighboring reservation or no-retry policy", credit, pending, applied, reruns.Load())
		}
		complete, busy, _, err = flushLegacySettlement(ctx, first)
		if err != nil || complete || !busy {
			t.Fatal("common-owner replay reclaimed a terminal intent", complete, busy, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `WITH wanted AS (
 SELECT * FROM unnest($1::uuid[], $2::bigint[]) AS wanted(contract_id, bytes)
), unapplied AS (
 SELECT allocation FROM pending_task
 CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
 WHERE function_name=$4 AND (args_json::jsonb->>'applied')::boolean=false
  AND (allocation->>'network_id')::uuid=$3
) SELECT NOT EXISTS (
 SELECT 1 FROM wanted WHERE
  COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep
   WHERE contract_id=wanted.contract_id AND network_id=$3),0)<>wanted.bytes
  OR COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep
   WHERE contract_id=wanted.contract_id AND network_id=$3),0)<>0
) AND COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0)
 +COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0)=29
 AND COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0)
 +COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)=0
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$5)=971
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE balance_id=$5)`,
				[]server.Id{first, second, native.ContractId}, []int64{11, 11, 7}, f.destinationNetworkId,
				task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), f.balanceId).Scan(&exact))
			if !exact {
				t.Fatal("common owners or replay changed exact per-contract and aggregate provider accounting")
			}
		})
		server.Redis(ctx, func(r server.RedisClient) {
			if value := r.Get(ctx, accountBalanceNetPayoutByteCountKey(f.destinationNetworkId)).Val(); value != "" && value != "0" {
				t.Fatal("common owner duplicated a durable provider contribution in Redis")
			}
		})
	})
}

// The direct owner is admitted on its transaction's backend. Positive refusal
// observations in each caller prove exclusion; cleanup always joins its end.
func holdTransferBalanceTestOwner(t testing.TB, ctx context.Context, ids []server.Id) func() {
	t.Helper()
	ready, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- server.HandleError(func() {
			server.OwnedTx(ctx, transferBalanceOwnershipKeys(ids), func(tx server.PgTx) {
				close(ready)
				select {
				case <-release:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal("balance ownership fixture never entered its transaction", err)
	case <-ctx.Done():
		t.Fatal("balance ownership fixture admission expired", ctx.Err())
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Error("balance ownership fixture failed to join", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("balance ownership fixture exceeded bounded cleanup")
			}
		})
	}
}

// An absent grant still has a revision row touched by the outcome trigger.
func TestTransferBalanceOwnerIncludesDanglingEscrowRevision(t *testing.T) {
	testTransferBalanceDanglingOwnership(t, 1)
}

// A cohort's bounded overflow falls back before mutation; its ordinary owner
// must admit the complete escrow scope instead of discarding dangling keys.
func TestTransferBalanceOwnerIncludesOverflowFallbackRevision(t *testing.T) {
	testTransferBalanceDanglingOwnership(t, legacyFinancialCohortEscrowLimit)
}

func testTransferBalanceDanglingOwnership(t *testing.T, missingCount int) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first := newLegacyPayerTestIntent(t, ctx, f, server.NewId(), 100, 11)
		second := newLegacyPayerTestIntent(t, ctx, f, server.NewId(), 100, 11)
		missingIds := make([]server.Id, missingCount)
		for i := range missingIds {
			missingIds[i] = server.NewId()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
 SELECT $1,balance_id,4096 FROM unnest($2::uuid[]) AS balance_id`, first, missingIds))
		}, server.TxReadCommitted, server.OptNoRetry())
		var before int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT sum(revision) FROM transfer_balance_net_escrow_revision WHERE balance_id=ANY($1)`, missingIds).Scan(&before))
		})
		heldId := missingIds[len(missingIds)-1]
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{heldId})
		defer release()
		var refused atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused && slices.Contains(event.Keys, server.NewPgOwnershipKey("transfer_balance", heldId)) {
				refused.Add(1)
			}
		})
		attempts, err := flushLegacySettlementCohort(observed, []server.Id{first, second})
		if err != nil {
			t.Fatal("dangling membership changed cohort authority", err)
		}
		if missingCount+2 > legacyFinancialCohortEscrowLimit {
			if len(attempts) != 1 || !attempts[0].fallback || attempts[0].completed || refused.Load() != 0 {
				t.Fatal("overflow did not defer before complete ownership", attempts, refused.Load())
			}
		} else {
			if len(attempts) != 2 || refused.Load() != 1 {
				t.Fatal("cohort omitted a dangling revision owner", attempts, refused.Load())
			}
			for _, attempt := range attempts {
				if !attempt.busy || attempt.completed || attempt.fallback || attempt.busyGate != legacySettlementBusyAdmission {
					t.Fatal("dangling revision conflict entered financial fallback", attempt)
				}
			}
		}
		beforeSingle := refused.Load()
		complete, busy, gate, err := flushLegacySettlement(observed, first)
		if err != nil || complete || !busy || gate != legacySettlementBusyAdmission || refused.Load() != beforeSingle+1 {
			t.Fatal("single financial owner omitted a dangling revision", complete, busy, gate, refused.Load(), err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT sum(revision) FROM transfer_balance_net_escrow_revision WHERE balance_id=ANY($1))=$2
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$3)=1000
 AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($4))=2
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($4))`,
				missingIds, before, f.balanceId, []server.Id{first, second}).Scan(&exact))
			if !exact {
				t.Fatal("refused dangling ownership changed money or revision")
			}
		})
		release()
		for _, id := range []server.Id{first, second} {
			complete, busy, _, err = flushLegacySettlement(ctx, id)
			if err != nil || !complete || busy {
				t.Fatal("retained dangling membership did not settle under ordinary authority", complete, busy, err)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT sum(revision) FROM transfer_balance_net_escrow_revision WHERE balance_id=ANY($1))>$2
 AND NOT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=ANY($1))
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$3)=978
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($4))
 AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($4))=22`,
				missingIds, before, f.balanceId, []server.Id{first, second}).Scan(&exact))
			if !exact || reruns.Load() != 0 {
				t.Fatal("dangling membership changed debit or failed to retain its shared revision", reruns.Load())
			}
		})
	})
}

// Retirement and a delayed metadata post must use the same owner as debit;
// refused cleanup retains both rows and its input cursor for the next turn.
func TestTransferBalanceOwnerCoversMetadataAndRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := newLegacyPayerTestIntent(t, ctx, f, server.NewId(), 100, 11)
		completed, busy, _, err := flushLegacySettlement(ctx, contract)
		if err != nil || !completed || busy {
			t.Fatal("retention fixture failed its actual financial settlement", completed, busy, err)
		}
		expired := newNetEscrowOrderingTestFixture(t, ctx)
		orphan := newNetEscrowOrderingTestFixture(t, ctx)
		orphanId := server.Id{15: 1}
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			// Preserve the terminal usage/debit while modeling a lost legacy
			// metadata post and an already eligible historical retention row.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET create_time=$2::timestamp-interval '301 days',reap_time=$2::timestamp-interval '1 day'
 WHERE contract_id=$1`, contract, now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=false WHERE contract_id=$1`, contract))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2::timestamp-interval '8 days' WHERE balance_id=$1`, expired.balanceId, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,17)`, orphanId, orphan.balanceId))
		}, server.TxReadCommitted, server.OptNoRetry())
		ids := []server.Id{f.balanceId, expired.balanceId, orphan.balanceId}
		release := holdTransferBalanceTestOwner(t, ctx, ids)
		defer release()
		var refused atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused.Add(1)
			}
		})
		err = server.HandleError(func() {
			server.Tx(observed, func(tx server.PgTx) {
				settleEscrowMetadataInTx(observed, tx, contract, now, map[server.Id]sweepPayout{
					f.balanceId: {escrowBalanceByteCount: 100, payoutByteCount: 11},
				})
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if !errors.Is(err, errTransferBalanceOwnershipBusy) || refused.Load() != 1 {
			t.Fatal("metadata entered a held balance owner", refused.Load(), err)
		}
		removeCompletedTransferBalanceBatch(observed, []server.Id{expired.balanceId}, now.Add(-7*24*time.Hour))
		if refused.Load() != 2 {
			t.Fatal("grant retention omitted common ownership", refused.Load())
		}
		removeDueContractBatches(observed, now, now.Add(-300*24*time.Hour), 1)
		if refused.Load() != 3 {
			t.Fatal("contract retention omitted escrow revision ownership", refused.Load())
		}
		removed, cursor, done := SweepOrphanContractData(observed, SweepOrphanCursor{Step: 1}, 1, 1)
		if removed != 0 || cursor.Step != 1 || len(cursor.Key) != 0 || done || refused.Load() != 4 {
			t.Fatal("orphan refusal lost its row or advanced the refused input cursor", removed, cursor, done, refused.Load())
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2 AND NOT settled)
 AND EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND outcome='settled')
 AND EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$3)
 AND EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$4 AND balance_id=$5)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=989`,
				contract, f.balanceId, expired.balanceId, orphanId, orphan.balanceId).Scan(&exact))
			if !exact {
				t.Fatal("refused retirement mutated held financial custody")
			}
		})
		release()
		removeCompletedTransferBalanceBatch(ctx, []server.Id{expired.balanceId}, now.Add(-7*24*time.Hour))
		removeDueContractBatches(ctx, now, now.Add(-300*24*time.Hour), 1)
		removed, _, _ = SweepOrphanContractData(ctx, cursor, 1, 1)
		if removed != 1 {
			t.Fatal("retained orphan cursor did not resume exactly once", removed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($2))
 AND NOT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$3)
 AND EXISTS(SELECT 1 FROM transfer_balance_net_escrow_revision WHERE balance_id=$3 AND revision>0)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$4)=989
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$5)=1000`,
				contract, []server.Id{contract, orphanId}, expired.balanceId, f.balanceId, orphan.balanceId).Scan(&exact))
			if !exact || reruns.Load() != 0 {
				t.Fatal("resumed cleanup changed debit or failed its tombstone/cursor custody", reruns.Load())
			}
		})
	})
}
