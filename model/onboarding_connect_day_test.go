package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The connect.day writer's per-process memory: one write per client per UTC
// day, reset when the day changes, retried after a failure.
func TestConnectDayCache(t *testing.T) {
	day1 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	a, b := server.NewId(), server.NewId()
	c := &connectDayCache{}

	connect.AssertEqual(t, true, c.remember(a, day1))
	connect.AssertEqual(t, false, c.remember(a, day1))
	connect.AssertEqual(t, true, c.remember(b, day1))
	// a failed write is retried on the next connect
	c.forget(a, day1)
	connect.AssertEqual(t, true, c.remember(a, day1))
	// a new UTC day starts over for every client
	connect.AssertEqual(t, true, c.remember(a, day2))
	connect.AssertEqual(t, true, c.remember(b, day2))
	connect.AssertEqual(t, false, c.remember(b, day2))
	// forgetting a stale day changes nothing
	c.forget(a, day1)
	connect.AssertEqual(t, false, c.remember(a, day2))
}

func TestConnectDayStart(t *testing.T) {
	loc := time.FixedZone("west", -7*60*60)
	at := time.Date(2026, 9, 9, 20, 30, 0, 0, loc) // 03:30 UTC on the 10th
	connect.AssertEqual(t, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), ConnectDayStart(at))
	connect.AssertEqual(t, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), ConnectDayStart(time.Date(2026, 9, 10, 23, 59, 59, 0, time.UTC)))
}

func TestRecordConnectDayPersistsOncePerNetworkUtcDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		secondClientId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true), ($3, $2, true)`,
				clientId,
				networkId,
				secondClientId,
			))
		})

		connectAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		RecordConnectDay(ctx, clientId, connectAt)
		RecordConnectDay(ctx, clientId, connectAt.Add(time.Hour))
		RecordConnectDay(ctx, secondClientId, connectAt.Add(2*time.Hour))
		awaitConnectDayEventCount(t, ctx, networkId, 1)

		RecordConnectDay(ctx, clientId, connectAt.Add(24*time.Hour))
		awaitConnectDayEventCount(t, ctx, networkId, 2)
	})
}

// The connection transaction is already durable before RecordConnectDay
// starts. A transport/session cancellation at that boundary must not discard
// the optional event, and a retry must still remain idempotent.
func TestRecordConnectDayPersistsOnceAfterCallerCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		queryCtx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		server.Tx(queryCtx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				queryCtx,
				`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
				clientId,
				networkId,
			))
		})

		callerCtx, cancelCaller := context.WithCancel(queryCtx)
		cancelCaller()
		connectAt := time.Date(2026, 9, 11, 12, 30, 0, 0, time.UTC)
		RecordConnectDay(callerCtx, clientId, connectAt)
		RecordConnectDay(callerCtx, clientId, connectAt.Add(time.Hour))
		awaitConnectDayEventCount(t, queryCtx, networkId, 1)
		if connectDaySeen.remember(clientId, ConnectDayStart(connectAt)) {
			t.Fatal("successful canceled-parent write was forgotten from the daily cache")
		}
	})
}

func awaitConnectDayEventCount(t testing.TB, ctx context.Context, networkId server.Id, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if count := connectDayEventCount(t, ctx, networkId); count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect.day events never reached %d", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func connectDayEventCount(t testing.TB, ctx context.Context, networkId server.Id) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT count(*) FROM network_onboarding_event WHERE network_id = $1 AND name = $2`,
			networkId,
			EventConnectDay,
		)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatal("connect-day event count returned no row")
			}
			server.Raise(result.Scan(&count))
		})
	})
	return count
}
