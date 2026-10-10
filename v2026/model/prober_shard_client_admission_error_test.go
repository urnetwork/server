package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Client admission must preserve a canceled registry query exactly as escrow
// admission does. Returning a database error from the void transaction callback
// falsely reaches COMMIT and can replace cancellation with cache cleanup errors.
func TestProberShardClientCanceledAdmissionQueryRollsBack(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
		defer stop()
		network := server.NewId()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TABLE prober_shard_run (
				network_id uuid PRIMARY KEY, state text NOT NULL, deadline timestamp NOT NULL)`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO prober_shard_run VALUES ($1,'active',$2)`, network, server.NowUtc().Add(time.Hour)))
		}, server.OptReadWrite())
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM prober_shard_run WHERE network_id=$1 FOR UPDATE`, network))
		heldPID := contractLifecycleTestBackendPid(t, ctx, held)
		admissionCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		type outcome struct {
			panicValue  any
			returnedErr error
			calls       int
		}
		finished := make(chan outcome, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			got := outcome{}
			got.panicValue = captureShardQueryPanic(func() {
				got.calls++
				_, _, _, _, got.returnedErr = ConnectNetworkClientWithIpFamily(
					admissionCtx, server.NewId(), "192.0.2.1:1234", server.NewId(), 4, network)
			})
			finished <- got
		}()
		defer func() { cancel(); _ = held.Rollback(context.Background()); <-joined }()
		requireContractLifecycleBlockedBy(t, ctx, held, heldPID)
		cancel()
		select {
		case got := <-finished:
			if !server.IsDoneError(got.panicValue) || got.returnedErr != nil || got.calls != 1 {
				t.Fatalf("canceled admission panic=%v returned=%v calls=%d; want original cancellation and one model call",
					got.panicValue, got.returnedErr, got.calls)
			}
		case <-ctx.Done():
			t.Fatal("canceled registry query did not finish")
		}
	})
}

// An operational SQL failure must retain its original SQLSTATE through the
// public model call rather than return from its Tx callback and fail COMMIT.
func TestProberShardClientAdmissionPreservesSQLFailure(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		recovered := captureShardQueryPanic(func() {
			_, _, _, _, _ = ConnectNetworkClientWithIpFamily(t.Context(),
				server.NewId(), "192.0.2.1:1234", server.NewId(), 4, server.NewId())
		})
		err, ok := recovered.(error)
		var pgErr *pgconn.PgError
		if !ok || !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
			t.Fatalf("admission failure=%v; want original undefined-table SQLSTATE 42P01", recovered)
		}
	})
}

// A client-side decode error does not abort PostgreSQL. Raising it is necessary
// to roll back writes already made by an enclosing admission transaction.
func TestProberShardClientAdmissionScanFailureRollsBackEarlierWrite(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := server.NewId()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TABLE prober_shard_run (
				network_id uuid PRIMARY KEY, state text, deadline timestamp NOT NULL);
				CREATE TABLE client_admission_effect (value integer NOT NULL)`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO prober_shard_run VALUES ($1,NULL,$2)`, network, server.NowUtc().Add(time.Hour)))
		}, server.OptReadWrite())
		recovered := captureShardQueryPanic(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_admission_effect VALUES (1)`))
				_ = lockProberShardClientAdmissionInTx(ctx, tx, network)
			}, server.OptNoRetry())
		})
		if recovered == nil || server.IsDoneError(recovered) {
			t.Fatalf("scan error was lost or classified as cancellation: %v", recovered)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM client_admission_effect`).Scan(&count))
			if count != 0 {
				t.Fatalf("scan failure committed %d earlier writes", count)
			}
		})
	})
}

// Ordinary networks and live shards remain admissible. A policy refusal stays
// a normal return, distinct from operational failures that must unwind Tx.
func TestProberShardClientAdmissionHealthyAndRefused(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TABLE prober_shard_run (
				network_id uuid PRIMARY KEY, state text NOT NULL, deadline timestamp NOT NULL)`))
		}, server.OptReadWrite())
		for _, tc := range []struct {
			name    string
			state   string
			age     time.Duration
			refused bool
		}{
			{name: "ordinary network"},
			{name: "live shard", state: "active", age: time.Hour},
			{name: "draining shard", state: "draining", age: time.Hour, refused: true},
			{name: "expired shard", state: "active", age: -time.Hour, refused: true},
		} {
			network := server.NewId()
			if tc.state != "" {
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, `INSERT INTO prober_shard_run VALUES ($1,$2,$3)`, network, tc.state, server.NowUtc().Add(tc.age)))
				}, server.OptReadWrite())
			}
			var admissionErr error
			var continued bool
			server.Tx(ctx, func(tx server.PgTx) {
				admissionErr = lockProberShardClientAdmissionInTx(ctx, tx, network)
				continued = true
			}, server.OptNoRetry())
			if !continued || errors.Is(admissionErr, ErrProberShardRetired) != tc.refused || (!tc.refused && admissionErr != nil) {
				t.Fatalf("%s: continued=%t admissionErr=%v refused=%t", tc.name, continued, admissionErr, tc.refused)
			}
		}
	})
}
