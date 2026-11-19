// SIGNALS.md §25.4 measures bytes observed on a fixed cohort of Connect exchange
// and hosted proxy slots. Neither boundary measures customer application receipt.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	deliveredTrafficClass        = "delivered-traffic-floor"
	deliveredTrafficProbeId      = "mimir/" + deliveredTrafficClass
	deliveredTrafficUnknownClass = "delivered-traffic-floor-unobservable"
	// The catalog defines the seasonal reference as exactly seven separate days.
	deliveredTrafficHistoryDays = 7
)

// The current and historical source windows, relative floors and read limits.
type DeliveredTrafficFloorSettings struct {
	// Window is the trailing interval of each observed byte rate, positive and at most an hour.
	Window time.Duration
	// WarnHold and PageHold span complete sampled intervals, including both endpoints.
	WarnHold, PageHold time.Duration
	// WarnRatio and PageRatio are strict lower bounds relative to the positive reference median.
	WarnRatio, PageRatio float64
	// ReadBudget bounds all eight sequential history reads combined.
	ReadBudget time.Duration
	StormReadSettings
}

// The October replay compares half-hour rates at the same UTC times on seven days.
func DefaultDeliveredTrafficFloorSettings() *DeliveredTrafficFloorSettings {
	return &DeliveredTrafficFloorSettings{
		Window: 30 * time.Minute, WarnHold: 15 * time.Minute, PageHold: 10 * time.Minute,
		WarnRatio: .5, PageRatio: .25, ReadBudget: 50 * time.Second,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

// Registers independent Connect and proxy volume targets for the same signal.
func NewDeliveredTrafficFloorSignal() Signal {
	return NewDeliveredTrafficFloorSignalWithSettings(DefaultDeliveredTrafficFloorSettings())
}

// Exposes the observation windows and bounds for synthetic and alternate callers.
func NewDeliveredTrafficFloorSignalWithSettings(settings *DeliveredTrafficFloorSettings) Signal {
	if settings == nil {
		settings = DefaultDeliveredTrafficFloorSettings()
	}
	return &signalAdapter{number: "25.4", key: deliveredTrafficClass, name: "Observed exchange and hosted proxy traffic floor", probe: deliveredTrafficProbe{settings: settings}}
}

// A stateless reader. Mimir supplies each observation at its own evaluation time.
type deliveredTrafficProbe struct {
	settings *DeliveredTrafficFloorSettings
}

func (deliveredTrafficProbe) id() string                    { return deliveredTrafficProbeId }
func (deliveredTrafficProbe) tier() string                  { return tierPage }
func (self deliveredTrafficProbe) cadence() time.Duration   { return self.settings.schedule() }
func (self deliveredTrafficProbe) runBudget() time.Duration { return self.settings.ReadBudget }

// The metrics have finite label sets. Connect eagerly binds its complete current
// schema. The hosted proxy binds a pair of directions on the first dial per protocol.
type deliveredTrafficFamily struct {
	service, part, metric, matchers, labels string
	cells                                   int
}

var deliveredTrafficFamilies = [...]deliveredTrafficFamily{
	{service: "connect", part: "socket", metric: "urnetwork_connect_exchange_io_bytes_total", matchers: `op=~"unknown|transport|forward",direction=~"sent|received",kind=~"data|ping|handshake"`, labels: "op,direction,kind", cells: 18},
	{service: "connect", part: "local", metric: "urnetwork_connect_exchange_local_bytes_total", matchers: `op=~"transport|forward",direction=~"sent|received",kind=~"data|ping|handshake"`, labels: "op,direction,kind", cells: 12},
	{service: "proxy", part: "session", metric: "urnetwork_proxy_session_bytes_total", matchers: `protocol=~"http|socks",direction=~"client_to_destination|destination_to_client"`, labels: "protocol,direction", cells: 4},
}

// Use native process starts as the slot census, independently of the byte
// families. Return only aggregate rates after every expected child qualified.
func deliveredTrafficQuery(environment string, inventories map[string]fleetSlotInventory, settings *DeliveredTrafficFloorSettings) string {
	window := stormDuration(settings.Window)
	scope := "env=" + strconv.Quote(environment)
	slot := "job,host,block"
	process := slot + ",instance"
	group := func(op, labels, expr string) string { return op + " by (" + labels + ") (" + expr + ")" }
	seen := func(metric string) string {
		return "(count_over_time(" + metric + "[" + window + "]) or " + metric + " offset " + window + " or " + metric + ")"
	}
	allStarts := "process_start_time_seconds{" + scope + `,job=~"connect|proxy"}`
	parts := []string{fleetPart("publishers", group("count", slot, group("group", process, seen(allStarts))))}
	for _, family := range deliveredTrafficFamilies {
		selector := inventories[family.service].selector(environment)
		if selector == "" {
			continue
		}
		identity := selector + ",service=" + strconv.Quote(family.service) + `,instance=~"[0-9a-f]{8}"`
		raw := family.metric + "{" + selector + "}"
		allowed := family.metric + "{" + identity + "," + family.matchers + "}"
		start := "process_start_time_seconds{" + identity + "}"
		offset := " offset " + window
		stamp := func(expr string) string { return "timestamp(" + expr + ")" }
		seconds := strconv.FormatInt(int64(settings.Window/time.Second), 10)
		freshness := strconv.FormatInt(int64(settings.Freshness/time.Second), 10)
		allProcessStarts := "process_start_time_seconds{" + selector + "}"
		stable := strings.Join([]string{
			"(" + start + " > 0)", "(" + start + " <= time() - " + seconds + ")",
			"(" + start + " == " + start + offset + ")", "(changes(" + start + "[" + window + "]) == 0)",
			"(" + stamp(start) + " >= time() - " + freshness + ")",
			"(" + stamp(start+offset) + " >= time() - " + seconds + " - " + freshness + ")",
		}, " and ") + " and on (" + process + ") (" + group("count", process, seen(allProcessStarts)) + " == 1)"
		// The gateway timestamps each complete push once. Both endpoint counter
		// samples must come from the same pushes as the stable process starts.
		samePush := func(shift string) string {
			return "(" + stamp(raw+shift) + " == on (" + process + ") group_left " + group("max", process, stamp(start+shift)) + ")"
		}
		good := strings.Join([]string{
			allowed, "(" + raw + offset + " >= 0)", "(" + raw + " < +Inf)",
			"(min_over_time(" + raw + "[" + window + "]) >= " + raw + offset + ")",
			"(resets(" + raw + "[" + window + "]) == 0)", samePush(""), samePush(offset),
			"on (" + process + ") (" + stable + ")",
		}, " and ")
		// Check both distinct declared cells and all series seen. Duplicate
		// labels, vanished children or two generations cannot replace a missing cell.
		n := strconv.Itoa(family.cells)
		cells := group("count", slot, group("group", slot+","+family.labels, good))
		rates := "(" + raw + " - " + raw + offset + ") / (" + stamp(raw) + " - " + stamp(raw+offset) + ")"
		parts = append(parts, fleetPart(family.part, group("sum", slot, rates)+
			" and ("+cells+" == "+n+") and ("+group("count", slot, seen(raw))+" == "+n+")"))
	}
	return strings.Join(parts, " or ")
}

var deliveredTrafficShapes = map[string]stormShape{
	"publishers": {keys: []string{"job", "host", "block"}},
	"socket":     {keys: []string{"job", "host", "block"}}, "local": {keys: []string{"job", "host", "block"}},
	"session": {keys: []string{"job", "host", "block"}},
}

// Evaluates one service at one point. An observed historical extra publisher,
// a disabled or missing slot, and a missing component all disqualify that cohort.
func deliveredTrafficRate(history *stormHistory, service string, slots map[string]bool, at int64) stormReading {
	if len(slots) == 0 {
		return stormReading{}
	}
	for _, key := range history.keys("publishers") {
		if strings.HasPrefix(key, service+"/") && !slots[key] {
			if _, present := history.values["publishers"][key][at]; present || history.ambiguous["publishers"][key] {
				return stormReading{}
			}
		}
	}
	var rates []float64
	for key, allowed := range slots {
		publishers := history.reading("publishers", key, at)
		if !allowed || !publishers.present || publishers.value != 1 {
			return stormReading{}
		}
		for _, family := range deliveredTrafficFamilies {
			if family.service != service {
				continue
			}
			rate := history.reading(family.part, key, at)
			if !rate.present || rate.value < 0 || math.IsInf(rate.value, 0) {
				return stormReading{}
			}
			rates = append(rates, rate.value)
		}
	}
	total := contractFailureRateSum(rates)
	if math.IsInf(total, 0) || math.IsNaN(total) {
		return stormReading{}
	}
	return stormValue(total)
}

// Read eight disjoint half-hour windows, never every intervening minute of the
// week. The fixed end clock also keeps a slow transport from shifting reference days.
func (self deliveredTrafficProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	services := []string{"connect", "proxy"}
	target := func(service string) string { return env.cfg.env + "/" + service }
	unavailable := func(reason string, err error) []finding {
		var out []finding
		for _, service := range services {
			unknown := stormUnobservable(self.id(), deliveredTrafficUnknownClass, target(service), "§25.4", []string{reason}, nil, 1)
			out = append(out, stormReadFailureFinding(unknown, err), stormClearHold(self.id(), deliveredTrafficClass, target(service)))
		}
		return out
	}
	if settings.Window < time.Second || settings.Window > time.Hour || settings.Window%time.Second != 0 ||
		settings.Freshness < time.Second || settings.Freshness >= settings.Window || settings.Freshness%time.Second != 0 ||
		!(0 < settings.PageRatio && settings.PageRatio < settings.WarnRatio && settings.WarnRatio <= 1) ||
		settings.PageHold > settings.WarnHold || settings.ReadBudget <= 0 || !settings.valid(settings.WarnHold, settings.PageHold) {
		return unavailable("invalid_policy", nil), nil
	}
	inventories := map[string]fleetSlotInventory{}
	expected := map[string]map[string]bool{}
	for _, service := range services {
		inventory := newFleetSlotInventory(env, []string{service}, nil)
		inventories[service] = inventory
		slots := map[string]bool{}
		for _, slot := range inventory.slots {
			slots[slot.label()] = slot.allowed
		}
		expected[service] = slots
	}
	if inventories["connect"].selector(env.cfg.env) == "" && inventories["proxy"].selector(env.cfg.env) == "" {
		return unavailable("placement_unavailable", nil), nil
	}
	query := deliveredTrafficQuery(env.cfg.env, inventories, settings)
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag).UTC()
	warnPoints := 1 + settings.holdSteps(settings.WarnHold)
	pagePoints := 1 + settings.holdSteps(settings.PageHold)
	var histories [deliveredTrafficHistoryDays + 1]*stormHistory
	for day := range histories {
		history, err := readStormHistoryAt(ctx, env, query, deliveredTrafficShapes, warnPoints, settings.StormReadSettings, at.AddDate(0, 0, -day))
		if lifeErr := probeLifecycleError(ctx, err); lifeErr != nil {
			return nil, lifeErr
		}
		if err != nil {
			return unavailable(stormReadReason(err), err), nil
		}
		if len(history.reasons) != 0 {
			return unavailable("invalid_series_schema", nil), nil
		}
		histories[day] = history
	}
	var out []finding
	for _, service := range services {
		ratios := map[int64]stormReading{}
		var current, reference stormReading
		gaps := map[string]bool{}
		for i, step := range histories[0].steps {
			current = deliveredTrafficRate(histories[0], service, expected[service], step)
			if !current.present {
				gaps["current_cohort_incomplete"] = true
			}
			var past []float64
			for _, day := range histories[1:] {
				rate := deliveredTrafficRate(day, service, expected[service], day.steps[i])
				if rate.present {
					past = append(past, rate.value)
				}
			}
			reference = stormReading{}
			if len(past) != deliveredTrafficHistoryDays {
				gaps["historical_cohort_incomplete"] = true
				continue
			}
			median := medianFloat64(past)
			if median == 0 {
				gaps["zero_reference_median"] = true
				continue
			}
			reference = stormValue(median)
			if current.present {
				ratio := current.value / median
				if math.IsInf(ratio, 0) {
					gaps["invalid_ratio"] = true
					continue
				}
				ratios[step] = stormValue(ratio)
			}
		}
		serviceTarget := target(service)
		if len(gaps) > 0 {
			var reasons []string
			for reason := range gaps {
				reasons = append(reasons, reason)
			}
			out = append(out, stormUnobservable(self.id(), deliveredTrafficUnknownClass, serviceTarget, "§25.4", reasons, nil, len(reasons)))
		} else {
			out = append(out, healthyFinding(self.id(), tierWarn, deliveredTrafficUnknownClass, serviceTarget))
		}
		ratioAt := func(step int64) stormReading { return ratios[step] }
		warn := stormHoldOf(histories[0].last(warnPoints), stormLevel{threshold: settings.WarnRatio, under: true}, ratioAt)
		page := stormHoldOf(histories[0].last(pagePoints), stormLevel{threshold: settings.PageRatio, under: true}, ratioAt)
		switch {
		case warn.held || page.held:
			severity := tierWarn
			if page.held {
				severity = tierPage
			}
			out = append(out, finding{probeId: self.id(), class: deliveredTrafficClass, target: serviceTarget, tier: severity, sustain: 1,
				symptom:   "Observed " + service + " bytes per second fell below their seasonal floor on matched service slots",
				observed:  fmt.Sprintf("current_bytes_per_second=%.3f reference_median_bytes_per_second=%.3f ratio=%.4g reference_days=%d matched_slots=%d evaluation_unix=%d", current.value, reference.value, ratios[at.Unix()].value, deliveredTrafficHistoryDays, len(expected[service]), at.Unix()),
				baseline:  fmt.Sprintf("At every sampled endpoint, WARN below %.0f%% for %s and PAGE below %.0f%% for %s of the prior seven same-window rates.", 100*settings.WarnRatio, settings.WarnHold, 100*settings.PageRatio, settings.PageHold),
				mechanism: "Connect counts socket and in-process exchange traffic in both directions, including framing. Hosted proxy counts HTTP and SOCKS upstream reads and writes. A change in demand also changes these counts.",
				evidence:  fmt.Sprintf("Eight bounded Mimir reads compare %s rates on the same desired slots with qualified native process and counter evidence in each window.", settings.Window),
				context:   "Customer demand, downstream application receipt and the intended fleet outside the matched slots require independent evidence.",
				action:    "Compare provider supply, handshake refusals, returned contracts, demand changes and rollouts before attributing the decline to a service failure.",
				verify:    fmt.Sprintf("All matched slots and seven references qualify and observed traffic is at or above %.0f%% of its reference for %s.", 100*settings.WarnRatio, settings.WarnHold),
				playbook:  "SIGNALS.md §25.4"})
		case warn.clear:
			out = append(out, healthyFinding(self.id(), tierPage, deliveredTrafficClass, serviceTarget))
		default:
			out = append(out, stormClearHold(self.id(), deliveredTrafficClass, serviceTarget))
		}
	}
	return out, nil
}
