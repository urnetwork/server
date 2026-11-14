// The hot table vacuum's task chain: its cadence, its arguments, and one run
// of its body and post on the migrated schema.
package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The next run starts an interval after this one started, so a run's own
// length does not stretch the cadence; after a run longer than the interval,
// the next starts at once.
func TestVacuumHotTablesNextRunAtKeepsTheCadence(t *testing.T) {
	interval := server.DefaultDbHotVacuumSettings().Interval
	startTime := time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{now: startTime.Add(3 * time.Second), want: startTime.Add(interval)},
		{now: startTime.Add(4 * time.Minute), want: startTime.Add(interval)},
		{now: startTime.Add(interval), want: startTime.Add(interval)},
		{now: startTime.Add(interval + time.Minute), want: startTime.Add(interval + time.Minute)},
	} {
		if got := vacuumHotTablesNextRunAt(startTime, c.now, interval); !got.Equal(c.want) {
			t.Errorf("after a run ending at %s: next at %s, want %s", c.now, got, c.want)
		}
	}
}

// The state a run leaves reaches the next run's arguments intact, and a chain
// scheduled fresh decodes to the zero state.
func TestVacuumHotTablesArgsCarryTheState(t *testing.T) {
	vacuumTime := time.Date(2026, 10, 24, 3, 4, 5, 6, time.UTC)
	result := &VacuumHotTablesResult{
		State: server.DbHotVacuumState{
			NextTable: "synthetic_hot_b",
			Tables: map[string]server.DbHotVacuumTableState{
				"synthetic_hot_a": {TrackTime: vacuumTime.Add(-time.Hour), VacuumTime: vacuumTime, AnalyzeTime: vacuumTime},
				"synthetic_hot_b": {TrackTime: vacuumTime.Add(-time.Hour)},
			},
			IndexPassTime: vacuumTime.Add(-time.Minute),
		},
		StartTime: vacuumTime,
	}
	encoded, err := json.Marshal(&VacuumHotTablesArgs{State: result.State})
	if err != nil {
		t.Fatal(err)
	}
	var decoded VacuumHotTablesArgs
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State.NextTable != "synthetic_hot_b" || !decoded.State.IndexPassTime.Equal(result.State.IndexPassTime) ||
		len(decoded.State.Tables) != 2 || decoded.State.Tables["synthetic_hot_a"] != result.State.Tables["synthetic_hot_a"] ||
		decoded.State.Tables["synthetic_hot_b"] != result.State.Tables["synthetic_hot_b"] {
		t.Fatalf("decoded %+v from %s", decoded, encoded)
	}
	var fresh VacuumHotTablesArgs
	if err := json.Unmarshal([]byte(`{}`), &fresh); err != nil || fresh.State.NextTable != "" || fresh.State.Tables != nil {
		t.Fatalf("fresh args = %+v, %v", fresh, err)
	}
}

// One run of the body on the migrated schema vacuums every listed table, and
// its post schedules the one successor with that state an interval after the
// run started.
func TestVacuumHotTablesTaskRunsAndSchedulesItsSuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		settings := server.DefaultDbHotVacuumSettings()

		result, err := VacuumHotTables(&VacuumHotTablesArgs{}, owner)
		if err != nil {
			t.Fatal(err)
		}
		for _, tableName := range settings.Tables {
			tableState, ok := result.State.Tables[tableName]
			if !ok || tableState.VacuumTime.IsZero() || tableState.AnalyzeTime.IsZero() {
				t.Errorf("%s state = %+v, %t", tableName, tableState, ok)
			}
		}
		if result.State.NextTable != settings.Tables[0] || result.State.IndexPassTime.IsZero() {
			t.Fatalf("state = %+v", result.State)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(VacuumHotTablesPost(&VacuumHotTablesArgs{}, result, owner, tx))
		})
		var count int
		var argsJson string
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) OVER (), args_json, run_at
				FROM pending_task
				WHERE run_once_key = '["vacuum_hot_tables"]'
			`).Scan(&count, &argsJson, &runAt))
		})
		var args VacuumHotTablesArgs
		if err := json.Unmarshal([]byte(argsJson), &args); err != nil {
			t.Fatal(err)
		}
		if count != 1 || len(args.State.Tables) != len(settings.Tables) || args.State.NextTable != result.State.NextTable {
			t.Fatalf("%d successor(s) with args %s", count, argsJson)
		}
		// the run took far less than its interval; run_at keeps microseconds
		if want := result.StartTime.Add(settings.Interval); time.Microsecond < runAt.Sub(want).Abs() {
			t.Fatalf("successor runs at %s, want %s", runAt, want)
		}
	})
}
