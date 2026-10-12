// The reaper removes queued closes whose contract another closer retired, in
// claim order and bounded chunks, only while contract_close.yml enables it.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A retained queue: open contracts' closes, closes that outlived their
// contracts, one whose contract is gone, one a live claim leases and one
// another transaction holds. The rows span several claim-order groups.
type scheduledCloseReapFixture struct {
	open    []server.Id
	retired []server.Id
	missing server.Id
	leased  server.Id
	held    server.Id
}

func newScheduledCloseReapFixture(t testing.TB, ctx context.Context) *scheduledCloseReapFixture {
	t.Helper()
	f := newScheduledCloseOwnerFixture(ctx)
	payer := f.newPayer(t, ctx, "synthetic-reap-payer")
	ids := f.addLegacyContracts(ctx, payer, 9)
	reap := &scheduledCloseReapFixture{open: ids[:3], retired: ids[3:7], leased: ids[7], held: ids[8], missing: server.NewId()}
	deadline := publishScheduledCloses(t, ctx)
	for _, id := range append(append([]server.Id{}, reap.retired...), reap.leased, reap.held) {
		if closed, err := model.ReconcileContractAtDeadline(ctx, id, deadline); err != nil || closed.AlreadyClosed {
			t.Fatal("fixture did not retire the contract", err)
		}
		requeueRetiredScheduledClose(t, ctx, id, deadline)
	}
	requeueRetiredScheduledClose(t, ctx, reap.missing, deadline)
	server.Tx(ctx, func(tx server.PgTx) {
		key := func(id server.Id) string { return model.ScheduledContractCloseRunOnce(id).String() }
		// Distinct claim-order groups: later and earlier blocks, a lower
		// priority and a shorter maximum time within one block.
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=run_at+interval '1 minute' WHERE run_once_key=ANY($1)`,
			[]string{key(reap.open[1]), key(reap.retired[1]), key(reap.retired[2])}))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_priority=run_priority+1 WHERE run_once_key=$1`, key(reap.retired[2])))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_max_time_seconds=10 WHERE run_once_key=$1`, key(reap.retired[3])))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=run_at-interval '1 minute' WHERE run_once_key=$1`, key(reap.open[2])))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_time=$2,release_time=$3 WHERE run_once_key=$1`,
			key(reap.leased), server.NowUtc(), server.NowUtc().Add(task.TaskLeaseTimeout)))
	})
	return reap
}

// Holds the row lock of one queued close until release.
func holdScheduledCloseRow(t testing.TB, ctx context.Context, contractId server.Id) (release func()) {
	t.Helper()
	holding, released, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		server.Tx(ctx, func(tx server.PgTx) {
			var found bool
			server.Raise(tx.QueryRow(ctx, `SELECT true FROM pending_task WHERE run_once_key=$1 FOR UPDATE`,
				model.ScheduledContractCloseRunOnce(contractId).String()).Scan(&found))
			close(holding)
			<-released
		})
	}()
	<-holding
	return func() {
		close(released)
		<-done
	}
}

// One sweep in chunks of two from the head; returns the rows visited and removed.
func sweepScheduledCloses(t testing.TB, ctx context.Context) (visited int, removed int) {
	t.Helper()
	var position *ScheduledCloseReapPosition
	for range 64 {
		next, chunkVisited, chunkRemoved, err := reapScheduledContractClosesChunk(ctx, position, 2, server.NowUtc())
		if err != nil {
			t.Fatal("reap chunk failed", err)
		}
		visited += chunkVisited
		removed += chunkRemoved
		if chunkVisited < 2 {
			return
		}
		position = next
	}
	t.Fatal("reap sweep did not reach the end of the queue")
	return
}

// Only rows of retired or missing contracts go, each visited once in claim
// order across every group boundary, and a held row neither blocks the sweep
// nor is removed under its holder.
func TestScheduledCloseReapRemovesOnlyRetiredContractsQueuedCloses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newScheduledCloseReapFixture(t, ctx)
		total := len(readQueuedScheduledCloseContracts(t, ctx))
		release := holdScheduledCloseRow(t, t.Context(), f.held)
		visited, removed := sweepScheduledCloses(t, ctx)
		// The sweep returned while the row was still held.
		release()
		if visited != total || removed != len(f.retired)+1 {
			t.Fatalf("sweep visited %d of %d rows and removed %d; want every row once and %d removed",
				visited, total, removed, len(f.retired)+1)
		}
		queued := readQueuedScheduledCloseContracts(t, ctx)
		for _, id := range append(append([]server.Id{}, f.retired...), f.missing) {
			if _, found := queued[id]; found {
				t.Fatal("a retired or missing contract's queued close remained")
			}
		}
		for _, id := range append(append([]server.Id{}, f.open...), f.leased, f.held) {
			if _, found := queued[id]; !found {
				t.Fatal("the sweep removed an open contract's, a leased or a held queued close")
			}
		}
		// The next sweep finds only what stays.
		if visited, removed := sweepScheduledCloses(t, ctx); visited != len(queued) || removed != 1 {
			t.Fatal("a second sweep did not remove the released row alone", visited, removed)
		}
	})
}

// Reads the reaper's pending successor.
func readScheduledCloseReapSuccessor(t testing.TB, ctx context.Context) (args ReapScheduledContractClosesArgs, runAt time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var argsJson string
		server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at FROM pending_task WHERE run_once_key=$1`,
			task.RunOnce("reap_scheduled_contract_closes").String()).Scan(&argsJson, &runAt))
		server.Raise(json.Unmarshal([]byte(argsJson), &args))
	})
	return
}

// Without config a run reads nothing and wakes after the idle delay, keeping
// its position; enabled, it sweeps and wakes after the configured delay.
func TestScheduledCloseReapRunsOnlyWhileEnabled(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newScheduledCloseReapFixture(t, ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		retained := &ScheduledCloseReapPosition{AvailableBlock: 1, Priority: task.DefaultPriority, MaxTimeSeconds: 30, TaskId: server.NewId()}
		runAndPost := func(args *ReapScheduledContractClosesArgs) *ReapScheduledContractClosesResult {
			result, err := ReapScheduledContractCloses(args, owner)
			if err != nil {
				t.Fatal("reap run failed", err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(ReapScheduledContractClosesPost(args, result, owner, tx))
			})
			return result
		}

		before := server.NowUtc()
		result := runAndPost(&ReapScheduledContractClosesArgs{After: retained})
		total := len(f.open) + len(f.retired) + 3
		if result.Enabled || result.Visited != 0 || result.Removed != 0 || len(readQueuedScheduledCloseContracts(t, ctx)) != total {
			t.Fatal("a disabled reaper read or removed queued closes", result)
		}
		args, runAt := readScheduledCloseReapSuccessor(t, ctx)
		if args.After == nil || *args.After != *retained || runAt.Before(before.Add(scheduledCloseReapIdleDelay)) {
			t.Fatal("a disabled reaper lost its position or woke before the idle delay", args.After, runAt)
		}

		defer server.Config.PushSimpleResource(scheduledCloseReapResourceName,
			[]byte("scheduled_close_reap:\n  enabled: true\n  chunk_size: 2\n  chunks: 64\n  delay_millis: 250\n"))()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, task.RunOnce("reap_scheduled_contract_closes").String()))
		})
		before = server.NowUtc()
		result = runAndPost(&ReapScheduledContractClosesArgs{})
		if !result.Enabled || !result.Wrapped || result.Removed != len(f.retired)+2 || result.Visited != total {
			t.Fatal("an enabled reaper did not sweep the queue", result)
		}
		args, runAt = readScheduledCloseReapSuccessor(t, ctx)
		if args.After != nil || runAt.Before(before.Add(250*time.Millisecond)) || before.Add(scheduledCloseReapIdleDelay).Before(runAt) {
			t.Fatal("an enabled reaper did not start its next sweep at the head after its delay", args.After, runAt)
		}
	})
}

// Values outside their bounds keep the defaults, and an unreadable or absent
// section leaves the reaper disabled.
func TestScheduledCloseReapSettingsKeepTheirBounds(t *testing.T) {
	defaults := scheduledCloseReapSettings{chunkSize: scheduledCloseReapDefaultChunkSize, chunks: scheduledCloseReapDefaultChunks,
		delay: scheduledCloseReapDefaultDelay}
	if settings := readScheduledCloseReapSettings(); settings != defaults {
		t.Fatal("settings without config", settings)
	}
	enabled := defaults
	enabled.enabled = true
	cases := []struct {
		config string
		want   scheduledCloseReapSettings
	}{
		{config: "scheduled_close_reap:\n  enabled: true\n  chunk_size: 64\n  chunks: 2\n  delay_millis: 500\n",
			want: scheduledCloseReapSettings{enabled: true, chunkSize: 64, chunks: 2, delay: 500 * time.Millisecond}},
		{config: "scheduled_close_reap:\n  enabled: true\n", want: enabled},
		{config: "scheduled_close_reap:\n  enabled: true\n  chunk_size: 0\n  chunks: 65\n  delay_millis: 99\n", want: enabled},
		{config: "scheduled_close_reap:\n  enabled: true\n  chunk_size: 1025\n  chunks: -1\n", want: enabled},
		{config: "scheduled_close_reap:\n  chunk_size: 64\n", want: scheduledCloseReapSettings{chunkSize: 64, chunks: scheduledCloseReapDefaultChunks,
			delay: scheduledCloseReapDefaultDelay}},
		{config: "steal_concurrency: 2\n", want: defaults},
		{config: "scheduled_close_reap: [enabled\n", want: defaults},
	}
	for _, c := range cases {
		func() {
			defer server.Config.PushSimpleResource(scheduledCloseReapResourceName, []byte(c.config))()
			if settings := readScheduledCloseReapSettings(); settings != c.want {
				t.Errorf("config %q: settings = %+v, want %+v", c.config, settings, c.want)
			}
		}()
	}
}
