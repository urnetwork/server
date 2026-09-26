// Task queue lock evidence must remain separate from generic transaction age.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The catalog must not advertise a task-lock detector that is absent at runtime.
func TestTaskLockChainSignalRegistered(t *testing.T) {
	for _, signal := range NewSignals() {
		if signal.Key() == "task-lock-chain" {
			if signal.Number() != "1.3e" {
				t.Fatal("task lock signal has the wrong catalog section")
			}
			return
		}
	}
	t.Fatal("task-lock-chain signal is not registered")
}

// Synthetic backend identities and a documentation-only address never contain
// production ownership or customer data.
func taskLockTestEdge(now time.Time, wait, idle int64) taskLockEdge {
	xact := idle + 30
	visible := true
	return taskLockEdge{
		BlockedPid: 101, BlockerPid: 202,
		BlockedBackendStart: now.Add(-time.Hour).Unix(), BlockerBackendStart: now.Add(-time.Hour).Unix(),
		WaitSeconds: &wait, IdleSeconds: &idle, XactSeconds: &xact,
		BlockerState: "idle in transaction", BlockerAddress: "192.0.2.51", BlockerVisible: &visible,
	}
}

// A bounded aggregate row carries only the selected direct edge details.
func taskLockTestRows(t testing.TB, now time.Time, writers, count int, edges []taskLockEdge) []Row {
	t.Helper()
	encoded, err := json.Marshal(edges)
	if err != nil {
		t.Fatal(err)
	}
	return []Row{{strconv.FormatInt(now.Unix(), 10), "t", strconv.Itoa(writers), strconv.Itoa(count), string(encoded)}}
}

// The source cannot silently substitute query age, logs, or a different host.
func taskLockTestSettings(t testing.TB, rows []Row, sourceErr error) SignalSettings {
	t.Helper()
	return syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if query != taskLockChainQuery {
			t.Fatal("task lock source escaped its fixed direct query")
		}
		return rows, sourceErr
	}})
}

// Both durations must cross their boundary. A long active transaction, harmless
// idle session, or short actual lock wait does not imply an abandoned task.
func TestTaskLockChainThresholdsAndHealthyControls(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, test := range []struct {
		name     string
		wait     int64
		idle     int64
		state    string
		severity Severity
	}{
		{name: "short wait", wait: 59, idle: 1800, state: "idle in transaction"},
		{name: "recent idle", wait: 1800, idle: 59, state: "idle in transaction"},
		{name: "warn boundary", wait: 60, idle: 60, state: "idle in transaction", severity: SeverityWarn},
		{name: "below page wait", wait: 299, idle: 1800, state: "idle in transaction", severity: SeverityWarn},
		{name: "below page idle", wait: 1800, idle: 299, state: "idle in transaction", severity: SeverityWarn},
		{name: "page boundary", wait: 300, idle: 300, state: "idle in transaction", severity: SeverityPage},
		{name: "active blocker", wait: 1800, state: "active"},
		{name: "idle without transaction", wait: 1800, state: "idle"},
	} {
		edge := taskLockTestEdge(now, test.wait, test.idle)
		edge.BlockerState = test.state
		alerts, err := NewTaskLockChainSignal().Run(context.Background(), taskLockTestSettings(t, taskLockTestRows(t, now, 1, 1, []taskLockEdge{edge}), nil))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.severity == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: harmless edge alerted: %+v", test.name, alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "task-write-idle-blocker")
		if len(alerts) != 1 || alert.Severity != test.severity || alert.Sustain != 2 {
			t.Fatalf("%s: wrong finding: %+v", test.name, alerts)
		}
		for _, want := range []string{"complete=true", "Query age is not lock-wait age", "operator authorization", "does not prove a dead worker", "blocker_backend_start="} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("%s: missing qualifier %q", test.name, want)
			}
		}
		requireAlertOmits(t, alert, edge.BlockerAddress)
	}
	alerts, err := NewTaskLockChainSignal().Run(context.Background(), taskLockTestSettings(t, taskLockTestRows(t, now, 0, 0, []taskLockEdge{}), nil))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("empty complete source: alerts=%+v err=%v", alerts, err)
	}
}

// Invalid or raced evidence must not resolve a prior blocked-write finding.
func TestTaskLockChainUnavailableDoesNotClearFault(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, name := range []string{
		"transport", "missing", "columns", "stale", "future", "visibility", "bad timestamp", "negative writers", "negative count",
		"missing array", "null array", "trailing data", "unknown field", "wrong detail count", "no edge for writer",
		"missing wait", "negative wait", "missing idle", "missing xact", "missing visibility", "raced blocker",
		"bad pid", "self blocker", "future backend", "idle exceeds xact", "invalid state", "active with idle age",
		"duplicate edge", "unrepresented writer", "excess represented writers",
	} {
		edge := taskLockTestEdge(now, 300, 300)
		rows := taskLockTestRows(t, now, 1, 1, []taskLockEdge{edge})
		var sourceErr error
		switch name {
		case "transport":
			sourceErr = errors.New("synthetic private remote detail")
		case "missing":
			rows = nil
		case "columns":
			rows[0] = rows[0][:4]
		case "stale":
			rows[0][0] = strconv.FormatInt(now.Add(-31*time.Second).Unix(), 10)
		case "future":
			rows[0][0] = strconv.FormatInt(now.Add(31*time.Second).Unix(), 10)
		case "visibility":
			rows[0][1] = "f"
		case "bad timestamp":
			rows[0][0] = "invalid"
		case "negative writers":
			rows[0][2] = "-1"
		case "negative count":
			rows[0][3] = "-1"
		case "missing array":
			rows[0][4] = ""
		case "null array":
			rows[0][4] = "null"
		case "trailing data":
			rows[0][4] += " {}"
		case "unknown field":
			rows[0][4] = strings.Replace(rows[0][4], "blocked_pid", "synthetic_secret_field", 1)
		case "wrong detail count":
			rows[0][3] = "2"
		case "no edge for writer":
			rows = taskLockTestRows(t, now, 1, 0, []taskLockEdge{})
		case "unrepresented writer":
			other := edge
			other.BlockerPid = 203
			rows = taskLockTestRows(t, now, 2, 2, []taskLockEdge{edge, other})
		case "duplicate edge":
			rows = taskLockTestRows(t, now, 1, 2, []taskLockEdge{edge, edge})
		case "excess represented writers":
			rows[0][2] = "0"
		default:
			switch name {
			case "missing wait":
				edge.WaitSeconds = nil
			case "negative wait":
				*edge.WaitSeconds = -1
			case "missing idle":
				edge.IdleSeconds = nil
			case "missing xact":
				edge.XactSeconds = nil
			case "missing visibility":
				edge.BlockerVisible = nil
			case "raced blocker":
				*edge.BlockerVisible = false
				edge.BlockerPid = 0
				edge.BlockerBackendStart = 0
				edge.BlockerState = "unavailable"
			case "bad pid":
				edge.BlockedPid = 0
			case "self blocker":
				edge.BlockerPid = edge.BlockedPid
			case "future backend":
				edge.BlockerBackendStart = now.Add(time.Minute).Unix()
			case "idle exceeds xact":
				*edge.XactSeconds = 1
			case "invalid state":
				edge.BlockerState = "synthetic private remote detail"
			case "active with idle age":
				edge.BlockerState = "active"
			}
			rows = taskLockTestRows(t, now, 1, 1, []taskLockEdge{edge})
		}
		env, err := newProbeEnv(taskLockTestSettings(t, rows, sourceErr).withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (taskLockChainProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		unavailable := false
		for _, finding := range findings {
			if finding.class == "task-write-idle-blocker" && finding.healthy {
				t.Fatalf("%s: incomplete evidence cleared prior fault", name)
			}
			unavailable = unavailable || finding.class == "task-lock-chain-unavailable" && !finding.healthy
			if strings.Contains(fmt.Sprintf("%+v", finding), "synthetic private remote detail") {
				t.Fatalf("%s: source details leaked", name)
			}
		}
		if !unavailable {
			t.Fatalf("%s: missing independent unknown finding", name)
		}
	}
}

// Capped coverage preserves confirmed faults, but cannot assert fleet recovery.
func TestTaskLockChainPartialCoverageRetainsConfirmedBlocker(t *testing.T) {
	now := syntheticSettings(nil).Now()
	edges := []taskLockEdge{}
	for index := range 32 {
		edge := taskLockTestEdge(now, int64(300+index), int64(300+index))
		edge.BlockedPid += int64(index)
		edges = append(edges, edge)
	}
	alerts, err := NewTaskLockChainSignal().Run(context.Background(), taskLockTestSettings(t, taskLockTestRows(t, now, 33, 33, edges), nil))
	if err != nil || len(alerts) != 2 {
		t.Fatalf("partial source: alerts=%+v err=%v", alerts, err)
	}
	requireAlertClass(t, alerts, "task-lock-chain-unavailable")
	alert := requireAlertClass(t, alerts, "task-write-idle-blocker")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "wait_s=331") || !strings.Contains(alert.Observed, "complete=false") {
		t.Fatalf("confirmed worst edge was suppressed: %+v", alert)
	}
}

// Executes the actual SQL on PostgreSQL, then substitutes only volatile system
// views and the blocker function to force exact lock evidence without sleeps.
func TestTaskLockChainQueryDirectEdgesAndWriteForms(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Db(ctx, func(conn server.PgConn) {
			var observed int64
			var visible bool
			var writers, count int
			var encoded []byte
			server.Raise(conn.QueryRow(ctx, taskLockChainQuery).Scan(&observed, &visible, &writers, &count, &encoded))
			if writers != 0 || count != 0 || string(encoded) != "[]" {
				t.Fatal("fresh local runtime source is not complete and unlocked")
			}
			for _, test := range []struct {
				query string
				want  bool
			}{
				{query: "\n INSERT INTO pending_task (task_id) VALUES ($1)", want: true},
				{query: "update public.pending_task SET claim_time=$1", want: true},
				{query: "DELETE FROM pending_task WHERE task_id=$1", want: true},
				{query: "SELECT * FROM pending_task"},
				{query: "UPDATE pending_task_history SET value=$1"},
				{query: "WITH old AS (SELECT * FROM pending_task) UPDATE pending_task SET value=$1"},
			} {
				var matched bool
				server.Raise(conn.QueryRow(ctx, `SELECT $1::text ~* $2::text`, test.query, taskLockWritePattern).Scan(&matched))
				if matched != test.want {
					t.Fatalf("write form %q: matched=%t", test.query, matched)
				}
			}
			fixture := `WITH synthetic_activity(pid,backend_start,xact_start,state_change,state,wait_event_type,query,client_addr) AS (
				VALUES (101,now()-interval '1 hour',now()-interval '20 minutes',now()-interval '15 minutes','active','Lock','INSERT INTO pending_task (task_id) VALUES ($1)','192.0.2.10'::inet),
				       (202,now()-interval '1 hour',now()-interval '11 minutes',now()-interval '10 minutes','idle in transaction','Client','UPDATE pending_task SET claim_time=$1','192.0.2.51'::inet),
				       (303,now()-interval '1 hour',now()-interval '20 minutes',now()-interval '15 minutes','active','Lock','SELECT * FROM pending_task','192.0.2.11'::inet)
			), synthetic_locks(pid,relation,mode,granted,waitstart) AS (
				VALUES (101,'public.pending_task'::regclass::oid,'RowExclusiveLock',true,NULL::timestamptz),
				       (101,NULL::oid,'ShareLock',false,now()-interval '2 minutes'),
				       (303,'public.pending_task'::regclass::oid,'RowExclusiveLock',true,NULL::timestamptz),
				       (303,NULL::oid,'ShareLock',false,now()-interval '15 minutes')
			), `
			query := strings.Replace(taskLockChainQuery, "WITH observed", fixture+"observed", 1)
			query = strings.ReplaceAll(query, "FROM pg_stat_activity WHERE datname = current_database()", "FROM synthetic_activity")
			query = strings.ReplaceAll(query, "FROM pg_locks", "FROM synthetic_locks")
			query = strings.ReplaceAll(query, "pg_blocking_pids(writer.pid)", "ARRAY[202]")
			server.Raise(conn.QueryRow(ctx, query).Scan(&observed, &visible, &writers, &count, &encoded))
			var edges []taskLockEdge
			server.Raise(json.Unmarshal(encoded, &edges))
			if writers != 1 || count != 1 || len(edges) != 1 || edges[0].BlockerPid != 202 || *edges[0].WaitSeconds != 120 || *edges[0].IdleSeconds != 600 {
				t.Fatalf("direct source mixed query age, read-only statement or blocker: writers=%d edges=%s", writers, encoded)
			}
		})
	})
}
