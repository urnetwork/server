// Redis non-expiring key skew (SIGNALS.md §3.3c): compare every master's
// persistent key density per owned slot, then attribute only the worst
// material outlier with two bounded, in-Redis PTTL samples.
package monitor

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Register the 15-minute fleet census and sampled attribution contract.
func NewRedisNonexpiringSignal() Signal {
	return &signalAdapter{
		number: "3.3c",
		key:    "redis-nonexpiring",
		name:   "Redis slot-normalized non-expiring key skew",
		probe:  redisNonexpiringProbe{},
		accept: acceptProbeIDs("redis/nonexpiring-skew", "monitor/visibility"),
	}
}

const (
	redisNonexpiringSkewRatio       = 1.20
	redisNonexpiringMinimumExcess   = int64(100_000)
	redisPersistentSampleKeyLimit   = 5_000
	redisPersistentSampleBatchLimit = 25
	redisPersistentSampleScanCount  = 250
	redisPersistentSampleMinimum    = 1_000
	redisPersistentAttributionDelta = 0.05
	redisPersistentAttributionLead  = 0.02
	redisPersistentAttributionCount = int64(100)
)

var redisPersistentFamilyOrder = []string{
	"provide-pms",
	"provide-rp",
	"provide-sk",
	"provide-other",
	"score",
	"client-key",
	"connect",
	"stream",
	"other",
}

// The key never leaves Redis. This fixed classifier returns only allowlisted
// family names and integer PTTL categories. Unknown or malformed namespaces
// are folded into fixed "provide-other" / "other" aggregates.
const redisPersistentFamiliesLua = `
local cursor = '0'
local seen = 0
local batches = 0
local persistent = 0
local expiring = 0
local missing = 0
local invalid = 0
local sample_limit = tonumber(ARGV[1])
local batch_limit = tonumber(ARGV[2])
local scan_count = tonumber(ARGV[3])
local names = {'provide-pms', 'provide-rp', 'provide-sk', 'provide-other', 'score', 'client-key', 'connect', 'stream', 'other'}
local stats = {}
for _, name in ipairs(names) do
  stats[name] = {0, 0, 0, 0, 0}
end

local function family(key)
  if string.sub(key, 1, 4) == '{pm_' then
    local close = string.find(key, '}', 5, true)
    if close then
      local suffix = string.sub(key, close + 1)
      if suffix == 'pms' then return 'provide-pms' end
      if suffix == 'rp' then return 'provide-rp' end
      if string.match(suffix, '^sk_[0-9]+$') then return 'provide-sk' end
    end
    return 'provide-other'
  end
  if string.sub(key, 1, 4) == '{cs_' then return 'score' end
  if string.sub(key, 1, 5) == 'ckey_' then return 'client-key' end
  if string.find(key, '}s_sk_', 1, true) or string.find(key, '}s2_sk_', 1, true) then return 'stream' end
  if string.find(key, '}s2_c_', 1, true) then return 'connect' end
  return 'other'
end

repeat
  local result = redis.call('SCAN', cursor, 'COUNT', scan_count)
  cursor = result[1]
  batches = batches + 1
  for _, key in ipairs(result[2]) do
    if seen < sample_limit then
      seen = seen + 1
      local name = family(key)
      local values = stats[name]
      local pttl = redis.call('PTTL', key)
      values[1] = values[1] + 1
      if pttl == -1 then
        persistent = persistent + 1
        values[2] = values[2] + 1
      elseif pttl >= 0 then
        expiring = expiring + 1
        values[3] = values[3] + 1
      elseif pttl == -2 then
        missing = missing + 1
        values[4] = values[4] + 1
      else
        invalid = invalid + 1
        values[5] = values[5] + 1
      end
    end
  end
until cursor == '0' or batches >= batch_limit or seen >= sample_limit

local response = {seen, persistent, expiring, missing, invalid}
for _, name in ipairs(names) do
  local values = stats[name]
  table.insert(response, name)
  for index = 1, 5 do
    table.insert(response, values[index])
  end
end
return response`

// redisNonexpiringProbe owns the exact fleet census and bounded attribution.
type redisNonexpiringProbe struct{}

// redisKeyspaceNode is one master's identifier-free keyspace and slot census.
type redisKeyspaceNode struct {
	port         int
	keys         int64
	expiringKeys int64
	slots        int64
}

// nonexpiringKeys derives persistent keys from exact INFO keyspace counters.
func (self redisKeyspaceNode) nonexpiringKeys() int64 { return self.keys - self.expiringKeys }

// nonexpiringPerSlot normalizes nodes with different slot ownership.
func (self redisKeyspaceNode) nonexpiringPerSlot() float64 {
	return float64(self.nonexpiringKeys()) / float64(self.slots)
}

// redisNonexpiringOutlier carries one material fleet comparison.
type redisNonexpiringOutlier struct {
	node            redisKeyspaceNode
	ratio           float64
	estimatedExcess int64
}

// redisPersistentFamily contains only fixed-label aggregate PTTL categories.
type redisPersistentFamily struct {
	sampled    int64
	persistent int64
	expiring   int64
	missing    int64
	invalid    int64
}

// redisPersistentSample is the complete privacy-safe response from one node.
type redisPersistentSample struct {
	seen       int64
	persistent int64
	expiring   int64
	missing    int64
	invalid    int64
	families   map[string]redisPersistentFamily
}

// redisPersistentAttribution records a fixed family or an explicit ambiguity.
type redisPersistentAttribution struct {
	status string
	delta  float64
}

// Bind the stable monitor identity, severity, and cadence.
func (self redisNonexpiringProbe) id() string { return "redis/nonexpiring-skew" }

// Keep persistent skew operational until it threatens capacity or writes.
func (self redisNonexpiringProbe) tier() string { return tierWarn }

// Limit the two detailed samples to one pass every 15 minutes.
func (self redisNonexpiringProbe) cadence() time.Duration { return 15 * time.Minute }

// Preserve exact fleet skew even when detailed attribution is unavailable.
func (self redisNonexpiringProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	ports := h.redisNodePorts()
	if len(ports) < 3 {
		return nil, fmt.Errorf("need at least three Redis masters for a fleet skew baseline")
	}
	nodes, err := collectRedisKeyspaceNodes(ctx, env, h, ports)
	if err != nil {
		return nil, err
	}

	medianDensity, outliers := redisNonexpiringOutliers(nodes)
	if len(outliers) == 0 {
		return []finding{healthyFinding("redis/nonexpiring-skew", tierWarn, "nonexpiring-key-skew", h.name)}, nil
	}

	worst := outliers[0]
	control := redisNonexpiringControl(nodes, worst.node.port, medianDensity)
	target := fmt.Sprintf("%s:%d", h.name, worst.node.port)
	attribution := redisPersistentAttribution{status: "unavailable"}
	observedParts := []string{
		fmt.Sprintf("worst_port=%d", worst.node.port),
		fmt.Sprintf("keys=%d", worst.node.keys),
		fmt.Sprintf("expiring_keys=%d", worst.node.expiringKeys),
		fmt.Sprintf("non_expiring_keys=%d", worst.node.nonexpiringKeys()),
		fmt.Sprintf("owned_slots=%d", worst.node.slots),
		fmt.Sprintf("non_expiring_per_slot=%.3f", worst.node.nonexpiringPerSlot()),
		fmt.Sprintf("fleet_median_per_slot=%.3f", medianDensity),
		"ratio=" + redisNonexpiringRatioText(worst.ratio),
		fmt.Sprintf("estimated_excess_keys=%d", worst.estimatedExcess),
		fmt.Sprintf("material_outlier_nodes=%d", len(outliers)),
		fmt.Sprintf("control_port=%d", control.port),
	}

	visibility := []finding{}
	candidateSample, candidateErr := sampleRedisPersistentFamilies(ctx, env, h, worst.node.port)
	controlSample, controlErr := sampleRedisPersistentFamilies(ctx, env, h, control.port)
	if candidateErr == nil && controlErr == nil {
		attribution = classifyRedisPersistentAttribution(candidateSample, controlSample)
		observedParts = append(observedParts, redisPersistentSampleSummary("candidate", candidateSample)...)
		observedParts = append(observedParts, redisPersistentSampleSummary("control", controlSample)...)
	} else {
		visibility = append(visibility, cannotObserveFinding(
			target+"/nonexpiring-attribution",
			fmt.Errorf("bounded Redis family/PTTL attribution was unavailable; no raw sample output was retained"),
		))
	}
	observedParts = append(observedParts, "detail_status="+attribution.status)
	if attribution.status != "unavailable" {
		observedParts = append(observedParts, fmt.Sprintf("detail_lead_percentage_points=%.1f", 100*attribution.delta))
	}

	mechanism := "One Redis master retains materially more non-expiring keys per owned hash slot than the fleet median. Equal raw key counts are not required when slot ownership differs, so the detector normalizes first; the remaining excess is a real persistent-key distribution defect rather than memory pressure or cluster-slot imbalance by itself."
	context := "The exact INFO keyspace census proves the skew; the bounded SCAN/PTTL samples are attribution rather than a full census. Redis SCAN may repeat or miss concurrently changing keys, so cleanup authority never follows from a sample alone."
	action := "Repeat the bounded aggregate sample and inspect only the attributed fixed family and its current writer. Do not print keys, delete data, or run a generic cleanup from an unattributed fleet ratio."
	verify := "On two consecutive samples, every master's non-expiring keys per owned slot are below both the 1.20x fleet-median ratio and the 100,000-key material-excess floor; cluster coverage, latency, errors, and evictions remain healthy."
	evidence := "The probe read only aggregate INFO keyspace counts and each master's own slot ranges. On the worst material outlier and one median control, a bounded EVAL_RO classified at most 5,000 keys entirely inside Redis and returned only fixed family/PTTL counters; no key, value, or identifier crossed the observation boundary."

	switch attribution.status {
	case "provide":
		mechanism = "The outlier's exact slot-normalized excess is paired with a material candidate-versus-control increase in persistent `{pm_…}` provide-mirror keys. Current writers assign these cache keys a 72-hour TTL, so the no-TTL cohort is legacy residue; the read-only sample cannot distinguish a historically missed/interrupted cleanup from restoration of an older RDB."
		context += " The fixed provide subtypes (pms, rp, and sk) are counted separately inside Redis; identifiers and raw keys never leave the node. Routine Redis deployment does not invoke the maintenance cleanup."
		action = "Confirm the deployed writers still assign the 72-hour TTL, then use a separate maintenance authorization for the Xops Redis cleanup's `{pm_*}` EXPIRE-NX phase. Review its dry-run scope first: do not run the broader script, delete keys, or reset an existing TTL solely from this alert."
		verify = "Immediately after an authorized EXPIRE-NX pass, a bounded candidate sample contains zero persistent allowlisted provide-mirror keys and no existing TTL was extended; after one full 72-hour window, the slot-normalized non-expiring skew clears while cluster coverage, latency, errors, and evictions remain healthy."
	case "score", "client-key", "connect", "stream":
		mechanism += fmt.Sprintf(" The bounded candidate-versus-control sample attributes the largest persistent-key excess to the fixed %s family, but does not establish which writer generation created it.", attribution.status)
		action = "Verify the attributed family's current TTL contract and deployment generation, then identify whether the cohort is still being written or is restored residue. Do not reuse a cleanup written for another key schema, print keys, or mutate Redis from this alert alone."
	case "ambiguous":
		mechanism += " The candidate and fleet-control samples contain mixed fixed-family deltas without one family clearing the confidence margin, so the monitor deliberately leaves ownership ambiguous."
		action = "Repeat the bounded sample on the same candidate and median control, then correlate fixed family aggregates with current writer TTL contracts. Do not choose the provide-mirror cleanup, print keys, or mutate Redis while attribution remains ambiguous."
	case "unavailable":
		context += " Detailed attribution is unavailable rather than zero; a separate visibility finding preserves that distinction."
		action = "Restore the bounded EVAL_RO observation path and rerun this signal. Preserve the exact aggregate skew, but do not infer a zero family count, print keys, or mutate Redis without a complete candidate/control sample."
		evidence = "The aggregate INFO keyspace and owned-slot census completed. The bounded candidate/control EVAL_RO detail did not return a complete fixed aggregate; its raw response was discarded and no family count was rendered."
	}

	skewFinding := finding{
		probeId: "redis/nonexpiring-skew", tier: tierWarn,
		class: "nonexpiring-key-skew", target: target, sustain: 1,
		symptom: fmt.Sprintf(
			"Redis port %d retains %s the fleet-median non-expiring keys per slot, with about %d excess keys",
			worst.node.port, redisNonexpiringRatioText(worst.ratio), worst.estimatedExcess,
		),
		mechanism: mechanism,
		baseline:  "Every master stays below 1.20x the fleet median after normalizing non-expiring keys by owned slots, or differs by fewer than 100,000 material keys; detailed attribution is complete before prescribing family-specific maintenance.",
		observed:  strings.Join(observedParts, " "),
		evidence:  evidence,
		context:   context,
		action:    action,
		verify:    verify,
		playbook:  "SIGNALS.md §3.3c",
	}
	return append([]finding{skewFinding}, visibility...), nil
}

// Keep topology and key counts on the Redis host; return numeric aggregates.
func collectRedisKeyspaceNodes(ctx context.Context, env *probeEnv, h *host, ports []int) ([]redisKeyspaceNode, error) {
	portNames := make([]string, len(ports))
	for index, port := range ports {
		portNames[index] = strconv.Itoa(port)
	}
	command := fmt.Sprintf(`for p in %s; do
  info=$(timeout 3 redis-cli -p "$p" --raw INFO keyspace 2>/dev/null | tr -d '\r')
  [ -z "$info" ] && { echo "$p unreachable"; continue; }
  stats=$(printf '%%s\n' "$info" | awk -F: '
    BEGIN{keys=0;expires=0}
    /^db0:/{split($2,a,",");for(i in a){split(a[i],v,"=");if(v[1]=="keys")keys=v[2];else if(v[1]=="expires")expires=v[2]}}
    END{print keys" "expires}')
  slots=$(timeout 3 redis-cli -p "$p" --raw CLUSTER NODES 2>/dev/null | tr -d '\r' | awk '
    index(","$3",",",myself,") {
      for(i=9;i<=NF;i++) {
        if($i ~ /^[0-9]+-[0-9]+$/) {split($i,a,"-");slots+=a[2]-a[1]+1}
        else if($i ~ /^[0-9]+$/) {slots++}
      }
    }
    END{print slots+0}')
  echo "$p $stats $slots"
done`, strings.Join(portNames, " "))
	out, err := env.runner.shell(ctx, h, command)
	if err != nil {
		return nil, fmt.Errorf("Redis keyspace census command failed: %w", err)
	}
	return parseRedisKeyspaceNodes(out, ports)
}

// Fail closed on partial, duplicate, or internally inconsistent census rows.
func parseRedisKeyspaceNodes(out string, expectedPorts []int) ([]redisKeyspaceNode, error) {
	expected := make(map[int]bool, len(expectedPorts))
	for _, port := range expectedPorts {
		expected[port] = true
	}
	seen := make(map[int]bool, len(expectedPorts))
	nodes := make([]redisKeyspaceNode, 0, len(expectedPorts))
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 2 && fields[1] == "unreachable" {
			return nil, fmt.Errorf("Redis keyspace census is incomplete because a configured master was unreachable")
		}
		if len(fields) != 4 {
			return nil, fmt.Errorf("Redis keyspace census returned a malformed aggregate row")
		}
		values := make([]int64, 4)
		for index, field := range fields {
			value, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("Redis keyspace census returned a malformed numeric aggregate")
			}
			values[index] = value
		}
		port := int(values[0])
		if !expected[port] || seen[port] {
			return nil, fmt.Errorf("Redis keyspace census returned an unexpected or duplicate master")
		}
		if values[1] < 0 || values[2] < 0 || values[1] < values[2] || values[3] <= 0 {
			return nil, fmt.Errorf("Redis keyspace census returned inconsistent key or slot aggregates")
		}
		seen[port] = true
		nodes = append(nodes, redisKeyspaceNode{
			port: port, keys: values[1], expiringKeys: values[2], slots: values[3],
		})
	}
	if len(nodes) != len(expectedPorts) {
		return nil, fmt.Errorf("Redis keyspace census omitted one or more configured masters")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].port < nodes[j].port })
	return nodes, nil
}

// Compare normalized density against both ratio and material-count floors.
func redisNonexpiringOutliers(nodes []redisKeyspaceNode) (float64, []redisNonexpiringOutlier) {
	densities := make([]float64, len(nodes))
	for index, node := range nodes {
		densities[index] = node.nonexpiringPerSlot()
	}
	sort.Float64s(densities)
	median := densities[len(densities)/2]
	if len(densities)%2 == 0 {
		median = (densities[len(densities)/2-1] + median) / 2
	}
	outliers := []redisNonexpiringOutlier{}
	for _, node := range nodes {
		density := node.nonexpiringPerSlot()
		excess := int64(math.Round((density - median) * float64(node.slots)))
		ratio := math.Inf(1)
		if median > 0 {
			ratio = density / median
		}
		if excess >= redisNonexpiringMinimumExcess &&
			(median == 0 && density > 0 || median > 0 && ratio >= redisNonexpiringSkewRatio) {
			outliers = append(outliers, redisNonexpiringOutlier{node: node, ratio: ratio, estimatedExcess: excess})
		}
	}
	sort.Slice(outliers, func(i, j int) bool {
		if outliers[i].ratio != outliers[j].ratio {
			return outliers[i].ratio > outliers[j].ratio
		}
		if outliers[i].estimatedExcess != outliers[j].estimatedExcess {
			return outliers[i].estimatedExcess > outliers[j].estimatedExcess
		}
		return outliers[i].node.port < outliers[j].node.port
	})
	return median, outliers
}

// Select a non-candidate master closest to the fleet median.
func redisNonexpiringControl(nodes []redisKeyspaceNode, candidatePort int, median float64) redisKeyspaceNode {
	control := redisKeyspaceNode{}
	bestDistance := math.Inf(1)
	for _, node := range nodes {
		if node.port == candidatePort {
			continue
		}
		distance := math.Abs(node.nonexpiringPerSlot() - median)
		if control.port == 0 || distance < bestDistance || distance == bestDistance && node.port < control.port {
			control = node
			bestDistance = distance
		}
	}
	return control
}

// Render a finite ratio or the explicit zero-baseline state.
func redisNonexpiringRatioText(ratio float64) string {
	if math.IsInf(ratio, 1) {
		return "unbounded_vs_zero"
	}
	return fmt.Sprintf("%.3fx", ratio)
}

// Run fixed-label key classification and PTTL checks entirely inside Redis.
func sampleRedisPersistentFamilies(ctx context.Context, env *probeEnv, h *host, port int) (redisPersistentSample, error) {
	encodedScript := base64.StdEncoding.EncodeToString([]byte(redisPersistentFamiliesLua))
	command := fmt.Sprintf(
		`timeout 45 redis-cli -p %d --raw EVAL_RO "$(printf %%s %s | base64 -d)" 0 %d %d %d`,
		port,
		encodedScript,
		redisPersistentSampleKeyLimit,
		redisPersistentSampleBatchLimit,
		redisPersistentSampleScanCount,
	)
	out, err := env.runner.shell(ctx, h, command)
	if err != nil {
		return redisPersistentSample{}, fmt.Errorf("bounded Redis family/PTTL command failed")
	}
	sample, err := parseRedisPersistentSample(out)
	if err != nil {
		return redisPersistentSample{}, err
	}
	if sample.seen < redisPersistentSampleMinimum || sample.invalid > 0 || sample.missing*10 > sample.seen {
		return redisPersistentSample{}, fmt.Errorf("bounded Redis family/PTTL aggregate was incomplete")
	}
	return sample, nil
}

// Reject unknown labels and inconsistent totals without echoing raw output.
func parseRedisPersistentSample(out string) (redisPersistentSample, error) {
	fields := strings.Fields(out)
	expectedFields := 5 + 6*len(redisPersistentFamilyOrder)
	if len(fields) != expectedFields {
		return redisPersistentSample{}, fmt.Errorf("bounded Redis family/PTTL response had an unexpected aggregate shape")
	}
	parse := func(field string) (int64, error) {
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("bounded Redis family/PTTL response contained an invalid aggregate")
		}
		return value, nil
	}
	header := make([]int64, 5)
	for index := range header {
		value, err := parse(fields[index])
		if err != nil {
			return redisPersistentSample{}, err
		}
		header[index] = value
	}
	sample := redisPersistentSample{
		seen: header[0], persistent: header[1], expiring: header[2], missing: header[3], invalid: header[4],
		families: make(map[string]redisPersistentFamily, len(redisPersistentFamilyOrder)),
	}
	familyTotals := [5]int64{}
	for familyIndex, expectedName := range redisPersistentFamilyOrder {
		offset := 5 + 6*familyIndex
		if fields[offset] != expectedName {
			return redisPersistentSample{}, fmt.Errorf("bounded Redis family/PTTL response contained an unexpected family label")
		}
		values := make([]int64, 5)
		for valueIndex := range values {
			value, err := parse(fields[offset+1+valueIndex])
			if err != nil {
				return redisPersistentSample{}, err
			}
			values[valueIndex] = value
			familyTotals[valueIndex] += value
		}
		if values[0] != values[1]+values[2]+values[3]+values[4] {
			return redisPersistentSample{}, fmt.Errorf("bounded Redis family/PTTL response contained inconsistent family aggregates")
		}
		sample.families[expectedName] = redisPersistentFamily{
			sampled: values[0], persistent: values[1], expiring: values[2], missing: values[3], invalid: values[4],
		}
	}
	if sample.seen <= 0 || sample.seen != sample.persistent+sample.expiring+sample.missing+sample.invalid ||
		familyTotals != [5]int64{sample.seen, sample.persistent, sample.expiring, sample.missing, sample.invalid} {
		return redisPersistentSample{}, fmt.Errorf("bounded Redis family/PTTL response contained inconsistent totals")
	}
	return sample, nil
}

// Require one allowlisted family to lead its control by a fixed margin.
func classifyRedisPersistentAttribution(candidate, control redisPersistentSample) redisPersistentAttribution {
	type group struct {
		name    string
		members []string
		safe    bool
	}
	groups := []group{
		{name: "provide", members: []string{"provide-pms", "provide-rp", "provide-sk"}, safe: true},
		{name: "provide-other", members: []string{"provide-other"}},
		{name: "score", members: []string{"score"}, safe: true},
		{name: "client-key", members: []string{"client-key"}, safe: true},
		{name: "connect", members: []string{"connect"}, safe: true},
		{name: "stream", members: []string{"stream"}, safe: true},
		{name: "other", members: []string{"other"}},
	}
	type rankedGroup struct {
		group
		candidatePersistent int64
		delta               float64
	}
	ranked := make([]rankedGroup, 0, len(groups))
	for _, group := range groups {
		candidatePersistent := int64(0)
		controlPersistent := int64(0)
		for _, member := range group.members {
			candidatePersistent += candidate.families[member].persistent
			controlPersistent += control.families[member].persistent
		}
		delta := float64(candidatePersistent)/float64(candidate.seen) -
			float64(controlPersistent)/float64(control.seen)
		ranked = append(ranked, rankedGroup{group: group, candidatePersistent: candidatePersistent, delta: delta})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].delta != ranked[j].delta {
			return ranked[i].delta > ranked[j].delta
		}
		return ranked[i].name < ranked[j].name
	})
	top := ranked[0]
	secondDelta := ranked[1].delta
	if top.safe && top.candidatePersistent >= redisPersistentAttributionCount &&
		top.delta >= redisPersistentAttributionDelta && top.delta-secondDelta >= redisPersistentAttributionLead {
		return redisPersistentAttribution{status: top.name, delta: top.delta}
	}
	return redisPersistentAttribution{status: "ambiguous", delta: max(0, top.delta)}
}

// Render only fixed field names and numeric aggregate values.
func redisPersistentSampleSummary(prefix string, sample redisPersistentSample) []string {
	parts := []string{
		fmt.Sprintf("%s_sampled=%d", prefix, sample.seen),
		fmt.Sprintf("%s_persistent=%d", prefix, sample.persistent),
		fmt.Sprintf("%s_expiring=%d", prefix, sample.expiring),
		fmt.Sprintf("%s_missing=%d", prefix, sample.missing),
	}
	for _, name := range redisPersistentFamilyOrder {
		label := strings.ReplaceAll(name, "-", "_")
		parts = append(parts, fmt.Sprintf("%s_%s_persistent=%d", prefix, label, sample.families[name].persistent))
	}
	return parts
}
