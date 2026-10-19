// Shared Redis snapshot collection and evaluation used by redis-memory,
// redis-buffers, redis-connections, and redis-topology.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// Reaching every Redis maxmemory ceiling is not a healthy terminal state:
	// the host still needs room for allocator/RSS variation, the kernel, and
	// non-Redis services. Keep an absolute floor for ordinary hosts and scale
	// it slightly on large-memory hosts.
	redisHostCapacityReserveMinimum = float64(8 << 30)
	redisHostCapacityReserveRatio   = 0.02
)

// redisMemoryProbe is SIGNALS.md 3.1/3.2/3.5: the per-node memory table (skew
// detector), dataset-vs-clients attribution, and connected_clients. One ssh
// round collects INFO from every node; findings are per-node so each sick node
// gets its own ticket identity. The per-node escalation battery runs once per
// trip (batteryLatch), not on every 5-minute tick while a node stays sick.
type redisMemoryProbe struct {
	batteries       *batteryLatch
	batteryProbeIDs map[string]bool
}

func newRedisMemoryProbe(batteryProbeIDs ...string) *redisMemoryProbe {
	enabled := make(map[string]bool, len(batteryProbeIDs))
	for _, probeID := range batteryProbeIDs {
		enabled[probeID] = true
	}
	return &redisMemoryProbe{batteries: newBatteryLatch(), batteryProbeIDs: enabled}
}

func (self *redisMemoryProbe) batteryEvidence(probeID, key string, battery func() string) string {
	if !self.batteryProbeIDs[probeID] {
		return ""
	}
	return self.batteries.broken(key, battery)
}

func (self *redisMemoryProbe) rearmBattery(probeID, key string) {
	if self.batteryProbeIDs[probeID] {
		self.batteries.healthy(key)
	}
}

func (self *redisMemoryProbe) id() string             { return "redis/node-mem" }
func (self *redisMemoryProbe) tier() string           { return tierWarn }
func (self *redisMemoryProbe) cadence() time.Duration { return 5 * time.Minute }

// redisNodeMem is one node's parsed INFO memory/clients numbers.
type redisNodeMem struct {
	port                        int
	usedBytes                   float64
	maxmemoryBytes              float64
	datasetBytes                float64
	clientsBytes                float64
	connectedClients            float64
	maxmemoryPolicy             string
	keys                        float64
	expiringKeys                float64
	averageTTLMillis            float64
	evictedKeys                 float64
	currentEvictionExceededTime float64
	totalErrorReplies           float64
	oomErrors                   float64
	rssBytes                    float64
}

// redisNodeUsage is the small shared preflight used by probes that need one
// representative node for a bounded keyspace sample. Selecting by percentage
// keeps the sample on the node closest to its own configured ceiling even if
// Redis nodes do not all have the same maxmemory.
type redisNodeUsage struct {
	port           int
	usedBytes      int64
	maxmemoryBytes int64
}

// redisHostFailure is a redis-cluster host that a probe could not read. A
// probe that sweeps every host keeps judging the hosts it could read, reports
// each failed host on its own target, and fails as a whole only when no host
// could be read, as the single-host probe always has.
type redisHostFailure struct {
	host *host
	err  error
}

// redisHostFailuresError is the probe error once no host could be read. One
// host keeps its own error; several are joined, so a host-scope exclusion of
// one host cannot stand in for another host's real failure.
func redisHostFailuresError(failures []redisHostFailure) error {
	if len(failures) == 1 {
		return failures[0].err
	}
	errs := make([]error, 0, len(failures))
	for _, failure := range failures {
		errs = append(errs, failure.err)
	}
	return errors.Join(errs...)
}

// redisHostCannotObserveFinding reports one host a sweep could not read. The
// host/observation target is the one the memory, process, and cluster sweeps
// use, so host-scope reduction and per-host identity treat it the same way.
func redisHostCannotObserveFinding(failure redisHostFailure, observation string) finding {
	unknown := cannotObserveFinding(failure.host.name+"/"+observation, failure.err)
	unknown.observed += fmt.Sprintf(" unobserved_nodes=%d", len(failure.host.redisNodePorts()))
	return unknown
}

// redisUnobservedScope names the hosts a partial verdict was judged without
// and counts their configured nodes.
func redisUnobservedScope(failures []redisHostFailure) (string, int) {
	names := make([]string, 0, len(failures))
	nodes := 0
	for _, failure := range failures {
		names = append(names, failure.host.name)
		nodes += len(failure.host.redisNodePorts())
	}
	return strings.Join(names, ","), nodes
}

// redisUnobservedHosts marks the observed values of a cluster-wide verdict
// that was judged without the failed hosts' nodes.
func redisUnobservedHosts(failures []redisHostFailure) string {
	hosts, nodes := redisUnobservedScope(failures)
	return fmt.Sprintf("unobserved_hosts=%s unobserved_nodes=%d", hosts, nodes)
}

// redisPartialSymptom marks the symptom of such a verdict.
func redisPartialSymptom(failures []redisHostFailure) string {
	hosts, nodes := redisUnobservedScope(failures)
	return fmt.Sprintf(" (partial: %d node(s) on %s not read)", nodes, hosts)
}

// fullestRedisNodeUsage samples every node on every redis-cluster host and
// returns the fullest observed bounded node with its owning host. Missing,
// ambiguous, or unbounded nodes leave fleet selection unknown; valid siblings
// remain useful for qualified samples. With no selectable node the sample fails.
func fullestRedisNodeUsage(ctx context.Context, env *probeEnv) (*host, redisNodeUsage, []redisHostFailure, error) {
	hosts := env.cfg.redisSweepHosts()
	if len(hosts) == 0 {
		return nil, redisNodeUsage{}, nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	var fullestHost *host
	fullest := redisNodeUsage{}
	fullestRatio := -1.0
	failures := []redisHostFailure{}
	for _, h := range hosts {
		ports := h.redisNodePorts()
		if len(ports) == 0 {
			return nil, redisNodeUsage{}, nil, fmt.Errorf("no redis node ports configured")
		}
		portNames := make([]string, len(ports))
		for i, port := range ports {
			portNames[i] = fmt.Sprint(port)
		}
		out, err := env.runner.shell(ctx, h, fmt.Sprintf(`for p in %s; do
  m=$(timeout 3 redis-cli -p "$p" INFO memory 2>/dev/null)
  info_exit=$?
  [ "$info_exit" -eq 0 ] && [ -n "$m" ] || { echo "$p unreachable"; continue; }
  echo "$p $(printf '%%s\n' "$m" | awk -F: '
    {sub(/\r$/, ""); if($0=="" || /^#/)next; if($0 !~ /^[a-zA-Z0-9_]+:/){invalid=1;next}}
    /^used_memory:/{if(NF!=2 || used_seen++ || $2 !~ /^[0-9]+$/)invalid=1;u=$2}
    /^maxmemory:/{if(NF!=2 || max_seen++ || $2 !~ /^[0-9]+$/)invalid=1;x=$2}
    END{if(invalid || !used_seen || !max_seen)print "unreachable";else print u" "x}')"
done`, strings.Join(portNames, " ")))
		if err != nil {
			failures = append(failures, redisHostFailure{host: h, err: err})
			continue
		}
		nodes, censusErr := parseRedisNodeUsage(h, out)
		if censusErr != nil {
			failures = append(failures, redisHostFailure{host: h, err: censusErr})
		}
		for _, candidate := range nodes {
			if candidate.maxmemoryBytes == 0 {
				continue
			}
			ratio := float64(candidate.usedBytes) / float64(candidate.maxmemoryBytes)
			if fullest.port == 0 || fullestRatio < ratio {
				fullest = candidate
				fullestRatio = ratio
				fullestHost = h
			}
		}
	}
	if fullest.port == 0 {
		if len(failures) > 0 {
			return nil, redisNodeUsage{}, nil, redisHostFailuresError(failures)
		}
		return nil, redisNodeUsage{}, nil, fmt.Errorf("could not determine fullest Redis node from the configured census")
	}
	return fullestHost, fullest, failures, nil
}

type redisConnectionDiagnosis struct {
	mechanism string
	context   string
	action    string
	verify    string
}

func redisConnectionEvidenceInt(evidence, key string) (int, bool) {
	prefix := key + "="
	for _, field := range strings.Fields(evidence) {
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		value, err := strconv.Atoi(strings.TrimPrefix(field, prefix))
		return value, err == nil
	}
	return 0, false
}

func redisConnectionEvidenceFloat(evidence, key string) (float64, bool) {
	prefix := key + "="
	for _, field := range strings.Fields(evidence) {
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimPrefix(field, prefix), 64)
		return value, err == nil
	}
	return 0, false
}

func redisConnectionEvidenceInt64(evidence, key string) (int64, bool) {
	prefix := key + "="
	for _, field := range strings.Fields(evidence) {
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimPrefix(field, prefix), 10, 64)
		return value, err == nil
	}
	return 0, false
}

func redisConnectionControlSummary(evidence string) (string, bool) {
	details := []string{}
	unhealthy := false
	complete := true

	blocked, blockedKnown := redisConnectionEvidenceInt(evidence, "blocked_clients")
	complete = complete && blockedKnown
	if blockedKnown {
		details = append(details, fmt.Sprintf("blocked_clients=%d", blocked))
	}

	latency, latencyKnown := redisConnectionEvidenceFloat(evidence, "latency_avg_ms")
	complete = complete && latencyKnown
	if latencyKnown {
		details = append(details, fmt.Sprintf("latency_avg_ms=%.3f", latency))
		unhealthy = unhealthy || latency >= 10
	}

	acceptQueue, acceptQueueKnown := redisConnectionEvidenceInt(evidence, "accept_recv_q")
	acceptBacklog, acceptBacklogKnown := redisConnectionEvidenceInt(evidence, "accept_send_q")
	complete = complete && acceptQueueKnown && acceptBacklogKnown
	if acceptQueueKnown {
		details = append(details, fmt.Sprintf("accept_recv_q=%d", acceptQueue))
	}
	if acceptBacklogKnown {
		details = append(details, fmt.Sprintf("accept_send_q=%d", acceptBacklog))
	}
	if acceptQueueKnown && acceptBacklogKnown {
		unhealthy = unhealthy || acceptQueue > 0 && (acceptBacklog <= 0 || acceptBacklog <= acceptQueue)
	}

	clientMemory, clientMemoryKnown := redisConnectionEvidenceInt64(evidence, "mem_clients_normal_bytes")
	usedMemory, usedMemoryKnown := redisConnectionEvidenceInt64(evidence, "used_memory_bytes")
	complete = complete && clientMemoryKnown && usedMemoryKnown
	if clientMemoryKnown {
		details = append(details, fmt.Sprintf("client_memory_bytes=%d", clientMemory))
	}
	if usedMemoryKnown {
		details = append(details, fmt.Sprintf("used_memory_bytes=%d", usedMemory))
	}
	if clientMemoryKnown && usedMemoryKnown {
		unhealthy = unhealthy || clientMemory > int64(2<<30) || 4*clientMemory > usedMemory
	}

	maxOutputMemory, maxOutputMemoryKnown := redisConnectionEvidenceInt64(evidence, "client_output_memory_max_bytes")
	complete = complete && maxOutputMemoryKnown
	if maxOutputMemoryKnown {
		details = append(details, fmt.Sprintf("max_client_output_memory_bytes=%d", maxOutputMemory))
		unhealthy = unhealthy || maxOutputMemory > int64(32<<20)
	}

	if len(details) == 0 {
		return "", false
	}
	summary := "Trip-time Redis controls: " + strings.Join(details, " ") + "."
	switch {
	case complete && unhealthy:
		summary += " At least one captured latency, accept-queue/backlog, client-memory, or output-buffer control is unhealthy; the attributed workload shape is exerting active pressure. Blocked-client state is retained as supporting context rather than treated as pressure by itself."
	case complete:
		summary += " All captured latency, accept-queue/backlog, client-memory, and output-buffer controls are below their alert bands; blocked-client state is retained as supporting context."
	default:
		summary += " Control coverage is incomplete, so this battery cannot independently clear Redis impairment."
	}
	return summary, complete && unhealthy
}

func diagnoseRedisConnectionSpike(evidence string) redisConnectionDiagnosis {
	lowerEvidence := strings.ToLower(evidence)
	controlSummary, controlsUnhealthy := redisConnectionControlSummary(lowerEvidence)
	appendControlSummary := func(context string) string {
		if controlSummary == "" {
			return context
		}
		return context + " " + controlSummary
	}
	appendPressureAction := func(action string) string {
		if !controlsUnhealthy {
			return action
		}
		return action + " At least one trip-time control is already unhealthy; treat this as active Redis pressure and execute the compatible workload-distribution repair instead of waiting only for idle-pool contraction."
	}
	clientTotal, clientTotalKnown := redisConnectionEvidenceInt(lowerEvidence, "client_list_total")
	hgetClients, hgetClientsKnown := redisConnectionEvidenceInt(lowerEvidence, "client_cmd_hget_count")
	hgetRate, hgetRateKnown := redisConnectionEvidenceFloat(lowerEvidence, "hget_calls_per_second")
	if clientTotalKnown && hgetClientsKnown && hgetRateKnown &&
		clientTotal > 0 && hgetClients >= 50 && 2*hgetClients >= clientTotal && hgetRate >= 50 {
		return redisConnectionDiagnosis{
			mechanism: "One network-peer keyspace event is being fanned out to every resident listener in the Connect process, and each listener independently reads the same peer metadata with HGET. That live-delta read amplification both produces the measured HGET command rate and causes lazy Redis cluster clients to retain large per-node connection pools for the peer registry's hot slot.",
			context:   appendControlSummary(fmt.Sprintf("The trip battery counted %d of %d clients ending in HGET and measured %.3f HGET calls/second. This joint cohort-and-rate fingerprint attributes the connection shape to network-peer live-delta fanout rather than a reconnect storm, the retired reliability marker, or Redis alone.", hgetClients, clientTotal, hgetRate)),
			action:    appendPressureAction("Deploy the Connect version that constructs one lazy peer delta per subscriber event and shares its pipelined metadata/version read across all resident listeners. Do not kill Redis clients or raise pool floors; old per-node pools should contract through their ordinary idle lifetime after the rollout."),
			verify:    "On consecutive samples, the HGET command rate and HGET-ended client cohort collapse, the outlier node returns near the fleet connection median, key-event delivery continues, listener resets do not spike, and Redis latency plus pool-timeout signals remain healthy.",
		}
	}
	if strings.Contains(lowerEvidence, "queried_node_owns_reliability_marker=true") &&
		strings.Contains(lowerEvidence, "cmd=sadd") {
		return redisConnectionDiagnosis{
			mechanism: "A Redis cluster client creates and retains a per-node pool when a process first touches that node. The SADD cohort identifies the legacy client_reliability_stats_blocks fixed-slot key writer: one fleet-wide marker key concentrates every writer process's pool on its owner.",
			context:   appendControlSummary("Dominant long-lived normal-client cohorts ending in SADD/EXPIRE identify the legacy client_reliability_stats_blocks fixed-slot variant; the connection count is pool amplification, not by itself a slow Redis process or a reconnect storm."),
			action:    appendPressureAction("Roll out the marker-free reliability writer after the compatible high-water-mark rollup, then restart writers normally so old pools age out. Do not raise pool floors or kill a healthy Redis owner; those actions multiply or churn the connections."),
			verify:    "The outlier returns near the fleet median after writer rollout/restarts, the SADD/EXPIRE cohorts disappear, and Redis pool-timeout plus node-latency signals remain healthy.",
		}
	}
	currentShards, currentKnown := redisConnectionEvidenceInt(lowerEvidence, "current_reliability_shards_on_node")
	previousShards, previousKnown := redisConnectionEvidenceInt(lowerEvidence, "previous_reliability_shards_on_node")
	shardCount, shardCountKnown := redisConnectionEvidenceInt(lowerEvidence, "reliability_shard_count")
	recentMax, recentMaxKnown := redisConnectionEvidenceInt(lowerEvidence, "reliability_shards_recent_max")
	lookbackBlocks, lookbackKnown := redisConnectionEvidenceInt(lowerEvidence, "reliability_shard_lookback_blocks")
	recentMaxAge, recentMaxAgeKnown := redisConnectionEvidenceInt(lowerEvidence, "reliability_shards_recent_max_age_blocks")
	currentCollision := currentKnown && previousKnown && currentShards >= 0 && previousShards >= 0 &&
		(currentShards >= 2 || previousShards >= 2)
	historicalCollision := recentMaxKnown && recentMax >= 2
	if shardCountKnown && (currentCollision || historicalCollision) && strings.Contains(lowerEvidence, "cmd=expire") {
		mechanism := fmt.Sprintf("At the connection-spike trip, this Redis master owned %d of %d client-reliability shards for the trip minute and %d for the immediately preceding minute. Independent shard hashing can place multiple hot hashes on one master; each reliability transaction ends in EXPIRE, and that concentrated command load expands the lazy per-node client pools that touched the master.", currentShards, shardCount, previousShards)
		if !currentCollision && historicalCollision && lookbackKnown && recentMaxAgeKnown {
			mechanism = fmt.Sprintf("At the connection-spike trip, this Redis master owned %d of %d client-reliability shards for the trip minute and %d for the immediately preceding minute, but had owned as many as %d within the bounded %d-block history (%d block(s) before the trip). Independent shard hashing created that earlier hot-key collision, each reliability transaction ended in EXPIRE, and lazy per-node pools outlived the one-minute key ownership that expanded them.", currentShards, shardCount, previousShards, recentMax, lookbackBlocks, recentMaxAge)
		}
		frameContext := "The trip-time EXPIRE cohort and bounded client_reliability_stats.<block>.<shard> ownership history identify a rotating marker-free reliability load collision."
		if strings.Contains(lowerEvidence, "battery collected once at trip") {
			frameContext += " This cached battery precedes the alert symptom's later sustained client ratio, so a different CLIENT LIST total is expected while idle pools contract."
		} else {
			frameContext += " In a one-shot alert, these battery values share the trip frame with the observed client ratio."
		}
		frameContext += " This is not the retired fixed discovery-set writer, and CLIENT LIST alone is not evidence of a reconnect storm or Redis impairment."
		return redisConnectionDiagnosis{
			mechanism: mechanism,
			context:   appendControlSummary(frameContext),
			action:    appendPressureAction("Do not roll back the marker-free writer, kill healthy clients, or lower pool limits from the fleet-median ratio alone. Let minute ownership rotate and ordinary pool idle lifetime contract the shape. If latency, pool timeouts, queues, or memory become unhealthy, use a rolling-compatible wider fanout or deliberate slot placement so simultaneous reliability shards distribute more evenly."),
			verify:    "The bounded shard-history collision ages out together with the EXPIRE-heavy connection shape, and Redis latency, pool-timeout logs, accept queues, and client-buffer memory remain healthy on consecutive samples.",
		}
	}
	return redisConnectionDiagnosis{
		mechanism: "Connected clients are concentrated on this Redis node, but its bounded battery does not prove the reliability-marker fingerprint (the node must own client_reliability_stats_blocks and expose a SADD cohort). Long-lived PING/GET/EXEC cohorts can be normal lazy cluster pools on an actively used slot; uniformly young NULL/PING cohorts instead indicate reconnect churn, while abnormal flags or output memory indicate a stalled consumer.",
		context:   appendControlSummary("A fleet-median ratio identifies shape, not root cause. Attribute this node's command rate and hot key slots, compare cohort ages and source processes, and read node latency before treating the sockets as harmful."),
		action:    appendPressureAction("If latency and pool-timeout signals are healthy with long-lived normal-client cohorts, observe consecutive ticks and fix only the workload or pool ownership that explains the hot slots. For uniformly young cohorts, investigate the matching deploy/reconnect boundary; for abnormal flags or output memory, follow the slow-consumer playbook. Do not apply the reliability-marker rollout solely from this alert."),
		verify:    "The node returns near the fleet connection shape or a stable expected hot-slot owner is documented; node latency, pool timeouts, accept queue, and client output memory remain healthy on consecutive samples.",
	}
}

func (self *redisMemoryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.redisSweepHosts()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}

	// Each redis-cluster host reports its own memory and its own nodes. A host
	// that cannot be read is reported and the rest are still judged; with no
	// host readable the probe fails as the single-host probe always has.
	observedHosts := []redisHostMemory{}
	type failedHost struct {
		host *host
		err  error
	}
	failedHosts := []failedHost{}
	for _, h := range hosts {
		ports := h.redisNodePorts()
		if len(ports) == 0 {
			return nil, fmt.Errorf("no redis node ports configured")
		}
		portNames := make([]string, len(ports))
		for i, port := range ports {
			portNames[i] = strconv.Itoa(port)
		}

		// one round trip: per node, one compact line. Client-buffer memory is
		// mem_clients_normal + mem_clients_slaves (the INFO fields this redis
		// actually exposes — there is no used_memory_clients key; SIGNALS.md 3.2
		// note). Required measurements need explicit numeric values; only
		// optional diagnostic fields may use zero when absent.
		script := fmt.Sprintf(`awk '/^MemTotal:/{total=$2*1024}/^MemAvailable:/{available=$2*1024}END{print "host_memory",total,available}' /proc/meminfo 2>/dev/null
for p in %s; do
  m=$(timeout 3 redis-cli -p "$p" INFO 2>/dev/null)
  info_exit=$?
  [ "$info_exit" -eq 0 ] && [ -n "$m" ] || { echo "$p unreachable"; continue; }
  echo "$p $(printf '%%s\n' "$m" | awk -F: '
    function number(name,value) {if(NF!=2 || seen[name]++ || value !~ /^[0-9]+$/)invalid=1;return value}
    BEGIN{u=0;rss=0;x=0;d=0;c=0;n=0;policy="unknown";keys=0;expires=0;avg=0;evicted=0;exceeded=0;errors=0;oom=0}
    {sub(/\r$/,"");if($0=="" || /^#/)next;if($0 !~ /^[a-zA-Z0-9_]+:/){invalid=1;next}}
    /^used_memory:/{u=number($1,$2)} /^used_memory_rss:/{rss=number($1,$2)} /^maxmemory:/{x=number($1,$2)} /^used_memory_dataset:/{d=number($1,$2)}
    /^mem_clients_normal:/{c+=number($1,$2)} /^mem_clients_slaves:/{c+=number($1,$2)}
    /^connected_clients:/{n=number($1,$2)}
    /^maxmemory_policy:/{if(seen[$1]++ || NF!=2)invalid=1;else if($2 ~ /^(noeviction|allkeys-lru|volatile-lru|allkeys-random|volatile-random|volatile-ttl|allkeys-lfu|volatile-lfu)$/)policy=$2}
    /^evicted_keys:/{evicted=number($1,$2)} /^current_eviction_exceeded_time:/{exceeded=number($1,$2)}
    /^total_error_replies:/{errors=number($1,$2)}
    /^errorstat_OOM:/{if($2 !~ /^count=[0-9]+$/)invalid=1;split($2,a,"=");oom=number($1,a[2])}
    /^db0:/{if(seen[$1]++)invalid=1;split($2,a,",");for(i in a){split(a[i],v,"=");if(v[1]=="keys")keys=number("db0_keys",v[2]);else if(v[1]=="expires")expires=number("db0_expires",v[2]);else if(v[1]=="avg_ttl")avg=number("db0_avg_ttl",v[2])}if(!seen["db0_keys"] || !seen["db0_expires"] || !seen["db0_avg_ttl"])invalid=1}
    END{if(invalid || !seen["used_memory"] || !seen["maxmemory"] || !seen["used_memory_dataset"] || !seen["mem_clients_normal"] || !seen["mem_clients_slaves"] || !seen["connected_clients"])print "unreachable";else print u" "x" "d" "c" "n" "policy" "keys" "expires" "avg" "evicted" "exceeded" "errors" "oom" "rss}')"
done`, strings.Join(portNames, " "))
		out, err := env.runner.shell(ctx, h, script)
		if err != nil {
			failedHosts = append(failedHosts, failedHost{host: h, err: err})
			continue
		}
		observedHosts = append(observedHosts, parseRedisHostMemory(h, out))
	}
	if len(observedHosts) == 0 {
		return nil, failedHosts[0].err
	}

	nodeMems := []redisNodeMem{}
	unreachablePorts := []string{}
	completeInfo := len(failedHosts) == 0
	for _, observed := range observedHosts {
		nodeMems = append(nodeMems, observed.nodes...)
		unreachablePorts = append(unreachablePorts, observed.unreachable...)
		completeInfo = completeInfo && len(observed.unreachable) == 0 && !observed.invalidRows
	}
	if len(nodeMems) == 0 {
		return nil, fmt.Errorf("no node INFO parsed (unreachable: %v)", unreachablePorts)
	}

	// fleet medians for the skew checks, across every host's nodes
	usedValues := make([]float64, 0, len(nodeMems))
	connectedValues := make([]float64, 0, len(nodeMems))
	for _, n := range nodeMems {
		usedValues = append(usedValues, n.usedBytes)
		connectedValues = append(connectedValues, n.connectedClients)
	}
	sort.Float64s(usedValues)
	sort.Float64s(connectedValues)
	medianUsed := usedValues[len(usedValues)/2]
	medianConnected := connectedValues[len(connectedValues)/2]
	// Missing or ambiguous nodes cannot establish a fleet median.
	if env.baseline != nil && completeInfo {
		env.baseline.record("redis/used-median", time.Now(), medianUsed)
		env.baseline.record("redis/clients-median", time.Now(), medianConnected)
	}

	findings := []finding{}
	for _, observed := range observedHosts {
		// Aggregate ceilings need every configured node; the observation gap
		// is reported below without clearing the previous capacity finding.
		if len(observed.nodes) == 0 || len(observed.unreachable) != 0 || observed.invalidRows {
			continue
		}
		findings = append(findings, redisHostCapacityFinding(observed))
	}
	for _, failed := range failedHosts {
		// the memory signal surfaces sweep visibility under redis/node-mem
		unknown := cannotObserveFinding(failed.host.name+"/node-mem", failed.err)
		unknown.probeId = "redis/node-mem"
		findings = append(findings, unknown)
	}
	for _, observed := range observedHosts {
		h := observed.host
		for _, n := range observed.nodes {
			target := fmt.Sprintf("%s:%d", h.name, n.port)
			usedPct := 0.0
			if n.maxmemoryBytes > 0 {
				usedPct = 100 * n.usedBytes / n.maxmemoryBytes
			}

			switch {
			case usedPct > 92:
				self.rearmBattery("redis/node-mem-high", "node-mem-high/"+target)
				mechanism := "Redis dataset memory, rather than client output buffers, is consuming the node's maxmemory headroom. At the volatile-ttl wall Redis can evict only keys carrying TTLs; if that candidate set cannot keep pace, writes fail while reads can remain healthy."
				context := "Evicted-key and error counters are cumulative boot totals, so a single sample does not prove a current error rate. current_eviction_exceeded_time distinguishes a node presently unable to stay below maxmemory; task/log signals remain authoritative for a current OOM response."
				action := "Use the key-family and TTL probes to identify the growing dataset before changing capacity. If writes are currently failing and host RAM permits, a documented temporary maxmemory increase can create drain headroom; otherwise do not raise maxmemory to conceal a live leak."
				verify := "Used memory returns below 85%, current_eviction_exceeded_time is zero, current OOM/error rates remain zero, and the attributed key family either drains or stays bounded on consecutive samples."
				playbook := "SIGNALS.md §3.3b and §5.4"
				if n.averageTTLMillis > float64(redisImpossibleAverageTTL.Milliseconds()) {
					mechanism = "Redis dataset memory, rather than client output buffers, is consuming the node's maxmemory headroom. Its impossible average TTL independently proves stale expiry metadata exists, but an average TTL does not measure the residue's bytes and therefore cannot identify it as the capacity root cause."
					context += " Correlate ttl-leaks for expiry correctness and redis-bytes for memory ownership. A bounded production sample found the known stream residue real but tiny while caller-scoped score blobs owned nearly all sampled bytes; keep those diagnoses separate."
					action = "Use redis-bytes to remove the measured dominant footprint and restore headroom. Handle an independently attributed stream TTL cleanup only with explicit maintenance authority; do not count it as material capacity relief without MEMORY USAGE evidence, delete keys through shell text, or raise maxmemory to hide either defect."
					verify = "The measured dominant byte family drains, used memory falls below 85%, eviction pressure decays, and OOM/error rates stay flat; separately, any authorized TTL cleanup clamps only attributed stream keys and returns average TTL below two years."
					playbook = "SIGNALS.md §3.3a, §3.3b, and §5.4"
				}
				nonExpiringKeys := max(0, int(n.keys-n.expiringKeys))
				findings = append(findings, finding{
					probeId: "redis/node-mem-critical", tier: tierPage,
					class: "node-mem-critical", target: target, sustain: 1,
					symptom:   fmt.Sprintf("redis node %d at %.0f%% of maxmemory (page > 92%%)", n.port, usedPct),
					mechanism: mechanism,
					baseline:  "fleet baseline 3–8G used and below 85%; average TTL below two years; no current maxmemory-exceeded interval or OOM replies",
					observed: fmt.Sprintf("used=%.2fG max=%.2fG dataset=%.2fG clients=%.2fG connected=%.0f policy=%s keys=%.0f expiring_keys=%.0f non_expiring_keys=%d avg_ttl_days=%.0f evicted_keys_total=%.0f current_eviction_exceeded_time=%.0f total_error_replies=%.0f oom_errors_total=%.0f",
						gb(n.usedBytes), gb(n.maxmemoryBytes), gb(n.datasetBytes), gb(n.clientsBytes), n.connectedClients,
						n.maxmemoryPolicy, n.keys, n.expiringKeys, nonExpiringKeys, n.averageTTLMillis/float64((24*time.Hour).Milliseconds()), n.evictedKeys, n.currentEvictionExceededTime, n.totalErrorReplies, n.oomErrors),
					evidence: self.batteryEvidence("redis/node-mem-critical", "node-mem-critical/"+target, func() string {
						return redisNodeBattery(ctx, env, h, n.port)
					}),
					context:  context,
					action:   action,
					verify:   verify,
					playbook: playbook,
				})
			case usedPct > 85:
				self.rearmBattery("redis/node-mem-critical", "node-mem-critical/"+target)
				findings = append(findings, finding{
					probeId: "redis/node-mem-high", tier: tierWarn,
					class: "node-mem-high", target: target, sustain: 2,
					symptom:  fmt.Sprintf("redis node %d at %.0f%% of maxmemory (warn > 85%%)", n.port, usedPct),
					baseline: "fleet baseline well under 85%; sustained growth = un-drained pile or missing ttl (3.1/3.3)",
					observed: fmt.Sprintf("used=%.2fG max=%.2fG dataset=%.2fG clients=%.2fG", gb(n.usedBytes), gb(n.maxmemoryBytes), gb(n.datasetBytes), gb(n.clientsBytes)),
					evidence: self.batteryEvidence("redis/node-mem-high", "node-mem-high/"+target, func() string {
						return redisNodeBattery(ctx, env, h, n.port)
					}),
					playbook: "SIGNALS.md 5.4",
				})
			default:
				self.rearmBattery("redis/node-mem-high", "node-mem-high/"+target)
				self.rearmBattery("redis/node-mem-critical", "node-mem-critical/"+target)
				findings = append(findings, healthyFinding("redis/node-mem-high", tierWarn, "node-mem-high", target))
				findings = append(findings, healthyFinding("redis/node-mem-critical", tierPage, "node-mem-critical", target))
			}

			// skew: > 3x fleet median (3.1)
			if completeInfo && medianUsed > 0 && n.usedBytes > 3*medianUsed {
				findings = append(findings, finding{
					probeId: "redis/mem-skew", tier: tierWarn,
					class: "mem-skew", target: target, sustain: 1,
					symptom:  fmt.Sprintf("redis node %d used memory %.2fG is %.1fx the fleet median %.2fG", n.port, gb(n.usedBytes), n.usedBytes/medianUsed, gb(medianUsed)),
					baseline: "all nodes within ~2x of each other; skew = hot key family or un-drained pile (3.1 → 3.3 family histogram)",
					observed: fmt.Sprintf("used=%.2fG median=%.2fG dataset=%.2fG clients=%.2fG", gb(n.usedBytes), gb(medianUsed), gb(n.datasetBytes), gb(n.clientsBytes)),
					playbook: "SIGNALS.md 5.4",
				})
			} else if completeInfo {
				findings = append(findings, healthyFinding("redis/mem-skew", tierWarn, "mem-skew", target))
			}

			// client buffers: > 25% of used or > 2G (3.2 — the pubsub-blowup tell)
			if n.clientsBytes > 2e9 || (n.usedBytes > 0 && n.clientsBytes > 0.25*n.usedBytes) {
				findings = append(findings, finding{
					probeId: "redis/client-buffers", tier: tierWarn,
					class: "client-buffers", target: target, sustain: 1,
					symptom:  fmt.Sprintf("redis node %d client buffers %.2fG (%.0f%% of used) — output-buffer accumulation", n.port, gb(n.clientsBytes), 100*n.clientsBytes/n.usedBytes),
					baseline: "used_memory_clients well under 25% of used_memory and under 2G; growth here = pubsub/slow consumers, not keys (3.2)",
					observed: fmt.Sprintf("clients=%.2fG used=%.2fG dataset=%.2fG", gb(n.clientsBytes), gb(n.usedBytes), gb(n.datasetBytes)),
					evidence: self.batteryEvidence("redis/client-buffers", "client-buffers/"+target, func() string {
						return redisNodeBattery(ctx, env, h, n.port)
					}),
					playbook: "SIGNALS.md 5.5",
				})
			} else {
				self.rearmBattery("redis/client-buffers", "client-buffers/"+target)
				findings = append(findings, healthyFinding("redis/client-buffers", tierWarn, "client-buffers", target))
			}

			// The native spatial arm of §3.5. redisConnectionsProbe combines it
			// with independently timestamped per-node history before reporting health.
			if completeInfo && medianConnected > 0 && n.connectedClients > 3*medianConnected {
				evidence := self.batteryEvidence("redis/clients-spike", "clients-spike/"+target, func() string {
					return redisConnectionBattery(ctx, env, h, n.port)
				})
				diagnosis := diagnoseRedisConnectionSpike(evidence)
				findings = append(findings, finding{
					probeId: "redis/clients-spike", tier: tierWarn,
					class: "clients-spike", target: target, sustain: 2,
					symptom:   fmt.Sprintf("redis node %d connected_clients %.0f is %.1fx the fleet median %.0f", n.port, n.connectedClients, n.connectedClients/medianConnected, medianConnected),
					mechanism: diagnosis.mechanism,
					baseline:  "baseline ~pool_floor x processes, roughly uniform across nodes; +50% step in 10 min = reconnect storm or pool misconfig (3.5)",
					observed:  fmt.Sprintf("connected=%.0f median=%.0f ratio=%.1fx", n.connectedClients, medianConnected, n.connectedClients/medianConnected),
					evidence:  evidence,
					context:   diagnosis.context,
					action:    diagnosis.action,
					verify:    diagnosis.verify,
					playbook:  "SIGNALS.md 3.5",
				})
			} else if completeInfo {
				self.rearmBattery("redis/clients-spike", "clients-spike/"+target)
				findings = append(findings, healthyFinding("redis/clients-spike", tierWarn, "clients-spike", target))
			}
		}

	}

	for _, observed := range observedHosts {
		if len(observed.unreachable) == 0 && !observed.invalidRows {
			continue
		}
		// per-node ping is the tier-0 probe's page; here just visibility
		findings = append(findings, finding{
			probeId: "redis/node-mem", tier: tierWarn,
			class: "cannot-observe", target: observed.host.name,
			frame:     "node-info",
			sustain:   2,
			symptom:   "The configured Redis node INFO census is incomplete or invalid",
			mechanism: "A command failure, missing measurement, or ambiguous node row leaves part of this signal unknown. Valid sibling measurements remain usable for per-node thresholds, but an incomplete census cannot establish fleet medians or aggregate host capacity.",
			observed:  fmt.Sprintf("unreachable_ports=%s invalid_rows=%t", strings.Join(observed.unreachable, ","), observed.invalidRows),
			playbook:  "SIGNALS.md 5.2",
		})
	}

	return findings, nil
}

// redisHostMemory is one redis-cluster host's memory and node observation.
type redisHostMemory struct {
	host           *host
	totalBytes     float64
	availableBytes float64
	nodes          []redisNodeMem
	unreachable    []string
	invalidRows    bool
}

// Admit one finite, nonnegative measurement row per configured port. Missing,
// duplicate, foreign, or malformed rows never become healthy zero samples.
func parseRedisHostMemory(h *host, out string) redisHostMemory {
	nodeMems := []redisNodeMem{}
	unreachablePorts := []string{}
	ports := h.redisNodePorts()
	nodeByPort := make(map[int]redisNodeMem, len(ports))
	seenPorts := make(map[int]bool, len(ports))
	expectedPorts := make(map[int]bool, len(ports))
	for _, port := range ports {
		expectedPorts[port] = true
	}
	invalidRows := false
	parseNumber := func(value string) (float64, bool) {
		number, err := strconv.ParseFloat(value, 64)
		return number, err == nil && number >= 0 && !math.IsNaN(number) && !math.IsInf(number, 0)
	}
	hostMemoryTotalBytes := 0.0
	hostMemoryAvailableBytes := 0.0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "host_memory" {
			hostMemoryTotalBytes = atof(fields[1])
			hostMemoryAvailableBytes = atof(fields[2])
			continue
		}
		if len(fields) == 0 {
			continue
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || !expectedPorts[port] {
			invalidRows = true
			continue
		}
		if seenPorts[port] {
			delete(nodeByPort, port)
			invalidRows = true
			continue
		}
		seenPorts[port] = true
		if len(fields) != 6 && len(fields) != 14 && len(fields) != 15 {
			invalidRows = invalidRows || len(fields) != 2 || fields[1] != "unreachable"
			continue
		}
		node := redisNodeMem{port: port, maxmemoryPolicy: "unknown"}
		measurements := []*float64{&node.usedBytes, &node.maxmemoryBytes, &node.datasetBytes, &node.clientsBytes, &node.connectedClients}
		valid := true
		for i, measurement := range measurements {
			value, ok := parseNumber(fields[i+1])
			*measurement = value
			valid = valid && ok
		}
		if len(fields) >= 14 {
			node.maxmemoryPolicy = fields[6]
			switch node.maxmemoryPolicy {
			case "unknown", "noeviction", "allkeys-lru", "volatile-lru", "allkeys-random", "volatile-random", "volatile-ttl", "allkeys-lfu", "volatile-lfu":
			default:
				node.maxmemoryPolicy = "unknown"
			}
			measurements = []*float64{&node.keys, &node.expiringKeys, &node.averageTTLMillis, &node.evictedKeys, &node.currentEvictionExceededTime, &node.totalErrorReplies, &node.oomErrors}
			for i, measurement := range measurements {
				value, ok := parseNumber(fields[i+7])
				*measurement = value
				valid = valid && ok
			}
		}
		if len(fields) == 15 {
			value, ok := parseNumber(fields[14])
			node.rssBytes = value
			valid = valid && ok
		}
		if valid {
			nodeByPort[port] = node
		} else {
			invalidRows = true
		}
	}
	for _, port := range ports {
		if node, ok := nodeByPort[port]; ok {
			nodeMems = append(nodeMems, node)
		} else {
			unreachablePorts = append(unreachablePorts, strconv.Itoa(port))
		}
	}
	return redisHostMemory{
		host:           h,
		totalBytes:     hostMemoryTotalBytes,
		availableBytes: hostMemoryAvailableBytes,
		nodes:          nodeMems,
		unreachable:    unreachablePorts,
		invalidRows:    invalidRows,
	}
}

// redisHostCapacityFinding judges one host's aggregate Redis ceilings against
// the RAM that host can still supply.
func redisHostCapacityFinding(observed redisHostMemory) finding {
	h := observed.host
	hostMemoryTotalBytes := observed.totalBytes
	hostMemoryAvailableBytes := observed.availableBytes
	nodeMems := observed.nodes
	sumUsedBytes := 0.0
	sumRSSBytes := 0.0
	sumMaxmemoryBytes := 0.0
	totalKeys := 0.0
	criticalNodeCount := 0
	impossibleTTLNodeCount := 0
	for _, n := range nodeMems {
		sumUsedBytes += n.usedBytes
		sumRSSBytes += n.rssBytes
		sumMaxmemoryBytes += n.maxmemoryBytes
		totalKeys += n.keys
		if n.maxmemoryBytes > 0 && 92 < 100*n.usedBytes/n.maxmemoryBytes {
			criticalNodeCount++
		}
		if n.averageTTLMillis > float64(redisImpossibleAverageTTL.Milliseconds()) {
			impossibleTTLNodeCount++
		}
	}
	remainingConfiguredHeadroom := max(0.0, sumMaxmemoryBytes-sumUsedBytes)
	operationalReserve := max(redisHostCapacityReserveMinimum, redisHostCapacityReserveRatio*hostMemoryTotalBytes)
	requiredAvailable := remainingConfiguredHeadroom + operationalReserve
	if criticalNodeCount > 0 && hostMemoryAvailableBytes > 0 && hostMemoryAvailableBytes < requiredAvailable {
		capacityDeficit := requiredAvailable - hostMemoryAvailableBytes
		action := "Do not increase maxmemory on this host. Add physical memory or Redis masters, or reduce the retained dataset, before expanding aggregate ceilings; preserve enough reserve for Redis RSS overhead, the kernel, and non-Redis processes."
		if impossibleTTLNodeCount > 0 {
			action = "Do not increase maxmemory on this host. Create immediate capacity headroom with additional RAM, Redis masters on additional hosts, or a smaller retained key footprint, preserving reserve for Redis RSS overhead, the kernel, and non-Redis processes. Treat the independently attributed impossible-TTL residue as a separate correctness cleanup: measure its bytes before counting it as capacity relief, and do not assume that clamping a small residue resolves this deficit."
		}
		return finding{
			probeId: "redis/host-capacity", tier: tierPage,
			class: "redis-host-capacity", target: h.name, frame: "aggregate-maxmemory", sustain: 1,
			symptom:   fmt.Sprintf("Redis can grow %.1fGiB to its configured ceilings and needs %.1fGiB reserve, but %s has only %.1fGiB available", gib(remainingConfiguredHeadroom), gib(operationalReserve), h.name, gib(hostMemoryAvailableBytes)),
			mechanism: "The sum of per-node maxmemory ceilings plus the host's operational reserve exceeds the RAM the host can still supply. Each Redis process can remain below its own limit while their combined RSS exhausts host memory first; at the ceilings, a missing reserve can still turn controlled per-node eviction into host swapping or an OOM kill.",
			baseline:  "Host-available RAM exceeds all remaining configured Redis growth plus an explicit reserve for RSS overhead, kernel memory, and non-Redis processes; no node is above 92%.",
			observed: fmt.Sprintf("host_total_gib=%.1f host_available_gib=%.1f redis_used_gib=%.1f redis_rss_gib=%.1f aggregate_maxmemory_gib=%.1f remaining_configured_headroom_gib=%.1f operational_reserve_gib=%.1f required_available_gib=%.1f capacity_deficit_gib=%.1f nodes=%d critical_nodes=%d total_keys=%.0f impossible_ttl_nodes=%d",
				gib(hostMemoryTotalBytes), gib(hostMemoryAvailableBytes), gib(sumUsedBytes), gib(sumRSSBytes), gib(sumMaxmemoryBytes), gib(remainingConfiguredHeadroom), gib(operationalReserve), gib(requiredAvailable), gib(capacityDeficit), len(nodeMems), criticalNodeCount, totalKeys, impossibleTTLNodeCount),
			context:  "MemAvailable already includes reclaimable cache. The operational reserve is the larger of 8GiB or 2% of physical RAM. The comparison is intentionally conservative because remaining maxmemory excludes future RSS-over-used variation and every non-Redis allocation; unused swap is not healthy Redis capacity.",
			action:   action,
			verify:   "Aggregate Redis ceilings plus measured overhead fit beneath physical RAM with operational reserve, host swap and memory pressure remain zero, every node returns below 85%, and current OOM/error rates remain zero on consecutive samples.",
			playbook: "SIGNALS.md §3.1, §3.3a, §3.3b, and §5.4",
		}
	}
	return healthyFinding("redis/host-capacity", tierPage, "redis-host-capacity", h.name)
}

// redisTopologyProbe is SIGNALS.md 3.6: phantom entries, replica count, and
// the node inventory against the cluster's membership.
type redisTopologyProbe struct{}

func (self redisTopologyProbe) id() string             { return "redis/topology" }
func (self redisTopologyProbe) tier() string           { return tierWarn }
func (self redisTopologyProbe) cadence() time.Duration { return time.Hour }

func (self redisTopologyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.redisEntryHost()
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	out, err := env.runner.redis(ctx, h, h.redisEntryPort, "CLUSTER", "NODES")
	if err != nil {
		return nil, err
	}
	phantomCount := 0
	replicaCount := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "noaddr") || strings.Contains(line, " :0@0 ") || strings.HasPrefix(line, ":0") {
			phantomCount += 1
			continue
		}
		if strings.Contains(line, "slave") {
			replicaCount += 1
		}
	}

	findings := []finding{}
	if phantomCount > 0 {
		findings = append(findings, finding{
			probeId: "redis/phantom-nodes", tier: tierWarn,
			class: "phantom-nodes", target: h.name, sustain: 1,
			symptom:  fmt.Sprintf("%d phantom (noaddr/:0) entries in cluster nodes", phantomCount),
			baseline: "0 phantoms (purged 2026-07-17); phantoms break every iterate-the-cluster tool (3.6)",
			observed: fmt.Sprintf("phantoms=%d", phantomCount),
			context:  "purge: per-node CLUSTER FORGET loop — each node forgets the ids in its own noaddr list",
			playbook: "SIGNALS.md 3.6",
		})
	} else {
		findings = append(findings, healthyFinding("redis/phantom-nodes", tierWarn, "phantom-nodes", h.name))
	}

	// replica-cover is armed by the inventory's explicit expected count, a
	// cluster-wide setting read from the entry host. Zero remains today's
	// documented dark state; once replicas are configured, a later loss
	// becomes observable without a code change.
	if env.baseline != nil {
		env.baseline.record("redis/replica-count", time.Now(), float64(replicaCount))
	}
	if replicaCount < h.redisExpectedReplicas {
		findings = append(findings, finding{
			probeId: "redis/replica-cover", tier: tierWarn,
			class: "replica-cover", target: h.name, sustain: 2,
			symptom:   fmt.Sprintf("Redis cluster has %d replica node(s), expected at least %d", replicaCount, h.redisExpectedReplicas),
			mechanism: "One or more configured replicas left cluster membership, reducing or eliminating automatic failover coverage.",
			baseline:  fmt.Sprintf("replica_count >= %d from monitor inventory", h.redisExpectedReplicas),
			observed:  fmt.Sprintf("replica_count=%d expected=%d", replicaCount, h.redisExpectedReplicas),
			action:    "Identify the missing replica IDs and hosts in CLUSTER NODES; restore membership without promoting or forgetting nodes blindly.",
			verify:    "CLUSTER NODES reports the configured replica count and every replica has a healthy master relationship.",
			playbook:  "SIGNALS.md §3.6",
		})
	} else {
		findings = append(findings, healthyFinding("redis/replica-cover", tierWarn, "replica-cover", h.name))
	}

	findings = append(findings, redisNodeInventoryFindings(env.cfg, h, out)...)
	return findings, nil
}

// redisClusterMember is one CLUSTER NODES entry that carries an address: the
// IP it announces (empty if the node never learned one) and its client port.
type redisClusterMember struct {
	address string
	port    int
}

// parseRedisClusterMembers reads the addressed members of a CLUSTER NODES
// reply. Phantom entries (noaddr, or port 0) belong to the phantom check and
// handshake entries have not joined, so neither is a member. A line without
// the documented shape fails the whole read: a member it hid would otherwise
// read as a configured node missing from the cluster.
func parseRedisClusterMembers(out string) ([]redisClusterMember, error) {
	members := []redisClusterMember{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// <id> <ip:port@cport[,hostname]> <flags> <master> <ping-sent>
		// <pong-recv> <config-epoch> <link-state> [<slot> ...]
		if len(fields) < 8 {
			return nil, fmt.Errorf("invalid response: CLUSTER NODES line has %d fields", len(fields))
		}
		flags := strings.Split(fields[2], ",")
		if slices.Contains(flags, "noaddr") || slices.Contains(flags, "handshake") {
			continue
		}
		address, _, _ := strings.Cut(fields[1], "@")
		separator := strings.LastIndex(address, ":")
		if separator < 0 {
			return nil, fmt.Errorf("invalid response: CLUSTER NODES address without a port")
		}
		port, err := strconv.Atoi(address[separator+1:])
		if err != nil || port < 0 || 65535 < port {
			return nil, fmt.Errorf("invalid response: CLUSTER NODES address with an invalid port")
		}
		if port == 0 {
			continue
		}
		members = append(members, redisClusterMember{address: redisInventoryAddress(address[:separator]), port: port})
	}
	return members, nil
}

// redisInventoryAddress compares numeric addresses by value.
func redisInventoryAddress(address string) string {
	address = strings.Trim(strings.TrimSpace(address), "[]")
	if parsed, err := netip.ParseAddr(address); err == nil {
		return parsed.Unmap().String()
	}
	return address
}

// redisPortRanges renders ports as sorted inclusive ranges, e.g. 6380-6411,6428.
func redisPortRanges(ports []int) string {
	sorted := slices.Compact(slices.Sorted(slices.Values(ports)))
	if len(sorted) == 0 {
		return "none"
	}
	ranges := []string{}
	for start := 0; start < len(sorted); {
		end := start
		for end+1 < len(sorted) && sorted[end+1] == sorted[end]+1 {
			end++
		}
		if start == end {
			ranges = append(ranges, strconv.Itoa(sorted[start]))
		} else {
			ranges = append(ranges, fmt.Sprintf("%d-%d", sorted[start], sorted[end]))
		}
		start = end + 1
	}
	return strings.Join(ranges, ",")
}

// redisNodeInventoryFindings compares the cluster's membership, read through
// the entry host, with the monitor.yml node inventory. Settings validation can
// only check the inventory against itself, and two broken layouts pass it: a
// host whose redis block is missing silently drops its nodes from every
// per-node probe, and a node_ports range that covers another host's node
// ports still answers on a host that bridges them (edge-6's nginx forwards
// 6412-6427 to edge-5), so the other host's nodes are read as this host's.
// The cluster is the authority: every member must announce the LAN address
// (config settings.yml routes) of the redis-cluster host that lists its port,
// and every listed port must be such a member.
func redisNodeInventoryFindings(cfg *monitorConfig, entryHost *host, clusterNodes string) []finding {
	members, err := parseRedisClusterMembers(clusterNodes)
	if err == nil && len(members) == 0 {
		err = fmt.Errorf("invalid response: CLUSTER NODES listed no addressed member")
	}
	if err != nil {
		return []finding{cannotObserveFinding(entryHost.name+"/node-inventory", err)}
	}

	// Members are attributed by address. The redis-cluster hosts own their LAN
	// addresses before any other inventory host, which only names an address.
	redisHosts := cfg.hostsWithRole("redis-cluster")
	addressHosts := map[string]*host{}
	unknown := []finding{}
	for _, h := range redisHosts {
		address := redisInventoryAddress(h.lanIp)
		if address == "" {
			if 0 < len(h.redisNodePorts()) {
				unattributed := cannotObserveFinding(h.name+"/node-inventory", fmt.Errorf("no LAN address"))
				unattributed.symptom = fmt.Sprintf("The monitor cannot compare %s's Redis node inventory with CLUSTER NODES: the host has no LAN address in config settings.yml routes", h.name)
				unattributed.action = "Restore the host's route in config/<env>/settings.yml (or its monitor.yml lan_ip), then rerun redis-topology."
				unknown = append(unknown, unattributed)
			}
			continue
		}
		if owner, ok := addressHosts[address]; ok {
			shared := cannotObserveFinding(h.name+"/node-inventory", fmt.Errorf("LAN address shared with %s", owner.name))
			shared.symptom = fmt.Sprintf("The monitor cannot attribute Redis cluster members to %s: %s resolves to the same LAN address %s", h.name, owner.name, address)
			shared.action = "Correct the routes in config/<env>/settings.yml (or the monitor.yml lan_ip overrides) so every redis-cluster host has its own LAN address, then rerun redis-topology."
			unknown = append(unknown, shared)
			continue
		}
		addressHosts[address] = h
	}
	if len(unknown) > 0 {
		return unknown
	}
	for _, h := range cfg.hosts {
		if address := redisInventoryAddress(h.lanIp); address != "" && addressHosts[address] == nil {
			addressHosts[address] = h
		}
	}

	memberPorts := map[string]map[int]bool{}
	portAddresses := map[int][]string{}
	addresses := []string{}
	for _, member := range members {
		if memberPorts[member.address] == nil {
			memberPorts[member.address] = map[int]bool{}
			addresses = append(addresses, member.address)
		}
		if !memberPorts[member.address][member.port] {
			memberPorts[member.address][member.port] = true
			portAddresses[member.port] = append(portAddresses[member.port], member.address)
		}
	}
	sort.Strings(addresses)
	addressName := func(address string) string {
		if address == "" {
			return "no address"
		}
		if owner := addressHosts[address]; owner != nil {
			return address + " (" + owner.name + ")"
		}
		return address
	}

	findings := []finding{}
	for _, h := range redisHosts {
		address := redisInventoryAddress(h.lanIp)
		if address == "" {
			// lists no node port (or the comparison stopped above), and owns
			// no address a member could announce
			continue
		}
		configured := map[int]bool{}
		for _, port := range h.redisNodePorts() {
			configured[port] = true
		}
		missing := []int{}
		elsewhere := map[string][]int{}
		for port := range configured {
			if memberPorts[address][port] {
				continue
			}
			missing = append(missing, port)
			for _, other := range portAddresses[port] {
				elsewhere[other] = append(elsewhere[other], port)
			}
		}
		unlisted := []int{}
		for port := range memberPorts[address] {
			if !configured[port] {
				unlisted = append(unlisted, port)
			}
		}
		if len(missing) == 0 && len(unlisted) == 0 {
			findings = append(findings, healthyFinding("redis/node-inventory", tierWarn, "node-inventory-mismatch", h.name))
			continue
		}

		clauses := []string{}
		if 0 < len(unlisted) {
			clauses = append(clauses, fmt.Sprintf("cluster members on port(s) %s announce its LAN address %s but are not in its redis.node_ports", redisPortRanges(unlisted), address))
		}
		if 0 < len(missing) {
			clause := fmt.Sprintf("its redis.node_ports %s are not cluster members at its LAN address %s", redisPortRanges(missing), address)
			otherAddresses := slices.Sorted(maps.Keys(elsewhere))
			announced := []string{}
			for _, other := range otherAddresses {
				announced = append(announced, fmt.Sprintf("%s at %s", redisPortRanges(elsewhere[other]), addressName(other)))
			}
			if 0 < len(announced) {
				clause += "; the cluster announces " + strings.Join(announced, " and ")
			}
			clauses = append(clauses, clause)
		}
		observed := fmt.Sprintf("host=%s lan_address=%s configured_node_ports=%s member_ports=%s unlisted_member_ports=%s missing_node_ports=%s",
			h.name, address, redisPortRanges(h.redisNodePorts()), redisPortRanges(slices.Collect(maps.Keys(memberPorts[address]))), redisPortRanges(unlisted), redisPortRanges(missing))
		for _, other := range slices.Sorted(maps.Keys(elsewhere)) {
			observed += fmt.Sprintf(" missing_ports_announced_elsewhere=%s@%s", redisPortRanges(elsewhere[other]), other)
		}
		findings = append(findings, redisNodeInventoryMismatch(h.name,
			fmt.Sprintf("Redis node inventory for %s disagrees with CLUSTER NODES: %s", h.name, strings.Join(clauses, "; ")),
			observed))
	}

	// members at an address that no redis-cluster host owns
	for _, address := range addresses {
		owner := addressHosts[address]
		if owner != nil && owner.hasRole("redis-cluster") {
			continue
		}
		ports := slices.Collect(maps.Keys(memberPorts[address]))
		target := address
		symptom := fmt.Sprintf("CLUSTER NODES members on port(s) %s announce %s, which is no monitor.yml host's LAN address, so no per-node probe observes them", redisPortRanges(ports), address)
		if address == "" {
			target = "unannounced-address"
			symptom = fmt.Sprintf("CLUSTER NODES members on port(s) %s announce no address, so no monitor.yml host can be matched to them", redisPortRanges(ports))
		} else if owner != nil {
			target = owner.name
			symptom = fmt.Sprintf("CLUSTER NODES members on port(s) %s announce %s, the LAN address of %s, which monitor.yml does not give the redis-cluster role, so no per-node probe observes them", redisPortRanges(ports), address, owner.name)
		}
		findings = append(findings, redisNodeInventoryMismatch(target, symptom,
			fmt.Sprintf("announced_address=%s member_ports=%s", addressName(address), redisPortRanges(ports))))
	}
	return findings
}

// redisNodeInventoryMismatch is one host's, or one unowned address's,
// disagreement between monitor.yml and the cluster's membership.
func redisNodeInventoryMismatch(target, symptom, observed string) finding {
	return finding{
		probeId: "redis/node-inventory", tier: tierWarn,
		class: "node-inventory-mismatch", target: target, sustain: 1,
		symptom:   symptom,
		mechanism: "Per-node Redis probes sweep each redis-cluster host's monitor.yml node_ports over that host's loopback. A cluster member missing from the inventory is never observed for memory, keyspace, latency, or liveness. A listed port the host does not run either fails each sweep or, on a host that bridges other hosts' node ports, answers with another host's node, whose memory is then judged against this host's RAM and whose findings target this host.",
		baseline:  "Every addressed CLUSTER NODES member (phantom and handshake entries excluded) announces the LAN address, from config settings.yml routes, of the redis-cluster host whose redis.node_ports lists its port, and every listed node port is such a member.",
		observed:  observed,
		action:    "Correct vault/<env>/monitor.yml so each redis-cluster host's redis.node_ports lists exactly the members announced at its LAN address. If the cluster itself is wrong (a node announcing the wrong address, a forgotten node, or an unfinished add-node), repair the cluster instead of widening the inventory.",
		verify:    "redis-topology reports every member on the host that lists it, with no node-inventory-mismatch, and the per-node Redis signals cover every member on its own host.",
		playbook:  "SIGNALS.md §3.6",
	}
}
