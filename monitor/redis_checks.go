// Shared Redis snapshot collection and evaluation used by redis-memory,
// redis-buffers, redis-connections, and redis-topology.
package monitor

import (
	"context"
	"fmt"
	"sort"
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

type redisConnectionDiagnosis struct {
	mechanism string
	context   string
	action    string
	verify    string
}

func diagnoseRedisConnectionSpike(evidence string) redisConnectionDiagnosis {
	lowerEvidence := strings.ToLower(evidence)
	if strings.Contains(lowerEvidence, "queried_node_owns_reliability_marker=true") &&
		strings.Contains(lowerEvidence, "cmd=sadd") {
		return redisConnectionDiagnosis{
			mechanism: "A Redis cluster client creates and retains a per-node pool when a process first touches that node. The SADD cohort identifies the legacy client_reliability_stats_blocks fixed-slot key writer: one fleet-wide marker key concentrates every writer process's pool on its owner.",
			context:   "Dominant long-lived normal-client cohorts ending in SADD/EXPIRE identify the legacy client_reliability_stats_blocks fixed-slot variant; the connection count is pool amplification, not a slow Redis process or a reconnect storm.",
			action:    "Roll out the marker-free reliability writer after the compatible high-water-mark rollup, then restart writers normally so old pools age out. Do not raise pool floors or kill a healthy Redis owner; those actions multiply or churn the connections.",
			verify:    "The outlier returns near the fleet median after writer rollout/restarts, the SADD/EXPIRE cohorts disappear, and Redis pool-timeout plus node-latency signals remain healthy.",
		}
	}
	return redisConnectionDiagnosis{
		mechanism: "Connected clients are concentrated on this Redis node, but its bounded battery does not prove the reliability-marker fingerprint (the node must own client_reliability_stats_blocks and expose a SADD cohort). Long-lived PING/GET/EXEC cohorts can be normal lazy cluster pools on an actively used slot; uniformly young NULL/PING cohorts instead indicate reconnect churn, while abnormal flags or output memory indicate a stalled consumer.",
		context:   "A fleet-median ratio identifies shape, not root cause. Attribute this node's command rate and hot key slots, compare cohort ages and source processes, and read node latency before treating the sockets as harmful.",
		action:    "If latency and pool-timeout signals are healthy with long-lived normal-client cohorts, observe consecutive ticks and fix only the workload or pool ownership that explains the hot slots. For uniformly young cohorts, investigate the matching deploy/reconnect boundary; for abnormal flags or output memory, follow the slow-consumer playbook. Do not apply the reliability-marker rollout solely from this alert.",
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
			action = "Do not increase maxmemory on this host. Create immediate capacity headroom with additional RAM, Redis masters on additional hosts, or a smaller retained key footprint, preserving reserve for Redis RSS overhead, the kernel, and non-Redis processes. With explicit maintenance authority, also run the independently attributed binary-safe `bringyourctl streams expire-leaked-ttls` cleanup: it clamps leaked keys to an 8-hour TTL and starts a bounded drain, but does not create immediate host capacity."
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
			playbook: "SIGNALS.md §3.1, §3.3a, and §5.4",
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
			playbook := "SIGNALS.md §5.4"
			if n.averageTTLMillis > float64(redisImpossibleAverageTTL.Milliseconds()) {
				mechanism = "Redis dataset memory, rather than client output buffers, is consuming the node's maxmemory headroom, and its impossible average TTL proves stale expiry residue is preventing natural drain. The known duration-as-nanoseconds stream keys carry effectively immortal TTLs; volatile-ttl may evict some under pressure, but ordinary expiry cannot reclaim them on an operational timescale."
				context += " Correlate the fleet-wide ttl-leaks signal's binary-safe family sample before applying its family-specific cleanup; an impossible average alone does not authorize changing arbitrary keys."
				action = "Confirm current writers emit no stream-family TTL warnings and the ttl-leaks sample attributes the residue to legacy/current stream keys. With explicit maintenance authority, run the binary-safe `expire-leaked-ttls` cleanup; do not delete keys through shell text, raise maxmemory to hide the residue, or run a family-specific cleanup against unknown keys."
				verify = "The cleanup clamps only attributed stream keys, average TTL returns below two years, used memory falls below 85%, eviction pressure decays, and no new stream-family TTL warnings or OOM replies appear."
				playbook = "SIGNALS.md §3.3a and §5.4"
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
