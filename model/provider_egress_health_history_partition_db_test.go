package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// provider_egress_health_history across its cutover to daily partitions
// (PARTITION-MAINTENANCE-PLAN.md C2), on the migrated test database: results
// without a run id, a run resent with a new time, a delayed result, a replay
// at the retention boundary, writes during the swap, and the cutover's steps.
// Fixed clocks and barriers; no sleep orders anything.

// Cuts the test database's history over at now: prepare, then swap.
func testEgressHistoryCutover(t testing.TB, ctx context.Context, now time.Time) {
	t.Helper()
	for _, options := range []ProviderEgressHealthHistoryCutoverOptions{{}, {Swap: true}} {
		options.Logf = t.Logf
		if err := MigrateProviderEgressHealthHistoryToPartitions(ctx, now, options); err != nil {
			t.Fatal(err)
		}
	}
	connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable), "p")
}

// An admitted URL cycle for a new client.
func testEgressHistoryCycle(t testing.TB, ctx context.Context, cycleStartedAt time.Time) server.Id {
	t.Helper()
	clientId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle (client_id, cycle_started_at, next_attempt_at) VALUES ($1, $2, $2)`, clientId, cycleStartedAt))
	})
	return clientId
}

// One URL receipt of the cycle, with the current policy's evidence.
func testEgressHistoryReceipt(clientId server.Id, cycleStartedAt time.Time, runId server.Id, measuredAt time.Time, ok bool) *ProviderEgressHealth {
	okCount := 0
	if ok {
		okCount = 1
	}
	return &ProviderEgressHealth{
		RunId:            runId,
		ClientId:         clientId,
		CycleStartedAt:   cycleStartedAt,
		MeasuredAt:       measuredAt,
		OKCount:          okCount,
		Total:            1,
		UrlProbeEvidence: fp2TestUrlProbeEvidence(measuredAt, ok),
	}
}

func testEgressHistoryCount(t testing.TB, ctx context.Context, table string, clientId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM public.%s WHERE client_id = $1`, table), clientId).Scan(&count))
	})
	return
}

func testEgressHistoryOutcomes(t testing.TB, ctx context.Context, clientId server.Id) (outcomes int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT outcome_count FROM provider_egress_probe_cycle WHERE client_id = $1`, clientId).Scan(&outcomes))
	})
	return
}

// The history's leaf for the day holding t.
func testEgressHistoryLeafName(t time.Time) string {
	family := providerEgressHealthHistoryPartitionFamily
	return family.LeafName(providerEgressHealthHistoryTable, family.RangeAt(t))
}

func testEgressHistoryLeafOf(t testing.TB, ctx context.Context, runId server.Id) (leaf string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT tableoid::regclass::text FROM provider_egress_health_history WHERE run_id = $1`, runId).Scan(&leaf))
	})
	return
}

// A result without a run id gets one from its client and measurement time on
// either layout: stored once, its repeat stores nothing, and the copied row
// still deduplicates it after the swap.
func TestProviderEgressHealthWithoutRunIdAcrossTheCutover(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := server.NewId()
		legacy := func(measuredAt time.Time) *ProviderEgressHealth {
			return &ProviderEgressHealth{ClientId: clientId, MeasuredAt: measuredAt, OKCount: 3, Total: 4,
				ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 3, Total: 4}}}
		}
		first := now.Add(-time.Hour)
		SetProviderEgressHealth(ctx, legacy(first))
		SetProviderEgressHealth(ctx, legacy(first))
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 1)

		testEgressHistoryCutover(t, ctx, now)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 1)
		SetProviderEgressHealth(ctx, legacy(first))
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 1)
		second := now.Add(-time.Minute)
		SetProviderEgressHealth(ctx, legacy(second))
		SetProviderEgressHealth(ctx, legacy(second))
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 2)
	})
}

// A run resent with a new time, as a report the server times at arrival is,
// stores and publishes once on either layout: the plain table's unique run id
// and the partitioned table's run id lookup both catch it.
func TestProviderEgressHealthRunIdResentWithANewTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		resend := func(clientId server.Id, runId server.Id, measuredAt time.Time) {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: runId, ClientId: clientId, MeasuredAt: measuredAt, OKCount: 1, Total: 1,
				ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}}})
		}
		for _, partitioned := range []bool{false, true} {
			if partitioned {
				testEgressHistoryCutover(t, ctx, now)
			}
			clientId := server.NewId()
			runId := server.NewId()
			resend(clientId, runId, now.Add(-2*time.Minute))
			resend(clientId, runId, now.Add(-time.Minute))
			connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 1)
			// the latest snapshot keeps the first post's time
			health := GetProviderEgressHealth(ctx, clientId)
			connect.AssertEqual(t, health.MeasuredAt.Equal(now.Add(-2*time.Minute)), true)
		}
	})
}

// A delayed result keeps its measured_at: on the partitioned history it lands
// in its own day's partition, not the arrival's, and credits its cycle once.
func TestProviderEgressHealthDelayedResultLandsInItsDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		testEgressHistoryCutover(t, ctx, now)
		cycleStartedAt := now.Add(-30 * time.Hour)
		clientId := testEgressHistoryCycle(t, ctx, cycleStartedAt)
		measuredAt := now.Add(-20 * time.Hour)
		runId := server.NewId()
		receipt := testEgressHistoryReceipt(clientId, cycleStartedAt, runId, measuredAt, true)
		SetProviderEgressHealth(ctx, receipt)
		SetProviderEgressHealth(ctx, receipt)
		connect.AssertEqual(t, testEgressHistoryLeafOf(t, ctx, runId), testEgressHistoryLeafName(measuredAt))
		connect.AssertEqual(t, testEgressHistoryOutcomes(t, ctx, clientId), 1)
	})
}

// A replay at the retention boundary never credits cycle progress twice:
// while the day is kept the replay meets its row, under a new time too; once
// the day is dropped the ingest refuses the replay's time, and the store does
// not recreate a day retention may have dropped.
func TestProviderEgressHealthReplayAtTheRetentionBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		testEgressHistoryCutover(t, ctx, now)
		cycleStartedAt := now.Add(-2 * time.Hour)
		clientId := testEgressHistoryCycle(t, ctx, cycleStartedAt)
		measuredAt := now.Add(-time.Hour)
		runId := server.NewId()
		receipt := testEgressHistoryReceipt(clientId, cycleStartedAt, runId, measuredAt, true)
		setProviderEgressHealthAt(ctx, receipt, now)
		connect.AssertEqual(t, testEgressHistoryOutcomes(t, ctx, clientId), 1)

		family := providerEgressHealthHistoryPartitionFamily
		day := family.RangeAt(measuredAt)
		cut := day.Upper.Add(family.Retention)
		// the last instant the day is kept: the replay meets its row
		setProviderEgressHealthAt(ctx, receipt, cut.Add(-time.Microsecond))
		resent := testEgressHistoryReceipt(clientId, cycleStartedAt, runId, measuredAt.Add(time.Minute), true)
		setProviderEgressHealthAt(ctx, resent, cut.Add(-time.Microsecond))
		connect.AssertEqual(t, testEgressHistoryOutcomes(t, ctx, clientId), 1)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 1)

		// at the cut the day is dropped
		report, _ := family.Retire(ctx, cut, server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, 0 < len(report.Dropped), true)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 0)
		// the ingest takes nothing that old at the cut
		connect.AssertEqual(t, measuredAt.Before(cut.Add(-DefaultProviderEgressHealthHistorySettings().MaxIngestAge)), true)
		// and the store refuses to recreate the day
		refused := func() (r any) {
			defer func() {
				r = recover()
			}()
			setProviderEgressHealthAt(ctx, receipt, cut)
			return nil
		}()
		connect.AssertEqual(t, refused != nil && strings.Contains(fmt.Sprint(refused), "is not created"), true)
		connect.AssertEqual(t, testEgressHistoryOutcomes(t, ctx, clientId), 1)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 0)
	})
}

// A write that arrives while the swap holds its locks waits for the swap, then
// lands in the partitioned table; a write before it is copied, so it is in
// both; each credits its cycle once.
func TestProviderEgressHealthWritesDuringTheSwap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		if err := MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Logf: t.Logf}); err != nil {
			t.Fatal(err)
		}
		cycleStartedAt := now.Add(-2 * time.Hour)
		clientId := testEgressHistoryCycle(t, ctx, cycleStartedAt)
		before := testEgressHistoryReceipt(clientId, cycleStartedAt, server.NewId(), now.Add(-time.Minute), true)
		SetProviderEgressHealth(ctx, before)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryStagedTable, clientId), 1)

		locked := make(chan struct{})
		release := make(chan struct{})
		swapped := make(chan error, 1)
		go func() {
			swapped <- MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{
				Swap: true,
				Logf: t.Logf,
				swapLocked: func() {
					close(locked)
					<-release
				},
			})
		}()
		<-locked
		during := testEgressHistoryReceipt(clientId, cycleStartedAt, server.NewId(), now, false)
		stored := make(chan struct{})
		go func() {
			defer close(stored)
			SetProviderEgressHealth(ctx, during)
		}()
		// the barrier: the write waits on the swap's lock on the history
		waiting := false
		for i := 0; i < 500 && !waiting; i += 1 {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE NOT granted AND relation = to_regclass('public.provider_egress_health_history'))`).Scan(&waiting))
			})
			if !waiting {
				select {
				case <-stored:
					t.Fatal("the write stored while the swap held its locks")
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if !waiting {
			t.Fatal("the write never waited on the swap's lock")
		}
		close(release)
		connect.AssertEqual(t, <-swapped, nil)
		<-stored

		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable), "p")
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 2)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryOldTable, clientId), 1)
		connect.AssertEqual(t, testEgressHistoryOutcomes(t, ctx, clientId), 2)
		after := testEgressHistoryReceipt(clientId, cycleStartedAt, server.NewId(), now.Add(time.Second), true)
		SetProviderEgressHealth(ctx, after)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 3)
		connect.AssertEqual(t, testEgressHistoryOutcomes(t, ctx, clientId), 3)
	})
}

// The cutover's steps: a dry run changes nothing; prepare copies the window
// and leaves older rows behind; a resumed prepare copies only what differs;
// the swap moves the names; the first partition boundary routes each row to
// its day; verify passes; the registry keeps the days ahead; drop-old drops
// the old table only once verify passes, and the rerun of any step is a
// no-op.
func TestProviderEgressHealthHistoryCutoverLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		today := server.PartitionGranularityDaily.RangeStart(now)
		clientId := server.NewId()
		store := func(measuredAt time.Time) server.Id {
			runId := server.NewId()
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: runId, ClientId: clientId, MeasuredAt: measuredAt, OKCount: 1, Total: 1,
				ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}}})
			return runId
		}
		// three days back is outside the two-day window
		store(today.AddDate(0, 0, -3).Add(time.Hour))
		yesterdayRunId := store(today.AddDate(0, 0, -1).Add(time.Hour))
		store(now.Add(-time.Minute))

		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{DryRun: true, Logf: t.Logf}), nil)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryStagedTable), "")

		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Logf: t.Logf}), nil)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryStagedTable, clientId), 2)
		connect.AssertEqual(t, providerEgressHealthHistoryTriggerExists(ctx, providerEgressHealthHistoryTable, providerEgressHealthHistoryMirror), true)
		// mirrored as it is written
		store(now)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryStagedTable, clientId), 3)
		// an interrupted copy: a staged row lost, then prepare resumes
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM provider_egress_health_history_new WHERE run_id = $1`, yesterdayRunId))
		}, server.OptReadWrite())
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryStagedTable, clientId), 2)
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Logf: t.Logf}), nil)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryStagedTable, clientId), 3)

		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Swap: true, DryRun: true, Logf: t.Logf}), nil)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable), "r")
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Swap: true, Logf: t.Logf}), nil)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable), "p")
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable), "r")
		connect.AssertEqual(t, providerEgressHealthHistoryTriggerExists(ctx, providerEgressHealthHistoryOldTable, providerEgressHealthHistoryMirror), false)
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Swap: true, Logf: t.Logf}), nil)

		// the first partition boundary: each side of midnight in its own day
		tomorrow := today.AddDate(0, 0, 1)
		lastOfToday := store(tomorrow.Add(-time.Microsecond))
		firstOfTomorrow := store(tomorrow)
		connect.AssertEqual(t, testEgressHistoryLeafOf(t, ctx, lastOfToday), testEgressHistoryLeafName(today))
		connect.AssertEqual(t, testEgressHistoryLeafOf(t, ctx, firstOfTomorrow), testEgressHistoryLeafName(tomorrow))

		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Verify: true, Logf: t.Logf}), nil)
		report := providerEgressHealthHistoryPartitionFamily.EnsureRunway(ctx, now)
		connect.AssertEqual(t, len(report.Errors), 0)
		connect.AssertEqual(t, report.CurrentCovered, true)
		connect.AssertEqual(t, providerEgressHealthHistoryPartitionFamily.AheadRanges <= report.FutureRanges, true)

		// drop-old refuses while a row of the window is missing
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO provider_egress_health_history_old (run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure)
				VALUES ($1, $2, $3, 1, 1, '{}', false)`, server.NewId(), clientId, now))
		}, server.OptReadWrite())
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{DropOld: true, Logf: t.Logf}) != nil, true)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable), "r")
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM provider_egress_health_history_old WHERE client_id = $1 AND measured_at = $2`, clientId, now))
		}, server.OptReadWrite())
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{DropOld: true, Logf: t.Logf}), nil)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable), "")
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{DropOld: true, Logf: t.Logf}), nil)
	})
}

// Until the old table is dropped, rollback undoes the swap and keeps every
// row written since; a prepare then resumes the staged copy and swaps again.
// Before a swap, abort removes the mirror and the staged table.
func TestProviderEgressHealthHistoryRollbackKeepsLaterWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := server.NewId()
		store := func(measuredAt time.Time) {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: measuredAt, OKCount: 1, Total: 1,
				ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}}})
		}
		store(now.Add(-time.Hour))
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Rollback: true, Logf: t.Logf}) != nil, true)

		testEgressHistoryCutover(t, ctx, now)
		store(now.Add(-time.Minute))
		store(now)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 3)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryOldTable, clientId), 1)

		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Rollback: true, DryRun: true, Logf: t.Logf}), nil)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable), "p")
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Rollback: true, Logf: t.Logf}), nil)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable), "r")
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryStagedTable), "p")
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable), "")
		connect.AssertEqual(t, providerEgressHealthHistoryTriggerExists(ctx, providerEgressHealthHistoryStagedTable, providerEgressHealthHistoryRollbackMirror), false)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 3)
		// the plain table takes writes again, with the unique run id
		store(now.Add(time.Second))
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 4)

		// prepare resumes the staged copy, and the swap goes again
		testEgressHistoryCutover(t, ctx, now)
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 4)

		// once swapped, abort refuses: rollback is the way back
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Abort: true, Logf: t.Logf}) != nil, true)
	})
}

// Before its swap a prepared cutover aborts to the plain table alone.
func TestProviderEgressHealthHistoryAbortBeforeTheSwap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Logf: t.Logf}), nil)
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Abort: true, Logf: t.Logf}), nil)
		connect.AssertEqual(t, providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryStagedTable), "")
		connect.AssertEqual(t, providerEgressHealthHistoryTriggerExists(ctx, providerEgressHealthHistoryTable, providerEgressHealthHistoryMirror), false)
		// writes go on without the mirror
		clientId := server.NewId()
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now, OKCount: 1, Total: 1,
			ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}}})
		connect.AssertEqual(t, testEgressHistoryCount(t, ctx, providerEgressHealthHistoryTable, clientId), 1)
		connect.AssertEqual(t, MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Abort: true, Logf: t.Logf}), nil)
	})
}
