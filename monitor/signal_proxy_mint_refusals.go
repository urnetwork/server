package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.12 maps to signal_proxy_mint_refusals.go and
// signal_proxy_mint_refusals_test.go. A proxy process admits 2 hosted
// window-client mints at once (defaultHostedMintConcurrency) and refuses the
// rest with a 429 before any database work, counting each refusal in
// urnetwork_hosted_mint_admission_refusals_total. Connect reads the 429 as a
// rate limit and backs off 5 s, doubling, so a window whose first mints are
// refused holds its device's first CONNECT (proxy/P90SPEED.md §3.3). A mint
// starved of CPU holds its slot longer, so refusals rise when the processes
// sit at their Go memory limit (§26.13). The probe sums each host's refusals
// over its blocks whose newest process has run 30 minutes: a block's restart
// refuses in bursts while its devices rebuild, so the catalog PromQL drops a
// block at every evaluation where its newest process is younger, and an
// evaluation where none of the host's blocks is that old counts as neither
// beyond nor within the band. A sum over fewer blocks can only be lower, so a
// held band stands while a block is missing; a clear needs every block.
func NewProxyMintRefusalsSignal() Signal {
	return NewProxyMintRefusalsSignalWithSettings(DefaultProxyMintRefusalsSettings())
}

// The signal with explicit thresholds.
func NewProxyMintRefusalsSignalWithSettings(settings *ProxyMintRefusalsSettings) Signal {
	return &signalAdapter{
		number: "26.12", key: "proxy-mint-refusals", name: "Proxy host hosted mint refusals",
		probe: proxyMintRefusalsProbe{settings: settings},
	}
}

const proxyMintRefusalsProbeId = "mimir/proxy-mint-refusals"

// The thresholds and bounds of §26.12.
type ProxyMintRefusalsSettings struct {
	// the service whose hosts and blocks run the mint admission
	Service string
	// the rate window of each evaluation
	Window time.Duration
	// a host's refusals above WarnRefusals per second for Hold warn, summed
	// over its blocks whose newest process has run MinUptime
	WarnRefusals float64
	Hold         time.Duration
	MinUptime    time.Duration
	// the most gaps and blocks one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.12.
func DefaultProxyMintRefusalsSettings() *ProxyMintRefusalsSettings {
	return &ProxyMintRefusalsSettings{
		Service:           "proxy",
		Window:            5 * time.Minute,
		WarnRefusals:      5,
		Hold:              10 * time.Minute,
		MinUptime:         30 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *ProxyMintRefusalsSettings) valid() bool {
	return self != nil && self.Service != "" && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 <= self.WarnRefusals && 0 <= self.MinUptime && self.MinUptime%time.Second == 0 && 0 < self.MaxListed &&
		self.StormReadSettings.valid(self.Hold)
}

type proxyMintRefusalsProbe struct {
	settings *ProxyMintRefusalsSettings
}

func (proxyMintRefusalsProbe) id() string   { return proxyMintRefusalsProbeId }
func (proxyMintRefusalsProbe) tier() string { return tierWarn }

func (self proxyMintRefusalsProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self proxyMintRefusalsProbe) effectiveSettings() *ProxyMintRefusalsSettings {
	if self.settings == nil {
		return DefaultProxyMintRefusalsSettings()
	}
	return self.settings
}

// §26.12's expression, each block's refusals, and each block's newest
// process's uptime.
func proxyMintRefusalsQuery(environment string, settings *ProxyMintRefusalsSettings) string {
	selector := "{env=" + strconv.Quote(environment) + ",service=" + strconv.Quote(settings.Service) + "}"
	blockRefusals := "sum by (host, block) (rate(urnetwork_hosted_mint_admission_refusals_total" + selector + "[" + stormDuration(settings.Window) + "]))"
	uptime := "time() - max by (host, block) (process_start_time_seconds" + selector + ")"
	return strings.Join([]string{
		fleetPart("refusals", "sum by (host) ("+blockRefusals+" and on (host, block) ("+uptime+" > "+strconv.FormatInt(int64(settings.MinUptime/time.Second), 10)+"))"),
		fleetPart("block_refusals", blockRefusals),
		fleetPart("uptime", uptime),
	}, " or ")
}

var proxyMintRefusalsShapes = map[string]stormShape{
	"refusals":       {keys: []string{"host"}},
	"block_refusals": {keys: []string{"host", "block"}},
	"uptime":         {keys: []string{"host", "block"}},
}

// One host's evaluation: the band's reading, the host's blocks old enough to
// judge, and the blocks whose absence leaves its sum unknown.
type proxyMintRefusalsStep struct {
	reading stormReading
	mature  []string
	absent  []string
}

func (self proxyMintRefusalsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/proxy-mint-refusals"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(proxyMintRefusalsProbeId, "proxy-mint-refusals-unobservable", unobservableTarget, "§26.12", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, proxyMintRefusalsQuery(env.cfg.env, settings), proxyMintRefusalsShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	// every placed block is required; any other answering block counts with
	// its host while it answers
	placed, placementGaps := stormPlacedHostBlocks(env, settings.Service)
	hostBlocks := map[string]map[string]bool{}
	for name, blocks := range placed {
		hostBlocks[name] = map[string]bool{}
		for _, block := range blocks {
			hostBlocks[name][block] = true
		}
	}
	for _, part := range []string{"block_refusals", "uptime"} {
		for _, key := range history.keys(part) {
			name, block, _ := strings.Cut(key, "/")
			if hostBlocks[name] == nil {
				hostBlocks[name] = map[string]bool{}
			}
			hostBlocks[name][block] = true
		}
	}
	reasons := history.invalidReasons()
	if len(placed) == 0 {
		reasons = append(reasons, "placement_missing")
	}
	if 0 < len(placementGaps) {
		reasons = append(reasons, "placement_incomplete")
	}
	level := stormLevel{threshold: settings.WarnRefusals}
	minUptime := settings.MinUptime.Seconds()
	steps := history.last(settings.holdSteps(settings.Hold))
	gaps := []string{}
	findings := []finding{}
	for _, name := range stormJudgedHosts(placed, history, "refusals", "block_refusals", "uptime") {
		blocks := []string{}
		for block := range hostBlocks[name] {
			blocks = append(blocks, block)
		}
		sort.Strings(blocks)
		evaluate := func(step int64) proxyMintRefusalsStep {
			// a placed block without a process is unknown; a block old enough
			// to judge must have a rate
			result := proxyMintRefusalsStep{absent: stormAbsentBlocks(history, "uptime", name, placed[name], step)}
			for _, block := range blocks {
				key := name + "/" + block
				uptime := history.reading("uptime", key, step)
				if !uptime.present || uptime.value <= minUptime {
					continue
				}
				result.mature = append(result.mature, block)
				if !history.reading("block_refusals", key, step).present {
					result.absent = append(result.absent, key)
				}
			}
			refusals := history.reading("refusals", name, step)
			switch {
			case refusals.present && (len(result.absent) == 0 || level.beyond(refusals.value)):
				// a sum over fewer blocks can only be lower: beyond stands
				result.reading = refusals
			case refusals.present || 0 < len(result.absent):
			case len(result.mature) == 0:
				// no block has run long enough to judge
				result.reading = stormLeftOut
			}
			return result
		}
		hold := stormHoldOf(steps, level, func(step int64) stormReading { return evaluate(step).reading })
		absentBools := map[string]bool{}
		for _, step := range steps {
			for _, key := range evaluate(step).absent {
				absentBools[key] = true
			}
		}
		for key := range absentBools {
			gaps = append(gaps, key+"[missing]")
		}
		if !hold.complete && len(absentBools) == 0 {
			gaps = append(gaps, name+"[missing]")
		}
		now := evaluate(history.at())
		switch {
		case hold.held:
			findings = append(findings, finding{
				probeId: proxyMintRefusalsProbeId, tier: tierWarn, class: "proxy-mint-refusals", target: name, sustain: 1,
				symptom:   fmt.Sprintf("Proxy host %s refused more than %g hosted mints a second at every evaluation for %s", name, settings.WarnRefusals, settings.Hold),
				mechanism: "A proxy process admits 2 hosted window-client mints at once and refuses the rest with a 429 before any database work. Connect reads the 429 as a rate limit and backs off 5 s, doubling, so a window whose first mints are refused holds its device's first CONNECT: one refusal cost a setup about 3-5 s, two 9-13 s and three 24-31 s (proxy/P90SPEED.md §3.3). A mint starved of CPU holds its slot longer, so a host whose processes sit at their Go memory limit refuses more: from 15Z on 2026-11-14 each proxy host refused tens to hundreds of mints a second (peaks of about 1,130/s), and setup p90 went from 4.3-6.9 s to 16-37 s.",
				baseline:  fmt.Sprintf("Per host from 2026-11-07 to the load ramp at 13:51Z on 11-14: median 0.11/s, 99th percentile 1.5/s, peak 3.8/s. WARN when a host passes %g/s at every one-minute evaluation for %s, each evaluation summing the blocks whose newest process has run %s.", settings.WarnRefusals, settings.Hold, settings.MinUptime),
				observed:  fmt.Sprintf("host=%s refusals_per_second_now=%s level=%g hold=%s mature_blocks_now=%d/%d blocks_now=%s evaluation_unix=%d", name, stormNumber(now.reading, "%.1f"), settings.WarnRefusals, settings.Hold, len(now.mature), len(blocks), proxyMintRefusalsBusiest(history, name, now.mature, history.at(), settings.MaxListed), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of each host's sum by (host, block) (rate(urnetwork_hosted_mint_admission_refusals_total{service=%q}[%s])) over its blocks whose newest process has run more than %s, with each block's rate and uptime, through a services host's loopback Mimir. Process instance nonces stay in Mimir.", settings.Service, stormDuration(settings.Window), settings.MinUptime),
				context:   "The uptime rule leaves out a block's own restart: the nightly restart of all 20 processes at 14:04-14:07Z on 11-17 refused 47-87/s per host until 14:12Z. Load sweeps that reopen thousands of hosted proxies every 35-45 minutes make a host near the level open and clear with each sweep (crisp at 8000 MiB: 29-35 times a day on 11-16 and 11-17). An out-of-balance payer's refilling windows refuse mints too (11-04 and 11-05, CPUOPT4 Part B): refusals say windows cannot mint, not why.",
				action:    "Read the heap-limit entry (§26.13) and proxy host CPU (§26.9) for the host: a host at its Go memory limit refuses more. Restore the host's headroom (P90SPEED.md §9, decision 1) rather than restarting: restarts relieved it for about an hour each time. Read the abandoned-CONNECT entry (§26.14) for what clients see.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold, once a block of the host has run %s, reads the host's refusals at or under %g/s.", settings.Hold, settings.MinUptime, settings.WarnRefusals),
				playbook:  "SIGNALS.md §26.12 and proxy/P90SPEED.md",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(proxyMintRefusalsProbeId, tierWarn, "proxy-mint-refusals", name))
		default:
			findings = append(findings, stormClearHold(proxyMintRefusalsProbeId, "proxy-mint-refusals", name))
		}
	}
	sort.Strings(gaps)
	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, append(placementGaps, gaps...)))
	} else {
		findings = append(findings, healthyFinding(proxyMintRefusalsProbeId, tierWarn, "proxy-mint-refusals-unobservable", unobservableTarget))
	}
	return findings, nil
}

// The host's blocks with the most refusals at a step, most first, as
// block:rate.
func proxyMintRefusalsBusiest(history *stormHistory, host string, blocks []string, step int64, limit int) string {
	type blockRate struct {
		block string
		rate  float64
	}
	rates := []blockRate{}
	for _, block := range blocks {
		if reading := history.reading("block_refusals", host+"/"+block, step); reading.present {
			rates = append(rates, blockRate{block: block, rate: reading.value})
		}
	}
	sort.SliceStable(rates, func(i, j int) bool { return rates[i].rate > rates[j].rate })
	listed := make([]string, 0, len(rates))
	for _, one := range rates {
		listed = append(listed, fmt.Sprintf("%s:%.1f", one.block, one.rate))
	}
	return stormListed(listed, limit)
}
