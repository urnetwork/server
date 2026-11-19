// Permitted route failures and explicit exclusions must coexist in the public signal.
package monitor

import (
	"strings"
	"testing"
)

// The first permitted route is absent and the second cannot be parsed. The
// disabled sibling must never reach the source or erase either observation.
func TestObserverRouteKeepsPermittedFailuresWithExcludedSibling(t *testing.T) {
	lookups := 0
	settings := observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"test-tunnel"}}, func(_ string, args ...string) (string, error) {
		lookups++
		switch args[len(args)-1] {
		case "192.0.2.11":
			return "route: writing to routing socket: not in table\n", nil
		case "2001:db8::13":
			return "unparseable synthetic output", nil
		default:
			t.Fatal("a denied or unknown target reached the local lookup")
			return "", nil
		}
	})
	settings.ExcludedHosts = []string{"edge-2"}
	alerts := observerRouteTestRun(t, "darwin", settings)
	if lookups != 2 {
		t.Fatalf("got %d lookups, want the two permitted targets", lookups)
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	var diverted, unknown bool
	for _, alert := range alerts {
		switch alert.Class {
		case "observer-route-diverted":
			diverted = alert.Target == "monitor-host/overlay" && alert.Sustain == 2 && strings.Contains(alert.Evidence, "absent=[edge-1]")
			requireAlertOmits(t, alert, "edge-2", "192.0.2.11")
		case "observer-route-unobservable":
			unknown = alert.Target == "monitor-host/overlay" && alert.Sustain == 2 && strings.Contains(alert.Observed, "unknown_hosts=[edge-3]")
			requireAlertOmits(t, alert, "edge-2", "2001:db8::13")
		}
	}
	if !diverted || !unknown {
		t.Fatalf("permitted route verdicts were lost under an unrelated host exclusion: diverted=%t unknown=%t", diverted, unknown)
	}
}

// A selected tunnel outside the declared contract proves only a route mismatch.
func TestObserverRouteDivertedKeepsSshCauseUnknown(t *testing.T) {
	settings := observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"declared-tunnel"}}, func(_ string, _ ...string) (string, error) {
		return observerRouteTestDarwin("other-tunnel", ""), nil
	})
	alert := requireAlertClass(t, observerRouteTestRun(t, "darwin", settings), "observer-route-diverted")
	if !strings.Contains(alert.Action, "Confirm the intended route") || strings.Contains(alert.Mechanism, "their SSH failures and stale tails are observation-path loss") {
		t.Fatal("route mismatch was attributed to a broken path before operator intent or packet delivery was established")
	}
}
