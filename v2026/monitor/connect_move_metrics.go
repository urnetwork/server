// SIGNALS.md §24 shared observation of the connect placement probes: mover
// (signal_mover.go), move-outcomes (signal_move_outcomes.go) and drain-moves
// (signal_drain_moves.go). Each reads the mover's and the move primitives'
// series (connect/move_metrics.go, connect/resident_move.go, connect/resident.go
// and controller/resident_move_result.go) with one bounded instant query
// through the loopback Mimir of one services host.
//
// Every series is per process and rides the stats pusher's {env, service,
// block, host, instance} labels. A series a process cannot compute is absent,
// and an absent series is unknown, never zero. Only a counter whose process is
// known to run the move code may read an absent lazy child as zero events. The
// mover ships off (connect/MOVE.md §5.8), so a fleet without its series, or
// with the mover off, is a standing state of its own: never healthy and never
// an incident.
//
// Query answers carry inventory host and block names, fixed label values and
// counts. Process instance nonces are private join keys and never reach an
// alert; nor does any response body, label text outside the fixed sets, or a
// move, client or address identity.
package monitor

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The thresholds, windows and bounds of SIGNALS.md §24, named as the sections
// name them, with their defaults. The bands are design targets
// (connect/MOVE.md §5.8), not measured production baselines.
type ConnectMoveSettings struct {
	// How often each probe runs; a watcher's cadence floor can only slow it.
	Cadence time.Duration
	// Mimir is read this far behind now: the newest pushes are partial while
	// the receiver catches up (RUN-MAIN.md, tier-0 vitals).
	EvaluationLag time.Duration
	// The bound on one query's transfer through the services host.
	QueryTimeout time.Duration
	// A process is current while its newest push is at most this old. The
	// stats pusher pushes every 15 s.
	ProcessFreshness time.Duration
	// How far a push may read ahead of the evaluation instant, or a process
	// start ahead of its push, before either is invalid: the receiver's clock
	// and the process's clock are not one clock.
	ClockSkew time.Duration
	// The step of every sustained window.
	SustainStep time.Duration
	// The largest query, the largest answer and the most answer rows one read
	// may carry, and the most desired host/block slots.
	MaxQueryBytes    int
	MaxResponseBytes int
	MaxRows          int
	MaxSlots         int
	// The longest label value an answer may carry.
	MaxLabelBytes int
	// The most hosts or drains one alert lists by name.
	MaxListed int

	// §24.4: the mode while the Redis control sets none (connect's
	// MoveDefaultMode), and the consecutive runs a divergent fleet holds before
	// it alerts, so a control change or a rollout step in flight is not one.
	DefaultMode       string
	DivergenceSustain int

	// §24.5: the window of the tick-outcome shares; the share of one process's
	// ticks that failed closed on a store error; the share of the fleet's
	// acting ticks the database guard paused, and the fewest acting ticks
	// that share is judged on; the fewest processes, and the least share of
	// their attempts, of a send-error burst on every sending process.
	TickWindow            time.Duration
	StoreErrorShare       float64
	DbPausedShare         float64
	MinActingTicks        int
	SendErrorMinProcesses int
	SendErrorMinShare     float64

	// §24.1: the max over min normalized load above which the hosts are
	// imbalanced at a step, held at every step of the window while the mover
	// is on; and the arrivals per minute of the window at the least loaded
	// host, whose load rose, that mark it filling under the target cap.
	ImbalanceRatio              float64
	ImbalanceWindow             time.Duration
	FillingMinArrivalsPerMinute float64

	// §24.2: the window of one arrived over sent ratio, how long the ratio
	// stays below the floor before it alerts, and the fewest sends one
	// window's ratio is judged on.
	SuccessWindow     time.Duration
	SuccessSustain    time.Duration
	SuccessRatioFloor float64
	MinSends          int

	// §24.6: the share of one target host's sends whose replacement failed,
	// over the success window, and how long it holds before it alerts; how
	// long a breaker must read open, at every step of BreakerStep, to be held
	// open, and the held open addresses of one host that alert.
	TargetFailureShare      float64
	TargetFailureSustain    time.Duration
	BreakerHold             time.Duration
	BreakerStep             time.Duration
	BreakerMinOpenAddresses int

	// §24.7 and §24.3: the window of drain refusals, the window of drains and
	// their compulsory closes, and the closes above a drained process's
	// legacy residents that alert.
	RefusalWindow   time.Duration
	DrainWindow     time.Duration
	MinExcessCloses int
}

// The defaults of SIGNALS.md §24.
func DefaultConnectMoveSettings() *ConnectMoveSettings {
	return &ConnectMoveSettings{
		Cadence:          5 * time.Minute,
		EvaluationLag:    2 * time.Minute,
		QueryTimeout:     15 * time.Second,
		ProcessFreshness: 90 * time.Second,
		ClockSkew:        30 * time.Second,
		SustainStep:      5 * time.Minute,
		MaxQueryBytes:    64 * 1024,
		MaxResponseBytes: 4 * 1024 * 1024,
		MaxRows:          16384,
		MaxSlots:         64,
		MaxLabelBytes:    256,
		MaxListed:        8,

		DefaultMode:       "off",
		DivergenceSustain: 3,

		TickWindow:            15 * time.Minute,
		StoreErrorShare:       0.5,
		DbPausedShare:         0.5,
		MinActingTicks:        30,
		SendErrorMinProcesses: 2,
		SendErrorMinShare:     0.5,

		ImbalanceRatio:              1.5,
		ImbalanceWindow:             2 * time.Hour,
		FillingMinArrivalsPerMinute: 10,

		SuccessWindow:     time.Hour,
		SuccessSustain:    2 * time.Hour,
		SuccessRatioFloor: 0.5,
		MinSends:          20,

		TargetFailureShare:      0.25,
		TargetFailureSustain:    time.Hour,
		BreakerHold:             10 * time.Minute,
		BreakerStep:             time.Minute,
		BreakerMinOpenAddresses: 2,

		RefusalWindow:   time.Hour,
		DrainWindow:     24 * time.Hour,
		MinExcessCloses: 1,
	}
}

// Whether the settings can be judged by. Windows and steps must be whole
// seconds, so a query window is exactly the window the alert names.
func (self *ConnectMoveSettings) valid() bool {
	if self == nil {
		return false
	}
	for _, window := range []time.Duration{
		self.Cadence, self.EvaluationLag, self.QueryTimeout, self.ProcessFreshness, self.ClockSkew, self.SustainStep, self.TickWindow,
		self.ImbalanceWindow, self.SuccessWindow, self.SuccessSustain, self.TargetFailureSustain,
		self.BreakerHold, self.BreakerStep, self.RefusalWindow, self.DrainWindow,
	} {
		if window <= 0 || window%time.Second != 0 {
			return false
		}
	}
	for _, sustained := range []time.Duration{self.ImbalanceWindow, self.SuccessSustain, self.TargetFailureSustain} {
		if sustained < self.SustainStep {
			return false
		}
	}
	if self.BreakerHold < self.BreakerStep {
		return false
	}
	for _, share := range []float64{self.StoreErrorShare, self.DbPausedShare, self.SendErrorMinShare, self.SuccessRatioFloor, self.TargetFailureShare} {
		if !(0 < share && share <= 1) {
			return false
		}
	}
	for _, count := range []int{self.MaxQueryBytes, self.MaxResponseBytes, self.MaxRows, self.MaxSlots, self.MaxLabelBytes, self.MaxListed, self.DivergenceSustain, self.MinActingTicks, self.SendErrorMinProcesses, self.MinSends, self.BreakerMinOpenAddresses, self.MinExcessCloses} {
		if count <= 0 {
			return false
		}
	}
	_, modeKnown := connectMoveModeValues[self.DefaultMode]
	return 1 < self.ImbalanceRatio && !math.IsInf(self.ImbalanceRatio, 0) && 0 <= self.FillingMinArrivalsPerMinute && modeKnown
}

// The mover's modes as the mode gauge exports them (model.ConnectMoveMode).
var connectMoveModeValues = map[string]int{"off": 0, "dry_run": 1, "on": 2}

// The mode's control value, for observed text.
func connectMoveModeName(mode int) string {
	for name, value := range connectMoveModeValues {
		if value == mode {
			return name
		}
	}
	return "unknown"
}

// A PromQL duration of whole seconds.
func connectMoveDuration(window time.Duration) string {
	return strconv.FormatInt(int64(window/time.Second), 10) + "s"
}

// The desired connect slots, host by block, from the active services.yml.
// Placement survives exclusions: a host the monitor may not contact, a
// disabled host and a host outside the monitor inventory keep their slots,
// whose state stays unknown and is never borrowed from a sibling.
type connectMoveScope struct {
	hosts  []string
	blocks []string
	// the reason each such host's slots are unknown: excluded or
	// inventory_unknown
	unknownHostReasons map[string]string
	gateway            *host
}

// The desired slots, or a fixed reason they are unavailable.
func newConnectMoveScope(env *probeEnv, settings *ConnectMoveSettings) (*connectMoveScope, string) {
	hosts, blocks := env.cfg.logServiceHosts["connect"], env.cfg.logServiceBlocks["connect"]
	if len(hosts) == 0 || len(blocks) == 0 || settings.MaxSlots < len(hosts)*len(blocks) {
		return nil, "slot_inventory_unavailable_or_over_bound"
	}
	scope := &connectMoveScope{
		hosts:              append([]string(nil), hosts...),
		blocks:             append([]string(nil), blocks...),
		unknownHostReasons: map[string]string{},
	}
	sort.Strings(scope.hosts)
	sort.Strings(scope.blocks)
	for _, names := range [][]string{scope.hosts, scope.blocks} {
		for i, name := range names {
			if name == "" || (0 < i && names[i-1] == name) {
				return nil, "slot_inventory_incomplete"
			}
		}
	}
	excludedHosts := map[string]bool{}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		for _, name := range scoped.excludedNames() {
			excludedHosts[name] = true
		}
	}
	configuredHosts := map[string]*host{}
	for _, h := range env.cfg.scopeHosts() {
		configuredHosts[h.name] = h
		excludedHosts[h.name] = excludedHosts[h.name] || h.disabled
	}
	for _, name := range scope.hosts {
		switch {
		case excludedHosts[name]:
			scope.unknownHostReasons[name] = "excluded"
		case configuredHosts[name] == nil:
			scope.unknownHostReasons[name] = "inventory_unknown"
		}
	}
	for _, candidate := range env.cfg.hostsWithRole("services") {
		if !excludedHosts[candidate.name] {
			scope.gateway = candidate
			break
		}
	}
	return scope, ""
}

// The label matchers of the connect service's desired slots.
func (self *connectMoveScope) selector(environment string) string {
	pattern := func(values []string) string {
		quoted := make([]string, len(values))
		for i, value := range values {
			quoted[i] = regexp.QuoteMeta(value)
		}
		return strconv.Quote(strings.Join(quoted, "|"))
	}
	return "env=" + strconv.Quote(environment) + `,job="connect",service="connect",host=~` + pattern(self.hosts) + ",block=~" + pattern(self.blocks)
}

// One part of a tagged union query: the expression and the tag the monitor
// label gives its series.
func connectMoveTagged(tag string, expression string) string {
	return "label_replace(" + expression + `,"monitor_move",` + strconv.Quote(tag) + `,"","")`
}

// The label names of each tag's series besides the tag and the metric name,
// in key order.
type connectMoveShape map[string][]string

// The identity labels of a raw per-process series.
var connectMoveRawProcessLabels = []string{"env", "job", "service", "host", "block", "instance"}

// One answer of a tagged union query, the series of each tag keyed by the
// joined values of the tag's labels. The evaluation instant is the answer's.
type connectMoveFrame struct {
	at         time.Time
	tagSeries  map[string]map[string]float64
	tagLabels  map[string][]string
	seriesRows int
}

// The key of a tag's series from its label values, in shape order.
func connectMoveKey(values ...string) string {
	return strings.Join(values, "\x00")
}

// The tag's value for the label values, and whether it is present.
func (self *connectMoveFrame) value(tag string, values ...string) (float64, bool) {
	value, ok := self.tagSeries[tag][connectMoveKey(values...)]
	return value, ok
}

// The tag's label-free value, and whether it is present.
func (self *connectMoveFrame) scalar(tag string) (float64, bool) {
	return self.value(tag)
}

// Calls visit for each series of the tag, in key order.
func (self *connectMoveFrame) each(tag string, visit func(values []string, value float64)) {
	keys := make([]string, 0, len(self.tagSeries[tag]))
	for key := range self.tagSeries[tag] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		visit(strings.Split(key, "\x00"), self.tagSeries[tag][key])
	}
}

// Runs the query once, through the first services host the monitor may
// contact, at the evaluation instant. A failure is a fixed reason; only a
// parent that ended returns an error. No retry changes the source window.
func readConnectMoveFrame(
	ctx context.Context,
	env *probeEnv,
	settings *ConnectMoveSettings,
	scope *connectMoveScope,
	query string,
	at time.Time,
	shape connectMoveShape,
) (*connectMoveFrame, string, error) {
	if scope.gateway == nil {
		return nil, "metrics_gateway_unavailable", nil
	}
	if settings.MaxQueryBytes < len(query) {
		return nil, "query_bound", nil
	}
	command := "curl -fsS --max-time " + strconv.FormatInt(int64(settings.QueryTimeout/time.Second), 10) + " --max-filesize " + strconv.Itoa(settings.MaxResponseBytes) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(at.Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	output, err := mimirShellParsed(ctx, env.runner, scope.gateway, command, settings.MaxResponseBytes)
	select {
	case <-ctx.Done():
		if err != nil {
			return nil, "", &signalContextError{contextErr: ctx.Err(), sourceErr: err}
		}
		return nil, "", ctx.Err()
	default:
	}
	if err != nil {
		return nil, "bounded_source_unavailable", nil
	}
	frame, reason := parseConnectMoveFrame(output, at, settings, shape)
	return frame, reason, nil
}

// Strict decoding: duplicate object fields, unknown fields, annotations, an
// evaluation instant other than the request's, a non-finite or negative value,
// an unknown tag, a label set other than the tag's shape and a repeated series
// each reject the whole answer with a fixed reason. No label text or body
// reaches a reason.
func parseConnectMoveFrame(raw string, at time.Time, settings *ConnectMoveSettings, shape connectMoveShape) (*connectMoveFrame, string) {
	var response struct {
		Status    string   `json:"status"`
		Error     string   `json:"error"`
		ErrorType string   `json:"errorType"`
		Warnings  []string `json:"warnings"`
		Infos     []string `json:"infos"`
		Data      struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if settings.MaxResponseBytes < len(raw) || !providerConnectionsJsonShape(raw) || decoder.Decode(&response) != nil {
		return nil, "invalid_source_response"
	}
	if response.Status != "success" || response.Error != "" || response.ErrorType != "" || response.Data.ResultType != "vector" || settings.MaxRows < len(response.Data.Result) {
		return nil, "invalid_source_response"
	}
	if len(response.Warnings) != 0 || len(response.Infos) != 0 {
		return nil, "response_annotation_unqualified"
	}
	frame := &connectMoveFrame{
		at:         at,
		tagSeries:  map[string]map[string]float64{},
		tagLabels:  map[string][]string{},
		seriesRows: len(response.Data.Result),
	}
	for tag, labelNames := range shape {
		frame.tagSeries[tag] = map[string]float64{}
		frame.tagLabels[tag] = labelNames
	}
	for _, series := range response.Data.Result {
		tag := series.Metric["monitor_move"]
		labelNames, known := shape[tag]
		if !known {
			return nil, "unknown_series_tag"
		}
		labelCount := 0
		for name := range series.Metric {
			if name != "monitor_move" && name != "__name__" {
				labelCount++
			}
		}
		if labelCount != len(labelNames) {
			return nil, "invalid_series_labels"
		}
		values := make([]string, len(labelNames))
		for i, name := range labelNames {
			value, present := series.Metric[name]
			if !present || value == "" || settings.MaxLabelBytes < len(value) || strings.ContainsAny(value, "\x00\r\n") {
				return nil, "invalid_series_labels"
			}
			values[i] = value
		}
		evaluatedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || evaluatedAt.Unix() != at.Unix() || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, "invalid_series_value"
		}
		key := connectMoveKey(values...)
		if _, duplicate := frame.tagSeries[tag][key]; duplicate {
			return nil, "duplicate_series"
		}
		frame.tagSeries[tag][key] = value
	}
	return frame, ""
}

// One connect process: its slot, its private instance nonce, its start and
// the receiver time of its newest push.
type connectMoveProcess struct {
	host     string
	block    string
	instance string
	start    float64
	pushedAt float64
}

// The key of the process's raw per-process series.
func (self *connectMoveProcess) rawValues(environment string) []string {
	return []string{environment, "connect", "connect", self.host, self.block, self.instance}
}

// The key of the process's series aggregated by host, block and instance.
func (self *connectMoveProcess) values(extra ...string) []string {
	return append([]string{self.host, self.block, self.instance}, extra...)
}

// The current process of every desired slot, and why the other slots have
// none. A slot's current process is its newest start, which must be fresh: a
// draining older process never stands in for a newer stale one, and equal
// starts are ambiguous (provider-connections, connect-h1plus).
type connectMoveCensus struct {
	slotProcesses map[string]*connectMoveProcess
	reasonCounts  map[string]int
	slots         int
}

// The slot key of a host and block.
func connectMoveSlot(hostName string, block string) string {
	return hostName + "\x00" + block
}

// Builds the census from the start and start_time tags, each with the raw
// per-process labels. A start without its push time, or a series outside the
// desired slots, rejects the answer.
func newConnectMoveCensus(frame *connectMoveFrame, environment string, scope *connectMoveScope, settings *ConnectMoveSettings) (*connectMoveCensus, string) {
	census := &connectMoveCensus{
		slotProcesses: map[string]*connectMoveProcess{},
		reasonCounts:  map[string]int{},
		slots:         len(scope.hosts) * len(scope.blocks),
	}
	desiredSlots := map[string]bool{}
	for _, hostName := range scope.hosts {
		for _, block := range scope.blocks {
			desiredSlots[connectMoveSlot(hostName, block)] = true
		}
	}
	slotCandidates := map[string][]*connectMoveProcess{}
	invalid := ""
	frame.each("start", func(values []string, start float64) {
		if invalid != "" {
			return
		}
		if values[0] != environment || values[1] != "connect" || values[2] != "connect" || !desiredSlots[connectMoveSlot(values[3], values[4])] {
			invalid = "invalid_process_identity"
			return
		}
		pushedAt, ok := frame.value("start_time", values...)
		if !ok {
			invalid = "process_timestamp_companion_missing"
			return
		}
		process := &connectMoveProcess{host: values[3], block: values[4], instance: values[5], start: start, pushedAt: pushedAt}
		slot := connectMoveSlot(process.host, process.block)
		slotCandidates[slot] = append(slotCandidates[slot], process)
	})
	if invalid != "" {
		return nil, invalid
	}
	if len(frame.tagSeries["start_time"]) != len(frame.tagSeries["start"]) {
		return nil, "process_timestamp_companion_missing"
	}
	at, skew := float64(frame.at.Unix()), settings.ClockSkew.Seconds()
	for _, hostName := range scope.hosts {
		for _, block := range scope.blocks {
			slot := connectMoveSlot(hostName, block)
			if reason, unknown := scope.unknownHostReasons[hostName]; unknown {
				census.reasonCounts[reason]++
				continue
			}
			candidates := slotCandidates[slot]
			if len(candidates) == 0 {
				census.reasonCounts["missing"]++
				continue
			}
			var current *connectMoveProcess
			ambiguous := false
			for _, candidate := range candidates {
				switch {
				case current == nil || current.start < candidate.start:
					current, ambiguous = candidate, false
				case current.start == candidate.start:
					ambiguous = true
				}
			}
			age := at - current.pushedAt
			switch {
			case ambiguous:
				census.reasonCounts["ambiguous"]++
			case current.start <= 0 || current.pushedAt+skew < current.start:
				census.reasonCounts["invalid_start"]++
			case settings.ProcessFreshness.Seconds() < age || age < -skew:
				census.reasonCounts["stale"]++
			default:
				census.slotProcesses[slot] = current
			}
		}
	}
	return census, ""
}

// Whether every desired slot has a current process.
func (self *connectMoveCensus) complete() bool {
	return len(self.slotProcesses) == self.slots
}

// The current processes in slot order.
func (self *connectMoveCensus) processes() []*connectMoveProcess {
	slots := make([]string, 0, len(self.slotProcesses))
	for slot := range self.slotProcesses {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	processes := make([]*connectMoveProcess, len(slots))
	for i, slot := range slots {
		processes[i] = self.slotProcesses[slot]
	}
	return processes
}

// The slots without a current process, by fixed reason, for observed text.
func (self *connectMoveCensus) coverage() string {
	text := "desired_slots=" + strconv.Itoa(self.slots) + " current_slots=" + strconv.Itoa(len(self.slotProcesses))
	for _, reason := range []string{"missing", "stale", "ambiguous", "invalid_start", "excluded", "inventory_unknown"} {
		text += " " + reason + "_slots=" + strconv.Itoa(self.reasonCounts[reason])
	}
	return text
}

// At most limit names, then a count of the rest.
func connectMoveNames(names []string, limit int) string {
	if len(names) == 0 {
		return "none"
	}
	if len(names) <= limit {
		return strings.Join(names, ",")
	}
	return strings.Join(names[:limit], ",") + ",and_" + strconv.Itoa(len(names)-limit) + "_more"
}

// A count or share for observed text: whole numbers bare, others to three
// places.
func connectMoveNumber(value float64) string {
	if math.Trunc(value) == value && math.Abs(value) < 1e15 {
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	return strconv.FormatFloat(value, 'f', 3, 64)
}
