// Aggregate maintenance timestamps require a whole partitioned-table pass.
package server

import (
	"strings"
	"testing"
	"time"
)

// A real skipped leaf keeps the parent's stale warning until a complete pass.
func TestDbHotVacuumPartitionSkipKeepsStaleWarning(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_hot_parent (id bigint) PARTITION BY RANGE (id);
				CREATE TABLE synthetic_hot_child_a PARTITION OF synthetic_hot_parent FOR VALUES FROM (0) TO (10) WITH (autovacuum_enabled = false);
				CREATE TABLE synthetic_hot_child_b PARTITION OF synthetic_hot_parent FOR VALUES FROM (10) TO (20) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_hot_parent VALUES (1), (11);
				DELETE FROM synthetic_hot_parent;
			`))
		})
		_, release := testDbMaintenanceHoldVacuumLock(t, ctx, "synthetic_hot_child_b")
		defer release()
		clock := newTestDbHotVacuumClock()
		settings := testDbHotVacuumSettings("synthetic_hot_parent")
		state := DbHotVacuumState{
			IndexPassTime: clock.now(),
			Tables: map[string]DbHotVacuumTableState{
				"synthetic_hot_parent": {TrackTime: clock.now().Add(-2 * time.Hour)},
			},
		}
		run := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, state)
		run.run(ctx)
		result := run.report.tables[0]
		if result.outcome != dbHotVacuumSkipped || !strings.Contains(result.detail, "synthetic_hot_child_b") || result.analyzed ||
			!run.state.Tables["synthetic_hot_parent"].VacuumTime.IsZero() || !run.state.Tables["synthetic_hot_parent"].AnalyzeTime.IsZero() || len(run.report.staleTables) != 1 {
			t.Fatalf("partial vacuum counted as complete: result=%+v state=%+v stale=%v", result, run.state, run.report.staleTables)
		}
		release()
		complete := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, run.state)
		complete.run(ctx)
		if complete.report.tables[0].outcome != dbHotVacuumVacuumed || !complete.state.Tables["synthetic_hot_parent"].VacuumTime.Equal(clock.now()) ||
			!complete.state.Tables["synthetic_hot_parent"].AnalyzeTime.Equal(clock.now()) || len(complete.report.staleTables) != 0 {
			t.Fatalf("complete pass did not clear stale state: result=%+v state=%+v", complete.report, complete.state)
		}
	})
}

// The wire fixture sends an earlier leaf's real completion notices before the
// later leaf times out. A hand-driven clock makes the warning deterministic.
func TestDbHotVacuumPartitionTimeoutKeepsStaleWarning(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	reply := testDbHotVacuumFinished("synthetic_hot_child_a", 13, 0)
	reply.relations = []string{"synthetic_hot_child_a", "synthetic_hot_child_b"}
	reply.err = testDbHotVacuumCanceled()
	reply.duration = 2 * time.Minute
	server.setReply("synthetic_hot_parent", reply)
	settings := testDbHotVacuumSettings("synthetic_hot_parent")
	state := DbHotVacuumState{
		IndexPassTime: clock.now(),
		Tables: map[string]DbHotVacuumTableState{
			"synthetic_hot_parent": {TrackTime: clock.now().Add(-2 * time.Hour)},
		},
	}
	run := newDbHotVacuumRun(settings, server.pool, clock.now, state)
	run.run(t.Context())
	result := run.report.tables[0]
	if result.outcome != dbHotVacuumTimedOut || result.analyzed || result.removedRows != 13 || !strings.Contains(result.detail, "statement timeout") ||
		!run.state.Tables["synthetic_hot_parent"].VacuumTime.IsZero() || len(run.report.staleTables) != 1 {
		t.Fatalf("partial timeout counted as complete: result=%+v state=%+v stale=%v", result, run.state, run.report.staleTables)
	}
	completeReply := testDbHotVacuumFinished("synthetic_hot_child_a", 0, 0)
	completeReply.relations = reply.relations
	completeReply.notices = append(completeReply.notices, testDbHotVacuumFinished("synthetic_hot_child_b", 7, 0).notices...)
	completeReply.notices = append(completeReply.notices, testDbHotVacuumFinished("synthetic_hot_parent", 0, 0).notices[1])
	server.setReply("synthetic_hot_parent", completeReply)
	complete := newDbHotVacuumRun(settings, server.pool, clock.now, run.state)
	complete.run(t.Context())
	if complete.report.tables[0].outcome != dbHotVacuumVacuumed || len(complete.report.staleTables) != 0 ||
		!complete.state.Tables["synthetic_hot_parent"].AnalyzeTime.Equal(clock.now()) {
		t.Fatalf("complete partition pass did not clear stale warning: %+v", complete.report)
	}
}

// A plain table's finished vacuum still counts when only its ANALYZE fails.
func TestDbHotVacuumCompletedHeapBeforeAnalyzeTimeout(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	reply := testDbHotVacuumFinished("synthetic_hot_heap", 3, 0)
	reply.notices = reply.notices[:1]
	reply.err = testDbHotVacuumCanceled()
	server.setReply("synthetic_hot_heap", reply)
	run := newDbHotVacuumRun(testDbHotVacuumSettings("synthetic_hot_heap"), server.pool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
	run.run(t.Context())
	if run.report.tables[0].outcome != dbHotVacuumVacuumed || run.report.tables[0].analyzed ||
		!run.state.Tables["synthetic_hot_heap"].VacuumTime.Equal(clock.now()) || !run.state.Tables["synthetic_hot_heap"].AnalyzeTime.IsZero() {
		t.Fatalf("a failed analyze erased the completed heap vacuum: result=%+v state=%+v", run.report, run.state)
	}
}
