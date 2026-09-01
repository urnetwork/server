package model

// The redis net-escrow counter mirrors a postgres-durable reservation. The
// reservation is committed in the escrow tx; the mirror is updated afterwards
// in a post. If the mirror update is bound to the caller's request context, a
// client that disconnects in that window desyncs the counter permanently:
// downward on a lost create (over-reporting available balance, seen as a
// negative residue), upward on a lost settle (hiding balance, the
// "insufficient balance" lockup).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A large scalar-array predicate is not a durable access-path boundary: at
// production cardinality PostgreSQL planned every 10,000-balance page as a
// parallel scan of the complete transfer_escrow table. Pin the relational
// shape that makes the existing balance index lookup structural.
func TestNetEscrowReservationPageForcesPerBalanceIndexBoundary(t *testing.T) {
	for _, want := range []string{
		"FROM unnest($1::uuid[])",
		"CROSS JOIN LATERAL",
		"transfer_escrow.balance_id = requested_balance.balance_id",
		"transfer_escrow.settled = false",
		"OFFSET 0",
		"transfer_contract.outcome IS NULL",
	} {
		if !strings.Contains(netEscrowReservationPageSQL, want) {
			t.Fatalf("net-escrow reservation page lost planner boundary %q:\n%s", want, netEscrowReservationPageSQL)
		}
	}
	if strings.Contains(netEscrowReservationPageSQL, "balance_id = ANY") {
		t.Fatalf("net-escrow reservation page restored the full-scan-prone ANY shape:\n%s", netEscrowReservationPageSQL)
	}
	if strings.Index(netEscrowReservationPageSQL, "transfer_escrow.settled = false") >
		strings.Index(netEscrowReservationPageSQL, "OFFSET 0") {
		t.Fatalf("unsettled prefilter escaped the lateral optimization boundary:\n%s", netEscrowReservationPageSQL)
	}
}

// The expiry-boundary repair must stay proportional to authoritative open
// escrow. Scanning all historical balances would turn one five-minute repair
// pass into a production-cardinality table walk.
func TestNetEscrowNoncurrentOpenBalancePageStaysIndexBounded(t *testing.T) {
	for _, want := range []string{
		"transfer_escrow.settled = false",
		"transfer_contract.outcome IS NULL",
		"NOT (",
		"transfer_balance.start_time <= $1 AND $1 < transfer_balance.end_time",
		"transfer_escrow.balance_id > $2",
		"GROUP BY transfer_escrow.balance_id",
		"ORDER BY transfer_escrow.balance_id",
		"LIMIT $3",
	} {
		if !strings.Contains(netEscrowNoncurrentOpenBalancePageSQL, want) {
			t.Fatalf("non-current open-escrow page lost bounded predicate %q:\n%s", want, netEscrowNoncurrentOpenBalancePageSQL)
		}
	}
	if strings.Contains(netEscrowNoncurrentOpenBalancePageSQL, "end_time +") {
		t.Fatalf("non-current open-escrow page restored a fixed grace window:\n%s", netEscrowNoncurrentOpenBalancePageSQL)
	}
}

// TestNetEscrowMirrorSurvivesCallerCancel requires the mirror to match the
// committed reservation once the create call returns, even though the caller's
// context is cancelled the moment it does.
//
// Today the mirror update runs synchronously inside the create (RunPosts before
// return), so this holds trivially. It is a guard, not a regression test: making
// the mirror update asynchronous or binding it to the caller's context would
// reopen the window where postgres holds a reservation the counter never
// recorded, and the eventual settle then decrements past zero (a negative
// residue, which over-reports available balance).
func TestNetEscrowMirrorSurvivesCallerCancel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024)
		contractByteCount := ByteCount(4 * 1024 * 1024)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			UsdToNanoCents(10.00),
			"net-escrow-cancel",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		_, err = RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, ctx)
		connect.AssertEqual(t, err, nil)

		// the caller goes away as soon as the request returns: the contract is
		// committed, the mirror update is still outstanding
		cancelCtx, cancel := context.WithCancel(ctx)
		transferEscrow, err := CreateTransferEscrow(
			cancelCtx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractByteCount,
		)
		cancel()
		connect.AssertEqual(t, err, nil)

		// the reservation is durable in postgres, so the mirror must reach the
		// same total. poll: the mirror update is asynchronous by design.
		var netEscrow ByteCount
		deadline := time.Now().Add(10 * time.Second)
		for {
			netEscrow = ByteCount(0)
			for _, balance := range transferEscrow.Balances {
				netEscrow += Testing_NetEscrowByteCount(ctx, balance.BalanceId)
			}
			if netEscrow == contractByteCount {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf(
					"net escrow mirror = %d, want %d: the mirror update was lost with the caller's context, so the settle will decrement a reservation the counter never recorded (negative residue)",
					netEscrow,
					contractByteCount,
				)
			}
			select {
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
}

// TestNetEscrowReconcileRepairsExpiredBalanceWithOpenEscrow reproduces the
// 2026-09-01 UTC activation boundary. A balance stopped being current while
// contracts created in its final interval remained open for the close worker's
// grace period. The old current-window-only reconcile skipped the balance, so
// a lost reservation mirror could not be repaired before settlement released
// it and drove the counter negative.
func TestNetEscrowReconcileRepairsExpiredBalanceWithOpenEscrow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		sourceNetworkId := server.NewId()
		sourceUserId := server.NewId()
		sourceClientId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationUserId := server.NewId()
		destinationClientId := server.NewId()
		Testing_CreateNetwork(ctx, sourceNetworkId, "expiry-source", sourceUserId)
		Testing_CreateNetwork(ctx, destinationNetworkId, "expiry-destination", destinationUserId)

		const balanceByteCount = ByteCount(1024 * 1024 * 1024)
		const contractByteCount = ByteCount(32 * 1024 * 1024)
		err := AddBasicTransferBalance(
			ctx,
			sourceNetworkId,
			balanceByteCount,
			now.Add(-2*time.Hour),
			now.Add(time.Hour),
		)
		connect.AssertEqual(t, err, nil)

		transferEscrow, err := CreateTransferEscrow(
			ctx,
			sourceNetworkId,
			sourceClientId,
			destinationNetworkId,
			destinationClientId,
			contractByteCount,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(transferEscrow.Balances), 1)
		balanceId := transferEscrow.Balances[0].BalanceId

		// Cross the balance boundary without closing the contract, then reproduce
		// the lost create mirror exposed by the production close cohort.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE transfer_balance
				SET end_time = $2
				WHERE balance_id = $1
			`, balanceId, now.Add(-time.Minute)))
		}, server.TxReadCommitted)
		Testing_DeleteNetEscrow(ctx, balanceId)
		connect.AssertEqual(t, len(GetActiveTransferBalances(ctx, sourceNetworkId)), 0)

		driftByNetworkId, reconciledBalanceCount := ReconcileNetEscrow(ctx, true)
		connect.AssertEqual(t, reconciledBalanceCount, 1)
		connect.AssertEqual(t, driftByNetworkId[sourceNetworkId], -contractByteCount)
		connect.AssertEqual(t, Testing_NetEscrowByteCount(ctx, balanceId), contractByteCount)

		// The operator-targeted form must cover the same lifecycle set; otherwise
		// a directed repair would misleadingly report zero balances at the exact
		// boundary where the fleet pass now succeeds.
		Testing_DeleteNetEscrow(ctx, balanceId)
		targetedDrift, targetedBalanceCount := ReconcileNetEscrowForNetwork(ctx, sourceNetworkId, true)
		connect.AssertEqual(t, targetedBalanceCount, 1)
		connect.AssertEqual(t, targetedDrift, -contractByteCount)
		connect.AssertEqual(t, Testing_NetEscrowByteCount(ctx, balanceId), contractByteCount)
	})
}

// Reconciliation observes Redis, computes the PostgreSQL correction, and then
// applies it. A live contract mirror can change the counter between those two
// Redis operations. SET used to erase that concurrent change; the additive
// Lua correction must preserve it deterministically.
func TestNetEscrowCorrectionPreservesConcurrentMirrorWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := netEscrowKey(server.NewId())
		defer server.Redis(ctx, func(r server.RedisClient) { r.Del(ctx, key) })

		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, 100, time.Hour).Err())
			observed, err := r.Get(ctx, key).Int64()
			server.Raise(err)

			// PostgreSQL says 80, so reconciliation computed -20 from the
			// observed 100. A contract reserves another 10 before correction.
			server.Raise(r.IncrBy(ctx, key, 10).Err())
			corrected, err := applyNetEscrowCorrection(ctx, r, key, 80-observed).Int64()
			server.Raise(err)
			if corrected != 90 {
				t.Fatalf("corrected counter = %d, want 90 (fresh 80 + concurrent 10)", corrected)
			}
			if ttl := r.TTL(ctx, key).Val(); ttl <= 0 {
				t.Fatalf("corrected counter has no fallback ttl: %s", ttl)
			}
		})
	})
}

// A PostgreSQL statement takes its snapshot before executing the reservation
// page. If a live settlement commits and updates Redis while that page is
// still running, reconcileNetEscrowBatch receives the old PostgreSQL total but
// its later Redis GET sees the new mirror. INCRBY preserves writes after GET;
// it cannot identify this already-visible write as newer than the page
// snapshot. Pin the matched inverse observed in production: the stale pass
// re-adds the released bytes and the next fresh pass removes the same bytes.
// The unsettled partial access path bounds this exposure by making the page
// fast; durable cross-store fencing/versioning is required to eliminate it.
func TestNetEscrowSlowReservationPageProducesMatchedReversal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		balanceId := server.NewId()
		key := netEscrowKey(balanceId)
		defer server.Redis(ctx, func(r server.RedisClient) { r.Del(ctx, key) })

		// The statement snapshot still contains 20 reserved bytes. While its
		// slow page runs, a settlement commits and its mirror post leaves 10.
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, 10, time.Hour).Err())
		})
		staleDrift := reconcileNetEscrowBatch(
			ctx,
			map[server.Id]ByteCount{balanceId: 20},
			[]server.Id{balanceId},
			true,
		)
		if staleDrift[balanceId] != -10 {
			t.Fatalf("stale-page drift = %d, want -10 under-reserved", staleDrift[balanceId])
		}
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.Get(ctx, key).Int64()
			server.Raise(err)
			if value != 20 {
				t.Fatalf("stale-page correction = %d, want old snapshot value 20", value)
			}
		})

		// The next statement sees the committed settlement and reverses the
		// exact quantity that the stale page reintroduced.
		freshDrift := reconcileNetEscrowBatch(
			ctx,
			map[server.Id]ByteCount{balanceId: 10},
			[]server.Id{balanceId},
			true,
		)
		if freshDrift[balanceId] != 10 {
			t.Fatalf("fresh-page drift = %d, want 10 over-reserved", freshDrift[balanceId])
		}
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.Get(ctx, key).Int64()
			server.Raise(err)
			if value != 10 {
				t.Fatalf("fresh-page correction = %d, want current value 10", value)
			}
		})
	})
}

// A fleet-wide reconcile normally finds most mirrors already equal to the
// PostgreSQL source. The pre-fix implementation nevertheless SET or DELeted
// every one of them, turning a 900k-balance no-op pass into 900k writes and
// refreshing every matching key's fallback TTL. Preserve a deliberately short
// TTL to prove an in-band balance is observation-only.
func TestNetEscrowReconcileSkipsInBandMirrorWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		balanceId := server.NewId()
		key := netEscrowKey(balanceId)
		defer server.Redis(ctx, func(r server.RedisClient) { r.Del(ctx, key) })

		const reserved ByteCount = 80
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, reserved, 30*time.Minute).Err())
		})

		drift := reconcileNetEscrowBatch(
			ctx,
			map[server.Id]ByteCount{balanceId: reserved},
			[]server.Id{balanceId},
			true,
		)
		if drift[balanceId] != 0 {
			t.Fatalf("in-band mirror drift = %d, want 0", drift[balanceId])
		}

		server.Redis(ctx, func(r server.RedisClient) {
			ttl := r.TTL(ctx, key).Val()
			if ttl <= 0 || time.Hour <= ttl {
				t.Fatalf("in-band reconcile rewrote the mirror TTL: got %s, want the original 30m TTL", ttl)
			}
			value, err := r.Get(ctx, key).Int64()
			server.Raise(err)
			if value != int64(reserved) {
				t.Fatalf("in-band mirror value = %d, want %d", value, reserved)
			}
		})
	})
}

// PostgreSQL commits a settlement before its Redis mirror release. A
// reconcile can legitimately observe that durable settlement while the mirror
// post is still in flight, so the delayed release may see zero and produce a
// negative diagnostic result. The release must retain that evidence but
// atomically remove the negative key; otherwise available balance remains
// overstated until another scheduled reconcile.
func TestNetEscrowReleaseClampsNegativeAtomically(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := netEscrowKey(server.NewId())
		defer server.Redis(ctx, func(r server.RedisClient) { r.Del(ctx, key) })

		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, 100, 30*time.Minute).Err())

			remaining, err := applyNetEscrowRelease(ctx, r, key, 40).Int64()
			server.Raise(err)
			if remaining != 60 {
				t.Fatalf("positive release result = %d, want 60", remaining)
			}
			if ttl := r.TTL(ctx, key).Val(); ttl <= 0 || time.Hour <= ttl {
				t.Fatalf("positive release replaced the shorter precise ttl: %s", ttl)
			}

			negative, err := applyNetEscrowRelease(ctx, r, key, 100).Int64()
			server.Raise(err)
			if negative != -40 {
				t.Fatalf("diagnostic release result = %d, want -40", negative)
			}
			if err := r.Get(ctx, key).Err(); err != redis.Nil {
				t.Fatalf("negative mirror survived atomic clamp: %v", err)
			}
		})
	})
}
