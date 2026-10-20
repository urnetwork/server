// Exercises kernel boot authority without treating unavailable identity as a generation.
package monitor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const proxyMemoryBootTestId = "00000000-0000-4000-8000-000000000001"
const proxyMemoryBootTestNextId = "00000000-0000-4000-8000-000000000002"

// Two unavailable identities cannot authorize a same-generation loss interval.
func TestProxyMemorySignalUdpBootUnknownCannotQualifyDelta(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	sample.bootID = "unknown"
	sample.udpInDatagrams = 10_000
	sample.udpRcvbufErrors = 500
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	observations := []Alerts{}
	for index := 0; index < 2; index++ {
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		observations = append(observations, alerts)
		now = now.Add(time.Minute)
		sample.udpInDatagrams += 10_000
		sample.udpRcvbufErrors += 250
	}
	second := observations[1]
	if len(second) != 1 || second[0].Class != "cannot-observe" {
		t.Fatal("UDP boot authority: two unknown boot identities with increasing counters authorized a loss or healthy interval; want cannot-observe")
	}
	for _, alerts := range observations {
		requireProxyMemoryBootUnknown(t, alerts)
	}
}

// Only one complete kernel UUID gives an otherwise complete sample UDP authority.
func TestProxyMemorySignalUdpBootMalformedRemainsUnknown(t *testing.T) {
	for _, c := range []struct {
		name string
		rows string
	}{
		{name: "missing"},
		{name: "empty", rows: "boot_id\n"},
		{name: "empty value", rows: "boot_id \n"},
		{name: "whitespace", rows: "boot_id \t \n"},
		{name: "unavailable", rows: "boot_id unknown\n"},
		{name: "opaque token", rows: "boot_id synthetic-private-boot\n"},
		{name: "truncated UUID", rows: "boot_id " + proxyMemoryBootTestId[:35] + "\n"},
		{name: "invalid hex", rows: "boot_id z0000000-0000-4000-8000-000000000001\n"},
		{name: "embedded space", rows: "boot_id synthetic private boot\n"},
		{name: "trailing data", rows: "boot_id " + proxyMemoryBootTestId + "-private\n"},
		{name: "duplicate same", rows: "boot_id " + proxyMemoryBootTestId + "\nboot_id " + proxyMemoryBootTestId + "\n"},
		{name: "duplicate changed", rows: "boot_id " + proxyMemoryBootTestId + "\nboot_id " + proxyMemoryBootTestNextId + "\n"},
		{name: "unknown then known", rows: "boot_id unknown\nboot_id " + proxyMemoryBootTestId + "\n"},
		{name: "known then unknown", rows: "boot_id " + proxyMemoryBootTestId + "\nboot_id unknown\n"},
		{name: "empty then known", rows: "boot_id\nboot_id " + proxyMemoryBootTestId + "\n"},
		{name: "known then empty", rows: "boot_id " + proxyMemoryBootTestId + "\nboot_id\n"},
	} {
		now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		sample := proxyMemoryDeltaFixtureSample()
		source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			return proxyMemoryBootFixture(sample, c.rows), nil
		}}
		settings := proxyMemorySyntheticSettings(source)
		settings.Now = func() time.Time { return now }
		signal := NewProxyMemorySignal()
		for index := 0; index < 2; index++ {
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
				t.Fatalf("%s: invalid boot observation %d did not remain unknown: alerts=%d err=%v", c.name, index+1, len(alerts), err)
			}
			requireProxyMemoryBootUnknown(t, alerts)
			now = now.Add(time.Minute)
			sample.udpInDatagrams += 10_000
			sample.udpRcvbufErrors += 250
		}
	}
}

// The collector must discard partial output even if the failed read emitted a UUID.
func TestProxyMemoryScriptUdpBootReadFailureDiscardsOutput(t *testing.T) {
	const hostMarker = "printf 'proxy_host 1\\n'\n"
	start := strings.Index(proxyMemoryScript, hostMarker)
	end := strings.Index(proxyMemoryScript, "warpctl_path=")
	if start < 0 || end <= start {
		t.Fatal("boot collector fixture could not locate the actual source stanza")
	}
	start += len(hostMarker)
	directory := t.TempDir()
	bootPath := filepath.Join(directory, "synthetic-boot-id")
	shimPath := filepath.Join(directory, "tr")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\ncat\nexit \"$SYNTHETIC_BOOT_EXIT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		body string
		exit string
		want string
	}{
		{name: "failed read with complete-looking output", body: proxyMemoryBootTestId + "\n", exit: "1", want: "unknown"},
		{name: "failed empty read", exit: "1", want: "unknown"},
		{name: "successful empty read", exit: "0", want: "unknown"},
		{name: "successful known read", body: proxyMemoryBootTestId + "\n", exit: "0", want: proxyMemoryBootTestId},
	} {
		if err := os.WriteFile(bootPath, []byte(c.body), 0600); err != nil {
			t.Fatal(err)
		}
		commandText := strings.ReplaceAll(proxyMemoryScript[start:end], "/proc/sys/kernel/random/boot_id", shellSingleQuote(bootPath))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, "/bin/sh", "-c", commandText)
		command.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "SYNTHETIC_BOOT_EXIT=" + c.exit}
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: isolated collector failed: %v", c.name, err)
		}
		if string(output) != "boot_id "+c.want+"\n" {
			t.Fatalf("UDP boot read authority: %s retained unqualified read output", c.name)
		}
	}
}

// A real same-UUID interval retains its page, healthy control, identity and privacy.
func TestProxyMemorySignalUdpBootKnownSameGeneration(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	sample.udpInDatagrams = 10_000
	sample.udpRcvbufErrors = 500
	sample.oomLine = "synthetic-private-journal"
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("known boot first sample did not warm up: alerts=%d err=%v", len(alerts), err)
	}
	now = now.Add(time.Minute)
	sample.udpInDatagrams += 10_000
	sample.udpRcvbufErrors += 250
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("known same-boot loss interval: alerts=%d err=%v", len(alerts), err)
	}
	drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
	if drops.Identity() != "proxy/host-memory|proxy-udp-receive-drops|proxy-1|receive-buffer" || drops.Severity != SeverityPage || drops.Sustain != 1 {
		t.Fatal("known same-boot interval lost its stable immediate page identity")
	}
	for _, want := range []string{"sample_elapsed_s=60 ", "udp_in_datagrams_delta=10000 ", "udp_rcvbuf_errors_delta=250 ", "udp_rcvbuf_errors_rate_per_min=250.00 "} {
		if !strings.Contains(drops.Observed, want) {
			t.Errorf("known same-boot interval missing %q", want)
		}
	}
	requireProxyMemoryBootPrivate(t, alerts)
	now = now.Add(time.Minute)
	sample.udpInDatagrams += 100_000
	sample.udpRcvbufErrors += 9
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("known same-boot healthy control: alerts=%d err=%v", len(alerts), err)
	}
}

// An unknown interval never overwrites the last proven generation's counters.
func TestProxyMemorySignalUdpBootRecoveryRetainsOnlyKnownBaseline(t *testing.T) {
	for _, c := range []struct {
		name          string
		hasBaseline   bool
		recoveredBoot string
		wantDelta     bool
	}{
		{name: "first known sample after unknown", recoveredBoot: proxyMemoryBootTestId},
		{name: "same known UUID after unknown", hasBaseline: true, recoveredBoot: proxyMemoryBootTestId, wantDelta: true},
		{name: "changed known UUID after unknown", hasBaseline: true, recoveredBoot: proxyMemoryBootTestNextId},
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
		if c.hasBaseline {
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("%s: known baseline: alerts=%d err=%v", c.name, len(alerts), err)
			}
			now = now.Add(time.Minute)
		}
		sample.bootID = "unknown"
		sample.udpInDatagrams = 900_000
		sample.udpRcvbufErrors = 90_000
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireProxyMemoryBootUnknown(t, alerts)
		now = now.Add(time.Minute)
		sample.bootID = c.recoveredBoot
		sample.udpInDatagrams = 30_000
		sample.udpRcvbufErrors = 1_000
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if c.wantDelta {
			if len(alerts) != 1 {
				t.Fatalf("%s: valid endpoint UUIDs lost the complete 120-second interval", c.name)
			}
			drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
			if drops.Severity != SeverityPage || !strings.Contains(drops.Observed, "sample_elapsed_s=120 ") ||
				!strings.Contains(drops.Observed, "udp_rcvbuf_errors_delta=500 ") || !strings.Contains(drops.Observed, "udp_rcvbuf_errors_rate_per_min=250.00 ") {
				t.Fatalf("%s: invalid sample replaced the last authoritative baseline", c.name)
			}
		} else if len(alerts) != 0 {
			t.Fatalf("%s: a new generation qualified before its first valid pair", c.name)
		}
		now = now.Add(30 * time.Second)
		sample.udpInDatagrams += 10_000
		sample.udpRcvbufErrors += 250
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: recovered known pair at 30 seconds: alerts=%d err=%v", c.name, len(alerts), err)
		}
		drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
		if drops.Severity != SeverityPage || !strings.Contains(drops.Observed, "sample_elapsed_s=30 ") || !strings.Contains(drops.Observed, "udp_rcvbuf_errors_delta=250 ") {
			t.Fatalf("%s: recovery did not advance to the latest valid baseline", c.name)
		}
	}
}

// Neither an invalid current sample nor a legacy invalid cache entry is reusable.
func TestProxyMemoryObserveUdpBootInvalidBaselineIsDiscarded(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, bootId := range []string{"unknown", "", "synthetic-private-boot", proxyMemoryBootTestId[:35]} {
		probe := &proxyMemoryProbe{udpSamples: map[string]proxyUDPCounterSample{
			"proxy-1": {bootID: bootId, observedAt: now, inDatagrams: 10_000, rcvbufErrors: 500},
		}}
		sample := proxyMemoryDeltaFixtureSample()
		sample.bootID = bootId
		sample.udpInDatagrams = 20_000
		sample.udpRcvbufErrors = 750
		if _, valid := probe.observeUDP("proxy-1", sample, now.Add(time.Minute)); valid {
			t.Fatal("UDP boot baseline authority: invalid cached identity authorized a counter delta")
		}
		if _, retained := probe.udpSamples["proxy-1"]; retained {
			t.Fatal("UDP boot baseline authority: invalid cached identity remained reusable")
		}
		sample.bootID = proxyMemoryBootTestId
		if _, valid := probe.observeUDP("proxy-1", sample, now.Add(2*time.Minute)); valid {
			t.Fatal("first known boot sample used an invalid cached baseline")
		}
		known := probe.udpSamples["proxy-1"]
		sample.bootID = bootId
		if _, valid := probe.observeUDP("proxy-1", sample, now.Add(3*time.Minute)); valid || probe.udpSamples["proxy-1"] != known {
			t.Fatal("invalid current boot replaced or consumed a valid known baseline")
		}
		sample.bootID = proxyMemoryBootTestId
		sample.udpInDatagrams += 20_000
		sample.udpRcvbufErrors += 500
		delta, valid := probe.observeUDP("proxy-1", sample, now.Add(4*time.Minute))
		if !valid || delta.elapsed != 2*time.Minute || delta.inDatagrams != 20_000 || delta.rcvbufErrors != 500 {
			t.Fatal("matching known boot endpoints lost the full authoritative elapsed interval")
		}
	}
}

// Missing boot authority cannot suppress separately proven memory and OOM evidence.
func TestProxyMemorySignalUdpBootUnknownPreservesMemoryFindings(t *testing.T) {
	for _, bootRows := range []string{"boot_id unknown\n", "", "boot_id synthetic private boot\n"} {
		sample := proxyMemoryDeltaFixtureSample()
		sample.recentProxyOOMKills = 1
		source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			return proxyMemoryBootFixture(sample, bootRows), nil
		}}
		alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
		if err != nil || len(alerts) != 2 {
			t.Fatalf("missing boot authority suppressed independently observed OOM: alerts=%d err=%v", len(alerts), err)
		}
		oom := requireAlertClass(t, alerts, "proxy-host-oom")
		if oom.Severity != SeverityPage || oom.SignalNumber != "14.7" || oom.SignalKey != "proxy-memory" {
			t.Fatal("independent OOM finding lost its page identity")
		}
		unknown := requireAlertClass(t, alerts, "cannot-observe")
		requireProxyMemoryBootUnknown(t, Alerts{unknown})
	}
}

// One host's absent boot identity leaves a complete sibling's loss interval intact.
func TestProxyMemorySignalUdpBootPartialHostCoverage(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "proxy-2" {
			return proxyMemoryBootFixture(sample, "boot_id unknown\n"), nil
		}
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "proxy-2", Roles: []string{"services"}, Proxy: &ProxyHostSettings{PublicHostname: "proxy-2.example"}})
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatalf("partial boot first sample: alerts=%d err=%v", len(alerts), err)
	}
	now = now.Add(time.Minute)
	sample.udpInDatagrams += 10_000
	sample.udpRcvbufErrors += 250
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("partial boot interval lost sibling evidence: alerts=%d err=%v", len(alerts), err)
	}
	drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if drops.Target != "proxy-1" || drops.Severity != SeverityPage || unknown.Target != "proxy-2/proxy-udp-receive-drops" || !unknown.unobservable {
		t.Fatal("boot authority crossed host ownership or suppressed the complete host")
	}
	requireProxyMemoryBootPrivate(t, alerts)
}

// Replaces only the native boot row, bypassing the legacy fixture's empty-id default.
func proxyMemoryBootFixture(sample proxyMemorySample, bootRows string) string {
	sample.bootID = proxyMemoryBootTestId
	return strings.Replace(proxyMemoryFixture(sample), "boot_id "+proxyMemoryBootTestId+"\n", bootRows, 1)
}

// The public unknown identity is specific to UDP, without publishing source values.
func requireProxyMemoryBootUnknown(t *testing.T, alerts Alerts) {
	t.Helper()
	if len(alerts) != 1 {
		t.Fatalf("missing boot identity produced %d findings, want one UDP visibility finding", len(alerts))
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if unknown.Identity() != "monitor/visibility|cannot-observe|proxy-1/proxy-udp-receive-drops|" || !unknown.unobservable ||
		unknown.Severity != SeverityWarn || unknown.SignalNumber != "14.7" || unknown.SignalKey != "proxy-memory" {
		t.Fatal("missing boot identity lost its scoped unknown identity")
	}
	if !strings.Contains(unknown.Observed, "boot_identity=unavailable") {
		t.Fatal("missing boot observation does not identify its authority gap")
	}
	requireProxyMemoryBootPrivate(t, alerts)
}

// Boot identity and raw synthetic source detail stay out of Markdown and JSONL.
func requireProxyMemoryBootPrivate(t *testing.T, alerts Alerts) {
	t.Helper()
	var jsonLines strings.Builder
	if err := alerts.WriteJSONL(&jsonLines); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{proxyMemoryBootTestId, proxyMemoryBootTestNextId, "synthetic-private-boot", "synthetic private boot", "synthetic-private-journal"} {
		if strings.Contains(alerts.Markdown(), private) || strings.Contains(jsonLines.String(), private) {
			t.Fatal("boot observation leaked a private synthetic source value")
		}
	}
}
