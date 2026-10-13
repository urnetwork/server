// Regressions for the bounds the reindex rotation gained: PostgreSQL's transient
// naming, the load guard, the size bound, the cleanup lock bound, and the
// outcome an interrupted run reports to its task. The database tests reuse the
// genuine-debris fixtures and state barriers of db_maintenance_debris_test.go.
package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// The transient names must equal the ones PostgreSQL's ChooseRelationName
// makes, including the truncation that keeps a name within 63 bytes and the
// conflict counter, or cleanup cannot prove a copy and keeps it forever.
func TestReindexArtifactNameMirrorsPostgresNaming(t *testing.T) {
	longName := "synthetic_" + strings.Repeat("y", 63-len("synthetic_"))
	// a 3-byte character straddles the 56-byte cut of a ccnew2 label
	multibyteName := strings.Repeat("z", 55) + "€" + "tail"
	for _, c := range []struct {
		original string
		label    string
		want     string
	}{
		{original: "synthetic_value", label: "ccnew", want: "synthetic_value_ccnew"},
		{original: "synthetic_value", label: "ccold3", want: "synthetic_value_ccold3"},
		{original: longName, label: "ccnew", want: longName[:57] + "_ccnew"},
		{original: longName, label: "ccnew12", want: longName[:55] + "_ccnew12"},
		{original: multibyteName, label: "ccnew2", want: strings.Repeat("z", 55) + "_ccnew2"},
	} {
		got := reindexArtifactName(c.original, c.label)
		if got != c.want {
			t.Errorf("reindexArtifactName(%q, %q) = %q, want %q", c.original, c.label, got, c.want)
		}
		if 63 < len(got) {
			t.Errorf("reindexArtifactName(%q, %q) is %d bytes", c.original, c.label, len(got))
		}
		if !isReindexArtifactOf(got, c.original) {
			t.Errorf("%q is not recognized as the copy of %q", got, c.original)
		}
	}

	for _, c := range []struct {
		candidate string
		original  string
	}{
		// a suffix alone proves nothing
		{candidate: "synthetic_orphan_ccnew", original: "synthetic_value"},
		{candidate: "synthetic_valuex_ccnew", original: "synthetic_value"},
		{candidate: "synthetic_value_ccnew", original: "synthetic_valuex"},
		// PostgreSQL never writes a zero or zero-padded counter
		{candidate: "synthetic_value_ccnew0", original: "synthetic_value"},
		{candidate: "synthetic_value_ccnew01", original: "synthetic_value"},
		{candidate: "synthetic_value_ccnewer", original: "synthetic_value"},
		{candidate: "synthetic_value_ccnew", original: "synthetic_value_ccnew"},
		// truncated only when the original is too long
		{candidate: longName[:40] + "_ccnew", original: longName},
	} {
		if isReindexArtifactOf(c.candidate, c.original) {
			t.Errorf("%q was accepted as the copy of %q", c.candidate, c.original)
		}
	}
}

// The load guard refuses a rebuild beside another index build, behind an old
// transaction (the 5.7-hour payout and the Sunday pg_dump), and on a saturated
// primary; anything below every bound may start.
func TestDbMaintenanceDeferReasonRefusesLoadedPrimary(t *testing.T) {
	defaults := DefaultDbMaintenanceOptions()
	custom := &DbMaintenanceOptions{
		MaxTransactionAge:  time.Minute,
		MaxRunningBackends: 4,
	}
	for _, c := range []struct {
		load     dbMaintenanceLoad
		opts     *DbMaintenanceOptions
		deferred bool
	}{
		{load: dbMaintenanceLoad{}, opts: defaults, deferred: false},
		{load: dbMaintenanceLoad{otherIndexBuilds: 1}, opts: defaults, deferred: true},
		{load: dbMaintenanceLoad{oldestTransactionAge: DefaultDbMaintenanceMaxTransactionAge - time.Second}, opts: defaults, deferred: false},
		{load: dbMaintenanceLoad{oldestTransactionAge: DefaultDbMaintenanceMaxTransactionAge}, opts: defaults, deferred: true},
		{load: dbMaintenanceLoad{oldestTransactionAge: 6 * time.Hour}, opts: defaults, deferred: true},
		{load: dbMaintenanceLoad{runningBackends: DefaultDbMaintenanceMaxRunningBackends - 1}, opts: defaults, deferred: false},
		{load: dbMaintenanceLoad{runningBackends: DefaultDbMaintenanceMaxRunningBackends}, opts: defaults, deferred: true},
		{load: dbMaintenanceLoad{oldestTransactionAge: 2 * time.Minute}, opts: custom, deferred: true},
		{load: dbMaintenanceLoad{runningBackends: 4}, opts: custom, deferred: true},
		{load: dbMaintenanceLoad{oldestTransactionAge: 30 * time.Second, runningBackends: 3}, opts: custom, deferred: false},
	} {
		reason := dbMaintenanceDeferReason(c.load, c.opts)
		if (reason != "") != c.deferred {
			t.Errorf("load %+v with %+v: reason %q, want deferred=%t", c.load, *c.opts, reason, c.deferred)
		}
	}
}

// A toast copy pairs with the toast index it copies. Dropping it needs
// privileges the local test role lacks, so this checks the listing alone.
func TestDbMaintenanceListsToastCopyOfToastIndex(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_toast_debris"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_toast_debris (id bigint PRIMARY KEY, label text);
				INSERT INTO synthetic_toast_debris SELECT n, 'label ' || n FROM generate_series(1, 100) n
			`))
		})
		testDbMaintenanceInterruptIndexBuild(t, ctx, tableName, `REINDEX TABLE CONCURRENTLY synthetic_toast_debris`)

		var debris []dbMaintenanceReindexDebris
		var kept []string
		MaintenanceDb(ctx, func(conn PgConn) {
			var err error
			debris, kept, err = listDbMaintenanceReindexDebris(ctx, conn, tableName, 0)
			Raise(err)
		})
		toastPaired := false
		pkeyPaired := false
		for _, candidate := range debris {
			switch {
			case candidate.schema == "pg_toast" &&
				strings.HasPrefix(candidate.original, "pg_toast_") &&
				strings.HasSuffix(candidate.original, "_index") &&
				candidate.indexName == candidate.original+"_ccnew":
				toastPaired = true
			case candidate.schema == "public" &&
				candidate.original == "synthetic_toast_debris_pkey" &&
				candidate.indexName == "synthetic_toast_debris_pkey_ccnew":
				pkeyPaired = true
			}
		}
		if len(debris) != 2 || !toastPaired || !pkeyPaired || len(kept) != 0 {
			t.Fatalf("debris %+v, kept %v: want the toast and primary key copies paired with their originals", debris, kept)
		}
	})
}

// A table above the size bound is cleanup-only in the automatic rotation and
// is rebuilt only with the explicit opt-in.
func TestDbMaintenanceSizeBoundLeavesLargeTableToCleanup(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_large_rebuild"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_large_rebuild (id bigint PRIMARY KEY, value bigint);
				INSERT INTO synthetic_large_rebuild SELECT n, n FROM generate_series(1, 10000) n
			`))
		})
		epoch := testDbMaintenanceEpoch(tableName)
		// far below the table's heap alone
		const maxBytes = 64 * 1024

		before := testDbMaintenanceIndexOid(ctx, "synthetic_large_rebuild_pkey")
		outcome := DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
			Reindex:              true,
			Cleanup:              true,
			MaxReindexTableBytes: maxBytes,
		})
		if outcome.Stopped != "" || !slices.Contains(outcome.DoneTables, tableName) {
			t.Fatalf("bounded run outcome = %+v, want the table done", *outcome)
		}
		if after := testDbMaintenanceIndexOid(ctx, "synthetic_large_rebuild_pkey"); after != before {
			t.Fatal("a table above the size bound was rebuilt automatically", before, after)
		}

		outcome = DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
			Reindex:              true,
			Cleanup:              true,
			MaxReindexTableBytes: maxBytes,
			IncludeLargeTables:   true,
		})
		if outcome.Stopped != "" {
			t.Fatalf("opted-in run outcome = %+v", *outcome)
		}
		if after := testDbMaintenanceIndexOid(ctx, "synthetic_large_rebuild_pkey"); after == before {
			t.Fatal("the explicit opt-in did not rebuild the large table")
		}
	})
}

// An open transaction older than the bound defers the rebuild and keeps the
// table for the continuation; once it ends the rebuild runs.
func TestDbMaintenanceDefersRebuildBehindAnOpenTransaction(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_deferred_rebuild"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_deferred_rebuild (id bigint PRIMARY KEY);
				INSERT INTO synthetic_deferred_rebuild SELECT generate_series(1, 100);
				CREATE TABLE synthetic_unrelated (id bigint)
			`))
		})
		epoch := testDbMaintenanceEpoch(tableName)
		opts := &DbMaintenanceOptions{
			Reindex: true,
			Cleanup: true,
			// the holder's transaction has slept longer than this
			MaxTransactionAge: time.Millisecond,
		}
		before := testDbMaintenanceIndexOid(ctx, "synthetic_deferred_rebuild_pkey")

		release := testDbMaintenanceHoldWriterLock(t, ctx, "synthetic_unrelated")
		defer release()
		outcome := DbMaintenance(ctx, epoch, opts)
		if outcome.Stopped != DbMaintenanceDeferred || !strings.Contains(outcome.Reason, "transaction") ||
			slices.Contains(outcome.DoneTables, tableName) {
			t.Fatalf("outcome behind an open transaction = %+v, want deferred with the table left", *outcome)
		}
		if after := testDbMaintenanceIndexOid(ctx, "synthetic_deferred_rebuild_pkey"); after != before {
			t.Fatal("rebuilt behind an open transaction", before, after)
		}

		release()
		outcome = DbMaintenance(ctx, epoch, opts)
		if outcome.Stopped != "" || !slices.Contains(outcome.DoneTables, tableName) {
			t.Fatalf("outcome after the transaction ended = %+v", *outcome)
		}
		if after := testDbMaintenanceIndexOid(ctx, "synthetic_deferred_rebuild_pkey"); after == before {
			t.Fatal("the deferred table was not rebuilt once the transaction ended")
		}
	})
}

// A debris drop blocked by a lock holder gives up at its lock bound instead of
// waiting for the holder (on 2026-10-08 one waited 10386s until a drain), keeps
// the table from being rebuilt into a numbered sibling, and a later cleanup
// drops the debris once the holder is gone.
func TestDbMaintenanceBlockedCleanupGivesUpAtItsLockBound(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_blocked_cleanup"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_blocked_cleanup (id bigint PRIMARY KEY);
				INSERT INTO synthetic_blocked_cleanup SELECT generate_series(1, 100)
			`))
		})
		testDbMaintenanceInterruptIndexBuild(t, ctx, tableName, `REINDEX TABLE CONCURRENTLY synthetic_blocked_cleanup`)
		epoch := testDbMaintenanceEpoch(tableName)
		before := testDbMaintenanceIndexOid(ctx, "synthetic_blocked_cleanup_pkey")

		release := testDbMaintenanceHoldWriterLock(t, ctx, tableName)
		defer release()
		outcome := DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
			Reindex:            true,
			Cleanup:            true,
			CleanupLockTimeout: 50 * time.Millisecond,
		})
		if outcome.Stopped != "" {
			t.Fatalf("blocked cleanup outcome = %+v", *outcome)
		}
		if _, invalid := testDbMaintenanceIndexes(ctx, tableName); !slices.Equal(invalid, []string{"synthetic_blocked_cleanup_pkey_ccnew"}) {
			t.Fatalf("invalid indexes behind the lock holder = %v", invalid)
		}
		if after := testDbMaintenanceIndexOid(ctx, "synthetic_blocked_cleanup_pkey"); after != before {
			t.Fatal("rebuilt while its earlier copy could not be dropped", before, after)
		}

		release()
		DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
			Cleanup: true,
		})
		if _, invalid := testDbMaintenanceIndexes(ctx, tableName); len(invalid) != 0 {
			t.Fatalf("debris left after the holder ended = %v", invalid)
		}
	})
}

// A run interrupted mid-rebuild reports the interruption and leaves the table
// out of its done list, so the continuation repeats it.
func TestDbMaintenanceInterruptedRunReportsTheUnfinishedTable(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_interrupted_run"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_interrupted_run (id bigint PRIMARY KEY);
				INSERT INTO synthetic_interrupted_run SELECT generate_series(1, 100)
			`))
		})
		release := testDbMaintenanceHoldWriterLock(t, ctx, tableName)
		defer release()

		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		outcomes := make(chan *DbMaintenanceOutcome, 1)
		go func() {
			outcomes <- DbMaintenance(runCtx, testDbMaintenanceEpoch(tableName), &DbMaintenanceOptions{
				Reindex:    true,
				Cleanup:    true,
				Analyze:    true,
				DoneTables: []string{"synthetic_done_earlier"},
			})
		}()
		pid := testDbMaintenanceAwaitBlockedIndexBuild(t, ctx, tableName)
		runCancel()
		testDbMaintenanceAwaitIndexBuildEnd(t, ctx, pid)
		release()
		var outcome *DbMaintenanceOutcome
		select {
		case outcome = <-outcomes:
		case <-time.After(testDbMaintenanceBarrierTimeout):
			t.Fatal("interrupted maintenance did not return")
		}
		if outcome.Stopped != DbMaintenanceInterrupted ||
			!slices.Equal(outcome.DoneTables, []string{"synthetic_done_earlier"}) {
			t.Fatalf("interrupted outcome = %+v, want interrupted with only the earlier table done", *outcome)
		}
	})
}
