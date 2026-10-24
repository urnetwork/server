// The debit chains back off while their partition is empty and are kicked when
// a debit is journaled, through the real worker, journal, Redis marks and queue
// rows. Eligibility waits only for the queue's one-second availability block of
// a row already due.
package work

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A partition chain's pending row as the tests read it.
type transferDebitChainRow struct {
	taskId         server.Id
	args           FlushTransferDebitsArgs
	runAt          time.Time
	availableBlock int64
	// Changes with every write of the row.
	version string
}

// The partition chain's single pending row.
func requireTransferDebitChain(t testing.TB, ctx context.Context, shard int) (row transferDebitChainRow) {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT task_id,args_json,run_at,available_block,xmin::text FROM pending_task WHERE run_once_key=$1`,
			task.RunOnce(model.TransferDebitFlushRunOnceKey(shard)).String())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var data []byte
				server.Raise(rows.Scan(&row.taskId, &data, &row.runAt, &row.availableBlock, &row.version))
				server.Raise(json.Unmarshal(data, &row.args))
				count++
			}
		})
	})
	if count != 1 {
		t.Fatal("the partition chain is not exactly one pending row", shard, count)
	}
	return
}

// Publishes one partition's chain due now, as the startup scheduler does.
func scheduleTransferDebitChainDue(ctx context.Context, shard int) {
	owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer owner.Cancel()
	key := task.RunOnceOwnershipKey(task.RunOnce(model.TransferDebitFlushRunOnceKey(shard)))
	server.OwnedTx(ctx, []server.PgOwnershipKey{key}, func(tx server.PgTx) {
		scheduleFlushTransferDebits(owner, tx, &FlushTransferDebitsArgs{Shard: shard}, server.NowUtc().Add(-time.Second), DefaultSettlementChainSettings())
	}, server.TxReadCommitted, server.OptNoRetry())
}

// A real grant in its own debit partition.
func newTransferDebitGrant(ctx context.Context) (balanceId server.Id, shard int) {
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "synthetic-debit-wake", server.NewId())
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(model.AddBasicTransferBalanceInTx(tx, ctx, networkId, 1024, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour)))
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT balance_id FROM transfer_balance WHERE network_id=$1`, networkId).Scan(&balanceId))
	})
	return balanceId, int(balanceId[15]) % model.TransferDebitShardCount
}

// The zero-use settlement shape: one committed zero-byte journal row.
func journalTransferDebit(ctx context.Context, balanceId server.Id, shard int) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal(contract_id,balance_id,debit_byte_count,shard) VALUES($1,$2,0,$3)`,
			server.NewId(), balanceId, shard))
	}, server.TxReadCommitted, server.OptNoRetry())
}

// The partition's journal rows, pending or applied.
func transferDebitJournalCount(ctx context.Context, shard int) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_debit_journal WHERE shard=$1`, shard).Scan(&count))
	})
	return
}

// A committed debit's wake of its partitions' chains, as the settlement post
// sends it for the partitions it journaled.
func wakeTransferDebitChain(ctx context.Context, shards ...int) ([]string, error) {
	keys := make([]string, 0, len(shards))
	for _, shard := range shards {
		keys = append(keys, model.TransferDebitFlushRunOnceKey(shard))
	}
	return task.WakeIdleRunOnce(ctx, task.DefaultIdleWakeSettings(), keys...)
}

// Waits for the availability block of a row due now or at the active delay,
// never for a backed-off one.
func waitForTransferDebitChainBlock(t testing.TB, ctx context.Context, row transferDebitChainRow) {
	t.Helper()
	delay := time.Until(time.Unix(row.availableBlock*task.BlockSizeSeconds, 0))
	if DefaultSettlementChainSettings().ActiveDelay+2*task.BlockSizeSeconds*time.Second < delay {
		t.Fatal("the chain is not due within its active cadence", delay)
	}
	if delay <= 0 {
		return
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		t.Fatal("the due chain never became eligible")
	}
}

// Runs the partition's due chain once through the real worker, its finishing
// transaction and its commit post.
func runTransferDebitChain(t testing.TB, ctx context.Context, worker *task.TaskWorker, shard int) {
	t.Helper()
	row := requireTransferDebitChain(t, ctx, shard)
	waitForTransferDebitChainBlock(t, ctx, row)
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != row.taskId || len(retried)+len(posts) != 0 {
		t.Fatal("the partition chain did not run once", finished, retried, posts, err)
	}
}

// Runs one empty pass of the partition's due chain and checks the successor's
// count and delay.
func runEmptyTransferDebitPass(t testing.TB, ctx context.Context, worker *task.TaskWorker, shard int, wantPasses int, wantDelay time.Duration) transferDebitChainRow {
	t.Helper()
	before := server.NowUtc()
	runTransferDebitChain(t, ctx, worker, shard)
	after := server.NowUtc()
	row := requireTransferDebitChain(t, ctx, shard)
	if row.args.EmptyPasses != wantPasses || row.args.AfterBalanceId != nil || row.runAt.Before(before.Add(wantDelay)) || row.runAt.After(after.Add(wantDelay)) {
		t.Fatal("an empty pass scheduled the wrong successor", row.args, row.runAt.Sub(before), wantPasses, wantDelay)
	}
	return row
}

// Backs the partition's chain off with real empty passes.
func backOffTransferDebitChain(t testing.TB, ctx context.Context, worker *task.TaskWorker, shard int) transferDebitChainRow {
	t.Helper()
	settings := DefaultSettlementChainSettings()
	scheduleTransferDebitChainDue(ctx, shard)
	var row transferDebitChainRow
	for pass := 1; pass <= settings.EmptyPassesBeforeBackOff; pass++ {
		delay := settings.ActiveDelay
		if pass == settings.EmptyPassesBeforeBackOff {
			delay = settings.IdleDelay
		}
		row = runEmptyTransferDebitPass(t, ctx, worker, shard, pass, delay)
	}
	return row
}

// The first empty pass keeps the two-second cadence and leaves no mark; the
// second backs off to the idle delay and marks the chain so that a producer
// can kick it.
func TestTransferDebitIdleChainBacksOffToIdleDelay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		worker := startupClosureWorker(ctx, NewTransferDebitTaskTarget())
		defer worker.Close()
		const shard = 5
		settings := DefaultSettlementChainSettings()
		key := model.TransferDebitFlushRunOnceKey(shard)
		scheduleTransferDebitChainDue(ctx, shard)
		first := runEmptyTransferDebitPass(t, ctx, worker, shard, 1, settings.ActiveDelay)
		if kicked, err := task.WakeIdleRunOnce(ctx, settings.Wake, key); err != nil || len(kicked) != 0 ||
			requireTransferDebitChain(t, ctx, shard).version != first.version {
			t.Fatal("a single empty pass marked its chain for kicks", kicked, err)
		}
		backedOff := runEmptyTransferDebitPass(t, ctx, worker, shard, 2, settings.IdleDelay)
		kicked, err := task.WakeIdleRunOnce(ctx, settings.Wake, key)
		if err != nil || len(kicked) != 1 {
			t.Fatal("the backed-off chain left no mark for producers", kicked, err)
		}
		if row := requireTransferDebitChain(t, ctx, shard); row.taskId != backedOff.taskId || row.runAt.After(server.NowUtc()) {
			t.Fatal("the mark did not lead to the backed-off successor", row.taskId, row.runAt)
		}
	})
}

// A debit journaled while its partition's chain is backed off kicks the chain:
// it is due at once, not at the idle delay, and the next run flushes the debit.
// That run is back at work and clears the mark, so producers stop kicking.
func TestTransferDebitBackedOffChainFlushesKickedDebit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		worker := startupClosureWorker(ctx, NewTransferDebitTaskTarget())
		defer worker.Close()
		balanceId, shard := newTransferDebitGrant(ctx)
		backedOff := backOffTransferDebitChain(t, ctx, worker, shard)
		journalTransferDebit(ctx, balanceId, shard)
		kicked, err := wakeTransferDebitChain(ctx, shard, shard)
		woke := server.NowUtc()
		if err != nil || len(kicked) != 1 || kicked[0] != model.TransferDebitFlushRunOnceKey(shard) {
			t.Fatal("the producer did not kick the backed-off chain", kicked, err)
		}
		row := requireTransferDebitChain(t, ctx, shard)
		if row.taskId != backedOff.taskId || row.runAt.After(woke) || row.availableBlock > woke.Unix()/task.BlockSizeSeconds+2 {
			t.Fatal("the kicked chain is not due within its availability block", row.runAt.Sub(woke), row.availableBlock)
		}
		runTransferDebitChain(t, ctx, worker, shard)
		if transferDebitJournalCount(ctx, shard) != 0 {
			t.Fatal("the kicked run did not flush the debit")
		}
		resumed := requireTransferDebitChain(t, ctx, shard)
		if resumed.args.EmptyPasses != 0 {
			t.Fatal("a run that flushed work counted an empty pass", resumed.args)
		}
		kicked, err = wakeTransferDebitChain(ctx, shard)
		if err != nil || len(kicked) != 0 || requireTransferDebitChain(t, ctx, shard).version != resumed.version {
			t.Fatal("a producer kicked the resumed chain", kicked, err)
		}
	})
}

// A producer of an active partition writes nothing to the queue: there is no
// mark, so the chain's row is left exactly as its post scheduled it.
func TestTransferDebitActiveChainIsNotKicked(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balanceId, shard := newTransferDebitGrant(ctx)
		scheduleTransferDebitChainDue(ctx, shard)
		before := requireTransferDebitChain(t, ctx, shard)
		journalTransferDebit(ctx, balanceId, shard)
		kicked, err := wakeTransferDebitChain(ctx, shard)
		if err != nil || len(kicked) != 0 {
			t.Fatal("a producer kicked an active chain", kicked, err)
		}
		if after := requireTransferDebitChain(t, ctx, shard); after.version != before.version || !after.runAt.Equal(before.runAt) {
			t.Fatal("a producer wrote the active chain's queue row")
		}
	})
}

// Concurrent producers of one backed-off partition kick it once: one removes
// the mark. The run-once key keeps one row, and one claim runs the chain once.
func TestTransferDebitKickedChainRunsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		worker := startupClosureWorker(ctx, NewTransferDebitTaskTarget())
		defer worker.Close()
		balanceId, shard := newTransferDebitGrant(ctx)
		backedOff := backOffTransferDebitChain(t, ctx, worker, shard)
		journalTransferDebit(ctx, balanceId, shard)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var stateLock sync.Mutex
		kicks, failures := 0, 0
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				kicked, err := wakeTransferDebitChain(ctx, shard)
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					kicks += len(kicked)
					if err != nil {
						failures++
					}
				}()
			}()
		}
		close(start)
		wg.Wait()
		if kicks != 1 || failures != 0 {
			t.Fatal("concurrent producers did not coalesce into one kick", kicks, failures)
		}
		if row := requireTransferDebitChain(t, ctx, shard); row.taskId != backedOff.taskId {
			t.Fatal("a kick replaced the backed-off chain row")
		}
		countFinished := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM finished_task WHERE run_once_key=$1`,
					task.RunOnce(model.TransferDebitFlushRunOnceKey(shard)).String()).Scan(&count))
			})
			return
		}
		finishedBefore := countFinished()
		row := requireTransferDebitChain(t, ctx, shard)
		waitForTransferDebitChainBlock(t, ctx, row)
		finished, retried, posts, err := worker.EvalTasks(4)
		if err != nil || len(finished) != 1 || finished[0] != backedOff.taskId || len(retried)+len(posts) != 0 {
			t.Fatal("the kicked chain did not run exactly once", finished, retried, posts, err)
		}
		if countFinished() != finishedBefore+1 || transferDebitJournalCount(ctx, shard) != 0 {
			t.Fatal("the kick ran the chain other than once")
		}
		requireTransferDebitChain(t, ctx, shard)
	})
}

// A taskworker restart re-runs the startup scheduler over a backed-off chain.
// The run-once merge keeps the one row and its arguments and pulls it to the
// startup cadence; the mark lives in Redis, so producers still kick it, and a
// new worker runs it.
func TestTransferDebitChainSurvivesTaskworkerRestart(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balanceId, shard := newTransferDebitGrant(ctx)
		first := startupClosureWorker(ctx, NewTransferDebitTaskTarget())
		backedOff := backOffTransferDebitChain(t, ctx, first, shard)
		first.Close()
		restarted := server.NowUtc()
		ScheduleSettlementAccountingTasks(ctx)
		row := requireTransferDebitChain(t, ctx, shard)
		active := DefaultSettlementChainSettings().ActiveDelay
		if row.taskId != backedOff.taskId || row.args.EmptyPasses != backedOff.args.EmptyPasses || row.runAt.After(server.NowUtc().Add(active)) || row.runAt.Before(restarted) {
			t.Fatal("the restart lost, replaced or did not pull in the backed-off chain", row.args, row.runAt.Sub(restarted))
		}
		for other := range model.TransferDebitShardCount {
			requireTransferDebitChain(t, ctx, other)
		}
		// Only this partition's chain is under test; the others would be due
		// with it at the startup cadence.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key LIKE '["flush_transfer_debits_%' AND run_once_key<>$1`,
				task.RunOnce(model.TransferDebitFlushRunOnceKey(shard)).String()))
		}, server.TxReadCommitted, server.OptNoRetry())
		journalTransferDebit(ctx, balanceId, shard)
		kicked, err := wakeTransferDebitChain(ctx, shard)
		if err != nil || len(kicked) != 1 {
			t.Fatal("the mark did not survive the restart", kicked, err)
		}
		second := startupClosureWorker(ctx, NewTransferDebitTaskTarget())
		defer second.Close()
		runTransferDebitChain(t, ctx, second, shard)
		if transferDebitJournalCount(ctx, shard) != 0 || requireTransferDebitChain(t, ctx, shard).args.EmptyPasses != 0 {
			t.Fatal("the restarted chain did not flush its debit")
		}
	})
}

// A debit that commits after a backing-off run's traversal but before its mark
// finds no mark to clear. The run looks at the journal again after setting the
// mark and kicks its own successor.
func TestTransferDebitIdleRunWakesDebitCommittedBeforeItsMark(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer owner.Cancel()
		balanceId, shard := newTransferDebitGrant(ctx)
		// The pass after one empty pass backs off.
		args := &FlushTransferDebitsArgs{Shard: shard, EmptyPasses: 1}
		result, err := FlushTransferDebits(args, owner)
		if err != nil || !transferDebitFlushEmpty(args, result) {
			t.Fatal("the empty partition's traversal was not empty", result, err)
		}
		// The producer commits here and finds no mark yet.
		journalTransferDebit(ctx, balanceId, shard)
		if kicked, err := wakeTransferDebitChain(ctx, shard); err != nil || len(kicked) != 0 {
			t.Fatal("a producer found a mark before the backing-off run set it", kicked, err)
		}
		var commitPosts []server.PostFunction
		key := task.RunOnceOwnershipKey(task.RunOnce(model.TransferDebitFlushRunOnceKey(shard)))
		server.OwnedTx(ctx, []server.PgOwnershipKey{key}, func(tx server.PgTx) {
			commitPosts, err = FlushTransferDebitsCommitPost(args, result, owner, tx)
			server.Raise(err)
		}, server.TxReadCommitted, server.OptNoRetry())
		idle := DefaultSettlementChainSettings().IdleDelay
		if row := requireTransferDebitChain(t, ctx, shard); row.args.EmptyPasses != 2 || row.runAt.Before(server.NowUtc().Add(idle/2)) {
			t.Fatal("the second empty pass did not back off", row.args, row.runAt)
		}
		server.RunPosts(ctx, commitPosts...)
		if row := requireTransferDebitChain(t, ctx, shard); row.runAt.After(server.NowUtc()) {
			t.Fatal("the backing-off run's second look did not wake it for the committed debit", row.runAt)
		}
		if kicked, err := wakeTransferDebitChain(ctx, shard); err != nil || len(kicked) != 0 {
			t.Fatal("the self-wake left its mark for another kick", kicked, err)
		}
	})
}
