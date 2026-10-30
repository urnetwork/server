// The weekly bloat measurement's chain: the successor its post schedules for
// each outcome, and its body run on the real schema.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Every stop carries the week's cursor. An interruption continues within
// minutes, a deferral an hour later, and a finished week waits for the next
// week's slot.
func TestMeasureDbBloatNextTaskKeepsTheWeek(t *testing.T) {
	settings := server.DefaultDbBloatMeasureSettings()
	// a Tuesday; the next slot is Sunday 2026-11-01 12:00 UTC
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	nextSlot := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cursor := server.DbBloatMeasureCursor{
		Week:        time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
		Table:       "synthetic_bloat_c",
		ScanSpent:   20 * time.Minute,
		CatalogDone: true,
	}
	for _, c := range []struct {
		stopped server.DbBloatMeasureStop
		runAt   time.Time
	}{
		{stopped: server.DbBloatMeasureInterrupted, runAt: now.Add(5 * time.Minute)},
		{stopped: server.DbBloatMeasureDeferred, runAt: now.Add(time.Hour)},
		{stopped: "", runAt: nextSlot},
	} {
		next, runAt := measureDbBloatNextTask(&MeasureDbBloatResult{Cursor: cursor, Stopped: c.stopped}, now, settings)
		if next.Cursor != cursor || !runAt.Equal(c.runAt) {
			t.Errorf("after %q: next %+v at %s, want the cursor at %s", c.stopped, next.Cursor, runAt, c.runAt)
		}
	}

	// a chain seeded before any run decodes to a new week
	var seeded MeasureDbBloatArgs
	if err := json.Unmarshal([]byte(`{"cursor":{}}`), &seeded); err != nil || seeded.Cursor != (server.DbBloatMeasureCursor{}) {
		t.Fatalf("seeded args = %+v, %v", seeded, err)
	}
	encoded, err := json.Marshal(&MeasureDbBloatArgs{Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	var decoded MeasureDbBloatArgs
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.Cursor.Week.Equal(cursor.Week) ||
		decoded.Cursor.Table != cursor.Table || decoded.Cursor.ScanSpent != cursor.ScanSpent || !decoded.Cursor.CatalogDone {
		t.Fatalf("args round trip %s = %+v, %v", encoded, decoded, err)
	}
}

// The body records the week's measurements on the migrated schema, its post
// leaves one successor, and a canceled invocation is not acknowledged.
func TestMeasureDbBloatTaskRecordsTheWeekAndRejectsCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		// 200 heap pages, over the measurement's 1 MiB line
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_bloat_task (id bigint PRIMARY KEY, value bigint NOT NULL, create_time timestamp NOT NULL, token uuid NOT NULL)
			`))
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO synthetic_bloat_task
				SELECT n, n, timestamp '2026-10-01' + n * interval '1 second', md5(n::text)::uuid
				FROM generate_series(1, 24000) n
			`))
		}, server.OptReadWrite(), server.OptNoRetry())

		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		result, err := MeasureDbBloat(&MeasureDbBloatArgs{}, owner)
		if err != nil {
			t.Fatal("measurement body failed", err)
		}
		if result.Stopped != "" || !result.Cursor.CatalogDone || !result.Cursor.ScanDone {
			t.Fatalf("result = %+v", *result)
		}
		var measurements int
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM db_bloat_measurement WHERE table_name = 'synthetic_bloat_task' AND relation_kind = 'heap'`,
			).Scan(&measurements))
		}, server.OptNoRetry())
		if measurements != 1 {
			t.Fatalf("the body recorded %d heap measurements, want 1", measurements)
		}

		// the finished week's post schedules the next week's slot with its cursor
		settings := server.DefaultDbBloatMeasureSettings()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(MeasureDbBloatPost(&MeasureDbBloatArgs{}, result, owner, tx))
		})
		after := server.NowUtc()
		name := task.NewTaskTarget(MeasureDbBloat).TargetFunctionName()
		successors := []*task.Task{}
		for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
			if pending.FunctionName == name {
				successors = append(successors, pending)
			}
		}
		if len(successors) != 1 ||
			!(successors[0].RunAt.Equal(settings.NextWeekStart(before)) || successors[0].RunAt.Equal(settings.NextWeekStart(after))) {
			t.Fatalf("successors = %+v", successors)
		}
		var successorArgs MeasureDbBloatArgs
		if err := json.Unmarshal([]byte(successors[0].ArgsJson), &successorArgs); err != nil ||
			!successorArgs.Cursor.Week.Equal(result.Cursor.Week) || !successorArgs.Cursor.CatalogDone || !successorArgs.Cursor.ScanDone {
			t.Fatalf("successor args %s = %+v, %v", successors[0].ArgsJson, successorArgs, err)
		}

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		queued := &task.Task{TaskId: server.NewId(), ArgsJson: `{"cursor":{}}`, ClientAddress: ""}
		if _, _, err := task.NewTaskTarget(MeasureDbBloat).RunSpecific(canceled, queued); err == nil {
			t.Fatal("a canceled measurement was acknowledged")
		}
	})
}
