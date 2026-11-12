// Tests for `sidecarctl dns restoring` and `sidecarctl dns restore` against a fake step: which root they choose,
// what they hand the step, what they print, and the exit codes a runbook reads. The step itself is dnsservice's
// (restore_test.go) and its store dnshost's; cmd_dns_restore_server_test.go runs the commands against Postgres and
// Redis on the private test stack.

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

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The root the fake env serves when a test names no other.
const dnsRestoreTestRoot = "zone.example."

// One SetRestoring a fake step took.
type fakeDnsRestoringCall struct {
	root      string
	restoring bool
}

// One Run a fake step took.
type fakeDnsRestoreRun struct {
	root       string
	incidentId sidecar.Id
}

// A step a test sets up: it records what it was asked and answers as told.
type fakeDnsRestoreStep struct {
	restoringCalls []fakeDnsRestoringCall
	restoringErr   error
	runs           []fakeDnsRestoreRun
	// what Run answers, with the root and incident it was asked for filled in
	runProgress *dnsservice.DnsRestoreProgress
	runErr      error
}

// Records the call and answers restoringErr.
func (self *fakeDnsRestoreStep) SetRestoring(ctx context.Context, root string, restoring bool) error {
	self.restoringCalls = append(self.restoringCalls, fakeDnsRestoringCall{root: root, restoring: restoring})
	return self.restoringErr
}

// Records the run and answers runProgress and runErr.
func (self *fakeDnsRestoreStep) Run(ctx context.Context, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error) {
	self.runs = append(self.runs, fakeDnsRestoreRun{root: root, incidentId: incidentId})
	if self.runProgress == nil {
		return nil, self.runErr
	}
	progress := *self.runProgress
	progress.Root = root
	progress.IncidentId = incidentId
	return &progress, self.runErr
}

// Settings that open an env named test serving roots over fake, counting the opens and closes.
func fakeDnsRestoreSettings(fake *fakeDnsRestoreStep, roots []string, openCount *int, closeCount *int) *dnsRestoreSettings {
	settings := defaultDnsRestoreSettings()
	settings.openEnv = func(ctx context.Context, log sidecar.Logger) (*dnsRestoreEnv, error) {
		*openCount += 1
		return &dnsRestoreEnv{
			env:   "test",
			roots: roots,
			step:  fake,
			close: func() { *closeCount += 1 },
		}, nil
	}
	return settings
}

// A command set holding both commands under settings, so a test reads the exit code a runbook step reads.
func dnsRestoreTestCommands(settings *dnsRestoreSettings) *sidecarctlCommandSet {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "dns restoring",
		usage:   "on|off [--root=<root>]",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runDnsRestoringWithSettings(args, settings, out)
		},
	})
	commands.add(&sidecarctlCommand{
		words:   "dns restore",
		usage:   "--incident=<id> [--root=<root>] [--timeout=<duration>]",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runDnsRestoreWithSettings(context.Background(), args, settings, out)
		},
	})
	return commands
}

// on sets restoring and off clears it, for dns.yml's one root or the one --root names in any spelling, and each
// says what it did and closes what it opened.
func TestDnsRestoringSetsAndClearsRestoring(t *testing.T) {
	cases := []struct {
		args  []string
		roots []string
		want  fakeDnsRestoringCall
		print string
	}{
		{args: []string{"on"}, roots: []string{dnsRestoreTestRoot}, want: fakeDnsRestoringCall{root: dnsRestoreTestRoot, restoring: true}, print: "restoring is on for zone.example. in test"},
		{args: []string{"off"}, roots: []string{dnsRestoreTestRoot}, want: fakeDnsRestoringCall{root: dnsRestoreTestRoot, restoring: false}, print: "restoring is off for zone.example. in test"},
		{args: []string{"--root=Other.Example", "on"}, roots: []string{dnsRestoreTestRoot, "other.example."}, want: fakeDnsRestoringCall{root: "other.example.", restoring: true}, print: "restoring is on for other.example. in test"},
		{args: []string{"off", "--root=zone.example."}, roots: []string{dnsRestoreTestRoot, "other.example."}, want: fakeDnsRestoringCall{root: dnsRestoreTestRoot, restoring: false}, print: "restoring is off for zone.example. in test"},
	}
	for _, c := range cases {
		fake := &fakeDnsRestoreStep{}
		openCount, closeCount := 0, 0
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := dnsRestoreTestCommands(fakeDnsRestoreSettings(fake, c.roots, &openCount, &closeCount)).run(append([]string{"dns", "restoring"}, c.args...), stdout, stderr)
		if code != 0 {
			t.Fatalf("%q: exited %d: %s", c.args, code, stderr.String())
		}
		if !slices.Equal(fake.restoringCalls, []fakeDnsRestoringCall{c.want}) {
			t.Errorf("%q: the step took %v, want %v", c.args, fake.restoringCalls, c.want)
		}
		if !strings.Contains(stdout.String(), c.print) {
			t.Errorf("%q: printed %q, want %q in it", c.args, stdout.String(), c.print)
		}
		if openCount != 1 || closeCount != 1 {
			t.Errorf("%q: opened %d and closed %d, want one of each", c.args, openCount, closeCount)
		}
	}
}

// A command line that names no state, or a root dns.yml does not serve, or none where it serves two, exits 2 with
// restoring untouched; a store that refuses exits 1 and prints nothing.
func TestDnsRestoringChangesNothingItCannotName(t *testing.T) {
	twoRoots := []string{dnsRestoreTestRoot, "other.example."}
	cases := []struct {
		args     []string
		roots    []string
		setErr   error
		wantCode int
		want     string
		wantOpen int
		wantSet  int
	}{
		{args: nil, roots: twoRoots, wantCode: 2, want: "want on or off"},
		{args: []string{"maybe"}, roots: twoRoots, wantCode: 2, want: "want on or off"},
		{args: []string{"on", "off"}, roots: twoRoots, wantCode: 2, want: "want on or off"},
		{args: []string{"on", "--incident=1"}, roots: twoRoots, wantCode: 2, want: "flag provided but not defined"},
		{args: []string{"on"}, roots: twoRoots, wantCode: 2, want: "--root must name one", wantOpen: 1},
		{args: []string{"on"}, roots: nil, wantCode: 2, want: "--root must name one", wantOpen: 1},
		{args: []string{"on", "--root=elsewhere.example"}, roots: twoRoots, wantCode: 2, want: "is not a root dns.yml serves", wantOpen: 1},
		{args: []string{"on"}, roots: []string{dnsRestoreTestRoot}, setErr: errors.New("dns tables are not linked in this binary"), wantCode: 1, want: "restoring was not changed for zone.example. in test: dns tables are not linked", wantOpen: 1, wantSet: 1},
	}
	for _, c := range cases {
		fake := &fakeDnsRestoreStep{restoringErr: c.setErr}
		openCount, closeCount := 0, 0
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := dnsRestoreTestCommands(fakeDnsRestoreSettings(fake, c.roots, &openCount, &closeCount)).run(append([]string{"dns", "restoring"}, c.args...), stdout, stderr)
		if code != c.wantCode || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%q: exited %d with %q, want %d and %q", c.args, code, stderr.String(), c.wantCode, c.want)
		}
		if stdout.Len() != 0 {
			t.Errorf("%q: printed %q", c.args, stdout.String())
		}
		if openCount != c.wantOpen || closeCount != c.wantOpen || len(fake.restoringCalls) != c.wantSet {
			t.Errorf("%q: opened %d, closed %d and set %d, want %d, %d and %d", c.args, openCount, closeCount, len(fake.restoringCalls), c.wantOpen, c.wantOpen, c.wantSet)
		}
	}
}

// An env that does not open, a run past its bound and a step that raises all exit 1 with restoring untouched.
func TestDnsRestoringExitsOneWhenTheEnvFails(t *testing.T) {
	settings := defaultDnsRestoreSettings()
	settings.openEnv = func(ctx context.Context, log sidecar.Logger) (*dnsRestoreEnv, error) {
		return nil, errors.New("dns.yml: resource not found")
	}
	stderr := &bytes.Buffer{}
	if code := dnsRestoreTestCommands(settings).run([]string{"dns", "restoring", "on"}, io.Discard, stderr); code != 1 || !strings.Contains(stderr.String(), "restoring was not changed: dns.yml") {
		t.Errorf("an env that does not open: exited %d with %q", code, stderr.String())
	}

	fake := &fakeDnsRestoreStep{}
	openCount, closeCount := 0, 0
	settings = fakeDnsRestoreSettings(fake, []string{dnsRestoreTestRoot}, &openCount, &closeCount)
	settings.restoringTimeout = 0
	stderr.Reset()
	if code := dnsRestoreTestCommands(settings).run([]string{"dns", "restoring", "on"}, io.Discard, stderr); code != 1 || !strings.Contains(stderr.String(), "bound passed") {
		t.Errorf("past its bound: exited %d with %q", code, stderr.String())
	}
	if openCount != 0 || len(fake.restoringCalls) != 0 {
		t.Errorf("past its bound: opened %d and set %d", openCount, len(fake.restoringCalls))
	}

	settings = defaultDnsRestoreSettings()
	settings.openEnv = func(ctx context.Context, log sidecar.Logger) (*dnsRestoreEnv, error) {
		return &dnsRestoreEnv{env: "test", roots: []string{dnsRestoreTestRoot}, step: &raisingDnsRestoreStep{}, close: func() {}}, nil
	}
	stderr.Reset()
	if code := dnsRestoreTestCommands(settings).run([]string{"dns", "restoring", "on"}, io.Discard, stderr); code != 1 || !strings.Contains(stderr.String(), "the store raised") {
		t.Errorf("a step that raises: exited %d with %q", code, stderr.String())
	}
}

// A step whose store raises, as server's model does on a failed statement.
type raisingDnsRestoreStep struct{}

// Raises.
func (self *raisingDnsRestoreStep) SetRestoring(ctx context.Context, root string, restoring bool) error {
	panic(errors.New("the store raised"))
}

// Raises.
func (self *raisingDnsRestoreStep) Run(ctx context.Context, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error) {
	panic(errors.New("the store raised"))
}

// A step that ends exits 0 having run once for the incident and root named, and prints the serial, the NS, the
// counts and the end, and never an owner name.
func TestDnsRestoreRunsTheStepForTheIncident(t *testing.T) {
	incidentId := server.NewId()
	networkId := sidecar.Id(server.NewId())
	clearTime := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	fake := &fakeDnsRestoreStep{runProgress: &dnsservice.DnsRestoreProgress{
		RestoreSerial: 1<<24 + 77,
		CountedNs:     []string{"ns2"},
		DiffCount:     2,
		Listing: &dnsservice.DnsRestoreListing{Entries: []dnsservice.DnsRestoreEntry{
			{NetworkId: &networkId, Owner: "private-owner.zone.example.", Type: 1, Flagged: true},
			{NetworkId: &networkId, Owner: "second-owner.zone.example.", Type: 16, Flagged: true, FlagClearTime: &clearTime},
			{Owner: "third-owner.zone.example.", Type: 28},
		}},
		Done: true,
	}}
	openCount, closeCount := 0, 0
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	args := []string{"dns", "restore", "--incident=" + incidentId.String()}
	if code := dnsRestoreTestCommands(fakeDnsRestoreSettings(fake, []string{dnsRestoreTestRoot}, &openCount, &closeCount)).run(args, stdout, stderr); code != 0 {
		t.Fatalf("exited %d: %s", code, stderr.String())
	}
	if !slices.Equal(fake.runs, []fakeDnsRestoreRun{{root: dnsRestoreTestRoot, incidentId: sidecar.Id(incidentId)}}) {
		t.Fatalf("the step ran %v, want once for %s on %s", fake.runs, incidentId, dnsRestoreTestRoot)
	}
	for _, want := range []string{
		"zone.example. in test, incident " + incidentId.String(),
		"restore serial: 16777293",
		"ns stopped until they serve it: none",
		"ns that did not answer and are still counted: ns2",
		"ns not yet serving it: none",
		"diffs posted at or above it: 2",
		"rrsets listed under the incident: 3, 1 of them flagged",
		"done: restoring is clear",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("printed %q, want %q in it", stdout.String(), want)
		}
	}
	for _, private := range []string{"owner", networkId.String()} {
		if strings.Contains(stdout.String(), private) {
			t.Errorf("printed %q, which names %q", stdout.String(), private)
		}
	}
	if openCount != 1 || closeCount != 1 {
		t.Errorf("opened %d and closed %d, want one of each", openCount, closeCount)
	}
}

// A step that has not ended never exits 0: its bound, a root that is not restoring, another incident's restore
// and an answer with no end each exit 1, saying which, after printing where the run stood.
func TestDnsRestoreExitsOneUntilTheStepEnds(t *testing.T) {
	waiting := &dnsservice.DnsRestoreProgress{RestoreSerial: 1 << 24, WaitingNs: []string{"ns1", "ns2"}}
	cases := []struct {
		name      string
		progress  *dnsservice.DnsRestoreProgress
		runErr    error
		want      string
		wantPrint string
	}{
		{name: "the bound", progress: waiting, runErr: context.DeadlineExceeded, want: "has not ended; rerun with --incident=", wantPrint: "ns not yet serving it: ns1 ns2"},
		{name: "not restoring", runErr: dnsservice.ErrDnsNotRestoring, want: "is not restoring in test"},
		{name: "another incident", runErr: dnsservice.ErrDnsRestoreIncident, want: "committed under another incident"},
		{name: "no root row", runErr: dnsservice.ErrDnsRootUnknown, want: "has no dns_root row in test"},
		{name: "unlinked tables", runErr: model.ErrDnsUnlinked, want: "did not run and changed nothing: dns tables are not linked"},
		{name: "no end", progress: waiting, want: "answered no end", wantPrint: "restore serial: 16777216"},
		{name: "no answer", want: "answered no end"},
	}
	for _, c := range cases {
		fake := &fakeDnsRestoreStep{runProgress: c.progress, runErr: c.runErr}
		openCount, closeCount := 0, 0
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		args := []string{"dns", "restore", "--incident=" + server.NewId().String()}
		code := dnsRestoreTestCommands(fakeDnsRestoreSettings(fake, []string{dnsRestoreTestRoot}, &openCount, &closeCount)).run(args, stdout, stderr)
		if code != 1 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: exited %d with %q, want 1 and %q", c.name, code, stderr.String(), c.want)
		}
		if !strings.Contains(stdout.String(), c.wantPrint) || strings.Contains(stdout.String(), "done:") {
			t.Errorf("%s: printed %q, want %q and no end", c.name, stdout.String(), c.wantPrint)
		}
		if len(fake.runs) != 1 || closeCount != 1 {
			t.Errorf("%s: ran %d and closed %d, want one of each", c.name, len(fake.runs), closeCount)
		}
	}
}

// A command line with no incident, an id that is none, a stray word, an unknown root or a spent bound runs no step.
func TestDnsRestoreRunsNothingItCannotName(t *testing.T) {
	incident := "--incident=" + server.NewId().String()
	cases := []struct {
		args     []string
		wantCode int
		want     string
	}{
		{args: nil, wantCode: 2, want: "--incident=<id> names the incident"},
		{args: []string{"--incident=nonsense"}, wantCode: 2, want: "is not an incident id"},
		{args: []string{"--incident=" + (server.Id{}).String()}, wantCode: 2, want: "is not an incident id"},
		{args: []string{incident, "now"}, wantCode: 2, want: "takes flags only"},
		{args: []string{incident, "--root=elsewhere.example"}, wantCode: 2, want: "is not a root dns.yml serves"},
		{args: []string{incident, "--timeout=soon"}, wantCode: 2, want: "invalid value"},
		{args: []string{incident, "--timeout=0s"}, wantCode: 1, want: "did not run: the run's 0s bound passed"},
	}
	for _, c := range cases {
		fake := &fakeDnsRestoreStep{runProgress: &dnsservice.DnsRestoreProgress{Done: true}}
		openCount, closeCount := 0, 0
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := dnsRestoreTestCommands(fakeDnsRestoreSettings(fake, []string{dnsRestoreTestRoot}, &openCount, &closeCount)).run(append([]string{"dns", "restore"}, c.args...), stdout, stderr)
		if code != c.wantCode || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%q: exited %d with %q, want %d and %q", c.args, code, stderr.String(), c.wantCode, c.want)
		}
		if len(fake.runs) != 0 || stdout.Len() != 0 || openCount != closeCount {
			t.Errorf("%q: ran %d, printed %q, opened %d and closed %d", c.args, len(fake.runs), stdout.String(), openCount, closeCount)
		}
	}
}

// The step's log lines reach the command's output, a line each, with a warning marked.
func TestDnsRestoreLoggerWritesLines(t *testing.T) {
	out := &bytes.Buffer{}
	var log sidecar.Logger = &dnsRestoreLogger{out: out}
	log.Infof("[dns]restore: %s lands at serial %d", dnsRestoreTestRoot, 7)
	log.Warningf("[dns]restore: %s did not answer\n", "ns1")
	log.Errorf("[dns]restore: %v", errors.New("failed"))
	log.Info("plain")
	if log.V(1).Enabled() {
		t.Error("a verbose level is on")
	}
	want := "[dns]restore: zone.example. lands at serial 7\nwarning: [dns]restore: ns1 did not answer\nerror: [dns]restore: failed\nplain\n"
	if out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
}

// The binary as built refuses both commands with no WARP_ENV before it reads dns.yml or opens a store, and its
// registry holds them apart though one's words begin the other's.
func TestDnsRestoreCommandsRefuseWithNoEnv(t *testing.T) {
	t.Setenv("WARP_ENV", "")
	for _, args := range [][]string{
		{"dns", "restoring", "on"},
		{"dns", "restore", "--incident=" + server.NewId().String()},
	} {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		if code := sidecarctlCommands.run(args, stdout, stderr); code != 1 {
			t.Fatalf("%q: exited %d, want 1: %s", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "WARP_ENV is not set") || stdout.Len() != 0 {
			t.Errorf("%q: printed %q and %q, want the refusal alone", args, stdout.String(), stderr.String())
		}
	}
	if command, _ := sidecarctlCommands.find([]string{"dns", "restoring", "on"}); command == nil || command.words != "dns restoring" {
		t.Errorf("dns restoring on chose %v", command)
	}
	if command, _ := sidecarctlCommands.find([]string{"dns", "restore"}); command == nil || command.words != "dns restore" {
		t.Errorf("dns restore chose %v", command)
	}
}
