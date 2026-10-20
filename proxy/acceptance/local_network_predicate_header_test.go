// Predicate echoes are command metadata, not Darwin event records. These
// controls exercise the real collector and parser without running a log query.
package acceptance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Reuses the actual production query instead of maintaining a second predicate.
func localNetworkPredicateHeaderFixture(t *testing.T, args []string) string {
	t.Helper()
	for i, arg := range args {
		if arg == "--predicate" && i+1 < len(args) {
			return "Filtering the log data using \"" + args[i+1] + "\"\n"
		}
	}
	t.Fatal("collector did not supply its predicate")
	return ""
}

// A predicate-only command response must be clean even though its query text
// includes the exact route-event signatures being searched for.
func TestLocalNetworkPredicateHeaderDoesNotCountSignals(t *testing.T) {
	started := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, padding := range []string{"", " \t"} {
		diagnostic := collectLocalNetworkFailureDiagnosticWith(
			started, started.Add(time.Second), "darwin", "proxy-fixture", 42,
			func(_ context.Context, _ string, args ...string) ([]byte, error) {
				output := padding + localNetworkPredicateHeaderFixture(t, args) +
					"Timestamp                       Thread     Type        Activity             PID    TTL\n"
				signals := parseLocalNetworkSignals([]byte(output), 42)
				if signals != (localNetworkSignals{}) {
					t.Errorf("predicate metadata was treated as log events: %+v", signals)
				}
				return []byte(output), nil
			},
		)
		for _, want := range []string{
			"classification=no-local-kernel-signal",
			"ipv6_router_lifetime_zeros=0",
			"ip_monitor_network_changes=0",
			"request_interval=2026-01-02T03:04:05Z/2026-01-02T03:04:06Z",
		} {
			if !strings.Contains(diagnostic, want) {
				t.Errorf("predicate-only diagnostic lacks %q: %s", want, diagnostic)
			}
		}
	}
}

// Excluding the command header must not suppress genuine records or the
// existing multiline socket-summary join used for independent peer stalls.
func TestLocalNetworkPredicateHeaderPreservesActualEvents(t *testing.T) {
	started := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	wantSignals := localNetworkSignals{
		skywalkSlabFailures: 1, gsoFailures: 1, wifiStalls: 1, maxWifiStallScore: 7,
		ipv6RouterLifetimeZeros: 1, ipMonitorNetworkChanges: 1,
		peerTCPStallFlows: 2, peerTCPStallProcesses: 2,
	}
	diagnostic := collectLocalNetworkFailureDiagnosticWith(
		started, started.Add(time.Second), "darwin", "proxy-fixture", 42,
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			output := localNetworkPredicateHeaderFixture(t, args) + strings.Join([]string{
				"2026-01-02 03:04:05.000000+0000 kernel[0:0] skmem_slab_alloc_locked: failed to allocate slab",
				"2026-01-02 03:04:05.000001+0000 kernel[0:0] netif_gso_tcp_segment_mbuf failed to alloc segment mbuf",
				"2026-01-02 03:04:05.000002+0000 kernel[0:0] DPS Symptoms StallScore:7",
				"2026-01-02 03:04:05.000003+0000 configd[7:8] RTADV enFixture: router lifetime became zero",
				"2026-01-02 03:04:05.000004+0000 configd[7:8] [fixture:IPMonitor] network changed: IPv6 absent",
				darwinPeerTCPSummary(101, "fixture-one", 43, "0xa1", "0/2", "redacted"),
				darwinPeerTCPSummary(102, "fixture-two", 44, "0xa2", "0/3", "redacted"),
			}, "\n")
			if got := parseLocalNetworkSignals([]byte(output), 42); got != wantSignals {
				t.Errorf("predicate header changed actual event counts: got %+v want %+v", got, wantSignals)
			}
			return []byte(output), nil
		},
	)
	if !strings.Contains(diagnostic, "classification=local-kernel-buffer-pressure+wifi-stall+network-path-churn+peer-tcp-stall") {
		t.Fatalf("actual event classification changed: %s", diagnostic)
	}
	for _, private := range []string{"Filtering the log data", "fixture-one", "fixture-two", "0xa1", "0xa2"} {
		if strings.Contains(diagnostic, private) {
			t.Errorf("diagnostic leaked raw metadata %q: %s", private, diagnostic)
		}
	}
}

// Empty output and unrelated command metadata stay clean; a failed query
// remains unavailable even if its partial output resembles matching events.
func TestLocalNetworkPredicateHeaderPreservesEmptyAndQueryFailure(t *testing.T) {
	started := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name       string
		output     string
		queryErr   error
		want       string
		wantStatus string
	}{
		{name: "empty", want: "no-local-kernel-signal"},
		{name: "metadata", output: "Timestamp Thread Type Activity PID TTL\nLog statistics - Default: 0\n", want: "no-local-kernel-signal"},
		{name: "query failure", queryErr: errors.New("fixture query failed"), want: "query-unavailable", wantStatus: "query_status=failed"},
	}
	for _, c := range cases {
		diagnostic := collectLocalNetworkFailureDiagnosticWith(
			started, started.Add(time.Second), "darwin", "proxy-fixture", 42,
			func(_ context.Context, _ string, args ...string) ([]byte, error) {
				output := c.output
				if c.queryErr != nil {
					output += localNetworkPredicateHeaderFixture(t, args)
				}
				return []byte(output), c.queryErr
			},
		)
		if !strings.Contains(diagnostic, "classification="+c.want) ||
			(c.wantStatus != "" && !strings.Contains(diagnostic, c.wantStatus)) {
			t.Errorf("%s diagnostic = %s", c.name, diagnostic)
		}
	}
}

// Repeated headers and a long ordinary line must not hide a real tail event.
// A timestamped message containing the header phrase remains an event.
func TestLocalNetworkPredicateHeaderPreservesLargeInputTail(t *testing.T) {
	started := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	diagnostic := collectLocalNetworkFailureDiagnosticWith(
		started, started.Add(time.Second), "darwin", "proxy-fixture", 42,
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			header := localNetworkPredicateHeaderFixture(t, args)
			output := strings.Repeat(header, 128) + strings.Repeat("x", 128*1024) + "\n" +
				"2026-01-02 03:04:05.000001+0000 configd[7:8] RTADV enFixture: router lifetime became zero; Filtering the log data using fixture\n" +
				"2026-01-02 03:04:05.000002+0000 configd[7:8] [fixture:IPMonitor] network changed: IPv6 absent\n"
			want := localNetworkSignals{ipv6RouterLifetimeZeros: 1, ipMonitorNetworkChanges: 1}
			if got := parseLocalNetworkSignals([]byte(output), 42); got != want {
				t.Errorf("large-input metadata contaminated tail events: got %+v want %+v", got, want)
			}
			return []byte(output), nil
		},
	)
	for _, want := range []string{
		"classification=local-network-path-churn",
		"ipv6_router_lifetime_zeros=1",
		"ip_monitor_network_changes=1",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("large-input diagnostic lacks %q: %s", want, diagnostic)
		}
	}
}
