// Tests for `sidecarctl evs purge-turn-on` against a fake env. The request's shape and once key are evsservice's
// (sweep_task_test.go) and the fold is server/task's (sidecarhost's TestTasksTwoSchedulesOfOneKeyRunOnce), so these
// hold the command to its contract: it schedules exactly once a run when the gate is enabled with purge on, refuses
// and schedules nothing otherwise, asks a second run for the same request, never reports a failure with exit 0, and
// refuses an env its process does not run as before reading anything. cmd_evs_purge_turn_on_server_test.go runs it
// against server's loader and queue on the private test stack.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// The time every run is judged at.
var evsPurgeTurnOnTestNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// The fake queue's request for the turn-on: a job name and once key that no real request carries.
const (
	evsPurgeTurnOnTestJobName = "evs.sweep"
	evsPurgeTurnOnTestOnceKey = "evs.sweep.fake-turn-on"
)

// An env a test sets up: the format2 gate a read answers, and a queue that folds a request into one pending under
// the same job name and once key, at the sooner due time, as server/task's RunOnce does for sidecarhost.Tasks.
type fakeEvsPurgeTurnOnEnv struct {
	gate        *evsFormat2GateRead
	readErr     error
	scheduleErr error
	// what the queue holds, by job name and once key
	keyPendingTasks map[string]*evsScheduledTask
	readCount       int
	scheduleCount   int
}

// An env whose gate reads as given and whose queue is empty.
func newFakeEvsPurgeTurnOnEnv(gate *evsFormat2GateRead) *fakeEvsPurgeTurnOnEnv {
	return &fakeEvsPurgeTurnOnEnv{
		gate:            gate,
		keyPendingTasks: map[string]*evsScheduledTask{},
	}
}

// See evsPurgeTurnOnEnv.
func (self *fakeEvsPurgeTurnOnEnv) ReadFormat2Gate(ctx context.Context) (*evsFormat2GateRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.readCount += 1
	return self.gate, self.readErr
}

// See evsPurgeTurnOnEnv. Like sidecarhost.Tasks, it answers what this run asked for, not what the fold left.
func (self *fakeEvsPurgeTurnOnEnv) ScheduleTurnOn(ctx context.Context, dueTime time.Time) (*evsScheduledTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.scheduleCount += 1
	if self.scheduleErr != nil {
		return nil, self.scheduleErr
	}
	key := evsPurgeTurnOnTestJobName + "/" + evsPurgeTurnOnTestOnceKey
	if pending, ok := self.keyPendingTasks[key]; !ok {
		self.keyPendingTasks[key] = &evsScheduledTask{name: evsPurgeTurnOnTestJobName, onceKey: evsPurgeTurnOnTestOnceKey, dueTime: dueTime}
	} else if dueTime.Before(pending.dueTime) {
		pending.dueTime = dueTime
	}
	return &evsScheduledTask{name: evsPurgeTurnOnTestJobName, onceKey: evsPurgeTurnOnTestOnceKey, dueTime: dueTime}, nil
}

// Settings that open env on fake, judged at now, recording each env opened.
func fakeEvsPurgeTurnOnSettings(fake *fakeEvsPurgeTurnOnEnv, now *time.Time, openedEnvs *[]string) *evsPurgeTurnOnSettings {
	settings := defaultEvsPurgeTurnOnSettings()
	settings.now = func() time.Time {
		return *now
	}
	settings.openEnv = func(ctx context.Context, env string) (evsPurgeTurnOnEnv, error) {
		*openedEnvs = append(*openedEnvs, env)
		return fake, nil
	}
	return settings
}

// A command set holding the command under settings, so a test reads the exit code a runbook step reads.
func evsPurgeTurnOnTestCommands(settings *evsPurgeTurnOnSettings) *sidecarctlCommandSet {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "evs purge-turn-on",
		usage:   "<env>",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runEvsPurgeTurnOnWithSettings(args, settings, out)
		},
	})
	return commands
}

// With the gate enabled and purge on, a run reads the gate once, schedules the turn-on once, due the start delay
// after the run, prints what it scheduled and exits 0.
func TestEvsPurgeTurnOnSchedulesOneTurnOnRequest(t *testing.T) {
	fake := newFakeEvsPurgeTurnOnEnv(&evsFormat2GateRead{enabled: true, purge: "on"})
	now := evsPurgeTurnOnTestNow
	openedEnvs := []string{}
	settings := fakeEvsPurgeTurnOnSettings(fake, &now, &openedEnvs)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := evsPurgeTurnOnTestCommands(settings).run([]string{"evs", "purge-turn-on", "test"}, stdout, stderr); code != 0 {
		t.Fatalf("exited %d: %s", code, stderr.String())
	}
	if !slices.Equal(openedEnvs, []string{"test"}) || fake.readCount != 1 || fake.scheduleCount != 1 || len(fake.keyPendingTasks) != 1 {
		t.Fatalf("opened %v, read %d, scheduled %d, pending %d", openedEnvs, fake.readCount, fake.scheduleCount, len(fake.keyPendingTasks))
	}
	wantDueTime := evsPurgeTurnOnTestNow.Add(settings.startDelay)
	for _, pending := range fake.keyPendingTasks {
		if !pending.dueTime.Equal(wantDueTime) {
			t.Fatalf("due %v, want %v", pending.dueTime, wantDueTime)
		}
	}
	for _, want := range []string{evsPurgeTurnOnTestJobName, evsPurgeTurnOnTestOnceKey, "test's task queue", wantDueTime.Format(time.RFC3339)} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("printed %q, want %q in it", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("printed %q to stderr", stderr.String())
	}
}

// Under observe, with the gate off, or with a purge that is not exactly on, a run refuses with exit 1, saying why,
// and schedules nothing.
func TestEvsPurgeTurnOnRefusesUnderObserveOrWithTheGateOff(t *testing.T) {
	cases := []struct {
		gate       *evsFormat2GateRead
		wantStderr string
	}{
		{gate: &evsFormat2GateRead{enabled: true, purge: "observe"}, wantStderr: `format2.purge reads "observe"`},
		{gate: &evsFormat2GateRead{enabled: true, purge: "On"}, wantStderr: `format2.purge reads "On"`},
		{gate: &evsFormat2GateRead{enabled: true, purge: ""}, wantStderr: `format2.purge reads ""`},
		{gate: &evsFormat2GateRead{enabled: false, purge: "on"}, wantStderr: "format2 is not enabled"},
		{gate: &evsFormat2GateRead{enabled: false, purge: "observe"}, wantStderr: "format2 is not enabled"},
	}
	for _, c := range cases {
		fake := newFakeEvsPurgeTurnOnEnv(c.gate)
		now := evsPurgeTurnOnTestNow
		openedEnvs := []string{}
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := evsPurgeTurnOnTestCommands(fakeEvsPurgeTurnOnSettings(fake, &now, &openedEnvs)).run([]string{"evs", "purge-turn-on", "test"}, stdout, stderr)
		if code != 1 {
			t.Errorf("%+v: exited %d, want 1", *c.gate, code)
		}
		if fake.readCount != 1 || fake.scheduleCount != 0 || len(fake.keyPendingTasks) != 0 {
			t.Errorf("%+v: read %d, scheduled %d, pending %d", *c.gate, fake.readCount, fake.scheduleCount, len(fake.keyPendingTasks))
		}
		if !strings.Contains(stderr.String(), c.wantStderr) || !strings.Contains(stderr.String(), "nothing was scheduled") {
			t.Errorf("%+v: printed %q to stderr, want %q and that nothing was scheduled", *c.gate, stderr.String(), c.wantStderr)
		}
		if stdout.Len() != 0 {
			t.Errorf("%+v: printed %q", *c.gate, stdout.String())
		}
	}
}

// A second run is no mistake: it asks for the same request again, which the queue folds into the pending one at the
// sooner due time, so the pass is queued once.
func TestEvsPurgeTurnOnSecondRunFoldsIntoThePendingRequest(t *testing.T) {
	fake := newFakeEvsPurgeTurnOnEnv(&evsFormat2GateRead{enabled: true, purge: "on"})
	now := evsPurgeTurnOnTestNow
	openedEnvs := []string{}
	commands := evsPurgeTurnOnTestCommands(fakeEvsPurgeTurnOnSettings(fake, &now, &openedEnvs))
	outputs := []string{}
	for range 2 {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		if code := commands.run([]string{"evs", "purge-turn-on", "test"}, stdout, stderr); code != 0 {
			t.Fatalf("exited %d: %s", code, stderr.String())
		}
		outputs = append(outputs, stdout.String())
		now = now.Add(30 * time.Second)
	}
	if fake.scheduleCount != 2 || len(fake.keyPendingTasks) != 1 {
		t.Fatalf("scheduled %d, pending %d: want two schedules of one request", fake.scheduleCount, len(fake.keyPendingTasks))
	}
	for _, pending := range fake.keyPendingTasks {
		if wantDueTime := evsPurgeTurnOnTestNow.Add(defaultEvsPurgeTurnOnSettings().startDelay); !pending.dueTime.Equal(wantDueTime) {
			t.Fatalf("the fold left it due %v, want the first run's %v", pending.dueTime, wantDueTime)
		}
	}
	for _, output := range outputs {
		if !strings.Contains(output, "under once key "+evsPurgeTurnOnTestOnceKey+" ") {
			t.Fatalf("a run printed %q, want the one once key", output)
		}
	}
}

// An env that does not open, read or schedule fails the run with exit 1 and its cause, and nothing is scheduled, nor
// claimed to be. So does a run whose bound has passed before it starts, which opens nothing.
func TestEvsPurgeTurnOnSchedulesNothingWhenTheEnvFails(t *testing.T) {
	cases := []struct {
		label             string
		edit              func(settings *evsPurgeTurnOnSettings, fake *fakeEvsPurgeTurnOnEnv)
		wantStderr        string
		wantOpened        bool
		wantRead          bool
		wantScheduleTried bool
	}{
		{
			label: "an env that does not open",
			edit: func(settings *evsPurgeTurnOnSettings, fake *fakeEvsPurgeTurnOnEnv) {
				settings.openEnv = func(ctx context.Context, env string) (evsPurgeTurnOnEnv, error) {
					return nil, errors.New("the env's vault holds no pg config")
				}
			},
			wantStderr: "the env's vault holds no pg config",
		},
		{
			label: "an env that opens as nothing",
			edit: func(settings *evsPurgeTurnOnSettings, fake *fakeEvsPurgeTurnOnEnv) {
				settings.openEnv = func(ctx context.Context, env string) (evsPurgeTurnOnEnv, error) {
					return nil, nil
				}
			},
			wantStderr: "opened as no env",
		},
		{
			label: "a gate that does not read",
			edit: func(settings *evsPurgeTurnOnSettings, fake *fakeEvsPurgeTurnOnEnv) {
				fake.gate = nil
				fake.readErr = errors.New("sidecar.yml is a directory")
			},
			wantStderr: "sidecar.yml is a directory",
			wantOpened: true,
			wantRead:   true,
		},
		{
			label: "a read that answers no gate",
			edit: func(settings *evsPurgeTurnOnSettings, fake *fakeEvsPurgeTurnOnEnv) {
				fake.gate = nil
			},
			wantStderr: "answered no gate",
			wantOpened: true,
			wantRead:   true,
		},
		{
			label: "a queue that refuses",
			edit: func(settings *evsPurgeTurnOnSettings, fake *fakeEvsPurgeTurnOnEnv) {
				fake.scheduleErr = errors.New("the pool is closed")
			},
			wantStderr:        "the pool is closed",
			wantOpened:        true,
			wantRead:          true,
			wantScheduleTried: true,
		},
		{
			label: "a run whose bound has passed",
			edit: func(settings *evsPurgeTurnOnSettings, fake *fakeEvsPurgeTurnOnEnv) {
				settings.timeout = 0
			},
			wantStderr: "bound passed",
		},
	}
	for _, c := range cases {
		fake := newFakeEvsPurgeTurnOnEnv(&evsFormat2GateRead{enabled: true, purge: "on"})
		now := evsPurgeTurnOnTestNow
		openedEnvs := []string{}
		settings := fakeEvsPurgeTurnOnSettings(fake, &now, &openedEnvs)
		c.edit(settings, fake)
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		if code := evsPurgeTurnOnTestCommands(settings).run([]string{"evs", "purge-turn-on", "test"}, stdout, stderr); code != 1 {
			t.Errorf("%s: exited %d, want 1", c.label, code)
		}
		if !strings.Contains(stderr.String(), c.wantStderr) {
			t.Errorf("%s: printed %q to stderr, want %q in it", c.label, stderr.String(), c.wantStderr)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: printed %q", c.label, stdout.String())
		}
		if (len(openedEnvs) == 1) != c.wantOpened || (fake.readCount == 1) != c.wantRead || (fake.scheduleCount == 1) != c.wantScheduleTried {
			t.Errorf("%s: opened %v, read %d, tried to schedule %d", c.label, openedEnvs, fake.readCount, fake.scheduleCount)
		}
		if len(fake.keyPendingTasks) != 0 {
			t.Errorf("%s: the queue holds %d", c.label, len(fake.keyPendingTasks))
		}
	}
}

// A line with no env, two envs, a flag the command does not have or an env that is not one directory's name is a
// usage mistake, exit 2, and opens no env.
func TestEvsPurgeTurnOnRefusesCommandLinesItCannotTake(t *testing.T) {
	for _, args := range [][]string{{}, {"test", "main"}, {"test", "--dry-run"}, {"../test"}, {"test/x"}, {"."}, {".."}, {".test"}, {""}} {
		fake := newFakeEvsPurgeTurnOnEnv(&evsFormat2GateRead{enabled: true, purge: "on"})
		now := evsPurgeTurnOnTestNow
		openedEnvs := []string{}
		stderr := &bytes.Buffer{}
		code := evsPurgeTurnOnTestCommands(fakeEvsPurgeTurnOnSettings(fake, &now, &openedEnvs)).run(append([]string{"evs", "purge-turn-on"}, args...), io.Discard, stderr)
		if code != 2 || !strings.Contains(stderr.String(), "sidecarctl evs purge-turn-on <env>") {
			t.Errorf("%q: exited %d with %q, want 2 and the usage", args, code, stderr.String())
		}
		if len(openedEnvs) != 0 || fake.scheduleCount != 0 {
			t.Errorf("%q: opened %v and scheduled %d", args, openedEnvs, fake.scheduleCount)
		}
	}
}

// The binary as built reaches one env, the one its WARP_ENV names: asked for another, or with no WARP_ENV, it exits 1
// before it reads a gate or reaches a queue, naming both envs.
func TestEvsPurgeTurnOnRefusesAnEnvThisProcessDoesNotRunAs(t *testing.T) {
	cases := []struct {
		processEnv string
		want       string
	}{
		{processEnv: "canary", want: "WARP_ENV=canary and was asked for test"},
		{processEnv: "", want: "WARP_ENV is not set"},
	}
	for _, c := range cases {
		t.Setenv("WARP_ENV", c.processEnv)
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		if code := sidecarctlCommands.run([]string{"evs", "purge-turn-on", "test"}, stdout, stderr); code != 1 {
			t.Fatalf("WARP_ENV=%q: exited %d, want 1: %s", c.processEnv, code, stderr.String())
		}
		for _, want := range []string{"nothing was scheduled", c.want, "WARP_ENV=test"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("WARP_ENV=%q: printed %q to stderr, want %q in it", c.processEnv, stderr.String(), want)
			}
		}
		if stdout.Len() != 0 {
			t.Errorf("WARP_ENV=%q: printed %q", c.processEnv, stdout.String())
		}
	}
}

// The binary's own registry holds evs purge-turn-on, which its cmd file's init registers, under the usage its doc
// gives, and a set that also holds a shorter evs command still chooses it.
func TestSidecarctlRegistersEvsPurgeTurnOn(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"evs", "purge-turn-on", "test"})
	if command == nil || command.words != "evs purge-turn-on" || command.usage != "<env>" || !slices.Equal(args, []string{"test"}) {
		t.Fatalf("found %v with %v, want evs purge-turn-on <env> with [test]", command, args)
	}

	ranArgs := [][]string{}
	commands := newSidecarctlCommandSet()
	commands.add(recordingSidecarctlCommand("evs", nil, &ranArgs))
	commands.add(command)
	if found, foundArgs := commands.find([]string{"evs", "purge-turn-on", "test"}); found != command || !slices.Equal(foundArgs, []string{"test"}) {
		t.Fatalf("found %v with %v, want evs purge-turn-on with [test]", found, foundArgs)
	}
}
