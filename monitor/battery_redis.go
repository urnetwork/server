// Optional Redis batteries project remote data into a bounded numeric schema
// before caching or public rendering. Missing diagnostics never clear a native
// memory or connection-count violation.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Require an actual nonnegative integer; source text is never an output value.
func redisBatteryInteger(raw string) (int64, bool) {
	if raw == "" || len(raw) > 19 {
		return 0, false
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil
}

// Every field must have one known owner. Unknown and duplicate keys invalidate
// the projection instead of being scanned for the first usable-looking token.
func redisBatteryFields(raw string, keys []string) (map[string]string, bool) {
	if len(raw) > 16*1024 {
		return nil, false
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	values := make(map[string]string, len(keys))
	for _, field := range strings.Fields(raw) {
		key, value, found := strings.Cut(field, "=")
		if !found || !allowed[key] || value == "" {
			return nil, false
		}
		if _, duplicate := values[key]; duplicate {
			return nil, false
		}
		values[key] = value
	}
	return values, len(values) == len(keys)
}

// INFO contains legitimate extra fields, including human-readable strings.
// Only the complete selected numeric fields become public memory evidence.
func redisNodeMemoryEvidence(raw string, port int) (string, bool) {
	keys := []string{"used_memory", "maxmemory", "used_memory_dataset", "mem_clients_normal", "mem_clients_slaves", "mem_fragmentation_ratio"}
	selected := make(map[string]bool, len(keys))
	for _, key := range keys {
		selected[key] = true
	}
	values := map[string]string{}
	header := false
	if len(raw) > 1024*1024 {
		return "", false
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if line == "# Memory" && !header {
			header = true
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found || key == "" {
			return "", false
		}
		if selected[key] {
			if _, duplicate := values[key]; duplicate {
				return "", false
			}
			values[key] = value
		}
	}
	if !header || len(values) != len(keys) {
		return "", false
	}
	numbers := map[string]int64{}
	for _, key := range keys[:len(keys)-1] {
		value, valid := redisBatteryInteger(values[key])
		if !valid {
			return "", false
		}
		numbers[key] = value
	}
	fragmentation, err := strconv.ParseFloat(values["mem_fragmentation_ratio"], 64)
	if err != nil || math.IsNaN(fragmentation) || math.IsInf(fragmentation, 0) || fragmentation < 0 ||
		numbers["mem_clients_normal"] > math.MaxInt64-numbers["mem_clients_slaves"] {
		return "", false
	}
	return fmt.Sprintf("node %d memory: used_bytes=%d maxmemory_bytes=%d dataset_bytes=%d client_bytes=%d fragmentation_ratio=%s",
		port, numbers["used_memory"], numbers["maxmemory"], numbers["used_memory_dataset"],
		numbers["mem_clients_normal"]+numbers["mem_clients_slaves"], strconv.FormatFloat(fragmentation, 'g', -1, 64)), true
}

// CLIENT metadata stays on the remote host. Required server-owned counters
// must be unique integers; arbitrary names, users, addresses and libraries are
// never printed. The exact-integer limit also bounds awk's numeric arithmetic.
const redisBatteryClientsAwk = `
function number(value) { return value ~ /^[0-9]+$/ && value+0 <= 9007199254740991 }
BEGIN { count=0; total=0; maximum=0; hget=0; sadd=0; expire=0; over=0; maxIdle=0; maxAge=0; expireIdle=0 }
{
  delete seen
  omem=0; idle=0; age=0; cmd=""
  for (i=1; i<=NF; i++) {
    valueCount=split($i, value, "=")
    key=value[1]
    if (key=="omem" || key=="idle" || key=="age" || key=="cmd") {
      if (seen[key]++ || valueCount!=2 || value[2]=="") invalid=1
      if (key!="cmd" && !number(value[2])) invalid=1
      if (key=="omem") omem=value[2]+0
      if (key=="idle") idle=value[2]+0
      if (key=="age") age=value[2]+0
      if (key=="cmd") cmd=value[2]
    }
  }
  if (!seen["omem"] || !seen["idle"] || !seen["age"] || !seen["cmd"] || idle>age) invalid=1
  count++; total+=omem
  if (count>9007199254740991 || total>9007199254740991) invalid=1
  if (maximum<omem) maximum=omem
  if (omem>1048576) over++
  if (maxIdle<idle) maxIdle=idle
  if (maxAge<age) maxAge=age
  if (cmd=="hget") hget++
  if (cmd=="sadd") sadd++
  if (cmd=="expire") { expire++; if (expireIdle<idle) expireIdle=idle }
}
END {
  if (invalid || count==0) exit 1
  printf "client_list_total=%.0f client_cmd_hget_count=%.0f client_cmd_sadd_count=%.0f client_cmd_expire_count=%.0f client_output_memory_bytes=%.0f client_output_memory_max_bytes=%.0f client_output_over_1mb_count=%.0f client_max_idle_s=%.0f client_max_age_s=%.0f max_expire_idle_s=%.0f\n", count,hget,sadd,expire,total,maximum,over,maxIdle,maxAge,expireIdle
}`

// A successful ss command must actually contain the selected listener. Missing
// or malformed queues are unknown, including when a trailing pipeline succeeds.
const redisBatteryQueueAwk = `
$4 ~ (":" port "$") {
  if (NF<5 || $1!="LISTEN" || $2 !~ /^[0-9]+$/ || $3 !~ /^[0-9]+$/ || $2+0>9007199254740991 || $3+0>9007199254740991) invalid=1
  found++
  if (maxRecv<$2+0) maxRecv=$2+0
  if (maxSend<$3+0) maxSend=$3+0
}
END {
  if (invalid || !found) exit 1
  printf "accept_recv_q=%.0f accept_send_q=%.0f\n",maxRecv,maxSend
}`

// Both batteries consume the same finite client schema and consistency checks.
var redisBatteryClientKeys = []string{
	"client_list_total", "client_cmd_hget_count", "client_cmd_sadd_count", "client_cmd_expire_count",
	"client_output_memory_bytes", "client_output_memory_max_bytes", "client_output_over_1mb_count",
	"client_max_idle_s", "client_max_age_s", "max_expire_idle_s",
}

// The exact-count relationships prevent malformed aggregates acquiring authority.
func redisBatteryClientValues(values map[string]int64) bool {
	total := values["client_list_total"]
	return total > 0 && values["client_cmd_hget_count"] <= total && values["client_cmd_sadd_count"] <= total-values["client_cmd_hget_count"] &&
		values["client_cmd_expire_count"] <= total-values["client_cmd_hget_count"]-values["client_cmd_sadd_count"] &&
		values["client_output_over_1mb_count"] <= total && values["client_output_memory_max_bytes"] <= values["client_output_memory_bytes"] &&
		values["client_max_idle_s"] <= values["client_max_age_s"] && values["max_expire_idle_s"] <= values["client_max_idle_s"] &&
		(values["client_cmd_expire_count"] > 0 || values["max_expire_idle_s"] == 0)
}

// Project one complete integer-only diagnostic, discarding all malformed output.
func redisBatteryIntegerEvidence(raw string, keys []string) (string, bool) {
	fields, valid := redisBatteryFields(raw, keys)
	if !valid {
		return "", false
	}
	values := make(map[string]int64, len(keys))
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value, valid := redisBatteryInteger(fields[key])
		if !valid {
			return "", false
		}
		values[key] = value
		parts = append(parts, key+"="+strconv.FormatInt(value, 10))
	}
	if _, clients := fields["client_list_total"]; clients && !redisBatteryClientValues(values) {
		return "", false
	}
	return strings.Join(parts, " "), true
}

// Each optional source has a fixed error class. A failed client or queue read
// cannot become a false zero, and no raw source byte enters the trip cache.
func redisNodeBattery(ctx context.Context, env *probeEnv, h *host, port int) string {
	if h == nil {
		return ""
	}
	parts := []string{}
	info, err := env.runner.redis(ctx, h, port, "INFO", "memory")
	if err != nil {
		parts = append(parts, fmt.Sprintf("node %d INFO memory unavailable: error_class=%s", port, classifyObservationError(err)))
	} else if evidence, valid := redisNodeMemoryEvidence(info, port); valid {
		parts = append(parts, evidence)
	} else {
		parts = append(parts, fmt.Sprintf("node %d INFO memory unavailable: error_class=%s", port, observationErrorClassInvalidResponse))
	}
	clientCommand := fmt.Sprintf("client_list=$(redis-cli --raw -p %d CLIENT LIST 2>/dev/null) || exit 1\nprintf '%%s\\n' \"$client_list\" | awk '", port) + redisBatteryClientsAwk + "'"
	queueCommand := "queues=$(ss -lnt 2>/dev/null) || exit 1\nprintf '%s\\n' \"$queues\" | awk -v port=" + strconv.Itoa(port) + " '" + redisBatteryQueueAwk + "'"
	for _, diagnostic := range []struct {
		name    string
		command string
		keys    []string
	}{
		{name: "client buffers", command: clientCommand, keys: redisBatteryClientKeys},
		{name: "accept queue", command: queueCommand, keys: []string{"accept_recv_q", "accept_send_q"}},
	} {
		out, err := env.runner.shell(ctx, h, diagnostic.command)
		if err != nil {
			parts = append(parts, fmt.Sprintf("node %d %s unavailable: error_class=%s", port, diagnostic.name, classifyObservationError(err)))
		} else if evidence, valid := redisBatteryIntegerEvidence(out, diagnostic.keys); valid {
			parts = append(parts, fmt.Sprintf("node %d %s: %s", port, diagnostic.name, evidence))
		} else {
			parts = append(parts, fmt.Sprintf("node %d %s unavailable: error_class=%s", port, diagnostic.name, observationErrorClassInvalidResponse))
		}
	}
	return strings.Join(parts, "\n")
}

// Every connection field is mandatory. A partial optional battery remains
// unavailable; it can neither choose a workload repair nor clear pressure.
var redisConnectionIntegerKeys = append([]string{
	"redis_connection_diagnostics_version", "queried_node_port", "reliability_marker_slot", "reliability_marker_owner_port",
	"reliability_stats_current_block", "current_reliability_shards_on_node", "previous_reliability_shards_on_node",
	"reliability_shard_count", "reliability_shard_lookback_blocks", "reliability_shards_recent_max",
	"reliability_shards_recent_max_age_blocks", "reliability_shards_recent_total", "blocked_clients",
	"used_memory_bytes", "mem_clients_normal_bytes", "hget_nominal_sample_seconds", "hget_calls_delta", "accept_recv_q", "accept_send_q",
}, redisBatteryClientKeys...)

// Strings carry only the canonical numeric representation, never source text.
type redisConnectionDiagnostics struct {
	values          map[string]int64
	latency         float64
	hgetNominalRate float64 // encoded delta/2, without elapsed or generation authority
}

// The complete schema is also the attribution boundary, including cached
// evidence. The latch's exact generated suffix is the sole permitted prose.
func parseRedisConnectionDiagnostics(raw string) (redisConnectionDiagnostics, bool) {
	if data, suffix, cached := strings.Cut(raw, "\n(battery collected once at trip "); cached {
		stamp, valid := strings.CutSuffix(suffix, "; re-runs after a healthy tick re-arms)")
		if _, err := time.Parse(time.RFC3339, stamp); !valid || err != nil {
			return redisConnectionDiagnostics{}, false
		}
		raw = data
	}
	keys := append(append([]string(nil), redisConnectionIntegerKeys...), "latency_avg_ms", "hget_calls_per_nominal_second")
	fields, valid := redisBatteryFields(raw, keys)
	if !valid {
		return redisConnectionDiagnostics{}, false
	}
	values := make(map[string]int64, len(redisConnectionIntegerKeys))
	for _, key := range redisConnectionIntegerKeys {
		value, valid := redisBatteryInteger(fields[key])
		if !valid {
			return redisConnectionDiagnostics{}, false
		}
		values[key] = value
	}
	latency, latencyErr := strconv.ParseFloat(fields["latency_avg_ms"], 64)
	// Rate equality below checks encoding only. The source has no measured
	// monotonic INFO-to-INFO interval or same-generation observation.
	rate, rateErr := strconv.ParseFloat(fields["hget_calls_per_nominal_second"], 64)
	if latencyErr != nil || rateErr != nil || math.IsNaN(latency) || math.IsInf(latency, 0) || latency < 0 ||
		math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || values["redis_connection_diagnostics_version"] != 1 ||
		values["queried_node_port"] == 0 || values["queried_node_port"] > 65535 ||
		values["reliability_marker_slot"] > 16383 || values["reliability_marker_owner_port"] == 0 || values["reliability_marker_owner_port"] > 65535 ||
		values["reliability_stats_current_block"] < 62 || values["reliability_shard_count"] != 32 ||
		values["reliability_shard_lookback_blocks"] < 2 || values["reliability_shard_lookback_blocks"] > 62 ||
		values["current_reliability_shards_on_node"] > 32 || values["previous_reliability_shards_on_node"] > 32 ||
		values["reliability_shards_recent_max"] > 32 || values["reliability_shards_recent_max"] < values["current_reliability_shards_on_node"] ||
		values["reliability_shards_recent_max"] < values["previous_reliability_shards_on_node"] ||
		values["reliability_shards_recent_max_age_blocks"] >= values["reliability_shard_lookback_blocks"] ||
		values["reliability_shards_recent_total"] < values["reliability_shards_recent_max"] ||
		values["reliability_shards_recent_total"] < values["current_reliability_shards_on_node"]+values["previous_reliability_shards_on_node"] ||
		values["reliability_shards_recent_total"] > values["reliability_shards_recent_max"]*values["reliability_shard_lookback_blocks"] ||
		values["reliability_shard_lookback_blocks"] != min(int64(62), values["max_expire_idle_s"]/60+2) ||
		(values["reliability_shards_recent_max_age_blocks"] == 0 && values["reliability_shards_recent_max"] != values["current_reliability_shards_on_node"]) ||
		(values["reliability_shards_recent_max_age_blocks"] == 1 && values["reliability_shards_recent_max"] != values["previous_reliability_shards_on_node"]) ||
		values["hget_nominal_sample_seconds"] != 2 || rate != float64(values["hget_calls_delta"])/2 || !redisBatteryClientValues(values) {
		return redisConnectionDiagnostics{}, false
	}
	return redisConnectionDiagnostics{values: values, latency: latency, hgetNominalRate: rate}, true
}

// Reconstruct in one fixed order so no original numeric spelling or arbitrary
// field can survive into the public alert or its later cached copies.
func (self redisConnectionDiagnostics) evidence() string {
	parts := make([]string, 0, len(redisConnectionIntegerKeys)+2)
	for _, key := range redisConnectionIntegerKeys {
		parts = append(parts, key+"="+strconv.FormatInt(self.values[key], 10))
	}
	parts = append(parts, "latency_avg_ms="+strconv.FormatFloat(self.latency, 'g', -1, 64), "hget_calls_per_nominal_second="+strconv.FormatFloat(self.hgetNominalRate, 'g', -1, 64))
	return strings.Join(parts, "\n")
}

// Collection is one bounded host command. Transport failures and malformed
// projections are optional-source notes, independent of the count violation.
func redisConnectionBattery(ctx context.Context, env *probeEnv, h *host, port int) string {
	if h == nil {
		return ""
	}
	out, err := env.runner.shell(ctx, h, redisConnectionBatteryCommand(port))
	if err != nil {
		return fmt.Sprintf("node %d connection diagnostics unavailable: error_class=%s", port, classifyObservationError(err))
	}
	diagnostic, valid := parseRedisConnectionDiagnostics(out)
	if !valid || diagnostic.values["queried_node_port"] != int64(port) {
		return fmt.Sprintf("node %d connection diagnostics unavailable: error_class=%s", port, observationErrorClassInvalidResponse)
	}
	return diagnostic.evidence()
}
