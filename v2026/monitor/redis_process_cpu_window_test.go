// CPU controls execute the emitted collector against invented proc files and
// deterministic native clocks. Legacy ps values remain independent inputs for
// the pre-fix causal controls; no fixture reads real proc, Redis, SSH or dmesg.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const redisProcessCpuTestBoot = "00000000-0000-4000-8000-000000000001"
const redisProcessCpuTestGeneration = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Raw stat rows, clock/sysconf overrides and source status are fixture inputs;
// the production collector owns parsing, identity checks and the CPU formula.
type redisProcessCpuFixture struct {
	before        map[string]string
	after         map[string]string
	beforeBoot    string
	afterBoot     string
	legacyRows    string
	kernel        string
	kernelStatus  int
	processStatus int
	processPrefix string
	options       map[string]any
}

// These indexes are Linux stat fields, not a replica of CPU reduction logic.
func redisProcessCpuStat(pid uint64, comm string, start, user, system uint64) string {
	fields := make([]string, 50)
	for index := range fields {
		fields[index] = "0"
	}
	fields[0] = "S"
	fields[11] = strconv.FormatUint(user, 10)
	fields[12] = strconv.FormatUint(system, 10)
	fields[19] = strconv.FormatUint(start, 10)
	fields[20] = "9216000000"
	fields[21] = "1000000"
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " "))
}

// The lifetime average is deliberately independent of the actual interval.
func redisProcessCpuSingleFixture(legacyCpu string, before, after uint64) redisProcessCpuFixture {
	return redisProcessCpuFixture{
		before:     map[string]string{"4242": redisProcessCpuStat(4242, "redis-server", 100, before, 0)},
		after:      map[string]string{"4242": redisProcessCpuStat(4242, "redis-server", 100, after, 0)},
		legacyRows: "4242 " + legacyCpu + " 9000000 4000000 redis-server\n",
	}
}

// Native dependencies are restricted to the fixture. The same public signal
// invocation works with both the original ps collector and the interval fix.
func redisProcessCpuNativeFixture(t *testing.T, fixture redisProcessCpuFixture) (SignalSettings, *string, *string) {
	t.Helper()
	directory := t.TempDir()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Redis CPU collector fixture requires python3")
	}
	for _, name := range []string{"awk", "grep", "tail", "sed", "timeout"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("Redis CPU collector fixture requires %s", name)
		}
		if err := os.Symlink(path, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, data string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.beforeBoot == "" {
		fixture.beforeBoot = redisProcessCpuTestBoot
	}
	if fixture.afterBoot == "" {
		fixture.afterBoot = fixture.beforeBoot
	}
	if fixture.after == nil {
		fixture.after = fixture.before
	}
	for _, sample := range []struct {
		name string
		boot string
		rows map[string]string
	}{
		{name: "before", boot: fixture.beforeBoot, rows: fixture.before},
		{name: "after", boot: fixture.afterBoot, rows: fixture.after},
	} {
		write(filepath.Join(directory, sample.name, "boot_id"), sample.boot+"\n", 0600)
		for pid, row := range sample.rows {
			path := filepath.Join(directory, sample.name, pid, "stat")
			if row == "missing" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				write(path, row, 0600)
			}
		}
	}
	options := map[string]any{
		"ticks": 100, "page_bytes": 4096,
		"before_ns": int64(10001000000000), "after_ns": int64(10002000000000),
		"clock_step_ns": 0, "entry_count": 0, "entry_kind": "pid", "duplicate_pid": false,
		"scandir_error": "",
	}
	for key, value := range fixture.options {
		options[key] = value
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(directory, "options.json"), string(encoded), 0600)
	write(filepath.Join(directory, "legacy-ps"), fixture.legacyRows, 0600)
	write(filepath.Join(directory, "kernel"), fixture.kernel, 0600)
	write(filepath.Join(directory, "shim.py"), redisProcessCpuNativeShim, 0600)
	write(filepath.Join(directory, "python3"), "#!/bin/sh\nprintf 'python3\\n' >> \"$REDIS_CPU_FIXTURE/calls\"\nprintf '%s' \"$REDIS_CPU_PREFIX\"\n[ \"$REDIS_CPU_STATUS\" -eq 0 ] || exit \"$REDIS_CPU_STATUS\"\nexec "+shellSingleQuote(python)+" -Es "+shellSingleQuote(filepath.Join(directory, "shim.py"))+"\n", 0700)
	write(filepath.Join(directory, "ps"), "#!/bin/sh\nprintf 'ps\\n' >> \"$REDIS_CPU_FIXTURE/calls\"\n/bin/cat \"$REDIS_CPU_FIXTURE/legacy-ps\"\nexit \"$REDIS_CPU_STATUS\"\n", 0700)
	write(filepath.Join(directory, "dmesg"), "#!/bin/sh\nprintf 'dmesg\\n' >> \"$REDIS_CPU_FIXTURE/calls\"\n/bin/cat \"$REDIS_CPU_FIXTURE/kernel\"\nprintf '%s\\n' synthetic-private-diagnostic >&2\nexit \"$REDIS_KERNEL_STATUS\"\n", 0700)
	output, calls := "", ""
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		write(filepath.Join(directory, "calls"), "", 0600)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := redisAuthorityNativeCommand(ctx, command)
		cmd.Env = []string{
			"PATH=" + directory, "LC_ALL=C", "REDIS_CPU_FIXTURE=" + directory, "REDIS_CPU_PREFIX=" + fixture.processPrefix,
			fmt.Sprintf("REDIS_CPU_STATUS=%d", fixture.processStatus),
			fmt.Sprintf("REDIS_KERNEL_STATUS=%d", fixture.kernelStatus),
		}
		bytes, err := cmd.Output()
		redisAuthorityKillNativeGroup(cmd)
		output = string(bytes)
		callBytes, readErr := os.ReadFile(filepath.Join(directory, "calls"))
		if readErr != nil {
			t.Fatal("collector fixture did not record its source invocations")
		}
		calls = string(callBytes)
		if calls != "python3\ndmesg\n" && calls != "ps\ndmesg\n" {
			t.Fatal("signal did not execute one process collector and one kernel source")
		}
		return output, err
	}}
	return syntheticSettings(source), &output, &calls
}

// Only operating-system boundaries are replaced. The unchanged program from
// the actual shell command still performs every source and measurement check.
const redisProcessCpuNativeShim = `import builtins
import contextlib
import hashlib
import json
import os
import re
import sys
import time
import types

root = os.environ["REDIS_CPU_FIXTURE"]
with open(root + "/options.json") as source:
    options = json.load(source)
program = sys.stdin.read()
real_open = builtins.open
real_scandir = os.scandir
stage = "before"
clock_calls = {"before": 0, "after": 0}

def fixture_open(path, *args, **kwargs):
    path = os.fspath(path)
    if path == "/proc/sys/kernel/random/boot_id":
        path = root + "/" + stage + "/boot_id"
    elif path.startswith("/proc/"):
        if not re.fullmatch(r"/proc/[1-9][0-9]*/stat", path):
            raise OSError()
        path = root + "/" + stage + path[len("/proc"):]
    return real_open(path, *args, **kwargs)

@contextlib.contextmanager
def fixture_scandir(path):
    if path != "/proc" or options["scandir_error"] == stage:
        raise OSError()
    if options["entry_count"]:
        entries = [types.SimpleNamespace(name=("synthetic_" if options["entry_kind"] == "other" else "") + str(index + 1)) for index in range(options["entry_count"])]
    else:
        with real_scandir(root + "/" + stage) as source:
            entries = list(source)
    if options["duplicate_pid"]:
        entries.append(types.SimpleNamespace(name="4242"))
    yield iter(entries)

def fixture_clock():
    clock_calls[stage] += 1
    return options[stage + "_ns"] + clock_calls[stage] * options["clock_step_ns"]

def fixture_sleep(seconds):
    global stage
    if seconds != 1 or stage != "before":
        raise ValueError()
    stage = "after"

def fixture_sysconf(name):
    return {"SC_CLK_TCK": options["ticks"], "SC_PAGE_SIZE": options["page_bytes"]}[name]

builtins.open = fixture_open
os.scandir = fixture_scandir
os.sysconf = fixture_sysconf
time.monotonic_ns = fixture_clock
time.sleep = fixture_sleep
exec(compile(program, "actual-redis-process-collector", "exec"))
`

// A historical 250% lifetime average cannot establish current CPU pressure.
func TestRedisProcessCpuWindowHistoricalBusyCurrentIdle(t *testing.T) {
	fixture := redisProcessCpuSingleFixture("250.0", 2500000, 2500000)
	settings, _, calls := redisProcessCpuNativeFixture(t, fixture)
	alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		if len(alerts) == 1 && alerts[0].Class == "redis-cpu-sustained" {
			t.Fatal("historical lifetime 250% with zero interval CPU was misclassified as current pressure")
		}
		t.Fatal("historical-idle fixture failed its observation boundary")
	}
	if *calls != "python3\ndmesg\n" {
		t.Fatal("current CPU observation did not execute the actual interval collector")
	}
}

// A long idle lifetime must not dilute a newly busy interval. The watcher gate
// requires two actual independent probes, with no wall-clock sleeps in the test.
func TestRedisProcessCpuWindowNewBusyReachesSecondSustain(t *testing.T) {
	fixture := redisProcessCpuSingleFixture("0.0", 0, 250)
	settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
	signal := NewRedisProcessSignal()
	gate := newCadenceAlertGate()
	for sample := 1; sample <= 2; sample++ {
		observedAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(sample-1) * 5 * time.Minute)
		settings.Now = func() time.Time { return observedAt }
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) == 0 {
			t.Fatal("historical lifetime 0% hid a current 250% CPU interval")
		}
		if len(alerts) != 1 || alerts[0].Class != "redis-cpu-sustained" {
			t.Fatal("new-busy fixture failed its observation boundary")
		}
		alert := alerts[0]
		if alert.Sustain != 2 || alert.Severity != SeverityWarn || alert.Target != "redis-1" || alert.SignalNumber != "3.4" || signal.Cadence() != 5*time.Minute {
			t.Fatal("interval CPU changed the owning signal or sustain contract")
		}
		if !strings.Contains(alert.Observed, "cpu_pct=250 ") || !strings.Contains(alert.Observed, "cpu_window_ns=1000000000") || !strings.Contains(alert.Context, "do not establish continuous CPU pressure") {
			t.Fatal("current CPU evidence lost its measured interval or sampling limit")
		}
		ready := gate.filter(signal, alerts)
		if len(ready) != sample-1 {
			t.Fatal("current CPU did not become eligible exactly on its second actual observation")
		}
		requireAlertOmits(t, alert, redisProcessCpuTestBoot, "synthetic-private-diagnostic")
	}
}

// User and system jiffies contribute to current CPU; exact 200% stays healthy.
func TestRedisProcessCpuWindowNativeThresholdAndStatLayout(t *testing.T) {
	for _, test := range []struct {
		name   string
		comm   string
		user   uint64
		system uint64
		want   bool
	}{
		{name: "exact edge", comm: "redis-server", user: 100, system: 100},
		{name: "above edge", comm: "redis-server", user: 100, system: 101, want: true},
		{name: "comm closing parenthesis", comm: "redis-server)", user: 100, system: 101, want: true},
		{name: "unrelated spaced command", comm: "synthetic (worker)", user: 500},
	} {
		fixture := redisProcessCpuFixture{
			before: map[string]string{"4242": redisProcessCpuStat(4242, test.comm, 100, 1000, 1000)},
			after:  map[string]string{"4242": redisProcessCpuStat(4242, test.comm, 100, 1000+test.user, 1000+test.system)},
		}
		settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
		alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.want {
			requireAlertClass(t, alerts, "redis-cpu-sustained")
			if len(alerts) != 1 {
				t.Errorf("%s: complete source produced an extra finding", test.name)
			}
		} else if len(alerts) != 0 {
			t.Errorf("%s: healthy source produced a finding", test.name)
		}
	}
}

// Every ambiguous generation, interval, counter and census invalidates CPU
// authority, including a bad sibling after an otherwise positive process.
func TestRedisProcessCpuWindowNativeUnknowns(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*redisProcessCpuFixture)
	}{
		{name: "pid reused", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = redisProcessCpuStat(4242, "redis-server", 200, 1250, 0)
		}},
		{name: "zero Redis start", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = redisProcessCpuStat(4242, "redis-server", 0, 1250, 0)
		}},
		{name: "system counter reset", change: func(f *redisProcessCpuFixture) {
			f.before["4242"] = redisProcessCpuStat(4242, "redis-server", 100, 1000, 500)
			f.after["4242"] = redisProcessCpuStat(4242, "redis-server", 100, 1500, 499)
		}},
		{name: "counter reset", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = redisProcessCpuStat(4242, "redis-server", 100, 999, 0)
		}},
		{name: "one counter reset masked by another", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = redisProcessCpuStat(4242, "redis-server", 100, 999, 2000)
		}},
		{name: "pid disappeared", change: func(f *redisProcessCpuFixture) { f.after = map[string]string{} }},
		{name: "pid appeared", change: func(f *redisProcessCpuFixture) { f.before = map[string]string{} }},
		{name: "missing stat", change: func(f *redisProcessCpuFixture) { f.after["4242"] = "missing" }},
		{name: "malformed stat", change: func(f *redisProcessCpuFixture) { f.after["4242"] = "synthetic-private-diagnostic\n" }},
		{name: "partial stat", change: func(f *redisProcessCpuFixture) { f.after["4242"] = "4242 (redis-server) S 0\n" }},
		{name: "unterminated stat", change: func(f *redisProcessCpuFixture) { f.after["4242"] = strings.TrimSuffix(f.after["4242"], "\n") }},
		{name: "oversized stat", change: func(f *redisProcessCpuFixture) { f.after["4242"] = strings.Repeat("x", 4096) + "\n" }},
		{name: "stat pid mismatch", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = redisProcessCpuStat(4243, "redis-server", 100, 1250, 0)
		}},
		{name: "nonfinite counter", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = strings.Replace(f.after["4242"], " 1250 ", " NaN ", 1)
		}},
		{name: "negative counter", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = strings.Replace(f.after["4242"], " 1250 ", " -1 ", 1)
		}},
		{name: "counter sum overflow", change: func(f *redisProcessCpuFixture) {
			f.after["4242"] = redisProcessCpuStat(4242, "redis-server", 100, ^uint64(0), 1)
		}},
		{name: "boot changed", change: func(f *redisProcessCpuFixture) { f.afterBoot = "00000000-0000-4000-8000-000000000002" }},
		{name: "malformed boot", change: func(f *redisProcessCpuFixture) { f.afterBoot = "synthetic-private-diagnostic" }},
		{name: "zero clock", change: func(f *redisProcessCpuFixture) { f.options["before_ns"] = 0 }},
		{name: "clock reversed", change: func(f *redisProcessCpuFixture) { f.options["after_ns"] = int64(10000000000000) }},
		{name: "short interval", change: func(f *redisProcessCpuFixture) { f.options["after_ns"] = int64(10001999999999) }},
		{name: "long interval gap", change: func(f *redisProcessCpuFixture) { f.options["after_ns"] = int64(10004000000001) }},
		{name: "census over budget", change: func(f *redisProcessCpuFixture) { f.options["clock_step_ns"] = 100000000 }},
		{name: "missing tick frequency", change: func(f *redisProcessCpuFixture) { f.options["ticks"] = 0 }},
		{name: "invalid page size", change: func(f *redisProcessCpuFixture) { f.options["page_bytes"] = -1 }},
		{name: "census unavailable", change: func(f *redisProcessCpuFixture) { f.options["scandir_error"] = "after" }},
		{name: "duplicate census pid", change: func(f *redisProcessCpuFixture) { f.options["duplicate_pid"] = true }},
		{name: "census cap", change: func(f *redisProcessCpuFixture) { f.options["entry_count"] = 8193 }},
		{name: "directory cap", change: func(f *redisProcessCpuFixture) { f.options["entry_count"] = 16385; f.options["entry_kind"] = "other" }},
		{name: "empty census long gap", change: func(f *redisProcessCpuFixture) {
			f.before = map[string]string{}
			f.after = map[string]string{}
			f.options["after_ns"] = int64(10004000000001)
		}},
		{name: "source failure", change: func(f *redisProcessCpuFixture) { f.processStatus = 1 }},
		{name: "failed partial stdout", change: func(f *redisProcessCpuFixture) {
			f.processStatus = 1
			f.processPrefix = redisProcessCpuRow(4242, "250")
		}},
		{name: "source timeout", change: func(f *redisProcessCpuFixture) { f.processStatus = 124 }},
		{name: "source missing", change: func(f *redisProcessCpuFixture) { f.processStatus = 127 }},
		{name: "partial sibling", change: func(f *redisProcessCpuFixture) {
			f.before["4343"] = redisProcessCpuStat(4343, "redis-server", 100, 0, 0)
			f.after["4343"] = "synthetic-private-diagnostic\n"
		}},
	} {
		fixture := redisProcessCpuSingleFixture("250", 1000, 1250)
		fixture.options = map[string]any{}
		test.change(&fixture)
		settings, output, _ := redisProcessCpuNativeFixture(t, fixture)
		alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		alert := requireAlertClass(t, alerts, "cannot-observe")
		if len(alerts) != 1 || alert.Target != "redis-1/redis-process" || !alert.unobservable || alert.Sustain != 2 {
			t.Errorf("%s: incomplete CPU evidence escaped its authority boundary", test.name)
		}
		if strings.Contains(*output, "redis ") || strings.Contains(*output, "synthetic-private-diagnostic") {
			t.Errorf("%s: failed collector published partial rows or source diagnostics", test.name)
		}
		requireAlertOmits(t, alert, "synthetic-private-diagnostic", redisProcessCpuTestBoot)
	}
}

// A reused PID starts its own sustain sequence, and a visibility gap breaks
// the previous sequence even when the next sample has the same generation.
func TestRedisProcessCpuWindowGenerationAndUnknownBreakSustain(t *testing.T) {
	signal := NewRedisProcessSignal()
	gate := newCadenceAlertGate()
	previousFrame := ""
	for index, start := range []uint64{100, 200, 200, 200, 200, 200} {
		fixture := redisProcessCpuSingleFixture("250", 1000, 1250)
		fixture.before["4242"] = redisProcessCpuStat(4242, "redis-server", start, 1000, 0)
		fixture.after["4242"] = redisProcessCpuStat(4242, "redis-server", start, 1250, 0)
		if index == 3 {
			fixture.after["4242"] = "missing"
		}
		settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if index != 3 {
			alert := requireAlertClass(t, alerts, "redis-cpu-sustained")
			if index == 1 && alert.Frame == previousFrame {
				t.Fatal("reused pid inherited the previous process generation")
			}
			previousFrame = alert.Frame
		}
		ready := gate.filter(signal, alerts)
		want := 0
		if index == 2 || index == 5 {
			want = 1
		}
		if len(ready) != want {
			t.Fatalf("sample %d: generation or visibility gap leaked a sustain count", index)
		}
	}
}

// A Redis kernel victim remains an independent first-observation page when
// the CPU window cannot be measured, and valid CPU survives missing kernel.
func TestRedisProcessCpuWindowKernelAuthorityIndependent(t *testing.T) {
	for _, kernelUnavailable := range []bool{false, true} {
		fixture := redisProcessCpuSingleFixture("0", 1000, 1250)
		if kernelUnavailable {
			fixture.kernelStatus = 1
		} else {
			fixture.after["4242"] = "missing"
			fixture.kernel = "Out of memory: Killed process 4242 (redis-server)\n"
		}
		settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
		alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 2 {
			t.Fatal("one unknown source suppressed its independent positive sibling")
		}
		unknown := requireAlertClass(t, alerts, "cannot-observe")
		if kernelUnavailable {
			requireAlertClass(t, alerts, "redis-cpu-sustained")
			if unknown.Target != "redis-1/redis-kernel" {
				t.Fatal("kernel visibility identity changed")
			}
		} else {
			page := requireAlertClass(t, alerts, "redis-kernel-oom")
			if page.Sustain != 1 || page.Severity != SeverityPage || unknown.Target != "redis-1/redis-process" {
				t.Fatal("known kernel victim lost independent page authority")
			}
		}
	}
}

// Framed fixtures retain the parser boundary independently of native inputs.
func redisProcessCpuRow(pid uint64, cpu string) string {
	return fmt.Sprintf("redis %d %s 9000000 4000000 %s 1000000000\n", pid, cpu, redisProcessCpuTestGeneration)
}

// Generation changes across complete windows cannot borrow an old boot's
// first breach, even when the new boot happens to reuse PID and start ticks.
func TestRedisProcessCpuWindowBootChangeRestartsSustain(t *testing.T) {
	signal := NewRedisProcessSignal()
	gate := newCadenceAlertGate()
	previousFrame := ""
	for index, boot := range []string{redisProcessCpuTestBoot, "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000002"} {
		fixture := redisProcessCpuSingleFixture("250", 1000, 1250)
		fixture.beforeBoot = boot
		settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "redis-cpu-sustained")
		if index == 1 && alert.Frame == previousFrame {
			t.Fatal("new boot inherited the old process generation")
		}
		previousFrame = alert.Frame
		want := 0
		if index == 2 {
			want = 1
		}
		if len(gate.filter(signal, alerts)) != want {
			t.Fatal("boot change inherited a previous sustain count")
		}
	}
}

// A finite source cap refuses the whole census rather than silently sampling
// its first processes. Exactly the permitted census remains observable.
func TestRedisProcessCpuWindowRedisCensusBound(t *testing.T) {
	for _, count := range []int{512, 513} {
		fixture := redisProcessCpuFixture{before: map[string]string{}}
		for index := 1; index <= count; index++ {
			pid := uint64(index)
			fixture.before[strconv.Itoa(index)] = redisProcessCpuStat(pid, "redis-server", 100, 0, 0)
		}
		settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
		alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if count == 512 {
			if len(alerts) != 0 {
				t.Fatal("complete maximum-size census lost health authority")
			}
		} else {
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			if len(alerts) != 1 || unknown.Target != "redis-1/redis-process" {
				t.Fatal("oversized Redis census acquired partial authority")
			}
		}
	}
}

// Early kernel tasks may have zero start ticks; they are not Redis generations.
func TestRedisProcessCpuWindowUnrelatedZeroStartIsHealthy(t *testing.T) {
	fixture := redisProcessCpuFixture{before: map[string]string{"2": redisProcessCpuStat(2, "synthetic-worker", 0, 0, 0)}}
	settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
	alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("an unrelated early-boot task invalidated the Redis census")
	}
}

// Native tick frequency and page size are independent units; neither is an
// assumed machine constant nor a substitute for elapsed monotonic time.
func TestRedisProcessCpuWindowNativeUnits(t *testing.T) {
	fixture := redisProcessCpuSingleFixture("0", 0, 2500)
	fixture.options = map[string]any{"ticks": 1000, "page_bytes": 8192}
	settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
	alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "redis-cpu-sustained")
	if !strings.Contains(alert.Observed, "cpu_pct=250 ") || !strings.Contains(alert.Observed, "virt_kb=9000000") || !strings.Contains(alert.Observed, "res_kb=8000000") {
		t.Fatal("native CPU or contextual memory units were not applied")
	}
}

// A malformed transport-level CPU frame cannot invalidate an independently
// complete kernel victim, and it cannot acquire a CPU finding of its own.
func TestRedisProcessCpuWindowMalformedFrameKeepsKernelVictim(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		row := strings.Replace(redisProcessCpuRow(4242, "250"), redisProcessCpuTestGeneration, "invalid", 1)
		return row + "process_source complete 1\nkernel_oom Out of memory: Killed process 4242 (redis-server)\nkernel_source complete\n", nil
	}}
	alerts, err := NewRedisProcessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	page := requireAlertClass(t, alerts, "redis-kernel-oom")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if len(alerts) != 2 || page.Sustain != 1 || unknown.Target != "redis-1/redis-process" {
		t.Fatal("CPU frame failure escaped its independent source boundary")
	}
}
