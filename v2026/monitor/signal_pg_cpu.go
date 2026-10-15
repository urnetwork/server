// Direct systemd accounting measures PostgreSQL CPU independently of query
// wall time, host load, and telemetry ingestion. Incarnations stay transient.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	pgCpuWarnRatio = 0.25
	pgCpuPageRatio = 0.85
	pgCpuTimeout   = 20 * time.Second
	pgCpuFreshness = 30 * time.Second
)

// SIGNALS.md §1.3c measures the CPU of active PostgreSQL service cgroups,
// including backend CPU charged before a backend exits between samples.
func NewPgCpuSignal() Signal {
	return &signalAdapter{
		number: "1.3c", key: "pg-cpu", name: "PostgreSQL CPU consumption",
		probe: pgCpuProbe{},
	}
}

// Each run owns its two samples; no counters survive a run or service restart.
type pgCpuProbe struct{}

func (pgCpuProbe) id() string             { return "pg/cpu" }
func (pgCpuProbe) tier() string           { return tierPage }
func (pgCpuProbe) cadence() time.Duration { return time.Minute }

// The remote shell only reads Linux/systemd source fields. All parsing,
// incarnation checks, rates, and alert decisions are implemented in Go.
// No process arguments, query text, credentials, or customer IDs are read.
const pgCpuCommand = `set -eu
for pg_cpu_sample in 1 2; do
    printf '%s\n' '--pg-cpu-snapshot--'
    printf 'host='; hostname -s
    printf 'timestamp='; date +%s
    printf 'boot='; cat /proc/sys/kernel/random/boot_id
    printf 'uptime='; head -c 128 /proc/uptime
    printf 'cores='; getconf _NPROCESSORS_ONLN
    printf '\n'
    systemctl show 'postgresql@*.service' --no-pager --property=Id,ActiveState,SubState,CPUAccounting,CPUUsageNSec,InvocationID
    printf '\nuptime_end='; head -c 128 /proc/uptime
    printf '%s\n' '--pg-cpu-end--'
    if [ "$pg_cpu_sample" = 1 ]; then sleep 5; fi
done`

// Cgroup accounting persists across backend churn but not a unit invocation.
type pgCpuUnit struct {
	invocationId string
	cpuNs        uint64
}

// Wall time proves freshness; monotonic uptime is the rate denominator.
type pgCpuSample struct {
	timestamp time.Time
	bootId    string
	uptime    float64
	cores     int
	unitIdCpu map[string]pgCpuUnit
}

// Only bounded numeric aggregates may cross into an alert.
type pgCpuObservation struct {
	seconds float64
	cores   int
	units   int
	used    float64
	ratio   float64
}

// Rejects incomplete, duplicate, or unexpected fields without retaining raw
// source text in an error that could reach the monitoring ledger.
func pgCpuFields(block string, keys ...string) (map[string]string, error) {
	fields := make(map[string]string, len(keys))
	for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("pg CPU: missing source field")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("pg CPU: duplicate source field")
		}
		fields[key] = value
	}
	if len(fields) != len(keys) {
		return nil, fmt.Errorf("pg CPU: unexpected source fields")
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("pg CPU: incomplete source fields")
		}
	}
	return fields, nil
}

// Both samples must identify the inventory host and a complete active-unit
// cohort. Unsupported accounting or an inactive-only host remains unknown.
func parsePgCpuSample(frame, expectedHost string) (pgCpuSample, error) {
	blocks := strings.Split(strings.TrimSpace(frame), "\n\n")
	if len(blocks) < 3 || len(blocks) > 34 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: missing or excessive unit census")
	}
	fields, err := pgCpuFields(blocks[0], "host", "timestamp", "boot", "uptime", "cores")
	if err != nil {
		return pgCpuSample{}, err
	}
	if fields["host"] != strings.Split(expectedHost, ".")[0] {
		return pgCpuSample{}, fmt.Errorf("pg CPU: remote hostname mismatch")
	}
	timestamp, err := strconv.ParseInt(fields["timestamp"], 10, 64)
	if err != nil || timestamp <= 0 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: invalid source time")
	}
	uptimeFields := strings.Fields(fields["uptime"])
	if len(uptimeFields) != 2 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: invalid uptime")
	}
	uptime, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil || math.IsNaN(uptime) || math.IsInf(uptime, 0) || uptime <= 0 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: invalid uptime")
	}
	cores, err := strconv.Atoi(fields["cores"])
	if err != nil || cores < 1 || cores > 8192 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: invalid logical CPU count")
	}
	if len(fields["boot"]) != 36 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: missing boot incarnation")
	}
	footer, err := pgCpuFields(blocks[len(blocks)-1], "uptime_end")
	if err != nil {
		return pgCpuSample{}, err
	}
	endFields := strings.Fields(footer["uptime_end"])
	if len(endFields) != 2 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: invalid sample completion")
	}
	endUptime, err := strconv.ParseFloat(endFields[0], 64)
	if err != nil || math.IsNaN(endUptime) || math.IsInf(endUptime, 0) || endUptime < uptime || endUptime-uptime > 0.5 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: slow or invalid sample collection")
	}
	sample := pgCpuSample{
		timestamp: time.Unix(timestamp, 0), bootId: fields["boot"],
		uptime: (uptime + endUptime) / 2, cores: cores, unitIdCpu: map[string]pgCpuUnit{},
	}
	for _, block := range blocks[1 : len(blocks)-1] {
		fields, err := pgCpuFields(block, "Id", "ActiveState", "SubState", "CPUAccounting", "CPUUsageNSec", "InvocationID")
		if err != nil {
			return pgCpuSample{}, err
		}
		unitId := fields["Id"]
		if !strings.HasPrefix(unitId, "postgresql@") || !strings.HasSuffix(unitId, ".service") {
			return pgCpuSample{}, fmt.Errorf("pg CPU: unexpected service identity")
		}
		if fields["ActiveState"] != "active" || fields["SubState"] != "running" {
			continue
		}
		if fields["CPUAccounting"] != "yes" || len(fields["InvocationID"]) != 32 {
			return pgCpuSample{}, fmt.Errorf("pg CPU: unavailable service accounting")
		}
		cpuNs, err := strconv.ParseUint(fields["CPUUsageNSec"], 10, 64)
		if err != nil || cpuNs == ^uint64(0) {
			return pgCpuSample{}, fmt.Errorf("pg CPU: invalid CPU counter")
		}
		if _, exists := sample.unitIdCpu[unitId]; exists {
			return pgCpuSample{}, fmt.Errorf("pg CPU: duplicate service identity")
		}
		sample.unitIdCpu[unitId] = pgCpuUnit{invocationId: fields["InvocationID"], cpuNs: cpuNs}
	}
	if len(sample.unitIdCpu) == 0 {
		return pgCpuSample{}, fmt.Errorf("pg CPU: no active PostgreSQL accounting source")
	}
	return sample, nil
}

// Counter decreases, changed unit/boot incarnations, stale samples, and CPU
// hotplug invalidate the rate instead of manufacturing a spike or recovery.
func parsePgCpuObservation(output, expectedHost string, now time.Time) (pgCpuObservation, error) {
	if len(output) > 64*1024 {
		return pgCpuObservation{}, fmt.Errorf("pg CPU: source output exceeds bound")
	}
	frames := strings.Split(output, "--pg-cpu-snapshot--\n")
	if len(frames) != 3 || strings.TrimSpace(frames[0]) != "" {
		return pgCpuObservation{}, fmt.Errorf("pg CPU: missing sample pair")
	}
	samples := make([]pgCpuSample, 0, 2)
	for _, frame := range frames[1:] {
		body, tail, ok := strings.Cut(frame, "--pg-cpu-end--")
		if !ok || strings.TrimSpace(tail) != "" {
			return pgCpuObservation{}, fmt.Errorf("pg CPU: truncated sample")
		}
		sample, err := parsePgCpuSample(body, expectedHost)
		if err != nil {
			return pgCpuObservation{}, err
		}
		age := now.Sub(sample.timestamp)
		if age > pgCpuFreshness || age < -pgCpuFreshness {
			return pgCpuObservation{}, fmt.Errorf("pg CPU: stale or future sample")
		}
		samples = append(samples, sample)
	}
	first, second := samples[0], samples[1]
	seconds := second.uptime - first.uptime
	if seconds < 4 || seconds > 15 || math.Abs(second.timestamp.Sub(first.timestamp).Seconds()-seconds) > 2 {
		return pgCpuObservation{}, fmt.Errorf("pg CPU: invalid sample interval")
	}
	if first.bootId != second.bootId || first.cores != second.cores || len(first.unitIdCpu) != len(second.unitIdCpu) {
		return pgCpuObservation{}, fmt.Errorf("pg CPU: changed capacity or incarnation")
	}
	var deltaNs float64
	for unitId, before := range first.unitIdCpu {
		after, ok := second.unitIdCpu[unitId]
		if !ok || before.invocationId != after.invocationId || after.cpuNs < before.cpuNs {
			return pgCpuObservation{}, fmt.Errorf("pg CPU: changed service incarnation or counter reset")
		}
		deltaNs += float64(after.cpuNs - before.cpuNs)
	}
	used := deltaNs / float64(time.Second) / seconds
	ratio := used / float64(second.cores)
	if ratio > 1.05 {
		return pgCpuObservation{}, fmt.Errorf("pg CPU: counter exceeds host capacity")
	}
	return pgCpuObservation{seconds: seconds, cores: second.cores, units: len(second.unitIdCpu), used: used, ratio: ratio}, nil
}

// Reads only enabled database hosts and preserves visibility independently
// from the high-CPU finding so a missing observation cannot resolve it.
func (pgCpuProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("pg-primary")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("pg CPU: no enabled database host")
	}
	findings := []finding{}
	for _, target := range hosts {
		output, err := env.runner.sshTimeout(ctx, target, pgCpuCommand, "", pgCpuTimeout)
		var observation pgCpuObservation
		if err == nil {
			observation, err = parsePgCpuObservation(output, target.name, env.now())
		}
		if err != nil {
			findings = append(findings, finding{
				probeId: "pg/cpu", tier: tierWarn, class: "pg-cpu-unavailable", target: target.name, sustain: 2,
				symptom:   "The database host has no complete current PostgreSQL CPU observation.",
				mechanism: "The direct source, host identity, service accounting, sample freshness, or stable counter incarnation could not be verified. CPU usage is unknown, not zero.",
				baseline:  "Two fresh bounded samples identify the same host, boot, active PostgreSQL units, and monotonically increasing CPU counters.",
				observed:  "source_status=unavailable", evidence: "No raw source output or service incarnation is retained in the finding.",
				action: "Check the inventory-owned host route and active postgresql@ service CPUAccounting/CPUUsageNSec. Reobserve after a restart or CPU-count change; do not infer CPU headroom from SQL wall time or an unavailable OS counter.",
				verify: "Two consecutive runs return complete fresh, stable service-accounting pairs.", playbook: "SIGNALS.md §1.3c",
			})
			continue
		}
		findings = append(findings, healthyFinding("pg/cpu", tierWarn, "pg-cpu-unavailable", target.name))
		if observation.ratio < pgCpuWarnRatio {
			findings = append(findings, healthyFinding("pg/cpu", tierWarn, "pg-cpu-high", target.name))
			continue
		}
		tier := tierWarn
		if pgCpuPageRatio <= observation.ratio {
			tier = tierPage
		}
		findings = append(findings, finding{
			probeId: "pg/cpu", tier: tier, class: "pg-cpu-high", target: target.name, sustain: 2,
			symptom:   "PostgreSQL is consuming a sustained high share of the database host's logical CPU capacity.",
			mechanism: "Active PostgreSQL service cgroups consumed CPU time during this measured interval. Their counters include short-lived backends; no runnable-load threshold or database exporter is required.",
			baseline:  fmt.Sprintf("PostgreSQL CPU/host capacity < %.0f%%; PAGE at %.0f%%, each for two one-minute observations", 100*pgCpuWarnRatio, 100*pgCpuPageRatio),
			observed:  fmt.Sprintf("sample_s=%.2f postgres_cpu_cores=%.3f logical_cpu_count=%d postgres_cpu_fraction=%.4f active_service_count=%d", observation.seconds, observation.used, observation.cores, observation.ratio, observation.units),
			evidence:  "CPU cores = summed service CPUUsageNSec deltas / 1e9 / monotonic elapsed seconds; fraction = CPU cores / online logical CPUs. Remote hostname, boot, unit invocation, and sample freshness matched.",
			context:   "False-positive qualifier: a legitimate query, vacuum, or maintenance workload may explain high CPU; this signal establishes consumption, not the owner query or a throughput defect. False-negative qualifiers: short peaks, CPU quotas/affinity below host capacity, and PostgreSQL outside the observed systemd units need separate controls. Query cumulative execution time includes waits and is not CPU time. BufferContent/WALInsert contention and zero-byte escrow fanout may be severe even while this signal is healthy; correlate §1.3d and §2.2.",
			action:    "Correlate bounded direct active-query/wait snapshots, statement call deltas, and §1.3d escrow fanout with a healthy control. Separate useful workload, repeated work, lock/WAL waits, cgroup throttling, and hardware limits. Do not restart PostgreSQL, cancel work, or raise concurrency as a diagnostic shortcut.",
			verify:    "Two consecutive fresh samples fall below the CPU band while the affected query/contract path recovers and escrow amplification does not recur.", playbook: "SIGNALS.md §1.3c",
		})
	}
	return findings, nil
}
