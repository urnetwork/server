// Pinned PG18 error_severity maps every debug level to the single JSON value
// DEBUG. A native debug record must not invalidate unrelated duration members.
package monitor

import (
	"strings"
	"testing"
	"time"
)

// The canonical writer value retains a real native cluster; enum-looking
// DEBUG1..DEBUG5 aliases remain source-schema uncertainty. The first assertion
// is the causal RED on exact V3, whose allowlist omitted canonical DEBUG.
func TestPgFreezeJsonCanonicalDebugPreservesNativeAuthority(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, severity := range []string{"DEBUG", "DEBUG1", "DEBUG2", "DEBUG3", "DEBUG4", "DEBUG5"} {
		var body strings.Builder
		record := pgFreezeNativeRecord(now.Add(-16*time.Minute), "private-debug-canary")
		record["error_severity"] = severity
		body.WriteString(pgFreezeNativeJson(record))
		body.WriteString(pgFreezeNativeLine(now.Add(-61*time.Second), "1000.000"))
		for range 250 {
			body.WriteString(pgFreezeNativeLine(now.Add(-time.Minute), "3000.000"))
		}
		body.WriteString(pgFreezeNativeLine(now.Add(-59*time.Second), "1000.000"))
		payload := pgFreezeCollectForTest(t, now, body.String())
		if strings.Contains(payload, "private-debug-canary") {
			t.Fatal("native debug projection leaked its primary message")
		}
		alerts := pgFreezeRunForTest(t, now, payload)
		if severity == "DEBUG" {
			if requireAlertClassCount(alerts, "pg-freeze-clusters") != 1 {
				t.Fatal("canonical PG18 DEBUG record suppressed native duration authority")
			}
		} else {
			if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
				t.Fatal("nonwriter debug severity supplied source authority")
			}
			requireAlertClass(t, alerts, "pg-freeze-unobservable")
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-debug-canary")
		}
	}
	// A DEBUG message resembling duration is not a native LOG duration event.
	old := pgFreezeNativeRecord(now.Add(-16*time.Minute), "duration: 3000.000 ms private-debug-canary")
	old["error_severity"] = "DEBUG"
	current := pgFreezeNativeRecord(now.Add(-time.Minute), "duration: 3000.000 ms private-debug-canary")
	current["error_severity"] = "DEBUG"
	alerts := pgFreezeRunForTest(t, now, pgFreezeCollectForTest(t, now, pgFreezeNativeJson(old)+pgFreezeNativeJson(current)))
	if len(alerts) != 0 {
		t.Fatal("canonical DEBUG-only quiet observed window lost coverage")
	}
}
