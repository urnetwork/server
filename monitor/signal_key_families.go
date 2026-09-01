// redis keyspace family histogram (SIGNALS.md 3.3): what is growing. Shapes,
// not bytes — a family growing without bound = missing ttl, the recurring
// disease. Daily on the fullest node; counts are recorded to the baseline
// store so growth is judged against the family's own trailing history.
package monitor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/glog"
)

// SIGNALS.md §3.3 maps to signal_key_families.go and signal_key_families_test.go.
// Keep the catalog number, implementation, registration, and synthetic test
// together when this signal changes.
func NewKeyFamiliesSignal() Signal {
	return &signalAdapter{number: "3.3", key: "key-families", name: "Redis keyspace family histogram", probe: redisFamilyProbe{}}
}

// how many keys the sampled scan reads on the fullest node. Sampling bounds
// the scan's runtime and event-loop cost; family proportions at this size are
// stable enough for growth detection.
const familyScanKeyLimit = 300_000

// a family only alerts once it is both large in the sample and well above its
// own trailing median count
const familyAlertMinCount = 20_000

// Every retained label is a static Redis schema shape with all dynamic values
// replaced. New schemas remain in a redacted aggregate until their normalized
// form is deliberately reviewed and added here.
var safeRedisFamilyShapes = []*regexp.Regexp{
	regexp.MustCompile(`^ckey_<id>$`),
	regexp.MustCompile(`^ncr_<id>$`),
	regexp.MustCompile(`^\{pm_<id>\}(rp|pms|sk_[0-9]+)$`),
	regexp.MustCompile(`^\{<id>\}s2?_c_eid$`),
	regexp.MustCompile(`^\{connect_<id><id>\}total_<id>$`),
	regexp.MustCompile(`^\{cs_[0-9]+_[a-z]_<id>_<id>\}(a|c|f)_(g|l)$`),
	regexp.MustCompile(`^\{cs_[0-9]+_[a-z]_<id>_<id>\}s_(g|l)_[0-9]+$`),
	regexp.MustCompile(`^\{vce2_<id>\}$`),
	regexp.MustCompile(`^\{pd_<id>\}c$`),
	regexp.MustCompile(`^verify_egress_v2_<id>(<id>)?$`),
	regexp.MustCompile(`^\{account_balance_<id>\}(np|npbc)$`),
}

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

	fullest, err := fullestRedisNodeUsage(ctx, env, h)
	if err != nil {
		return nil, err
	}

	// sampled scan, ids normalized to <id>, counted by shape. LC_ALL=C per
	// 3.3; head bounds the scan; the generous remote+ssh timeouts cover a
	// slow walk.
	scan := fmt.Sprintf(
		`LC_ALL=C timeout 170 redis-cli -p %d --scan --count 5000 2>/dev/null | head -%d `+
			`| sed -E 's/[0-9A-Fa-f]{8}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{12}/<id>/g; s/[0-9]{6,}/<n>/g' `+
			`| sort | uniq -c | sort -rn | head -20`,
		fullest.port, familyScanKeyLimit)
	out, err := env.runner.sshTimeout(ctx, h, scan, "", 200*time.Second)
	if err != nil {
		return nil, err
	}

	target := fmt.Sprintf("%s:%d", h.name, fullest.port)
	findings := []finding{}
	histogramLines := []string{}
	alerted := false
	familyCounts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		count := atoi(fields[0])
		family := safeRedisFamilyLabel(strings.Join(fields[1:], " "))
		familyCounts[family] += count
	}
	type familyCount struct {
		family string
		count  int
	}
	families := make([]familyCount, 0, len(familyCounts))
	for family, count := range familyCounts {
		families = append(families, familyCount{family: family, count: count})
	}
	sort.Slice(families, func(i, j int) bool {
		if families[i].count != families[j].count {
			return families[i].count > families[j].count
		}
		return families[i].family < families[j].family
	})
	for _, sampled := range families {
		family := sampled.family
		count := sampled.count
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
				observed: fmt.Sprintf("count=%d median=%.0f node=%d", count, median, fullest.port),
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

// safeRedisFamilyLabel is the persistence boundary for sampled key material.
// The remote scan replaces known identifier forms with placeholders, but an
// unknown schema can contain binary bytes or an identifier that those patterns
// do not recognize. Keep useful normalized shapes and collapse everything else
// into non-identifying classes before it reaches a log, baseline key, or alert.
func safeRedisFamilyLabel(family string) string {
	if !utf8.ValidString(family) {
		return "redacted-binary-family"
	}
	if len(family) > 160 {
		return "redacted-oversize-family"
	}
	for _, value := range []byte(family) {
		if ('a' <= value && value <= 'z') ||
			('A' <= value && value <= 'Z') ||
			('0' <= value && value <= '9') ||
			strings.ContainsRune("_{}:<>=+./-", rune(value)) {
			continue
		}
		return "redacted-unnormalized-family"
	}
	for _, shape := range safeRedisFamilyShapes {
		if shape.MatchString(family) {
			return family
		}
	}
	return "redacted-unnormalized-family"
}
