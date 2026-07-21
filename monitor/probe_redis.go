// redis tier-0 probe: cluster state + per-node liveness (SIGNALS.md 1.4).
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// redisClusterProbe is SIGNALS.md 1.4: cluster state + per-node liveness. With
// cluster-require-full-coverage=no, a single dead shard degrades 1/32 of keys
// without flipping cluster_state elsewhere, so the monitor must check per-node
// ping, not just cluster_state. A wedged node hangs rather than erroring, so
// each ping carries a hard timeout.
type redisClusterProbe struct{}

func (self redisClusterProbe) id() string             { return "redis/cluster" }
func (self redisClusterProbe) tier() string           { return tierPage }
func (self redisClusterProbe) cadence() time.Duration { return 60 * time.Second }

func (self redisClusterProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	findings := []finding{}

	// cluster_state / slots_fail / known_nodes from the entry port
	info, err := env.runner.redis(ctx, h, h.redisEntryPort, "CLUSTER", "INFO")
	if err != nil {
		return nil, err
	}
	kv := parseRedisInfo(info)
	state := kv["cluster_state"]
	slotsFail := kv["cluster_slots_fail"]
	knownNodes := kv["cluster_known_nodes"]

	if state != "ok" || slotsFail != "0" {
		findings = append(findings, finding{
			probeId: "redis/cluster-state", tier: tierPage,
			class: "cluster-state", target: h.name, sustain: 1,
			symptom:  fmt.Sprintf("redis (%s) cluster_state=%q slots_fail=%s (expected ok / 0)", h.name, state, slotsFail),
			baseline: "cluster_state:ok, slots_fail 0, known_nodes 32 (2026-07-17 phantom purge: 32 masters, 0 replicas)",
			observed: fmt.Sprintf("cluster_state=%s slots_fail=%s known_nodes=%s", state, slotsFail, knownNodes),
			evidence: redisFailingNodes(ctx, env, h),
			playbook: "SIGNALS.md 5.3",
		})
	} else {
		findings = append(findings, healthyFinding("redis/cluster-state", tierPage, "cluster-state", h.name))
	}

	// per-node ping sweep in a single shell loop (a wedged node hangs; the
	// remote `timeout 2` bounds each ping)
	ports := h.redisNodePorts()
	if len(ports) > 0 {
		lo, hi := ports[0], ports[len(ports)-1]
		loop := fmt.Sprintf(
			"for p in $(seq %d %d); do timeout 2 redis-cli -p $p PING >/dev/null 2>&1 || echo $p; done",
			lo, hi,
		)
		out, err := env.runner.shell(ctx, h, loop)
		if err != nil {
			return findings, err
		}
		deadPorts := strings.Fields(out)
		if len(deadPorts) > 0 {
			// 5.2 evidence for the first dead node (bounded: one battery/trip)
			evidence := ""
			if p := atoi(deadPorts[0]); p > 0 {
				evidence = redisNodeBattery(ctx, env, p)
			}
			findings = append(findings, finding{
				probeId: "redis/node-unreachable", tier: tierPage,
				class: "node-unreachable", target: h.name,
				frame:   strings.Join(deadPorts, ","),
				sustain: 1,
				symptom: fmt.Sprintf("redis (%s) %d node(s) not responding to ping: ports %s",
					h.name, len(deadPorts), strings.Join(deadPorts, ",")),
				baseline: "every node ping < 100ms; a timeout = event-loop wedge (the #1 recurring failure)",
				observed: fmt.Sprintf("dead_ports=%s of range %d-%d", strings.Join(deadPorts, ","), lo, hi),
				evidence: evidence,
				playbook: "SIGNALS.md 5.2",
			})
		} else {
			findings = append(findings, healthyFinding("redis/node-unreachable", tierPage, "node-unreachable", h.name))
		}
	}

	return findings, nil
}

// redisFailingNodes lists nodes marked fail/noaddr (evidence for cluster-state).
func redisFailingNodes(ctx context.Context, env *probeEnv, h *host) string {
	out, err := env.runner.redis(ctx, h, h.redisEntryPort, "CLUSTER", "NODES")
	if err != nil {
		return "cluster nodes failed: " + err.Error()
	}
	badAddrs := []string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "fail") || strings.Contains(line, "noaddr") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				badAddrs = append(badAddrs, fields[1])
			}
		}
	}
	if len(badAddrs) == 0 {
		return "no fail/noaddr nodes in cluster nodes"
	}
	return "fail/noaddr nodes: " + strings.Join(badAddrs, ", ")
}

// parseRedisInfo parses "key:value" lines from INFO / CLUSTER INFO output.
func parseRedisInfo(info string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, ':'); i > 0 {
			kv[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}
	return kv
}
