package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestProxyMemorySignalSyntheticGlobalOOM(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, proxyMemoryMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		return proxyMemoryFixture(proxyMemorySample{
			warpctlRolloutGuard:  proxyRolloutGuardDrainOnly,
			memTotalKiB:          98284728,
			memAvailableKiB:      36805604,
			swapTotalKiB:         8388604,
			swapFreeKiB:          7788196,
			runningUnits:         10,
			proxyProcesses:       10,
			proxyRSSKiB:          52842940,
			proxyMaxRSSKiB:       5498500,
			proxyMemoryUnbounded: 10,
			udpRcvbufErrors:      122012,
			recentProxyOOMKills:  1,
			oomProxyProcesses:    19,
			oomLine:              "Out of memory: Killed process 2515221 (bringyour-proxy) anon-rss:5014876kB",
		}), nil
	}}
	settings := proxyMemorySyntheticSettings(source)

	alerts, err := NewProxyMemorySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	oom := requireAlertClass(t, alerts, "proxy-host-oom")
	if oom.Severity != SeverityPage || oom.SignalNumber != "14.7" || oom.SignalKey != "proxy-memory" {
		t.Fatalf("wrong OOM identity: %+v", oom)
	}
	markdown := oom.Markdown()
	for _, want := range []string{
		"19 proxy processes for 10 running block units",
		"warpctl_rollout_guard=drain-only",
		"udp_rcvbuf_errors_since_boot=122012",
		"7e2075c",
		"Do not restart WireGuard",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("OOM alert missing %q:\n%s", want, markdown)
		}
	}
	headroom := requireAlertClass(t, alerts, "proxy-rollout-headroom")
	if headroom.Severity != SeverityWarn || !strings.Contains(headroom.Observed, "capacity_deficit_gib=23.29") {
		t.Fatalf("wrong Fireside headroom alert: %+v", headroom)
	}
}

func TestProxyMemorySignalSyntheticUnsafeLiveOverlap(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, proxyMemoryMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		return proxyMemoryFixture(proxyMemorySample{
			warpctlRolloutGuard:  proxyRolloutGuardFull,
			memTotalKiB:          98284728,
			memAvailableKiB:      10 * 1024 * 1024,
			swapTotalKiB:         8388604,
			swapFreeKiB:          512 * 1024,
			runningUnits:         10,
			proxyProcesses:       13,
			proxyRSSKiB:          66 * 1024 * 1024,
			proxyMaxRSSKiB:       5500 * 1024,
			proxyMemoryUnbounded: 13,
			udpRcvbufErrors:      121454,
		}), nil
	}}

	alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	overlap := requireAlertClass(t, alerts, "proxy-rollout-overlap")
	if overlap.Severity != SeverityPage {
		t.Fatalf("live overlap severity = %q", overlap.Severity)
	}
	for _, want := range []string{"running_block_units=10", "proxy_processes=13", "Pause new candidates"} {
		if !strings.Contains(overlap.Markdown(), want) {
			t.Fatalf("live overlap alert missing %q:\n%s", want, overlap.Markdown())
		}
	}
}

func TestProxyMemorySignalSyntheticHealthyHost(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, proxyMemoryMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		return proxyMemoryFixture(proxyMemorySample{
			warpctlRolloutGuard:  proxyRolloutGuardFull,
			memTotalKiB:          131314856,
			memAvailableKiB:      68950196,
			swapTotalKiB:         8388604,
			swapFreeKiB:          8388604,
			runningUnits:         10,
			proxyProcesses:       10,
			proxyRSSKiB:          53323752,
			proxyMaxRSSKiB:       5531060,
			proxyMemoryUnbounded: 10,
			udpRcvbufErrors:      25,
		}), nil
	}}

	alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy proxy host alerted: %+v", alerts)
	}
}

func TestProxyMemorySignalSyntheticStaleRolloutGuard(t *testing.T) {
	for _, guard := range []string{proxyRolloutGuardDrainOnly, proxyRolloutGuardDisabled} {
		t.Run(guard, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
				if !strings.Contains(command, proxyMemoryMarker) {
					return "", errors.New("unexpected synthetic host command")
				}
				return proxyMemoryFixture(proxyMemorySample{
					warpctlRolloutGuard:  guard,
					memTotalKiB:          128 * 1024 * 1024,
					memAvailableKiB:      80 * 1024 * 1024,
					swapTotalKiB:         8 * 1024 * 1024,
					swapFreeKiB:          8 * 1024 * 1024,
					runningUnits:         10,
					proxyProcesses:       10,
					proxyRSSKiB:          50 * 1024 * 1024,
					proxyMaxRSSKiB:       5 * 1024 * 1024,
					proxyMemoryUnbounded: 10,
				}), nil
			}}

			alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 1 {
				t.Fatalf("stale rollout guard alerts = %d, want 1: %+v", len(alerts), alerts)
			}
			stale := requireAlertClass(t, alerts, "proxy-rollout-guard-stale")
			if stale.Severity != SeverityWarn {
				t.Fatalf("stale guard severity = %q", stale.Severity)
			}
			for _, want := range []string{
				"warpctl_rollout_guard=" + guard,
				"7e2075c",
				"restart every Warp service worker",
				"Do not test the fix by starting a full proxy rollout",
				"separate from the hardware headroom boundary",
			} {
				if !strings.Contains(stale.Markdown(), want) {
					t.Fatalf("stale rollout guard alert missing %q:\n%s", want, stale.Markdown())
				}
			}
		})
	}
}

func TestProxyMemorySignalSyntheticUnverifiedRolloutGuard(t *testing.T) {
	for _, guard := range []string{proxyRolloutGuardMissing, proxyRolloutGuardUnknown} {
		t.Run(guard, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
				if !strings.Contains(command, proxyMemoryMarker) {
					return "", errors.New("unexpected synthetic host command")
				}
				return proxyMemoryFixture(proxyMemorySample{
					warpctlRolloutGuard:  guard,
					memTotalKiB:          128 * 1024 * 1024,
					memAvailableKiB:      80 * 1024 * 1024,
					swapTotalKiB:         8 * 1024 * 1024,
					swapFreeKiB:          8 * 1024 * 1024,
					runningUnits:         10,
					proxyProcesses:       10,
					proxyRSSKiB:          50 * 1024 * 1024,
					proxyMaxRSSKiB:       5 * 1024 * 1024,
					proxyMemoryUnbounded: 10,
				}), nil
			}}

			alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 1 {
				t.Fatalf("unverified rollout guard alerts = %d, want 1: %+v", len(alerts), alerts)
			}
			unverified := requireAlertClass(t, alerts, "proxy-rollout-guard-unverified")
			for _, want := range []string{
				"warpctl_rollout_guard=" + guard,
				"Do not begin a proxy rollout",
				"7e2075c",
			} {
				if !strings.Contains(unverified.Markdown(), want) {
					t.Fatalf("unverified rollout guard alert missing %q:\n%s", want, unverified.Markdown())
				}
			}
		})
	}
}

func TestProxyMemorySignalSkipsNonProxyHost(t *testing.T) {
	called := false
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		called = true
		if !strings.Contains(command, proxyMemoryMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		return "proxy_host 0\n", nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "services-only", Roles: []string{"services"}}}

	alerts, err := NewProxyMemorySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("non-proxy host alerted: %+v", alerts)
	}
	if !called {
		t.Fatal("services host was not checked for running proxy units")
	}
}

func proxyMemorySyntheticSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{
		Name:  "proxy-1",
		Roles: []string{"services"},
		Proxy: &ProxyHostSettings{PublicHostname: "proxy.example"},
	}}
	return settings
}

func proxyMemoryFixture(sample proxyMemorySample) string {
	if sample.warpctlRolloutGuard == "" {
		sample.warpctlRolloutGuard = proxyRolloutGuardFull
	}
	return fmt.Sprintf(`proxy_host 1
warpctl_rollout_guard %s
mem_total_kib %d
mem_available_kib %d
swap_total_kib %d
swap_free_kib %d
running_units %d
proxy_processes %d
proxy_rss_kib %d
proxy_max_rss_kib %d
proxy_memory_bounded %d
proxy_memory_unbounded %d
proxy_memory_unknown %d
udp_rcvbuf_errors %d
kernel_journal_status %d
recent_proxy_oom_kills %d
oom_proxy_processes %d
oom_line %s
`, sample.warpctlRolloutGuard, sample.memTotalKiB, sample.memAvailableKiB, sample.swapTotalKiB, sample.swapFreeKiB,
		sample.runningUnits, sample.proxyProcesses, sample.proxyRSSKiB, sample.proxyMaxRSSKiB,
		sample.proxyMemoryBounded, sample.proxyMemoryUnbounded, sample.proxyMemoryUnknown,
		sample.udpRcvbufErrors, sample.kernelJournalStatus, sample.recentProxyOOMKills,
		sample.oomProxyProcesses, sample.oomLine)
}
