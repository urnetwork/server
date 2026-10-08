package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

func captureShardQueryPanic(callback func()) (recovered any) {
	defer func() { recovered = recover() }()
	callback()
	return
}

// A canceled registry lock read must unwind the transaction with its original
// cancellation. Returning it as a policy refusal instead lets Tx reach COMMIT,
// whose statement-cache cleanup masks the owner with "conn closed".
func TestProberShardCanceledAdmissionQueryRollsBack(t *testing.T) {
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
			callbacks   int
			escrow      *TransferEscrow
			posts       int
		}
		finished := make(chan outcome, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			got := outcome{}
			got.panicValue = captureShardQueryPanic(func() {
				server.Tx(admissionCtx, func(tx server.PgTx) {
					got.callbacks++
					var posts []func() any
					got.escrow, posts, got.returnedErr = createTransferEscrowInTx(admissionCtx, tx,
						network, server.NewId(), server.NewId(), server.NewId(), network, 1024, nil)
					got.posts = len(posts)
				}, server.OptNoRetry())
			})
			finished <- got
		}()
		defer func() { cancel(); _ = held.Rollback(context.Background()); <-joined }()
		requireContractLifecycleBlockedBy(t, ctx, held, heldPID)
		cancel()
		select {
		case got := <-finished:
			if !server.IsDoneError(got.panicValue) || got.returnedErr != nil || got.callbacks != 1 || got.escrow != nil || got.posts != 0 {
				t.Fatalf("canceled admission panic=%v returned=%v callbacks=%d escrow=%v posts=%d; want cancellation panic, one callback, no effects",
					got.panicValue, got.returnedErr, got.callbacks, got.escrow, got.posts)
			}
		case <-ctx.Done():
			t.Fatal("canceled registry query did not finish")
		}
	})
}

// A decode failure does not abort PostgreSQL itself. It must still unwind the
// transaction, so earlier writes cannot commit behind a validation failure.
func TestProberShardAdmissionScanFailureRollsBackEarlierWrite(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := server.NewId()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TABLE prober_shard_run (
				network_id uuid PRIMARY KEY, state text NOT NULL, deadline text NOT NULL);
				CREATE TABLE shard_validation_effect (value integer NOT NULL)`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO prober_shard_run VALUES ($1,'active','malformed timestamp')`, network))
		}, server.OptReadWrite())
		recovered := captureShardQueryPanic(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO shard_validation_effect VALUES (1)`))
				_, _ = validateProberShardPayerInTx(ctx, tx, network, network, network)
			}, server.OptNoRetry())
		})
		if recovered == nil || server.IsDoneError(recovered) {
			t.Fatalf("scan error was lost or classified as cancellation: %v", recovered)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM shard_validation_effect`).Scan(&count))
			if count != 0 {
				t.Fatalf("scan failure committed %d earlier writes", count)
			}
		})
	})
}

// The final database-clock check has the same transaction boundary as the
// registry read. Cancellation there must not return an ordinary refusal and
// let the enclosing transaction commit work that preceded the check.
func TestProberShardDeadlineQueryCancellationRollsBackEarlierWrite(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TABLE shard_deadline_effect (value integer NOT NULL)`))
		}, server.OptReadWrite())
		recovered := captureShardQueryPanic(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO shard_deadline_effect VALUES (1)`))
				queryCtx, cancel := context.WithCancel(ctx)
				cancel()
				deadline := server.NowUtc().Add(time.Hour)
				_ = validateProberShardAdmissionDeadlineInTx(queryCtx, tx, &deadline)
			}, server.OptNoRetry())
		})
		if !server.IsDoneError(recovered) {
			t.Fatalf("deadline query cancellation was lost: %v", recovered)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM shard_deadline_effect`).Scan(&count))
			if count != 0 {
				t.Fatalf("canceled deadline query committed %d earlier writes", count)
			}
		})
	})
}

// Operational SQL errors must retain their SQLSTATE and roll back rather than
// becoming a validation return followed by a failed COMMIT.
func TestProberShardAdmissionQueryPreservesSQLFailure(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		callbacks := 0
		recovered := captureShardQueryPanic(func() {
			server.Tx(t.Context(), func(tx server.PgTx) {
				callbacks++
				_, _ = validateProberShardPayerInTx(t.Context(), tx, server.NewId(), server.NewId(), server.NewId())
			}, server.OptNoRetry())
		})
		err, ok := recovered.(error)
		var pgErr *pgconn.PgError
		if !ok || !errors.As(err, &pgErr) || pgErr.Code != "42P01" || callbacks != 1 {
			t.Fatalf("validation failure=%v callbacks=%d; want original undefined-table error and one callback", recovered, callbacks)
		}
	})
}
