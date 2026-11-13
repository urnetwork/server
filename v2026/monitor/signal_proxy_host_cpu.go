package monitor

import (
	"context"
	"fmt"
	"time"
)

// SIGNALS.md §26.9 maps to signal_proxy_host_cpu.go and
// signal_proxy_host_cpu_test.go. The proxy hosts are the hosts the active
// placement runs the proxy on (crisp and fireside, 24 CPUs each, on
// 2026-10-26); each runs 10 proxy processes at GOMAXPROCS 8, up to 80 cores of
// demand on 24. In incident B their CPU rose late, after 77k-182k contract
// creations were already stuck, so this is the last of B's signals: read it
// with the stuck-creation entry (§26.4), not alone. The probe reads each
// host's busy share, the non-idle, non-I/O-wait share of node_cpu_seconds_total,
// at each minute of its hold.
func NewProxyHostCpuSignal() Signal {
	return NewProxyHostCpuSignalWithSettings(DefaultProxyHostCpuSettings())
}

// The signal with explicit thresholds.
func NewProxyHostCpuSignalWithSettings(settings *ProxyHostCpuSettings) Signal {
	return &signalAdapter{
		number: "26.9", key: "proxy-host-cpu", name: "Proxy host CPU",
		probe: proxyHostCpuProbe{settings: settings},
	}
}

const proxyHostCpuProbeId = "mimir/proxy-host-cpu"

// The thresholds and bounds of §26.9.
type ProxyHostCpuSettings struct {
	// the services whose active placement names the proxy hosts
	Services []string
	// the rate window of each evaluation
	Window time.Duration
	// the busy share of one host above HostBusy for Hold warns
	HostBusy float64
	Hold     time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.9.
func DefaultProxyHostCpuSettings() *ProxyHostCpuSettings {
	return &ProxyHostCpuSettings{
		Services:          []string{"proxy"},
		Window:            5 * time.Minute,
		HostBusy:          0.85,
		Hold:              10 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *ProxyHostCpuSettings) valid() bool {
	return self != nil && 0 < len(self.Services) && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 < self.HostBusy && self.HostBusy <= 1 && 0 < self.MaxListed && self.StormReadSettings.valid(self.Hold)
}

type proxyHostCpuProbe struct {
	settings *ProxyHostCpuSettings
}

func (proxyHostCpuProbe) id() string   { return proxyHostCpuProbeId }
func (proxyHostCpuProbe) tier() string { return tierWarn }

func (self proxyHostCpuProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self proxyHostCpuProbe) effectiveSettings() *ProxyHostCpuSettings {
	if self.settings == nil {
		return DefaultProxyHostCpuSettings()
	}
	return self.settings
}

// §26.9's expression over the placed proxy hosts.
func proxyHostCpuQuery(environment string, hosts []string, settings *ProxyHostCpuSettings) string {
	return fleetPart("host_busy", stormBusyShareExpression(environment, hosts, settings.Window, "host"))
}

var proxyHostCpuShapes = map[string]stormShape{
	"host_busy": {keys: []string{"host"}},
}

func (self proxyHostCpuProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/proxy-host-cpu"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(proxyHostCpuProbeId, "proxy-host-cpu-unobservable", unobservableTarget, "§26.9", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	hosts, placementGaps := stormPlacedHosts(env, settings.Services)
	if len(hosts) == 0 {
		return []finding{unobservable([]string{"placement_missing"}, placementGaps)}, nil
	}
	history, err := readStormHistory(ctx, env, proxyHostCpuQuery(env.cfg.env, hosts, settings), proxyHostCpuShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	reasons := history.invalidReasons()
	if 0 < len(placementGaps) {
		reasons = append(reasons, "placement_incomplete")
	}
	missing := []string{}
	findings := []finding{}
	for _, name := range hosts {
		hostAt := func(step int64) stormReading { return history.reading("host_busy", name, step) }
		hold := stormHoldOf(history.last(settings.holdSteps(settings.Hold)), stormLevel{threshold: settings.HostBusy}, hostAt)
		if !hold.complete {
			missing = append(missing, name+"[missing]")
		}
		switch {
		case hold.held:
			findings = append(findings, finding{
				probeId: proxyHostCpuProbeId, tier: tierWarn, class: "proxy-host-cpu", target: name, sustain: 1,
				symptom:   fmt.Sprintf("Proxy host %s was more than %.0f%% busy at every evaluation for %s", name, 100*settings.HostBusy, settings.Hold),
				mechanism: "Busy is the non-idle, non-I/O-wait share of CPU time. Each proxy host runs 10 proxy processes at GOMAXPROCS 8, up to 80 cores of demand on 24 CPUs. In incident B (2026-10-26 14:43Z to 18:08Z) a connect drain burst contract creation on every proxy process, each failed creation waited out its reservation cleanup, and the cleanup loops churned Redis connections until the hosts ran 88.6-89% busy.",
				baseline:  fmt.Sprintf("9-24%% busy on a healthy day (2026-10-20), 34-42%% with 250-418 replies/s on the morning of 10-26 and 38-51%% from 18:20Z on 10-26. WARN when a host passes %.0f%% at every one-minute evaluation for %s.", 100*settings.HostBusy, settings.Hold),
				observed:  fmt.Sprintf("host=%s busy_share_now=%s level=%.2f hold=%s evaluation_unix=%d", name, stormNumber(hostAt(history.at()), "%.3f"), settings.HostBusy, settings.Hold, history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of node_cpu_seconds_total rates over %s for the placed proxy hosts, by host, through a services host's loopback Mimir.", stormDuration(settings.Window)),
				context:   "This is the last of B's signals: by the time it fired (15:26Z) 77k-182k creations were stuck. A proxy rollout runs two generations per block for minutes (§14.7), and the recovery from a full outage reconnects every device at once (10-24).",
				action:    "Hold connect, alt and proxy rollouts and read the stuck-creation entry (§26.4), then the release-error and Redis entries (§26.6-§26.8). Same-image proxy restarts held for minutes only.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the host at or under %.0f%% busy.", settings.Hold, 100*settings.HostBusy),
				playbook:  "SIGNALS.md §26.9 and §26.4",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(proxyHostCpuProbeId, tierWarn, "proxy-host-cpu", name))
		default:
			findings = append(findings, stormClearHold(proxyHostCpuProbeId, "proxy-host-cpu", name))
		}
	}
	if 0 < len(missing) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, append(placementGaps, missing...)))
	} else {
		findings = append(findings, healthyFinding(proxyHostCpuProbeId, tierWarn, "proxy-host-cpu-unobservable", unobservableTarget))
	}
	return findings, nil
}
