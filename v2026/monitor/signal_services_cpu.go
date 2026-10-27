package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SIGNALS.md §26.2 maps to signal_services_cpu.go and
// signal_services_cpu_test.go. The services edges are the hosts the active
// placement runs api, connect and taskworker on (edge-0, edge-1, edge-3 and
// edge-4 on 2026-10-26). In incident A connect ran with GOMAXPROCS 72 there
// and filled their CPUs, and its CPU-starved clients held their transactions
// open, which queued PgBouncer (§26.1). The probe reads the busy share of the
// edges together and of each edge at each minute of its holds: the
// non-idle, non-I/O-wait share of node_cpu_seconds_total, summed before
// dividing so each host weighs by its CPUs. The edges together are judged
// only at evaluations where every edge answered.
func NewServicesCpuSignal() Signal {
	return NewServicesCpuSignalWithSettings(DefaultServicesCpuSettings())
}

// The signal with explicit thresholds.
func NewServicesCpuSignalWithSettings(settings *ServicesCpuSettings) Signal {
	return &signalAdapter{
		number: "26.2", key: "services-cpu", name: "Services-tier CPU",
		probe: servicesCpuProbe{settings: settings},
	}
}

const servicesCpuProbeId = "mimir/services-cpu"

// The thresholds and bounds of §26.2.
type ServicesCpuSettings struct {
	// the services whose active placement names the services edges
	Services []string
	// the rate window of each evaluation
	Window time.Duration
	// the busy share of the edges together above TierBusy for TierHold warns
	TierBusy float64
	TierHold time.Duration
	// the busy share of one edge above HostBusy for HostHold warns
	HostBusy float64
	HostHold time.Duration
	// the most hosts and gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.2.
func DefaultServicesCpuSettings() *ServicesCpuSettings {
	return &ServicesCpuSettings{
		Services:          []string{"api", "connect", "taskworker"},
		Window:            5 * time.Minute,
		TierBusy:          0.80,
		TierHold:          10 * time.Minute,
		HostBusy:          0.85,
		HostHold:          30 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *ServicesCpuSettings) valid() bool {
	return self != nil && 0 < len(self.Services) && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 < self.TierBusy && self.TierBusy <= 1 && 0 < self.HostBusy && self.HostBusy <= 1 &&
		0 < self.MaxListed && self.StormReadSettings.valid(self.TierHold, self.HostHold)
}

type servicesCpuProbe struct {
	settings *ServicesCpuSettings
}

func (servicesCpuProbe) id() string   { return servicesCpuProbeId }
func (servicesCpuProbe) tier() string { return tierWarn }

func (self servicesCpuProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self servicesCpuProbe) effectiveSettings() *ServicesCpuSettings {
	if self.settings == nil {
		return DefaultServicesCpuSettings()
	}
	return self.settings
}

// §26.2's two expressions over the placed edges.
func servicesCpuQuery(environment string, hosts []string, settings *ServicesCpuSettings) string {
	return strings.Join([]string{
		fleetPart("tier_busy", stormBusyShareExpression(environment, hosts, settings.Window, "")),
		fleetPart("host_busy", stormBusyShareExpression(environment, hosts, settings.Window, "host")),
	}, " or ")
}

var servicesCpuShapes = map[string]stormShape{
	"tier_busy": {},
	"host_busy": {keys: []string{"host"}},
}

func (self servicesCpuProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	tierTarget := env.cfg.env + "/services-tier"
	unobservableTarget := env.cfg.env + "/services-cpu"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(servicesCpuProbeId, "services-cpu-unobservable", unobservableTarget, "§26.2", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	hosts, placementGaps := stormPlacedHosts(env, settings.Services)
	if len(hosts) == 0 {
		return []finding{unobservable([]string{"placement_missing"}, placementGaps)}, nil
	}
	history, err := readStormHistory(ctx, env, servicesCpuQuery(env.cfg.env, hosts, settings), servicesCpuShapes, settings.holdSteps(max(settings.TierHold, settings.HostHold)), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{unobservable([]string{stormReadReason(err)}, nil)}, nil
	}

	reasons := history.invalidReasons()
	if 0 < len(placementGaps) {
		reasons = append(reasons, "placement_incomplete")
	}
	missing := []string{}
	findings := []finding{}
	hostNow := []string{}
	for _, name := range hosts {
		hostAt := func(step int64) stormReading { return history.reading("host_busy", name, step) }
		hostNow = append(hostNow, name+":"+stormNumber(hostAt(history.at()), "%.3f"))
		hold := stormHoldOf(history.last(settings.holdSteps(settings.HostHold)), stormLevel{threshold: settings.HostBusy}, hostAt)
		if !hold.complete {
			missing = append(missing, name+"[missing]")
		}
		switch {
		case hold.held:
			findings = append(findings, servicesCpuFinding(name, settings, fmt.Sprintf("Services edge %s was more than %.0f%% busy at every evaluation for %s", name, 100*settings.HostBusy, settings.HostHold),
				fmt.Sprintf("host=%s busy_share_now=%s level=%.2f hold=%s evaluation_unix=%d", name, stormNumber(hostAt(history.at()), "%.3f"), settings.HostBusy, settings.HostHold, history.at())))
		case hold.clear:
			findings = append(findings, healthyFinding(servicesCpuProbeId, tierWarn, "services-cpu-busy", name))
		default:
			findings = append(findings, stormClearHold(servicesCpuProbeId, "services-cpu-busy", name))
		}
	}

	// the edges together count only at evaluations where every edge answered
	tierAt := func(step int64) stormReading {
		if 0 < len(placementGaps) {
			return stormReading{}
		}
		for _, name := range hosts {
			if !history.reading("host_busy", name, step).present {
				return stormReading{}
			}
		}
		return history.reading("tier_busy", "", step)
	}
	tier := stormHoldOf(history.last(settings.holdSteps(settings.TierHold)), stormLevel{threshold: settings.TierBusy}, tierAt)
	switch {
	case tier.held:
		findings = append(findings, servicesCpuFinding(tierTarget, settings, fmt.Sprintf("The %d services edges together were more than %.0f%% busy at every evaluation for %s", len(hosts), 100*settings.TierBusy, settings.TierHold),
			fmt.Sprintf("busy_share_now=%s level=%.2f hold=%s hosts=%s evaluation_unix=%d", stormNumber(tierAt(history.at()), "%.3f"), settings.TierBusy, settings.TierHold, stormListed(hostNow, settings.MaxListed), history.at())))
	case tier.clear:
		findings = append(findings, healthyFinding(servicesCpuProbeId, tierWarn, "services-cpu-busy", tierTarget))
	default:
		findings = append(findings, stormClearHold(servicesCpuProbeId, "services-cpu-busy", tierTarget))
	}
	if !tier.complete && len(placementGaps) == 0 {
		missing = append(missing, "services-tier[missing]")
	}
	if 0 < len(missing) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, append(placementGaps, missing...)))
	} else {
		findings = append(findings, healthyFinding(servicesCpuProbeId, tierWarn, "services-cpu-unobservable", unobservableTarget))
	}
	return findings, nil
}

// One services-cpu-busy finding: the edges together or one edge.
func servicesCpuFinding(target string, settings *ServicesCpuSettings, symptom string, observed string) finding {
	return finding{
		probeId: servicesCpuProbeId, tier: tierWarn, class: "services-cpu-busy", target: target, sustain: 1,
		symptom:   symptom,
		mechanism: "Busy is the non-idle, non-I/O-wait share of CPU time. When the services edges run out of CPU, their processes answer late and their database clients hold transactions open, so PgBouncer's servers stay busy and clients queue (§26.1). In incident A connect ran with GOMAXPROCS 72 on these hosts and filled their CPUs from 2026-10-25 19:00Z until it moved to 16 cores on 10-26.",
		baseline:  fmt.Sprintf("The edges together 46-56%% busy on a healthy day (2026-10-20) and at most 72%% on any day from 10-19 to 10-24. WARN when the edges together pass %.0f%% for %s, or one edge passes %.0f%% for %s, at every one-minute evaluation. No PAGE: §26.1 pages the effect on customers.", 100*settings.TierBusy, settings.TierHold, 100*settings.HostBusy, settings.HostHold),
		observed:  observed,
		evidence:  fmt.Sprintf("One bounded Mimir range read of node_cpu_seconds_total rates over %s for the placed services edges, together and by host, through a services host's loopback Mimir.", stormDuration(settings.Window)),
		context:   "A placement hot spot can hold one edge high for hours while customers are fine (edge-3 ran 76-80% on 10-20), hence the long hold on one edge. A rollout's overlap runs two generations of a service for minutes (§8.2). Busy share is not run-queue delay: read host-pressure (§8.14a) beside it.",
		action:    "Read CPU by service on the hot hosts (process_cpu_seconds_total) and §8.14a. Hold connect and alt rollouts. Reduce load (connect's cores, §24 placement, capacity) rather than restarting.",
		verify:    fmt.Sprintf("Every evaluation of a complete hold reads the edges together at or under %.0f%% and each edge at or under %.0f%%.", 100*settings.TierBusy, 100*settings.HostBusy),
		playbook:  "SIGNALS.md §26.2 and §8.14",
	}
}
