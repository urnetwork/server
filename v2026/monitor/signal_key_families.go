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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/glog/v2026"
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
	regexp.MustCompile(`^\{vce[24]_<id>\}$`),
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
	if env.cfg.redisEntryHost() == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	if len(env.cfg.redisAllNodePorts()) == 0 {
		return nil, fmt.Errorf("no redis node ports configured")
	}

	// the sampled node may be on any redis-cluster host; scan it where it runs.
	// With any node unreadable or unranked it is only the fullest observed node:
	// its families are still judged, but they are not the fleet's fullest-node
	// series, so they are not recorded as its baseline.
	h, fullest, failures, err := fullestRedisNodeUsage(ctx, env)
	if err != nil {
		return nil, err
	}
	partial := len(failures) > 0

	// Retain only the bounded histogram in the shell. A streaming validator
	// counts newlines in fixed chunks and distinguishes cap completion from early EOF.
	scan := fmt.Sprintf(`/bin/bash <<'REDIS_FAMILY'
export LC_ALL=C
if observation=$(
 exec 3>&1
 # Preserve NUL fragment presence across Bash read without making it a known
 # family: 0xff is non-UTF-8 and is redacted by the persistence boundary.
 timeout 170 redis-cli -p %d --scan --count 5000 2>/dev/null | head -n %d | tr '\000' '\377' | /bin/bash -c '
 count=0
 last=
 newline="
"
 while IFS= read -r -N 16384 chunk || [[ -n "$chunk" ]]; do
  printf "%%s" "$chunk" || exit 1
  without_newlines=${chunk//$newline/}
  count=$((count + ${#chunk} - ${#without_newlines}))
  last=${chunk: -1}
 done
 [[ -z "$last" || "$last" == "$newline" ]] || exit 2
 printf "family_source complete %%d\n" "$count" >&3 || exit 1
 if (( count == %d )); then exit 42; fi
 exit 0
 ' | sed -E 's/[0-9A-Fa-f]{8}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{4}-?[0-9A-Fa-f]{12}/<id>/g; s/[0-9]{6,}/<n>/g' | sort | uniq -c | sort -rn | sed -n '1,20p'
 statuses=("${PIPESTATUS[@]}")
 if (( statuses[1] != 0 || statuses[2] != 0 || (statuses[3] != 0 && statuses[3] != 42) || statuses[4] != 0 || statuses[5] != 0 || statuses[6] != 0 || statuses[7] != 0 || statuses[8] != 0 )); then exit 1; fi
 if (( statuses[0] != 0 && !(statuses[0] == 141 && statuses[3] == 42) )); then exit 1; fi
); then
 # sort needs EOF, so the validator's side-channel header precedes histogram
 # output. Move that header to the terminal position expected by the parser.
 source_header=${observation%%%%$'\n'*}
 if [[ "$observation" == *$'\n'* ]]; then
  printf '%%s\n' "${observation#*$'\n'}"
 fi
 printf '%%s\n' "$source_header"
else
 printf 'family_source unavailable\n'
fi
REDIS_FAMILY`, fullest.port, familyScanKeyLimit, familyScanKeyLimit)
	out, err := env.runner.sshTimeout(ctx, h, scan, "", 200*time.Second)
	if err != nil {
		return nil, err
	}

	target := fmt.Sprintf("%s:%d", h.name, fullest.port)
	findings := []finding{}
	histogramLines := []string{}
	alerted := false
	familyCounts, scanErr := redisFamilyCounts(out)
	if scanErr != nil {
		findings = append(findings, cannotObserveFinding(target+"/key-families", scanErr))
		for _, failure := range failures {
			findings = append(findings, redisUsageCannotObserveFinding(failure, "key-families"))
		}
		return findings, nil
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
			if !partial {
				env.baseline.record(metric, time.Now(), float64(count))
			}
		}
		if haveHistory && count > familyAlertMinCount && float64(count) > 3*median {
			alerted = true
			observed := fmt.Sprintf("count=%d median=%.0f node=%d", count, median, fullest.port)
			if partial {
				observed += " " + redisUsageSelectionScope(failures)
			}
			findings = append(findings, finding{
				probeId: "redis/key-families", tier: tierWarn,
				class: "family-growth", target: target, frame: family, sustain: 1,
				symptom: fmt.Sprintf("key family %q at %d keys in the sample, > 3x its 7-day median %.0f — missing-ttl signature",
					family, count, median),
				baseline: fmt.Sprintf("trailing 7-day median %.0f keys in a %d-key sample (learned)", median, familyScanKeyLimit),
				observed: observed,
				context:  "a family growing without bound = missing ttl (3.3); pair with --memkeys for byte attribution",
				playbook: "SIGNALS.md 3.3",
			})
		}
	}
	for _, failure := range failures {
		findings = append(findings, redisUsageCannotObserveFinding(failure, "key-families"))
	}
	// the histogram is the daily inventory a diagnostician greps for — log it
	// unconditionally, since a healthy finding emits no ticket
	selectionScope := "fleet_fullest=observed"
	if partial {
		selectionScope = redisUsageSelectionScope(failures)
	}
	glog.Infof("[monitor]key families on %s (sample %d keys; %s):\n%s\n",
		target, familyScanKeyLimit, selectionScope, strings.Join(histogramLines, "\n"))

	if !alerted && !partial {
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

// Validate the entire bounded histogram before labels or counts can reach a
// baseline, log or alert. Missing completion cannot authorize an empty scan.
func redisFamilyCounts(output string) (map[string]int, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	familyCounts := map[string]int{}
	seenFamilies := map[string]bool{}
	total := 0
	rows := 0
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "family_source" {
			if index != len(lines)-1 {
				return nil, fmt.Errorf("invalid response from Redis family source")
			}
			if len(fields) == 2 && fields[1] == "unavailable" && rows == 0 {
				return nil, fmt.Errorf("Redis family source exit status failure")
			}
			if len(fields) != 3 || fields[1] != "complete" {
				return nil, fmt.Errorf("invalid response from Redis family source")
			}
			count, err := strconv.Atoi(fields[2])
			if err != nil || count < 0 || count > familyScanKeyLimit || total > count || (count > 0 && rows == 0) || (rows < 20 && total != count) {
				return nil, fmt.Errorf("invalid response from Redis family source")
			}
			return familyCounts, nil
		}
		if len(fields) < 2 || rows >= 20 {
			return nil, fmt.Errorf("invalid response from Redis family source")
		}
		count, err := strconv.Atoi(fields[0])
		family := strings.Join(fields[1:], " ")
		if err != nil || count <= 0 || count > familyScanKeyLimit || seenFamilies[family] || total > familyScanKeyLimit-count {
			return nil, fmt.Errorf("invalid response from Redis family source")
		}
		seenFamilies[family] = true
		total += count
		rows++
		familyCounts[safeRedisFamilyLabel(family)] += count
	}
	return nil, fmt.Errorf("invalid response from Redis family source")
}
