// The missing-reader control compiles on the frozen parent with only this
// file overlaid; registration and the actual public signal are the boundary.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// These frames contain no new production types, preserving the baseline overlay.
func pgAutovacuumCancelPayload(t testing.TB, now time.Time, events []time.Time, flags ...string) string {
	t.Helper()
	rows := [][]string{}
	first, last := now.Add(-16*time.Minute), now.Add(-16*time.Minute)
	for index, event := range events {
		rows = append(rows, []string{fmt.Sprint(100 + index*100), fmt.Sprint(event.UnixMicro())})
		if event.Before(first) {
			first = event
		}
		if event.After(last) {
			last = event
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
		t.Fatal("cannot encode synthetic cancellation projection")
	}
	return string(encoded) + "\n"
}

// A real registry lookup reaches the missing pre-fix reader without a shim.
func pgAutovacuumCancelRegistered(t testing.TB) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == "pg-autovacuum-cancel" {
			return signal
		}
	}
	t.Fatal("no registered signal observes bounded structured autovacuum cancellations")
	return nil
}

// One timed primary read, including the real command guard, owns the public run.
func pgAutovacuumCancelRun(t *testing.T, now time.Time, payload string) Alerts {
	t.Helper()
	calls := 0
	settings := syntheticSettings(&syntheticSource{hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
		calls++
		if host.Name != "pg-1" || timeout != 25*time.Second ||
			!strings.HasPrefix(command, "[ \"$(hostname -s)\" = 'pg-1' ] || exit 74;") ||
			!strings.Contains(command, "timeout -k 2s 20s python3 -c") ||
			!strings.Contains(command, "/var/lib/postgresql/18/main") || !strings.Contains(command, "current_logfiles") {
			t.Fatal("cancellation reader lost finite primary-owned source framing")
		}
		return payload, nil
	}})
	settings.Now = func() time.Time { return now }
	alerts, err := pgAutovacuumCancelRegistered(t).Run(context.Background(), settings)
	if err != nil || calls != 1 {
		t.Fatal("public cancellation reader did not perform exactly one bounded observation")
	}
	return alerts
}

// The frozen baseline has no reader; the successor observes one structured record
// without claiming that the event was harmful or that work was discarded.
func TestPgAutovacuumCancelRegistryObservesNativeRecord(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelPayload(t, now, []time.Time{now.Add(-time.Minute)}))
	alert := requireAlertClass(t, alerts, "pg-autovacuum-cancel-observed")
	signal := pgAutovacuumCancelRegistered(t)
	if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.unobservable ||
		signal.Number() != "2.4e" || signal.ID() != "pg/autovacuum-cancel" || signal.Cadence() != 15*time.Minute ||
		!strings.Contains(alert.Observed, "cancellation_records_at_least=1") {
		t.Fatal("observed cancellation lost its narrow WARN1 fifteen-minute policy")
	}
}
