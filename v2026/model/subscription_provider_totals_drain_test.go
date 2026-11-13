package model

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Publishes one allocation through the real producer, then gives its row an id
// created age ago, as a row queued that long ago has.
func providerTotalsDrainTestPublish(ctx context.Context, payouts map[server.Id]*contractPayout, age time.Duration) server.Id {
	id := providerTotalsTestPublish(ctx, server.NewId(), payouts)
	aged := server.ContractIdFloor(server.NowUtc().Add(-age))
	_, err := rand.Read(aged[6:])
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET task_id=$2 WHERE task_id=$1`, id, aged))
		if tag.RowsAffected() != 1 {
			panic(errors.New("provider total fixture lost its row"))
		}
	})
	return aged
}

func providerTotalsDrainTestAccount(t testing.TB, ctx context.Context, networkId server.Id) (bytes int64, revenue int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$1),0),
            COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$1),0)`, networkId).Scan(&bytes, &revenue))
	})
	return
}

func requireProviderTotalsDrainTestAccount(t testing.TB, ctx context.Context, networkId server.Id, wantBytes, wantRevenue int64) {
	t.Helper()
	if bytes, revenue := providerTotalsDrainTestAccount(t, ctx, networkId); bytes != wantBytes || revenue != wantRevenue {
		t.Fatalf("provider account bytes %d want %d, revenue %d want %d", bytes, wantBytes, revenue, wantRevenue)
	}
}

// The ids still queued among ids.
func providerTotalsDrainTestPending(ctx context.Context, ids ...server.Id) map[server.Id]bool {
	pending := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT task_id FROM pending_task WHERE task_id=ANY($1)`, ids)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				pending[id] = true
			}
		})
	})
	return pending
}

// One pass from the oldest row, read and consumed at now.
func providerTotalsDrainTestPass(t testing.TB, ctx context.Context, now time.Time) ([]legacyProviderTotalsDrainCandidate, legacyProviderTotalsDrainOutcome) {
	t.Helper()
	candidates, err := readLegacyProviderTotalsDrainCandidates(ctx, server.Id{}, now, legacyProviderTotalsDrainPassRows)
	server.Raise(err)
	outcome, err := consumeLegacyProviderTotals(ctx, candidates, now)
	server.Raise(err)
	return candidates, outcome
}

// A pass credits each unapplied allocation once in one sum per provider,
// removes an already applied row without a credit, and leaves an invalid
// payload, a live lease and a row another transaction holds, without waiting.
func TestLegacyProviderTotalsDrainPassCreditsAndRemovesExactly(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		a, b, c := server.NewId(), server.NewId(), server.NewId()
		age := 5 * time.Minute
		r1 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{a: {payoutByteCount: 10, payout: 1}}, age)
		r2 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{a: {payoutByteCount: 20, payout: 2}}, age)
		r3 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{b: {payoutByteCount: 30, payout: 3}}, age)
		r4 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{a: {payoutByteCount: 40, payout: 4}, c: {payoutByteCount: 50, payout: 5}}, age)
		r5 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{b: {payoutByteCount: 60, payout: 6}}, age)
		r6 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{b: {payoutByteCount: 61, payout: 7}}, age)
		r7 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{a: {payoutByteCount: 70, payout: 7}}, age)
		r8 := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{c: {payoutByteCount: 80, payout: 8}}, age)
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			// r5's per-task reader committed its credit and marker, not its completion.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{applied}','true'::jsonb)::text WHERE task_id=$1`, r5))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance (network_id,provided_byte_count,provided_net_revenue_nano_cents) VALUES ($1,60,6)`, b))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json='{"_private_task_arguments":true,"version":2}' WHERE task_id=$1`, r6))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_time=$2,release_time=$3 WHERE task_id=$1`, r7, now, now.Add(10*time.Minute)))
		})
		before := task.GetTasks(ctx, r6, r7, r8)

		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		server.RaisePgResult(held.Exec(ctx, `SELECT task_id FROM pending_task WHERE task_id=$1 FOR UPDATE`, r8))
		candidates, outcome := providerTotalsDrainTestPass(t, ctx, now)
		if len(candidates) != 8 || outcome != (legacyProviderTotalsDrainOutcome{credited: 4, applied: 1, left: 3}) {
			t.Fatalf("pass read %d rows with outcome %+v", len(candidates), outcome)
		}
		requireProviderTotalsDrainTestAccount(t, ctx, a, 70, 7)
		requireProviderTotalsDrainTestAccount(t, ctx, b, 90, 9)
		requireProviderTotalsDrainTestAccount(t, ctx, c, 50, 5)
		pending := providerTotalsDrainTestPending(ctx, r1, r2, r3, r4, r5, r6, r7, r8)
		if len(pending) != 3 || !pending[r6] || !pending[r7] || !pending[r8] {
			t.Fatal("pass removed a row it must leave or left one it consumed", pending)
		}
		after := task.GetTasks(ctx, r6, r7, r8)
		for id, row := range before {
			if after[id] == nil || after[id].ArgsJson != row.ArgsJson || !after[id].ReleaseTime.Equal(row.ReleaseTime) {
				t.Fatal("pass changed a row it left")
			}
		}
		server.Raise(held.Rollback(ctx))

		_, outcome = providerTotalsDrainTestPass(t, ctx, now)
		if outcome != (legacyProviderTotalsDrainOutcome{credited: 1, left: 2}) {
			t.Fatalf("released row outcome %+v", outcome)
		}
		requireProviderTotalsDrainTestAccount(t, ctx, c, 130, 13)
		requireProviderTotalsDrainTestAccount(t, ctx, a, 70, 7)
	})
}

// A rolled-back pass leaves every row and credit; the next pass credits them
// once, and replaying its stale candidates credits nothing.
func TestLegacyProviderTotalsDrainRollbackAndReplay(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		// More providers than one write page, so the failure falls between pages.
		networkIds := make([]server.Id, legacyProviderTotalsWriteBatchLimit+6)
		ids := []server.Id{}
		for index := range networkIds {
			networkIds[index] = server.NewId()
			for copy := range 2 {
				ids = append(ids, providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{
					networkIds[index]: {payoutByteCount: ByteCount(100 + index + copy), payout: NanoCents(1 + index)},
				}, 3*time.Minute))
			}
		}
		now := server.NowUtc()
		candidates, err := readLegacyProviderTotalsDrainCandidates(ctx, server.Id{}, now, legacyProviderTotalsDrainPassRows)
		server.Raise(err)
		if len(candidates) != len(ids) {
			t.Fatal("pass did not read every queued row", len(candidates), len(ids))
		}
		abort := errors.New("synthetic failure after the first credit page")
		writes := 0
		failing := context.WithValue(ctx, accountBalanceWriteTestKey{}, func(context.Context, server.PgTx, server.Id) {
			writes++
			panic(abort)
		})
		outcome, err := consumeLegacyProviderTotals(failing, candidates, now)
		if !errors.Is(err, abort) || writes != 1 || outcome != (legacyProviderTotalsDrainOutcome{}) {
			t.Fatal("failed pass did not report its rollback", err, writes, outcome)
		}
		if len(providerTotalsDrainTestPending(ctx, ids...)) != len(ids) {
			t.Fatal("rolled-back pass removed rows")
		}
		for _, networkId := range networkIds {
			requireProviderTotalsDrainTestAccount(t, ctx, networkId, 0, 0)
		}

		outcome, err = consumeLegacyProviderTotals(ctx, candidates, now)
		server.Raise(err)
		if outcome != (legacyProviderTotalsDrainOutcome{credited: len(ids)}) {
			t.Fatalf("pass outcome %+v", outcome)
		}
		replayed, err := consumeLegacyProviderTotals(ctx, candidates, now)
		server.Raise(err)
		if replayed != (legacyProviderTotalsDrainOutcome{left: len(ids)}) {
			t.Fatalf("replayed candidates outcome %+v", replayed)
		}
		for index, networkId := range networkIds {
			requireProviderTotalsDrainTestAccount(t, ctx, networkId, int64(2*(100+index)+1), int64(2*(1+index)))
		}
		if len(providerTotalsDrainTestPending(ctx, ids...)) != 0 {
			t.Fatal("pass left consumed rows")
		}
	})
}

// A commit whose reply is lost is an error, and the pass that follows finds its
// rows gone: the credit is never repeated.
func TestLegacyProviderTotalsDrainLostCommitReplyIsNotReplayed(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		ids := []server.Id{}
		for index := range 5 {
			ids = append(ids, providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{
				networkId: {payoutByteCount: ByteCount(10 * (index + 1)), payout: NanoCents(index + 1)},
			}, 2*time.Minute))
		}
		now := server.NowUtc()
		candidates, err := readLegacyProviderTotalsDrainCandidates(ctx, server.Id{}, now, legacyProviderTotalsDrainPassRows)
		server.Raise(err)
		lost := errors.New("synthetic lost commit reply")
		outcome, err := consumeLegacyProviderTotalsWithRunner(ctx, candidates, now,
			func(ctx context.Context, networkIds []server.Id, apply func(server.PgTx) error) error {
				server.Raise(runLegacyProviderTotalsDrainTx(ctx, networkIds, apply))
				return lost
			})
		if !errors.Is(err, lost) || outcome != (legacyProviderTotalsDrainOutcome{}) {
			t.Fatal("lost reply was not reported without an outcome", err, outcome)
		}
		requireProviderTotalsDrainTestAccount(t, ctx, networkId, 150, 15)
		if len(providerTotalsDrainTestPending(ctx, ids...)) != 0 {
			t.Fatal("committed pass left its rows")
		}
		_, outcome = providerTotalsDrainTestPass(t, ctx, now)
		if outcome != (legacyProviderTotalsDrainOutcome{}) {
			t.Fatalf("pass after a lost reply outcome %+v", outcome)
		}
		requireProviderTotalsDrainTestAccount(t, ctx, networkId, 150, 15)
	})
}

// Concurrent passes from stale positions and the per-task reader on the same
// rows apply every allocation exactly once.
func TestLegacyProviderTotalsDrainConcurrentPassesAndPerTaskReader(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		type sum struct{ bytes, revenue int64 }
		expected := map[server.Id]*sum{}
		for _, networkId := range networkIds {
			expected[networkId] = &sum{}
		}
		ids := []server.Id{}
		for index := range 240 {
			payouts := map[server.Id]*contractPayout{
				networkIds[index%len(networkIds)]: {payoutByteCount: ByteCount(1000 + index), payout: NanoCents(3 + index)},
			}
			if index%7 == 0 {
				payouts[networkIds[(index+1)%len(networkIds)]] = &contractPayout{payoutByteCount: ByteCount(500 + index), payout: NanoCents(2 + index)}
			}
			for networkId, payout := range payouts {
				expected[networkId].bytes += int64(payout.payoutByteCount)
				expected[networkId].revenue += int64(payout.payout)
			}
			ids = append(ids, providerTotalsDrainTestPublish(ctx, payouts, 4*time.Minute))
		}
		queued := task.GetTasks(ctx, ids...)
		target := task.NewTaskTarget(ApplyLegacyProviderTotals)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for worker := range 4 {
			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				for range 20 {
					now := server.NowUtc()
					candidates, err := readLegacyProviderTotalsDrainCandidates(ctx, server.Id{}, now, 64)
					if err != nil {
						continue
					}
					// A refused or busy owner leaves the rows to a later pass.
					_, _ = consumeLegacyProviderTotals(ctx, candidates, now)
				}
			}()
			go func() {
				defer workers.Done()
				<-start
				for index := worker; index < len(ids); index += 4 {
					// The drain may already have removed the row.
					_, _, _ = target.RunSpecific(ctx, queued[ids[index]])
				}
			}()
		}
		close(start)
		workers.Wait()
		for range 10 {
			if _, outcome := providerTotalsDrainTestPass(t, ctx, server.NowUtc()); outcome.removed() == 0 {
				break
			}
		}
		if pending := providerTotalsDrainTestPending(ctx, ids...); len(pending) != 0 {
			t.Fatal("rows remain after the final passes", len(pending))
		}
		for networkId, want := range expected {
			requireProviderTotalsDrainTestAccount(t, ctx, networkId, want.bytes, want.revenue)
		}
	})
}

// The walk stops before ids younger than the lag, so a row an open producer
// transaction may still commit behind it is never passed.
func TestLegacyProviderTotalsDrainLeavesYoungIds(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		old := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{networkId: {payoutByteCount: 10, payout: 1}}, legacyProviderTotalsDrainLag+time.Minute)
		young := providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{networkId: {payoutByteCount: 20, payout: 2}}, legacyProviderTotalsDrainLag/2)
		candidates, outcome := providerTotalsDrainTestPass(t, ctx, server.NowUtc())
		if len(candidates) != 1 || candidates[0].taskId != old || outcome != (legacyProviderTotalsDrainOutcome{credited: 1}) {
			t.Fatal("pass read past the lag", len(candidates), outcome)
		}
		if pending := providerTotalsDrainTestPending(ctx, old, young); len(pending) != 1 || !pending[young] {
			t.Fatal("young row did not stay queued", pending)
		}
		// Once it is older than the lag, a pass after the old position takes it.
		later := server.NowUtc().Add(legacyProviderTotalsDrainLag)
		candidates, err := readLegacyProviderTotalsDrainCandidates(ctx, old, later, legacyProviderTotalsDrainPassRows)
		server.Raise(err)
		if len(candidates) != 1 || candidates[0].taskId != young {
			t.Fatal("pass after the position did not read the aged row", len(candidates))
		}
		requireProviderTotalsDrainTestAccount(t, ctx, networkId, 10, 1)
	})
}

// The registered chain consumes the queue, then publishes its successor from the
// position it reached after the idle delay.
func TestLegacyProviderTotalsDrainChainRunsAndReschedules(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		ids := []server.Id{}
		for index := range 3 {
			ids = append(ids, providerTotalsDrainTestPublish(ctx, map[server.Id]*contractPayout{
				networkId: {payoutByteCount: ByteCount(7 + index), payout: NanoCents(1)},
			}, 2*time.Minute))
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleDrainLegacyProviderTotals(owner, tx) })
		key := legacyProviderTotalsDrainRunOnce.String()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE run_once_key=$1`, key, time.Time{}))
		})
		worker := task.NewTaskWorker(ctx, task.DefaultTaskWorkerSettings())
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsDrainTaskTarget())
		finished, retried, _, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried) != 0 {
			t.Fatal("drain run did not finish", err, finished, retried)
		}
		requireProviderTotalsDrainTestAccount(t, ctx, networkId, 7+8+9, 3)
		if len(providerTotalsDrainTestPending(ctx, ids...)) != 0 {
			t.Fatal("drain run left queued rows")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var args string
			var runAt time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at FROM pending_task WHERE run_once_key=$1`, key).Scan(&args, &runAt))
			var next DrainLegacyProviderTotalsArgs
			server.Raise(json.Unmarshal([]byte(args), &next))
			if next.After != ids[len(ids)-1] {
				t.Fatal("successor did not keep the reached position", next.After)
			}
			if delay := runAt.Sub(server.NowUtc()); delay < legacyProviderTotalsDrainIdleDelay-2*time.Second || legacyProviderTotalsDrainIdleDelay < delay {
				t.Fatal("caught-up successor is not due after the idle delay", delay)
			}
		})
	})
}

// Each run's result sets its successor's due time; the position always carries.
func TestLegacyProviderTotalsDrainPostDelays(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := legacyProviderTotalsDrainRunOnce.String()
		position := server.NewId()
		for _, test := range []struct {
			result DrainLegacyProviderTotalsResult
			delay  time.Duration
		}{
			{DrainLegacyProviderTotalsResult{Enabled: true, Next: position, More: true}, 0},
			{DrainLegacyProviderTotalsResult{Enabled: true, Next: position}, legacyProviderTotalsDrainIdleDelay},
			{DrainLegacyProviderTotalsResult{Enabled: true, Next: position, Error: "failed"}, legacyProviderTotalsDrainErrorDelay},
			{DrainLegacyProviderTotalsResult{Next: position}, legacyProviderTotalsDrainPausedDelay},
		} {
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, key))
				server.Raise(DrainLegacyProviderTotalsPost(&DrainLegacyProviderTotalsArgs{}, &test.result, owner, tx))
			})
			server.Db(ctx, func(conn server.PgConn) {
				var args string
				var runAt time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at FROM pending_task WHERE run_once_key=$1`, key).Scan(&args, &runAt))
				var next DrainLegacyProviderTotalsArgs
				server.Raise(json.Unmarshal([]byte(args), &next))
				if next.After != position || runAt.Before(before.Add(test.delay)) || server.NowUtc().Add(test.delay).Before(runAt) {
					t.Fatalf("successor of %+v due at %s with %s", test.result, runAt.Sub(before), next.After)
				}
			})
		}
	})
}

// Only an explicit enabled: false pauses the drain; an out-of-bounds pace keeps
// the default.
func TestLegacyProviderTotalsDrainSettingsFromConfig(t *testing.T) {
	off, on := false, true
	low, high, valid := legacyProviderTotalsDrainMinRowsPerSecond-1, legacyProviderTotalsDrainMaxRowsPerSecond+1, 9000
	defaults := legacyProviderTotalsDrainSettings{enabled: true, rowsPerSecond: legacyProviderTotalsDrainDefaultRowsPerSecond}
	for _, test := range []struct {
		config *legacyProviderTotalsDrainConfig
		want   legacyProviderTotalsDrainSettings
	}{
		{nil, defaults},
		{&legacyProviderTotalsDrainConfig{}, defaults},
		{&legacyProviderTotalsDrainConfig{Drain: &legacyProviderTotalsDrainConfigValues{}}, defaults},
		{&legacyProviderTotalsDrainConfig{Drain: &legacyProviderTotalsDrainConfigValues{Enabled: &off}}, legacyProviderTotalsDrainSettings{rowsPerSecond: legacyProviderTotalsDrainDefaultRowsPerSecond}},
		{&legacyProviderTotalsDrainConfig{Drain: &legacyProviderTotalsDrainConfigValues{Enabled: &on, RowsPerSecond: &low}}, defaults},
		{&legacyProviderTotalsDrainConfig{Drain: &legacyProviderTotalsDrainConfigValues{RowsPerSecond: &high}}, defaults},
		{&legacyProviderTotalsDrainConfig{Drain: &legacyProviderTotalsDrainConfigValues{RowsPerSecond: &valid}}, legacyProviderTotalsDrainSettings{enabled: true, rowsPerSecond: valid}},
	} {
		if got := legacyProviderTotalsDrainSettingsFromConfig(test.config); got != test.want {
			t.Fatalf("settings %+v want %+v", got, test.want)
		}
	}
}

// Passes start at the configured rate of removed rows and never closer than the
// minimum interval; a pass slower than its share does not wait.
func TestLegacyProviderTotalsDrainPace(t *testing.T) {
	pace := &legacyProviderTotalsDrainPace{rowsPerSecond: 2000}
	start := time.Now()
	pace.AwaitNextPass(t.Context(), start, 400)
	if elapsed := time.Since(start); elapsed < 190*time.Millisecond {
		t.Fatal("pace did not hold 400 rows to 200 ms at 2000 rows a second", elapsed)
	}
	start = time.Now()
	pace.AwaitNextPass(t.Context(), start, 0)
	if elapsed := time.Since(start); elapsed < legacyProviderTotalsDrainPassInterval-time.Millisecond {
		t.Fatal("pace started an empty pass before the minimum interval", elapsed)
	}
	slow := time.Now().Add(-time.Second)
	pace = &legacyProviderTotalsDrainPace{rowsPerSecond: 2000}
	start = time.Now()
	pace.AwaitNextPass(t.Context(), slow, 100)
	if elapsed := time.Since(start); 50*time.Millisecond < elapsed {
		t.Fatal("pace waited after a pass slower than its share", elapsed)
	}
}

// A published allocation stays out of the per-task reader's claims for the
// deferral, while the drain takes it once it is older than the lag.
func TestLegacyProviderTotalsQueueDefersPerTaskReader(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		published := server.NowUtc()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		queued := task.GetTasks(ctx, id)[id]
		if queued == nil || queued.RunAt.Before(published.Add(legacyProviderTotalsDeferral-time.Second)) {
			t.Fatal("published allocation is due before the deferral")
		}
		worker := task.NewTaskWorker(ctx, task.DefaultTaskWorkerSettings())
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
		finished, retried, _, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 0 || len(retried) != 0 {
			t.Fatal("per-task reader claimed a deferred allocation", err, finished, retried)
		}
		_, outcome := providerTotalsDrainTestPass(t, ctx, server.NowUtc().Add(2*legacyProviderTotalsDrainLag))
		if outcome != (legacyProviderTotalsDrainOutcome{credited: 1}) {
			t.Fatalf("drain did not take the deferred allocation: %+v", outcome)
		}
		requireProviderTotalsDrainTestAccount(t, ctx, networkId, 17, 29)
	})
}
