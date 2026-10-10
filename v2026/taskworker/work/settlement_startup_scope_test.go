// A family publisher must own every queue it writes while remaining independent
// of live owners in the other family. Combined startup still owns the full set.
package work

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A nested real transaction runs while the outer physical backend still holds
// the conflicting family key; no timing window establishes this ordering.
func requireIndependentSettlementStartup(t *testing.T, heldName, publishedPrefix string, schedule func(*session.ClientSession, server.PgTx)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		var admissions []server.PgOwnershipEvent
		ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted {
				admissions = append(admissions, event)
			}
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		heldKey := task.RunOnceOwnershipKey(task.RunOnce(heldName))
		server.OwnedTx(ctx, []server.PgOwnershipKey{heldKey}, func(heldTx server.PgTx) {
			server.Tx(ctx, func(tx server.PgTx) {
				schedule(owner, tx)
				schedule(owner, tx)
				if server.TxOwnsKeys(tx, []server.PgOwnershipKey{heldKey}) {
					t.Fatal("family publisher acquired the unrelated held key")
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			if !server.TxOwnsKeys(heldTx, []server.PgOwnershipKey{heldKey}) {
				t.Fatal("outer owner ended before independent publication")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if len(admissions) != 2 || len(admissions[0].Keys) != 1 || len(admissions[1].Keys) != 16 ||
			admissions[0].BackendPid == admissions[1].BackendPid || !admissions[1].TransactionScoped || reruns.Load() != 0 {
			t.Fatal("independent publication did not use one complete no-retry family owner", admissions, reruns.Load())
		}
		for shard := range 16 {
			key := task.RunOnceOwnershipKey(task.RunOnce(fmt.Sprintf("%s%d", publishedPrefix, shard)))
			if !slices.Contains(admissions[1].Keys, key) {
				t.Fatal("publisher omitted a written shard key", shard)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var total, family int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE run_once_key LIKE $1) FROM pending_task`, `["`+publishedPrefix+`%`).Scan(&total, &family))
			if total != 16 || family != 16 {
				t.Fatal("independent publication changed its exact durable family", total, family)
			}
		})
	})
}

func TestLegacyStartupPublishesWhileDebitQueueOwnerHeld(t *testing.T) {
	requireIndependentSettlementStartup(t, "flush_transfer_debits_0", "flush_legacy_settlements_", ScheduleFlushLegacySettlements)
}

func TestDebitStartupPublishesWhileLegacyQueueOwnerHeld(t *testing.T) {
	requireIndependentSettlementStartup(t, "flush_legacy_settlements_0", "flush_transfer_debits_", ScheduleFlushTransferDebits)
}

// Removing unrelated keys never permits publication around a held key that the
// family actually writes. Refusal precedes all pending rows in both directions.
func TestSettlementFamilyStartupRefusesHeldWrittenQueue(t *testing.T) {
	for _, test := range []struct {
		name     string
		schedule func(*session.ClientSession, server.PgTx)
	}{
		{name: "flush_transfer_debits_0", schedule: ScheduleFlushTransferDebits},
		{name: "flush_legacy_settlements_0", schedule: ScheduleFlushLegacySettlements},
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

// The production combined initializer admits its union before calling either
// publisher; their subset validation must not create a second acquisition.
func TestSettlementAccountingStartupPredeclaresBothFamilies(t *testing.T) {
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
		if len(admissions) != 1 || len(admissions[0].Keys) != 32 || admissions[0].TransactionScoped {
			t.Fatal("combined initializer did not predeclare one complete owner", admissions)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var total, debit, legacy int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),
count(*) FILTER(WHERE run_once_key LIKE '["flush_transfer_debits_%'),
count(*) FILTER(WHERE run_once_key LIKE '["flush_legacy_settlements_%') FROM pending_task`).Scan(&total, &debit, &legacy))
			if total != 32 || debit != 16 || legacy != 16 {
				t.Fatal("combined initializer omitted a durable family", total, debit, legacy)
			}
		})
	})
}
