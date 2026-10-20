// Observation-authority controls use invented producers and the actual emitted
// shell commands. No test reaches Redis, SSH or a real process/kernel census.
package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The native fixture replaces source executables while retaining the actual
// collector pipeline and independently executing both framed observations.
func TestRedisProcessAuthorityNativeCensus(t *testing.T) {
	cases := []struct {
		name        string
		fixture     redisProcessCpuFixture
		wantUnknown bool
		wantCpu     bool
		wantOom     bool
	}{
		{name: "complete empty"},
		{name: "complete unrelated process", fixture: redisProcessCpuFixture{before: map[string]string{"4242": redisProcessCpuStat(4242, "synthetic-worker", 100, 0, 0)}}},
		{name: "complete spaced command", fixture: redisProcessCpuFixture{before: map[string]string{"4242": redisProcessCpuStat(4242, "synthetic worker name", 100, 0, 0)}}},
		{name: "cpu boundary", fixture: redisProcessCpuSingleFixture("200", 0, 200)},
		{name: "cpu positive", fixture: redisProcessCpuSingleFixture("201", 0, 201), wantCpu: true},
		{name: "failed empty", fixture: redisProcessCpuFixture{processStatus: 1}, wantUnknown: true},
		{name: "failed partial", fixture: redisProcessCpuFixture{processStatus: 1, processPrefix: redisProcessCpuRow(4242, "250")}, wantUnknown: true},
		{name: "unavailable", fixture: redisProcessCpuFixture{processStatus: 127}, wantUnknown: true},
		{name: "timeout", fixture: redisProcessCpuFixture{processStatus: 124}, wantUnknown: true},
		{name: "malformed source", fixture: redisProcessCpuFixture{before: map[string]string{"4242": "synthetic-private-diagnostic\n"}}, wantUnknown: true},
		{name: "kernel survives failed census", fixture: redisProcessCpuFixture{processStatus: 1, kernel: "Out of memory: Killed process 4242 (redis-server)\n"}, wantUnknown: true, wantOom: true},
	}
	for _, test := range cases {
		settings, _, _ := redisProcessCpuNativeFixture(t, test.fixture)
		alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		wantCount := 0
		if test.wantUnknown {
			wantCount++
			alert := requireAlertClass(t, alerts, "cannot-observe")
			if alert.Target != "redis-1/redis-process" || alert.SignalKey != "redis-process" || alert.SignalNumber != "3.4" || alert.Sustain != 2 || !alert.unobservable {
				t.Errorf("%s: process visibility identity changed", test.name)
			}
		}
		if test.wantCpu {
			wantCount++
			requireAlertClass(t, alerts, "redis-cpu-sustained")
		}
		if test.wantOom {
			wantCount++
			requireAlertClass(t, alerts, "redis-kernel-oom")
		}
		if len(alerts) != wantCount {
			t.Errorf("%s: got %d alerts, want %d", test.name, len(alerts), wantCount)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "synthetic-private-diagnostic")
		}
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (redisProcessProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			if test.wantUnknown && finding.healthy {
				t.Errorf("%s: unknown census became healthy", test.name)
			}
		}
	}
}

// Successful transport must not convert bad source numbers or truncated census
// frames into zero CPU; kernel authority remains independent.
func TestRedisProcessAuthorityRejectsIncompleteCensus(t *testing.T) {
	row := redisProcessCpuRow(4242, "250")
	complete := "process_source complete 1\n"
	cases := []string{
		"", row, complete,
		redisProcessCpuRow(4242, "NaN") + complete,
		redisProcessCpuRow(4242, "+Inf") + complete,
		redisProcessCpuRow(4242, "-1") + complete,
		strings.Replace(row, " 9000000 ", " bad ", 1) + complete,
		redisProcessCpuRow(0, "250") + complete,
		strings.Replace(row, " 1000000000", "", 1) + complete,
		strings.TrimSuffix(row, "\n") + " extra\n" + complete,
		strings.Replace(row, redisProcessCpuTestGeneration, "invalid", 1) + complete,
		strings.Replace(row, redisProcessCpuTestGeneration, strings.Repeat("g", 64), 1) + complete,
		strings.Replace(row, " 1000000000", " 0", 1) + complete,
		strings.Replace(row, " 1000000000", " 999999999", 1) + complete,
		strings.Replace(row, " 1000000000", " 3000000001", 1) + complete,
		row + row + "process_source complete 2\n",
		"process_source complete 0\nprocess_source complete 0\n",
		"process_source complete 0\n" + row,
		row + "process_source unavailable\n",
	}

	for index, output := range cases {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return output + "kernel_source complete\n", nil }}
		alerts, err := NewRedisProcessSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "cannot-observe")
		if len(alerts) != 1 || alert.Target != "redis-1/redis-process" {
			t.Errorf("case %d: malformed process evidence escaped its authority boundary", index)
		}
	}
}

// Validate every row before consulting history; a late malformed row cannot
// leave a previously parsed count in the learned full-fleet series.
func TestRedisFamilyAuthorityRejectsBadFramesBeforeBaseline(t *testing.T) {
	cases := []string{
		"", "30000 ckey_<id>", "family_source complete 1\n",
		"30000 ckey_<id>\nfamily_source unavailable\n",
		"bad ckey_<id>\nfamily_source complete 0\n",
		"-1 ckey_<id>\nfamily_source complete 0\n",
		"0 ckey_<id>\nfamily_source complete 0\n",
		"30000 ckey_<id>\nfamily_source complete 30001\n",
		"30000 ckey_<id>\nfamily_source complete 29999\n",
		"30000 ckey_<id>\n1 synthetic-private-diagnostic\nfamily_source complete 30000\n",
		"30000 ckey_<id>\n30000 ckey_<id>\nfamily_source complete 60000\n",
		"300001 ckey_<id>\nfamily_source complete 300001\n",
		"family_source complete 0\nfamily_source complete 0\n",
		"family_source complete 0\nsynthetic-private-diagnostic\n",
	}
	for index, output := range cases {
		source := &syntheticSource{
			hostFn: func(HostSettings, string) (string, error) {
				return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
			},
			hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { return output, nil },
		}
		env, err := newProbeEnv(syntheticSettings(source).withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		env.baseline, err = newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		for _, age := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
			env.baseline.record("redis/family/ckey_<id>", now.Add(-age), 1000)
		}
		findings, err := (redisFamilyProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 || findings[0].class != "cannot-observe" || findings[0].healthy || len(env.baseline.metricSamples["redis/family/ckey_<id>"]) != 3 {
			t.Errorf("case %d: incomplete scan published growth, health or baseline", index)
		}
		alerts, err := NewKeyFamiliesSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		for _, alert := range alerts {
			encoded, err := json.Marshal(alert)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded)+alert.Markdown(), "synthetic-private-diagnostic") {
				t.Errorf("case %d: private scan diagnostic escaped", index)
			}
		}
	}
}

// A canceled source returns before baseline publication. This is a transport
// failure, distinct from a successful transport carrying source-unavailable.
func TestRedisFamilyAuthorityCancellationPreservesBaseline(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
		},
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { return "30000 ckey_<id>", context.Canceled },
	}
	env, err := newProbeEnv(syntheticSettings(source).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	env.baseline, err = newBaselineStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (redisFamilyProbe{}).check(context.Background(), env)
	if err == nil || len(findings) != 0 || len(env.baseline.metricSamples) != 0 {
		t.Fatal("canceled partial scan acquired observation authority")
	}
}

// A stale learned band cannot turn a complete current sample into growth;
// current complete evidence starts the new series using the existing policy.
func TestRedisFamilyAuthorityStaleHistoryDoesNotInventGrowth(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
		},
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			return "30000 ckey_<id>\nfamily_source complete 30000\n", nil
		},
	}
	env, err := newProbeEnv(syntheticSettings(source).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	env.baseline, err = newBaselineStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, age := range []time.Duration{8 * 24 * time.Hour, 9 * 24 * time.Hour, 10 * 24 * time.Hour} {
		env.baseline.record("redis/family/ckey_<id>", time.Now().Add(-age), 1000)
	}
	findings, err := (redisFamilyProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !findings[0].healthy {
		t.Fatal("expired baseline invented a current family-growth breach")
	}
	samples := env.baseline.metricSamples["redis/family/ckey_<id>"]
	if len(samples) != 4 || samples[3].v != 30000 {
		t.Fatal("complete sample did not advance the current baseline")
	}
}

// A canceled process census is an outer observation failure, even if its
// returned prefix resembles a positive CPU finding.
func TestRedisProcessAuthorityCancellationDoesNotPublishPrefix(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "redis 4242 250 100 50\n", context.Canceled }}
	alerts, err := NewRedisProcessSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil || len(alerts) != 0 {
		t.Fatal("canceled process census published its partial prefix")
	}
}

// Each owner has a separate process group. Context cancellation kills that
// group, Output/Wait joins the direct child, and pipe waiting is bounded. This
// fixture boundary does not attest remote SSH descendants or grandchild reaping.
func redisAuthorityNativeCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return redisAuthorityKillNativeGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// Kill any members remaining in the private group after normal completion as
// well as on cancellation; an absent group already satisfies that operation.
func redisAuthorityKillNativeGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// An inherited readiness descriptor proves that the actual collector reached
// its fake source before cancellation. No wall-clock sleep selects the race.
func TestRedisFamilyAuthorityNativeOwnerCancellation(t *testing.T) {
	command := ""
	source := &syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
		},
		hostTimeoutFn: func(_ HostSettings, actual string, _ time.Duration) (string, error) {
			command = actual
			return "", context.Canceled
		},
	}
	_, err := NewKeyFamiliesSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil || command == "" {
		t.Fatal("native collector command was not captured through the real signal")
	}
	directory := t.TempDir()
	for name, script := range map[string]string{
		"timeout":   "#!/bin/sh\nshift\nexec \"$@\"\n",
		"redis-cli": "#!/bin/sh\nprintf 'native-ready\\n' >&9\nexec /bin/sleep 3600\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := redisAuthorityNativeCommand(ctx, command)
	cmd.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C", "TMPDIR=" + directory}
	// fd 3 is the collector's count side channel; fd 9 belongs to this barrier.
	cmd.ExtraFiles = []*os.File{nil, nil, nil, nil, nil, nil, readyWrite}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		readyWrite.Close()
		t.Fatal(err)
	}
	readyWrite.Close()
	defer redisAuthorityKillNativeGroup(cmd)
	ready, readyErr := bufio.NewReader(readyRead).ReadString('\n')
	cancel()
	waitErr := cmd.Wait()
	if readyErr != nil || ready != "native-ready\n" {
		t.Fatalf("native readiness barrier failed: %v", readyErr)
	}
	if !errors.Is(ctx.Err(), context.Canceled) || waitErr == nil || cmd.ProcessState == nil {
		t.Fatal("canceled native owner was not joined")
	}
	if strings.Contains(output.String(), "family_source complete") {
		t.Fatal("canceled stalled source published completion")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("collector created a persistent sample file")
	}
}

// A Bash whose read rejects -N (before 4.1, such as macOS /bin/bash 3.2) fails
// with status 2 without consuming input. The BASH_ENV shim drains the stream
// before failing, forcing the ordering in which every producer exits cleanly,
// so no upstream SIGPIPE can decide the verdict in place of the validator.
// The shim scans "$@" because the caller's empty IFS would join "$*" unspaced.
func TestRedisFamilyAuthorityNativeUnsupportedChunkReadIsUnknown(t *testing.T) {
	directory := t.TempDir()
	unsupportedPath := filepath.Join(directory, "unsupported-read")
	for name, script := range map[string]string{
		"timeout":   "#!/bin/sh\nshift\nexec \"$@\"\n",
		"redis-cli": "#!/bin/sh\nprintf '%s\\n' ckey_00000000-0000-0000-0000-000000000001\n",
		"unsupported-read": `read() {
 for read_argument in "$@"; do
  if [[ "$read_argument" == -N ]]; then
   cat >/dev/null
   return 2
  fi
 done
 builtin read "$@"
}
`,
	} {
		mode := os.FileMode(0700)
		if name == "unsupported-read" {
			mode = 0600
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), mode); err != nil {
			t.Fatal(err)
		}
	}
	source := &syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
		},
		hostTimeoutFn: func(_ HostSettings, command string, _ time.Duration) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := redisAuthorityNativeCommand(ctx, command)
			cmd.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C", "TMPDIR=" + directory, "BASH_ENV=" + unsupportedPath}
			output, err := cmd.Output()
			redisAuthorityKillNativeGroup(cmd)
			return string(output), err
		},
	}
	env, err := newProbeEnv(syntheticSettings(source).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	env.baseline, err = newBaselineStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (redisFamilyProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].class != "cannot-observe" || findings[0].healthy || len(env.baseline.metricSamples) != 0 {
		t.Fatal("a read that rejected -N authorized an empty complete scan")
	}
}
