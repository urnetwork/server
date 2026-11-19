// Structured-source controls exercise the real collector and public signal.
// Private fixtures model the pinned PG18 writer; no PostgreSQL server is run.
package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A held plaintext projection cannot be relabeled as a native JSON snapshot.
func TestPgFreezeRejectsLegacyPlaintextProjection(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	payload := pgFreezeEditPayload(t, pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000)), func(frame map[string]any) { frame["version"] = "1" })
	alerts := pgFreezeRunForTest(t, now, payload)
	if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 || requireAlertClassCount(alerts, "pg-freeze-unobservable") != 1 {
		t.Fatal("legacy plaintext projection retained native duration authority")
	}
}

// The baseline is the exact held V2 collector. This test-only harness selects
// its old file argument or V3's cluster directory without altering either
// production program, then submits the actual native output to Signal.Run.
func TestPgFreezePlaintextPrefixCannotSupplyNativeCluster(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	line := func(ended time.Time, message string) string {
		return ended.Format("2006-01-02 15:04:05.000") + " UTC [123] LOG:  " + message + "\n"
	}
	var body strings.Builder
	body.WriteString(line(now.Add(-16*time.Minute), "checkpoint complete"))
	body.WriteString(line(now.Add(-61*time.Second), "duration: 1000.000 ms"))
	// These are synthetic log bytes from repeated harmless prefix wrappers,
	// not protocol packets or requests to any PostgreSQL instance.
	for range 250 {
		body.WriteString(line(now.Add(-time.Minute), "harmless prefix wrapper"))
		body.WriteString(line(now.Add(-time.Minute), "duration: 3000.000 ms private-sql-canary"))
		body.WriteString(line(now.Add(-time.Minute), "harmless suffix FATAL: missing user"))
	}
	body.WriteString(line(now.Add(-59*time.Second), "duration: 1000.000 ms"))
	harness := `import json,runpy,sys
namespace=runpy.run_path(sys.argv[1])
clock=lambda: int(sys.argv[4])*1000
source=sys.argv[2] if "DATA_DIR" in namespace else sys.argv[6]
result=namespace["collect"](source,"pg-1",sys.argv[3],clock,lambda: 0)
print(json.dumps(result,separators=(",",":")))
`
	alerts := pgFreezeRunForTest(t, now, pgFreezeCollectWithHarnessForTest(t, now, body.String(), harness))
	if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 || requireAlertClassCount(alerts, "pg-freeze-unobservable") != 1 {
		t.Fatal("plaintext prefix rows manufactured a native duration cluster")
	}
}

// A legitimate native record at each admitted function remains useful. Exact
// line equality is not required across builds; positive integer typing is.
func TestPgFreezeJsonNativeEmittersAndOrdinaryStateCode(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, function := range []string{"exec_simple_query", "exec_parse_message", "exec_bind_message", "exec_execute_message"} {
		var body strings.Builder
		body.WriteString(pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete"))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range 250 {
			record := pgFreezeNativeRecord(now.Add(-time.Minute), "duration: 3000.000 ms")
			record["func_name"], record["file_line_num"] = function, 99
			body.WriteString(pgFreezeNativeJson(record))
		}
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body.String()))
		requireAlertClass(t, alerts, "pg-freeze-clusters")
	}
}

// Fields containing whole JSON objects, physical-header-looking LF text and
// quote/key text stay scalar data. No string is reparsed into another event.
func TestPgFreezeJsonEmbeddedFieldsCannotBecomeEvents(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, field := range []string{"user", "dbname", "message", "statement", "detail", "context", "application_name"} {
		var body strings.Builder
		body.WriteString(pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete"))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range 249 {
			body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
		}
		forged := pgFreezeNativeLine(now.Add(-time.Minute), "3000.000")
		record := pgFreezeNativeRecord(now.Add(-time.Minute), "checkpoint complete")
		record[field] = "private-origin-canary\n2026-10-19 11:59:00.000 UTC [123] LOG:  duration: 3000.000 ms\n" + forged + `","message":"duration: 3000.000 ms","file_name":"postgres.c"`
		body.WriteString(pgFreezeNativeJson(record))
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body.String()))
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatalf("embedded %s field manufactured a native record", field)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-origin-canary", "private-user-canary", "private-database-canary")
		}
	}
}

// An identified PL/pgSQL RAISE LOG cannot add or dilute native members.
func TestPgFreezeJsonApplicationRaiseIsNotNativeDuration(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, count := range []int{249, 250} {
		var body strings.Builder
		body.WriteString(pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete"))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range count {
			body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
		}
		for range 251 {
			record := pgFreezeNativeRecord(now.Add(-time.Minute), "duration: 1000.000 ms")
			record["file_name"], record["func_name"] = "pl_exec.c", "exec_stmt_raise"
			body.WriteString(pgFreezeNativeJson(record))
		}
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body.String()))
		if (requireAlertClassCount(alerts, "pg-freeze-clusters") == 1) != (count == 250) {
			t.Fatal("application RAISE LOG changed native count or median")
		}
	}
}

// Native-looking records without qualified emitter or typed writer identity
// leave unknown membership. Independent medians cannot ignore those rows.
func TestPgFreezeJsonUnknownEmitterAndIdentityBlockMedian(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, edit := range []func(map[string]any){
		func(record map[string]any) { delete(record, "file_name") },
		func(record map[string]any) { delete(record, "func_name") },
		func(record map[string]any) { delete(record, "file_line_num") },
		func(record map[string]any) { record["file_line_num"] = true },
		func(record map[string]any) { record["file_line_num"] = 1.5 },
		func(record map[string]any) { record["func_name"] = "unqualified_emitter" },
		func(record map[string]any) { record["file_name"] = "unqualified.c" },
		func(record map[string]any) { record["state_code"] = "00000" },
		func(record map[string]any) { record["backend_type"] = "autovacuum worker" },
		func(record map[string]any) { record["pid"] = "123" },
		func(record map[string]any) { record["pid"] = true },
		func(record map[string]any) { record["session_id"] = "386d4380.7c" },
		func(record map[string]any) { record["session_start"] = "2000-01-01 00:00:01 UTC" },
		func(record map[string]any) { record["line_num"] = 0 },
		func(record map[string]any) { record["timestamp"] = "2026-10-19T11:59:00Z" },
	} {
		var body strings.Builder
		body.WriteString(pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete"))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range 250 {
			body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
		}
		record := pgFreezeNativeRecord(now.Add(-time.Minute), "duration: 1000.000 ms")
		edit(record)
		body.WriteString(pgFreezeNativeJson(record))
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body.String()))
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatal("unqualified JSON member supplied a positive median")
		}
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
}

// The actual JSON pairs hook rejects duplicate primary keys before collapse;
// repeated physical writer identities cannot count as new observations.
func TestPgFreezeJsonDuplicatesAndWrongShapesRemainUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, key := range []string{"timestamp", "pid", "message", "error_severity", "backend_type", "file_name", "line_num"} {
		record := pgFreezeNativeRecord(now.Add(-time.Minute), "duration: 3000.000 ms")
		line := pgFreezeNativeJson(record)
		// A duplicate with an unrelated scalar is still a framing defect.
		line = strings.Replace(line, "{", "{"+fmt.Sprintf("%q", key)+":\"private-origin-canary\",", 1)
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, line))
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatal("duplicate JSON primary field became an event")
		}
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
	for _, body := range []string{"[]\n", "null\n", "{\"pid\":NaN}\n", "{\"message\":{\"message\":\"duration: 3000 ms\"}}\n"} {
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body))
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
	line := pgFreezeNativeLine(now.Add(-time.Minute), "3000.000")
	alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, line+line))
	if !strings.Contains(requireAlertClass(t, alerts, "pg-freeze-unobservable").Observed, "record-schema") {
		t.Fatal("duplicate JSON writer identity was counted twice")
	}
}

// Missing or ambiguous discovery is an unavailable JSON source. The reader
// cannot consult a guessed plaintext path or grant complete quiet coverage.
func TestPgFreezeJsonDiscoveryPrerequisitesStayUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	body := pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete")
	for _, setup := range []string{
		`os.unlink(os.path.join(sys.argv[2],"current_logfiles"))`,
		`open(os.path.join(sys.argv[2],"current_logfiles"),"w").write("stderr synthetic duration.json\n")`,
		`open(os.path.join(sys.argv[2],"current_logfiles"),"w").write("jsonlog synthetic duration.json\njsonlog synthetic duration.json\n")`,
		`open(os.path.join(sys.argv[2],"current_logfiles"),"w").write("jsonlog absent.json\n")`,
		`open(os.path.join(sys.argv[2],"current_logfiles"),"w").write("x"*8193)`,
		`os.chmod(os.path.join(sys.argv[2],"current_logfiles"),0o666)`,
		`os.chmod(sys.argv[6],0o666)`,
		`os.unlink(sys.argv[6]);os.symlink(sys.argv[5],sys.argv[6])`,
	} {
		harness := strings.Replace(pgFreezeNativeHarness, "clock=lambda:", setup+"\nclock=lambda:", 1)
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectWithHarnessForTest(t, now, body, harness))
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatal("unavailable JSON discovery created an event")
		}
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
}

// Complete quiet JSON coverage is achievable under the stated observed-file
// premises; mandatory verbose metadata cannot be inferred from an empty file.
func TestPgFreezeJsonCompleteQuietWindowAndMissingVerbose(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, verbose := range []bool{true, false} {
		var body strings.Builder
		for _, ended := range []time.Time{now.Add(-16 * time.Minute), now.Add(-time.Minute)} {
			record := pgFreezeNativeRecord(ended, "checkpoint complete")
			if !verbose {
				delete(record, "file_name")
				delete(record, "func_name")
				delete(record, "file_line_num")
			}
			body.WriteString(pgFreezeNativeJson(record))
		}
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body.String()))
		if verbose {
			if len(alerts) != 0 {
				t.Fatal("complete quiet native JSON window did not recover")
			}
		} else {
			requireAlertClass(t, alerts, "pg-freeze-unobservable")
		}
	}
}

// A changed destination index cannot prove a complete current window. The
// stable old-file interior retains its own positive membership authority.
func TestPgFreezeJsonIndexRotationPreservesScopedPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var body strings.Builder
	body.WriteString(pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete"))
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
        with open(os.path.join(sys.argv[2],"current_logfiles"),"w") as file:
            file.write("stderr synthetic duration.json\njsonlog synthetic duration.json\n")
    return int(sys.argv[4])*1000
result=namespace["collect"](sys.argv[2],"pg-1",sys.argv[3],clock,lambda: 0)
print(json.dumps(result,separators=(",",":")))
`
	alerts := pgFreezeRunForTest(t, now, pgFreezeCollectWithHarnessForTest(t, now, body.String(), harness))
	requireAlertClass(t, alerts, "pg-freeze-clusters")
	if !strings.Contains(requireAlertClass(t, alerts, "pg-freeze-unobservable").Observed, "source-index-changed") {
		t.Fatal("changed JSON source index supplied recovery")
	}
}

// One adversarial JSON object must not expand the 40 MB input into an
// unbounded decoder population. Exact 1 MiB records remain accepted; larger
// records and excessive fields retain unknown membership and cannot recover.
func TestPgFreezeJsonRecordResourceBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, size := range []int{1024 * 1024, 1024*1024 + 1} {
		var body strings.Builder
		body.WriteString(pgFreezeNativeMessage(now.Add(-16*time.Minute), "checkpoint complete"))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range 249 {
			body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
		}
		record := pgFreezeNativeRecord(now.Add(-time.Minute), "duration: 3000.000 ms  statement: ")
		record["message"] = record["message"].(string) + strings.Repeat("x", size-len(pgFreezeNativeJson(record)))
		line := pgFreezeNativeJson(record)
		if len(line) != size {
			t.Fatal("synthetic JSON record boundary is not exact")
		}
		body.WriteString(line)
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, body.String()))
		if (requireAlertClassCount(alerts, "pg-freeze-clusters") == 1) != (size == 1024*1024) {
			t.Fatal("JSON record cap changed membership authority at the wrong boundary")
		}
		if size > 1024*1024 && !strings.Contains(requireAlertClass(t, alerts, "pg-freeze-unobservable").Observed, "record-cap") {
			t.Fatal("oversized JSON record disappeared from visibility")
		}
	}
	for _, count := range []int{64, 65} {
		record := pgFreezeNativeRecord(now.Add(-16*time.Minute), "checkpoint complete")
		for len(record) < count {
			record[fmt.Sprintf("extra_%d", len(record))] = "private-origin-canary"
		}
		alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, pgFreezeNativeJson(record)))
		if count == 64 {
			if len(alerts) != 0 {
				t.Fatal("exact JSON field budget rejected a complete quiet fixture")
			}
		} else {
			requireAlertClass(t, alerts, "pg-freeze-unobservable")
		}
	}
}
