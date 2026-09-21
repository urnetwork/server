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
		writes := []<-chan struct{}{
			recordConnectDayForTest(ctx, clientId, connectAt),
			recordConnectDayForTest(ctx, clientId, connectAt.Add(time.Hour)),
			recordConnectDayForTest(ctx, secondClientId, connectAt.Add(2*time.Hour)),
		}
		defer func() { waitConnectDayWrites(writes...) }()
		waitConnectDayWrites(writes...)
		connect.AssertEqual(t, 1, connectDayEventCount(t, ctx, networkId))

		writes = append(writes, recordConnectDayForTest(ctx, clientId, connectAt.Add(24*time.Hour)))
		waitConnectDayWrites(writes...)
		connect.AssertEqual(t, 2, connectDayEventCount(t, ctx, networkId))
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
		writes := []<-chan struct{}{
			recordConnectDayForTest(callerCtx, clientId, connectAt),
			recordConnectDayForTest(callerCtx, clientId, connectAt.Add(time.Hour)),
		}
		defer waitConnectDayWrites(writes...)
		waitConnectDayWrites(writes...)
		connect.AssertEqual(t, 1, connectDayEventCount(t, queryCtx, networkId))
		if connectDaySeen.remember(clientId, ConnectDayStart(connectAt)) {
			t.Fatal("successful canceled-parent write was forgotten from the daily cache")
		}
	})
}

// A failed optional write still completes its job and forgets the cache entry,
// allowing the same client's next connection to persist the missing day.
func TestRecordConnectDayFailedWriteCompletesAndCanRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
				clientId, networkId,
			))
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE FUNCTION synthetic_reject_connect_day() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION 'synthetic connect.day rejection';
				END;
				$$;
				CREATE TRIGGER synthetic_reject_connect_day
				BEFORE INSERT ON network_onboarding_event
				FOR EACH ROW EXECUTE FUNCTION synthetic_reject_connect_day();
			`))
		})

		connectAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		waitConnectDayWrites(recordConnectDayForTest(ctx, clientId, connectAt))
		connect.AssertEqual(t, 0, connectDayEventCount(t, ctx, networkId))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`DROP TRIGGER synthetic_reject_connect_day ON network_onboarding_event`,
			))
		})
		waitConnectDayWrites(recordConnectDayForTest(ctx, clientId, connectAt))
		connect.AssertEqual(t, 1, connectDayEventCount(t, ctx, networkId))
	})
}

// Uses the real nonblocking admission path with a per-call completion signal.
func recordConnectDayForTest(ctx context.Context, clientId server.Id, connectTime time.Time) <-chan struct{} {
	done := make(chan struct{})
	recordConnectDay(ctx, clientId, connectTime, done)
	return done
}

// Join every write, including deduplicated admissions and serialization retries,
// before asserting the final event count or tearing down the private database.
func waitConnectDayWrites(writes ...<-chan struct{}) {
	for _, done := range writes {
		<-done
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
