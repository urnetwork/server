// Shared Redis snapshot collection and evaluation used by redis-memory,
// redis-buffers, redis-connections, and redis-topology.
package monitor

import (
	"context"
	"fmt"
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

func fullestRedisNodeUsage(ctx context.Context, env *probeEnv, h *host) (redisNodeUsage, error) {
	ports := h.redisNodePorts()
	if len(ports) == 0 {
		return redisNodeUsage{}, fmt.Errorf("no redis node ports configured")
	}
	portNames := make([]string, len(ports))
	for i, port := range ports {
		portNames[i] = fmt.Sprint(port)
	}
	out, err := env.runner.shell(ctx, h, fmt.Sprintf(`for p in %s; do
  m=$(timeout 3 redis-cli -p $p INFO memory 2>/dev/null | tr -d '\r')
  [ -z "$m" ] && continue
  echo "$p $(echo "$m" | awk -F: 'BEGIN{u=0;x=0}/^used_memory:/{u=$2}/^maxmemory:/{x=$2}END{print u" "x}')"
done`, strings.Join(portNames, " ")))
	if err != nil {
		return redisNodeUsage{}, err
	}

	fullest := redisNodeUsage{}
	fullestRatio := -1.0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		candidate := redisNodeUsage{
			port:           atoi(fields[0]),
			usedBytes:      atoi64(fields[1]),
			maxmemoryBytes: atoi64(fields[2]),
		}
		if candidate.port == 0 || candidate.usedBytes <= 0 || candidate.maxmemoryBytes <= 0 {
			continue
		}
		ratio := float64(candidate.usedBytes) / float64(candidate.maxmemoryBytes)
		if fullest.port == 0 || fullestRatio < ratio {
			fullest = candidate
			fullestRatio = ratio
		}
	}
	if fullest.port == 0 {
		return redisNodeUsage{}, fmt.Errorf("could not determine fullest Redis node from %q", strings.TrimSpace(out))
	}
	return fullest, nil
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
		mechanism := fmt.Sprintf("This Redis master owns %d of %d current-minute and %d previous-minute client-reliability shards. Independent shard hashing can place multiple hot hashes on one master; each reliability transaction ends in EXPIRE, and that concentrated command load expands the lazy per-node client pools that touched the master.", currentShards, shardCount, previousShards)
		if !currentCollision && historicalCollision && lookbackKnown && recentMaxAgeKnown {
			mechanism = fmt.Sprintf("This Redis master owns %d of %d current-minute and %d previous-minute client-reliability shards, but owned as many as %d within the bounded %d-block history (%d block(s) ago). Independent shard hashing created that earlier hot-key collision, each reliability transaction ended in EXPIRE, and lazy per-node pools outlived the one-minute key ownership that expanded them.", currentShards, shardCount, previousShards, recentMax, lookbackBlocks, recentMaxAge)
		}
		return redisConnectionDiagnosis{
			mechanism: mechanism,
			context:   appendControlSummary("The EXPIRE cohort and bounded client_reliability_stats.<block>.<shard> ownership history identify a rotating marker-free reliability load collision. This is not the retired fixed discovery-set writer, and CLIENT LIST alone is not evidence of a reconnect storm or Redis impairment."),
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
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	ports := h.redisNodePorts()
	if len(ports) == 0 {
		return nil, fmt.Errorf("no redis node ports configured")
	}
	lo, hi := ports[0], ports[len(ports)-1]

	// one round trip: per node, one compact line. Client-buffer memory is
	// mem_clients_normal + mem_clients_slaves (the INFO fields this redis
	// actually exposes — there is no used_memory_clients key; SIGNALS.md 3.2
	// note). awk defaults keep every field numeric even if a key is absent.
	script := fmt.Sprintf(`awk '/^MemTotal:/{total=$2*1024}/^MemAvailable:/{available=$2*1024}END{print "host_memory",total,available}' /proc/meminfo 2>/dev/null
for p in $(seq %d %d); do
  m=$(timeout 3 redis-cli -p $p INFO 2>/dev/null | tr -d '\r')
  [ -z "$m" ] && { echo "$p unreachable"; continue; }
  echo "$p $(echo "$m" | awk -F: '
    BEGIN{u=0;rss=0;x=0;d=0;c=0;n=0;policy="unknown";keys=0;expires=0;avg=0;evicted=0;exceeded=0;errors=0;oom=0}
    /^used_memory:/{u=$2} /^used_memory_rss:/{rss=$2} /^maxmemory:/{x=$2} /^used_memory_dataset:/{d=$2}
    /^mem_clients_normal:/{c+=$2} /^mem_clients_slaves:/{c+=$2}
    /^connected_clients:/{n=$2}
    /^maxmemory_policy:/{policy=$2}
    /^evicted_keys:/{evicted=$2} /^current_eviction_exceeded_time:/{exceeded=$2}
    /^total_error_replies:/{errors=$2}
    /^errorstat_OOM:/{split($2,a,"=");oom=a[2]+0}
    /^db0:/{split($2,a,",");for(i in a){split(a[i],v,"=");if(v[1]=="keys")keys=v[2];else if(v[1]=="expires")expires=v[2];else if(v[1]=="avg_ttl")avg=v[2]}}
    END{print u" "x" "d" "c" "n" "policy" "keys" "expires" "avg" "evicted" "exceeded" "errors" "oom" "rss}')"
done`, lo, hi)
	out, err := env.runner.shell(ctx, h, script)
	if err != nil {
		return nil, err
	}

	nodeMems := []redisNodeMem{}
	unreachablePorts := []string{}
	hostMemoryTotalBytes := 0.0
	hostMemoryAvailableBytes := 0.0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "host_memory" {
			hostMemoryTotalBytes = atof(fields[1])
			hostMemoryAvailableBytes = atof(fields[2])
			continue
		}
		if len(fields) == 2 && fields[1] == "unreachable" {
			unreachablePorts = append(unreachablePorts, fields[0])
			continue
		}
		if len(fields) < 6 {
			continue
		}
		node := redisNodeMem{
			port:             atoi(fields[0]),
			usedBytes:        atof(fields[1]),
			maxmemoryBytes:   atof(fields[2]),
			datasetBytes:     atof(fields[3]),
			clientsBytes:     atof(fields[4]),
			connectedClients: atof(fields[5]),
		}
		if len(fields) >= 14 {
			node.maxmemoryPolicy = fields[6]
			node.keys = atof(fields[7])
			node.expiringKeys = atof(fields[8])
			node.averageTTLMillis = atof(fields[9])
			node.evictedKeys = atof(fields[10])
			node.currentEvictionExceededTime = atof(fields[11])
			node.totalErrorReplies = atof(fields[12])
			node.oomErrors = atof(fields[13])
		}
		if len(fields) >= 15 {
			node.rssBytes = atof(fields[14])
		}
		nodeMems = append(nodeMems, node)
	}
	if len(nodeMems) == 0 {
		return nil, fmt.Errorf("no node INFO parsed (unreachable: %v)", unreachablePorts)
	}

	// fleet medians for the skew checks
	usedValues := make([]float64, 0, len(nodeMems))
	connectedValues := make([]float64, 0, len(nodeMems))
	sumUsedBytes := 0.0
	sumRSSBytes := 0.0
	sumMaxmemoryBytes := 0.0
	totalKeys := 0.0
	criticalNodeCount := 0
	impossibleTTLNodeCount := 0
	for _, n := range nodeMems {
		usedValues = append(usedValues, n.usedBytes)
		connectedValues = append(connectedValues, n.connectedClients)
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
	sort.Float64s(usedValues)
	sort.Float64s(connectedValues)
	medianUsed := usedValues[len(usedValues)/2]
	medianConnected := connectedValues[len(connectedValues)/2]
	if env.baseline != nil {
		env.baseline.record("redis/used-median", time.Now(), medianUsed)
		env.baseline.record("redis/clients-median", time.Now(), medianConnected)
	}

	findings := []finding{}
	remainingConfiguredHeadroom := max(0.0, sumMaxmemoryBytes-sumUsedBytes)
	operationalReserve := max(redisHostCapacityReserveMinimum, redisHostCapacityReserveRatio*hostMemoryTotalBytes)
	requiredAvailable := remainingConfiguredHeadroom + operationalReserve
	if criticalNodeCount > 0 && hostMemoryAvailableBytes > 0 && hostMemoryAvailableBytes < requiredAvailable {
		capacityDeficit := requiredAvailable - hostMemoryAvailableBytes
		action := "Do not increase maxmemory on this host. Add physical memory or Redis masters, or reduce the retained dataset, before expanding aggregate ceilings; preserve enough reserve for Redis RSS overhead, the kernel, and non-Redis processes."
		if impossibleTTLNodeCount > 0 {
			action = "Do not increase maxmemory on this host. Create immediate capacity headroom with additional RAM, Redis masters on additional hosts, or a smaller retained key footprint, preserving reserve for Redis RSS overhead, the kernel, and non-Redis processes. Treat the independently attributed impossible-TTL residue as a separate correctness cleanup: measure its bytes before counting it as capacity relief, and do not assume that clamping a small residue resolves this deficit."
		}
		findings = append(findings, finding{
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
		})
	} else {
		findings = append(findings, healthyFinding("redis/host-capacity", tierPage, "redis-host-capacity", h.name))
	}
	for _, n := range nodeMems {
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
					return redisNodeBattery(ctx, env, n.port)
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
					return redisNodeBattery(ctx, env, n.port)
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
		if medianUsed > 0 && n.usedBytes > 3*medianUsed {
			findings = append(findings, finding{
				probeId: "redis/mem-skew", tier: tierWarn,
				class: "mem-skew", target: target, sustain: 1,
				symptom:  fmt.Sprintf("redis node %d used memory %.2fG is %.1fx the fleet median %.2fG", n.port, gb(n.usedBytes), n.usedBytes/medianUsed, gb(medianUsed)),
				baseline: "all nodes within ~2x of each other; skew = hot key family or un-drained pile (3.1 → 3.3 family histogram)",
				observed: fmt.Sprintf("used=%.2fG median=%.2fG dataset=%.2fG clients=%.2fG", gb(n.usedBytes), gb(medianUsed), gb(n.datasetBytes), gb(n.clientsBytes)),
				playbook: "SIGNALS.md 5.4",
			})
		} else {
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
					return redisNodeBattery(ctx, env, n.port)
				}),
				playbook: "SIGNALS.md 5.5",
			})
		} else {
			self.rearmBattery("redis/client-buffers", "client-buffers/"+target)
			findings = append(findings, healthyFinding("redis/client-buffers", tierWarn, "client-buffers", target))
		}

		// connected_clients spike vs fleet median (3.5 approximation of the
		// step-change rule until per-node history is kept)
		if medianConnected > 0 && n.connectedClients > 3*medianConnected {
			evidence := self.batteryEvidence("redis/clients-spike", "clients-spike/"+target, func() string {
				return redisConnectionBattery(ctx, env, n.port)
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
		} else {
			self.rearmBattery("redis/clients-spike", "clients-spike/"+target)
			findings = append(findings, healthyFinding("redis/clients-spike", tierWarn, "clients-spike", target))
		}
	}

	if len(unreachablePorts) > 0 {
		// per-node ping is the tier-0 probe's page; here just visibility
		findings = append(findings, finding{
			probeId: "redis/node-mem", tier: tierWarn,
			class: "cannot-observe", target: h.name,
			frame:    strings.Join(unreachablePorts, ","),
			sustain:  2,
			symptom:  fmt.Sprintf("INFO unavailable from %d node(s): %s", len(unreachablePorts), strings.Join(unreachablePorts, ",")),
			observed: fmt.Sprintf("unreachable_ports=%s", strings.Join(unreachablePorts, ",")),
			playbook: "SIGNALS.md 5.2",
		})
	}

	return findings, nil
}

// redisTopologyProbe is SIGNALS.md 3.6: phantom entries and replica count.
type redisTopologyProbe struct{}

func (self redisTopologyProbe) id() string             { return "redis/topology" }
func (self redisTopologyProbe) tier() string           { return tierWarn }
func (self redisTopologyProbe) cadence() time.Duration { return time.Hour }

func (self redisTopologyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.hostByRole("redis-cluster")
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

	// replica-cover is armed by the inventory's explicit expected count. Zero
	// remains today's documented dark state; once replicas are configured, a
	// later loss becomes observable without a code change.
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

	return findings, nil
}
