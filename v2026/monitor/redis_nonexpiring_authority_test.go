// Exercise native slot completion and complete-cohort authority through the
// real signal. Native fakes accept the original and repaired collector argv.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Forward the actual caller context. Individual fixtures own their source
// lifecycle policy, just as the production Host implementation does.
type redisNonexpiringAuthoritySource struct {
	*syntheticSource
	host func(context.Context, HostSettings, string) (string, error)
}

func (self *redisNonexpiringAuthoritySource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.host(ctx, host, command)
}

// All identities and native response bytes are synthetic and fixture-local.
type redisNonexpiringAuthorityFixture struct {
	settings      SignalSettings
	dir           string
	downHost      string
	privateDown   bool
	candidatePort int
	samples       []string
}

// Two disjoint 1,024-slot ranges make each of eight masters own 2,048 slots.
func redisNonexpiringAuthorityNode(port int, slots string) string {
	return fmt.Sprintf("%040x 192.0.2.1:%d@16380 myself,master - 0 0 1 connected %s\r\n", port, port, slots)
}

// Each port keeps independent INFO and CLUSTER replies and producer status.
func (self *redisNonexpiringAuthorityFixture) write(t *testing.T, port int, kind, body string, status int) {
	t.Helper()
	for suffix, value := range map[string]string{"reply": body, "status": fmt.Sprint(status)} {
		path := filepath.Join(self.dir, fmt.Sprintf("%d.%s.%s", port, kind, suffix))
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// INFO uses exact native counters; the independently measured density changes
// only when this fixture replaces that port's current response.
func (self *redisNonexpiringAuthorityFixture) density(t *testing.T, port, density int) {
	t.Helper()
	self.write(t, port, "INFO", fmt.Sprintf("# Keyspace\r\ndb0:keys=%d,expires=100,avg_ttl=1000\r\n", density*2048+100), 0)
}

// Run the actual emitted shell only against private redis-cli/timeout fakes.
// The existing native helper owns the local process group, not remote SSH.
func newRedisNonexpiringAuthorityFixture(t *testing.T, groups [][]int, densities []int) *redisNonexpiringAuthorityFixture {
	t.Helper()
	f := &redisNonexpiringAuthorityFixture{dir: t.TempDir()}
	for index, density := range densities {
		port := 6380 + index
		f.density(t, port, density)
		first := index * 2048
		f.write(t, port, "CLUSTER", redisNonexpiringAuthorityNode(port, fmt.Sprintf("%d-%d %d-%d", first, first+1023, first+1024, first+2047)), 0)
		if f.candidatePort == 0 || density > densities[f.candidatePort-6380] {
			f.candidatePort = port
		}
	}
	for name, script := range map[string]string{
		"redis-cli": `#!/bin/sh
[ "$#" -ge 4 ] && [ "$1" = -p ] || exit 126
port=$2
shift 2
[ "$1" != --raw ] || shift
[ "$#" -eq 2 ] || exit 126
case "$1 $2" in
  'INFO keyspace'|'CLUSTER NODES') ;;
  *) exit 126 ;;
esac
printf '%s %s\n' "$1" "$port" >> "$NONEXPIRING_TEST_DIR/calls"
printf '%s\n' synthetic-private-sentinel >&2
[ -r "$NONEXPIRING_TEST_DIR/$port.$1.reply" ] || exit 126
reply_status=$(cat "$NONEXPIRING_TEST_DIR/$port.$1.status") || exit 126
cat "$NONEXPIRING_TEST_DIR/$port.$1.reply"
printf '%s %s %s\n' "$1" "$port" "$reply_status" >> "$NONEXPIRING_TEST_DIR/exits"
exit "$reply_status"
`,
		"timeout": "#!/bin/sh\n[ \"$1\" = 3 ] || exit 126\nshift\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	candidate, control := redisPersistentProvideFixtures()
	source := &redisNonexpiringAuthoritySource{syntheticSource: &syntheticSource{}, host: func(parentCtx context.Context, configured HostSettings, command string) (string, error) {
		if err := parentCtx.Err(); err != nil {
			return "", err
		}
		if configured.Name == f.downHost {
			if f.privateDown {
				return "synthetic-private-sentinel", fmt.Errorf("synthetic-private-sentinel")
			}
			return "", fmt.Errorf("synthetic host unavailable")
		}
		if strings.Contains(command, "EVAL_RO") {
			f.samples = append(f.samples, command)
			if strings.Contains(command, fmt.Sprintf("-p %d ", f.candidatePort)) {
				return candidate, nil
			}
			return control, nil
		}
		if !strings.Contains(command, "INFO keyspace") {
			t.Fatal("unexpected synthetic nonexpiring collector")
		}
		ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
		defer cancel()
		cmd := redisAuthorityNativeCommand(ctx, command)
		cmd.Env = []string{"PATH=" + f.dir + ":/usr/bin:/bin", "LC_ALL=C", "TMPDIR=" + f.dir, "NONEXPIRING_TEST_DIR=" + f.dir}
		out, err := cmd.Output()
		redisAuthorityKillNativeGroup(cmd)
		if parentErr := parentCtx.Err(); parentErr != nil {
			return string(out), errors.Join(parentErr, err)
		}
		if err != nil {
			t.Fatal("synthetic nonexpiring collector failed before its authority assertion")
		}
		if strings.Contains(string(out), "synthetic-private-sentinel") {
			t.Fatal("nonexpiring native privacy assertion: raw bytes crossed the aggregate boundary")
		}
		entries, err := os.ReadDir(f.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "monitor-redis-slots.") {
				t.Fatal("nonexpiring native privacy assertion: completed collection retained its private topology")
			}
		}
		return string(out), nil
	}}
	f.settings = syntheticSettings(source)
	f.settings.Hosts = f.settings.Hosts[:1]
	for index, ports := range groups {
		configured := HostSettings{Name: fmt.Sprintf("redis-nonexpiring-%d.example", index), Roles: []string{"redis-cluster"}, RedisNodePorts: append([]int(nil), ports...)}
		if index == 0 {
			configured.RedisEntryPort = 6379
		}
		f.settings.Hosts = append(f.settings.Hosts, configured)
	}
	return f
}

// Public rendering and identity cannot carry native output or transport errors.
func redisNonexpiringAuthorityPrivate(t *testing.T, alerts Alerts, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "synthetic-private-sentinel") {
		t.Fatal("nonexpiring privacy assertion: raw transport error entered the public error")
	}
	encoded, encodeErr := json.Marshal(alerts)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if strings.Contains(string(encoded), "synthetic-private-sentinel") || strings.Contains(alerts.Markdown(), "synthetic-private-sentinel") {
		t.Fatal("nonexpiring privacy assertion: raw source entered public JSON or Markdown")
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Identity(), "synthetic-private-sentinel") {
			t.Fatal("nonexpiring privacy assertion: raw source entered public identity")
		}
	}
}

// Observation loss is explicit and has neither product nor family authority.
func redisNonexpiringAuthorityUnknown(t *testing.T, f *redisNonexpiringAuthorityFixture, target string) {
	t.Helper()
	alerts, err := NewWithSignals(f.settings, NewRedisNonexpiringSignal()).Run(context.Background())
	redisNonexpiringAuthorityPrivate(t, alerts, err)
	if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatal("nonexpiring source authority assertion: incomplete evidence acquired a product or healthy verdict")
	}
	gap := alerts[0]
	if gap.SignalKey != "redis-nonexpiring" || gap.SignalNumber != "3.3c" || gap.SignalID != "monitor/visibility" ||
		gap.Target != target || gap.Frame != "" || gap.Severity != SeverityWarn || gap.Sustain != 2 || !gap.unobservable {
		t.Fatal("nonexpiring visibility identity, severity, sustain or unknown state changed")
	}
	if len(f.samples) != 0 || strings.Contains(gap.Markdown(), "EXPIRE-NX") {
		t.Fatal("nonexpiring source authority assertion: incomplete census acquired family attribution or maintenance authority")
	}
	env, envErr := newProbeEnv(f.settings.withDefaults())
	if envErr != nil {
		t.Fatal(envErr)
	}
	findings, checkErr := (redisNonexpiringProbe{}).check(context.Background(), env)
	if checkErr == nil && len(findings) == 0 {
		t.Fatal("nonexpiring internal authority assertion: missing census returned no unknown state")
	}
	for _, finding := range findings {
		if finding.healthy || finding.class != "cannot-observe" {
			t.Fatal("nonexpiring internal authority assertion: missing census emitted health or a relative product verdict")
		}
	}
}

// A native failure is decisive even when stdout contains the full positive
// range set or a plausible half-prefix that would double a healthy density.
func TestRedisNonexpiringAuthorityNativeCompletion(t *testing.T) {
	for _, status := range []int{1, 124, 127, 137} {
		for _, slots := range []string{"4096-5119 5120-6143", "4096-5119"} {
			f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}}, []int{100, 100, 100})
			f.write(t, 6382, "CLUSTER", redisNonexpiringAuthorityNode(6382, slots), status)
			redisNonexpiringAuthorityUnknown(t, f, "redis/nonexpiring-skew")
		}
	}
}

// Untrusted transport errors use the same fixed public failure boundary.
func TestRedisNonexpiringAuthorityTransportPrivacy(t *testing.T) {
	f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}}, []int{100, 100, 100})
	f.downHost = "redis-nonexpiring-0.example"
	f.privateDown = true
	redisNonexpiringAuthorityUnknown(t, f, "redis/nonexpiring-skew")
}

// Cancellation before collection keeps source custody and publishes no verdict.
func TestRedisNonexpiringAuthorityCanceledRunDoesNotPublish(t *testing.T) {
	f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}}, []int{100, 100, 200})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewRedisNonexpiringSignal().Run(ctx, f.settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || len(f.samples) != 0 {
		t.Fatal("canceled nonexpiring source acquired product or healthy authority")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "calls")); !os.IsNotExist(err) {
		t.Fatal("canceled nonexpiring source contacted a native producer")
	}
}

// Explicit native barriers keep a positive prefix pending until the producer
// fails or the caller cancels. Ordinary completion reaps both native PIDs;
// cancellation joins the existing group owner's direct child only, without
// asserting grandchild reaping or a remote SSH lifecycle guarantee.
func TestRedisNonexpiringAuthorityNativeJoinsAndCancellation(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, cancelPending := range []bool{false, true} {
			f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}}, []int{100, 100, 100})
			if scoped {
				var err error
				f.settings, err = ExcludeHosts(f.settings, "pg-1")
				if err != nil {
					t.Fatal("could not enable the unrelated native fixture scope")
				}
			}
			if err := os.Rename(filepath.Join(f.dir, "redis-cli"), filepath.Join(f.dir, "redis-cli-complete")); err != nil {
				t.Fatal(err)
			}
			for name, script := range map[string]string{
				"redis-cli": `#!/bin/sh
if [ "$*" = '-p 6380 --raw CLUSTER NODES' ]; then
  cat "$NONEXPIRING_TEST_DIR/6380.CLUSTER.reply" || exit 126
  printf 'writer %s\n' "$$" >&3
  IFS= read -r native_release <&4
  exit 137
fi
exec "$NONEXPIRING_TEST_DIR/redis-cli-complete" "$@"
`,
				"head": "#!/bin/sh\n[ \"$*\" = '-c 262145' ] || exit 126\nprintf 'reader %s\\n' \"$$\" >&3\nexec /usr/bin/head \"$@\"\n",
			} {
				if err := os.WriteFile(filepath.Join(f.dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			readyReader, readyWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer readyReader.Close()
			defer readyWriter.Close()
			releaseReader, releaseWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer releaseReader.Close()
			defer releaseWriter.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			joined := make(chan struct{})
			var nativeFailure error
			f.settings.Source.(*redisNonexpiringAuthoritySource).host = func(parentCtx context.Context, _ HostSettings, command string) (string, error) {
				cmd := redisAuthorityNativeCommand(parentCtx, command)
				cmd.Env = []string{"PATH=" + f.dir + ":/usr/bin:/bin", "LC_ALL=C", "TMPDIR=" + f.dir, "NONEXPIRING_TEST_DIR=" + f.dir}
				cmd.ExtraFiles = []*os.File{readyWriter, releaseReader}
				var out bytes.Buffer
				cmd.Stdout = &out
				if err := cmd.Start(); err != nil {
					close(joined)
					return "", err
				}
				readyWriter.Close()
				releaseReader.Close()
				err := cmd.Wait()
				redisAuthorityKillNativeGroup(cmd)
				nativeFailure = err
				close(joined)
				// This local producer returns both the caller lifecycle and the
				// joined child's native cause. exec.Wait alone does not wrap ctx.
				if parentErr := parentCtx.Err(); parentErr != nil {
					return out.String(), errors.Join(parentErr, err)
				}
				return out.String(), err
			}
			type outcome struct {
				alerts Alerts
				err    error
			}
			done := make(chan outcome, 1)
			finished := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				releaseWriter.Close()
				readyReader.Close()
				select {
				case <-finished:
				case <-time.After(15 * time.Second):
					t.Error("native nonexpiring fixture owner did not finish during cleanup")
				}
			})
			go func() {
				defer close(finished)
				alerts, err := NewRedisNonexpiringSignal().Run(ctx, f.settings)
				done <- outcome{alerts: alerts, err: err}
			}()
			nativePids := make(chan map[string]int, 1)
			go func() {
				pids := map[string]int{}
				for range 2 {
					var role string
					var pid int
					if _, err := fmt.Fscan(readyReader, &role, &pid); err != nil {
						break
					}
					pids[role] = pid
				}
				nativePids <- pids
			}()
			var pids map[string]int
			select {
			case pids = <-nativePids:
				if pids["reader"] <= 0 || pids["writer"] <= 0 || pids["reader"] == pids["writer"] {
					t.Fatal("native nonexpiring control did not reach both pending source barriers")
				}
			case <-done:
				t.Fatal("native nonexpiring control returned before reaching source barriers")
			case <-ctx.Done():
				t.Fatal("native nonexpiring control did not reach its source barriers")
			}
			select {
			case <-done:
				t.Fatal("native nonexpiring authority assertion: a pending positive prefix acquired a verdict")
			default:
			}
			if cancelPending {
				cancel()
			} else if _, err := fmt.Fprintln(releaseWriter, "complete"); err != nil {
				t.Fatal(err)
			}
			var result outcome
			select {
			case result = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("native nonexpiring collector did not join after source release or cancellation")
			}
			select {
			case <-joined:
			default:
				t.Fatal("native nonexpiring direct child was not joined before probe completion")
			}
			redisNonexpiringAuthorityPrivate(t, result.alerts, result.err)
			if result.err == nil || len(result.alerts) != 0 || cancelPending && !errors.Is(result.err, context.Canceled) {
				t.Fatal("native nonexpiring source release or cancellation acquired data-state authority")
			}
			if cancelPending {
				var native *exec.ExitError
				if nativeFailure == nil || !errors.Is(result.err, nativeFailure) || !errors.As(result.err, &native) {
					t.Fatal("native nonexpiring cancellation discarded its joined child cause")
				}
			}
			if !cancelPending {
				for _, pid := range pids {
					if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
						t.Fatal("native nonexpiring reader or writer survived ordinary collector completion")
					}
				}
			}
		}
	}
}

// Exit-zero errors, malformed rows, slot overlaps and truncated/oversized
// frames cannot become a positive slot count through partial parsing.
func TestRedisNonexpiringAuthorityNativeFrames(t *testing.T) {
	valid := redisNonexpiringAuthorityNode(6382, "4096-5119 5120-6143")
	for _, body := range []string{
		"", "ERR synthetic-private-sentinel\n", valid + "ERR synthetic-private-sentinel\n",
		valid + valid,
		valid + redisNonexpiringAuthorityNode(6381, "6144-8191"),
		redisNonexpiringAuthorityNode(6381, "6144-8191") + valid,
		strings.Replace(valid, "myself,master", "master", 1),
		strings.Replace(valid, "myself,master", "myself,myself,master", 1),
		strings.Replace(valid, "myself,master", "myself,slave", 1),
		strings.Replace(valid, "myself,master", "myself,master,fail?", 1),
		strings.Replace(valid, "myself,master", "myself,master,synthetic-private-sentinel", 1),
		strings.Replace(valid, ":6382@", ":6381@", 1),
		strings.Replace(valid, "connected", "disconnected", 1),
		strings.Replace(valid, " 0 0 1 ", " 0 0 18446744073709551616 ", 1),
		redisNonexpiringAuthorityNode(6382, "4096-5119 4096"),
		redisNonexpiringAuthorityNode(6382, "4096-5119 5000-6143"),
		redisNonexpiringAuthorityNode(6382, "4096-5119 6143-5120"),
		redisNonexpiringAuthorityNode(6382, "4096-5119 16384"),
		redisNonexpiringAuthorityNode(6382, "4096-5119 -1"),
		redisNonexpiringAuthorityNode(6382, "4096-5119 05120"),
		redisNonexpiringAuthorityNode(6382, "0-16383 0"),
		redisNonexpiringAuthorityNode(6382, "4096-5119 [5120->-synthetic-private-sentinel]"),
		redisNonexpiringAuthorityNode(6382, "4096-5119 [16384->-"+strings.Repeat("a", 40)+"]"),
		strings.TrimSuffix(valid, "\r\n"),
		"\r\r\n" + valid,
		strings.Replace(valid, "4096-5119", "4096-51\r19", 1),
		strings.Replace(valid, "4096-5119", "4096-5119\x00", 1),
		valid + strings.Repeat(" ", 256*1024) + "\n",
	} {
		f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}}, []int{100, 100, 100})
		f.write(t, 6382, "CLUSTER", body, 0)
		redisNonexpiringAuthorityUnknown(t, f, "redis/nonexpiring-skew")
		if len(body) > 256*1024 {
			exits, err := os.ReadFile(filepath.Join(f.dir, "exits"))
			if err != nil || !strings.Contains(string(exits), "CLUSTER 6382 0\n") {
				t.Fatal("oversized native frame did not preserve its independent zero-status producer control")
			}
		}
	}
}

// Preserve complete healthy and genuine outlier controls on all eight masters.
// CRLF, reversed range order, exact flags and migration metadata remain valid.
func TestRedisNonexpiringAuthorityCompleteNativeControls(t *testing.T) {
	ports := []int{6380, 6381, 6382, 6383, 6384, 6385, 6386, 6387}
	for _, outlier := range []bool{false, true} {
		densities := []int{100, 100, 100, 100, 100, 100, 100, 100}
		if outlier {
			densities[7] = 200
		}
		f := newRedisNonexpiringAuthorityFixture(t, [][]int{ports}, densities)
		body := redisNonexpiringAuthorityNode(6380, "1024-2047 0-1023 [100->-"+strings.Repeat("a", 40)+"] [2048-<-"+strings.Repeat("b", 40)+"]")
		body = strings.Replace(body, "myself,master", "master,myself", 1)
		body = strings.Replace(body, "192.0.2.1:6380", "[2001:db8::1]:6380", 1)
		body += fmt.Sprintf("%s 192.0.2.2:6381@16381,myself master - 0 0 1 connected 2048-4095\r\n", strings.Repeat("b", 40))
		f.write(t, 6380, "CLUSTER", "\n"+body+"\n", 0)
		alerts, err := NewWithSignals(f.settings, NewRedisNonexpiringSignal()).Run(context.Background())
		redisNonexpiringAuthorityPrivate(t, alerts, err)
		if err != nil {
			t.Fatal("complete native control was unavailable")
		}
		if outlier {
			if len(alerts) != 1 || alerts[0].Class != "nonexpiring-key-skew" || alerts[0].Target != "redis-nonexpiring-0.example:6387" ||
				!strings.Contains(alerts[0].Observed, "owned_slots=2048") || !strings.Contains(alerts[0].Observed, "fleet_median_per_slot=100.000") ||
				!strings.Contains(alerts[0].Observed, "estimated_excess_keys=204800") || !strings.Contains(alerts[0].Observed, "detail_status=provide") || len(f.samples) != 2 {
				t.Fatal("complete native positive lost exact skew or bounded family attribution")
			}
		} else {
			env, envErr := newProbeEnv(f.settings.withDefaults())
			if envErr != nil {
				t.Fatal(envErr)
			}
			findings, checkErr := (redisNonexpiringProbe{}).check(context.Background(), env)
			if len(alerts) != 0 || len(f.samples) != 0 || checkErr != nil || len(findings) != 1 || !findings[0].healthy {
				t.Fatal("complete native healthy control lost explicit health")
			}
		}
	}
}

// The exact native byte cap and both slot-domain endpoints remain admissible.
// Explicit zero keys are healthy even when owned-slot counts differ.
func TestRedisNonexpiringAuthorityNativeBoundaryControls(t *testing.T) {
	f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}}, []int{0, 0, 0})
	for index, slots := range []string{"0", "16383", "0-16383"} {
		port := 6380 + index
		body := redisNonexpiringAuthorityNode(port, slots)
		if index == 2 {
			body += strings.Repeat("\n", 256*1024-len(body))
		}
		f.write(t, port, "CLUSTER", body, 0)
	}
	env, err := newProbeEnv(f.settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (redisNonexpiringProbe{}).check(context.Background(), env)
	if err != nil || len(findings) != 1 || !findings[0].healthy || len(f.samples) != 0 {
		t.Fatal("native bound control rejected complete zero-key evidence at the exact source and slot bounds")
	}
}

// Losing high-density masters can create a false subset outlier; losing low
// masters can hide a real one. Neither subset establishes the fleet median.
func TestRedisNonexpiringAuthorityCompleteCohort(t *testing.T) {
	for _, test := range []struct {
		groups    [][]int
		densities []int
		breach    bool
		observed  string
	}{
		{groups: [][]int{{6380, 6381, 6382}, {6383, 6384, 6385, 6386, 6387}}, densities: []int{100, 100, 200, 200, 200, 200, 200, 200}, observed: "observed_masters=3 configured_masters=8 fleet_median=unknown"},
		{groups: [][]int{{6385, 6386, 6387}, {6380, 6381, 6382, 6383, 6384}}, densities: []int{100, 100, 100, 100, 100, 200, 200, 200}, breach: true, observed: "observed_masters=3 configured_masters=8 fleet_median=unknown"},
		{groups: [][]int{{6380, 6381}, {6382, 6383, 6384, 6385, 6386, 6387}}, densities: []int{100, 100, 100, 100, 100, 100, 100, 100}, observed: "observed_masters=2 configured_masters=8 fleet_median=unknown"},
	} {
		for _, reverse := range []bool{false, true} {
			f := newRedisNonexpiringAuthorityFixture(t, test.groups, test.densities)
			if reverse {
				f.settings.Hosts[1], f.settings.Hosts[2] = f.settings.Hosts[2], f.settings.Hosts[1]
				for index := 1; index < len(f.settings.Hosts); index++ {
					ports := f.settings.Hosts[index].RedisNodePorts
					for left, right := 0, len(ports)-1; left < right; left, right = left+1, right-1 {
						ports[left], ports[right] = ports[right], ports[left]
					}
				}
			}
			alerts, err := NewWithSignals(f.settings, NewRedisNonexpiringSignal()).Run(context.Background())
			redisNonexpiringAuthorityPrivate(t, alerts, err)
			if err != nil {
				t.Fatal("complete cohort control was unavailable")
			}
			if test.breach {
				if len(alerts) != 1 || alerts[0].Class != "nonexpiring-key-skew" ||
					!strings.Contains(alerts[0].Observed, "material_outlier_nodes=3") || !strings.Contains(alerts[0].Observed, "estimated_excess_keys=204800") || len(f.samples) != 2 {
					t.Fatal("complete cohort control lost its genuine three-master skew")
				}
			} else if len(alerts) != 0 || len(f.samples) != 0 {
				t.Fatal("complete cohort healthy control acquired a skew")
			}
			f.samples = nil
			f.downHost = "redis-nonexpiring-1.example"
			redisNonexpiringAuthorityUnknown(t, f, f.downHost+"/keyspace-census")
			alerts, err = NewWithSignals(f.settings, NewRedisNonexpiringSignal()).Run(context.Background())
			if err != nil || len(alerts) != 1 || !strings.Contains(alerts[0].Observed, test.observed) {
				t.Fatal("partial cohort lost its exact observed/configured scope")
			}
		}
	}
}

// Explicit scope exclusions keep the full denominator without contacting the
// denied host or converting typed policy denial into a workload failure.
func TestRedisNonexpiringAuthorityExcludedCohortIsUnknown(t *testing.T) {
	for _, all := range []bool{false, true} {
		f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}, {6383, 6384, 6385, 6386, 6387}}, []int{100, 100, 200, 200, 200, 200, 200, 200})
		names := []string{"redis-nonexpiring-1.example"}
		if all {
			names = append(names, "redis-nonexpiring-0.example")
		}
		settings, err := ExcludeHosts(f.settings, names...)
		if err != nil {
			t.Fatal(err)
		}
		alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), settings)
		redisNonexpiringAuthorityPrivate(t, alerts, err)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" || !alerts[0].unobservable || len(f.samples) != 0 {
			t.Fatal("excluded cohort acquired fleet skew, healthy or workload-failure authority")
		}
		calls, err := os.ReadFile(filepath.Join(f.dir, "calls"))
		if all {
			if !os.IsNotExist(err) {
				t.Fatal("fully excluded cohort contacted a native producer")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			for port := 6383; port <= 6387; port++ {
				if strings.Contains(string(calls), fmt.Sprint(port)) {
					t.Fatal("excluded host received a native keyspace or topology request")
				}
			}
		}
	}
}

// Partial relative evidence cannot suppress an independent absolute memory PAGE.
func TestRedisNonexpiringAuthorityPartialKeepsIndependentPressure(t *testing.T) {
	f := newRedisNonexpiringAuthorityFixture(t, [][]int{{6380, 6381, 6382}, {6383, 6384, 6385, 6386, 6387}}, []int{100, 100, 200, 200, 200, 200, 200, 200})
	f.downHost = "redis-nonexpiring-1.example"
	source := f.settings.Source.(*redisNonexpiringAuthoritySource)
	keyspace := source.host
	source.host = func(ctx context.Context, configured HostSettings, command string) (string, error) {
		if strings.Contains(command, "INFO keyspace") || strings.Contains(command, "EVAL_RO") {
			return keyspace(ctx, configured, command)
		}
		if configured.Name == f.downHost {
			return "", fmt.Errorf("synthetic host unavailable")
		}
		if strings.Contains(command, "/proc/meminfo") {
			return "host_memory 1000000 900000\n6380 93 100 80 1 10\n6381 10 100 9 1 10\n6382 10 100 9 1 10", nil
		}
		return "", nil
	}
	alerts, err := NewWithSignals(f.settings, NewRedisNonexpiringSignal(), NewRedisMemorySignal()).Run(context.Background())
	redisNonexpiringAuthorityPrivate(t, alerts, err)
	if err != nil {
		t.Fatal("independent pressure control failed")
	}
	page := requireAlertClass(t, alerts, "node-mem-critical")
	if page.Target != "redis-nonexpiring-0.example:6380" || page.Severity != SeverityPage || len(f.samples) != 0 {
		t.Fatal("partial relative census erased or misattributed independently proved pressure")
	}
	for _, alert := range alerts {
		if alert.SignalKey == "redis-nonexpiring" && alert.Class != "cannot-observe" {
			t.Fatal("partial fleet acquired skew authority beside independently proved pressure")
		}
	}
}

// The real cadence gate and notifier must keep an established incident during
// both native producer failure and a missing host, then clear on full recovery.
func TestRedisNonexpiringAuthorityNotifierRequiresCompleteRecovery(t *testing.T) {
	for _, missingHost := range []bool{false, true} {
		groups := [][]int{{6385, 6386, 6387}, {6380, 6381, 6382, 6383, 6384}}
		if !missingHost {
			groups = [][]int{{6380, 6381, 6382, 6383, 6384, 6385, 6386, 6387}}
		}
		f := newRedisNonexpiringAuthorityFixture(t, groups, []int{100, 100, 100, 100, 100, 200, 200, 200})
		webhook, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
		postSignalRunsThroughTheGate(t, NewRedisNonexpiringSignal(), f.settings, 6, func(run int) {
			f.downHost = ""
			status := 0
			if run == 1 || run == 2 {
				if missingHost {
					f.downHost = "redis-nonexpiring-1.example"
				} else {
					status = 124
				}
			}
			f.write(t, 6380, "CLUSTER", redisNonexpiringAuthorityNode(6380, "0-1023 1024-2047"), status)
			for port := 6385; port <= 6387; port++ {
				density := 200
				if run >= 4 || !missingHost && (run == 1 || run == 2) {
					density = 100
				}
				f.density(t, port, density)
			}
		}, notifier)
		closeSlackNotifier(t, notifier)
		texts := webhook.posted()
		identity := "redis/nonexpiring-skew · nonexpiring-key-skew · redis-nonexpiring-0.example:6385"
		if countSlackPosts(texts, "[WARN] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 {
			t.Fatal("nonexpiring notifier authority assertion: source loss cleared or re-raised a live incident, or full recovery did not clear it")
		}
		if strings.Contains(strings.Join(texts, "\n"), "synthetic-private-sentinel") {
			t.Fatal("nonexpiring notifier privacy assertion: source bytes entered notification text")
		}
	}
}
