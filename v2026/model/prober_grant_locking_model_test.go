// Lock ownership is part of the prober optimization: rejected candidate
// windows must not retain locks when the next window changes their order.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Lock acquisition sorts by id, while the payer's hash selects in discovery
// order. Reversing those orders must preserve spread and companion affinity.
func TestDynamicProberGrantKeepsClientDistributionAfterLockSort(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		const count = 4
		ids := make([]server.Id, count)
		server.Tx(ctx, func(tx server.PgTx) {
			for index := range count {
				ids[index] = server.Id{0: 0xf0, 15: byte(index + 1)}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
					(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
					VALUES ($1,$2,$3,$4,$5,4096,0,false)`, ids[index], clients.payerNetworkId,
					now.Add(time.Duration(index-count)*time.Minute), now.Add(time.Hour), ProberTransferBalanceTopUp))
			}
		}, server.TxReadCommitted)
		seen := map[int]bool{}
		for suffix := range 256 {
			payerId := server.Id{0: 0xf1, 15: byte(suffix)}
			position := int(payerId.Hash() % count)
			if seen[position] {
				continue
			}
			seen[position] = true
			insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{payerId: clients.payerNetworkId})
			clients.payerId = payerId
			for _, companion := range []bool{false, true} {
				escrow, err := clients.create(ctx, 1024, companion)
				selectedId := server.Id{}
				if escrow != nil && len(escrow.Balances) == 1 {
					selectedId = escrow.Balances[0].BalanceId
				}
				if err != nil || selectedId != ids[count-1-position] {
					t.Fatalf("payer position %d companion=%t selected %s, want %s: %v", position, companion, selectedId, ids[count-1-position], err)
				}
			}
			if len(seen) == count {
				break
			}
		}
		if len(seen) != count {
			t.Fatal("synthetic clients did not cover every discovery position")
		}
	})
}

// Real NOWAIT attempts establish lock release before the next discovery or
// complete fallback query. No elapsed-time threshold stands in for ordering.
func TestDynamicProberGrantReleasesRejectedWindowLocks(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		checker := acquireContractLifecycleTestConnection(t, ctx)
		defer checker.Release()
		for _, fallback := range []bool{false, true} {
			clients := newEscrowSelectionTestClients(t, ctx)
			setDynamicProberIdentityForTest(t, ctx, clients)
			now := server.NowUtc()
			count := proberGrantFirstCount + 1
			if fallback {
				count += proberGrantExtendedCount
			}
			ids := make([]server.Id, 0, count)
			for index := range count {
				grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId,
					now.Add(time.Duration(index-count)*time.Minute), now.Add(time.Hour), 1024)
				ids = append(ids, grant.BalanceId)
				if index > 0 {
					clients.reserve(ctx, grant.BalanceId, 1024)
				}
			}
			if fallback {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
						SET net_revenue_nano_cents=1 WHERE balance_id=$1`, ids[0]))
				}, server.TxReadCommitted)
			}
			assertReleased := func(checkedIds []server.Id) {
				t.Helper()
				if _, err := checker.Exec(ctx, `SELECT balance_id FROM transfer_balance
					WHERE balance_id=ANY($1) ORDER BY balance_id FOR UPDATE NOWAIT`, checkedIds); err != nil {
					t.Fatalf("rejected candidate window retained balance locks: %v", err)
				}
			}
			extendedChecked, fallbackChecked := false, false
			server.Tx(ctx, func(tx server.PgTx) {
				query := &escrowGrantQueryTestTx{PgTx: tx, beforeGrant: func(args []any) {
					switch len(args) {
					case 5: // The extended candidate read follows a rejected first window.
						assertReleased(ids[len(ids)-proberGrantFirstCount:])
						extendedChecked = true
					case 2: // Full locking fallback follows both rejected windows.
						assertReleased(ids[1:])
						fallbackChecked = true
					}
				}}
				escrow, _, err := createTransferEscrowInTx(ctx, query,
					clients.payerNetworkId, clients.payerId, clients.providerNetworkId,
					clients.providerId, clients.payerNetworkId, 1024, nil)
				if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != ids[0] {
					t.Fatalf("fallback=%t did not fund from the sole available grant: %+v, %v", fallback, escrow, err)
				}
				// A successful window must retain its grant lock through commit.
				_, err = checker.Exec(ctx, `SELECT balance_id FROM transfer_balance
					WHERE balance_id=$1 FOR UPDATE NOWAIT`, ids[0])
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
					t.Fatalf("selected grant lost its transaction lock: %v", err)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			if !extendedChecked || fallbackChecked != fallback {
				t.Fatalf("fallback=%t did not reach both lock-release boundaries: extended=%t full=%t", fallback, extendedChecked, fallbackChecked)
			}
		}
	})
}
