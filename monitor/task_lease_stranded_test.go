// The stranded-lease probe keeps the five minutes after a silent claim's last
// heartbeat, although heartbeats now extend the timestamp lease only at half.
package monitor

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The probe's own query against synthetic rows: a stalled claim is stranded
// whether or not its timestamp has lapsed, until five minutes after its last
// heartbeat; a live heartbeat, a handed-back row and a claim silent for longer
// are not, and an older claim's longer lease still counts.
func TestTaskLeaseStrandedQueryKeepsTheFiveMinuteWindow(t *testing.T) {
	var query string
	source := &syntheticSource{postgresFn: func(sql string) ([]Row, error) {
		switch {
		case strings.Contains(sql, "claim_time <"):
			query = sql
			return nil, nil
		case strings.Contains(sql, "greatest(run_at"):
			return []Row{{"0"}}, nil
		}
		return nil, nil
	}}
	if _, err := NewStuckLeasesSignal().Run(context.Background(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	if query == "" {
		t.Fatal("stranded-lease query not executed")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// Seconds since the last heartbeat, and seconds left on the lease.
		fixture := `WITH synthetic_pending_task AS (
		    SELECT function_name,
		           now()-make_interval(secs=>silent) AS claim_time,
		           now()+make_interval(secs=>lease) AS release_time,
		           3600 AS run_max_time_seconds
		    FROM (VALUES
		        ('synthetic.task.LiveHeartbeat',5,200),
		        ('synthetic.task.StalledWithLease',180,60),
		        ('synthetic.task.StalledLapsed',200,-50),
		        ('synthetic.task.HandedBack',180,-175),
		        ('synthetic.task.SilentTooLong',320,-170),
		        ('synthetic.task.OlderClaim',600,3000)
		    ) AS fixture(function_name,silent,lease)
		) `
		var stranded []string
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, fixture+strings.Replace(query, "FROM pending_task", "FROM synthetic_pending_task", 1))
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var task string
					var silent, remaining, maxTime int
					server.Raise(rows.Scan(&task, &silent, &remaining, &maxTime))
					stranded = append(stranded, task)
				}
			})
		})
		slices.Sort(stranded)
		if want := []string{"OlderClaim", "StalledLapsed", "StalledWithLease"}; !slices.Equal(stranded, want) {
			t.Fatal("stranded-lease window changed", stranded, want)
		}
	})
}
