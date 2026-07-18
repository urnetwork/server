// redis tier-1 probes: per-node memory table / skew / client buffers /
// connection counts (SIGNALS.md 3.1/3.2/3.5) and topology hygiene (3.6).
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// redisMemoryProbe is SIGNALS.md 3.1/3.2/3.5: the per-node memory table (skew
// detector), dataset-vs-clients attribution, and connected_clients. One ssh
// round collects INFO from every node; findings are per-node so each sick node
// gets its own ticket identity.
type redisMemoryProbe struct{}

func (self redisMemoryProbe) id() string             { return "redis/node-mem" }
func (self redisMemoryProbe) tier() string           { return tierWarn }
func (self redisMemoryProbe) cadence() time.Duration { return 5 * time.Minute }

// redisNodeMem is one node's parsed INFO memory/clients numbers.
type redisNodeMem struct {
	port             int
	usedBytes        float64
	maxmemoryBytes   float64
	datasetBytes     float64
	clientsBytes     float64
	connectedClients float64
}

func (self redisMemoryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
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
	script := fmt.Sprintf(`for p in $(seq %d %d); do
  m=$(timeout 3 redis-cli -p $p INFO 2>/dev/null | tr -d '\r')
  [ -z "$m" ] && { echo "$p unreachable"; continue; }
  echo "$p $(echo "$m" | awk -F: '
    BEGIN{u=0;x=0;d=0;c=0;n=0}
    /^used_memory:/{u=$2} /^maxmemory:/{x=$2} /^used_memory_dataset:/{d=$2}
    /^mem_clients_normal:/{c+=$2} /^mem_clients_slaves:/{c+=$2}
    /^connected_clients:/{n=$2}
    END{print u" "x" "d" "c" "n}')"
done`, lo, hi)
	out, err := env.runner.shell(ctx, h, script)
	if err != nil {
		return nil, err
	}

	nodeMems := []redisNodeMem{}
	unreachablePorts := []string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "unreachable" {
			unreachablePorts = append(unreachablePorts, fields[0])
			continue
		}
		if len(fields) < 6 {
			continue
		}
		nodeMems = append(nodeMems, redisNodeMem{
			port:             atoi(fields[0]),
			usedBytes:        atof(fields[1]),
			maxmemoryBytes:   atof(fields[2]),
			datasetBytes:     atof(fields[3]),
			clientsBytes:     atof(fields[4]),
			connectedClients: atof(fields[5]),
		})
	}
	if len(nodeMems) == 0 {
		return nil, fmt.Errorf("no node INFO parsed (unreachable: %v)", unreachablePorts)
	}

	// fleet medians for the skew checks
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
	if env.baseline != nil {
		env.baseline.record("redis/used-median", time.Now(), medianUsed)
		env.baseline.record("redis/clients-median", time.Now(), medianConnected)
	}

	findings := []finding{}
	for _, n := range nodeMems {
		target := fmt.Sprintf("%s:%d", h.name, n.port)
		usedPct := 0.0
		if n.maxmemoryBytes > 0 {
			usedPct = 100 * n.usedBytes / n.maxmemoryBytes
		}

		switch {
		case usedPct > 92:
			findings = append(findings, finding{
				probeId: "redis/node-mem-critical", tier: tierPage,
				class: "node-mem-critical", target: target, sustain: 1,
				symptom:  fmt.Sprintf("redis node %d at %.0f%% of maxmemory (page > 92%%)", n.port, usedPct),
				baseline: "fleet baseline 3–8G used; volatile-ttl means a full node of no-ttl keys rejects all writes while reads work (3.1)",
				observed: fmt.Sprintf("used=%.2fG max=%.2fG dataset=%.2fG clients=%.2fG connected=%.0f", gb(n.usedBytes), gb(n.maxmemoryBytes), gb(n.datasetBytes), gb(n.clientsBytes), n.connectedClients),
				evidence: redisNodeBattery(ctx, env, n.port),
				playbook: "SIGNALS.md 5.4",
			})
		case usedPct > 85:
			findings = append(findings, finding{
				probeId: "redis/node-mem-high", tier: tierWarn,
				class: "node-mem-high", target: target, sustain: 1,
				symptom:  fmt.Sprintf("redis node %d at %.0f%% of maxmemory (warn > 85%%)", n.port, usedPct),
				baseline: "fleet baseline well under 85%; sustained growth = un-drained pile or missing ttl (3.1/3.3)",
				observed: fmt.Sprintf("used=%.2fG max=%.2fG dataset=%.2fG clients=%.2fG", gb(n.usedBytes), gb(n.maxmemoryBytes), gb(n.datasetBytes), gb(n.clientsBytes)),
				playbook: "SIGNALS.md 5.4",
			})
		default:
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
				evidence: redisNodeBattery(ctx, env, n.port),
				playbook: "SIGNALS.md 5.5",
			})
		} else {
			findings = append(findings, healthyFinding("redis/client-buffers", tierWarn, "client-buffers", target))
		}

		// connected_clients spike vs fleet median (3.5 approximation of the
		// step-change rule until per-node history is kept)
		if medianConnected > 0 && n.connectedClients > 3*medianConnected {
			findings = append(findings, finding{
				probeId: "redis/clients-spike", tier: tierWarn,
				class: "clients-spike", target: target, sustain: 2,
				symptom:  fmt.Sprintf("redis node %d connected_clients %.0f is %.1fx the fleet median %.0f", n.port, n.connectedClients, n.connectedClients/medianConnected, medianConnected),
				baseline: "baseline ~pool_floor x processes, roughly uniform across nodes; +50% step in 10 min = reconnect storm or pool misconfig (3.5)",
				observed: fmt.Sprintf("connected=%.0f median=%.0f", n.connectedClients, medianConnected),
				playbook: "SIGNALS.md 3.5",
			})
		} else {
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

	// replica-cover: expected replicas is 0 today (standing risk, documented);
	// record the count so the day replicas are added, a later loss is caught.
	if env.baseline != nil {
		env.baseline.record("redis/replica-count", time.Now(), float64(replicaCount))
	}
	findings = append(findings, healthyFinding("redis/replica-cover", tierWarn, "replica-cover", h.name))

	return findings, nil
}
