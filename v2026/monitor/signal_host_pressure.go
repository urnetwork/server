package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Signal host-pressure implements SIGNALS.md §8.14a. Direct CPU PSI catches
// runnable delay even when aggregate CPU execution remains below §8.14's
// saturation band. It does not claim which process or cgroup owns the wait.
func NewHostPressureSignal() Signal {
	return &signalAdapter{
		number: "8.14a", key: "host-pressure", name: "Host CPU runnable delay",
		probe: hostPressureProbe{},
	}
}

const hostPressurePageAverage60 = 20.0

type hostPressureProbe struct{}

func (hostPressureProbe) id() string             { return "host/cpu-pressure" }
func (hostPressureProbe) tier() string           { return tierPage }
func (hostPressureProbe) cadence() time.Duration { return time.Minute }

type hostPressureSample struct {
	avg10  float64
	avg60  float64
	avg300 float64
	total  uint64
}

// The identity line is supplied by the contacted host, not a caller-provided
// label. Do not attribute a pressure sample to a stale inventory endpoint.
func parseHostPressure(output, expectedHost string) (hostPressureSample, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != strings.Split(expectedHost, ".")[0] {
		return hostPressureSample{}, fmt.Errorf("host CPU pressure: remote hostname mismatch or missing")
	}
	fields := strings.Fields(lines[1])
	if len(fields) != 5 || fields[0] != "some" {
		return hostPressureSample{}, fmt.Errorf("host CPU pressure: malformed some line")
	}
	values := map[string]string{}
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" || values[key] != "" {
			return hostPressureSample{}, fmt.Errorf("host CPU pressure: malformed field")
		}
		values[key] = value
	}
	if len(values) != 4 {
		return hostPressureSample{}, fmt.Errorf("host CPU pressure: incomplete sample")
	}
	parseAverage := func(key string) (float64, error) {
		value, err := strconv.ParseFloat(values[key], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return 0, fmt.Errorf("host CPU pressure: invalid average")
		}
		return value, nil
	}
	var sample hostPressureSample
	var err error
	if sample.avg10, err = parseAverage("avg10"); err != nil {
		return hostPressureSample{}, err
	}
	if sample.avg60, err = parseAverage("avg60"); err != nil {
		return hostPressureSample{}, err
	}
	if sample.avg300, err = parseAverage("avg300"); err != nil {
		return hostPressureSample{}, err
	}
	if sample.total, err = strconv.ParseUint(values["total"], 10, 64); err != nil {
		return hostPressureSample{}, fmt.Errorf("host CPU pressure: invalid total")
	}
	return sample, nil
}

func (hostPressureProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings := make([]finding, 0, len(env.cfg.hosts)*2)
	for _, target := range env.cfg.hosts {
		output, err := env.runner.sshTimeout(ctx, target,
			"hostname -s; head -c 512 /proc/pressure/cpu", "", 10*time.Second)
		var sample hostPressureSample
		if err == nil {
			sample, err = parseHostPressure(output, target.name)
		}
		if err != nil {
			findings = append(findings, finding{
				probeId: "host/cpu-pressure", tier: tierWarn,
				class: "host-cpu-pressure-unavailable", target: target.name, sustain: 2,
				symptom:   "The host's direct CPU runnable-delay observation is unavailable.",
				mechanism: "The SSH source, remote host identity, or Linux CPU pressure file could not be verified. Missing pressure is unknown, not zero.",
				baseline:  "Every enabled host returns its own hostname and a valid /proc/pressure/cpu some sample each minute.",
				observed:  "source_status=unavailable",
				action:    "Check the inventory-owned SSH route, exact remote hostname, and /proc/pressure/cpu. Preserve any operator host exclusion; do not infer spare CPU from missing PSI.",
				verify:    "Two consecutive bounded reads match the inventory host and parse a complete CPU PSI some sample.",
				playbook:  "SIGNALS.md §8.14a",
			})
			continue
		}
		findings = append(findings, healthyFinding("host/cpu-pressure", tierWarn, "host-cpu-pressure-unavailable", target.name))
		if sample.avg60 >= hostPressurePageAverage60 {
			findings = append(findings, finding{
				probeId: "host/cpu-pressure", tier: tierPage,
				class: "host-cpu-pressure", target: target.name, sustain: 2,
				symptom:   "Runnable work is spending a sustained share of time waiting for CPU on this host.",
				mechanism: "Linux CPU PSI reports time with at least one runnable task delayed. This can slow connection setup and packet callbacks even when aggregate CPU execution remains below the host-load saturation threshold.",
				baseline:  fmt.Sprintf("cpu_some_avg60 < %.1f%%", hostPressurePageAverage60),
				observed:  fmt.Sprintf("cpu_some_avg10=%.2f%% cpu_some_avg60=%.2f%% cpu_some_avg300=%.2f%% total_wait_us=%d", sample.avg10, sample.avg60, sample.avg300, sample.total),
				evidence:  "Direct, bounded /proc/pressure/cpu read after matching the remote short hostname to the enabled inventory target.",
				context:   "PSI proves runnable delay, not host-wide CPU exhaustion or a particular process. Cgroup quotas, scheduler contention, or legitimate demand can each produce it; compare §8.14, §8.15, process ownership, and a healthy host control.",
				action:    "Compare CPU mode, normalized load, per-process CPU/goroutines/sockets, cgroup throttling, and request latency before changing limits. Preserve service health; do not reboot to erase the owner.",
				verify:    "Two consecutive direct samples have cpu_some_avg60 below the threshold while affected service paths and probe results recover.",
				playbook:  "SIGNALS.md §8.14a",
			})
		} else {
			findings = append(findings, healthyFinding("host/cpu-pressure", tierPage, "host-cpu-pressure", target.name))
		}
	}
	return findings, nil
}
