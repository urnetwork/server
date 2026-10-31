package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.4 maps to signal_contract_creations_stuck.go and
// signal_contract_creations_stuck_test.go. urnetwork_contract_creation_stage_inflight
// counts the contract creations in flight in each stage; stage "other" holds
// the uninstrumented time, including a creation waiting out its reservation
// cleanup. In incident B up to 182k creations were stuck there, about 9k per
// proxy process, while the completed-call counters (§2.30, §25.1) saw almost
// nothing, because a call counts only once it returns. The count moves in
// waves as cleanup budgets end together, so each process's two-minute maximum
// is the reading. The probe reads, per service, the largest process's reading
// at each minute of its hold, the service's total, and which placed slots
// answered. A maximum over fewer processes can only be lower, so a held band
// is proven whatever the coverage; a clear needs every placed slot.
func NewContractCreationsStuckSignal() Signal {
	return NewContractCreationsStuckSignalWithSettings(DefaultContractCreationsStuckSettings())
}

// The signal with explicit thresholds.
func NewContractCreationsStuckSignalWithSettings(settings *ContractCreationsStuckSettings) Signal {
	return &signalAdapter{
		number: "26.4", key: "contract-creations-stuck", name: "Stuck contract creations",
		probe: contractCreationsStuckProbe{settings: settings},
	}
}

const contractCreationsStuckProbeId = "mimir/contract-creations-stuck"

// The thresholds and bounds of §26.4.
type ContractCreationsStuckSettings struct {
	// the services that export the stage gauge; one the active placement
	// does not run creates no requirement
	Services []string
	// the stage that holds the uninstrumented time
	Stage string
	// each process's reading is its maximum over this window
	Window time.Duration
	// a process above PageCalls for Hold pages
	PageCalls float64
	Hold      time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.4.
func DefaultContractCreationsStuckSettings() *ContractCreationsStuckSettings {
	read := DefaultStormReadSettings()
	// placed slots answer at every step: about 70 slots and 5 steps
	read.MaxRows = 16384
	return &ContractCreationsStuckSettings{
		Services:          []string{"alt", "api", "connect", "gossip", "mcp", "proxy", "taskworker"},
		Stage:             "other",
		Window:            2 * time.Minute,
		PageCalls:         1000,
		Hold:              5 * time.Minute,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *ContractCreationsStuckSettings) valid() bool {
	return self != nil && 0 < len(self.Services) && self.Stage != "" && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 < self.PageCalls && 0 < self.MaxListed && self.StormReadSettings.valid(self.Hold)
}

type contractCreationsStuckProbe struct {
	settings *ContractCreationsStuckSettings
}

func (contractCreationsStuckProbe) id() string   { return contractCreationsStuckProbeId }
func (contractCreationsStuckProbe) tier() string { return tierPage }

func (self contractCreationsStuckProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self contractCreationsStuckProbe) effectiveSettings() *ContractCreationsStuckSettings {
	if self.settings == nil {
		return DefaultContractCreationsStuckSettings()
	}
	return self.settings
}

// §26.4's expression, each service's total, and the slots that answered.
func contractCreationsStuckQuery(environment string, settings *ContractCreationsStuckSettings) string {
	gauge := "max_over_time(urnetwork_contract_creation_stage_inflight{env=" + strconv.Quote(environment) + ",stage=" + strconv.Quote(settings.Stage) + "}[" + stormDuration(settings.Window) + "])"
	return strings.Join([]string{
		fleetPart("stuck", "max by (service) (sum by (service, host, block, instance) ("+gauge+"))"),
		fleetPart("total", "sum by (service) ("+gauge+")"),
		fleetPart("slots", "count by (service, host, block) ("+gauge+")"),
	}, " or ")
}

var contractCreationsStuckShapes = map[string]stormShape{
	"stuck": {keys: []string{"service"}},
	"total": {keys: []string{"service"}},
	"slots": {keys: []string{"service", "host", "block"}},
}

func (self contractCreationsStuckProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/contract-creations-stuck"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(contractCreationsStuckProbeId, "contract-creations-stuck-unobservable", unobservableTarget, "§26.4", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, contractCreationsStuckQuery(env.cfg.env, settings), contractCreationsStuckShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	inventory := newFleetSlotInventory(env, stormPlacedServices(env, settings.Services), nil)
	serviceSlots := map[string][]fleetSlot{}
	serviceBools := map[string]bool{}
	for _, slot := range inventory.slots {
		serviceSlots[slot.service] = append(serviceSlots[slot.service], slot)
		serviceBools[slot.service] = true
	}
	for _, service := range history.keys("stuck") {
		serviceBools[service] = true
	}
	services := []string{}
	for service := range serviceBools {
		services = append(services, service)
	}
	sort.Strings(services)

	reasons := history.invalidReasons()
	if len(inventory.slots) == 0 {
		reasons = append(reasons, "placement_missing")
	}
	steps := history.last(settings.holdSteps(settings.Hold))
	gaps := []string{}
	findings := []finding{}
	for _, service := range services {
		// a step counts toward a clear only when every placed slot answered
		slotsAnswered := func(step int64) (int, []string) {
			answered, absent := 0, []string{}
			for _, slot := range serviceSlots[service] {
				if slot.allowed && history.reading("slots", slot.service+"/"+slot.host+"/"+slot.block, step).present {
					answered++
				} else {
					absent = append(absent, slot.label())
				}
			}
			return answered, absent
		}
		stuckAt := func(step int64) stormReading { return history.reading("stuck", service, step) }
		hold := stormHoldOf(steps, stormLevel{threshold: settings.PageCalls}, stuckAt)
		slotsComplete := true
		absentBools := map[string]bool{}
		for _, step := range steps {
			if _, absent := slotsAnswered(step); 0 < len(absent) {
				slotsComplete = false
				for _, label := range absent {
					absentBools[label] = true
				}
			}
		}
		for label := range absentBools {
			gaps = append(gaps, label+"[missing]")
		}
		if !hold.complete {
			gaps = append(gaps, service+"[missing]")
		}
		answeredNow, _ := slotsAnswered(history.at())
		target := env.cfg.env + "/" + service
		switch {
		case hold.held:
			findings = append(findings, finding{
				probeId: contractCreationsStuckProbeId, tier: tierPage, class: "contract-creations-stuck", target: target, sustain: 1,
				symptom:   fmt.Sprintf("A %s process held more than %.0f contract creations in stage %q at every evaluation for %s", service, settings.PageCalls, settings.Stage, settings.Hold),
				mechanism: "Stage \"other\" holds the uninstrumented time of a creation, including one waiting out its reservation cleanup. In incident B a connect drain burst contract creation on every proxy process; each failed creation waited up to 300 s on its own cleanup, whose loops churned the admission Redis client's connections until Redis main threads saturated, with up to 182k creations in flight. A call counts in the completed-call counters only once it returns, so this gauge leads them.",
				baseline:  fmt.Sprintf("At most 30 in any process and 0-142 across the proxy fleet on healthy days (2026-10-20; 10-26 08:00-12:00Z). PAGE when one process passes %.0f at every one-minute evaluation for %s, each process read at its %s maximum.", settings.PageCalls, settings.Hold, stormDuration(settings.Window)),
				observed:  fmt.Sprintf("service=%s largest_process_now=%s service_total_now=%s slots_answering=%d/%d evaluation_unix=%d", service, stormNumber(stuckAt(history.at()), "%.0f"), stormNumber(history.reading("total", service, history.at()), "%.0f"), answeredNow, len(serviceSlots[service]), history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of max by (service) (sum by (service, host, block, instance) (max_over_time(urnetwork_contract_creation_stage_inflight{stage=%q}[%s]))) through a services host's loopback Mimir. Process instance nonces stay in Mimir.", settings.Stage, stormDuration(settings.Window)),
				context:   "A proxy block's restart drains its devices onto the other blocks, which then create contracts together (10-26 19:03Z, 6,583 in one process); read rollout-guard. A capped process stays near 256 (HostedContractConcurrency), so once every block runs the cap the cap entry (§26.11) carries the storm. A process that stops pushing is unknown, not zero.",
				action:    "Find the trigger in rollout-guard (a connect, alt or proxy block drain), hold rollouts, and read the release-error and Redis entries (§26.6-§26.8) for the node. Same-image proxy restarts cleared it for minutes only.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads every process of the service at or under %.0f, with every placed slot answering.", settings.Hold, settings.PageCalls),
				playbook:  "SIGNALS.md §26.4",
			})
		case hold.clear && slotsComplete && 0 < len(serviceSlots[service]):
			findings = append(findings, healthyFinding(contractCreationsStuckProbeId, tierPage, "contract-creations-stuck", target))
		default:
			findings = append(findings, stormClearHold(contractCreationsStuckProbeId, "contract-creations-stuck", target))
		}
	}
	sort.Strings(gaps)
	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(inventory.missingServices) {
		reasons = append(reasons, "placement_incomplete")
	}
	for _, service := range inventory.missingServices {
		gaps = append(gaps, service+"[placement-missing]")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(contractCreationsStuckProbeId, tierWarn, "contract-creations-stuck-unobservable", unobservableTarget))
	}
	return findings, nil
}
