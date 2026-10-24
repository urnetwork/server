// Shared bounded fleet reads for the rate signals of SIGNALS.md §2.19d, §2.30,
// §14.8 and §23.3. Desired service slots come from active services.yml
// placement, never from the series that survived; a slot whose host is
// disabled, excluded or unconfigured is an explicit gap. One POSTed instant
// query through the first reachable services gateway returns samples tagged by
// a fixed part name, evaluated at an explicit time behind now so the newest
// partial scrapes are not read as a loss.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The label every fleet query part carries; its value names the part.
const fleetPartLabel = "monitor_part"

// The bounds of one fleet read, embedded in each fleet signal's settings.
type FleetReadSettings struct {
	// the read evaluates this far behind now, so the newest partial scrapes
	// are not read as a loss
	EvaluationLag time.Duration
	// a slot's family must be no older than Freshness and no further ahead
	// than FutureSkew at the evaluation clock
	Freshness  time.Duration
	FutureSkew time.Duration
	// the one gateway attempt's time limit, in whole seconds
	QueryTimeout time.Duration
	// the largest response, and the most series rows or range points in it
	MaxResponseBytes int
	MaxRows          int
}

// The shared defaults; each signal overrides its response bounds.
func DefaultFleetReadSettings() FleetReadSettings {
	return FleetReadSettings{
		EvaluationLag:    2 * time.Minute,
		Freshness:        3 * time.Minute,
		FutureSkew:       30 * time.Second,
		QueryTimeout:     15 * time.Second,
		MaxResponseBytes: 1024 * 1024,
		MaxRows:          4096,
	}
}

// One desired service placement. allowed is false for a disabled, excluded or
// unconfigured host, whose slot can only be unknown.
type fleetSlot struct {
	service string
	host    string
	block   string
	allowed bool
}

func (self fleetSlot) label() string {
	return self.service + "/" + self.host + "/" + self.block
}

// The desired slots of the named services. A required service without
// complete placement is listed in missingServices; an optional service with
// no placement creates no requirement.
type fleetSlotInventory struct {
	slots           []fleetSlot
	missingServices []string
}

func newFleetSlotInventory(env *probeEnv, services []string, optionalServices []string) fleetSlotInventory {
	optional := map[string]bool{}
	for _, service := range optionalServices {
		optional[service] = true
	}
	excluded := map[string]bool{}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		for _, name := range scoped.excludedNames() {
			excluded[name] = true
		}
	}
	enabled := map[string]bool{}
	for _, h := range env.cfg.scopeHosts() {
		if h.disabled {
			excluded[h.name] = true
		} else {
			enabled[h.name] = true
		}
	}
	inventory := fleetSlotInventory{}
	for _, service := range services {
		hosts, hasHosts := env.cfg.logServiceHosts[service]
		blocks, hasBlocks := env.cfg.logServiceBlocks[service]
		if optional[service] && len(hosts) == 0 {
			continue
		}
		if !hasHosts || !hasBlocks || len(hosts) == 0 || len(blocks) == 0 {
			inventory.missingServices = append(inventory.missingServices, service)
			continue
		}
		for _, name := range hosts {
			for _, block := range blocks {
				inventory.slots = append(inventory.slots, fleetSlot{
					service: service,
					host:    name,
					block:   block,
					allowed: enabled[name] && !excluded[name],
				})
			}
		}
	}
	sort.Slice(inventory.slots, func(i, j int) bool { return inventory.slots[i].label() < inventory.slots[j].label() })
	sort.Strings(inventory.missingServices)
	return inventory
}

// A PromQL label selector body over the allowed slots of every service. It is
// empty when no slot is allowed; callers then query no per-slot series.
func (self fleetSlotInventory) selector(environment string) string {
	services, hosts, blocks := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, slot := range self.slots {
		if slot.allowed {
			services[slot.service], hosts[slot.host], blocks[slot.block] = true, true, true
		}
	}
	if len(services) == 0 {
		return ""
	}
	return fmt.Sprintf("env=%s,job=~%s,host=~%s,block=~%s",
		strconv.Quote(environment), fleetPattern(services), fleetPattern(hosts), fleetPattern(blocks))
}

// An anchored PromQL regular expression matching exactly the given values.
func fleetPattern(values map[string]bool) string {
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, regexp.QuoteMeta(value))
	}
	sort.Strings(ordered)
	return strconv.Quote(strings.Join(ordered, "|"))
}

// Tags one PromQL expression with its fixed part name.
func fleetPart(part string, expression string) string {
	return fmt.Sprintf("label_replace(%s,%s,%s,\"\",\"\")", expression, strconv.Quote(fleetPartLabel), strconv.Quote(part))
}

// The freshest source timestamp of a metric family on every allowed slot,
// tagged fresh/<family part>.
func fleetSlotFreshnessPart(familyPart string, metric string, selector string) string {
	return fleetPart("fresh/"+familyPart, "max by (job,host,block) (timestamp("+metric+"{"+selector+"}))")
}

// The evaluation clock of a fleet read: whole seconds, behind now by lag.
func fleetEvaluationTime(now time.Time, lag time.Duration) time.Time {
	return now.Add(-lag).Truncate(time.Second)
}

// The bounded curl prefix of one loopback Mimir read.
func fleetCurl(read FleetReadSettings) string {
	return "curl -fsS --max-time " + strconv.FormatInt(max(1, int64(read.QueryTimeout/time.Second)), 10) +
		" --max-filesize " + strconv.Itoa(read.MaxResponseBytes)
}

// Exactly one bounded attempt per gateway; no retry changes the source window.
// The response is never rendered: callers receive fixed parse errors only.
func readFleetMimir(ctx context.Context, env *probeEnv, query string, at time.Time, read FleetReadSettings) (string, *host, error) {
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		return "", nil, fmt.Errorf("no services gateway for the loopback Mimir query")
	}
	command := fleetCurl(read) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(at.Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	return shellFirstServiceGateway(ctx, env.runner, gateways, nil, command)
}

// A bounded range read: the same tagged instant expression evaluated at every
// step from start through end, so per-minute history survives a probe that
// runs only every few minutes.
func readFleetMimirRange(ctx context.Context, env *probeEnv, query string, start time.Time, end time.Time, step time.Duration, read FleetReadSettings) (string, *host, error) {
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		return "", nil, fmt.Errorf("no services gateway for the loopback Mimir query")
	}
	command := fleetCurl(read) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("start="+strconv.FormatInt(start.Unix(), 10)) +
		" --data-urlencode " + shellSingleQuote("end="+strconv.FormatInt(end.Unix(), 10)) +
		" --data-urlencode " + shellSingleQuote("step="+strconv.FormatInt(int64(step/time.Second), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query_range'"
	return shellFirstServiceGateway(ctx, env.runner, gateways, nil, command)
}

// One tagged series of a range read, by evaluation step in Unix seconds.
type fleetSeries struct {
	part   string
	labels map[string]string
	points map[int64]float64
}

// Accepts only a complete, unannotated matrix whose points fall on the
// requested steps. A series may lack steps; callers decide what a gap means.
func parseFleetSeries(raw string, start time.Time, end time.Time, step time.Duration, read FleetReadSettings) ([]fleetSeries, error) {
	if read.MaxResponseBytes < len(raw) {
		return nil, fmt.Errorf("fleet response exceeds its bound")
	}
	var response struct {
		Status   string   `json:"status"`
		Error    string   `json:"error"`
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, fmt.Errorf("fleet response is not valid json")
	}
	if response.Status != "success" || response.Error != "" || response.Data.ResultType != "matrix" ||
		len(response.Warnings) != 0 || len(response.Infos) != 0 {
		return nil, fmt.Errorf("fleet response is not a clean matrix")
	}
	stepSeconds := int64(step / time.Second)
	if stepSeconds <= 0 || end.Before(start) {
		return nil, fmt.Errorf("fleet range is invalid")
	}
	points := 0
	series := make([]fleetSeries, 0, len(response.Data.Result))
	for _, result := range response.Data.Result {
		part := result.Metric[fleetPartLabel]
		if part == "" {
			return nil, fmt.Errorf("fleet response has an untagged series")
		}
		points += len(result.Values)
		if read.MaxRows < points {
			return nil, fmt.Errorf("fleet response exceeds its point bound")
		}
		labels := map[string]string{}
		for name, labelValue := range result.Metric {
			if name != fleetPartLabel && name != "__name__" {
				labels[name] = labelValue
			}
		}
		values := map[int64]float64{}
		for _, raw := range result.Values {
			evaluatedAt, value, err := mimirInstantValue(raw)
			if err != nil {
				return nil, fmt.Errorf("fleet response has an invalid value")
			}
			seconds := evaluatedAt.Unix()
			if evaluatedAt.Nanosecond() != 0 || seconds < start.Unix() || end.Unix() < seconds || (seconds-start.Unix())%stepSeconds != 0 {
				return nil, fmt.Errorf("fleet response has a point off its steps")
			}
			if _, duplicate := values[seconds]; duplicate {
				return nil, fmt.Errorf("fleet response repeats a step")
			}
			values[seconds] = value
		}
		series = append(series, fleetSeries{part: part, labels: labels, points: values})
	}
	return series, nil
}

// One tagged sample. labels excludes the part tag and the metric name.
type fleetSample struct {
	part   string
	labels map[string]string
	value  float64
}

// Accepts only a complete, unannotated vector evaluated at the requested
// clock. Values may be NaN or infinite: a ratio part decides what that means.
func parseFleetSamples(raw string, at time.Time, read FleetReadSettings) ([]fleetSample, error) {
	if read.MaxResponseBytes < len(raw) {
		return nil, fmt.Errorf("fleet response exceeds its bound")
	}
	var response struct {
		mimirInstantResponse
		Infos []string `json:"infos"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, fmt.Errorf("fleet response is not valid json")
	}
	if response.Status != "success" || response.Error != "" || response.Data.ResultType != "vector" ||
		len(response.Warnings) != 0 || len(response.Infos) != 0 {
		return nil, fmt.Errorf("fleet response is not a clean vector")
	}
	if read.MaxRows < len(response.Data.Result) {
		return nil, fmt.Errorf("fleet response exceeds its row bound")
	}
	samples := make([]fleetSample, 0, len(response.Data.Result))
	for _, series := range response.Data.Result {
		part := series.Metric[fleetPartLabel]
		if part == "" {
			return nil, fmt.Errorf("fleet response has an untagged series")
		}
		evaluatedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("fleet response has an invalid value")
		}
		if math.Abs(evaluatedAt.Sub(at).Seconds()) > 1 {
			return nil, fmt.Errorf("fleet response was evaluated at another time")
		}
		labels := map[string]string{}
		for name, labelValue := range series.Metric {
			if name != fleetPartLabel && name != "__name__" {
				labels[name] = labelValue
			}
		}
		samples = append(samples, fleetSample{part: part, labels: labels, value: value})
	}
	return samples, nil
}

// The samples of a range read at one step, for the per-slot coverage check.
func fleetSamplesAt(series []fleetSeries, at time.Time) []fleetSample {
	samples := []fleetSample{}
	for _, one := range series {
		if value, present := one.points[at.Unix()]; present {
			samples = append(samples, fleetSample{part: one.part, labels: one.labels, value: value})
		}
	}
	return samples
}

// Per-slot coverage of the families a signal needs. A slot is complete only
// when every family has a source sample no older than freshness at the
// evaluation clock and the optional check accepts it. Reasons are fixed words.
type fleetSlotCoverage struct {
	expected int
	complete int
	gaps     []string
	reasons  map[string]int
}

func newFleetSlotCoverage(
	inventory fleetSlotInventory,
	samples []fleetSample,
	families []string,
	at time.Time,
	read FleetReadSettings,
	check func(fleetSlot) string,
) fleetSlotCoverage {
	coverage := fleetSlotCoverage{expected: len(inventory.slots), reasons: map[string]int{}}
	sourceTimes := map[string]float64{}
	duplicate := map[string]bool{}
	for _, sample := range samples {
		family, ok := strings.CutPrefix(sample.part, "fresh/")
		if !ok {
			continue
		}
		key := family + "\x00" + sample.labels["job"] + "\x00" + sample.labels["host"] + "\x00" + sample.labels["block"]
		if _, exists := sourceTimes[key]; exists {
			duplicate[key] = true
		}
		sourceTimes[key] = sample.value
	}
	evaluated := float64(at.UnixNano()) / float64(time.Second)
	for _, service := range inventory.missingServices {
		coverage.gaps = append(coverage.gaps, service+"[placement-missing]")
		coverage.reasons["placement_missing"]++
	}
	for _, slot := range inventory.slots {
		reason := ""
		if !slot.allowed {
			reason = "excluded"
		} else {
			for _, family := range families {
				key := family + "\x00" + slot.service + "\x00" + slot.host + "\x00" + slot.block
				sourceTime, present := sourceTimes[key]
				switch {
				case duplicate[key]:
					reason = "ambiguous"
				case !present:
					reason = "missing"
				case math.IsNaN(sourceTime) || read.Freshness.Seconds() < evaluated-sourceTime || read.FutureSkew.Seconds() < sourceTime-evaluated:
					reason = "stale"
				}
				if reason != "" {
					break
				}
			}
			if reason == "" && check != nil {
				reason = check(slot)
			}
		}
		if reason == "" {
			coverage.complete++
			continue
		}
		coverage.reasons[reason]++
		coverage.gaps = append(coverage.gaps, slot.label()+"["+reason+"]")
	}
	return coverage
}

func (self fleetSlotCoverage) full() bool {
	return len(self.gaps) == 0 && 0 < self.expected
}

// Bounded slot gap listing: at most limit names, then a count of the rest.
func (self fleetSlotCoverage) listed(limit int) string {
	if len(self.gaps) == 0 {
		return "none"
	}
	if len(self.gaps) <= limit {
		return strings.Join(self.gaps, ",")
	}
	return strings.Join(self.gaps[:limit], ",") + fmt.Sprintf(",and %d more", len(self.gaps)-limit)
}

func (self fleetSlotCoverage) observed(limit int) string {
	reasons := make([]string, 0, len(self.reasons))
	for reason, count := range self.reasons {
		reasons = append(reasons, fmt.Sprintf("%s=%d", reason, count))
	}
	sort.Strings(reasons)
	if len(reasons) == 0 {
		reasons = append(reasons, "none")
	}
	return fmt.Sprintf("expected_slots=%d complete_slots=%d gap_reasons=%s gaps=%s",
		self.expected, self.complete, strings.Join(reasons, ","), self.listed(limit))
}
