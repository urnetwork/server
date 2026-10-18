// Raw Redis exporter history supplies the ten-minute arm of §3.5 without
// changing the monitor's active cadence or persisting alert state.
package monitor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	redisClientsHistoryMarker = "monitor-redis-clients-history"
	redisClientsHistoryWindow = 10 * time.Minute
	// Reuse the Redis exporter's existing freshness allowance. The extra
	// history only locates a coverage anchor; growth pairs stay within ten minutes.
	redisClientsHistoryLookback = redisClientsHistoryWindow + 2*redisRatesFreshness
	redisClientsHistoryMaxBytes = 1024 * 1024
)

// Only configured host/port pairs are observation targets. Run ids stay in
// memory and are never rendered into alerts.
type redisClientsHistoryTarget struct {
	host *host
	port int
}

// Every frame joins all four raw metric families at the original timestamp.
type redisClientsHistoryFrame struct {
	at      time.Time
	clients float64
	uptime  float64
	runId   string
	up      float64
	fields  uint8
}

// A reason is a fixed, privacy-safe unknown state. An empty reason permits
// evaluating the original +50% threshold; no local prior verdict is reused.
type redisClientsHistoryObservation struct {
	reason   string
	baseline redisClientsHistoryFrame
	latest   redisClientsHistoryFrame
	spike    bool
}

// This matrix is returned by an instant query containing a raw range selector,
// not query_range's resampled evaluation grid.
type redisClientsHistoryResponse struct {
	Status   string            `json:"status"`
	Warnings []json.RawMessage `json:"warnings"`
	Data     struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// Select only the intended inventory. The parser independently checks each
// host/port pair, since the two regular expressions also admit cross-pairs.
func redisClientsHistoryQuery(environment string, targets map[string]redisClientsHistoryTarget) string {
	hostNames := map[string]bool{}
	portNames := map[string]bool{}
	for _, target := range targets {
		hostNames[regexp.QuoteMeta(target.host.name)] = true
		portNames[strconv.Itoa(target.port)] = true
	}
	hosts := make([]string, 0, len(hostNames))
	ports := make([]string, 0, len(portNames))
	for name := range hostNames {
		hosts = append(hosts, name)
	}
	for port := range portNames {
		ports = append(ports, port)
	}
	sort.Strings(hosts)
	sort.Strings(ports)
	return `{__name__=~"redis_(connected_clients|uptime_in_seconds|instance_info|up)",env=` +
		strconv.Quote(environment) + `,job="redis",host=~` + strconv.Quote(strings.Join(hosts, "|")) +
		`,node=~` + strconv.Quote(strings.Join(ports, "|")) + `}[` + strconv.Itoa(int(redisClientsHistoryLookback/time.Second)) + `s]`
}

// One bounded query reads existing Mimir history through the established
// service gateway. Missing transport or schema authority remains unknown.
func observeRedisClientsHistory(ctx context.Context, env *probeEnv, targets map[string]redisClientsHistoryTarget, at time.Time) (map[string]redisClientsHistoryObservation, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("no service gateway for Redis client history")
	}
	command := "# " + redisClientsHistoryMarker + "\n" +
		"curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(redisClientsHistoryMaxBytes) +
		" --data-urlencode " + shellSingleQuote("query="+redisClientsHistoryQuery(env.cfg.env, targets)) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(at.Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, _, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command)
	if err != nil {
		return nil, fmt.Errorf("Redis client history query unavailable")
	}
	return parseRedisClientsHistory(out, env.cfg.env, targets, at)
}

// Validate the full bounded response before judging any node. Node-local
// omissions do not erase a complete sibling; foreign labels or partial-query
// warnings invalidate the response without exposing source labels or errors.
func parseRedisClientsHistory(output, environment string, targets map[string]redisClientsHistoryTarget, at time.Time) (map[string]redisClientsHistoryObservation, error) {
	if len(output) > redisClientsHistoryMaxBytes {
		return nil, fmt.Errorf("Redis client history response exceeds byte bound")
	}
	var response redisClientsHistoryResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil || response.Status != "success" ||
		response.Data.ResultType != "matrix" || len(response.Warnings) != 0 {
		return nil, fmt.Errorf("Redis client history response is incomplete or invalid")
	}
	if len(response.Data.Result) == 0 {
		return nil, fmt.Errorf("Redis client history has no configured-node series")
	}
	frames := make(map[string]map[int64]redisClientsHistoryFrame, len(targets))
	reasons := map[string]string{}
	for _, series := range response.Data.Result {
		labels := series.Metric
		port, err := strconv.Atoi(labels["node"])
		target := labels["host"] + ":" + strconv.Itoa(port)
		_, owned := targets[target]
		if err != nil || labels["node"] != strconv.Itoa(port) || !owned ||
			labels["env"] != environment || labels["job"] != "redis" {
			return nil, fmt.Errorf("Redis client history has unowned series")
		}
		var field uint8
		switch labels["__name__"] {
		case "redis_connected_clients":
			field = 1
		case "redis_uptime_in_seconds":
			field = 2
		case "redis_instance_info":
			field = 4
			_, runErr := hex.DecodeString(labels["run_id"])
			if runErr != nil || len(labels["run_id"]) != 40 || labels["tcp_port"] != labels["node"] {
				reasons[target] = "invalid-generation"
			}
		case "redis_up":
			field = 8
		default:
			return nil, fmt.Errorf("Redis client history has an unexpected metric")
		}
		if frames[target] == nil {
			frames[target] = map[int64]redisClientsHistoryFrame{}
		}
		var previousMillis int64
		for _, pair := range series.Values {
			var timestamp float64
			var valueText string
			if len(pair) != 2 || json.Unmarshal(pair[0], &timestamp) != nil || json.Unmarshal(pair[1], &valueText) != nil ||
				math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp <= 0 ||
				timestamp > float64(at.UnixMilli())/1000 || timestamp <= float64(at.Add(-redisClientsHistoryLookback).UnixMilli())/1000 {
				reasons[target] = "invalid-source-time"
				continue
			}
			millis := int64(math.Round(timestamp * 1000))
			if timestamp != float64(millis)/1000 {
				reasons[target] = "invalid-source-time"
				continue
			}
			if previousMillis >= millis {
				reasons[target] = "unordered-or-duplicate-samples"
			}
			previousMillis = millis
			value, err := strconv.ParseFloat(valueText, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.Trunc(value) != value || value > 9007199254740991 {
				reasons[target] = "invalid-value"
				continue
			}
			frame := frames[target][millis]
			if frame.fields&field != 0 {
				// Even equal duplicate values have ambiguous source ownership.
				reasons[target] = "ambiguous-samples"
			}
			frame.at = time.UnixMilli(millis)
			frame.fields |= field
			switch field {
			case 1:
				frame.clients = value
			case 2:
				frame.uptime = value
			case 4:
				frame.runId = labels["run_id"]
				if value != 1 {
					reasons[target] = "invalid-generation"
				}
			case 8:
				frame.up = value
			}
			frames[target][millis] = frame
		}
	}
	observations := make(map[string]redisClientsHistoryObservation, len(targets))
	for target := range targets {
		if reason := reasons[target]; reason != "" {
			observations[target] = redisClientsHistoryObservation{reason: reason}
			continue
		}
		samples := make([]redisClientsHistoryFrame, 0, len(frames[target]))
		for _, frame := range frames[target] {
			samples = append(samples, frame)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })
		observations[target] = evaluateRedisClientsHistory(samples, at)
	}
	return observations, nil
}

// Compare source-clocked window endpoints, not an interior minimum. A preceding
// anchor and bounded gaps establish coverage. If the left boundary falls
// between samples, its two neighbors must agree on the threshold. Redis run_id
// is the generation authority; uptime adds warmup/reset controls rather than
// an inferred boot-time identity.
func evaluateRedisClientsHistory(samples []redisClientsHistoryFrame, at time.Time) redisClientsHistoryObservation {
	observation := redisClientsHistoryObservation{reason: "missing-history"}
	if len(samples) == 0 {
		return observation
	}
	latest := samples[len(samples)-1]
	if latest.fields != 15 || latest.up != 1 {
		observation.reason = "incomplete-latest-sample"
		return observation
	}
	if at.Sub(latest.at) > redisRatesFreshness {
		observation.reason = "stale-history"
		return observation
	}
	observation.latest = latest
	if latest.uptime <= redisClientsHistoryWindow.Seconds() {
		observation.reason = "generation-warmup"
		return observation
	}
	cutoff := latest.at.Add(-redisClientsHistoryWindow)
	anchor := -1
	for index, sample := range samples {
		if !sample.at.After(cutoff) {
			anchor = index
		}
	}
	if anchor == -1 {
		observation.reason = "history-warmup"
		return observation
	}
	for index := anchor; index < len(samples); index++ {
		sample := samples[index]
		if sample.fields != 15 || sample.up != 1 {
			observation.reason = "incomplete-history"
			return observation
		}
		if sample.runId != latest.runId {
			observation.reason = "generation-change"
			return observation
		}
		if index > anchor {
			previous := samples[index-1]
			if sample.at.Sub(previous.at) > redisRatesFreshness {
				observation.reason = "history-gap"
				return observation
			}
			if sample.uptime < previous.uptime {
				observation.reason = "uptime-reset"
				return observation
			}
		}
	}
	left := samples[anchor]
	observation.baseline = left
	if left.at.Before(cutoff) {
		observation.baseline = samples[anchor+1]
	}
	if !observation.baseline.at.Before(latest.at) {
		observation.reason = "missing-baseline"
		return observation
	}
	if left.clients <= 0 || observation.baseline.clients <= 0 {
		observation.reason = "zero-baseline"
		return observation
	}
	// Parsed counts are exact nonnegative integers below 2^53. Integer
	// cross-products preserve the inclusive 50% boundary without rounding.
	growth := func(baseline float64) bool { return 2*uint64(latest.clients) >= 3*uint64(baseline) }
	observation.spike = growth(observation.baseline.clients)
	if left.at.Before(cutoff) && observation.spike != growth(left.clients) {
		observation.spike = false
		observation.reason = "ambiguous-window-boundary"
		return observation
	}
	observation.reason = ""
	return observation
}

// Unknown source coverage has a stable identity separate from the measured
// clients-spike symptom and never resolves that symptom by silence.
func redisClientsHistoryUnknown(target, reason string) finding {
	return finding{
		probeId: "redis/clients-spike", tier: tierWarn, class: "cannot-observe", target: target,
		frame: "clients-history", sustain: 2,
		symptom:   "The ten-minute Redis connected-client growth condition is unobservable for " + target,
		mechanism: "The historical source must join connected clients, Redis uptime, availability, and run_id at each original sample timestamp. A current fleet median cannot establish this temporal condition.",
		baseline:  "Complete same-generation Redis exporter samples over ten minutes, with source age and sample gaps at most three minutes.",
		observed:  "history_state=" + reason + " growth_window_seconds=600",
		context:   "Current fleet-median violations remain reportable independently. A missing, partial, stale, ambiguous, or restarting source is unknown; a fifteen-minute native sample interval is not a ten-minute comparison.",
		action:    "Restore the bounded Mimir history and exact configured-node labels from the Redis exporter. After a restart, wait for a complete same-generation window. Keep the configured monitor cadence; do not fabricate samples or persist sustain state.",
		verify:    "The same node returns a complete fresh source window with coherent run_id and uptime, then the actual ten-minute growth comparison is evaluated on consecutive probes.",
		playbook:  "SIGNALS.md §3.5",
	}
}

// Shared transport/schema/empty-response failures cover the complete inventory.
// The unobservable class keeps prior node incidents open through the ordinary
// signal execution contract, without one duplicate incident per dependent node.
func redisClientsHistorySourceUnknown(configuredNodes int) finding {
	unknown := redisClientsHistoryUnknown("redis-clients-history", "source-unavailable")
	unknown.symptom = "The ten-minute Redis connected-client growth condition is unobservable for all configured Redis nodes"
	unknown.observed = fmt.Sprintf("history_state=source-unavailable configured_nodes=%d unobservable_nodes=%d growth_window_seconds=600", configuredNodes, configuredNodes)
	unknown.context += " This single shared-source failure covers every configured node; it cannot clear an existing node spike or establish a healthy temporal comparison. Node-local gaps in an otherwise usable response retain their own node identities."
	unknown.verify = "The shared source returns a complete fresh window for every configured node with coherent run_id and uptime. Existing node incidents require actual healthy comparisons on consecutive complete probes before clearing."
	return unknown
}
