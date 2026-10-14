package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Tests added by the sn-reconcile mutation run: each one fails for a mutant
// of the census or the revive that the other tests let pass.

// A revive run beside the test, and how it ended.
type snReconcileTestRevival struct {
	revived   bool
	scheduled int
	panicked  any
}

func snReconcileTestReviveAsync(ctx context.Context, row *SnReconcileRow) <-chan snReconcileTestRevival {
	done := make(chan snReconcileTestRevival, 1)
	go func() {
		outcome := snReconcileTestRevival{}
		defer func() {
			outcome.panicked = recover()
			done <- outcome
		}()
		outcome.revived = ReviveSnReconcileRow(ctx, row, nil, server.NewId(), nil, func(server.PgTx) { outcome.scheduled++ })
	}()
	return done
}

// Polls a boolean query on its own connection until it holds, for at most a
// minute.
func snReconcileTestAwait(ctx context.Context, query string) bool {
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var holds bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, query).Scan(&holds))
		})
		if holds {
			return true
		}
	}
	return false
}

// RC-1, RC-2: a completed row is never stuck, whatever markers it kept.
func TestSnReconcileStuckRowsExcludeCompleted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, f.network)
		created := payoutTestCutoff.Add(-60 * 24 * time.Hour)
		for i, marker := range []struct {
			set   string
			value any
		}{
			{"canceled=true,circle_idempotency_key=$2", server.NewId()},
			{"canceled=true,payment_record=$2", "completed-record"},
			{"canceled=true,tx_hash=$2", "0xcompleted"},
			{"circle_idempotency_key=$2", server.NewId()},
		} {
			paymentId := snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(time.Duration(i)*time.Hour))
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET completed=true,complete_time=create_time,`+marker.set+` WHERE payment_id=$1`, paymentId, marker.value)
		}
		if rows := snReconcileTestStuck(t, ctx); len(rows) != 0 {
			t.Fatal("a completed row was found stuck", snReconcileTestIds(rows))
		}
	})
}

// RC-14: the schedule runs in the revive's own transaction, so a failed
// schedule leaves the row as the census found it, without an audit event.
func TestSnReconcileReviveScheduleFailureRollsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, row := snReconcileTestRevivable(t, ctx)
		before := snReconcileTestFingerprint(t, ctx, row.PaymentId)
		failure := errors.New("synthetic schedule failure")
		recovered := func() (value any) {
			defer func() {
				value = recover()
			}()
			ReviveSnReconcileRow(ctx, row, nil, server.NewId(), nil, func(server.PgTx) { panic(failure) })
			return nil
		}()
		if err, ok := recovered.(error); !ok || !errors.Is(err, failure) {
			t.Fatal("the schedule failure did not stop the revive", recovered)
		}
		if after := snReconcileTestFingerprint(t, ctx, row.PaymentId); after != before {
			t.Fatalf("a failed schedule left the revive committed: %s", after)
		}
	})
}

// RC-14: the revive holds the row lock from its snapshot comparison until it
// commits, so no writer can lock, journal or change the row in between.
func TestSnReconcileReviveHoldsRowLockUntilCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, row := snReconcileTestRevivable(t, ctx)
		// The revive's audit insert, between its comparison and its UPDATE,
		// waits for an advisory lock this test holds.
		snReconcileTestExec(t, ctx, `CREATE FUNCTION synthetic_pause_revive() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(8217); RETURN NEW; END $$;
			CREATE TRIGGER synthetic_pause_revive BEFORE INSERT ON audit_account_payment FOR EACH ROW
			WHEN (NEW.event_type='sn_reconcile_revived') EXECUTE FUNCTION synthetic_pause_revive()`)
		var done <-chan snReconcileTestRevival
		paused := false
		var writerErr error
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8217)`))
			done = snReconcileTestReviveAsync(ctx, row)
			paused = snReconcileTestAwait(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_database d ON d.oid=l.database
				WHERE d.datname=current_database() AND l.locktype='advisory' AND l.objid=8217 AND NOT l.granted)`)
			if paused {
				server.Db(ctx, func(conn server.PgConn) {
					_, writerErr = conn.Exec(ctx, `SELECT payment_id FROM account_payment WHERE payment_id=$1 FOR UPDATE NOWAIT`, row.PaymentId)
				})
			}
		})
		outcome := <-done
		var pgErr *pgconn.PgError
		if !paused || !errors.As(writerErr, &pgErr) || pgErr.Code != "55P03" {
			t.Fatal("a writer locked the row while its revive was in flight", paused, writerErr)
		}
		if !outcome.revived || outcome.scheduled != 1 || outcome.panicked != nil {
			t.Fatal("the paused revive did not complete", outcome)
		}
		snReconcileTestExec(t, ctx, `DROP TRIGGER synthetic_pause_revive ON audit_account_payment; DROP FUNCTION synthetic_pause_revive()`)
	})
}

// A1: a sender that locked the row and journaled a request under its key
// commits while the revive waits for that lock. The revive's re-check after
// FOR UPDATE must see the journal and leave the row untouched.
//
// This fails on v2/sn-reconcile ab9a187b: the revive runs in the default
// REPEATABLE READ transaction, whose snapshot is taken by its first statement,
// the SELECT ... FOR UPDATE that waits for the sender, so the re-check never
// sees a journal the lock holder commits. A READ COMMITTED revive passes.
func TestSnReconcileReviveSeesJournalOfLockHolder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, row := snReconcileTestRevivable(t, ctx)
		payment := func() (payment string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT row_to_json(p)::text FROM account_payment p WHERE payment_id=$1`, row.PaymentId).Scan(&payment))
			})
			return
		}
		before := payment()
		var done <-chan snReconcileTestRevival
		waited := false
		// As the sender's retainProviderPaymentRequestInTx does: lock the row,
		// then journal the request under its key.
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(ctx, `SELECT payment_id FROM account_payment WHERE payment_id=$1 FOR UPDATE`, row.PaymentId)
			server.WithPgResult(result, err, func() {
				for result.Next() {
				}
			})
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{}')`,
				*row.CircleIdempotencyKey, row.PaymentId))
			done = snReconcileTestReviveAsync(ctx, row)
			waited = snReconcileTestAwait(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid())`)
		}, server.TxReadCommitted)
		outcome := <-done
		if !waited || outcome.panicked != nil {
			t.Fatal("the revive did not wait for the sender's lock", waited, outcome.panicked)
		}
		if outcome.revived || outcome.scheduled != 0 || payment() != before {
			t.Fatalf("a row whose key its lock holder journaled was revived: revived=%v scheduled=%d\n%s", outcome.revived, outcome.scheduled, payment())
		}
	})
}
