// Redis connection shape combines the native fleet comparison with bounded
// same-generation exporter history, retaining unknowns for either source.
package monitor

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SIGNALS.md §3.5 maps to signal_redis_connections.go and signal_redis_connections_test.go.
func NewRedisConnectionsSignal() Signal {
	return &signalAdapter{
		number: "3.5", key: "redis-connections", name: "Redis connection shape",
		probe: &redisConnectionsProbe{
			redisMemoryProbe: newRedisMemoryProbe(), connectionBatteries: newBatteryLatch(), targetRunIds: map[string]string{},
		},
		accept: acceptProbeIDs("redis/clients-spike"),
	}
}

// The shared native reducer supplies the spatial arm with its own batteries
// disabled. This owner collects one battery for the combined condition and
// re-arms only after both arms are healthy or Redis generation changes.
type redisConnectionsProbe struct {
	*redisMemoryProbe
	connectionBatteries *batteryLatch
	stateLock           sync.Mutex
	targetRunIds        map[string]string
}

// Neither a missing history nor a failed native sweep may erase a violation
// established by the other source. Healthy requires both actual comparisons.
func (self *redisConnectionsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	targets := map[string]redisClientsHistoryTarget{}
	for _, h := range env.cfg.redisSweepHosts() {
		for _, port := range h.redisNodePorts() {
			target := fmt.Sprintf("%s:%d", h.name, port)
			if _, duplicate := targets[target]; duplicate || port <= 0 || port > 65535 {
				return nil, fmt.Errorf("ambiguous or invalid Redis node inventory")
			}
			targets[target] = redisClientsHistoryTarget{host: h, port: port}
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no configured Redis nodes for connection observations")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	native, nativeErr := self.redisMemoryProbe.check(ctx, env)
	findings := []finding{}
	spatial := map[string]finding{}
	for _, observed := range native {
		if observed.probeId == "redis/clients-spike" && observed.class == "clients-spike" {
			spatial[observed.target] = observed
		} else {
			findings = append(findings, observed)
		}
	}
	if nativeErr != nil {
		findings = append(findings, cannotObserveFinding("redis-connections/current", nativeErr))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	at := env.now().UTC().Truncate(time.Second)
	history, historyErr := observeRedisClientsHistory(ctx, env, targets, at)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	targetNames := make([]string, 0, len(targets))
	for target := range targets {
		targetNames = append(targetNames, target)
	}
	sort.Strings(targetNames)
	if historyErr != nil {
		// One failed shared query is one source incident, not a separate
		// incident on every node that happens to depend on that query.
		findings = append(findings, redisClientsHistorySourceUnknown(len(targetNames)))
	}
	for _, target := range targetNames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		temporal, found := history[target]
		if historyErr != nil || !found {
			temporal = redisClientsHistoryObservation{reason: "source-unavailable"}
		}
		if temporal.reason != "" && historyErr == nil {
			findings = append(findings, redisClientsHistoryUnknown(target, temporal.reason))
		}
		if temporal.latest.runId != "" {
			changed := func() bool {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				previous := self.targetRunIds[target]
				self.targetRunIds[target] = temporal.latest.runId
				return previous != "" && previous != temporal.latest.runId
			}()
			if changed {
				self.connectionBatteries.healthy(target)
			}
		}
		current, currentKnown := spatial[target]
		currentSpike := currentKnown && !current.healthy
		if !currentSpike && !temporal.spike {
			if currentKnown && temporal.reason == "" {
				self.connectionBatteries.healthy(target)
				findings = append(findings, healthyFinding("redis/clients-spike", tierWarn, "clients-spike", target))
			}
			continue
		}
		node := targets[target]
		evidence := self.connectionBatteries.broken(target, func() string {
			return redisConnectionBattery(ctx, env, node.host, node.port)
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if currentSpike {
			diagnosis := diagnoseRedisConnectionSpike(evidence)
			current.evidence = evidence
			current.mechanism = diagnosis.mechanism
			current.context = diagnosis.context
			current.action = diagnosis.action
			current.verify = diagnosis.verify
		} else {
			current = finding{
				probeId: "redis/clients-spike", tier: tierWarn, class: "clients-spike", target: target, sustain: 2,
				symptom: fmt.Sprintf("redis node %d connected_clients grew from %.0f to %.0f (+%.1f%%) within ten minutes", node.port,
					temporal.baseline.clients, temporal.latest.clients, 100*(temporal.latest.clients/temporal.baseline.clients-1)),
				mechanism: "The same Redis process has at least 50% more connected clients than an earlier sample within ten minutes. A planned workload or deployment change can produce this symptom as well as reconnect churn or a pool configuration change; the count alone does not establish the cause.",
				baseline:  "Connected clients grow less than 50% within ten minutes and remain at most three times the fleet median.",
				evidence:  evidence,
				context:   "The historical comparison uses original exporter timestamps. CLIENT LIST cohorts and pressure controls are collected at the later battery trip; they are not historical samples.",
				action:    "Compare the bounded CLIENT LIST cohort ages, commands and sources with the matching deployment or workload boundary, Redis latency, pool timeouts, accept queues and output memory. Repair the demonstrated reconnect or pool owner; do not kill clients or raise pool floors from the growth percentage alone.",
				verify:    "Fresh same-generation ten-minute samples remain below 50% growth, the current fleet shape is in band, and node latency, pool timeouts, accept queues and output memory remain healthy on consecutive probes.",
				playbook:  "SIGNALS.md §3.5",
			}
		}
		if temporal.reason == "" {
			historyObserved := fmt.Sprintf("history_baseline_clients=%.0f history_latest_clients=%.0f history_baseline_at=%s history_latest_at=%s history_elapsed_seconds=%.3f history_growth_percent=%.1f",
				temporal.baseline.clients, temporal.latest.clients, temporal.baseline.at.UTC().Format(time.RFC3339Nano),
				temporal.latest.at.UTC().Format(time.RFC3339Nano), temporal.latest.at.Sub(temporal.baseline.at).Seconds(),
				100*(temporal.latest.clients/temporal.baseline.clients-1))
			if current.observed == "" {
				current.observed = historyObserved
			} else {
				current.observed += " " + historyObserved
				current.context += " Historical client counts are source-clocked exporter observations and need not equal the later native INFO count or trip-time CLIENT LIST total."
			}
		}
		findings = append(findings, current)
	}
	return findings, nil
}
