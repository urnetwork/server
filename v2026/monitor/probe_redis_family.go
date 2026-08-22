// redis keyspace family histogram (SIGNALS.md 3.3): what is growing. Shapes,
// not bytes — a family growing without bound = missing ttl, the recurring
// disease. Daily on the fullest node; counts are recorded to the baseline
// store so growth is judged against the family's own trailing history.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
)

// how many keys the sampled scan reads on the fullest node. Sampling bounds
// the scan's runtime and event-loop cost; family proportions at this size are
// stable enough for growth detection.
const familyScanKeyLimit = 300_000

// a family only alerts once it is both large in the sample and well above its
// own trailing median count
const familyAlertMinCount = 20_000

// redisFamilyProbe runs the 3.3 histogram daily on the fullest node: a
// sampled --scan with ids normalized away, counted by family shape. Each
// family's count is recorded as a baseline metric; a family > 3x its trailing
// 7-day median (and large) is the missing-ttl signature.
type redisFamilyProbe struct{}

func (self redisFamilyProbe) id() string             { return "redis/key-families" }
func (self redisFamilyProbe) tier() string           { return tierWarn }
func (self redisFamilyProbe) cadence() time.Duration { return 24 * time.Hour }

func (self redisFamilyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	ports := h.redisNodePorts()
	if len(ports) == 0 {
		return nil, fmt.Errorf("no redis node ports configured")
	}
	lo, hi := ports[0], ports[len(ports)-1]

	// pick the fullest node (used_memory sweep is cheap)
	out, err := env.runner.shell(ctx, h, fmt.Sprintf(
		`for p in $(seq %d %d); do echo "$p $(timeout 3 redis-cli -p $p INFO memory 2>/dev/null | tr -d '\r' | awk -F: '/^used_memory:/{print $2}')"; done | sort -k2 -rn | head -1`,
		lo, hi))
	if err != nil {
		return nil, err
	}
	fullestPort := atoi(strings.Fields(out + " 0")[0])
	if fullestPort == 0 {
		return nil, fmt.Errorf("could not determine fullest node from %q", strings.TrimSpace(out))
	}

	// sampled scan, ids normalized to <id>, counted by shape. LC_ALL=C per
	// 3.3; head bounds the scan; the generous remote+ssh timeouts cover a
	// slow walk.
	scan := fmt.Sprintf(
		`LC_ALL=C timeout 170 redis-cli -p %d --scan --count 5000 2>/dev/null | head -%d `+
			`| sed -E 's/[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}/<id>/g; s/[0-9]{6,}/<n>/g' `+
			`| sort | uniq -c | sort -rn | head -20`,
		fullestPort, familyScanKeyLimit)
	out, err = env.runner.sshTimeout(ctx, h, scan, "", 200*time.Second)
	if err != nil {
		return nil, err
	}

	target := fmt.Sprintf("%s:%d", h.name, fullestPort)
	findings := []finding{}
	histogramLines := []string{}
	alerted := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		count := atoi(fields[0])
		family := strings.Join(fields[1:], " ")
		histogramLines = append(histogramLines, fmt.Sprintf("  %8d %s", count, family))

		metric := "redis/family/" + family
		var median float64
		var haveHistory bool
		if env.baseline != nil {
			median, _, haveHistory = env.baseline.trailingMedian(metric, 7*24*time.Hour, 3)
			env.baseline.record(metric, time.Now(), float64(count))
		}
		if haveHistory && count > familyAlertMinCount && float64(count) > 3*median {
			alerted = true
			findings = append(findings, finding{
				probeId: "redis/key-families", tier: tierWarn,
				class: "family-growth", target: target, frame: family, sustain: 1,
				symptom: fmt.Sprintf("key family %q at %d keys in the sample, > 3x its 7-day median %.0f — missing-ttl signature",
					family, count, median),
				baseline: fmt.Sprintf("trailing 7-day median %.0f keys in a %d-key sample (learned)", median, familyScanKeyLimit),
				observed: fmt.Sprintf("count=%d median=%.0f node=%d", count, median, fullestPort),
				context:  "a family growing without bound = missing ttl (3.3); pair with --memkeys for byte attribution",
				playbook: "SIGNALS.md 3.3",
			})
		}
	}
	// the histogram is the daily inventory a diagnostician greps for — log it
	// unconditionally, since a healthy finding emits no ticket
	glog.Infof("[monitor]key families on %s (sample %d keys):\n%s\n",
		target, familyScanKeyLimit, strings.Join(histogramLines, "\n"))

	if !alerted {
		findings = append(findings, healthyFinding("redis/key-families", tierWarn, "family-growth", target))
	}
	return findings, nil
}
