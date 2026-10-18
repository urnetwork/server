// Exercise the actual fullest-node collector with local scripted Redis replies,
// then verify the named samplers' coverage, identity, and baseline boundaries.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const redisUsageAuthorityHealthy = "# Memory\r\nused_memory:100\r\nmaxmemory:1000\r\n"

// A native reply and its independent child status are both source authority.
type redisUsageAuthorityReply struct {
	body   string
	status int
}

// Commands run only through a private fake redis-cli and timeout. Sample calls
// remain synthetic and record the owning host and selected port for assertions.
type redisUsageAuthorityFixture struct {
	settings SignalSettings
	source   *syntheticSource
	dir      string
	calls    string
	samples  []string
}

// Execute the generated shell, including command substitution and awk, without
// a real Redis, network access, production configuration, or wall-clock sleep.
func newRedisUsageAuthorityFixture(t *testing.T, replies map[int]redisUsageAuthorityReply) *redisUsageAuthorityFixture {
	t.Helper()
	f := &redisUsageAuthorityFixture{dir: t.TempDir()}
	f.calls = filepath.Join(f.dir, "calls")
	for port, reply := range replies {
		f.writeReply(t, port, reply)
	}
	for name, script := range map[string]string{
		"redis-cli": `#!/bin/sh
[ "$#" -eq 4 ] && [ "$1" = "-p" ] && [ "$3" = INFO ] && [ "$4" = memory ] || exit 126
printf '%s\n' "$2" >> "$REDIS_USAGE_TEST_CALLS"
cat "$REDIS_USAGE_TEST_DIR/$2.reply" || exit 126
exit "$(cat "$REDIS_USAGE_TEST_DIR/$2.status")"
`,
		"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.source = &syntheticSource{
		hostFn: func(h HostSettings, command string) (string, error) {
			if strings.Contains(command, "EVAL_RO") {
				f.samples = append(f.samples, h.Name+" "+command)
				return syntheticRedisByteSample, nil
			}
			if !strings.Contains(command, "INFO memory") {
				t.Fatal("unexpected synthetic usage command")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
			cmd.Env = []string{
				"PATH=" + f.dir + ":/usr/bin:/bin", "LC_ALL=C",
				"REDIS_USAGE_TEST_DIR=" + f.dir, "REDIS_USAGE_TEST_CALLS=" + f.calls,
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("synthetic fullest-node collector failed: %v", err)
			}
			return string(out), nil
		},
		hostTimeoutFn: func(h HostSettings, command string, _ time.Duration) (string, error) {
			f.samples = append(f.samples, h.Name+" "+command)
			return "30000 ckey_<id>", nil
		},
	}
	f.settings = syntheticSettings(f.source)
	f.settings.Hosts[1].Name = "redis-usage.example"
	f.settings.Hosts[1].RedisNodePorts = []int{6380, 6381}
	return f
}

// Replace one fake native observation to exercise correction or reset without
// rerunning an old snapshot or mutating a production system.
func (self *redisUsageAuthorityFixture) writeReply(t *testing.T, port int, reply redisUsageAuthorityReply) {
	t.Helper()
	for suffix, contents := range map[string]string{"reply": reply.body, "status": fmt.Sprint(reply.status)} {
		if err := os.WriteFile(filepath.Join(self.dir, fmt.Sprintf("%d.%s", port, suffix)), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Use the production environment and reducer entry point when healthy findings
// must be inspected before the public adapter omits them.
func redisUsageAuthorityEnv(t *testing.T, settings SignalSettings) *probeEnv {
	t.Helper()
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// Require the precise selection visibility target and reject raw native reply
// bytes in both structured and rendered output.
func redisUsageAuthorityRequireGap(t *testing.T, alerts Alerts, observation string, missing int) Alert {
	t.Helper()
	var gap *Alert
	for i := range alerts {
		alert := &alerts[i]
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "synthetic-private-sentinel") || strings.Contains(alert.Markdown(), "synthetic-private-sentinel") {
			t.Fatal("raw fullest-node source reply leaked into an alert")
		}
		if alert.Class == "cannot-observe" && alert.Target == "redis-usage.example/"+observation {
			gap = alert
		}
	}
	if gap == nil {
		t.Fatal("Redis fullest-node authority assertion: incomplete native census became a complete healthy selection")
	}
	if gap.SignalID != "monitor/visibility" || gap.Frame != "" || gap.Severity != SeverityWarn || gap.Sustain != 2 ||
		!strings.Contains(gap.Observed, fmt.Sprintf("unobserved_nodes=%d", missing)) || !strings.Contains(gap.Observed, "fleet_fullest=unknown") {
		t.Fatal("fullest-node visibility lost its stable identity or exact source qualification")
	}
	return *gap
}

// Native Redis error text can have exit zero; nonempty output is not INFO.
func TestRedisUsageAuthorityNativeErrorIsUnknown(t *testing.T) {
	for _, reply := range []string{"", "NOAUTH synthetic-private-sentinel\n", "ERR synthetic-private-sentinel\n", "LOADING synthetic-private-sentinel\n", redisUsageAuthorityHealthy + "NOAUTH synthetic-private-sentinel\n"} {
		f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
			6380: {body: redisUsageAuthorityHealthy}, 6381: {body: reply},
		})
		alerts, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
		if err != nil {
			t.Fatal(err)
		}
		redisUsageAuthorityRequireGap(t, alerts, "byte-families", 1)
		if len(alerts) != 1 || len(f.samples) != 1 || !strings.Contains(f.samples[0], "-p 6380 ") {
			t.Fatal("a native error displaced the valid low-usage sibling or invented a breach")
		}
	}
}

// Even plausible complete fields are unavailable after a failed native child.
func TestRedisUsageAuthorityNativeExitSurvivesNormalization(t *testing.T) {
	for _, status := range []int{1, 124} {
		f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
			6380: {body: redisUsageAuthorityHealthy},
			6381: {body: "used_memory:990\nmaxmemory:1000\n", status: status},
		})
		alerts, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
		if err != nil {
			t.Fatal(err)
		}
		redisUsageAuthorityRequireGap(t, alerts, "byte-families", 1)
		if len(alerts) != 1 || !strings.Contains(f.samples[0], "-p 6380 ") {
			t.Fatal("failed INFO child became an authoritative high-usage node")
		}
	}
}

// Required values must appear once as nonnegative integers, within int64.
func TestRedisUsageAuthorityRequiredMeasurementsAreExact(t *testing.T) {
	for _, reply := range []string{
		"used_memory:900\n", "maxmemory:1000\n", "used_memory:900\nused_memory:900\nmaxmemory:1000\n",
		"used_memory:900\nmaxmemory:1000\nmaxmemory:1000\n", "used_memory:900:extra\nmaxmemory:1000\n",
		"used_memory:-1\nmaxmemory:1000\n", "used_memory:NaN\nmaxmemory:1000\n", "used_memory:Inf\nmaxmemory:1000\n",
		"used_memory:9e2\nmaxmemory:1000\n", "used_memory:900.0\nmaxmemory:1000\n",
		"used_memory:9223372036854775808\nmaxmemory:1000\n", "used_memory:900\nmaxmemory:-1\n",
	} {
		f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
			6380: {body: redisUsageAuthorityHealthy}, 6381: {body: reply},
		})
		alerts, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
		if err != nil {
			t.Fatal(err)
		}
		redisUsageAuthorityRequireGap(t, alerts, "byte-families", 1)
	}
}

// Explicit zero usage and absent optional INFO diagnostics remain valid.
func TestRedisUsageAuthorityCompleteZeroAndHealthySamples(t *testing.T) {
	for _, reply := range []string{redisUsageAuthorityHealthy, "used_memory:0\nmaxmemory:1000\n", redisUsageAuthorityHealthy + "optional_future_field:synthetic-private-sentinel\n"} {
		f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
			6380: {body: reply}, 6381: {body: reply},
		})
		alerts, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
		if err != nil || len(alerts) != 0 || len(f.samples) != 1 {
			t.Fatal("complete valid or explicit-zero usage census lost healthy authority")
		}
	}
}

// A larger byte total is not necessarily closest to its own ceiling. Later
// reset/boot metadata cannot pin selection to the former highest ratio.
func TestRedisUsageAuthoritySelectsCurrentRatioAfterReset(t *testing.T) {
	f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
		6380: {body: "used_memory:800\nmaxmemory:1000\nrun_id:synthetic-old\n"},
		6381: {body: "used_memory:7000\nmaxmemory:10000\nrun_id:synthetic-old\n"},
	})
	env := redisUsageAuthorityEnv(t, f.settings)
	_, node, failures, err := fullestRedisNodeUsage(context.Background(), env)
	if err != nil || len(failures) != 0 || node.port != 6380 {
		t.Fatal("complete census selected by bytes instead of utilization")
	}
	f.writeReply(t, 6380, redisUsageAuthorityReply{body: "used_memory:0\nmaxmemory:1000\nrun_id:synthetic-new\n"})
	_, node, failures, err = fullestRedisNodeUsage(context.Background(), env)
	if err != nil || len(failures) != 0 || node.port != 6381 {
		t.Fatal("current zero/reset reading did not replace the prior fullest node")
	}
}

// Reconcile exact configured membership even when the outer transport succeeds.
func TestRedisUsageAuthorityCompactCensusIsExact(t *testing.T) {
	for _, sample := range []struct {
		rows    string
		missing int
	}{
		{rows: "6380 100 1000", missing: 1},
		{rows: "6380 100 1000\n6381 unreachable", missing: 1},
		{rows: "6380 100 1000\n6381 900 1000\n6381 900 1000", missing: 1},
		{rows: "6380 100 1000\n6381 100 1000\n6399 990 1000", missing: 0},
		{rows: "6380 100 1000\n6381 900 1000 synthetic-private-sentinel", missing: 1},
		{rows: "6380 100 1000\n6381 NaN 1000", missing: 1},
		{rows: "6380 100 1000\n6381 -1 1000", missing: 1},
		{rows: "6380 100 1000\n6381 1 9223372036854775808", missing: 1},
	} {
		f := newRedisUsageAuthorityFixture(t, nil)
		f.source.hostFn = func(h HostSettings, command string) (string, error) {
			if strings.Contains(command, "INFO memory") {
				return sample.rows, nil
			}
			f.samples = append(f.samples, h.Name+" "+command)
			return syntheticRedisByteSample, nil
		}
		alerts, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
		if err != nil {
			t.Fatal(err)
		}
		redisUsageAuthorityRequireGap(t, alerts, "byte-families", sample.missing)
		if len(alerts) != 1 || !strings.Contains(f.samples[0], "-p 6380 ") {
			t.Fatal("an invalid, duplicate, or foreign row displaced the valid configured node")
		}
	}
}

// Partial census samples retain a known family breach but cannot enter the
// full-fleet baseline; a corrected complete reading restores publication.
func TestRedisUsageAuthorityPartialFamilyBaselineAndCorrection(t *testing.T) {
	const metric = "redis/family/ckey_<id>"
	f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
		6380: {body: redisUsageAuthorityHealthy}, 6381: {body: "NOAUTH synthetic-private-sentinel\n"},
	})
	f.settings.StateDir = t.TempDir()
	populateMetric(t, f.settings.StateDir, metric, 1000, 1000, 1000)
	for _, missing := range []int{1, 0} {
		if missing == 0 {
			f.writeReply(t, 6381, redisUsageAuthorityReply{body: "used_memory:900\nmaxmemory:1000\n"})
		}
		alerts, err := NewKeyFamiliesSignal().Run(context.Background(), f.settings)
		if err != nil {
			t.Fatal(err)
		}
		growth := requireAlertClass(t, alerts, "family-growth")
		if missing == 1 {
			redisUsageAuthorityRequireGap(t, alerts, "key-families", 1)
			if !strings.Contains(growth.Observed, "fleet_fullest=unknown") || growth.Target != "redis-usage.example:6380" {
				t.Fatal("partial family breach lost selection qualification or node identity")
			}
		} else if len(alerts) != 1 || growth.Target != "redis-usage.example:6381" || strings.Contains(growth.Observed, "fleet_fullest=unknown") {
			t.Fatal("corrected census did not restore actual fullest-node authority")
		}
		store, err := newBaselineStore(f.settings.StateDir + "/baseline")
		if err != nil {
			t.Fatal(err)
		}
		if len(store.trailingWindow(metric, 24*time.Hour)) != 4-missing {
			t.Fatal("partial selection polluted the fleet baseline or correction failed to restore it")
		}
	}
}

// Absence of a breach on the observed subset cannot clear a fleet finding.
func TestRedisUsageAuthorityPartialHealthyDoesNotClear(t *testing.T) {
	for _, p := range []probe{redisFamilyProbe{}, redisBytesProbe{}} {
		f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
			6380: {body: redisUsageAuthorityHealthy}, 6381: {body: "", status: 124},
		})
		findings, err := p.check(context.Background(), redisUsageAuthorityEnv(t, f.settings))
		if err != nil || len(findings) != 1 || findings[0].class != "cannot-observe" {
			t.Fatal("partial healthy sample established or cleared a fleet verdict")
		}
	}
}

// An owned byte-dominance breach is still useful when another node is unknown.
func TestRedisUsageAuthorityPartialByteBreachRetained(t *testing.T) {
	f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
		6380: {body: "used_memory:850\nmaxmemory:1000\n"}, 6381: {body: "", status: 124},
	})
	alerts, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
	if err != nil {
		t.Fatal(err)
	}
	redisUsageAuthorityRequireGap(t, alerts, "byte-families", 1)
	breach := requireAlertClass(t, alerts, "score-byte-dominance")
	if len(alerts) != 2 || breach.Target != "redis-usage.example:6380" || breach.Frame != "score" ||
		!strings.Contains(breach.Observed, "fleet_fullest=unknown") || !strings.Contains(breach.Observed, "unobserved_nodes=1") {
		t.Fatal("partial census discarded or misattributed the known byte threshold breach")
	}
}

// Unlimited maxmemory is a valid source observation with no utilization rank.
func TestRedisUsageAuthorityUnboundedCeilingRemainsQualified(t *testing.T) {
	f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
		6380: {body: redisUsageAuthorityHealthy}, 6381: {body: "used_memory:900\nmaxmemory:0\n"},
	})
	alerts, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
	if err != nil {
		t.Fatal(err)
	}
	gap := redisUsageAuthorityRequireGap(t, alerts, "byte-families", 0)
	if !strings.Contains(gap.Observed, "unbounded_nodes=1") || !strings.Contains(gap.Observed, "unbounded_ports=[6381]") {
		t.Fatal("valid unlimited ceiling was confused with a missing source measurement")
	}
	f.writeReply(t, 6380, redisUsageAuthorityReply{body: "used_memory:0\nmaxmemory:0\n"})
	if _, err := NewRedisBytesSignal().Run(context.Background(), f.settings); err == nil {
		t.Fatal("entirely unbounded census claimed a percentage-fullest node")
	}
}

// When nothing is selectable, fail without copying native replies or sampling.
func TestRedisUsageAuthorityAllUnavailableIsSanitized(t *testing.T) {
	f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
		6380: {body: "NOAUTH synthetic-private-sentinel\n"}, 6381: {body: "ERR synthetic-private-sentinel\n"},
	})
	_, err := NewRedisBytesSignal().Run(context.Background(), f.settings)
	if err == nil || strings.Contains(err.Error(), "synthetic-private-sentinel") || len(f.samples) != 0 {
		t.Fatal("unreadable census became a sample or leaked the raw source error")
	}
	f.source.hostFn = func(HostSettings, string) (string, error) {
		return "synthetic-private-sentinel", nil
	}
	_, err = NewRedisBytesSignal().Run(context.Background(), f.settings)
	if err == nil || strings.Contains(err.Error(), "synthetic-private-sentinel") || len(f.samples) != 0 {
		t.Fatal("invalid compact census leaked its raw body through the probe error")
	}
}

// Contact only the configured sparse census; disabled inventory is not a gap.
func TestRedisUsageAuthoritySparseAndDisabledInventory(t *testing.T) {
	f := newRedisUsageAuthorityFixture(t, map[int]redisUsageAuthorityReply{
		6380: {body: redisUsageAuthorityHealthy}, 6382: {body: redisUsageAuthorityHealthy},
	})
	f.settings.Hosts[1].RedisNodePorts = []int{6380, 6382}
	f.settings.disabledHosts = []HostSettings{{Name: "redis-disabled.example", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6390}}}
	_, node, failures, err := fullestRedisNodeUsage(context.Background(), redisUsageAuthorityEnv(t, f.settings))
	if err != nil || len(failures) != 0 || node.port != 6380 {
		t.Fatal("sparse configured census failed or disabled inventory was treated as unavailable")
	}
	calls, err := os.ReadFile(f.calls)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(strings.Fields(string(calls)), []string{"6380", "6382"}) {
		t.Fatal("usage collector contacted a node outside the configured enabled census")
	}
}

// A gap on every host does not erase valid siblings on every host.
func TestRedisUsageAuthorityEveryHostPartialStillSelectsKnownNode(t *testing.T) {
	f := newRedisUsageAuthorityFixture(t, nil)
	f.settings.Hosts = append(f.settings.Hosts, HostSettings{Name: "redis-second.example", Roles: []string{"redis-cluster"}, RedisNodePorts: []int{6390, 6391}})
	f.source.hostFn = func(h HostSettings, _ string) (string, error) {
		if h.Name == "redis-usage.example" {
			return "6380 100 1000", nil
		}
		return "6390 900 1000", nil
	}
	h, node, failures, err := fullestRedisNodeUsage(context.Background(), redisUsageAuthorityEnv(t, f.settings))
	if err != nil || len(failures) != 2 || node.port != 6390 || h == nil || h.name != "redis-second.example" {
		t.Fatal("per-host census gaps erased the actual fullest observed sibling")
	}
}
