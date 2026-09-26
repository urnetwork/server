// Timestamp availability requires an eligible queue row and a complete scalar
// observation; malformed input must never resolve the due-lag finding.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Corrupt, missing and oversized primary results are unavailable, not zero.
// Catching the old missing-row panic lets every malformed control run red.
func TestTaskDueLagRejectsInvalidPrimaryRows(t *testing.T) {
	for _, test := range []struct {
		name string
		rows []Row
	}{
		{name: "missing"},
		{name: "empty row", rows: []Row{{}}},
		{name: "extra row", rows: []Row{{"0"}, {"3600"}}},
		{name: "extra column", rows: []Row{{"0", "synthetic-private-cell"}}},
		{name: "empty cell", rows: []Row{{""}}},
		{name: "non numeric", rows: []Row{{"synthetic-private-cell"}}},
		{name: "decimal", rows: []Row{{"180.9"}}},
		{name: "negative", rows: []Row{{"-1"}}},
		{name: "overflow", rows: []Row{{"9223372036854775808"}}},
	} {
		func() {
			defer func() {
				if recover() != nil {
					t.Errorf("%s: missing primary observation panicked", test.name)
				}
			}()
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "greatest(run_at") {
					return test.rows, nil
				}
				return nil, nil
			}}
			alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
			if err == nil || len(alerts) != 0 {
				t.Errorf("%s: invalid primary result returned alerts=%d err=%v", test.name, len(alerts), err)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-private-cell") {
				t.Errorf("%s: parser exposed the source cell", test.name)
			}
		}()
	}
}

// Match the worker's integer availability gate. Future blocks are excluded
// even when their run/lease timestamps are old, without masking eligible work.
func TestTaskDueLagQueryExcludesFutureAvailability(t *testing.T) {
	var query string
	source := &syntheticSource{postgresFn: func(sql string) ([]Row, error) {
		if strings.Contains(sql, "greatest(run_at") {
			query = sql
			return []Row{{"0"}}, nil
		}
		return nil, nil
	}}
	if _, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	if query == "" {
		t.Fatal("primary availability query not executed")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Db(ctx, func(conn server.PgConn) {
			for _, test := range []struct {
				name string
				rows string
				want int64
			}{
				{name: "future availability", rows: "(3600,3600,60)", want: 0},
				{name: "mixed eligible", rows: "(3600,3600,60),(181,181,0)", want: 181},
				{name: "future retry", rows: "(-60,3600,-3600)", want: 0},
				{name: "future lease", rows: "(3600,-60,-3600)", want: 0},
				{name: "threshold", rows: "(180,180,0)", want: 180},
			} {
				fixture := `WITH synthetic_pending_task AS (
				    SELECT now()-make_interval(secs=>run_age) AS run_at,
				           now()-make_interval(secs=>lease_age) AS release_time,
				           floor(extract(epoch FROM now()))::bigint+available_delta AS available_block
				    FROM (VALUES ` + test.rows + `) AS fixture(run_age,lease_age,available_delta)
				) `
				fixtureQuery := fixture + strings.Replace(query, "FROM pending_task", "FROM synthetic_pending_task", 1)
				var lag int64
				server.Raise(conn.QueryRow(ctx, fixtureQuery).Scan(&lag))
				if lag != test.want {
					t.Errorf("%s: lag=%d, want %d", test.name, lag, test.want)
				}
			}
		})
	})
}
