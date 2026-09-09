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
	host := env.cfg.hostByRole("redis-cluster")
	if host == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	out, err := env.runner.shell(ctx, host, `
ps -eo pid=,pcpu=,vsz=,rss=,comm= | awk '$5 ~ /^redis-server/ {print "redis",$1,$2,$3,$4}'
dmesg -T 2>/dev/null | grep -iE 'out of memory|oom-kill|killed process.*redis' | tail -1 | sed 's/^/kernel_oom /'
`)
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
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
			findings = append(findings, finding{
				probeId: "redis/process", tier: tierPage,
				class: "redis-kernel-oom", target: host.name, sustain: 1,
				symptom:   "The kernel OOM killer selected a Redis process on the cluster host.",
				mechanism: "Aggregate Redis allocation exceeded physical memory; the killed serving master leaves a shard unavailable without replica cover.",
				baseline:  "No Redis-related kernel OOM event.",
				observed:  "redis-related kernel OOM event present in the current dmesg window",
				evidence:  strings.TrimSpace(strings.TrimPrefix(line, "kernel_oom")),
				action:    "Confirm the killed PID and cluster slot impact, restore that node, then reconcile aggregate maxmemory with host RAM.",
				verify:    "Every node PING succeeds, cluster slots are covered, and no new OOM event appears.",
				playbook:  "SIGNALS.md §3.4 and §5.2",
			})
		}
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("redis/process", tierWarn, "redis-process", host.name))
	}
	return findings, nil
}
