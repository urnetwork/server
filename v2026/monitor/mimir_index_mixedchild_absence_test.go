// Fleet writer absence needs metrics authority from every identified child;
// a readable gateway does not exclude an unreadable compactor sibling.
package monitor

import (
	"strings"
	"testing"
	"time"
)

// Exercise the public constructor with both complete discovery orders. The
// unreadable child is identified, but its writer role and timestamp are unknown.
func TestMimirIndexMixedUnreadableChildDoesNotInventMissingWriter(t *testing.T) {
	now := syntheticSettings(nil).Now()
	ready := mimirIndexFixture(mimirIndexFixtureOptions{
		count: 1, metricsReady: true,
		processStart: now.Add(-2 * time.Hour), gatewayLastSync: now.Add(-5 * time.Minute),
		gatewaySyncCount: 8, tenantsDiscovered: 1, tenantsSynced: 1,
	})
	ready = strings.TrimSuffix(ready, "mimir_count 1\n")
	unreadable := "instance_begin 14820\nmetrics_ready 0\ninstance_end\n"
	for _, reverse := range []bool{false, true} {
		frame := ready + unreadable + "mimir_count 2\n"
		if reverse {
			frame = unreadable + ready + "mimir_count 2\n"
		}
		alerts := runMimirIndexSynthetic(t, map[string]string{"mimir-absence.example.test": frame})
		unknown := 0
		for _, alert := range alerts {
			if alert.Class == "mimir-index-unobservable" && isVisibilityAlert(alert) && alert.Severity == SeverityWarn && alert.Sustain == 2 {
				unknown++
			} else {
				t.Errorf("reverse=%t: partial child metrics invented a non-visibility finding of class %s", reverse, alert.Class)
			}
		}
		if unknown != 1 || len(alerts) != 1 {
			t.Errorf("reverse=%t: want exactly one unknown observation and no fleet writer-absence judgment; unknown=%d alerts=%d", reverse, unknown, len(alerts))
		}
	}
}

// Full metrics authority and a mature discovered tenant still establish a
// missing writer. The regression must not erase this independent true fault.
func TestMimirIndexCompleteChildrenRetainMissingWriter(t *testing.T) {
	now := syntheticSettings(nil).Now()
	ready := mimirIndexFixture(mimirIndexFixtureOptions{
		count: 1, metricsReady: true,
		processStart: now.Add(-2 * time.Hour), gatewayLastSync: now.Add(-5 * time.Minute),
		gatewaySyncCount: 8, tenantsDiscovered: 1, tenantsSynced: 1,
	})
	first := strings.TrimSuffix(ready, "mimir_count 1\n")
	second := strings.Replace(first, "instance_begin 14819\n", "instance_begin 14820\n", 1)
	for _, reverse := range []bool{false, true} {
		frame := first + second + "mimir_count 2\n"
		if reverse {
			frame = second + first + "mimir_count 2\n"
		}
		alerts := runMimirIndexSynthetic(t, map[string]string{"mimir-absence.example.test": frame})
		if len(alerts) != 1 {
			t.Errorf("reverse=%t: complete missing-writer control emitted %d findings, want one", reverse, len(alerts))
			continue
		}
		alert := alerts[0]
		if alert.Class != "mimir-bucket-index-stale" || alert.Target != "mimir-bucket-index" || alert.Severity != SeverityWarn || alert.Sustain != 2 || !strings.Contains(alert.Observed, "index_updates=0") {
			t.Errorf("reverse=%t: complete writer-absence fault lost its existing public contract", reverse)
		}
	}
}
