// The missing-reader regression compiles against the frozen parent with only
// this file overlaid. It reaches the actual public registry, never a stub.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Synthetic source rows retain distinct byte positions for identical records.
type pgFreezeTestEvent struct {
	ended    time.Time
	duration int64
}

// A full file begins before the requested window; flags narrow that authority.
func pgFreezeTestPayload(t testing.TB, now time.Time, events []pgFreezeTestEvent, flags ...string) string {
	t.Helper()
	rows := [][]string{}
	first, last := now.Add(-16*time.Minute), now.Add(-16*time.Minute)
	for index, event := range events {
		rows = append(rows, []string{fmt.Sprint(100 + index*100), fmt.Sprint(event.ended.UnixMicro()), fmt.Sprint(event.duration)})
		if event.ended.Before(first) {
			first = event.ended
		}
		if event.ended.After(last) {
			last = event.ended
		}
	}
	if flags == nil {
		flags = []string{}
	}
	file := []string{"1", "2", "1000000", fmt.Sprint(now.UnixNano()), fmt.Sprint(now.UnixNano())}
	frame := map[string]any{
		"version": "2", "status": "snapshot", "host": "pg-1",
		"clock":       []string{fmt.Sprint(now.UnixMicro()), fmt.Sprint(now.UnixMicro()), fmt.Sprint(now.UnixMicro()), "0", "0"},
		"boot":        []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000001"},
		"file_before": file, "file_after": file, "read": []string{"0", "1000000"},
		"span": []string{fmt.Sprint(first.UnixMicro()), fmt.Sprint(last.UnixMicro())}, "flags": flags, "rows": rows,
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal("cannot encode synthetic log projection")
	}
	return string(encoded) + "\n"
}

// Real >100ms statement boundaries surround the otherwise identical events.
func pgFreezeTestCluster(ended time.Time, count int, duration int64) []pgFreezeTestEvent {
	events := []pgFreezeTestEvent{{ended: ended.Add(-time.Second), duration: 1000000000}}
	for range count {
		events = append(events, pgFreezeTestEvent{ended: ended, duration: duration})
	}
	return append(events, pgFreezeTestEvent{ended: ended.Add(time.Second), duration: 1000000000})
}

// Registry lookup is the pre-fix failure boundary, not a constructor shim.
func pgFreezeSignalForTest(t testing.TB) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == "pg-freeze" {
			return signal
		}
	}
	t.Fatal("no registered signal reads bounded PostgreSQL duration-log release clusters")
	return nil
}

// The source seam supplies already-reduced private rows to the real signal.
func pgFreezeRunForTest(t *testing.T, now time.Time, payload string) Alerts {
	t.Helper()
	signal := pgFreezeSignalForTest(t)
	calls := 0
	settings := syntheticSettings(&syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls++
		if host.Name != "pg-1" || !strings.HasPrefix(command, "[ \"$(hostname -s)\" = 'pg-1' ] || exit 74;") ||
			!strings.Contains(command, "timeout -k 2s 20s python3 -c") || !strings.Contains(command, "/var/lib/postgresql/18/main") {
			t.Fatal("duration log reader lost its bounded same-host source")
		}
		return payload, nil
	}})
	settings.Now = func() time.Time { return now }
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || calls != 1 {
		t.Fatal("public duration log observation failed")
	}
	pgFreezeRequirePrivate(t, alerts)
	return alerts
}

// A witnessed 250-statement, three-second release previously had no reader.
func TestPgFreezeRegistryDetectsQualifiedReleaseCluster(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	payload := pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000))
	alerts := pgFreezeRunForTest(t, now, payload)
	alert := requireAlertClass(t, alerts, "pg-freeze-clusters")
	if alert.Severity != SeverityWarn || alert.Sustain != 1 || pgFreezeSignalForTest(t).Cadence() != 15*time.Minute {
		t.Fatal("observed-log policy changed from WARN1 at fifteen-minute cadence")
	}
	if !requireAlertClass(t, alerts, "pg-freeze-unobservable").unobservable {
		t.Fatal("the unresolved trailing boundary supplied healthy recovery")
	}
}

// Every public representation omits source identity and private fixture text.
func pgFreezeRequirePrivate(t testing.TB, alerts Alerts) {
	t.Helper()
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal("cannot encode public duration alert")
		}
		for _, canary := range []string{"private-sql-canary", "private-continuation-canary", "private-user-canary", "private-database-canary", "private-origin-canary", "private-host-canary", "private-clock-canary", "private-boot-canary", "private-error-canary", "private-source-error-canary", "private-failed-output-canary", "00000000-0000-4000-8000-000000000001"} {
			if strings.Contains(string(encoded), canary) || strings.Contains(alert.Markdown(), canary) || strings.Contains(alert.Identity(), canary) {
				t.Fatal("public duration alert exposed private source content")
			}
		}
	}
}
