// Exercises the independent host UDP loss bands through complete signal runs.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// One lost datagram out of one hundred is a page even below the warning rate.
func TestProxyMemorySignalUdpRatioPagesBelowWarnRate(t *testing.T) {
	alerts := runProxyMemoryUdpInterval(t, time.Minute, 99, 1)
	if len(alerts) != 1 {
		t.Fatalf("UDP independent ratio threshold: same-boot 60s interval with 1 dropped and 99 delivered datagrams produced %d alerts, want one page", len(alerts))
	}
	drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
	if drops.Severity != SeverityPage {
		t.Fatalf("UDP independent ratio threshold: 1%% loss at 1 drop/min has severity %q, want page", drops.Severity)
	}
	if drops.Identity() != "proxy/host-memory|proxy-udp-receive-drops|proxy-1|receive-buffer" ||
		drops.SignalNumber != "14.7" || drops.SignalKey != "proxy-memory" || drops.Sustain != 1 || drops.PageSustain != 0 {
		t.Fatalf("UDP ratio alert changed identity or immediate escalation: %+v", drops)
	}
	for _, want := range []string{
		"sample_elapsed_s=60",
		"udp_in_datagrams_delta=99",
		"udp_rcvbuf_errors_delta=1",
		"udp_rcvbuf_errors_rate_per_min=1.00",
		"udp_receive_drop_ratio_pct=1.0000",
		"udp_in_datagrams_before=1000000",
		"udp_in_datagrams_after=1000099",
		"udp_rcvbuf_errors_before=10000",
		"udp_rcvbuf_errors_after=10001",
		"udp_receive_drop_ratio stays below 1.00%",
		"host-wide loss below WireGuard authentication",
		"not socket ownership by itself",
		"Do not restart WireGuard",
		"at least ten minutes",
	} {
		if !strings.Contains(drops.Markdown(), want) {
			t.Errorf("UDP ratio alert missing %q", want)
		}
	}
	var jsonLines strings.Builder
	if err := alerts.WriteJSONL(&jsonLines); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "synthetic-private-journal", "synthetic-private-source"} {
		if strings.Contains(alerts.Markdown(), private) || strings.Contains(jsonLines.String(), private) {
			t.Errorf("UDP ratio alert exposed private synthetic source marker %q", private)
		}
	}
}

// The ratio denominator includes drops, and each rate edge is independent.
func TestProxyMemorySignalUdpThresholdBands(t *testing.T) {
	for _, c := range []struct {
		name      string
		elapsed   time.Duration
		delivered int64
		dropped   int64
		want      Severity
	}{
		{name: "no traffic", elapsed: time.Minute},
		{name: "delivered only", elapsed: time.Minute, delivered: 1},
		{name: "sparse below ratio", elapsed: time.Minute, delivered: 100, dropped: 1},
		{name: "sparse exact ratio", elapsed: time.Minute, delivered: 99, dropped: 1, want: SeverityPage},
		{name: "sparse above ratio", elapsed: time.Minute, delivered: 98, dropped: 1, want: SeverityPage},
		{name: "dropped only", elapsed: time.Minute, dropped: 1, want: SeverityPage},
		{name: "below warning rate", elapsed: time.Minute, delivered: 999_901, dropped: 99},
		{name: "below warning rate exact ratio", elapsed: time.Minute, delivered: 9_801, dropped: 99, want: SeverityPage},
		{name: "exact warning rate low ratio", elapsed: time.Minute, delivered: 999_900, dropped: 100, want: SeverityWarn},
		{name: "below page rate low ratio", elapsed: time.Minute, delivered: 9_990_001, dropped: 9_999, want: SeverityWarn},
		{name: "exact page rate low ratio", elapsed: time.Minute, delivered: 9_990_000, dropped: 10_000, want: SeverityPage},
		{name: "warning count spread over two minutes", elapsed: 2 * time.Minute, delivered: 999_900, dropped: 100},
		{name: "page rate over two minutes", elapsed: 2 * time.Minute, delivered: 19_980_000, dropped: 20_000, want: SeverityPage},
		{name: "exact warmup boundary", elapsed: 30 * time.Second, delivered: 99, dropped: 1, want: SeverityPage},
		{name: "ratio at fifteen minute cadence", elapsed: 15 * time.Minute, delivered: 99, dropped: 1, want: SeverityPage},
	} {
		alerts := runProxyMemoryUdpInterval(t, c.elapsed, c.delivered, c.dropped)
		if c.want == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: got %d alerts, want healthy", c.name, len(alerts))
			}
			continue
		}
		if len(alerts) != 1 {
			t.Errorf("%s: got %d alerts, want one %s", c.name, len(alerts), c.want)
			continue
		}
		if alerts[0].Class != "proxy-udp-receive-drops" || alerts[0].Severity != c.want {
			t.Errorf("%s: got class=%s severity=%s, want UDP drops severity=%s", c.name, alerts[0].Class, alerts[0].Severity, c.want)
		}
	}
}

// Counter and clock warmups retain their own next baseline before evaluation.
func TestProxyMemorySignalUdpWarmupAndCounterReset(t *testing.T) {
	for _, c := range []struct {
		name      string
		elapsed   time.Duration
		bootId    string
		delivered int64
		dropped   int64
	}{
		{name: "new boot with increasing counters", elapsed: time.Minute, bootId: "00000000-0000-4000-8000-000000000002", delivered: 10_099, dropped: 501},
		{name: "delivered counter regression", elapsed: time.Minute, bootId: "00000000-0000-4000-8000-000000000001", delivered: 9_999, dropped: 501},
		{name: "drop counter regression", elapsed: time.Minute, bootId: "00000000-0000-4000-8000-000000000001", delivered: 10_099, dropped: 499},
		{name: "same clock", bootId: "00000000-0000-4000-8000-000000000001", delivered: 10_099, dropped: 501},
		{name: "regressed clock", elapsed: -time.Minute, bootId: "00000000-0000-4000-8000-000000000001", delivered: 10_099, dropped: 501},
		{name: "short interval", elapsed: 29 * time.Second, bootId: "00000000-0000-4000-8000-000000000001", delivered: 10_099, dropped: 501},
	} {
		now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		sample := proxyMemoryDeltaFixtureSample()
		sample.udpInDatagrams = 10_000
		sample.udpRcvbufErrors = 500
		source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			return proxyMemoryFixture(sample), nil
		}}
		settings := proxyMemorySyntheticSettings(source)
		settings.Now = func() time.Time { return now }
		signal := NewProxyMemorySignal()
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Fatalf("%s: first cumulative sample: alerts=%d err=%v", c.name, len(alerts), err)
		}
		now = now.Add(c.elapsed)
		sample.bootID = c.bootId
		sample.udpInDatagrams = c.delivered
		sample.udpRcvbufErrors = c.dropped
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Errorf("%s: warmup alerted: alerts=%d err=%v", c.name, len(alerts), err)
		}
		now = now.Add(time.Minute)
		sample.udpInDatagrams += 10_000
		sample.udpRcvbufErrors += 250
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: valid interval after warmup: alerts=%d err=%v", c.name, len(alerts), err)
		}
		drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
		if drops.Severity != SeverityPage || !strings.Contains(drops.Observed, "udp_in_datagrams_delta=10000 ") ||
			!strings.Contains(drops.Observed, "udp_rcvbuf_errors_delta=250 ") || !strings.Contains(drops.Observed, "sample_elapsed_s=60 ") {
			t.Errorf("%s: valid interval did not use the warmup sample as its new baseline", c.name)
		}
	}
}

// Missing and malformed samples remain unknown and never replace valid counters.
func TestProxyMemorySignalUdpIncompleteSourceRemainsUnknown(t *testing.T) {
	for _, c := range []struct {
		name     string
		from     string
		to       string
		failure  error
		bootOnly bool
	}{
		{name: "missing delivered", from: "udp_in_datagrams 10000\n"},
		{name: "missing dropped", from: "udp_rcvbuf_errors 500\n"},
		{name: "missing boot", from: "boot_id 00000000-0000-4000-8000-000000000001\n", bootOnly: true},
		{name: "malformed delivered", from: "udp_in_datagrams 10000\n", to: "udp_in_datagrams synthetic-private-value\n"},
		{name: "negative dropped", from: "udp_rcvbuf_errors 500\n", to: "udp_rcvbuf_errors -1\n"},
		{name: "malformed boot", from: "boot_id 00000000-0000-4000-8000-000000000001\n", to: "boot_id synthetic private value\n", bootOnly: true},
		{name: "unavailable source", failure: errors.New("synthetic-private-transport")},
	} {
		now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		sample := proxyMemoryDeltaFixtureSample()
		sample.udpInDatagrams = 10_000
		sample.udpRcvbufErrors = 500
		output := proxyMemoryFixture(sample)
		var sourceErr error
		source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			return output, sourceErr
		}}
		settings := proxyMemorySyntheticSettings(source)
		settings.Now = func() time.Time { return now }
		signal := NewProxyMemorySignal()
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Fatalf("%s: first sample: alerts=%d err=%v", c.name, len(alerts), err)
		}
		now = now.Add(time.Minute)
		if c.failure == nil {
			if !strings.Contains(output, c.from) {
				t.Fatalf("%s: incomplete-source fixture did not reach its named field", c.name)
			}
			output = strings.Replace(output, c.from, c.to, 1)
		}
		sourceErr = c.failure
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: unavailable interval: alerts=%d err=%v", c.name, len(alerts), err)
		}
		unknown := requireAlertClass(t, alerts, "cannot-observe")
		visibilityTarget := "proxy-1/proxy-memory"
		if c.bootOnly {
			visibilityTarget = "proxy-1/proxy-udp-receive-drops"
		}
		if unknown.Identity() != "monitor/visibility|cannot-observe|"+visibilityTarget+"|" || !unknown.unobservable {
			t.Errorf("%s: unavailable UDP evidence lost its unknown identity", c.name)
		}
		var jsonLines strings.Builder
		if err := alerts.WriteJSONL(&jsonLines); err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"synthetic-private-value", "synthetic private value", "synthetic-private-transport"} {
			if strings.Contains(unknown.Markdown(), private) || strings.Contains(jsonLines.String(), private) {
				t.Errorf("%s: unavailable evidence leaked a synthetic private marker", c.name)
			}
		}
		now = now.Add(time.Minute)
		sample.udpInDatagrams += 20_000
		sample.udpRcvbufErrors += 500
		output = proxyMemoryFixture(sample)
		sourceErr = nil
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: restored interval: alerts=%d err=%v", c.name, len(alerts), err)
		}
		drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
		if drops.Severity != SeverityPage || !strings.Contains(drops.Observed, "sample_elapsed_s=120 ") ||
			!strings.Contains(drops.Observed, "udp_rcvbuf_errors_delta=500 ") || !strings.Contains(drops.Observed, "udp_rcvbuf_errors_rate_per_min=250.00 ") {
			t.Errorf("%s: unavailable sample replaced the prior complete counter baseline", c.name)
		}
	}
}

// A failed sibling host cannot hide the complete host's independent ratio alert.
func TestProxyMemorySignalUdpPartialHostCoverage(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	partial := false
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if partial && host.Name == "proxy-2" {
			return "proxy_host 1\n", nil
		}
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "proxy-2", Roles: []string{"services"}, Proxy: &ProxyHostSettings{PublicHostname: "proxy-2.example"},
	})
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("first host samples: alerts=%d err=%v", len(alerts), err)
	}
	now = now.Add(time.Minute)
	sample.udpInDatagrams = 99
	sample.udpRcvbufErrors = 1
	partial = true
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("partial UDP interval: alerts=%d err=%v, want complete-host page plus sibling unknown", len(alerts), err)
	}
	drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if drops.Target != "proxy-1" || drops.Severity != SeverityPage || unknown.Target != "proxy-2/proxy-memory" || !unknown.unobservable {
		t.Fatal("partial host coverage changed the complete-host failure or sibling unknown")
	}
}

// Severity changes reuse one identity while an in-band interval stays healthy.
func TestProxyMemorySignalUdpStableIdentityAcrossBands(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("first sample: alerts=%d err=%v", len(alerts), err)
	}
	for _, c := range []struct {
		delivered int64
		dropped   int64
		want      Severity
	}{
		{delivered: 999_900, dropped: 100, want: SeverityWarn},
		{delivered: 99, dropped: 1, want: SeverityPage},
		{delivered: 9_990_000, dropped: 10_000, want: SeverityPage},
		{delivered: 999_900, dropped: 100, want: SeverityWarn},
		{delivered: 100, dropped: 1},
	} {
		now = now.Add(time.Minute)
		sample.udpInDatagrams += c.delivered
		sample.udpRcvbufErrors += c.dropped
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" {
			if len(alerts) != 0 {
				t.Errorf("in-band UDP interval returned %d alerts", len(alerts))
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("UDP severity transition to %s returned %d alerts", c.want, len(alerts))
		}
		if alerts[0].Identity() != "proxy/host-memory|proxy-udp-receive-drops|proxy-1|receive-buffer" || alerts[0].Severity != c.want {
			t.Errorf("UDP severity transition changed identity or did not reach %s", c.want)
		}
	}
}

// Replays one valid same-boot interval through the parser, counter reducer and adapter.
func runProxyMemoryUdpInterval(t *testing.T, elapsed time.Duration, delivered, dropped int64) Alerts {
	t.Helper()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	sample.bootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	sample.udpInDatagrams = 1_000_000
	sample.udpRcvbufErrors = 10_000
	sample.oomLine = "synthetic-private-journal"
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "proxy-1" || !strings.Contains(command, proxyMemoryMarker) {
			t.Fatal("UDP fixture reached an unexpected host or command")
		}
		return proxyMemoryFixture(sample) + "ignored_detail synthetic-private-source\n", nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("first cumulative UDP sample: alerts=%d err=%v", len(alerts), err)
	}
	now = now.Add(elapsed)
	sample.udpInDatagrams += delivered
	sample.udpRcvbufErrors += dropped
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}
