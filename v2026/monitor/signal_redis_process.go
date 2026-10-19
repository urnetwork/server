package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SIGNALS.md §3.4 maps to signal_redis_process.go and signal_redis_process_test.go.
func NewRedisProcessSignal() Signal {
	return &signalAdapter{number: "3.4", key: "redis-process", name: "Redis node process signals", probe: redisProcessProbe{}}
}

type redisProcessProbe struct{}

func (redisProcessProbe) id() string             { return "redis/process" }
func (redisProcessProbe) tier() string           { return tierWarn }
func (redisProcessProbe) cadence() time.Duration { return 5 * time.Minute }

func (redisProcessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.redisSweepHosts()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	// every redis-cluster host runs its own Redis processes and kernel
	findings := []finding{}
	for _, host := range hosts {
		hostFindings, err := redisProcessHostFindings(ctx, env, host)
		if err != nil {
			if len(hosts) == 1 {
				return nil, err
			}
			findings = append(findings, cannotObserveFinding(host.name+"/redis-process", err))
			continue
		}
		findings = append(findings, hostFindings...)
	}
	return findings, nil
}

func redisProcessHostFindings(ctx context.Context, env *probeEnv, host *host) ([]finding, error) {
	command := fmt.Sprintf(`
ps -eo pid=,pcpu=,vsz=,rss=,comm= | awk '$5 ~ /^redis-server/ {print "redis",$1,$2,$3,$4}'
if redis_kernel_rows=$(/bin/bash -o pipefail <<'REDIS_KERNEL'
dmesg -T 2>/dev/null | %s
REDIS_KERNEL
); then
  printf '%%s\n' "$redis_kernel_rows"
  printf 'kernel_source complete\n'
else
  printf 'kernel_source unavailable\n'
fi
`, redisProcessKernelOomSelector())
	out, err := env.runner.shell(ctx, host, command)
	if err != nil {
		return nil, err
	}
	// Only a successful full pipeline and its terminal frame authorize kernel
	// evidence. Buffering the selected row discards partial output on failure.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	kernelStatus := ""
	kernelRows := 0
	kernelMalformed := false
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "redis":
		case "kernel_oom":
			kernelRows++
			if len(fields) < 2 || kernelRows > 1 {
				kernelMalformed = true
			}
		case "kernel_source":
			if len(fields) != 2 || kernelStatus != "" || index != len(lines)-1 {
				kernelMalformed = true
			} else {
				kernelStatus = fields[1]
			}
		default:
			kernelMalformed = true
		}
	}
	var kernelErr error
	if kernelMalformed || kernelStatus != "complete" && kernelStatus != "unavailable" {
		kernelErr = fmt.Errorf("invalid response from Redis kernel source")
	} else if kernelStatus == "unavailable" {
		kernelErr = fmt.Errorf("Redis kernel source exit status failure")
	}
	findings := []finding{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "redis":
			if len(fields) < 5 || atof(fields[2]) <= 200 {
				continue
			}
			findings = append(findings, finding{
				probeId: "redis/process", tier: tierWarn,
				class: "redis-cpu-sustained", target: host.name, frame: fields[1], sustain: 2,
				symptom:   fmt.Sprintf("redis process %s is using %s%% CPU", fields[1], fields[2]),
				mechanism: "Sustained multi-core Redis CPU indicates io-thread/lazyfree churn; a pegged fork child can stall the serving parent's event loop during BGSAVE.",
				baseline:  "No Redis process remains above 200% CPU across two samples.",
				observed:  fmt.Sprintf("pid=%s cpu_pct=%s virt_kb=%s res_kb=%s", fields[1], fields[2], fields[3], fields[4]),
				action:    "Identify whether the PID is a serving master or BGSAVE child, then correlate command rate and save cadence before intervening.",
				verify:    "Serving processes return below 200% CPU and local PING latency remains healthy.",
				playbook:  "SIGNALS.md §3.4 and §5.2",
			})
		case "kernel_oom":
			if kernelErr != nil {
				continue
			}
			victim := redisProcessKernelOomVictim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "kernel_oom")))
			if victim == "other" {
				continue
			}
			if victim == "unknown" {
				findings = append(findings, cannotObserveFinding(host.name+"/redis-kernel", fmt.Errorf("invalid response: kernel OOM victim is ambiguous")))
				continue
			}
			findings = append(findings, finding{
				probeId: "redis/process", tier: tierPage,
				class: "redis-kernel-oom", target: host.name, sustain: 1,
				symptom:   "The retained kernel log identifies a redis-server OOM victim on the cluster host.",
				mechanism: "The kernel selected a task named redis-server during an OOM event. The record alone does not identify its Redis role, the exhausted memory limit, or current cluster impact.",
				baseline:  "No explicit Redis victim in the retained kernel records.",
				observed:  "retained kernel record includes an explicit redis-server victim",
				evidence:  "victim_comm=redis-server; source=kernel-oom-victim-record",
				context:   "Kernel records may predate this observation. Confirm the event time and process identity before attributing current shard loss or recovery.",
				action:    "Confirm the victim PID, event time, Redis process role, cgroup and host memory limits, and cluster slot impact before choosing remediation.",
				verify:    "Every node PING succeeds, cluster slots are covered, and a bounded follow-up window records no new Redis OOM event.",
				playbook:  "SIGNALS.md §3.4 and §5.2",
			})
		}
	}
	if kernelErr != nil {
		findings = append(findings, cannotObserveFinding(host.name+"/redis-kernel", kernelErr))
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("redis/process", tierWarn, "redis-process", host.name))
	}
	return findings, nil
}
