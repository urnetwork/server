//go:build sidecar_pending

package model

// QUOTA-M2-06: FlushServiceDebits under -tags sidecar_pending through .stack/server-test.sh. A kill at
// each step reruns to the same end with nothing debited twice and every token released; a busy balance
// is skipped and revisited; a debit's day row lands on the day it applied, across 00:00 UTC; nothing
// pays anyone; a page stops at its deadline and hands on its cursor; FI-13(a) after each.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// A balance whose id falls in shard, with byteCount, in window around now.
func testingShardBalance(ctx context.Context, networkId server.Id, shard int, byteCount int64, now time.Time) server.Id {
	for {
		balanceId := server.NewId()
		if ServiceDebitShard(balanceId) != shard {
			continue
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count, pro)
					VALUES ($1, $2, $3, $4, $5, 0, $5, false)
				`,
				balanceId, networkId, now.Add(-time.Hour), now.Add(24*time.Hour), byteCount,
			))
		})
		return balanceId
	}
}

func testingBalanceByteCount(ctx context.Context, balanceId server.Id) (byteCount int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id = $1`, balanceId).Scan(&byteCount))
	})
	return
}

// The journal's rows of a balance: how many, and how many applied.
func testingServiceDebitRowCounts(ctx context.Context, balanceId server.Id) (rowCount int, appliedCount int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*), COUNT(*) FILTER (WHERE applied) FROM service_debit_journal WHERE balance_id = $1`,
			balanceId,
		).Scan(&rowCount, &appliedCount))
	})
	return
}

// The network's day rows by (day, meter).
func testingServiceUsage(ctx context.Context, networkId server.Id) (dayMeterQuantities map[string]int64) {
	dayMeterQuantities = map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT usage_day, meter, quantity FROM network_service_usage WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day time.Time
				var meter string
				var quantity int64
				server.Raise(result.Scan(&day, &meter, &quantity))
				dayMeterQuantities[day.Format("2006-01-02")+" "+meter] = quantity
			}
		})
	})
	return
}

// Reserves a token on a balance as a lease's carve or a relayed contract would hold it.
func testingReserveToken(ctx context.Context, balanceId server.Id, token string, byteCount int64) {
	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.Eval(ctx, redisContractReservationScript, redisContractReservationKeys(balanceId),
			"reserve", token, "1000000000", byteCount, redisContractReservationLease.Milliseconds()).Result()
		server.Raise(err)
	})
}

// Whether a balance's reservations hold token.
func testingTokenHeld(ctx context.Context, balanceId server.Id, token string) (held bool) {
	server.Redis(ctx, func(client server.RedisClient) {
		found, err := client.HExists(ctx, redisContractReservationKeys(balanceId)[1], token).Result()
		server.Raise(err)
		held = found
	})
	return
}

// One balance's flush at clock, with hooks, until it reports no failure.
func testingFlushBalance(t testing.TB, ctx context.Context, balanceId server.Id, clock sidecar.Clock, hooks *serviceDebitFlushHooks) (applied int, released int, err error) {
	applied, released, busy, err := flushServiceDebitBalance(ctx, balanceId, clock, hooks)
	if busy {
		t.Fatalf("the balance read busy")
	}
	return applied, released, err
}

// A kill before the commit, after it, or after the releases: the rerun debits once in all, releases
// every token, and deletes every row.
func TestServiceDebitFlushKillPoints(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		clock := sidecartest.NewFakeClock(now)
		killed := errors.New("killed")
		for _, kill := range []string{"before commit", "after commit", "after release"} {
			networkId := server.NewId()
			balanceId := testingShardBalance(ctx, networkId, 3, 10000, now)
			lease := testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterTls, 300)
			relay := testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterRelay, 200)
			testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterEvs, 100)
			leaseToken := "svc/" + lease.String()
			relayToken := relay.String()
			testingReserveToken(ctx, balanceId, leaseToken, 300)
			testingReserveToken(ctx, balanceId, relayToken, 500)

			hooks := &serviceDebitFlushHooks{}
			switch kill {
			case "before commit":
				hooks.beforeCommit = func(server.Id) error { return killed }
			case "after commit":
				hooks.afterCommit = func(server.Id) error { return killed }
			case "after release":
				hooks.afterRelease = func(server.Id) error { return killed }
			}
			_, _, err := testingFlushBalance(t, ctx, balanceId, clock, hooks)
			if !errors.Is(err, killed) {
				t.Fatalf("%s: %v", kill, err)
			}
			rowCount, appliedCount := testingServiceDebitRowCounts(ctx, balanceId)
			connect.AssertEqual(t, rowCount, 3)
			switch kill {
			case "before commit":
				connect.AssertEqual(t, appliedCount, 0)
				connect.AssertEqual(t, testingBalanceByteCount(ctx, balanceId), int64(10000))
				connect.AssertEqual(t, testingTokenHeld(ctx, balanceId, leaseToken), true)
			case "after commit":
				connect.AssertEqual(t, appliedCount, 3)
				connect.AssertEqual(t, testingBalanceByteCount(ctx, balanceId), int64(9400))
				connect.AssertEqual(t, testingTokenHeld(ctx, balanceId, leaseToken), true)
				connect.AssertEqual(t, testingTokenHeld(ctx, balanceId, relayToken), true)
			case "after release":
				connect.AssertEqual(t, appliedCount, 3)
				connect.AssertEqual(t, testingBalanceByteCount(ctx, balanceId), int64(9400))
				connect.AssertEqual(t, testingTokenHeld(ctx, balanceId, leaseToken), false)
				connect.AssertEqual(t, testingTokenHeld(ctx, balanceId, relayToken), false)
			}

			_, released, err := testingFlushBalance(t, ctx, balanceId, clock, nil)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, released, 3)
			rowCount, _ = testingServiceDebitRowCounts(ctx, balanceId)
			connect.AssertEqual(t, rowCount, 0)
			connect.AssertEqual(t, testingBalanceByteCount(ctx, balanceId), int64(9400))
			connect.AssertEqual(t, testingTokenHeld(ctx, balanceId, leaseToken), false)
			connect.AssertEqual(t, testingTokenHeld(ctx, balanceId, relayToken), false)
			usage := testingServiceUsage(ctx, networkId)
			connect.AssertEqual(t, usage, map[string]int64{"2026-04-01 tls": 300, "2026-04-01 relay": 200, "2026-04-01 evs": 100})
		}
	})
}

// A balance another transaction holds is skipped as busy, its rows kept, and the next pass applies
// them; the page goes on past it.
func TestServiceDebitFlushSkipsABusyBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		clock := sidecartest.NewFakeClock(now)
		networkId := server.NewId()
		busyId := testingShardBalance(ctx, networkId, 5, 1000, now)
		freeId := testingShardBalance(ctx, networkId, 5, 1000, now)
		testingJournalServiceDebit(ctx, busyId, sidecar.QuotaMeterEvs, 100)
		testingJournalServiceDebit(ctx, freeId, sidecar.QuotaMeterEvs, 100)

		held := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id = $1 FOR UPDATE`, busyId))
				close(held)
				<-release
			})
		}()
		<-held
		result, err := FlushServiceDebits(ctx, 5, nil, clock)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Balances, 2)
		connect.AssertEqual(t, result.Busy, 1)
		connect.AssertEqual(t, testingBalanceByteCount(ctx, busyId), int64(1000))
		connect.AssertEqual(t, testingBalanceByteCount(ctx, freeId), int64(900))
		close(release)
		<-done

		result, err = FlushServiceDebits(ctx, 5, nil, clock)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Busy, 0)
		connect.AssertEqual(t, testingBalanceByteCount(ctx, busyId), int64(900))
	})
}

// A debit counts on the UTC day it applied: rows applied at 23:59:59 land on that day, rows applied a
// second later on the next, whatever day their batch came in.
func TestServiceDebitDayRowAcrossMidnight(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clock := sidecartest.NewFakeClock(time.Date(2026, time.April, 1, 23, 59, 59, 0, time.UTC))
		networkId := server.NewId()
		balanceId := testingShardBalance(ctx, networkId, 7, 100000, clock.Now())
		testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterEvs, 100)
		testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterTls, 7)
		connect.AssertEqual(t, testingSettleServiceDebits(ctx, clock), nil)
		testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterEvs, 30)
		clock.Advance(time.Second)
		connect.AssertEqual(t, testingSettleServiceDebits(ctx, clock), nil)
		testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterEvs, 5)
		connect.AssertEqual(t, testingSettleServiceDebits(ctx, clock), nil)
		connect.AssertEqual(t, testingServiceUsage(ctx, networkId), map[string]int64{
			"2026-04-01 evs": 100,
			"2026-04-01 tls": 7,
			"2026-04-02 evs": 35,
		})
		connect.AssertEqual(t, testingBalanceByteCount(ctx, balanceId), int64(100000-142))
	})
}

// No row of any payout table moves: a service debit pays no one (D-QUOTA-4), and the journal's applied
// rows touch no escrow.
func TestServiceDebitFlushPaysNoOne(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		census := func() (counts []int64) {
			server.Db(ctx, func(conn server.PgConn) {
				for _, table := range []string{"transfer_escrow", "transfer_escrow_sweep", "account_balance", "account_payment", "transfer_debit_journal"} {
					var count int64
					server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count))
					counts = append(counts, count)
				}
			})
			return
		}
		networkId := server.NewId()
		balance := testingBalance(1000, now.Add(-time.Hour), now.Add(time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{balance})
		before := census()
		_, err := ApplyUsageBatch(ctx, testingEvsBatch(networkId, 400, 0), nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, testingSettleServiceDebits(ctx, sidecartest.NewFakeClock(now)), nil)
		connect.AssertEqual(t, census(), before)
		connect.AssertEqual(t, testingBalanceByteCount(ctx, server.Id(balance.BalanceId)), int64(600))
	})
}

// A page that reaches its deadline after a balance hands on its cursor with more to do and no error,
// and the next page starts after that balance.
func TestServiceDebitFlushPageDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		clock := sidecartest.NewFakeClock(now)
		settings := DefaultServiceDebitFlushSettings()
		settings.PageTimeout = 3 * time.Second
		defer Testing_SetServiceDebitFlushSettings(settings)()
		networkId := server.NewId()
		balanceIds := []server.Id{}
		for range 3 {
			balanceId := testingShardBalance(ctx, networkId, 9, 1000, now)
			testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterEvs, 10)
			balanceIds = append(balanceIds, balanceId)
		}
		waited := false
		result, err := flushServiceDebits(ctx, 9, nil, clock, &serviceDebitFlushHooks{
			afterBalance: func(pageCtx context.Context, balanceId server.Id) {
				if !waited {
					waited = true
					// the page's own deadline passes here
					<-pageCtx.Done()
				}
			},
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Balances, 1)
		connect.AssertEqual(t, result.More, true)
		connect.AssertEqual(t, result.LastBalanceId != nil, true)

		result, err = FlushServiceDebits(ctx, 9, result.LastBalanceId, clock)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Balances, 2)
		for _, balanceId := range balanceIds {
			connect.AssertEqual(t, testingBalanceByteCount(ctx, balanceId), int64(990))
		}
	})
}

// A balance gone while it holds rows is an invariant failure: the flush fails it and keeps the rows,
// never discarding the debt.
func TestServiceDebitFlushRefusesAMissingBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clock := sidecartest.NewFakeClock(time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC))
		var balanceId server.Id
		for {
			balanceId = server.NewId()
			if ServiceDebitShard(balanceId) == 11 {
				break
			}
		}
		testingJournalServiceDebit(ctx, balanceId, sidecar.QuotaMeterEvs, 10)
		result, err := FlushServiceDebits(ctx, 11, nil, clock)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Failed, 1)
		rowCount, appliedCount := testingServiceDebitRowCounts(ctx, balanceId)
		connect.AssertEqual(t, rowCount, 1)
		connect.AssertEqual(t, appliedCount, 0)
	})
}
