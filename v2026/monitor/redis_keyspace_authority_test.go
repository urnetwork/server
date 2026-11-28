// Exercise the actual collectors and public alerts with private native fakes.
// The same fixture accepts both original and repaired INFO invocations for RED.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Native status is independent of a plausible or malformed response body.
type redisKeyspaceAuthorityReply struct {
	body   string
	status int
}

// Each fixture owns its replies, native call log, and synthetic detail samples.
type redisKeyspaceAuthorityFixture struct {
	settings SignalSettings
	dir      string
	samples  []string
}

// Run the emitted shell against fake redis-cli/timeout only. The process-group
// helper is the existing local fixture boundary, not an SSH lifecycle claim.
func newRedisKeyspaceAuthorityFixture(t *testing.T, ports []int, replies map[int]redisKeyspaceAuthorityReply) *redisKeyspaceAuthorityFixture {
	t.Helper()
	f := &redisKeyspaceAuthorityFixture{dir: t.TempDir()}
	for _, port := range ports {
		reply, ok := replies[port]
		if !ok {
			reply = redisKeyspaceAuthorityReply{body: "# Keyspace\r\n"}
		}
		f.writeReply(t, port, reply)
	}
	for name, script := range map[string]string{
		"redis-cli": `#!/bin/sh
[ "$#" -ge 4 ] && [ "$1" = -p ] || exit 126
port=$2
shift 2
[ "$1" != --raw ] || shift
[ "$#" -eq 2 ] || exit 126
printf '%s %s\n' "$1" "$port" >> "$KEYSPACE_TEST_DIR/calls"
printf '%s\n' synthetic-private-sentinel >&2
case "$1 $2" in
  'INFO keyspace')
    cat "$KEYSPACE_TEST_DIR/$port.reply" || exit 126
    exit "$(cat "$KEYSPACE_TEST_DIR/$port.status")"
    ;;
  'CLUSTER NODES')
    printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 192.0.2.1:%s@16380 myself,master - 0 0 1 connected 0-511\r\n' "$port"
    ;;
  *) exit 126 ;;
esac
`,
		"timeout": "#!/bin/sh\n[ \"$1\" = 3 ] || exit 126\nshift\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "EVAL_RO") {
			f.samples = append(f.samples, command)
			if strings.Contains(command, "timeout 45") {
				return redisPersistentSampleFixture(map[string]redisPersistentFamily{
					"other": {sampled: 5000, persistent: 5000},
				}), nil
			}
			return "5000 10 0 0 0 0 10 2000 300 28799985312000000 other", nil
		}
		if !strings.Contains(command, "INFO keyspace") {
			t.Fatal("unexpected synthetic keyspace command")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := redisAuthorityNativeCommand(ctx, command)
		cmd.Env = []string{"PATH=" + f.dir + ":/usr/bin:/bin", "LC_ALL=C", "KEYSPACE_TEST_DIR=" + f.dir}
		out, err := cmd.Output()
		redisAuthorityKillNativeGroup(cmd)
		if err != nil {
			t.Fatal("synthetic keyspace collector failed before its authority assertion")
		}
		if strings.Contains(string(out), "synthetic-private-sentinel") {
			t.Fatal("keyspace native privacy assertion: raw response crossed the aggregate boundary")
		}
		return string(out), nil
	}}
	f.settings = syntheticSettings(source)
	f.settings.Hosts[1].Name = "redis-keyspace.example"
	f.settings.Hosts[1].RedisNodePorts = append([]int(nil), ports...)
	return f
}

// Replace the next native result without retaining the previous observation.
func (self *redisKeyspaceAuthorityFixture) writeReply(t *testing.T, port int, reply redisKeyspaceAuthorityReply) {
	t.Helper()
	for suffix, value := range map[string]string{"reply": reply.body, "status": fmt.Sprint(reply.status)} {
		if err := os.WriteFile(filepath.Join(self.dir, fmt.Sprintf("%d.%s", port, suffix)), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Check the real internal verdict where the public adapter omits healthy rows.
func redisKeyspaceAuthorityFindings(t *testing.T, settings SignalSettings, p probe) ([]finding, error) {
	t.Helper()
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	return p.check(context.Background(), env)
}

// Private native bytes must be absent from all public fields and identity.
func redisKeyspaceAuthorityPrivate(t *testing.T, alerts Alerts) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-private-sentinel") || strings.Contains(alerts.Markdown(), "synthetic-private-sentinel") {
		t.Fatal("Redis observation public privacy assertion: native bytes entered JSON or Markdown")
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Identity(), "synthetic-private-sentinel") {
			t.Fatal("Redis observation public privacy assertion: native bytes entered identity")
		}
	}
}

// Preserve existing signal identity, host scope, warning severity and sustain.
func redisKeyspaceAuthorityGap(t *testing.T, alerts Alerts, key, frame string) Alert {
	t.Helper()
	redisKeyspaceAuthorityPrivate(t, alerts)
	gap := requireAlertClass(t, alerts, "cannot-observe")
	if gap.SignalKey != key || gap.Severity != SeverityWarn || gap.Sustain != 2 || !gap.unobservable {
		t.Fatal("keyspace visibility identity or unknown-state classification changed")
	}
	if key == "ttl-leaks" {
		if gap.SignalID != "redis/ttl-leaks" || gap.Target != "redis-keyspace.example" || gap.Frame != frame {
			t.Fatal("TTL visibility lost its exact configured-host/port identity")
		}
	} else if gap.SignalID != "monitor/visibility" || gap.Target != "redis/nonexpiring-skew" || gap.Frame != "" {
		t.Fatal("nonexpiring whole-census failure lost its public visibility identity")
	}
	return gap
}

// A plausible body cannot override failure, and native error text can exit zero.
func TestRedisKeyspaceAuthorityNativeErrorsAreUnknown(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply redisKeyspaceAuthorityReply
	}{
		{name: "error with success status", reply: redisKeyspaceAuthorityReply{body: "NOAUTH synthetic-private-sentinel\n"}},
		{name: "failed error", reply: redisKeyspaceAuthorityReply{body: "ERR synthetic-private-sentinel\n", status: 1}},
		{name: "loading", reply: redisKeyspaceAuthorityReply{body: "LOADING synthetic-private-sentinel\n"}},
		{name: "failed valid prefix", reply: redisKeyspaceAuthorityReply{body: "# Keyspace\ndb0:keys=1,expires=1,avg_ttl=1\n", status: 1}},
		{name: "timeout prefix", reply: redisKeyspaceAuthorityReply{body: "# Keyspace\ndb0:keys=1,expires=1,avg_ttl=1\n", status: 124}},
		{name: "unavailable", reply: redisKeyspaceAuthorityReply{body: "# Keyspace\n", status: 127}},
		{name: "failed empty", reply: redisKeyspaceAuthorityReply{status: 1}},
		{name: "empty success", reply: redisKeyspaceAuthorityReply{}},
	} {
		for _, signal := range []Signal{NewTTLLeaksSignal(), NewRedisNonexpiringSignal()} {
			f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6381, 6382}, map[int]redisKeyspaceAuthorityReply{6380: test.reply, 6381: test.reply, 6382: test.reply})
			findings, err := redisKeyspaceAuthorityFindings(t, f.settings, signal.(*signalAdapter).probe)
			gap := err != nil
			for _, finding := range findings {
				if finding.healthy || finding.class != "cannot-observe" {
					t.Fatalf("keyspace native authority assertion: %s/%s acquired data-state authority from failed or non-protocol INFO", signal.Key(), test.name)
				}
				gap = true
			}
			if !gap {
				t.Fatalf("keyspace native authority assertion: %s/%s lost visibility", signal.Key(), test.name)
			}
			alerts, _ := NewWithSignals(f.settings, signal).Run(context.Background())
			redisKeyspaceAuthorityGap(t, alerts, signal.Key(), "6380,6381,6382")
			if len(alerts) != 1 || len(f.samples) != 0 {
				t.Fatal("invalid keyspace census invented a data-state alert or detail sample")
			}
		}
	}
}

// The public path independently rejects exit-zero Redis error replies. Keeping
// this separate gives RED a causal public assertion without new helper symbols.
func TestRedisKeyspaceAuthorityPublicNativeErrors(t *testing.T) {
	for _, signal := range []Signal{NewTTLLeaksSignal(), NewRedisNonexpiringSignal()} {
		reply := redisKeyspaceAuthorityReply{body: "NOAUTH synthetic-private-sentinel\n"}
		f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6381, 6382}, map[int]redisKeyspaceAuthorityReply{6380: reply, 6381: reply, 6382: reply})
		alerts, _ := NewWithSignals(f.settings, signal).Run(context.Background())
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Errorf("keyspace public authority assertion: %s accepted error text as a healthy empty census", signal.Key())
			continue
		}
		redisKeyspaceAuthorityGap(t, alerts, signal.Key(), "6380,6381,6382")
	}
}

// Only a complete fixed frame can supply counts; numeric checks are exact even
// beyond float64 integer precision, and CR is legal only at the line boundary.
func TestRedisKeyspaceAuthorityNativeFramesAreExact(t *testing.T) {
	for index, body := range []string{
		"db0:keys=1,expires=1,avg_ttl=1\n",
		"# Keyspace\n# Keyspace\n",
		"# Keyspace\ndb0:keys=1,expires=1\n",
		"# Keyspace\ndb0:keys=1,keys=1,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=1,expires=1,avg_ttl=1\ndb0:keys=1,expires=1,avg_ttl=1\n",
		"# Keyspace\ndb1:keys=1,expires=1,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=1,expires=1,avg_ttl=1,extra=1\n",
		"# Keyspace\ndb0:keys=synthetic-private-sentinel,expires=1,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=1,expires=-1,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=1,expires=1,avg_ttl=-1\n",
		"# Keyspace\ndb0:keys=1,expires=2,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=9007199254740992,expires=9007199254740993,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=9223372036854775808,expires=1,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=1,expires=9223372036854775808,avg_ttl=1\n",
		"# Keyspace\ndb0:keys=1,expires=1,avg_ttl=9223372036854775808\n",
		"# Keyspace\ndb0:keys=1,expires=1,avg_ttl=NaN\n",
		"# Keyspace\ndb0:keys=1,expires=1,avg_ttl=1e9\n",
		"# Keyspace\ndb0:keys=1,expires=1,avg_ttl=1\r0\n",
		"# Keyspace\nERR synthetic-private-sentinel\n",
	} {
		for _, signal := range []Signal{NewTTLLeaksSignal(), NewRedisNonexpiringSignal()} {
			reply := redisKeyspaceAuthorityReply{body: body}
			f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6381, 6382}, map[int]redisKeyspaceAuthorityReply{6380: reply, 6381: reply, 6382: reply})
			alerts, _ := NewWithSignals(f.settings, signal).Run(context.Background())
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
				t.Fatalf("keyspace frame authority assertion: case %d/%s accepted malformed native aggregates", index, signal.Key())
			}
			redisKeyspaceAuthorityGap(t, alerts, signal.Key(), "6380,6381,6382")
		}
	}
}

// Successful empty databases, explicit zero, annual escrow and the exact
// two-year threshold remain healthy; field order carries no meaning.
func TestRedisKeyspaceAuthorityNativeHealthyAndZero(t *testing.T) {
	for index, body := range []string{
		"# Keyspace\r\n",
		"# Keyspace\ndb0:keys=0,expires=0,avg_ttl=0\n",
		"# Keyspace\ndb0:keys=1000,expires=900,avg_ttl=34128000000\n",
		"# Keyspace\ndb0:keys=1000,expires=900,avg_ttl=63072000000\n",
		"# Keyspace\ndb0:avg_ttl=1000,keys=1000,expires=900\n",
		"# Keyspace\ndb0:keys=9223372036854775807,expires=9223372036854775807,avg_ttl=0\n",
	} {
		for _, signal := range []Signal{NewTTLLeaksSignal(), NewRedisNonexpiringSignal()} {
			reply := redisKeyspaceAuthorityReply{body: body}
			f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6381, 6382}, map[int]redisKeyspaceAuthorityReply{6380: reply, 6381: reply, 6382: reply})
			findings, err := redisKeyspaceAuthorityFindings(t, f.settings, signal.(*signalAdapter).probe)
			if err != nil || len(findings) != 1 || !findings[0].healthy {
				t.Fatalf("keyspace healthy control assertion: case %d/%s rejected a complete valid census", index, signal.Key())
			}
			alerts, err := NewWithSignals(f.settings, signal).Run(context.Background())
			if err != nil || len(alerts) != 0 || len(f.samples) != 0 {
				t.Fatal("valid keyspace control emitted a public alert or unnecessary sample")
			}
		}
	}
}

// An unread sibling cannot erase a breach from a valid configured node.
func TestRedisKeyspaceAuthorityNativeIndependentTtlBreach(t *testing.T) {
	f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6381, 6382}, map[int]redisKeyspaceAuthorityReply{
		6380: {body: "# Keyspace\ndb0:keys=100,expires=90,avg_ttl=63072000001\n"},
		6382: {body: "NOAUTH synthetic-private-sentinel\n"},
	})
	alerts, err := NewTTLLeaksSignal().Run(context.Background(), f.settings)
	if err != nil {
		t.Fatal(err)
	}
	gap := redisKeyspaceAuthorityGap(t, alerts, "ttl-leaks", "6382")
	breach := requireAlertClass(t, alerts, "ttl-leaks")
	if len(alerts) != 2 || gap.Identity() == breach.Identity() || breach.SignalID != "redis/ttl-leaks" ||
		breach.Frame != "duration-as-seconds" || breach.Target != "redis-keyspace.example" || breach.Sustain != 1 ||
		!strings.Contains(breach.Observed, "affected_ports=6380") || !strings.Contains(breach.Observed, "census_complete=false") ||
		!strings.Contains(breach.Observed, "sample_other=10") || strings.Contains(breach.Action, "expire-leaked-ttls") ||
		len(f.samples) != 1 || !strings.Contains(f.samples[0], "-p 6380 ") {
		t.Fatal("partial keyspace scope lost its independent breach, detail owner, or attribution boundary")
	}
}

// Valid keyspace plus independently positive slot counts still proves skew.
func TestRedisKeyspaceAuthorityNativeNonexpiringBreach(t *testing.T) {
	f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6381, 6382}, map[int]redisKeyspaceAuthorityReply{
		6380: {body: "# Keyspace\ndb0:keys=1100000,expires=100000,avg_ttl=1000\n"},
		6381: {body: "# Keyspace\ndb0:keys=1100000,expires=100000,avg_ttl=1000\n"},
		6382: {body: "# Keyspace\ndb0:keys=1400000,expires=100000,avg_ttl=1000\n"},
	})
	alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), f.settings)
	if err != nil {
		t.Fatal(err)
	}
	redisKeyspaceAuthorityPrivate(t, alerts)
	breach := requireAlertClass(t, alerts, "nonexpiring-key-skew")
	if len(alerts) != 1 || breach.Target != "redis-keyspace.example:6382" || breach.Sustain != 1 ||
		!strings.Contains(breach.Observed, "estimated_excess_keys=300000") || len(f.samples) != 2 {
		t.Fatal("native keyspace authority correction lost the independent nonexpiring positive")
	}
}

// Inventory is a set of configured ports, not an inclusive first/last range.
func TestRedisKeyspaceAuthorityNativeExactConfiguredPorts(t *testing.T) {
	for _, signal := range []Signal{NewTTLLeaksSignal(), NewRedisNonexpiringSignal()} {
		f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6382, 6384}, nil)
		alerts, err := NewWithSignals(f.settings, signal).Run(context.Background())
		calls, readErr := os.ReadFile(filepath.Join(f.dir, "calls"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		want := "INFO 6380\nINFO 6382\nINFO 6384\n"
		if signal.Key() == "redis-nonexpiring" {
			want = "INFO 6380\nCLUSTER 6380\nINFO 6382\nCLUSTER 6382\nINFO 6384\nCLUSTER 6384\n"
		}
		if string(calls) != want {
			t.Fatalf("keyspace port authority assertion: %s contacted a missing or unconfigured native port", signal.Key())
		}
		if err != nil || len(alerts) != 0 {
			t.Fatal("complete noncontiguous configured census was not healthy")
		}
	}
}

// Exact reducer coverage independently guards transport-projected rows. These
// tests use the existing public probe, so the same source compiles before repair.
func TestRedisTtlAuthorityPartialRowsAreUnknown(t *testing.T) {
	for _, test := range []struct {
		name, out, frame string
		breach           bool
	}{
		{name: "missing", out: "6380 0 0 0", frame: "6382"},
		{name: "empty", frame: "6380,6382"},
		{name: "unreachable", out: "6380 0 0 0\n6382 unreachable", frame: "6382"},
		{name: "unknown", out: "6380 0 0 0\n6382 0 0 0\n6381 0 0 0", frame: "6380,6382"},
		{name: "short", out: "6380 0 0 0\n6382 1 1", frame: "6382"},
		{name: "extra", out: "6380 0 0 0\n6382 1 1 1 extra", frame: "6382"},
		{name: "malformed", out: "6380 0 0 0\n6382 synthetic-private-sentinel 1 1", frame: "6382"},
		{name: "unowned malformed", out: "6380 0 0 0\n6382 0 0 0\nsynthetic-private-sentinel", frame: "6380,6382"},
		{name: "overflow", out: "6380 0 0 0\n6382 9223372036854775808 0 0", frame: "6382"},
		{name: "negative", out: "6380 0 0 0\n6382 1 -1 1", frame: "6382"},
		{name: "inconsistent", out: "6380 0 0 0\n6382 1 2 1", frame: "6382"},
		{name: "duplicate rejects own breach", out: "6380 1 1 63072000001\n6380 0 0 0\n6382 0 0 0", frame: "6380"},
		{name: "duplicate reversed", out: "6380 0 0 0\n6380 1 1 63072000001\n6382 0 0 0", frame: "6380"},
		{name: "independent breach", out: "6380 1 1 63072000001\n6382 unreachable", frame: "6382", breach: true},
		{name: "independent breach with duplicate sibling", out: "6380 1 1 63072000001\n6382 0 0 0\n6382 0 0 0", frame: "6382", breach: true},
	} {
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "EVAL_RO") {
				return "5000 10 0 0 0 0 10 2000 300 28799985312000000 other", nil
			}
			return test.out, nil
		}}
		settings := syntheticSettings(source)
		settings.Hosts[1].Name = "redis-keyspace.example"
		settings.Hosts[1].RedisNodePorts = []int{6380, 6382}
		findings, err := redisKeyspaceAuthorityFindings(t, settings, redisTTLLeaksProbe{})
		if err != nil {
			t.Fatalf("TTL census authority assertion: %s failed before returning scoped visibility", test.name)
		}
		for _, finding := range findings {
			if finding.healthy {
				t.Fatalf("TTL census authority assertion: %s incomplete scope emitted health", test.name)
			}
		}
		alerts, err := NewTTLLeaksSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisKeyspaceAuthorityGap(t, alerts, "ttl-leaks", test.frame)
		want := 1
		if test.breach {
			want++
			requireAlertClass(t, alerts, "ttl-leaks")
		}
		if len(alerts) != want {
			t.Fatalf("TTL census authority assertion: %s invented or erased an independently valid breach", test.name)
		}
	}
}

// Reusing a signal across cadences must consume the current response, never an
// older healthy/broken result. The controlled clock advances without sleeping.
func TestRedisKeyspaceAuthorityCurrentObservationReplacesPrior(t *testing.T) {
	for _, signal := range []Signal{NewTTLLeaksSignal(), NewRedisNonexpiringSignal()} {
		f := newRedisKeyspaceAuthorityFixture(t, []int{6380, 6381, 6382}, nil)
		now := f.settings.Now()
		f.settings.Now = func() time.Time { return now }
		monitor := NewWithSignals(f.settings, signal)
		alerts, err := monitor.Run(context.Background())
		if err != nil || len(alerts) != 0 {
			t.Fatal("freshness control did not begin healthy")
		}
		now = now.Add(15 * time.Minute)
		for _, port := range []int{6380, 6381, 6382} {
			f.writeReply(t, port, redisKeyspaceAuthorityReply{body: "NOAUTH synthetic-private-sentinel\n"})
		}
		alerts, _ = monitor.Run(context.Background())
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Fatal("keyspace freshness authority assertion: an older healthy census hid current observation loss")
		}
		redisKeyspaceAuthorityGap(t, alerts, signal.Key(), "6380,6381,6382")
		now = now.Add(15 * time.Minute)
		for _, port := range []int{6380, 6381, 6382} {
			f.writeReply(t, port, redisKeyspaceAuthorityReply{body: "# Keyspace\r\n"})
		}
		alerts, err = monitor.Run(context.Background())
		if err != nil || len(alerts) != 0 {
			t.Fatal("corrected current native census did not restore healthy authority")
		}
	}
}
