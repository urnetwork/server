package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.11 maps to signal_hosted_contract_cap.go and
// signal_hosted_contract_cap_test.go. A proxy process refuses a hosted
// device's contract-creation control pack with a 429 once 256 creations
// (HostedContractConcurrency) are already in flight in it (88117cc03), and
// counts the refusal in urnetwork_hosted_contract_admission_refusals_total.
// A saturated cap means the process's creations are not returning. A block's
// own restart refuses in bursts while its devices reconnect, so a process is
// judged only once it has run 30 minutes: the catalog PromQL drops a block at
// every evaluation where its newest process is younger, and the probe counts
// such an evaluation as neither beyond nor within the band. A block whose
// processes do not export the counter runs no cap and refuses nothing.
func NewHostedContractCapSignal() Signal {
	return NewHostedContractCapSignalWithSettings(DefaultHostedContractCapSettings())
}

// The signal with explicit thresholds.
func NewHostedContractCapSignalWithSettings(settings *HostedContractCapSettings) Signal {
	return &signalAdapter{
		number: "26.11", key: "hosted-contract-cap", name: "Hosted contract admission cap",
		probe: hostedContractCapProbe{settings: settings},
	}
}

const hostedContractCapProbeId = "mimir/hosted-contract-cap"

// The thresholds and bounds of §26.11.
type HostedContractCapSettings struct {
	// the service whose blocks run the cap
	Service string
	// the rate window of each evaluation
	Window time.Duration
	// a block's refusals above WarnRefusals per second for Hold warn, once
	// its newest process has run MinUptime
	WarnRefusals float64
	Hold         time.Duration
	MinUptime    time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.11.
func DefaultHostedContractCapSettings() *HostedContractCapSettings {
	read := DefaultStormReadSettings()
	// 20 blocks, three parts and ten steps
	read.MaxRows = 16384
	return &HostedContractCapSettings{
		Service:           "proxy",
		Window:            5 * time.Minute,
		WarnRefusals:      50,
		Hold:              10 * time.Minute,
		MinUptime:         30 * time.Minute,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *HostedContractCapSettings) valid() bool {
	return self != nil && self.Service != "" && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 <= self.WarnRefusals && 0 <= self.MinUptime && self.MinUptime%time.Second == 0 && 0 < self.MaxListed &&
		self.StormReadSettings.valid(self.Hold)
}

type hostedContractCapProbe struct {
	settings *HostedContractCapSettings
}

func (hostedContractCapProbe) id() string   { return hostedContractCapProbeId }
func (hostedContractCapProbe) tier() string { return tierWarn }

func (self hostedContractCapProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self hostedContractCapProbe) effectiveSettings() *HostedContractCapSettings {
	if self.settings == nil {
		return DefaultHostedContractCapSettings()
	}
	return self.settings
}

// §26.11's expression, the blocks whose processes export the counter, and
// each block's newest process's uptime.
func hostedContractCapQuery(environment string, settings *HostedContractCapSettings) string {
	env := "env=" + strconv.Quote(environment)
	uptime := "time() - max by (host, block) (process_start_time_seconds{" + env + ",service=" + strconv.Quote(settings.Service) + "})"
	return strings.Join([]string{
		fleetPart("refusals", "sum by (host, block) (rate(urnetwork_hosted_contract_admission_refusals_total{"+env+"}["+stormDuration(settings.Window)+"]))"+
			" and on (host, block) ("+uptime+" > "+strconv.FormatInt(int64(settings.MinUptime/time.Second), 10)+")"),
		fleetPart("capped", "count by (host, block) (urnetwork_hosted_contract_admission_refusals_total{"+env+"})"),
		fleetPart("uptime", uptime),
	}, " or ")
}

var hostedContractCapShapes = map[string]stormShape{
	"refusals": {keys: []string{"host", "block"}},
	"capped":   {keys: []string{"host", "block"}},
	"uptime":   {keys: []string{"host", "block"}},
}

func (self hostedContractCapProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/hosted-contract-cap"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(hostedContractCapProbeId, "hosted-contract-cap-unobservable", unobservableTarget, "§26.11", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, hostedContractCapQuery(env.cfg.env, settings), hostedContractCapShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	// every placed block is required; any other answering block is judged
	inventory := newFleetSlotInventory(env, []string{settings.Service}, nil)
	required := map[string]bool{}
	blockBools := map[string]bool{}
	for _, slot := range inventory.slots {
		required[slot.host+"/"+slot.block] = true
		blockBools[slot.host+"/"+slot.block] = true
	}
	for _, part := range []string{"refusals", "capped"} {
		for _, block := range history.keys(part) {
			blockBools[block] = true
		}
	}
	blocks := []string{}
	for block := range blockBools {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)

	reasons := history.invalidReasons()
	if len(inventory.slots) == 0 {
		reasons = append(reasons, "placement_missing")
	}
	minUptime := settings.MinUptime.Seconds()
	steps := history.last(settings.holdSteps(settings.Hold))
	missing := []string{}
	findings := []finding{}
	for _, block := range blocks {
		refusalsAt := func(step int64) stormReading {
			if refusals := history.reading("refusals", block, step); refusals.present {
				return refusals
			}
			uptime := history.reading("uptime", block, step)
			switch {
			case !uptime.present:
				return stormReading{}
			case !history.reading("capped", block, step).present:
				// a process without the counter runs no cap and refuses nothing
				return stormValue(0)
			case uptime.value <= minUptime:
				return stormLeftOut
			}
			return stormReading{}
		}
		hold := stormHoldOf(steps, stormLevel{threshold: settings.WarnRefusals}, refusalsAt)
		target := block
		if !hold.complete && required[block] {
			missing = append(missing, block+"[missing]")
		}
		switch {
		case hold.held:
			findings = append(findings, finding{
				probeId: hostedContractCapProbeId, tier: tierWarn, class: "hosted-contract-cap-saturated", target: target, sustain: 1,
				symptom:   fmt.Sprintf("Proxy block %s refused more than %.0f hosted contract creations a second at its concurrency cap at every evaluation for %s", block, settings.WarnRefusals, settings.Hold),
				mechanism: "A proxy process refuses a hosted device's contract-creation control pack with a 429 once 256 creations are already in flight in it, before any credential or database read; the device counts a refusal as a failed control round trip and asks again on its 1, 2, 4, 5 s schedule. A cap that stays full means the process's creations are not returning. On 2026-10-26 connect g4's drain at 19:39:56Z drove the fleet's refusals from 16/s to 923/s, and g4 on both proxy hosts held above 50/s from 20:12Z.",
				baseline:  fmt.Sprintf("No baseline yet; the fleet refused nothing from 19:30Z to 19:40Z on 2026-10-26, between the cap's rollout and connect g4's drain. WARN when a block passes %.0f refusals/s at every one-minute evaluation for %s, each evaluation once its newest process has run %s.", settings.WarnRefusals, settings.Hold, settings.MinUptime),
				observed:  fmt.Sprintf("block=%s refusals_per_second_now=%s newest_process_uptime_seconds=%s evaluation_unix=%d", block, stormNumber(refusalsAt(history.at()), "%.1f"), stormNumber(history.reading("uptime", block, history.at()), "%.0f"), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of sum by (host, block) (rate(urnetwork_hosted_contract_admission_refusals_total[%s])) and on (host, block) its newest %s process's uptime above %s, through a services host's loopback Mimir.", stormDuration(settings.Window), settings.Service, settings.MinUptime),
				context:   "The uptime rule leaves out a block's own restart: a rolling restart's refusals ended within 15 minutes of the last block's start. A connect or alt drain's burst is not excluded: it is how incident B began. A refused pack can carry several creations, so refusals undercount the creations turned away. The cap protects the process; it does not end the storm.",
				action:    "Read the stuck-creation (§26.4), release-error (§26.6) and Redis (§26.7, §26.8) entries, and hold connect, alt and proxy rollouts.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold, once the block's newest process has run %s, reads the block's refusals at or under %.0f/s.", settings.Hold, settings.MinUptime, settings.WarnRefusals),
				playbook:  "SIGNALS.md §26.11 and §26.4",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(hostedContractCapProbeId, tierWarn, "hosted-contract-cap-saturated", target))
		default:
			findings = append(findings, stormClearHold(hostedContractCapProbeId, "hosted-contract-cap-saturated", target))
		}
	}
	if 0 < len(missing) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(inventory.missingServices) {
		reasons = append(reasons, "placement_incomplete")
		missing = append(missing, settings.Service+"[placement-missing]")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, missing))
	} else {
		findings = append(findings, healthyFinding(hostedContractCapProbeId, tierWarn, "hosted-contract-cap-unobservable", unobservableTarget))
	}
	return findings, nil
}
