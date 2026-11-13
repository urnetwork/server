package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Redis callbacks and pooled connections are attributed to the goroutine's
// open transaction only; work before BEGIN, after commit and in post-commit
// projections leaves the transaction's server alone and is not reported.
func TestTxIoObservationAttributesOnlyOpenTransactionIo(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		observation, uninstall := Testing_ObserveTxIo()
		defer uninstall()

		redisPing := func() {
			Redis(ctx, func(client RedisClient) {
				Raise(client.Ping(ctx).Err())
			})
		}
		dbRead := func() {
			Db(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, "SELECT 1"))
			})
		}

		redisPing()
		dbRead()
		Tx(ctx, func(tx PgTx) {
			time.Sleep(20 * time.Millisecond)
			RaisePgResult(tx.Exec(ctx, "SELECT 1"))
			redisPing()
			dbRead()
			AddTxPostCommit(tx, "observed", func() any {
				redisPing()
				dbRead()
				return nil
			})
		}, TxReadCommitted)
		redisPing()
		dbRead()

		events := observation.Events()
		if len(events) != 2 || events[0].Kind != TxIoRedis || events[1].Kind != TxIoNestedDb {
			t.Fatalf("open transaction I/O = %+v; want one redis then one nested db", events)
		}
		for _, event := range events {
			if !strings.Contains(event.TxSite, "TestTxIoObservationAttributesOnlyOpenTransactionIo") ||
				!strings.Contains(event.Site, "TestTxIoObservationAttributesOnlyOpenTransactionIo") {
				t.Fatalf("event sites = %+v; want the test's frames", event)
			}
		}
		firstStatements := observation.FirstStatements()
		if len(firstStatements) != 1 || firstStatements[0].Delay < 20*time.Millisecond {
			t.Fatalf("first statements = %+v; want one delayed by the callback's 20ms", firstStatements)
		}
	})
}

// A nested transaction's I/O is attributed to the innermost open transaction,
// and the outer one owns the nested acquisition.
func TestTxIoObservationAttributesNestedTransactions(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		observation, uninstall := Testing_ObserveTxIo()
		defer uninstall()

		outer := func(inner func()) {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, "SELECT 1"))
				inner()
			})
		}
		outer(func() {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, "SELECT 2"))
				Redis(ctx, func(client RedisClient) {
					Raise(client.Ping(ctx).Err())
				})
			})
		})

		events := observation.Events()
		if len(events) != 2 || events[0].Kind != TxIoNestedDb || events[1].Kind != TxIoRedis {
			t.Fatalf("nested transaction I/O = %+v; want the nested acquisition then redis", events)
		}
		if events[0].TxSite == events[1].TxSite {
			t.Fatalf("nested redis was attributed to its outer transaction: %+v", events)
		}
		if len(observation.FirstStatements()) != 2 {
			t.Fatalf("first statements = %+v; want both transactions", observation.FirstStatements())
		}
	})
}

// Production never installs an observer, and one observer excludes another.
func TestTxIoObservationIsExclusiveAndUninstalls(t *testing.T) {
	if txIoObservation.Load() != nil {
		t.Fatal("an observer is installed by default")
	}
	_, uninstall := Testing_ObserveTxIo()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a second observer was installed")
			}
		}()
		Testing_ObserveTxIo()
	}()
	uninstall()
	if txIoObservation.Load() != nil {
		t.Fatal("uninstall left the observer")
	}
	uninstall()
}
