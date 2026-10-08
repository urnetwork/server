// Real grant writers retain complete ownership through commit and roll back
// rather than widening an admitted set or silently omitting revoked credit.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Catch the normal server transaction boundary without changing its outcome.
func rareGrantModelError(run func()) (err error) {
	server.HandleError(run, func(caught error) { err = caught })
	return
}

// Retain a real no-retry server transaction while the test runs SQL on its
// actual backend. Only the test goroutine uses tx while the callback is parked.
func beginRareGrantTestTx(t testing.TB, ctx context.Context) (server.PgTx, func(bool) error) {
	t.Helper()
	ready := make(chan server.PgTx, 1)
	release := make(chan bool, 1)
	done := make(chan error, 1)
	abort := errors.New("synthetic held transaction rollback")
	go func() {
		done <- rareGrantModelError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				ready <- tx
				select {
				case commit := <-release:
					if !commit {
						server.Raise(abort)
					}
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		})
	}()
	var once sync.Once
	var finalErr error
	finish := func(commit bool) error {
		once.Do(func() {
			release <- commit
			finalErr = <-done
			if errors.Is(finalErr, abort) {
				finalErr = nil
			}
		})
		return finalErr
	}
	select {
	case tx := <-ready:
		return tx, finish
	case err := <-done:
		t.Fatalf("held transaction did not start: %v", err)
	case <-ctx.Done():
		finish(false)
		t.Fatal("held transaction did not start before context end")
	}
	return nil, nil
}

// Expiration never rewrites the monetary principal or consumed-byte history.
type rareGrantState struct {
	EndTime    time.Time
	StartBytes int64
	Bytes      int64
	Revenue    int64
	Subsidy    int64
}

func rareGrantStates(ctx context.Context, networkId server.Id) map[server.Id]rareGrantState {
	states := map[server.Id]rareGrantState{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT balance_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,subsidy_net_revenue_nano_cents FROM transfer_balance WHERE network_id=$1`, networkId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var state rareGrantState
				server.Raise(rows.Scan(&id, &state.EndTime, &state.StartBytes, &state.Bytes, &state.Revenue, &state.Subsidy))
				states[id] = state
			}
		})
	})
	return states
}

// A second real constructor commits a matching grant exactly after admission.
// The refund must preserve both grants and the renewal until a fresh attempt
// can acquire both, including when the old grant set was otherwise sufficient.
func TestRareGrantEntitlementRejectsExpandedScope(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-refund-expansion", server.NewId())
		now := server.NowUtc().Truncate(time.Microsecond)
		start, end := now.Add(-time.Hour), now.Add(24*time.Hour)
		transactionId := "synthetic-renewal-" + server.NewId().String()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{NetworkId: networkId, SubscriptionType: SubscriptionTypeSupporter, StartTime: start, EndTime: end, SubscriptionMarket: SubscriptionMarketStripe, TransactionId: transactionId, NetRevenue: 500}))
			server.Raise(AddProTransferBalanceInTx(tx, ctx, networkId, 1024, start, end))
		}, server.TxReadCommitted, server.OptNoRetry())
		admissions := 0
		var reruns atomic.Int64
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted {
				admissions++
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(AddProTransferBalanceInTx(tx, ctx, networkId, 2048, start, end))
				}, server.TxReadCommitted, server.OptNoRetry())
			}
		})
		err := rareGrantModelError(func() {
			EndReconciledEntitlementForTransactions(observed, SubscriptionMarketStripe, []string{transactionId}, now)
		})
		if !errors.Is(err, errTransferBalanceOwnershipBusy) || admissions != 1 || reruns.Load() != 0 {
			t.Fatal("scope expansion did not roll back its one no-retry attempt", err)
		}
		before := rareGrantStates(ctx, networkId)
		if len(before) != 2 {
			t.Fatal("real competing grant did not commit")
		}
		for _, grant := range before {
			if !grant.EndTime.Equal(end) {
				t.Fatal("scope refusal partially expired a grant")
			}
		}
		var renewalEnd time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT end_time FROM subscription_renewal WHERE network_id=$1 AND transaction_id=$2`, networkId, transactionId).Scan(&renewalEnd))
		})
		if !renewalEnd.Equal(end) {
			t.Fatal("scope refusal consumed the renewal")
		}
		ended := EndReconciledEntitlementForTransactions(ctx, SubscriptionMarketStripe, []string{transactionId}, now)
		if len(ended) != 1 || ended[0] != networkId {
			t.Fatal("fresh complete scope did not end the entitlement")
		}
		after := rareGrantStates(ctx, networkId)
		if len(after) != len(before) {
			t.Fatal("refund changed grant cardinality")
		}
		for id, want := range before {
			want.EndTime = now
			if got, ok := after[id]; !ok || got != want {
				t.Fatal("refund failed exact expiration or altered monetary history")
			}
		}
		if len(EndReconciledEntitlementForTransactions(ctx, SubscriptionMarketStripe, []string{transactionId}, now)) != 0 {
			t.Fatal("replayed refund ended a second entitlement")
		}
	})
}

// A code redeemed after discovery is a new grant, not permission to acquire a
// second key set. Earlier code voids and the receipt must roll back together.
func TestRareGrantClawbackRedeemRaceRollsBackWholeRefund(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-clawback-race", server.NewId())
		purchaseEventId := "synthetic-checkout-" + server.NewId().String()
		codes := []*BalanceCode{}
		for i := 0; i < 3; i++ {
			code, err := CreateBalanceCode(ctx, 1024, 24*time.Hour, 500, fmt.Sprintf("%s/%d", purchaseEventId, i), "synthetic-receipt", "buyer@example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			codes = append(codes, code)
		}
		slices.SortFunc(codes, func(a, b *BalanceCode) int { return a.BalanceCodeId.Cmp(b.BalanceCodeId) })
		testingRedeemPaymentBalanceCode(t, ctx, networkId, codes[1].Secret)
		refundId := "synthetic-refund-" + server.NewId().String()
		now := server.NowUtc().Truncate(time.Microsecond)
		admissions := 0
		var reruns atomic.Int64
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted {
				admissions++
				testingRedeemPaymentBalanceCode(t, ctx, networkId, codes[2].Secret)
			}
		})
		apply := func(callCtx context.Context) {
			server.Tx(callCtx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(callCtx, `INSERT INTO stripe_refund(refund_id,charge_id) VALUES($1,'synthetic-charge')`, refundId))
				found, _, _ := ClawbackBalanceCodesForPurchaseEventInTx(tx, callCtx, purchaseEventId, now)
				if !found {
					server.Raise(errors.New("synthetic code scope missing"))
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		err := rareGrantModelError(func() { apply(observed) })
		if !errors.Is(err, errTransferBalanceOwnershipBusy) || admissions != 1 || reruns.Load() != 0 {
			t.Fatal("raced redeem did not abort the one refund attempt", err)
		}
		var receipts, cancelled int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM stripe_refund WHERE refund_id=$1),(SELECT count(*) FROM transfer_balance_code WHERE balance_code_id=ANY($2::uuid[]) AND cancel_time IS NOT NULL)`, refundId, []server.Id{codes[0].BalanceCodeId, codes[1].BalanceCodeId, codes[2].BalanceCodeId}).Scan(&receipts, &cancelled))
		})
		before := rareGrantStates(ctx, networkId)
		if receipts != 0 || cancelled != 0 || len(before) != 2 {
			t.Fatal("partial refund committed or raced redemption disappeared")
		}
		for _, grant := range before {
			if !grant.EndTime.After(now) {
				t.Fatal("raced refund partially expired a grant")
			}
		}
		apply(ctx)
		after := rareGrantStates(ctx, networkId)
		if len(after) != len(before) {
			t.Fatal("clawback changed grant cardinality")
		}
		for id, want := range before {
			want.EndTime = now
			if got, ok := after[id]; !ok || got != want {
				t.Fatal("fresh clawback did not expire both exact grants conservatively")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM stripe_refund WHERE refund_id=$1),(SELECT count(*) FROM transfer_balance_code WHERE balance_code_id=$2 AND cancel_time IS NOT NULL)`, refundId, codes[0].BalanceCodeId).Scan(&receipts, &cancelled))
		})
		if receipts != 1 || cancelled != 1 {
			t.Fatal("fresh refund did not atomically consume its receipt and unredeemed code")
		}
	})
}

// Begin, drain and reap all touch the same existing private grant. A held
// financial owner must leave lifecycle/client state intact until redelivery.
func TestRareGrantProberLifecycleRefusesHeldFinancialOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		independent := shardTestOwner(t, ctx, shardTestKey(1))
		before := rareGrantStates(ctx, owner.NetworkId)
		held, finish := beginRareGrantTestTx(t, ctx)
		defer finish(false)
		admitted, err := tryTransferBalanceOwnershipInTx(ctx, held, []server.Id{owner.BalanceId})
		if err != nil || !admitted {
			t.Fatal("hold exact grant owner", err)
		}
		var reruns atomic.Int64
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		if err := DrainProberShard(observed, owner.Key); !errors.Is(err, errTransferBalanceOwnershipBusy) {
			t.Fatal("drain bypassed the held financial owner", err)
		}
		nextKey := shardTestKey(0)
		if next, err := BeginProberShard(observed, nextKey, 64*1024, time.Hour); !errors.Is(err, errTransferBalanceOwnershipBusy) || next != nil {
			t.Fatal("new epoch bypassed previous grant ownership", err)
		}
		var state string
		var active bool
		var replacements int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT state,(SELECT active FROM network_client WHERE client_id=$3),(SELECT count(*) FROM prober_shard_run WHERE task_id=$4 AND epoch=$5) FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, owner.Key.TaskId, owner.Key.Epoch, owner.ClientId, nextKey.TaskId, nextKey.Epoch).Scan(&state, &active, &replacements))
		})
		if state != "active" || !active || replacements != 0 || rareGrantStates(ctx, owner.NetworkId)[owner.BalanceId] != before[owner.BalanceId] {
			t.Fatal("refused lifecycle changed the grant, client, or registry")
		}
		if err := DrainProberShard(ctx, independent.Key); err != nil {
			t.Fatal("independent lifecycle could not progress", err)
		}
		server.Raise(finish(true))
		if err := DrainProberShard(observed, owner.Key); err != nil {
			t.Fatal("drain redelivery failed", err)
		}
		heldReap, finishReap := beginRareGrantTestTx(t, ctx)
		defer finishReap(false)
		admitted, err = tryTransferBalanceOwnershipInTx(ctx, heldReap, []server.Id{owner.BalanceId})
		if err != nil || !admitted {
			t.Fatal("hold exact reaper grant owner", err)
		}
		if deleted, err := ReapProberShard(observed, owner.Key); deleted || !errors.Is(err, errTransferBalanceOwnershipBusy) {
			t.Fatal("reaper bypassed held grant owner", err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 1 || b != 1 {
			t.Fatal("refused reaper deleted part of the private cohort")
		}
		server.Raise(finishReap(true))
		if deleted, err := ReapProberShard(observed, owner.Key); !deleted || err != nil {
			t.Fatal("reaper redelivery failed", err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 || reruns.Load() != 0 {
			t.Fatal("reaper retained its cohort or reran a transaction callback")
		}
	})
}
