// The capability probe's failed pass through a task worker and the database:
// what its task's retry runs from.
package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The probe's pending row: its id, arguments, stored failure and its count,
// and the delay from its release to its run.
func capabilityProbeRetryTestRow(t testing.TB, ctx context.Context) (server.Id, CapabilityProbeArgs, string, int, time.Duration) {
	t.Helper()
	var taskId server.Id
	var raw, storedError string
	var errorCount int
	var runAt, released time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx,
			`SELECT task_id, args_json, coalesce(reschedule_error, ''), reschedule_error_count, run_at, release_time FROM pending_task WHERE run_once_key = $1`,
			capabilityProbeRunOnce().String(),
		).Scan(&taskId, &raw, &storedError, &errorCount, &runAt, &released))
	})
	var args CapabilityProbeArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("the probe's row holds arguments that do not read: %v", err)
	}
	return taskId, args, storedError, errorCount, runAt.Sub(released)
}

// Root cause (re-review finding 5): a failed pass's task retried with the
// arguments the pass ran with, so the held downgrades the pass returned with
// its failure, a hold it retired gone and one it held added, never reached
// the retry. Through a task worker, the failed pass's row keeps its failure
// and the pass's retry delay, and its arguments become the sweep the pass
// returned, which the retry then runs from.
func TestCapabilityProbeRetryRunsFromTheFailedPassSweep(t *testing.T) {
	settings := capprobe.DefaultSettings()
	settings.Enabled = true
	startedAt := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	retired, held := server.NewId(), server.NewId()
	sweep := capprobe.Sweep{
		Cursor:         server.NewId(),
		StartedAt:      startedAt,
		HeldDowngrades: []capprobe.HeldDowngrade{{ClientId: retired, SweepStartedAt: startedAt.Add(-6 * time.Hour)}},
	}
	// the same place, with the hold the pass retired gone and the one it
	// held added
	failedSweep := capprobe.Sweep{
		Cursor:         sweep.Cursor,
		StartedAt:      startedAt,
		HeldDowngrades: []capprobe.HeldDowngrade{{ClientId: held, SweepStartedAt: startedAt}},
	}
	passErr := errors.New("synthetic evidence write failure")
	ranFrom := []capprobe.Sweep{}
	withCapabilityProbeSeams(t,
		func() (capprobe.Settings, error) { return settings, nil },
		func(ctx context.Context, gotSettings capprobe.Settings, gotSweep capprobe.Sweep, maxTime time.Duration) (capprobe.Sweep, *capprobe.PassResult, error) {
			ranFrom = append(ranFrom, gotSweep)
			if len(ranFrom) == 1 {
				return failedSweep, &capprobe.PassResult{Started: 2, Recorded: 1, DowngradesHeld: 1}, passErr
			}
			return gotSweep, &capprobe.PassResult{}, nil
		},
	)
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		ranFrom = ranFrom[:0]
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleCapabilityProbeAt(clientSession, tx, &CapabilityProbeArgs{Sweep: sweep}, server.NowUtc().Add(-time.Minute), settings)
		})
		taskId, _, _, _, _ := capabilityProbeRetryTestRow(t, ctx)
		workerSettings := task.DefaultTaskWorkerSettings()
		workerSettings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, workerSettings)
		defer worker.Close()
		worker.AddTargets(NewCapabilityProbeTaskTarget())
		jsonOf := func(sweep capprobe.Sweep) string {
			encoded, err := json.Marshal(sweep)
			server.Raise(err)
			return string(encoded)
		}

		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != taskId {
			t.Fatalf("the failed pass was not retried as its own row: finished=%v retried=%v posts=%v err=%v", finished, retried, posts, err)
		}
		retryId, args, storedError, errorCount, delay := capabilityProbeRetryTestRow(t, ctx)
		if retryId != taskId || errorCount != 1 || !strings.Contains(storedError, passErr.Error()) {
			t.Fatalf("the retry lost the pass's failure: task %s error %q count %d", retryId, storedError, errorCount)
		}
		if delay < capabilityProbeRetryDelay-time.Second || capabilityProbeRetryDelay+time.Second < delay {
			t.Fatalf("the retry runs %s after its release, want the pass's retry delay %s", delay, capabilityProbeRetryDelay)
		}
		if jsonOf(args.Sweep) != jsonOf(failedSweep) || args.MaxTimeSeconds != int(settings.MaxTime()/time.Second) {
			t.Fatalf("the retry runs from %s (max time %d s), want the failed pass's sweep %s", jsonOf(args.Sweep), args.MaxTimeSeconds, jsonOf(failedSweep))
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2, release_time = $2 WHERE task_id = $1`, taskId, server.NowUtc().Add(-time.Minute)))
		})
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != taskId || len(retried) != 0 {
			t.Fatalf("the retry did not finish: finished=%v retried=%v posts=%v err=%v", finished, retried, posts, err)
		}
		if len(ranFrom) != 2 || jsonOf(ranFrom[1]) != jsonOf(failedSweep) {
			t.Fatalf("the passes ran from %v, want the retry from the failed pass's sweep", ranFrom)
		}
	})
}
