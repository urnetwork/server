// The debit publisher must own every queue it writes. Startup owns the full
// set; the retired legacy settlement family is no longer published.
package work

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Removing unrelated keys never permits publication around a held key that the
// family actually writes. Refusal precedes all pending rows.
func TestSettlementFamilyStartupRefusesHeldWrittenQueue(t *testing.T) {
	for _, test := range []struct {
		name     string
		schedule func(*session.ClientSession, server.PgTx)
	}{
		{name: "flush_transfer_debits_0", schedule: ScheduleFlushTransferDebits},
	} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			var refused, reruns atomic.Int32
			ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
			ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
				if event.Kind == server.PgOwnershipRefused {
					refused.Add(1)
				}
			})
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			var publishErr error
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce(test.name))}, func(server.PgTx) {
				server.HandleError(func() {
					server.Tx(ctx, func(tx server.PgTx) { test.schedule(owner, tx) }, server.TxReadCommitted, server.OptNoRetry())
				}, func(err error) { publishErr = err })
			}, server.TxReadCommitted, server.OptNoRetry())
			if publishErr == nil || refused.Load() != 1 || reruns.Load() != 0 {
				t.Fatal("held written queue did not refuse before publication", test.name, publishErr, refused.Load(), reruns.Load())
			}
			server.Db(ctx, func(conn server.PgConn) {
				var count int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task`).Scan(&count))
				if count != 0 {
					t.Fatal("refused family left partial queue rows", test.name, count)
				}
			})
		})
	}
}

// The production initializer admits the debit family before calling its
// publisher; the publisher's validation must not create a second acquisition.
// No legacy settlement shard is published.
func TestSettlementAccountingStartupPredeclaresDebitFamily(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		var admissions []server.PgOwnershipEvent
		ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted {
				admissions = append(admissions, event)
			}
		})
		ScheduleSettlementAccountingTasks(ctx)
		if len(admissions) != 1 || len(admissions[0].Keys) != 16 || admissions[0].TransactionScoped {
			t.Fatal("combined initializer did not predeclare one complete owner", admissions)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var total, debit, legacy int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),
count(*) FILTER(WHERE run_once_key LIKE '["flush_transfer_debits_%'),
count(*) FILTER(WHERE run_once_key LIKE '["flush_legacy_settlements_%') FROM pending_task`).Scan(&total, &debit, &legacy))
			if total != 16 || debit != 16 || legacy != 0 {
				t.Fatal("initializer omitted the debit family or published a retired one", total, debit, legacy)
			}
		})
	})
}
