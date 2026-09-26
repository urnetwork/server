// Overdue timestamps are not proof that a session-owned task is unclaimed.
package monitor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Other workers can still make progress while one task keeps its advisory
// owner and misses timestamp refreshes. The alert must not infer a fleet halt.
func TestTaskDueLagDoesNotClaimGlobalHalt(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "greatest(run_at") {
			return []Row{{"3600"}}, nil
		}
		return nil, nil
	}}
	alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-due-lag")
	for _, unsupported := range []string{"due-and-unclaimed", "the task plane is not claiming", "are all stalling"} {
		if strings.Contains(alert.Markdown(), unsupported) {
			t.Fatalf("timestamp-only lag inferred unsupported conclusion %q", unsupported)
		}
	}
}

// Fixed synthetic rows exercise healthy ownership visibility, an unowned
// prefix, truncation and unavailable controls without production identities.
func TestTaskDueOwnershipControls(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, test := range []struct {
		name string
		row  Row
		err  error
		want string
	}{
		{name: "all held", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "2", "3600", "0", "f"}, want: "sampled_advisory_held=2 sampled_without_advisory_owner=0"},
		{name: "unowned", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "0", "0", "3600", "f"}, want: "sampled_advisory_held=0 sampled_without_advisory_owner=2"},
		{name: "mixed", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "1", "3600", "181", "f"}, want: "oldest_held_s=3600 oldest_without_owner_s=181"},
		{name: "queue drained between reads", row: Row{strconv.FormatInt(now.Unix(), 10), "0", "0", "0", "0", "f"}, want: "sampled_due=0"},
		{name: "bounded prefix", row: Row{strconv.FormatInt(now.Unix(), 10), "256", "255", "3600", "181", "t"}, want: "prefix_truncated=true"},
		{name: "missing", want: "due_ownership=unknown"},
		{name: "failed", err: errors.New("synthetic private source error"), want: "due_ownership=unknown"},
		{name: "stale", row: Row{strconv.FormatInt(now.Add(-31*time.Second).Unix(), 10), "2", "2", "3600", "0", "f"}, want: "due_ownership=unknown"},
		{name: "future", row: Row{strconv.FormatInt(now.Add(31*time.Second).Unix(), 10), "2", "2", "3600", "0", "f"}, want: "due_ownership=unknown"},
		{name: "negative", row: Row{strconv.FormatInt(now.Unix(), 10), "-1", "0", "0", "0", "f"}, want: "due_ownership=unknown"},
		{name: "overcount", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "3", "3600", "0", "f"}, want: "due_ownership=unknown"},
		{name: "invalid held age", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "0", "3600", "0", "f"}, want: "due_ownership=unknown"},
		{name: "invalid unowned age", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "2", "3600", "181", "f"}, want: "due_ownership=unknown"},
		{name: "invalid partial", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "2", "3600", "0", "t"}, want: "due_ownership=unknown"},
		{name: "invalid boolean", row: Row{strconv.FormatInt(now.Unix(), 10), "2", "2", "3600", "0", "bad"}, want: "due_ownership=unknown"},
		{name: "overflow", row: Row{strconv.FormatInt(now.Unix(), 10), "9223372036854775808", "0", "0", "0", "f"}, want: "due_ownership=unknown"},
	} {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			switch {
			case query == taskDueOwnershipQuery:
				if test.row == nil {
					return nil, test.err
				}
				return []Row{test.row}, test.err
			case strings.Contains(query, "greatest(run_at"):
				return []Row{{"3600"}}, nil
			default:
				return nil, nil
			}
		}}
		alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		alert := requireAlertClass(t, alerts, "task-due-lag")
		if !strings.Contains(alert.Markdown(), test.want) {
			t.Fatalf("%s: missing ownership qualifier %q", test.name, test.want)
		}
		requireAlertOmits(t, alert, "synthetic private source error", "the task plane is not claiming")
	}
}

// The healthy timestamp control neither queries advisory context nor turns an
// unavailable auxiliary source into a healthy ownership assertion.
func TestTaskDueOwnershipHealthyDoesNotQuery(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if query == taskDueOwnershipQuery {
			t.Fatal("healthy lag requested optional ownership evidence")
		}
		if strings.Contains(query, "greatest(run_at") {
			return []Row{{"180"}}, nil
		}
		return nil, nil
	}}
	alerts, err := NewTaskConvergenceSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("healthy timestamp lag: alerts=%+v err=%v", alerts, err)
	}
}

// Execute the published query against a real local PostgreSQL advisory lock.
// Synthetic pending rows replace only queue input; the key, lock view, source
// clock and future retry/lease exclusion run exactly as in production.
func TestTaskDueOwnershipQueryMatchesRealSessionKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Db(ctx, func(conn server.PgConn) {
			var observed, count, held, heldAge, unownedAge int64
			var partial bool
			server.Raise(conn.QueryRow(ctx, taskDueOwnershipQuery).Scan(&observed, &count, &held, &heldAge, &unownedAge, &partial))
			if count != 0 || held != 0 || heldAge != 0 || unownedAge != 0 || partial {
				t.Fatal("empty local queue did not produce healthy ownership counts")
			}
			for _, highBit := range []byte{0, 0x80} {
				owned := server.NewId()
				owned[0] = (owned[0] & 0x7f) | highBit
				owned[8] &= 0x7f
				key := int64(binary.BigEndian.Uint64(owned[:8]) ^ binary.BigEndian.Uint64(owned[8:]) ^ uint64(0x75726e7461736b31))
				server.Raise(conn.QueryRow(ctx, "SELECT pg_advisory_lock($1)", key).Scan(new(any)))
				fixture := `WITH synthetic_pending_task AS (
				    SELECT task_id,now()-make_interval(secs=>run_age) AS run_at,
				           now()-make_interval(secs=>lease_age) AS release_time,
				           floor(extract(epoch FROM now()))::bigint-least(run_age,lease_age) AS available_block,
				           0 AS run_priority, 60 AS run_max_time_seconds
				    FROM (VALUES ($1::uuid,600,600),($2::uuid,300,300),($3::uuid,-60,3600),($4::uuid,3600,-60))
				         AS fixture(task_id,run_age,lease_age)
				), `
				query := strings.Replace(taskDueOwnershipQuery, "WITH observed", fixture+"observed", 1)
				query = strings.Replace(query, "FROM pending_task", "FROM synthetic_pending_task", 1)
				server.Raise(conn.QueryRow(ctx, query, owned, server.NewId(), server.NewId(), server.NewId()).Scan(&observed, &count, &held, &heldAge, &unownedAge, &partial))
				var unlocked bool
				server.Raise(conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked))
				if count != 2 || held != 1 || heldAge != 600 || unownedAge != 300 || partial || !unlocked {
					t.Fatalf("signed key %d: count=%d held=%d ages=%d/%d partial=%t unlocked=%t", highBit, count, held, heldAge, unownedAge, partial, unlocked)
				}
			}
			for _, total := range []int{256, 257, 1000} {
				fixture := fmt.Sprintf(`WITH synthetic_pending_task AS (
				    SELECT gen_random_uuid() AS task_id,now()-interval '10 minutes' AS run_at,
				           now()-interval '10 minutes' AS release_time,
				           floor(extract(epoch FROM now()))::bigint-600 AS available_block,
				           0 AS run_priority,60 AS run_max_time_seconds FROM generate_series(1,%d)
				), `, total)
				query := strings.Replace(taskDueOwnershipQuery, "WITH observed", fixture+"observed", 1)
				query = strings.Replace(query, "FROM pending_task", "FROM synthetic_pending_task", 1)
				server.Raise(conn.QueryRow(ctx, query).Scan(&observed, &count, &held, &heldAge, &unownedAge, &partial))
				if count != 256 || held != 0 || heldAge != 0 || unownedAge != 600 || partial != (total > 256) {
					t.Fatalf("bounded source total=%d count=%d held=%d ages=%d/%d partial=%t", total, count, held, heldAge, unownedAge, partial)
				}
			}
		})
	})
}
