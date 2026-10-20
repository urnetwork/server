// Structured source controls use the exact embedded collector and only
// private synthetic cluster/index/log files. Low owns all later execution.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This is the actual production exception/output boundary, with private paths
// and deterministic clocks. No alternate parser or predicate is substituted.
const pgAutovacuumCancelNativeHarness = `import runpy,sys
namespace=runpy.run_path(sys.argv[1])
clock=lambda: int(sys.argv[4])*1000
print(namespace["observe"](sys.argv[2],"pg-1",sys.argv[3],clock,lambda: 0))
`

// Index paths contain spaces deliberately. Hooks force source changes at exact
// reads without sleeps; every native child is finite and joined before use.
func pgAutovacuumCancelCollect(t *testing.T, now time.Time, body, harness string) string {
	t.Helper()
	dir := t.TempDir()
	cluster := filepath.Join(dir, "cluster")
	logDir := filepath.Join(cluster, "log directory")
	if os.MkdirAll(logDir, 0700) != nil {
		t.Fatal("cannot create private structured-source directories")
	}
	program, logPath, bootPath := filepath.Join(dir, "collector.py"), filepath.Join(logDir, "synthetic source.json"), filepath.Join(dir, "boot")
	replacement, indexPath := filepath.Join(dir, "replacement.json"), filepath.Join(cluster, "current_logfiles")
	for path, contents := range map[string]string{
		program: pgAutovacuumCancelProgram, logPath: body,
		bootPath:    "00000000-0000-4000-8000-000000000001\n",
		replacement: "private-source-canary replacement\n",
		indexPath:   "stderr ignored plaintext.log\njsonlog log directory/synthetic source.json\n",
	} {
		if os.WriteFile(path, []byte(contents), 0600) != nil {
			t.Fatal("cannot create private structured-source fixture")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-c", harness, program, cluster, bootPath, fmt.Sprint(now.UnixMicro()), replacement, logPath, indexPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("structured cancellation collector did not complete successfully")
	}
	for _, canary := range []string{"private-source-canary", "private-sql-canary", "private-client-canary", logPath, indexPath} {
		if strings.Contains(string(output), canary) {
			t.Fatal("structured projection exposed private source text or paths")
		}
	}
	return string(output)
}

// Model the pinned writer's typed primary/session fields. This encodes fixture
// strings; separate producer qualification must exercise the actual PG18 writer.
func pgAutovacuumCancelNativeRecord(ended time.Time, line int, backend, severity, state, message string) map[string]any {
	started := ended.UTC().Truncate(24 * time.Hour)
	record := map[string]any{
		"timestamp": ended.UTC().Format("2006-01-02 15:04:05.000 UTC"), "pid": 123,
		"session_id": fmt.Sprintf("%x.%x", started.Unix(), 123), "line_num": line,
		"session_start": started.Format("2006-01-02 15:04:05 UTC"),
		"backend_type":  backend, "error_severity": severity, "message": message,
	}
	if state != "" {
		record["state_code"] = state
	}
	return record
}

// Each encoded object ends at one physical LF; escaped values stay in that row.
func pgAutovacuumCancelJsonLine(t testing.TB, record map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal("cannot encode private structured log fixture")
	}
	return string(encoded) + "\n"
}

// A preceding ordinary record gives retained observed history, not logger
// liveness. Its schema uses the writer's omitted successful SQLSTATE.
func pgAutovacuumCancelOldJson(t testing.TB, now time.Time) string {
	record := pgAutovacuumCancelNativeRecord(now.Add(-16*time.Minute), 1, "checkpointer", "LOG", "", "checkpoint complete")
	record["pid"] = 122
	record["session_id"] = fmt.Sprintf("%x.%x", now.Add(-16*time.Minute).UTC().Truncate(24*time.Hour).Unix(), 122)
	return pgAutovacuumCancelJsonLine(t, record)
}

// Only all exact primary fields qualify. A client-generated identical message
// or a plaintext SQLSTATE prefix is a different record, not worker authority.
func TestPgAutovacuumCancelNativeExactPrimaryFields(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, backend, severity, state, message string
		want, unknown                           bool
	}{
		{name: "worker", backend: "autovacuum worker", severity: "ERROR", state: "57014", message: "canceling autovacuum task", want: true},
		{name: "client imitation", backend: "client backend", severity: "ERROR", state: "57014", message: "canceling autovacuum task", want: false},
		{name: "launcher", backend: "autovacuum launcher", severity: "ERROR", state: "57014", message: "canceling autovacuum task", want: false},
		{name: "other severity", backend: "autovacuum worker", severity: "LOG", state: "57014", message: "canceling autovacuum task", want: false},
		{name: "native debug", backend: "client backend", severity: "DEBUG", state: "", message: "debug context", want: false},
		{name: "unsupported debug", backend: "client backend", severity: "DEBUG1", state: "", message: "debug context", want: false, unknown: true},
		{name: "other state", backend: "autovacuum worker", severity: "ERROR", state: "57015", message: "canceling autovacuum task", want: false},
		{name: "plaintext prefix", backend: "autovacuum worker", severity: "ERROR", state: "57014", message: "57014: canceling autovacuum task", want: false, unknown: true},
		{name: "substring", backend: "autovacuum worker", severity: "ERROR", state: "57014", message: "private-sql-canary canceling autovacuum task", want: false, unknown: true},
		{name: "unsupported locale", backend: "autovacuum worker", severity: "ERROR", state: "57014", message: "private-source-canary unsupported translation", want: false, unknown: true},
	} {
		body := pgAutovacuumCancelOldJson(t, now) + pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, c.backend, c.severity, c.state, c.message))
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
		if (requireAlertClassCount(alerts, "pg-autovacuum-cancel-observed") == 1) != c.want ||
			(requireAlertClassCount(alerts, "pg-autovacuum-cancel-unobservable") == 1) != c.unknown {
			t.Fatalf("%s: structured primary-field origin predicate changed", c.name)
		}
		pgAutovacuumCancelRequirePrivate(t, alerts)
	}
}

// The complete source counterexample enters through identity-prefix bytes,
// not SQL continuations. JSON escaping keeps those bytes inside one value.
func TestPgAutovacuumCancelStructuredIdentityForgeryCannotCreateRecord(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	forged := now.Add(-time.Minute).Format("2006-01-02 15:04:05.000") + " UTC [123] ERROR:  canceling autovacuum task"
	fakeJson := pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task"))
	for _, field := range []string{"user", "dbname", "statement", "internal_query", "context", "detail", "message", "application_name"} {
		record := pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "client backend", "FATAL", "28000", "missing startup identity")
		record[field] = "private-client-canary LOG:  harmless\n" + forged + "\n" + fakeJson + "private-sql-canary\t\r\\\""
		body := pgAutovacuumCancelOldJson(t, now) + pgAutovacuumCancelJsonLine(t, record)
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
		if len(alerts) != 0 {
			t.Fatalf("%s: escaped identity/body text became a structured cancellation record", field)
		}
	}
	// The same physical plaintext forgery is unsupported input, never a fallback.
	body := pgAutovacuumCancelOldJson(t, now) + forged + "\n"
	alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
	if len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
		t.Fatal("plaintext record regained structured-origin authority")
	}
}

// Duplicate primary keys cannot collapse to the attacker's later value. Wrong
// typed, missing or conflicting writer/session fields remain unknown/private.
func TestPgAutovacuumCancelStructuredMalformedFields(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, edit := range []func(map[string]any){
		func(record map[string]any) { delete(record, "backend_type") },
		func(record map[string]any) { delete(record, "state_code") },
		func(record map[string]any) { delete(record, "message") },
		func(record map[string]any) { record["pid"] = true },
		func(record map[string]any) { record["pid"] = "123" },
		func(record map[string]any) { record["pid"] = -1 },
		func(record map[string]any) { record["pid"] = 124 },
		func(record map[string]any) { record["pid"] = 2147483648 },
		func(record map[string]any) { record["pid"] = nil },
		func(record map[string]any) { record["line_num"] = 0 },
		func(record map[string]any) { record["session_id"] = "private-source-canary" },
		func(record map[string]any) { record["session_start"] = now.Format("2006-01-02 15:04:05 UTC") },
		func(record map[string]any) { record["timestamp"] = "2026-10-19 11:59:00.000 PST" },
		func(record map[string]any) { record["timestamp"] = "2026-02-30 11:59:00.000 UTC" },
		func(record map[string]any) {
			record["message"] = map[string]any{"message": "canceling autovacuum task"}
		},
		func(record map[string]any) { record["state_code"] = 57014 },
		func(record map[string]any) { record["error_severity"] = "private-source-canary" },
	} {
		record := pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task")
		edit(record)
		body := pgAutovacuumCancelOldJson(t, now) + pgAutovacuumCancelJsonLine(t, record)
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
		if len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatal("malformed structured primary fields supplied event/quiet authority")
		}
		pgAutovacuumCancelRequirePrivate(t, alerts)
	}
	valid := pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task"))
	for _, field := range []string{"timestamp", "pid", "error_severity", "message", "backend_type", "state_code", "session_id", "line_num"} {
		body := pgAutovacuumCancelOldJson(t, now) + strings.Replace(valid, "{", "{\""+field+"\":\"private-source-canary\",", 1)
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
		if len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatalf("%s: duplicate JSON key was collapsed", field)
		}
	}
	for _, raw := range []string{
		strings.Replace(valid, `"pid":123`, `"pid":123.0`, 1),
		strings.Replace(valid, `"pid":123`, `"pid":NaN`, 1),
		strings.Replace(valid, `"pid":123`, `"pid":Infinity`, 1),
		"[" + strings.TrimSuffix(valid, "\n") + "]\n", "null\n",
	} {
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, pgAutovacuumCancelOldJson(t, now)+raw, pgAutovacuumCancelNativeHarness))
		if len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatal("ambiguous JSON type supplied event authority")
		}
	}
}

// A repeated process line cannot become a new target event by changing its
// primary fields. A separate already-qualified record survives as one event.
func TestPgAutovacuumCancelStructuredSessionIdentity(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, firstWorker := range []bool{false, true} {
		backend := "client backend"
		if firstWorker {
			backend = "autovacuum worker"
		}
		first := pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, backend, "ERROR", "57014", "canceling autovacuum task")
		repeated := pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task")
		body := pgAutovacuumCancelOldJson(t, now) + pgAutovacuumCancelJsonLine(t, first) + pgAutovacuumCancelJsonLine(t, repeated)
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
		if !strings.Contains(requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").Observed, "record-session") ||
			(requireAlertClassCount(alerts, "pg-autovacuum-cancel-observed") == 1) != firstWorker {
			t.Fatal("duplicate structured session line manufactured or erased origin authority")
		}
		if firstWorker && !strings.Contains(requireAlertClass(t, alerts, "pg-autovacuum-cancel-observed").Observed, "cancellation_records_at_least=1") {
			t.Fatal("duplicate structured session line was counted twice")
		}
	}
}

// Missing and recent-only JSON histories stay unknown. Independently complete
// records survive separate malformed, partial, future or out-of-order objects.
func TestPgAutovacuumCancelNativeHistoryAndPartialPositives(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := pgAutovacuumCancelOldJson(t, now)
	event := pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task"))
	for _, body := range []string{"", event, old + event + "private-source-canary malformed\n", old + event + "private-source-canary partial",
		old + event + pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-2*time.Minute), 3, "checkpointer", "LOG", "", "checkpoint complete")),
		old + event + pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(time.Minute), 3, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task")),
	} {
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
		if !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatal("incomplete JSON history supplied recovery")
		}
		expected := 1
		if body == "" {
			expected = 0
		}
		if requireAlertClassCount(alerts, "pg-autovacuum-cancel-observed") != expected {
			t.Fatal("incomplete JSON history erased a separately qualified record")
		}
		if expected != 0 && !strings.Contains(requireAlertClass(t, alerts, "pg-autovacuum-cancel-observed").Observed, "cancellation_records_at_least=1") {
			t.Fatal("future JSON record was counted in the current window")
		}
		pgAutovacuumCancelRequirePrivate(t, alerts)
	}
}

// A valid index selects exactly the named JSON file, including spaces/absolute
// paths. A missing/unreadable/ambiguous/untrusted index never reads plaintext.
func TestPgAutovacuumCancelStructuredDiscoveryAuthority(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	body := pgAutovacuumCancelOldJson(t, now)
	for _, setup := range []string{
		"os.remove(sys.argv[7])",
		"open(sys.argv[7],'w').write('stderr ignored plaintext.log\\n')",
		"open(sys.argv[7],'w').write('jsonlog log directory/synthetic source.json\\njsonlog log directory/synthetic source.json\\n')",
		"open(sys.argv[7],'w').write('jsonlog log directory/synthetic source.json')",
		"open(sys.argv[7],'w').write('jsonlog '+('x'*8192)+'\\n')",
		"os.chmod(sys.argv[7],0o666)",
		"os.chmod(sys.argv[6],0o666)",
		"os.chmod(sys.argv[2],0o777)",
		"os.remove(sys.argv[7]); os.symlink(sys.argv[6],sys.argv[7])",
		"os.remove(sys.argv[6]); os.symlink(sys.argv[5],sys.argv[6])",
		"os.remove(sys.argv[6]); os.mkfifo(sys.argv[6],0o600)",
	} {
		harness := "import os,runpy,sys\nnamespace=runpy.run_path(sys.argv[1])\n" + setup + "\nclock=lambda: int(sys.argv[4])*1000\nprint(namespace['observe'](sys.argv[2],'pg-1',sys.argv[3],clock,lambda: 0))\n"
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, harness))
		if len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatal("unqualified JSON source supplied quiet authority or fallback")
		}
	}
	absolute := "import os,runpy,sys\nnamespace=runpy.run_path(sys.argv[1])\nopen(sys.argv[7],'w').write('jsonlog '+sys.argv[6]+'\\n')\nclock=lambda: int(sys.argv[4])*1000\nprint(namespace['observe'](sys.argv[2],'pg-1',sys.argv[3],clock,lambda: 0))\n"
	if alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, absolute)); len(alerts) != 0 {
		t.Fatal("exact trusted absolute JSON path with spaces was rejected")
	}
}

// Stable old-file records remain observations when the index or selected path
// rotates; changed read content cannot supply origin or healthy recovery.
func TestPgAutovacuumCancelNativeRotationAndRewrite(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	body := pgAutovacuumCancelOldJson(t, now) + pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task"))
	for _, c := range []struct{ change, reason string }{
		{change: "os.replace(sys.argv[5],sys.argv[6])", reason: "path-changed"},
		{change: "open(sys.argv[7],'w').write('jsonlog '+sys.argv[5]+'\\n')", reason: "source-index-changed"},
	} {
		harness := "import os,runpy,sys\nnamespace=runpy.run_path(sys.argv[1])\ncalls=0\ndef clock():\n    global calls\n    calls+=1\n    if calls==2:\n        " + c.change + "\n    return int(sys.argv[4])*1000\nprint(namespace['observe'](sys.argv[2],'pg-1',sys.argv[3],clock,lambda: 0))\n"
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, harness))
		requireAlertClass(t, alerts, "pg-autovacuum-cancel-observed")
		if !strings.Contains(requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").Observed, c.reason) {
			t.Fatal("changed structured-source selection supplied healthy recovery")
		}
	}
	rewrite := `import os,runpy,sys
namespace=runpy.run_path(sys.argv[1])
original_read=os.read
changed=False
def read(descriptor,size):
    global changed
    result=original_read(descriptor,size)
    if not changed and os.fstat(descriptor).st_ino==os.stat(sys.argv[6]).st_ino:
        changed=True
        with open(sys.argv[6],"wb") as target:
            target.write(b"private-source-canary rewritten\n")
    return result
namespace["collect"].__globals__["os"].read=read
clock=lambda: int(sys.argv[4])*1000
print(namespace["observe"](sys.argv[2],"pg-1",sys.argv[3],clock,lambda: 0))
`
	alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, rewrite))
	if len(alerts) != 1 || !strings.Contains(requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").Observed, "source-snapshot-changed") {
		t.Fatal("rewritten JSON snapshot supplied record authority")
	}
}

// Exact record and field limits bound JSON allocation, while a separately
// qualified event survives an over-limit record without claiming full history.
func TestPgAutovacuumCancelStructuredRecordBounds(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := pgAutovacuumCancelOldJson(t, now)
	event := pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task"))
	for _, limit := range []string{"record", "fields"} {
		for _, extra := range []int{0, 1} {
			record := pgAutovacuumCancelNativeRecord(now.Add(-2*time.Minute), 1, "client backend", "LOG", "", "")
			record["pid"] = 124
			record["session_id"] = fmt.Sprintf("%x.%x", now.UTC().Truncate(24*time.Hour).Unix(), 124)
			if limit == "record" {
				initial := pgAutovacuumCancelJsonLine(t, record)
				record["message"] = strings.Repeat("x", 1048576-(len(initial)-1)+extra)
			} else {
				for len(record) < 64+extra {
					record[fmt.Sprintf("optional_%d", len(record))] = "private-client-canary"
				}
			}
			body := old + pgAutovacuumCancelJsonLine(t, record) + event
			alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, body, pgAutovacuumCancelNativeHarness))
			if requireAlertClassCount(alerts, "pg-autovacuum-cancel-observed") != 1 ||
				(requireAlertClassCount(alerts, "pg-autovacuum-cancel-unobservable") == 1) != (extra == 1) {
				t.Fatalf("%s extra=%d: structured record bound changed event or coverage authority", limit, extra)
			}
			pgAutovacuumCancelRequirePrivate(t, alerts)
		}
	}
}

// The unchanged forty-megabyte and hundred-thousand-row ceilings are exercised
// without changing production constants. Partial populations never prove quiet.
func TestPgAutovacuumCancelNativeResourceBounds(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := pgAutovacuumCancelOldJson(t, now)
	event := pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), 2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task"))
	large := pgAutovacuumCancelNativeRecord(now.Add(-2*time.Minute), 1, "client backend", "LOG", "", strings.Repeat("x", pgAutovacuumCancelMaxBytes))
	alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelCollect(t, now, old+pgAutovacuumCancelJsonLine(t, large)+event, pgAutovacuumCancelNativeHarness))
	requireAlertClass(t, alerts, "pg-autovacuum-cancel-observed")
	if !strings.Contains(requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").Observed, "tail-capped") {
		t.Fatal("structured byte cap supplied complete history")
	}
	var body strings.Builder
	body.WriteString(old)
	for index := 0; index <= pgAutovacuumCancelMaxRows; index++ {
		body.WriteString(pgAutovacuumCancelJsonLine(t, pgAutovacuumCancelNativeRecord(now.Add(-time.Minute), index+2, "autovacuum worker", "ERROR", "57014", "canceling autovacuum task")))
	}
	payload := pgAutovacuumCancelCollect(t, now, body.String(), pgAutovacuumCancelNativeHarness)
	if len(payload) > pgAutovacuumCancelMaxOutput {
		t.Fatal("structured output escaped its byte ceiling")
	}
	alerts = pgAutovacuumCancelRun(t, now, payload)
	requireAlertClass(t, alerts, "pg-autovacuum-cancel-observed")
	if !strings.Contains(requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").Observed, "row-cap") {
		t.Fatal("structured row cap supplied complete history")
	}
}

// The real hostname guard still runs before Python/index discovery. Private
// executables make mismatch finite without touching any cluster or system log.
func TestPgAutovacuumCancelNativeHostGuard(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"hostname": "#!/bin/sh\nprintf '%s\\n' 'wrong-synthetic-host'\n",
		"timeout":  "#!/bin/sh\nexit 98\n",
	} {
		if os.WriteFile(filepath.Join(dir, name), []byte(body), 0700) != nil {
			t.Fatal("cannot prepare private structured-source host guard")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", pgAutovacuumCancelCommand("pg-1"))
	command.Env = []string{"PATH=" + dir}
	output, err := command.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 74 || len(output) != 0 {
		t.Fatal("wrong host invoked structured reader or exposed source text")
	}
}
