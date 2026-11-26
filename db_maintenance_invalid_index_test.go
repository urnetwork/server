// The hot vacuum chain reports how long an invalid index has lingered.
package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// A gauge of the default registry by name, and whether it is registered.
func testDbInvalidIndexGauge(t testing.TB, name string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) == 1 && family.GetMetric()[0].GetGauge() != nil {
			return family.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// The run's report of the invalid indexes: how many, the oldest's age, and
// when it listed them.
func testDbInvalidIndexReport(t testing.TB) (count float64, maxAge float64, sampled float64) {
	t.Helper()
	var ok [3]bool
	count, ok[0] = testDbInvalidIndexGauge(t, "urnetwork_db_invalid_indexes")
	maxAge, ok[1] = testDbInvalidIndexGauge(t, "urnetwork_db_invalid_index_max_age_seconds")
	sampled, ok[2] = testDbInvalidIndexGauge(t, "urnetwork_db_invalid_index_sample_timestamp_seconds")
	if !ok[0] || !ok[1] || !ok[2] {
		t.Fatalf("the hot vacuum run reports no invalid index count, age or sample time (registered %v)", ok)
	}
	return
}

// The settings of a run that only lists the invalid indexes: no table to
// vacuum and no index pass.
func testDbInvalidIndexSettings() *DbHotVacuumSettings {
	settings := DefaultDbHotVacuumSettings()
	settings.Tables = nil
	settings.IndexPassInterval = 1000 * time.Hour
	return settings
}

// On Main (2026-11-26) a _ccnew copy left by an interrupted REINDEX
// CONCURRENTLY stayed invalid with 129 MB and nothing reported it. Here a
// unique build over duplicate values fails and leaves its index invalid, and
// the chain's state, as its task arguments carry it, says a run first found it
// three hours ago. Each run must report the index and an age of at least three
// hours, keep the time in the state for the next run, and once the index is
// dropped report it gone. Before the change no run reported anything.
func TestHotVacuumReportsHowLongAnInvalidIndexLingers(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := testDbInvalidIndexSettings()
		VacuumHotTables(ctx, DbHotVacuumState{}, settings)
		baseline, _, _ := testDbInvalidIndexReport(t)

		var oid int64
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE synthetic_invalid_index (value integer NOT NULL)`))
			RaisePgResult(conn.Exec(ctx, `INSERT INTO synthetic_invalid_index (value) VALUES (1), (1)`))
			if _, err := conn.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY synthetic_invalid_index_value ON synthetic_invalid_index (value)`); err == nil {
				t.Fatal("the unique build over duplicate values succeeded")
			}
			var valid bool
			Raise(conn.QueryRow(ctx, `SELECT indexrelid::bigint, indisvalid FROM pg_index WHERE indexrelid = to_regclass('public.synthetic_invalid_index_value')`).Scan(&oid, &valid))
			if valid {
				t.Fatal("the failed build left a valid index")
			}
		}, OptReadWrite(), OptNoRetry())

		firstFound := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
		var state DbHotVacuumState
		Raise(json.Unmarshal([]byte(fmt.Sprintf(`{"invalid_indexes":{"%d":{"name":"public.synthetic_invalid_index_value","table":"synthetic_invalid_index","since":%q}}}`,
			oid, firstFound.Format(time.RFC3339Nano))), &state))
		for run := range 2 {
			before := float64(time.Now().UnixNano()) / float64(time.Second)
			state = VacuumHotTables(ctx, state, settings)
			count, maxAge, sampled := testDbInvalidIndexReport(t)
			if count != baseline+1 || maxAge < (3*time.Hour).Seconds() || sampled < before {
				t.Fatalf("run %d reported %.0f invalid indexes (%.0f before the build), the oldest invalid for %.0f s, sampled at %.0f: want the failed build's index, invalid for three hours, sampled now",
					run, count, baseline, maxAge, sampled)
			}
			raw, err := json.Marshal(state)
			Raise(err)
			if want := fmt.Sprintf(`"%d":{"name":"public.synthetic_invalid_index_value","table":"synthetic_invalid_index","since":%q}`, oid, firstFound.Format(time.RFC3339Nano)); !strings.Contains(string(raw), want) {
				t.Fatalf("run %d did not carry the index's first time in its state %s, want %s", run, raw, want)
			}
		}

		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `DROP INDEX synthetic_invalid_index_value`))
		}, OptReadWrite(), OptNoRetry())
		state = VacuumHotTables(ctx, state, settings)
		count, maxAge, _ := testDbInvalidIndexReport(t)
		raw, err := json.Marshal(state)
		Raise(err)
		if count != baseline || (baseline == 0 && maxAge != 0) || strings.Contains(string(raw), fmt.Sprintf(`"%d"`, oid)) {
			t.Fatalf("after the drop the run reported %.0f invalid indexes, the oldest for %.0f s, state %s", count, maxAge, raw)
		}
	})
}
