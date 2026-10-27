// SIGNALS.md §26 shared history read of the backlog and storm signals from the
// 2026-10-26 incidents: pgbouncer-backlog (§26.1), services-cpu (§26.2),
// db-acquire-wait (§26.3), contract-creations-stuck (§26.4),
// reservation-release (§26.6), redis-pool-timeouts (§26.7), redis-main-thread
// (§26.8), proxy-host-cpu (§26.9), contract-reply-collapse (§26.10) and
// hosted-contract-cap (§26.11), each in signal_<key>.go.
//
// Each probe evaluates its catalog PromQL at every step of one bounded
// query_range through the loopback Mimir of a services host, ending
// EvaluationLag behind now. "For N minutes" is §26's: N consecutive one-minute
// evaluations held. The history is read on every run, so a probe the watcher
// runs every 15 minutes still judges every minute of its hold, and a monitor
// restart cannot learn an active storm as healthy.
//
// A band holds when every one of its last N evaluations is beyond its level,
// and clears only when every one has a value within it. Anything between, a
// hold the series broke or an evaluation without a value, opens nothing and
// keeps an open incident open. Answers carry only bounded labels (service,
// host, block, Redis node port, compensation result); process instance nonces
// and response bodies never reach an alert.
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

// The read every §26 probe shares.
type StormReadSettings struct {
	// How often the probe runs; a watcher's cadence floor can only slow it.
	Cadence time.Duration
	// The spacing of the evaluations a hold counts. §26 replays at one minute.
	Step time.Duration
	// The one bounded read: its lag behind now, its time limit and the
	// largest answer, in bytes and in range points.
	FleetReadSettings
}

// The defaults of the §26 read.
func DefaultStormReadSettings() StormReadSettings {
	return StormReadSettings{
		Cadence:           time.Minute,
		Step:              time.Minute,
		FleetReadSettings: DefaultFleetReadSettings(),
	}
}

// The most evaluations one hold may count, which bounds one read.
const stormMaxHoldSteps = 120

// Whether the read can judge holds of the given lengths: each a whole number
// of steps, and steps of whole seconds.
func (self StormReadSettings) valid(holds ...time.Duration) bool {
	if self.Cadence <= 0 || self.Step < time.Second || self.Step%time.Second != 0 ||
		self.EvaluationLag < 0 || self.QueryTimeout < time.Second ||
		self.MaxResponseBytes <= 0 || self.MaxRows <= 0 {
		return false
	}
	for _, hold := range holds {
		if hold < self.Step || hold%self.Step != 0 || stormMaxHoldSteps < hold/self.Step {
			return false
		}
	}
	return true
}

// The evaluations a hold of this length counts.
func (self StormReadSettings) holdSteps(hold time.Duration) int {
	return int(hold / self.Step)
}

// The cadence a probe hands the scheduler: an invalid one falls back to the
// default, so a bad setting cannot become a busy loop.
func (self StormReadSettings) schedule() time.Duration {
	if self.Cadence <= 0 {
		return DefaultStormReadSettings().Cadence
	}
	return self.Cadence
}

// How a part's series are named: the labels that key a series, and the other
// labels a series may carry. Any other label is an unrecognized series.
type stormShape struct {
	keys  []string
	extra []string
}

// A failed read, by a fixed reason; the source's text never leaves it.
type stormReadError struct {
	reason string
	cause  error
}

func (self *stormReadError) Error() string { return "storm history read: " + self.reason }

func (self *stormReadError) Unwrap() error { return self.cause }

// The fixed reason of a failed read.
func stormReadReason(err error) string {
	if readErr, ok := err.(*stormReadError); ok {
		return readErr.reason
	}
	return "source_unavailable"
}

// One §26 history: every part's series by key, with each value by step.
type stormHistory struct {
	// the evaluation steps in Unix seconds, oldest first; the last is the
	// evaluation instant
	steps     []int64
	values    map[string]map[string]map[int64]float64
	ambiguous map[string]map[string]bool
	// fixed reasons the answer carried series no shape names
	reasons map[string]bool
}

// Reads points evaluations of query at settings.Step, the last EvaluationLag
// behind now, and keys each tagged series by its part's shape.
func readStormHistory(ctx context.Context, env *probeEnv, query string, shapes map[string]stormShape, points int, settings StormReadSettings) (*stormHistory, error) {
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
	start := at.Add(-time.Duration(points-1) * settings.Step)
	raw, _, err := readFleetMimirRange(ctx, env, query, start, at, settings.Step, settings.FleetReadSettings)
	if err != nil {
		return nil, &stormReadError{reason: "source_unavailable", cause: err}
	}
	series, err := parseFleetSeries(raw, start, at, settings.Step, settings.FleetReadSettings)
	if err != nil {
		return nil, &stormReadError{reason: "invalid_response", cause: err}
	}
	history := &stormHistory{
		values:    map[string]map[string]map[int64]float64{},
		ambiguous: map[string]map[string]bool{},
		reasons:   map[string]bool{},
	}
	for step := start; !at.Before(step); step = step.Add(settings.Step) {
		history.steps = append(history.steps, step.Unix())
	}
	for _, one := range series {
		shape, known := shapes[one.part]
		if !known {
			history.reasons["unknown_part"] = true
			continue
		}
		key, reason := stormSeriesKey(one.labels, shape)
		if reason != "" {
			history.reasons[reason] = true
			continue
		}
		if history.values[one.part] == nil {
			history.values[one.part] = map[string]map[int64]float64{}
			history.ambiguous[one.part] = map[string]bool{}
		}
		if _, duplicate := history.values[one.part][key]; duplicate {
			history.ambiguous[one.part][key] = true
			continue
		}
		history.values[one.part][key] = one.points
	}
	return history, nil
}

// A series' key under its shape, or the fixed reason it has none.
func stormSeriesKey(labels map[string]string, shape stormShape) (string, string) {
	allowed := map[string]bool{}
	for _, name := range shape.keys {
		allowed[name] = true
	}
	for _, name := range shape.extra {
		allowed[name] = true
	}
	for name := range labels {
		if !allowed[name] {
			return "", "unrecognized_label"
		}
	}
	values := make([]string, 0, len(shape.keys))
	for _, name := range shape.keys {
		value := labels[name]
		if value == "" {
			return "", "unlabeled_series"
		}
		values = append(values, value)
	}
	return strings.Join(values, "/"), ""
}

// The steps of the last count evaluations, oldest first.
func (self *stormHistory) last(count int) []int64 {
	if len(self.steps) < count {
		return self.steps
	}
	return self.steps[len(self.steps)-count:]
}

// The evaluation instant.
func (self *stormHistory) at() int64 {
	return self.steps[len(self.steps)-1]
}

// The reading of a part's keyed series at a step. NaN, an ambiguous key and a
// missing step are gaps.
func (self *stormHistory) reading(part, key string, step int64) stormReading {
	if self.ambiguous[part][key] {
		return stormReading{}
	}
	value, present := self.values[part][key][step]
	if !present || math.IsNaN(value) {
		return stormReading{}
	}
	return stormValue(value)
}

// Whether the part answered the key at any step, ambiguous or not.
func (self *stormHistory) answered(part, key string) bool {
	_, present := self.values[part][key]
	return present || self.ambiguous[part][key]
}

// The keys a part answered, sorted.
func (self *stormHistory) keys(part string) []string {
	keys := []string{}
	for key := range self.values[part] {
		keys = append(keys, key)
	}
	for key := range self.ambiguous[part] {
		if _, present := self.values[part][key]; !present {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// The fixed reasons the answer carried unrecognized or ambiguous series.
func (self *stormHistory) invalidReasons() []string {
	reasons := []string{}
	for reason := range self.reasons {
		reasons = append(reasons, reason)
	}
	for _, keys := range self.ambiguous {
		if len(keys) != 0 {
			reasons = append(reasons, "ambiguous_series")
			break
		}
	}
	sort.Strings(reasons)
	return reasons
}

// A level a value must pass to count toward a hold: above it, or under it.
type stormLevel struct {
	threshold float64
	under     bool
}

func (self stormLevel) beyond(value float64) bool {
	if self.under {
		return value < self.threshold
	}
	return self.threshold < value
}

// One evaluation of a band: a value, a gap, or a step the band's own rule
// leaves out (neither beyond nor within its level, and no gap).
type stormReading struct {
	value    float64
	present  bool
	excluded bool
}

func stormValue(value float64) stormReading {
	return stormReading{value: value, present: true}
}

// A step the band's own rule leaves out, such as a process too young to judge.
var stormLeftOut = stormReading{excluded: true}

// The verdict of one band over its steps. held: every step has a value beyond
// the level. clear: every step has a value within it. complete: no step is a
// gap. A hold the series broke, or a step left out, is neither held nor clear.
type stormHold struct {
	held     bool
	clear    bool
	complete bool
}

// The verdict of a band whose reading at each step comes from readingAt.
func stormHoldOf(steps []int64, level stormLevel, readingAt func(step int64) stormReading) stormHold {
	beyond, within, gaps := 0, 0, 0
	for _, step := range steps {
		reading := readingAt(step)
		switch {
		case reading.excluded:
		case !reading.present:
			gaps++
		case level.beyond(reading.value):
			beyond++
		default:
			within++
		}
	}
	return stormHold{
		held:     0 < len(steps) && beyond == len(steps),
		clear:    0 < len(steps) && within == len(steps),
		complete: gaps == 0,
	}
}

// A bounded listing: at most limit names, then a count of the rest.
func stormListed(names []string, limit int) string {
	if len(names) == 0 {
		return "none"
	}
	if limit <= 0 || len(names) <= limit {
		return strings.Join(names, ",")
	}
	return strings.Join(names[:limit], ",") + fmt.Sprintf(",and %d more", len(names)-limit)
}

// A reading for an observed field; a gap is "unknown", never zero.
func stormNumber(reading stormReading, format string) string {
	switch {
	case reading.excluded:
		return "excluded"
	case !reading.present:
		return "unknown"
	}
	return fmt.Sprintf(format, reading.value)
}

// A PromQL duration as §26 spells its windows: whole hours as hours, whole
// minutes as minutes.
func stormDuration(duration time.Duration) string {
	if duration%time.Hour == 0 {
		return strconv.FormatInt(int64(duration/time.Hour), 10) + "h"
	}
	if duration%time.Minute == 0 {
		return strconv.FormatInt(int64(duration/time.Minute), 10) + "m"
	}
	return strconv.FormatInt(int64(duration/time.Second), 10) + "s"
}

// The clear hold of one incident identity: no absence credit from this run.
func stormClearHold(probeId, class, target string) finding {
	return finding{probeId: probeId, class: class, target: target, clearHold: true}
}

// The visibility finding of a §26 probe whose read, inventory or series are
// incomplete. It reports an unknown state, never a measured zero, and keeps
// every incident of its signal open while it lasts.
func stormUnobservable(probeId, class, target, section string, reasons []string, gaps []string, maxListed int) finding {
	sort.Strings(reasons)
	return finding{
		probeId: probeId, tier: tierWarn, class: class, target: target, sustain: 2,
		symptom:   "The " + section + " signal cannot judge every series it watches",
		mechanism: "The bounded Mimir history read failed, the inventory names no source to read, or a watched series is missing, ambiguous or without a value at an evaluation. A missing series is unknown, not zero: it can hide the backlog or storm this signal watches.",
		baseline:  "Every watched series answers at every one-minute evaluation of its hold, once.",
		observed:  fmt.Sprintf("visibility_reason=%s gaps=%s", stormListed(reasons, maxListed), stormListed(gaps, maxListed)),
		evidence:  "Fixed reasons and inventory names only; the response body, raw labels and process instance nonces stay in the monitor.",
		context:   "Positives the complete series prove are still reported beside this; nothing this signal reported clears while it lasts.",
		action:    "Restore the named sources (the exporter, the stats push or the services host's loopback Mimir path), then rerun the signal.",
		verify:    "Two consecutive runs judge every watched series at every evaluation.",
		playbook:  "SIGNALS.md " + section,
	}
}

// The hosts the active placement of services puts on, split into the hosts a
// read may name and the gaps (a disabled or excluded host, a service with no
// placement) that leave the set incomplete.
func stormPlacedHosts(env *probeEnv, services []string) ([]string, []string) {
	inventory := newFleetSlotInventory(env, services, nil)
	allowedBools, gapBools := map[string]bool{}, map[string]bool{}
	for _, slot := range inventory.slots {
		if slot.allowed {
			allowedBools[slot.host] = true
		} else {
			gapBools[slot.host+"[excluded]"] = true
		}
	}
	for _, service := range inventory.missingServices {
		gapBools[service+"[placement-missing]"] = true
	}
	hosts, gaps := []string{}, []string{}
	for host := range allowedBools {
		hosts = append(hosts, host)
	}
	for gap := range gapBools {
		gaps = append(gaps, gap)
	}
	sort.Strings(hosts)
	sort.Strings(gaps)
	return hosts, gaps
}

// The services among publishers that the active placement runs, sorted; a
// publisher the placement does not run creates no requirement.
func stormPlacedServices(env *probeEnv, publishers []string) []string {
	placed := map[string]bool{}
	for _, service := range env.cfg.logServices {
		placed[service] = true
	}
	services := []string{}
	for _, service := range publishers {
		if placed[service] {
			services = append(services, service)
		}
	}
	sort.Strings(services)
	return services
}

// §26.2 and §26.9's busy share of hosts' CPU: the non-idle, non-I/O-wait
// share, summed before dividing so each host weighs by its CPUs. by is empty
// for the hosts together, or the label to group by.
func stormBusyShareExpression(environment string, hosts []string, window time.Duration, by string) string {
	hostSet := map[string]bool{}
	for _, name := range hosts {
		hostSet[name] = true
	}
	selector := "env=" + strconv.Quote(environment) + ",host=~" + fleetPattern(hostSet)
	sum := "sum"
	if by != "" {
		sum = "sum by (" + by + ") "
	}
	rateWindow := "[" + stormDuration(window) + "]"
	return "1 - " + sum + "(rate(node_cpu_seconds_total{" + selector + `,mode=~"idle|iowait"}` + rateWindow + ")) / " +
		sum + "(rate(node_cpu_seconds_total{" + selector + "}" + rateWindow + "))"
}
