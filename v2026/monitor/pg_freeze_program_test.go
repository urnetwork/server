// Native controls exercise the exact embedded collector against private
// synthetic files. Sol Low owns execution; these tests never read Main.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The imported function is the production collector; only source path, boot
// fixture and deterministic clocks vary. No parser or threshold is replicated.
const pgFreezeNativeHarness = `import json,os,runpy,sys
namespace=runpy.run_path(sys.argv[1])
clock=lambda: int(sys.argv[4])*1000
result=namespace["collect"](sys.argv[2],"pg-1",sys.argv[3],clock,lambda: 0)
print(json.dumps(result,separators=(",",":")))
`

// Each native child has a finite context and is joined before its result is
// admitted. Private SQL canaries must never appear in either output channel.
func pgFreezeCollectForTest(t *testing.T, now time.Time, body string) string {
	t.Helper()
	return pgFreezeCollectWithHarnessForTest(t, now, body, pgFreezeNativeHarness)
}

// A controlled native clock hook can force source rotation after the exact
// bounded read, without timing sleeps or changing the collector implementation.
func pgFreezeCollectWithHarnessForTest(t *testing.T, now time.Time, body, harness string) string {
	t.Helper()
	dir := t.TempDir()
	program, logPath, bootPath := filepath.Join(dir, "collector.py"), filepath.Join(dir, "synthetic duration.json"), filepath.Join(dir, "boot")
	replacement := filepath.Join(dir, "replacement.log")
	for path, contents := range map[string]string{filepath.Join(dir, "current_logfiles"): "jsonlog synthetic duration.json\n", program: pgFreezeProgram, logPath: body, replacement: "synthetic replacement\n", bootPath: "00000000-0000-4000-8000-000000000001\n"} {
		if os.WriteFile(path, []byte(contents), 0600) != nil {
			t.Fatal("cannot prepare private native source fixture")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-c", harness, program, dir, bootPath, fmt.Sprint(now.UnixMicro()), replacement, logPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("native bounded log collector did not finish successfully")
	}
	for _, canary := range []string{"private-sql-canary", "private-continuation-canary", "private-user-canary", "private-database-canary", "private-origin-canary"} {
		if strings.Contains(string(output), canary) {
			t.Fatal("native log projection exported private source text")
		}
	}
	return string(output)
}

// Private PG18 JSON writer fixtures give every physical record a distinct
// server line identity even when its timestamp and duration are identical.
var pgFreezeNativeLineNumber int64

func pgFreezeNativeRecord(ended time.Time, message string) map[string]any {
	return map[string]any{
		"timestamp": ended.UTC().Format("2006-01-02 15:04:05.000") + " UTC",
		"pid":       123, "session_id": "386d4380.7b", "session_start": "2000-01-01 00:00:00 UTC",
		"line_num": atomic.AddInt64(&pgFreezeNativeLineNumber, 1), "error_severity": "LOG",
		"message": message, "backend_type": "client backend", "file_name": "postgres.c",
		"func_name": "exec_simple_query", "file_line_num": 1362,
		"user": "private-user-canary", "dbname": "private-database-canary",
	}
}

func pgFreezeNativeJson(record map[string]any) string {
	encoded, err := json.Marshal(record)
	if err != nil {
		panic("invalid synthetic JSON writer fixture")
	}
	return string(encoded) + "\n"
}

func pgFreezeNativeLine(ended time.Time, milliseconds string) string {
	return pgFreezeNativeJson(pgFreezeNativeRecord(ended, "duration: "+milliseconds+" ms  statement: SELECT 'private-sql-canary';"))
}

func pgFreezeNativeMessage(ended time.Time, message string) string {
	return pgFreezeNativeJson(pgFreezeNativeRecord(ended, message))
}

// Producer and public reducer agree on the one-second floor before counting
// distinct identical records. A multiline SQL imitation is never a record.
func TestPgFreezeNativeDurationFloorAndContinuation(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		last         string
		continuation bool
		want         bool
	}{
		{last: "999.999", want: false},
		{last: "1000.000", want: true},
		{last: "999.999", continuation: true, want: false},
	} {
		var body strings.Builder
		body.WriteString(pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete"))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range 249 {
			body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
		}
		last := pgFreezeNativeRecord(now.Add(-time.Minute), "duration: "+c.last+" ms  statement: SELECT private-sql-canary")
		if c.continuation {
			last["message"] = last["message"].(string) + "\n2026-10-19 11:59:00.000 UTC [123] LOG:  duration: 3000.000 ms private-continuation-canary"
		}
		body.WriteString(pgFreezeNativeJson(last))
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		payload := pgFreezeCollectForTest(t, now, body.String())
		alerts := pgFreezeRunForTest(t, now, payload)
		if (requireAlertClassCount(alerts, "pg-freeze-clusters") == 1) != c.want {
			t.Fatal("native duration floor or SQL continuation manufactured/lost a cluster")
		}
	}
}

// Raw malformed and partial records remain private and make cluster
// membership unknown; they cannot disappear from the median population.
func TestPgFreezeNativeMalformedAndPartialRecords(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, shape := range []string{"invalid-duration", "malformed", "partial"} {
		var body strings.Builder
		body.WriteString(pgFreezeNativeLine(now.Add(-16*time.Minute), "1000.000"))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range 250 {
			body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
		}
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		tail := pgFreezeNativeLine(now.Add(-30*time.Second), "NaN")
		if shape == "malformed" {
			tail = "private-sql-canary malformed record\n"
		}
		if shape == "partial" {
			tail = strings.TrimSuffix(pgFreezeNativeLine(now.Add(-30*time.Second), "3000.000"), "\n")
		}
		body.WriteString(tail)
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body.String()))
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatal("native malformed membership supplied a positive median")
		}
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
}

// The production 40MB limit is used unchanged. The initial clipped SQL line
// is discarded, while witnessed interior rows remain useful positive evidence.
func TestPgFreezeNativeByteCapIsNotWindowCoverage(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var body strings.Builder
	body.WriteString(strings.Repeat("x", 40000100))
	body.WriteString(" private-sql-canary\n")
	body.WriteString(pgFreezeNativeLine(now.Add(-16*time.Minute), "1000.000"))
	body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
	for range 250 {
		body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
	}
	body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
	payload := pgFreezeCollectForTest(t, now, body.String())
	var frame pgFreezeWire
	if json.Unmarshal([]byte(payload), &frame) != nil || len(frame.Read) != 2 || frame.Read[1] != "40000000" {
		t.Fatal("native reader did not honor the exact byte cap")
	}
	alerts := pgFreezeRunForTest(t, now, payload)
	requireAlertClass(t, alerts, "pg-freeze-clusters")
	if !strings.Contains(requireAlertClass(t, alerts, "pg-freeze-unobservable").Observed, "tail-capped") {
		t.Fatal("native capped source claimed complete absence")
	}
}

// Timestamp order is checked on all source records, including non-duration
// messages which never contribute to a cluster's median.
func TestPgFreezeNativeRecordOrderIsCoverageAuthority(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	body := pgFreezeNativeLine(now.Add(-16*time.Minute), "1000.000") +
		pgFreezeNativeMessage(now.Add(-time.Minute), "checkpoint complete") +
		pgFreezeNativeMessage(now.Add(-2*time.Minute), "checkpoint complete")
	alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body))
	if !strings.Contains(requireAlertClass(t, alerts, "pg-freeze-unobservable").Observed, "record-order") {
		t.Fatal("native source ordering was assumed instead of verified")
	}
}

// The actual shell guard runs before even the finite native reader command.
// Private fake executables prevent this control from opening a host log.
func TestPgFreezeNativeWrongHostPreventsReader(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"hostname": "#!/bin/sh\nprintf '%s\\n' 'wrong-synthetic-host'\n",
		"timeout":  "#!/bin/sh\nexit 98\n",
	} {
		if os.WriteFile(filepath.Join(dir, name), []byte(body), 0700) != nil {
			t.Fatal("cannot prepare private host guard fixture")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", pgFreezeCommand("pg-1"))
	command.Env = []string{"PATH=" + dir}
	output, err := command.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 74 || len(output) != 0 {
		t.Fatal("wrong host invoked the reader or emitted source content")
	}
}

// Rotation cannot erase a completed old-file cluster or supply recovery for
// the newly named file. The clock callback deterministically forces this order.
func TestPgFreezeNativeRotationKeepsOnlyScopedPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var body strings.Builder
	body.WriteString(pgFreezeNativeLine(now.Add(-16*time.Minute), "1000.000"))
	body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
	for range 250 {
		body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
	}
	body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
	harness := `import json,os,runpy,sys
namespace=runpy.run_path(sys.argv[1])
calls=0
def clock():
    global calls
    calls+=1
    if calls==2:
        os.replace(sys.argv[5],sys.argv[6])
    return int(sys.argv[4])*1000
result=namespace["collect"](sys.argv[2],"pg-1",sys.argv[3],clock,lambda: 0)
print(json.dumps(result,separators=(",",":")))
`
	alerts := pgFreezeRunForTest(t, now, pgFreezeCollectWithHarnessForTest(t, now, body.String(), harness))
	requireAlertClass(t, alerts, "pg-freeze-clusters")
	if !strings.Contains(requireAlertClass(t, alerts, "pg-freeze-unobservable").Observed, "path-changed") {
		t.Fatal("rotated path supplied healthy recovery")
	}
}
