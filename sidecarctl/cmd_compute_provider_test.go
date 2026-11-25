// `sidecarctl compute provider ls|gate|suspend|resume` with the env and its store replaced: the command lines each
// takes and refuses, a gate without a result among them, what each prints, and that a refused line opens no store.
// cmd_compute_provider_server_test.go runs them against the private test stack's compute_provider.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/computeservice"
)

// A store a test sets: the rows ls reads and what each change answers, and every call made.
type fakeComputeProviderCommandStore struct {
	rows    []*computeservice.ComputeProviderRow
	answer  *computeservice.ComputeProviderRow
	err     error
	calls   []string
	results []*computeservice.ComputeProviderGateResult
	changes []*computeservice.ComputeProviderStatusChange
}

// The rows set.
func (self *fakeComputeProviderCommandStore) Providers(ctx context.Context) ([]*computeservice.ComputeProviderRow, error) {
	self.calls = append(self.calls, "providers")
	return self.rows, self.err
}

// The answer set, recording the result.
func (self *fakeComputeProviderCommandStore) RecordGate(ctx context.Context, id computeservice.ComputeProviderId, result *computeservice.ComputeProviderGateResult, at time.Time) (*computeservice.ComputeProviderRow, error) {
	self.calls = append(self.calls, "gate "+string(id))
	self.results = append(self.results, result)
	return self.answer, self.err
}

// The answer set, recording the change.
func (self *fakeComputeProviderCommandStore) Suspend(ctx context.Context, id computeservice.ComputeProviderId, change *computeservice.ComputeProviderStatusChange, at time.Time) (*computeservice.ComputeProviderRow, error) {
	self.calls = append(self.calls, "suspend "+string(id))
	self.changes = append(self.changes, change)
	return self.answer, self.err
}

// The answer set, recording the change.
func (self *fakeComputeProviderCommandStore) Resume(ctx context.Context, id computeservice.ComputeProviderId, change *computeservice.ComputeProviderStatusChange, at time.Time) (*computeservice.ComputeProviderRow, error) {
	self.calls = append(self.calls, "resume "+string(id))
	self.changes = append(self.changes, change)
	return self.answer, self.err
}

// The binary's four compute provider commands over store, at now, opened only for env local.
func computeProviderTestCommands(store *fakeComputeProviderCommandStore, now time.Time, opened *int) *sidecarctlCommandSet {
	settings := defaultComputeProviderCommandSettings()
	settings.now = func() time.Time { return now }
	settings.openStore = func(ctx context.Context, env string, providers *computeservice.ComputeProviderSettings) (computeProviderCommandStore, error) {
		*opened += 1
		if env != "local" {
			return nil, errors.New("this process runs as WARP_ENV=local")
		}
		return store, nil
	}
	commands := newSidecarctlCommandSet()
	for _, command := range []*sidecarctlCommand{
		{words: "compute provider ls", usage: "<env>", run: func(args []string, out io.Writer) error {
			return runComputeProviderLsWithSettings(args, settings, out)
		}},
		{words: "compute provider gate", usage: "<env> <provider>", run: func(args []string, out io.Writer) error {
			return runComputeProviderGateWithSettings(args, settings, out)
		}},
		{words: "compute provider suspend", usage: "<env> <provider>", run: func(args []string, out io.Writer) error {
			return runComputeProviderChangeWithSettings(args, computeservice.ComputeProviderHistorySuspend, settings, out)
		}},
		{words: "compute provider resume", usage: "<env> <provider>", run: func(args []string, out io.Writer) error {
			return runComputeProviderChangeWithSettings(args, computeservice.ComputeProviderHistoryResume, settings, out)
		}},
	} {
		commands.add(command)
	}
	return commands
}

// Runs one command line over the test commands: its exit code and output.
func runComputeProviderTestCommand(store *fakeComputeProviderCommandStore, now time.Time, opened *int, args ...string) (int, string, string) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := computeProviderTestCommands(store, now, opened).run(args, stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// The four commands are the binary's own.
func TestComputeProviderCommandsAreRegistered(t *testing.T) {
	for _, words := range []string{"compute provider ls", "compute provider gate", "compute provider suspend", "compute provider resume"} {
		command, _ := sidecarctlCommands.find(strings.Fields(words))
		if command == nil || command.words != words {
			t.Errorf("%q is not registered", words)
		}
	}
}

// A line no command takes exits 2 with its usage and opens no store: a gate without --result, or with a result that
// is neither pass nor fail, without --gate or --by; a provider no table names; the wrong arguments; an env that names
// no directory; a suspension without its reason, a resumption with one.
func TestComputeProviderCommandsRefuseBadLines(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cases := [][]string{
		{"compute", "provider", "gate", "local", "engy", "--gate=G-PROV-engy", "--by=rota-a"},
		{"compute", "provider", "gate", "local", "engy", "--gate=G-PROV-engy", "--result=maybe", "--by=rota-a"},
		{"compute", "provider", "gate", "local", "engy", "--result=pass", "--by=rota-a"},
		{"compute", "provider", "gate", "local", "engy", "--gate=G-PROV-engy", "--result=pass"},
		{"compute", "provider", "gate", "local", "targon", "--gate=G-PROV-any", "--result=pass", "--by=rota-a"},
		{"compute", "provider", "gate", "local", "--gate=G-PROV-engy", "--result=pass", "--by=rota-a"},
		{"compute", "provider", "gate", "../main", "engy", "--gate=G-PROV-engy", "--result=pass", "--by=rota-a"},
		{"compute", "provider", "gate", "local", "engy", "--gate=G-PROV-engy", "--result=pass", "--by=rota-a", "--unknown=x"},
		{"compute", "provider", "suspend", "local", "engy", "--by=rota-a"},
		{"compute", "provider", "suspend", "local", "engy", "--reason=its account was suspended"},
		{"compute", "provider", "resume", "local", "engy", "--by=rota-a", "--reason=back"},
		{"compute", "provider", "ls"},
		{"compute", "provider", "ls", "local", "engy"},
	}
	for _, args := range cases {
		opened := 0
		store := &fakeComputeProviderCommandStore{}
		code, stdout, stderr := runComputeProviderTestCommand(store, now, &opened, args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "Usage:") {
			t.Errorf("%q: exited %d with %q %q", args, code, stdout, stderr)
		}
		if opened != 0 || len(store.calls) != 0 {
			t.Errorf("%q: opened %d stores, called %v", args, opened, store.calls)
		}
	}
}

// A gate's line reaches the store with every field, and prints the entry recorded and what the status means.
func TestComputeProviderGateRecordsWhatItWasGiven(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	opened := 0
	store := &fakeComputeProviderCommandStore{answer: &computeservice.ComputeProviderRow{
		Id: computeservice.ComputeProviderIdEngy, Status: computeservice.ComputeProviderStatusOut,
		History: []*computeservice.ComputeProviderHistoryEntry{{
			Kind: computeservice.ComputeProviderHistoryGate, Gate: computeservice.ComputeProviderGateAny,
			Result: computeservice.ComputeProviderGateFail, From: computeservice.ComputeProviderStatusGated,
			To: computeservice.ComputeProviderStatusOut, TimeMs: now.UnixMilli(), By: "rota-a",
		}},
	}}
	code, stdout, stderr := runComputeProviderTestCommand(store, now, &opened, "compute", "provider", "gate", "local", "engy",
		"--gate=G-PROV-any", "--result=fail", "--by=rota-a", "--evidence=sha256:00ff", "--note=no signed receipts")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if len(store.results) != 1 || *store.results[0] != (computeservice.ComputeProviderGateResult{
		Gate: "G-PROV-any", Passed: false, By: "rota-a", Evidence: "sha256:00ff", Note: "no signed receipts",
	}) {
		t.Fatalf("the store took %+v", store.results)
	}
	for _, want := range []string{"recorded G-PROV-any fail for engy in local: gated, then out", "engy takes no new work"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("printed %q, want %q in it", stdout, want)
		}
	}

	// a store's refusal exits 1, printing nothing but the error
	store = &fakeComputeProviderCommandStore{err: computeservice.ErrComputeProviderTransition}
	code, stdout, stderr = runComputeProviderTestCommand(store, now, &opened, "compute", "provider", "suspend", "local", "engy",
		"--by=rota-a", "--reason=its account was suspended")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "engy was not changed in local") {
		t.Fatalf("a refused suspension: %d %q %q", code, stdout, stderr)
	}
	// an env the process does not run as reads and writes nothing
	store = &fakeComputeProviderCommandStore{}
	code, _, stderr = runComputeProviderTestCommand(store, now, &opened, "compute", "provider", "resume", "main", "engy", "--by=rota-a")
	if code != 1 || len(store.calls) != 0 || !strings.Contains(stderr, "nothing was read or written in main") {
		t.Fatalf("another env: %d %v %q", code, store.calls, stderr)
	}
}

// ls lists every provider this build knows, a row's status, gates and due date where it has one, overdue past it,
// listed with no row where it has none, and G-PROV-any not at all for web search.
func TestComputeProviderLsListsEveryProvider(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	passed := now.AddDate(0, 0, -100)
	opened := 0
	store := &fakeComputeProviderCommandStore{rows: []*computeservice.ComputeProviderRow{
		{
			Id: computeservice.ComputeProviderIdEngy, Code: computeservice.ComputeProviderCodeEngy, Kind: computeservice.ComputeProviderKindInference,
			Status: computeservice.ComputeProviderStatusGated, StatusTime: passed,
			History: []*computeservice.ComputeProviderHistoryEntry{{
				Kind: computeservice.ComputeProviderHistoryGate, Gate: "G-PROV-engy", Result: computeservice.ComputeProviderGatePass,
				From: computeservice.ComputeProviderStatusListed, To: computeservice.ComputeProviderStatusGated, TimeMs: passed.UnixMilli(), By: "rota-a",
			}},
		},
	}}
	code, stdout, stderr := runComputeProviderTestCommand(store, now, &opened, "compute", "provider", "ls", "local")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2+len(computeservice.ComputeProviderIds()) {
		t.Fatalf("printed %d lines:\n%s", len(lines), stdout)
	}
	byProvider := map[string]string{}
	for _, line := range lines[2:] {
		byProvider[strings.Fields(line)[0]] = line
	}
	engy := byProvider["engy"]
	for _, want := range []string{"gated", "G-PROV-engy pass " + passed.Format(time.RFC3339) + " by rota-a", passed.AddDate(0, 0, 90).Format(time.RFC3339) + " overdue"} {
		if !strings.Contains(engy, want) {
			t.Errorf("engy's line %q lacks %q", engy, want)
		}
	}
	if chutes := byProvider["chutes"]; !strings.Contains(chutes, "listed") || !strings.Contains(chutes, "no row") || !strings.Contains(chutes, "G-PROV-chutes none") {
		t.Errorf("chutes' line reads %q", chutes)
	}
	if parallel := byProvider["parallel"]; !strings.Contains(parallel, "search") || !strings.Contains(parallel, "G-SRCH-parallel none") {
		t.Errorf("parallel's line reads %q", parallel)
	}
	if len(store.calls) != 1 || store.calls[0] != "providers" {
		t.Fatalf("ls called %v", store.calls)
	}
}
