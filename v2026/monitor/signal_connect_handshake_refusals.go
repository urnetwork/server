// SIGNALS.md §25.2. Count the outcomes Connect records when an H1 admission
// finishes. Each desired slot needs one stable publisher and a qualified rate
// for every observed counter child before its contribution is counted.
package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	connectHandshakeProbeId      = "mimir/connect-handshake-refusals"
	connectHandshakeClass        = "connect-handshake-refusals"
	connectHandshakeUnknownClass = "connect-handshake-refusals-unobservable"
	connectHandshakeMetric       = "urnetwork_connect_h1_handshake_results_total"
)

// The catalogued rates, shares and elapsed hold windows. Both bands use the
// fleet's sum of completed H1 admissions across its placed Connect processes.
type ConnectHandshakeRefusalsSettings struct {
	// Rate window for every completed-admission counter.
	Window time.Duration
	// Sustained inclusive absolute refusal rates in events per second.
	DependencyHold     time.Duration
	DependencyWarnRate float64
	DependencyPageRate float64
	// Strict lower bounds on accepted admissions divided by all completions.
	AcceptanceWarnHold  time.Duration
	AcceptancePageHold  time.Duration
	AcceptanceWarnShare float64
	AcceptancePageShare float64
	StormReadSettings
}

// Defaults come from §25.2's October incident replays.
func DefaultConnectHandshakeRefusalsSettings() *ConnectHandshakeRefusalsSettings {
	read := DefaultStormReadSettings()
	read.MaxRows = 16384
	return &ConnectHandshakeRefusalsSettings{
		Window:              5 * time.Minute,
		DependencyHold:      5 * time.Minute,
		DependencyWarnRate:  20,
		DependencyPageRate:  100,
		AcceptanceWarnHold:  15 * time.Minute,
		AcceptancePageHold:  10 * time.Minute,
		AcceptanceWarnShare: .5,
		AcceptancePageShare: .3,
		StormReadSettings:   read,
	}
}

// One read supplies both arms and their source coverage.
func NewConnectHandshakeRefusalsSignal() Signal {
	return NewConnectHandshakeRefusalsSignalWithSettings(DefaultConnectHandshakeRefusalsSettings())
}

// Exposes the signal with explicit thresholds and source bounds.
func NewConnectHandshakeRefusalsSignalWithSettings(settings *ConnectHandshakeRefusalsSettings) Signal {
	return &signalAdapter{number: "25.2", key: connectHandshakeClass, name: "Connect H1 handshake refusals and acceptance", probe: connectHandshakeProbe{settings: settings}}
}

// Reject unbounded reads and vacuous holds before contacting a gateway.
func (self *ConnectHandshakeRefusalsSettings) valid() bool {
	return self.Window >= time.Second && self.Window <= time.Hour && self.Window%time.Second == 0 &&
		self.Freshness >= time.Second && self.Freshness%time.Second == 0 &&
		self.DependencyWarnRate > 0 && self.DependencyWarnRate < self.DependencyPageRate && !math.IsInf(self.DependencyPageRate, 0) &&
		self.AcceptancePageShare > 0 && self.AcceptancePageShare < self.AcceptanceWarnShare && self.AcceptanceWarnShare <= 1 &&
		self.AcceptancePageHold <= self.AcceptanceWarnHold &&
		self.StormReadSettings.valid(self.DependencyHold, self.AcceptanceWarnHold, self.AcceptancePageHold)
}

// A stateless reader; the Mimir range includes both endpoints of every hold.
type connectHandshakeProbe struct {
	settings *ConnectHandshakeRefusalsSettings
}

func (connectHandshakeProbe) id() string                  { return connectHandshakeProbeId }
func (connectHandshakeProbe) tier() string                { return tierPage }
func (self connectHandshakeProbe) cadence() time.Duration { return self.policy().schedule() }

// A nil override uses the registry defaults.
func (self connectHandshakeProbe) policy() *ConnectHandshakeRefusalsSettings {
	if self.settings == nil {
		return DefaultConnectHandshakeRefusalsSettings()
	}
	return self.settings
}

// Keep private process, stage and outcome labels in Mimir. Count every process
// and every counter child seen across the window before allowing lazy zeros.
// A disappearing, reset or newly created child invalidates that slot's rate.
func connectHandshakeQuery(selector string, settings *ConnectHandshakeRefusalsSettings) string {
	window := stormDuration(settings.Window)
	seconds := strconv.FormatInt(int64(settings.Window/time.Second), 10)
	fresh := strconv.FormatInt(int64(settings.Freshness/time.Second), 10)
	start := "process_start_time_seconds{" + selector + "}"
	counter := func(extra string) string { return connectHandshakeMetric + "{" + selector + extra + "}" }
	all := counter("")
	seen := func(metric string) string {
		return "(count_over_time(" + metric + "[" + window + "]) or " + metric + " offset " + window + " or " + metric + ")"
	}
	freshAtEnds := func(metric string) string {
		return "(timestamp(" + metric + ") >= time() - " + fresh + ") and (timestamp(" + metric + " offset " + window + ") >= time() - " + seconds + " - " + fresh + ")"
	}
	byProcess := func(expr string) string { return "count by (host,block,instance) (" + expr + ")" }
	bySlot := func(expr string) string { return "count by (host,block) (" + expr + ")" }
	rate := func(metric string) string { return "sum by (host,block) (rate(" + metric + "[" + window + "]))" }

	// The stats pusher labels each Connect process. The producer increments
	// accepted only after authentication and each other result at http/upgraded.
	identity := `,service="connect",instance=~"[0-9a-f]{8}"`
	knownCarrier := identity + `,carrier=~"websocket|h1plus"`
	accepted := counter(knownCarrier + `,stage="authenticated",result="accepted"`)
	rejected := counter(knownCarrier + `,stage=~"http|upgraded",result!="",result!="accepted"`)
	startExact := "process_start_time_seconds{" + selector + identity + "}"
	stableStart := strings.Join([]string{
		"(" + startExact + " > 0)",
		"(" + startExact + " <= time() - " + seconds + ")",
		"(" + startExact + " == " + startExact + " offset " + window + ")",
		"(changes(" + startExact + "[" + window + "]) == 0)",
		freshAtEnds(startExact),
	}, " and ")
	stableProcess := "(" + byProcess(seen(start)) + " == 1) and (" + byProcess(stableStart) + " == 1)"
	goodChildren := "(" + accepted + " or " + rejected + ") and (rate(" + all + "[" + window + "]) >= 0) and (resets(" + all + "[" + window + "]) == 0) and " + freshAtEnds(all)
	completeFamily := "(" + byProcess(seen(all)) + " == " + byProcess(goodChildren) + ")"

	total := rate(all)
	lazyRate := func(metric string) string { return rate(metric) + " or (0 * " + total + ")" }
	return strings.Join([]string{
		fleetPart("publishers", bySlot("group by (host,block,instance) ("+seen(start)+" or "+seen(all)+")")),
		fleetPart("qualified", bySlot(stableProcess+" and "+completeFamily)),
		fleetPart("total", total),
		fleetPart("accepted", lazyRate(accepted)),
		fleetPart("dependency", lazyRate(counter(`,result="auth_dependency"`))),
	}, " or ")
}

var connectHandshakeShapes = map[string]stormShape{
	"publishers": {keys: []string{"host", "block"}},
	"qualified":  {keys: []string{"host", "block"}},
	"total":      {keys: []string{"host", "block"}},
	"accepted":   {keys: []string{"host", "block"}},
	"dependency": {keys: []string{"host", "block"}},
}

// One fleet evaluation. A partial dependency sum remains a lower bound.
type connectHandshakePoint struct {
	slots                                       int
	complete                                    bool
	total, accepted, dependency                 float64
	largestDependencySlot, lowestAcceptanceSlot string
}

// Only inventory slots reach diagnostics. Excess returned labels or process
// identities never become a finding target, frame or gap name.
func connectHandshakePointAt(history *stormHistory, inventory fleetSlotInventory, at int64, gaps map[string]bool) connectHandshakePoint {
	point := connectHandshakePoint{largestDependencySlot: "none", lowestAcceptanceSlot: "none"}
	worstShare, largestDependency := math.Inf(1), 0.0
	totals, acceptedRates, dependencyRates := []float64{}, []float64{}, []float64{}
	for _, slot := range inventory.slots {
		key := slot.host + "/" + slot.block
		publishers, qualified := history.reading("publishers", key, at), history.reading("qualified", key, at)
		total, accepted, dependency := history.reading("total", key, at), history.reading("accepted", key, at), history.reading("dependency", key, at)
		if !slot.allowed || !publishers.present || publishers.value != 1 || !qualified.present || qualified.value != 1 ||
			!total.present || !accepted.present || !dependency.present || math.IsInf(total.value, 0) ||
			total.value < 0 || accepted.value < 0 || accepted.value > total.value || dependency.value < 0 || dependency.value > total.value ||
			(math.IsInf(accepted.value+dependency.value, 1) || accepted.value+dependency.value > math.Nextafter(total.value, math.Inf(1))) {
			gaps[slot.label()] = true
			continue
		}
		point.slots++
		totals = append(totals, total.value)
		acceptedRates = append(acceptedRates, accepted.value)
		dependencyRates = append(dependencyRates, dependency.value)
		if largestDependency < dependency.value {
			point.largestDependencySlot, largestDependency = key, dependency.value
		}
		if total.value > 0 && accepted.value/total.value < worstShare {
			point.lowestAcceptanceSlot, worstShare = key, accepted.value/total.value
		}
	}
	point.total, point.accepted, point.dependency = contractFailureRateSum(totals), contractFailureRateSum(acceptedRates), contractFailureRateSum(dependencyRates)
	point.complete = point.slots > 0 && point.slots == len(inventory.slots) && len(inventory.missingServices) == 0
	return point
}

// An absent denominator supplies no evidence about acceptance.
func (self connectHandshakePoint) acceptance() stormReading {
	if !self.complete || !(self.total > 0 && self.total < math.Inf(1)) {
		return stormReading{}
	}
	return stormValue(self.accepted / self.total)
}

// Run all policy and visibility decisions on one bounded range. Partial
// high absolute counts can alert; a fleet share or recovery needs every slot.
func (self connectHandshakeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.policy()
	target := env.cfg.env + "/connect-h1"
	unknown := func(reasons []string, gaps []string) finding {
		return stormUnobservable(self.id(), connectHandshakeUnknownClass, target, "§25.2", reasons, gaps, 8)
	}
	if !settings.valid() {
		return []finding{unknown([]string{"invalid_policy"}, nil)}, nil
	}
	inventory := newFleetSlotInventory(env, []string{"connect"}, nil)
	selector := inventory.selector(env.cfg.env)
	if selector == "" {
		return []finding{unknown([]string{"placement_unavailable"}, nil)}, nil
	}
	steps := func(hold time.Duration) int { return settings.holdSteps(hold) + 1 }
	history, err := readStormHistory(ctx, env, connectHandshakeQuery(selector, settings), connectHandshakeShapes, steps(max(settings.DependencyHold, settings.AcceptanceWarnHold)), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unknown([]string{stormReadReason(err)}, nil), err)}, nil
	}
	if reasons := history.invalidReasons(); len(reasons) != 0 {
		return []finding{unknown(reasons, nil)}, nil
	}
	slotGaps := map[string]bool{}
	points := map[int64]connectHandshakePoint{}
	missingDenominator := false
	for _, at := range history.steps {
		point := connectHandshakePointAt(history, inventory, at, slotGaps)
		points[at] = point
		missingDenominator = missingDenominator || (point.complete && !point.acceptance().present)
	}
	dependencyHold := func(threshold float64) stormHold {
		return stormHoldOf(history.last(steps(settings.DependencyHold)), stormLevel{threshold: .5}, func(at int64) stormReading {
			point := points[at]
			if !(point.dependency >= 0 && point.dependency < math.Inf(1)) {
				return stormReading{}
			}
			if point.slots > 0 && point.dependency >= threshold {
				return stormValue(1)
			}
			if point.complete {
				return stormValue(0)
			}
			return stormReading{}
		})
	}
	acceptanceHold := func(threshold float64, hold time.Duration) stormHold {
		return stormHoldOf(history.last(steps(hold)), stormLevel{threshold: threshold, under: true}, func(at int64) stormReading { return points[at].acceptance() })
	}
	dependencyWarn, dependencyPage := dependencyHold(settings.DependencyWarnRate), dependencyHold(settings.DependencyPageRate)
	acceptanceWarn, acceptancePage := acceptanceHold(settings.AcceptanceWarnShare, settings.AcceptanceWarnHold), acceptanceHold(settings.AcceptancePageShare, settings.AcceptancePageHold)
	findings := []finding{}
	gaps := []string{}
	for gap := range slotGaps {
		gaps = append(gaps, gap)
	}
	sort.Strings(gaps)
	reasons := []string{}
	if len(gaps) != 0 {
		reasons = append(reasons, "slot_coverage_incomplete")
	}
	if missingDenominator {
		reasons = append(reasons, "acceptance_denominator_unavailable")
	}
	if len(reasons) > 0 {
		findings = append(findings, unknown(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(self.id(), tierWarn, connectHandshakeUnknownClass, target))
	}
	switch {
	case dependencyPage.held || acceptancePage.held || dependencyWarn.held || acceptanceWarn.held:
		tier := tierWarn
		if dependencyPage.held || acceptancePage.held {
			tier = tierPage
		}
		last := points[history.at()]
		findings = append(findings, finding{
			probeId: self.id(), tier: tier, class: connectHandshakeClass, target: target, sustain: 1,
			symptom:   "Connect H1 admission sustained high authentication dependency refusals or low acceptance",
			mechanism: "Connect records one terminal handshake outcome for websocket or H1+. A dependency refusal occurs during admission. Client cancellation and rollout drains also reduce the accepted share.",
			baseline:  fmt.Sprintf("WARN at %.0f dependency refusals/s for %s or below %.0f%% acceptance for %s. PAGE at %.0f/s for %s or below %.0f%% for %s.", settings.DependencyWarnRate, settings.DependencyHold, 100*settings.AcceptanceWarnShare, settings.AcceptanceWarnHold, settings.DependencyPageRate, settings.DependencyHold, 100*settings.AcceptancePageShare, settings.AcceptancePageHold),
			observed:  fmt.Sprintf("dependency_per_second=%.3f acceptance_share=%s completed_per_second=%.3f slots_observed=%d/%d largest_dependency_slot=%s lowest_acceptance_slot=%s held_dependency_warn=%t held_dependency_page=%t held_acceptance_warn=%t held_acceptance_page=%t evaluation_unix=%d", last.dependency, stormNumber(last.acceptance(), "%.4f"), last.total, last.slots, len(inventory.slots), last.largestDependencySlot, last.lowestAcceptanceSlot, dependencyWarn.held, dependencyPage.held, acceptanceWarn.held, acceptancePage.held, history.at()),
			evidence:  fmt.Sprintf("One bounded Mimir range query sums %s rates of %s on the placed Connect slots. Dependency counts on a partial fleet are a lower bound.", settings.Window, connectHandshakeMetric),
			context:   "October 19 reached 159 dependency refusals/s and 20% acceptance. An accepted transport can still fail at contract creation. H3 and attempts that never reach the handler have independent observation paths.",
			action:    "Inspect Connect admission outcomes on the affected host and block. For auth_dependency compare PgBouncer and Redis. For falling acceptance inspect client_canceled, draining, client releases and rollout timing.",
			verify:    fmt.Sprintf("Every placed Connect slot reports. Dependency refusals stay below %.0f/s for %s and positive-volume acceptance stays at or above %.0f%% for %s.", settings.DependencyWarnRate, settings.DependencyHold, 100*settings.AcceptanceWarnShare, settings.AcceptanceWarnHold),
			playbook:  "SIGNALS.md §25.2",
		})
	case dependencyWarn.clear && acceptanceWarn.clear:
		findings = append(findings, healthyFinding(self.id(), tierPage, connectHandshakeClass, target))
	default:
		findings = append(findings, stormClearHold(self.id(), connectHandshakeClass, target))
	}
	return findings, nil
}
