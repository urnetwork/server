package model

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func providerTotalsTestEnv(t *testing.T, run func(testing.TB, context.Context)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		run(t, ctx)
	})
}

func providerTotalsTestTask(ctx context.Context, contractId, networkId server.Id) (id server.Id) {
	return providerTotalsTestPublish(ctx, contractId, map[server.Id]*contractPayout{
		networkId: {payoutByteCount: 17, payout: 29},
	})
}

// Fixtures enter through the same complete queue owner as financial publishers.
func providerTotalsTestPublish(ctx context.Context, contractId server.Id, payouts map[server.Id]*contractPayout) (id server.Id) {
	server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))}, func(tx server.PgTx) {
		id = queueLegacyProviderTotalsInTx(ctx, tx, contractId, payouts)
	}, server.TxReadCommitted)
	return
}

func requireProviderTotalsTestState(t testing.TB, ctx context.Context, id, networkId server.Id, wantApplied bool, wantBytes, wantRevenue int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var applied bool
		var provided, revenue int64
		server.Raise(conn.QueryRow(ctx, `SELECT (SELECT (args_json::jsonb->>'applied')::boolean FROM pending_task WHERE task_id=$1),
            COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0),
            COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0)`, id, networkId).Scan(&applied, &provided, &revenue))
		if applied != wantApplied || provided != wantBytes || revenue != wantRevenue {
			t.Fatal("provider projection marker or exact accounting differs")
		}
	})
}

// Provider projection must neither claim nor discard the independent mirror owner.
func requireProviderTotalsTestMirrorUntouched(t testing.TB, ctx context.Context, before *task.Task) {
	t.Helper()
	after := task.GetTasks(ctx, before.TaskId)[before.TaskId]
	if after == nil || after.FunctionName != before.FunctionName || after.ArgsJson != before.ArgsJson ||
		!after.ClaimTime.Equal(before.ClaimTime) || !after.ReleaseTime.Equal(before.ReleaseTime) ||
		after.RescheduleErrorCount != before.RescheduleErrorCount || after.RescheduleError != before.RescheduleError {
		t.Fatal("provider projection changed the independent mirror owner")
	}
}

// Cold mirror recovery uses its actual target, fenced Redis publisher and normal
// delete/post finalizer. Merely projecting provider totals cannot acknowledge it.
func finalizeProviderTotalsTestMirror(t testing.TB, ctx context.Context, owner *task.Task) {
	t.Helper()
	payload, err := decodeLegacyNetEscrowMirror([]byte(owner.ArgsJson))
	server.Raise(err)
	target := NewLegacyNetEscrowMirrorTaskTarget()
	if owner.FunctionName != target.TargetFunctionName() {
		t.Fatal("expected the exact durable mirror owner")
	}
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	defer worker.Close()
	worker.AddTargets(target)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, owner.TaskId, time.Time{}))
	})
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != owner.TaskId || len(retried) != 0 || len(posts) != 0 {
		t.Fatal("independent mirror owner did not complete and finalize", err)
	}
	completed := task.GetFinishedTasks(ctx, owner.TaskId)[owner.TaskId]
	if completed == nil || !completed.PostCompleted || completed.ArgsJson != owner.ArgsJson {
		t.Fatal("mirror finalization lost its exact scope or completion")
	}
	var result LegacyNetEscrowMirrorResult
	server.Raise(json.Unmarshal([]byte(completed.ResultJson), &result))
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT
            NOT EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$1) AND
            COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$2),0)=$3`,
			task.RunOnce("legacy_net_escrow_mirror", payload.BalanceId).String(), payload.BalanceId, result.Revision).Scan(&exact))
		if !exact {
			t.Fatal("mirror finalization lost its acknowledged revision or left an owner pending")
		}
	})
}

// Each rollback and missing-reply boundary uses the actual PostgreSQL owner.
func TestLegacyProviderTotalsAtomicEnqueueApplyAndReplay(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId, contractId := server.NewId(), server.NewId()
		var abortedId server.Id
		abort := errors.New("synthetic provider publication rollback")
		var err error
		server.HandleError(func() {
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))}, func(tx server.PgTx) {
				abortedId = queueLegacyProviderTotalsInTx(ctx, tx, contractId, map[server.Id]*contractPayout{networkId: {payoutByteCount: 17, payout: 29}})
				panic(abort)
			}, server.TxReadCommitted)
		}, func(cause error) { err = cause })
		if !errors.Is(err, abort) || abortedId == (server.Id{}) {
			t.Fatal("provider publication did not reach its actual rollback", err)
		}
		if len(task.GetTasks(ctx, abortedId)) != 0 {
			t.Fatal("rolled-back projection became visible")
		}
		id := providerTotalsTestTask(ctx, contractId, networkId)
		stale := task.GetTasks(ctx, id)[id]
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		server.Raise(applyLegacyProviderTotalsInTx(ctx, tx, id))
		server.Raise(tx.Rollback(ctx))
		requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
		tx, err = conn.Begin(ctx)
		server.Raise(err)
		server.Raise(applyLegacyProviderTotalsInTx(ctx, tx, id))
		server.Raise(tx.Commit(ctx))
		// The invocation object predates the application commit. A missing reply
		// must not let these stale arguments replace the durable applied marker.
		target := task.NewTaskTarget(ApplyLegacyProviderTotals)
		_, _, err = target.RunSpecific(ctx, stale)
		server.Raise(err)
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
		server.Tx(ctx, func(tx server.PgTx) {
			if err := applyLegacyProviderTotalsInTx(ctx, tx, server.NewId()); err == nil {
				t.Fatal("missing owner was accepted")
			}
		})
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
	})
}

func TestLegacyProviderTotalsConcurrentStaleInvocations(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		stale := task.GetTasks(ctx, id)[id]
		target := task.NewTaskTarget(ApplyLegacyProviderTotals)
		start := make(chan struct{})
		errors := make(chan error, 8)
		var workers sync.WaitGroup
		for range 8 {
			workers.Add(1)
			go func() { defer workers.Done(); <-start; _, _, err := target.RunSpecific(ctx, stale); errors <- err }()
		}
		close(start)
		workers.Wait()
		for range 8 {
			if err := <-errors; err != nil {
				t.Fatal("concurrent projection failed", err)
			}
		}
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
	})
}

// Unsupported readers must retain the exact payload. A new reader then runs
// the same task through real claim, execution and finalization, without a post.
func TestLegacyProviderTotalsUnknownReaderCleanupAndFinalization(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		before := task.GetTasks(ctx, id)[id].ArgsJson
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, id, time.Time{}))
		})
		old := task.NewTaskWorkerWithDefaults(ctx)
		defer old.Close()
		old.EvalTasks(1)
		after := task.GetTasks(ctx, id)[id]
		if after == nil || after.ArgsJson != before || after.RescheduleErrorCount != 1 {
			t.Fatal("unsupported reader lost or mutated projection")
		}
		task.RemoveFinishedTasks(ctx, server.NowUtc().Add(time.Hour), server.NowUtc().Add(time.Hour))
		requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, id, time.Time{}))
		})
		current := task.NewTaskWorkerWithDefaults(ctx)
		defer current.Close()
		current.AddTargets(task.NewTaskTarget(ApplyLegacyProviderTotals))
		current.EvalTasks(1)
		if len(task.GetTasks(ctx, id)) != 0 {
			t.Fatal("applied task did not finalize")
		}
		finished := task.GetFinishedTasks(ctx, id)[id]
		if finished == nil {
			t.Fatal("projection finalization lost its completion record")
		}
		var payload legacyProviderTotalsPayload
		server.Raise(json.Unmarshal([]byte(finished.ArgsJson), &payload))
		if !payload.Applied {
			t.Fatal("finalization copied stale unapplied arguments")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var provided, revenue int64
			var postCount int
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents,
                (SELECT count(*) FROM pending_task) FROM account_balance WHERE network_id=$1`, networkId).Scan(&provided, &revenue, &postCount))
			if provided != 17 || revenue != 29 || postCount != 0 {
				t.Fatal("finalization added a post or changed accounting")
			}
		})
	})
}

func TestLegacyProviderTotalsSecondNetworkFailureRollsBackFirst(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networks := []server.Id{server.NewId(), server.NewId()}
		slices.SortFunc(networks, server.Id.Cmp)
		contractId := server.NewId()
		var id server.Id
		server.OwnedTx(ctx, []server.PgOwnershipKey{
			task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId)),
			server.NewPgOwnershipKey("account_balance", networks[1]),
		}, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id,provided_byte_count) VALUES($1,$2)`, networks[1], int64(math.MaxInt64)))
			id = queueLegacyProviderTotalsInTx(ctx, tx, contractId, map[server.Id]*contractPayout{
				networks[0]: {payoutByteCount: 17, payout: 29}, networks[1]: {payoutByteCount: 17, payout: 29},
			})
		}, server.TxReadCommitted)
		queued := task.GetTasks(ctx, id)[id]
		target := task.NewTaskTarget(ApplyLegacyProviderTotals)
		if _, _, err := target.RunSpecific(ctx, queued); err == nil {
			t.Fatal("second-network overflow was accepted")
		}
		requireProviderTotalsTestState(t, ctx, id, networks[0], false, 0, 0)
		requireProviderTotalsTestState(t, ctx, id, networks[1], false, math.MaxInt64, 0)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_balance SET provided_byte_count=0 WHERE network_id=$1`, networks[1]))
		})
		_, _, err := target.RunSpecific(ctx, queued)
		server.Raise(err)
		for _, networkId := range networks {
			requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
		}
	})
}

func TestLegacyProviderTotalsFinalizationRollbackKeepsAppliedOwner(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		forceDue := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, id, time.Time{}))
			})
		}
		forceDue()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_provider_totals_finalize_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic projection finalization failure' USING ERRCODE='P0001'; END $$;
                CREATE TRIGGER test_provider_totals_finalize_failure BEFORE INSERT ON finished_task FOR EACH ROW EXECUTE FUNCTION test_provider_totals_finalize_failure()`))
		})
		current := task.NewTaskWorkerWithDefaults(ctx)
		defer current.Close()
		current.AddTargets(task.NewTaskTarget(ApplyLegacyProviderTotals))
		failure := server.HandleError(func() { _, _, _, err := current.EvalTasks(1); server.Raise(err) })
		if failure == nil {
			t.Fatal("native finalization failure did not run")
		}
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
		if len(task.GetFinishedTasks(ctx, id)) != 0 {
			t.Fatal("rolled-back finalize published a finished row")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_provider_totals_finalize_failure ON finished_task; DROP FUNCTION test_provider_totals_finalize_failure()`))
		})
		forceDue()
		old := task.NewTaskWorkerWithDefaults(ctx)
		defer old.Close()
		_, retried, _, err := old.EvalTasks(1)
		server.Raise(err)
		if len(retried) != 1 {
			t.Fatal("unsupported reader did not retry applied owner")
		}
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
		forceDue()
		finished, _, _, err := current.EvalTasks(1)
		server.Raise(err)
		if len(finished) != 1 || len(task.GetTasks(ctx, id)) != 0 {
			t.Fatal("applied replay did not finalize")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=17 AND provided_net_revenue_nano_cents=29 FROM account_balance WHERE network_id=$1`, networkId).Scan(&exact))
			if !exact {
				t.Fatal("finalize rollback or unsupported-reader retry repeated totals")
			}
		})
	})
}

func TestLegacyProviderTotalsRejectsMissingIdentityAndInvalidPayload(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		if _, err := ApplyLegacyProviderTotals(nil, owner); err == nil {
			t.Fatal("projection accepted missing task execution")
		}
		networkId := server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{version}','2'::jsonb)::text WHERE task_id=$1`, id))
		})
		_, _, err := task.NewTaskTarget(ApplyLegacyProviderTotals).RunSpecific(ctx, task.GetTasks(ctx, id)[id])
		if err == nil {
			t.Fatal("unknown payload version was applied")
		}
		requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
	})
}
