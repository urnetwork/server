package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.13 maps to signal_proxy_heap_limit.go and
// signal_proxy_heap_limit_test.go. Each proxy host runs 10 proxy processes
// whose Go runtime collects to stay under a memory limit set per host
// (proxy/host_go_memory_limit.go). Near the limit GC runs back to back: from
// 15Z on 2026-11-14 heap plus stacks reached 6.31 GiB of the 6500 MiB limit,
// GC went from 0.6-1 to 10-13 cycles a minute per process, the hosts went
// from 3-5 to 16-23 busy cores of 24, and starved mints refused windows
// (§26.12, proxy/P90SPEED.md §4.2). The probe reads each host's highest
// process's heap plus stacks over its limit at each minute of its hold, with
// each block's highest ratio, and the host's GC cycles a minute and lowest
// limit as the alert's context. A process without a limit reports
// math.MaxInt64 as its limit and reads near zero. A maximum over fewer
// processes can only be lower, so a held band stands while a block is
// missing; a clear needs every placed block.
func NewProxyHeapLimitSignal() Signal {
	return NewProxyHeapLimitSignalWithSettings(DefaultProxyHeapLimitSettings())
}

// The signal with explicit thresholds.
func NewProxyHeapLimitSignalWithSettings(settings *ProxyHeapLimitSettings) Signal {
	return &signalAdapter{
		number: "26.13", key: "proxy-heap-limit", name: "Proxy heap at its Go memory limit",
		probe: proxyHeapLimitProbe{settings: settings},
	}
}

const proxyHeapLimitProbeId = "mimir/proxy-heap-limit"

// The thresholds and bounds of §26.13.
type ProxyHeapLimitSettings struct {
	// the service whose processes are read
	Service string
	// a host's highest heap plus stacks over its Go memory limit above Level
	// for Hold warns
	Level float64
	Hold  time.Duration
	// the rate window of the GC cycles the alert reports
	GcWindow time.Duration
	// the most gaps and blocks one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.13.
func DefaultProxyHeapLimitSettings() *ProxyHeapLimitSettings {
	return &ProxyHeapLimitSettings{
		Service:           "proxy",
		Level:             0.95,
		Hold:              15 * time.Minute,
		GcWindow:          5 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *ProxyHeapLimitSettings) valid() bool {
	return self != nil && self.Service != "" && 0 < self.Level && self.GcWindow >= time.Second && self.GcWindow%time.Second == 0 &&
		0 < self.MaxListed && self.StormReadSettings.valid(self.Hold)
}

type proxyHeapLimitProbe struct {
	settings *ProxyHeapLimitSettings
}

func (proxyHeapLimitProbe) id() string   { return proxyHeapLimitProbeId }
func (proxyHeapLimitProbe) tier() string { return tierWarn }

func (self proxyHeapLimitProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self proxyHeapLimitProbe) effectiveSettings() *ProxyHeapLimitSettings {
	if self.settings == nil {
		return DefaultProxyHeapLimitSettings()
	}
	return self.settings
}

// §26.13's expression, each block's highest ratio, and the host's most GC
// cycles a minute and lowest limit in MiB.
func proxyHeapLimitQuery(environment string, settings *ProxyHeapLimitSettings) string {
	selector := "{env=" + strconv.Quote(environment) + ",service=" + strconv.Quote(settings.Service) + "}"
	ratio := "(go_memstats_heap_inuse_bytes" + selector + " + go_memstats_stack_inuse_bytes" + selector + ") / go_gc_gomemlimit_bytes" + selector
	return strings.Join([]string{
		fleetPart("heap_limit", "max by (host) ("+ratio+")"),
		fleetPart("block_heap_limit", "max by (host, block) ("+ratio+")"),
		fleetPart("gc_per_minute", "max by (host) (60 * rate(go_gc_duration_seconds_count"+selector+"["+stormDuration(settings.GcWindow)+"]))"),
		fleetPart("limit_mib", "min by (host) (go_gc_gomemlimit_bytes"+selector+") / 1048576"),
	}, " or ")
}

var proxyHeapLimitShapes = map[string]stormShape{
	"heap_limit":       {keys: []string{"host"}},
	"block_heap_limit": {keys: []string{"host", "block"}},
	"gc_per_minute":    {keys: []string{"host"}},
	"limit_mib":        {keys: []string{"host"}},
}

func (self proxyHeapLimitProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/proxy-heap-limit"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(proxyHeapLimitProbeId, "proxy-heap-limit-unobservable", unobservableTarget, "§26.13", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, proxyHeapLimitQuery(env.cfg.env, settings), proxyHeapLimitShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	placed, placementGaps := stormPlacedHostBlocks(env, settings.Service)
	reasons := history.invalidReasons()
	if len(placed) == 0 {
		reasons = append(reasons, "placement_missing")
	}
	if 0 < len(placementGaps) {
		reasons = append(reasons, "placement_incomplete")
	}
	level := stormLevel{threshold: settings.Level}
	steps := history.last(settings.holdSteps(settings.Hold))
	gaps := []string{}
	findings := []finding{}
	for _, name := range stormJudgedHosts(placed, history, "heap_limit", "block_heap_limit") {
		// a maximum over fewer processes can only be lower: beyond stands,
		// and within is unknown while a placed block is missing
		ratioAt := func(step int64) stormReading {
			ratio := history.reading("heap_limit", name, step)
			if ratio.present && (level.beyond(ratio.value) || len(stormAbsentBlocks(history, "block_heap_limit", name, placed[name], step)) == 0) {
				return ratio
			}
			return stormReading{}
		}
		hold := stormHoldOf(steps, level, ratioAt)
		absentBools := map[string]bool{}
		for _, step := range steps {
			for _, key := range stormAbsentBlocks(history, "block_heap_limit", name, placed[name], step) {
				absentBools[key] = true
			}
		}
		for key := range absentBools {
			gaps = append(gaps, key+"[missing]")
		}
		if !hold.complete && len(absentBools) == 0 {
			gaps = append(gaps, name+"[missing]")
		}
		switch {
		case hold.held:
			blocks := stormHostBlocks(placed, history, name, "block_heap_limit")
			findings = append(findings, finding{
				probeId: proxyHeapLimitProbeId, tier: tierWarn, class: "proxy-heap-limit", target: name, sustain: 1,
				symptom:   fmt.Sprintf("A proxy process on %s held its heap plus stacks above %.0f%% of its Go memory limit at every evaluation for %s", name, 100*settings.Level, settings.Hold),
				mechanism: "Each proxy host runs 10 proxy processes (GOMAXPROCS 4, GOGC 100) under a Go memory limit set per host from proxy_sizing.yml (proxy/host_go_memory_limit.go). Near the limit the runtime collects to stay under it, so GC runs back to back: from 15Z on 2026-11-14 heap plus stacks reached 6.31 GiB of the 6500 MiB limit, GC went from 0.6-1 to 10-13 cycles a minute per process, the hosts went from 3-5 to 16-23 busy cores of 24, and the CPU-starved mint admission refused windows (§26.12). Setup p90 went from 4.3-6.9 s to 16-37 s (proxy/P90SPEED.md §4.2).",
				baseline:  fmt.Sprintf("From 2026-11-07 to the sign-up wave of 11-13 19:30Z each host's busiest process read 0.19-0.60 of its limit (median 0.33) and collected 0.6-2.5 times a minute. WARN when a host's highest process passes %.2f at every one-minute evaluation for %s.", settings.Level, settings.Hold),
				observed:  fmt.Sprintf("host=%s heap_and_stacks_over_limit_now=%s level=%.2f hold=%s gc_cycles_per_minute_now=%s limit_mib_now=%s blocks_over_level_now=%s evaluation_unix=%d", name, stormNumber(ratioAt(history.at()), "%.3f"), settings.Level, settings.Hold, stormNumber(history.reading("gc_per_minute", name, history.at()), "%.1f"), stormNumber(history.reading("limit_mib", name, history.at()), "%.0f"), stormBlocksListed(history, "block_heap_limit", name, blocks, history.at(), "%.3f", level.beyond, settings.MaxListed), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of max by (host) ((go_memstats_heap_inuse_bytes + go_memstats_stack_inuse_bytes) / go_gc_gomemlimit_bytes) for service %q, with each block's highest ratio, the host's most GC cycles a minute (60 * rate(go_gc_duration_seconds_count[%s])) and its lowest limit, through a services host's loopback Mimir. A process without a limit reports math.MaxInt64 as its limit and reads near 0.", settings.Service, stormDuration(settings.GcWindow)),
				context:   "GC cycles a minute is the cost: 10-13 at the limit against 0.6-1 when healthy, and P90SPEED.md §10's target for a fix is under 3. P1115 raised crisp's limit to 8000 MiB on 11-15 (21:39Z): from 11-16 to 11-18 05:30Z crisp fired once and fireside, still at 6500 MiB, 51 times. The per-minute maximum swings across the level with the GC cycles, so a broken hold is no recovery: fireside's median on 11-16 and 11-17 was 0.96. A restart empties the heap for about an hour (11-14 18:40Z, 11-15 01:14Z, 11-17 14:07Z). The limit is soft, so the ratio passes 1 under pressure (up to 1.13 on 11-04 and on 11-14), and heap plus stacks is most, not all, of what it counts.",
				action:    "Restore headroom on the host: raise its Go memory limit as far as its RAM allows (proxy_sizing.yml), move blocks to a host with room, or add RAM or a host (P90SPEED.md §9, decision 1). Read the mint-refusal (§26.12) and proxy host CPU (§26.9) entries for the cost. Do not count on same-image restarts.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads every process of the host at or under %.2f of its limit.", settings.Hold, settings.Level),
				playbook:  "SIGNALS.md §26.13 and proxy/P90SPEED.md §4.2",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(proxyHeapLimitProbeId, tierWarn, "proxy-heap-limit", name))
		default:
			findings = append(findings, stormClearHold(proxyHeapLimitProbeId, "proxy-heap-limit", name))
		}
	}
	sort.Strings(gaps)
	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, append(placementGaps, gaps...)))
	} else {
		findings = append(findings, healthyFinding(proxyHeapLimitProbeId, tierWarn, "proxy-heap-limit-unobservable", unobservableTarget))
	}
	return findings, nil
}
