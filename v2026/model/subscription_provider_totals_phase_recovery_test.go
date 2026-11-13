package model

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

func providerPhaseRequireFinished(t testing.TB, ctx context.Context, ids []server.Id, networkId server.Id, original map[server.Id]*task.Task) {
	t.Helper()
	if len(task.GetTasks(ctx, ids...)) != 0 {
		t.Fatal("provider phase recovery left an exact owner pending")
	}
	finished := task.GetFinishedTasks(ctx, ids...)
	if len(finished) != len(ids) {
		t.Fatal("provider phase recovery lost finished identities")
	}
	for _, id := range ids {
		row := finished[id]
		if row == nil || !row.PostCompleted || row.ResultJson != "{}" {
			t.Fatal("provider phase recovery lost an acknowledged finalization")
		}
		payload, err := decodeLegacyProviderTotals(row.ArgsJson)
		if err != nil || !payload.Applied || len(payload.Totals) != 1 ||
			payload.Totals[0].NetworkId != networkId || payload.Totals[0].Bytes != 17 || payload.Totals[0].Revenue != 29 {
			t.Fatal("provider phase recovery changed a durable applied allocation")
		}
		before := original[id]
		if before == nil {
			t.Fatal("provider phase recovery has no original identity oracle")
		}
		expected, err := decodeLegacyProviderTotals(before.ArgsJson)
		expected.Applied = true
		if err != nil || !reflect.DeepEqual(expected, payload) {
			t.Fatal("provider phase recovery permuted or replaced an immutable allocation")
		}
	}
	server.Db(ctx, func(conn server.PgConn) {
		var bytes, revenue int64
		var writes int
		server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents,
            (SELECT count(*) FROM test_provider_total_write WHERE network_id=$1)
            FROM account_balance WHERE network_id=$1`, networkId).Scan(&bytes, &revenue, &writes))
		if bytes != int64(17*len(ids)) || revenue != int64(29*len(ids)) || writes != 1 {
			t.Fatal("provider phase recovery repeated or lost exact credits")
		}
	})
}

// Real marker and deferred-commit failures leave credits and applied markers
// rolled back. The ordinary worker retains each exact ID and its retry count.
func TestLegacyProviderTotalsPhaseMarkerAndCommitRefusalRetainOwners(t *testing.T) {
	for _, count := range []int{1, 2} {
		for _, phase := range []string{"applied_marker", "commit"} {
			name := "single/" + phase
			if count > 1 {
				name = "batch/" + phase
			}
			t.Run(name, func(t *testing.T) {
				providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
					providerTotalsBatchWriteCounter(t, ctx)
					networkId := server.NewId()
					ids := make([]server.Id, 0, count)
					for range count {
						ids = append(ids, providerTotalsTestTask(ctx, server.NewId(), networkId))
					}
					providerTotalsBatchDue(ctx, ids)
					before := task.GetTasks(ctx, ids...)
					code := "55P03"
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE test_provider_phase_attempt`))
						if phase == "applied_marker" {
							server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_provider_phase_refusal() RETURNS trigger LANGUAGE plpgsql AS $$
                            BEGIN
                              IF (NEW.args_json::jsonb->>'applied')::boolean THEN
                                PERFORM nextval('test_provider_phase_attempt');
                                RAISE EXCEPTION 'private-marker-row %', NEW.task_id USING ERRCODE='55P03';
                              END IF;
                              RETURN NEW;
                            END $$;
                            CREATE TRIGGER test_provider_phase_refusal BEFORE UPDATE ON pending_task
                            FOR EACH ROW EXECUTE FUNCTION test_provider_phase_refusal()`))
						} else {
							code = "40001"
							server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_provider_phase_refusal() RETURNS trigger LANGUAGE plpgsql AS $$
                            BEGIN
                              PERFORM nextval('test_provider_phase_attempt');
                              RAISE EXCEPTION 'private-commit-row %', NEW.network_id USING ERRCODE='40001';
                            END $$;
                            CREATE CONSTRAINT TRIGGER test_provider_phase_refusal AFTER INSERT OR UPDATE ON account_balance
                            DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION test_provider_phase_refusal()`))
						}
					})
					settings := task.DefaultTaskWorkerSettings()
					settings.ClaimRegisteredTargetsOnly = true
					worker := task.NewTaskWorker(ctx, settings)
					defer worker.Close()
					worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
					finished, retried, posts, err := worker.EvalTasks(count)
					if err != nil || len(finished) != 0 || len(retried) != count || len(posts) != 0 {
						t.Fatal("provider phase refusal did not retain ordinary per-task retries", err)
					}
					pending := task.GetTasks(ctx, ids...)
					if len(pending) != count || len(task.GetFinishedTasks(ctx, ids...)) != 0 {
						t.Fatal("provider phase refusal surrendered a durable owner")
					}
					for _, id := range ids {
						row := pending[id]
						if row == nil || row.ArgsJson != before[id].ArgsJson || row.RescheduleErrorCount != 1 ||
							!strings.Contains(row.RescheduleError, "phase="+phase) || !strings.Contains(row.RescheduleError, "SQLSTATE "+code) ||
							strings.Contains(row.RescheduleError, "private-") || strings.Contains(row.RescheduleError, id.String()) ||
							strings.Contains(row.RescheduleError, networkId.String()) {
							t.Fatal("provider phase retry lost its cause discriminator or exposed an identifier")
						}
						requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
					}
					server.Db(ctx, func(conn server.PgConn) {
						var attempts, writes int
						var called bool
						server.Raise(conn.QueryRow(ctx, `SELECT last_value,is_called,(SELECT count(*) FROM test_provider_total_write)
                            FROM test_provider_phase_attempt`).Scan(&attempts, &called, &writes))
						if !called || attempts != 1 || writes != 0 {
							t.Fatal("provider phase retried the transaction or retained rolled-back accounting")
						}
					})
					server.Tx(ctx, func(tx server.PgTx) {
						if phase == "applied_marker" {
							server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_provider_phase_refusal ON pending_task`))
						} else {
							server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_provider_phase_refusal ON account_balance`))
						}
					})
					// This correctness control explicitly rearms the same failed IDs;
					// it is not a timing or natural retry-cadence benchmark.
					providerTotalsBatchDue(ctx, ids)
					finished, retried, posts, err = worker.EvalTasks(count)
					if err != nil || len(finished) != count || len(retried)+len(posts) != 0 {
						t.Fatal("provider phase recovery did not finish exact original owners", err)
					}
					providerPhaseRequireFinished(t, ctx, ids, networkId, before)
				})
			})
		}
	}
}

// The actual transaction commits, then its owner loses the reply. This is an
// owner-return fault, not a socket simulation; no credit may be replayed.
func TestLegacyProviderTotalsPhaseLostCommitReplyKeepsAppliedAuthority(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		ids := []server.Id{providerTotalsTestTask(ctx, server.NewId(), networkId), providerTotalsTestTask(ctx, server.NewId(), networkId)}
		original := task.GetTasks(ctx, ids...)
		ownerCalls, bodyCalls := 0, 0
		err := runLegacyProviderTotalsTxWithOwner(ctx, func(tx server.PgTx) error {
			bodyCalls++
			return applyLegacyProviderTotalsBatchInTx(ctx, tx, ids, networkId)
		}, func(ownerCtx context.Context, body func(server.PgTx), options ...any) {
			ownerCalls++
			server.Tx(ownerCtx, body, options...)
			panic(io.ErrUnexpectedEOF)
		})
		if ownerCalls != 1 || bodyCalls != 1 || !errors.Is(err, io.ErrUnexpectedEOF) ||
			!strings.Contains(err.Error(), "phase=commit") || len(task.GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("unknown provider commit reply was replayed or acknowledged")
		}
		for _, id := range ids {
			requireProviderTotalsTestState(t, ctx, id, networkId, true, 34, 58)
		}
		providerTotalsBatchDue(ctx, ids)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
		finished, retried, posts, err := worker.EvalTasks(len(ids))
		if err != nil || len(finished) != len(ids) || len(retried)+len(posts) != 0 {
			t.Fatal("fresh provider owner did not reconcile the committed marker", err)
		}
		providerPhaseRequireFinished(t, ctx, ids, networkId, original)
	})
}

func TestLegacyProviderTotalsPhaseCancellationKeepsStatementCause(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id) VALUES($1)`, networkId))
		})
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { _ = held.Rollback(context.Background()) }) }
		defer release()
		var holderPid int
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, networkId))
		ownerCtx, cancelOwner := context.WithCancel(ctx)
		defer cancelOwner()
		pidReady, done := make(chan int, 1), make(chan error, 1)
		go func() {
			done <- runLegacyProviderTotalsTx(ownerCtx, func(tx server.PgTx) error {
				// Only the control permits enough time to observe and cancel the
				// exact server wait; production keeps its unchanged SQL limits.
				server.RaisePgResult(tx.Exec(ownerCtx, `SET LOCAL lock_timeout='10s'; SET LOCAL statement_timeout='10s'`))
				var pid int
				server.Raise(tx.QueryRow(ownerCtx, `SELECT pg_backend_pid()`).Scan(&pid))
				pidReady <- pid
				return applyLegacyProviderTotalsInTx(ownerCtx, tx, id)
			})
		}()
		joined := false
		defer func() {
			cancelOwner()
			release()
			if !joined {
				<-done
			}
		}()
		var waiterPid int
		select {
		case waiterPid = <-pidReady:
		case <-ctx.Done():
			t.Fatal("cancelable provider phase did not start", ctx.Err())
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)) AND EXISTS(
                    SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE '%INSERT INTO account_balance%')`,
					waiterPid, holderPid).Scan(&blocked))
			})
			if blocked {
				break
			}
			select {
			case err := <-done:
				joined = true
				t.Fatal("cancelable provider phase ended before the actual account wait", err)
			case <-ctx.Done():
				t.Fatal("cancelable provider phase account wait was not observed", ctx.Err())
			case <-tick.C:
			}
		}
		cancelOwner()
		select {
		case err = <-done:
			joined = true
		case <-ctx.Done():
			t.Fatal("canceled provider phase did not unwind", ctx.Err())
		}
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "phase=account_write") {
			t.Fatal("provider cancellation lost its precise statement phase or cause")
		}
		requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
		release()
		server.Raise(runLegacyProviderTotalsTx(ctx, func(tx server.PgTx) error {
			return applyLegacyProviderTotalsInTx(ctx, tx, id)
		}))
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
	})
}
