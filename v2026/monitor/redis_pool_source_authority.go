// Keeps the existing pool distress bands behind desired-slot and discrete
// source-history authority. Current-pool accounting never certifies recovery.
package monitor

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Existing calibrated bands and timing defaults are owned settings. All
// durations must be positive whole seconds, except the nonnegative future skew.
type RedisPoolSettings struct {
	// Schedule after a completed evaluation; nonpositive is invalid.
	Cadence time.Duration
	// Rate lookback and floor duration/step; nonpositive or fractional is invalid.
	Window, Hold, HoldStep time.Duration
	// Maximum original source age and permitted future skew at each step.
	Freshness, FutureSkew time.Duration
	// Existing bounded HTTP intake and query duration; nonpositive is invalid.
	QueryTimeout time.Duration
	// Response byte and row bounds; nonpositive is invalid.
	MaxResponseBytes, MaxRows int
	// Existing warning and page bands, in reported accounting units per second.
	TimeoutsWarnPerSecond, TimeoutsPagePerSecond float64
	MissesWarnPerSecond, MissesPagePerSecond     float64
}

// Preserve the upstream calibration, cadence and intake policy unchanged.
func DefaultRedisPoolSettings() *RedisPoolSettings {
	return &RedisPoolSettings{Cadence: 5 * time.Minute, Window: 5 * time.Minute, Hold: 10 * time.Minute, HoldStep: time.Minute,
		Freshness: 5 * time.Minute, FutureSkew: 30 * time.Second, QueryTimeout: 15 * time.Second,
		MaxResponseBytes: mimirShellResponseMaxBytes, MaxRows: DefaultFleetReadSettings().MaxRows,
		TimeoutsWarnPerSecond: 50, TimeoutsPagePerSecond: 1000, MissesWarnPerSecond: 1000, MissesPagePerSecond: 10000}
}

// Invalid local policy cannot contact a source or become healthy evidence.
func (self *RedisPoolSettings) valid() bool {
	if self == nil || self.MaxRows <= 0 || self.MaxResponseBytes <= 0 {
		return false
	}
	for _, duration := range []time.Duration{self.Cadence, self.Window, self.Hold, self.HoldStep, self.Freshness, self.QueryTimeout} {
		if duration <= 0 || duration%time.Second != 0 {
			return false
		}
	}
	if self.FutureSkew < 0 || self.HoldStep > self.Hold {
		return false
	}
	for _, value := range []float64{self.TimeoutsWarnPerSecond, self.TimeoutsPagePerSecond, self.MissesWarnPerSecond, self.MissesPagePerSecond} {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return self.TimeoutsWarnPerSecond < self.TimeoutsPagePerSecond && self.MissesWarnPerSecond < self.MissesPagePerSecond
}

// Whole-minute defaults keep the existing PromQL request spelling.
func redisPoolDuration(duration time.Duration) string {
	if duration%time.Minute == 0 {
		return strconv.FormatInt(int64(duration/time.Minute), 10) + "m"
	}
	return strconv.FormatInt(int64(duration/time.Second), 10) + "s"
}

// Only code-owned pool-producing services in the active placement can create
// public targets. An entirely absent returned service still remains expected.
func redisPoolInventory(env *probeEnv) fleetSlotInventory {
	services := []string{}
	for _, active := range env.cfg.logServices {
		switch active {
		case "api", "connect", "proxy", "taskworker", "gossip", "mcp", "alt":
			services = append(services, active)
		}
	}
	return newFleetSlotInventory(env, services, nil)
}

// Supplements rates with original source clocks and per-process discrete
// hold coverage. A derived evaluation timestamp is never scrape freshness.
func redisPoolAuthorityQuery(selector string, settings *RedisPoolSettings) string {
	hold := redisPoolDuration(settings.Hold) + ":" + redisPoolDuration(settings.HoldStep)
	span := redisPoolDuration(settings.Hold + settings.Window)
	queryParts := []string{`label_replace(count_over_time(vector(1)[` + hold + `]),"monitor_metric","source/expected","__name__",".*")`}
	add := func(part, expression string) {
		queryParts = append(queryParts, fmt.Sprintf(`label_replace(%s,"monitor_metric",%s,"__name__",".*")`, expression, strconv.Quote("source/"+part)))
	}
	for _, counter := range []string{"timeouts", "misses"} {
		metric := "urnetwork_redis_pool_" + counter + "_total{" + selector + "}"
		rate := "rate(" + metric + "[" + redisPoolDuration(settings.Window) + "])"
		add(counter+"/points", "count_over_time(("+rate+")["+hold+"])")
		add(counter+"/clock_points", "count_over_time((timestamp("+metric+"))["+hold+"])")
		add(counter+"/max_age", "max_over_time((time()-timestamp("+metric+"))["+hold+"])")
		add(counter+"/min_age", "min_over_time((time()-timestamp("+metric+"))["+hold+"])")
		add(counter+"/resets", "resets("+metric+"["+span+"])")
		add(counter+"/prior_clock", "timestamp("+metric+" offset "+span+")")
	}
	start := "process_start_time_seconds{" + selector + "}"
	add("start", start)
	add("prior_start", start+" offset "+span)
	add("start_clock", "timestamp("+start+")")
	add("prior_start_clock", "timestamp("+start+" offset "+span+")")
	return strings.Join(queryParts, " or ")
}

// One private process tuple. Neither host labels nor process nonces become
// coverage-error text, and duplicate fields cannot overwrite valid authority.
type redisPoolProcessAuthority struct {
	slot       string
	partValues map[string]float64
	invalid    bool
}

var redisPoolProcessNonce = regexp.MustCompile(`^[0-9a-f]{8}$`)

// Removes proof rows before the existing rate reducer and returns qualified
// service membership. Missing whole services, sparse holds, reset samples,
// stale originals and foreign rows are unknown; qualified siblings survive.
func qualifyRedisPoolSources(response mimirInstantResponse, inventory fleetSlotInventory, environment string, at time.Time, settings *RedisPoolSettings) (mimirInstantResponse, map[string]bool, string) {
	filtered := response
	filtered.Data.Result = nil
	expectedSlotServices := map[string]string{}
	knownServices := map[string]bool{}
	serviceQualified := map[string]bool{}
	reason := ""
	for _, slot := range inventory.slots {
		knownServices[slot.service] = true
		if _, found := serviceQualified[slot.service]; !found {
			serviceQualified[slot.service] = true
		}
		key := slot.service + "\x00" + slot.host + "\x00" + slot.block
		expectedSlotServices[key] = slot.service
		if !slot.allowed {
			serviceQualified[slot.service] = false
			reason = "source_coverage_incomplete"
		}
	}
	for _, service := range inventory.missingServices {
		knownServices[service] = true
		serviceQualified[service] = false
		reason = "source_coverage_incomplete"
	}
	if len(knownServices) == 0 {
		reason = "source_coverage_incomplete"
	}
	expectedPartBools := map[string]bool{"source/start": true, "source/prior_start": true, "source/start_clock": true, "source/prior_start_clock": true}
	for _, counter := range []string{"timeouts", "misses"} {
		for _, part := range []string{"points", "clock_points", "max_age", "min_age", "resets", "prior_clock"} {
			expectedPartBools["source/"+counter+"/"+part] = true
		}
	}
	derivedServicePartBools := map[string]map[string]bool{}
	processKeyAuthorities := map[string]*redisPoolProcessAuthority{}
	expected, expectedSeen := float64(0), false
	expectedMinimum := float64(settings.Hold / settings.HoldStep)
	expectedValid := true
	for _, series := range response.Data.Result {
		part := series.Metric["monitor_metric"]
		if part == "source/expected" {
			observed, value, err := mimirInstantValue(series.Value)
			if expectedSeen || err != nil || !observed.Equal(at) || value < expectedMinimum || expectedMinimum+1 < value || value != math.Trunc(value) {
				expectedValid = false
			}
			expected, expectedSeen = value, true
			continue
		}
		service := series.Metric["service"]
		if !knownServices[service] {
			reason = "source_coverage_incomplete"
			continue
		}
		if !strings.HasPrefix(part, "source/") {
			switch part {
			case "timeouts_floor", "timeouts_now", "misses_floor", "misses_now":
				if derivedServicePartBools[service] == nil {
					derivedServicePartBools[service] = map[string]bool{}
				}
				derivedServicePartBools[service][part] = true
			}
			if strings.HasPrefix(part, "host_") {
				knownHost := false
				for _, slot := range inventory.slots {
					if slot.allowed && slot.service == service && slot.host == series.Metric["host"] {
						knownHost = true
					}
				}
				if !knownHost {
					serviceQualified[service] = false
					reason = "source_coverage_incomplete"
					continue
				}
			}
			filtered.Data.Result = append(filtered.Data.Result, series)
			continue
		}
		key := service + "\x00" + series.Metric["host"] + "\x00" + series.Metric["block"]
		_, known := expectedSlotServices[key]
		nonce := series.Metric["instance"]
		if !known || series.Metric["env"] != environment || series.Metric["job"] != service || !redisPoolProcessNonce.MatchString(nonce) {
			serviceQualified[service] = false
			reason = "source_coverage_incomplete"
			continue
		}
		processKey := key + "\x00" + nonce
		observation := processKeyAuthorities[processKey]
		if observation == nil {
			observation = &redisPoolProcessAuthority{slot: key, partValues: map[string]float64{}}
			processKeyAuthorities[processKey] = observation
		}
		observed, value, err := mimirInstantValue(series.Value)
		_, duplicate := observation.partValues[part]
		if !expectedPartBools[part] || duplicate || err != nil || !observed.Equal(at) || math.IsNaN(value) || math.IsInf(value, 0) {
			observation.invalid = true
		}
		observation.partValues[part] = value
	}
	coveredSlotCounts := map[string]int{}
	span := (settings.Window + settings.Hold).Seconds()
	for _, observation := range processKeyAuthorities {
		values := observation.partValues
		valid := !observation.invalid && expectedValid && expectedSeen && len(values) == len(expectedPartBools)
		for part := range expectedPartBools {
			if _, present := values[part]; !present {
				valid = false
			}
		}
		for _, counter := range []string{"timeouts", "misses"} {
			prefix := "source/" + counter + "/"
			valid = valid && values[prefix+"points"] == expected && values[prefix+"clock_points"] == expected && values[prefix+"resets"] == 0
			valid = valid && values[prefix+"max_age"] <= settings.Freshness.Seconds() && values[prefix+"max_age"] >= values[prefix+"min_age"] && values[prefix+"min_age"] >= -settings.FutureSkew.Seconds()
			clock, present := values[prefix+"prior_clock"]
			valid = valid && present && float64(at.Unix())-span-settings.Freshness.Seconds() <= clock && clock <= float64(at.Unix())-span+settings.FutureSkew.Seconds()
		}
		start, present := values["source/start"]
		valid = valid && present && 0 < start && start == values["source/prior_start"] && start <= float64(at.Unix())-span
		for _, endpoint := range []struct {
			part   string
			offset float64
		}{{part: "source/start_clock", offset: 0}, {part: "source/prior_start_clock", offset: span}} {
			clock, present := values[endpoint.part]
			valid = valid && present && float64(at.Unix())-endpoint.offset-settings.Freshness.Seconds() <= clock && clock <= float64(at.Unix())-endpoint.offset+settings.FutureSkew.Seconds()
		}
		service := expectedSlotServices[observation.slot]
		if !valid {
			serviceQualified[service] = false
			reason = "source_coverage_incomplete"
		} else {
			coveredSlotCounts[observation.slot]++
		}
	}
	for slot, service := range expectedSlotServices {
		if coveredSlotCounts[slot] != 1 {
			serviceQualified[service] = false
			reason = "source_coverage_incomplete"
		}
	}
	// Proof rows cannot substitute for a desired service's four derived rates.
	// Encoding and multiplicity remain the existing rate parser's responsibility.
	for service := range serviceQualified {
		if len(derivedServicePartBools[service]) != 4 {
			serviceQualified[service] = false
			reason = "source_coverage_incomplete"
		}
	}
	return filtered, serviceQualified, reason
}

// Resettable current-pool totals have no full interval-health witness, even
// with complete discrete source clocks. Silence cannot clear either band.
func redisPoolPressureHolds(environment string, services map[string]bool) []finding {
	findings := []finding{}
	for service := range services {
		for _, class := range []string{"redis-pool-distress", "redis-pool-distress-critical"} {
			findings = append(findings, finding{probeId: "mimir/redis-pool", class: class, target: environment + "/" + service, clearHold: true})
		}
	}
	return findings
}
