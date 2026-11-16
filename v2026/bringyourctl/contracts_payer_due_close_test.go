package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The command lines of the dry run and the commit parse to the close-due
// command with its network, bound and digest, not to contracts close. A commit
// needs the dry run's bound and digest, --dry-run is the default, and one
// without a network is refused.
func TestPayerDueContractCloseCommandLineParses(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	networkId := server.NewId()
	now := time.Date(2026, 10, 29, 8, 0, 30, 500, time.UTC)
	parse := func(args ...string) docopt.Opts {
		t.Helper()
		args = append([]string{"contracts", "close-due", "--network_id=" + networkId.String()}, args...)
		opts, err := parser.ParseArgs(bringyourctlUsage, args, "synthetic")
		if err != nil {
			t.Fatal(args, err)
		}
		closeDue, _ := opts.Bool("close-due")
		closeOne, _ := opts.Bool("close")
		parsedNetworkId, _ := opts.String("--network_id")
		if !closeDue || closeOne || parsedNetworkId != networkId.String() {
			t.Fatal(args, opts)
		}
		return opts
	}

	for _, args := range [][]string{nil, {"--dry-run"}} {
		before, commit, _, err := parsePayerDueContractCloseFlags(parse(args...), now)
		if err != nil || commit || !before.Equal(time.Date(2026, 10, 29, 7, 59, 30, 0, time.UTC)) {
			t.Fatal("a dry run did not default to the drain's bound in whole seconds", args, before, commit, err)
		}
	}
	before, commit, expect, err := parsePayerDueContractCloseFlags(parse("--before=2026-10-29T07:59:30Z", "--commit", "--expect=0123456789abcdef"), now)
	if err != nil || !commit || expect != "0123456789abcdef" || !before.Equal(time.Date(2026, 10, 29, 7, 59, 30, 0, time.UTC)) {
		t.Fatal("the commit line did not parse", before, commit, expect, err)
	}
	if _, _, _, err := parsePayerDueContractCloseFlags(parse("--commit", "--expect=0123456789abcdef"), now); err == nil {
		t.Fatal("a commit without the dry run's bound was accepted")
	}
	if _, _, _, err := parsePayerDueContractCloseFlags(parse("--dry-run", "--before=2026-10-29T07:59:30Z", "--commit", "--expect=0123456789abcdef"), now); err == nil {
		t.Fatal("--dry-run with --commit was accepted")
	}
	if _, err := parser.ParseArgs(bringyourctlUsage, []string{"contracts", "close-due", "--network_id=" + networkId.String(), "--commit"}, "synthetic"); err == nil {
		t.Fatal("--commit parsed without --expect")
	}
	if _, err := parser.ParseArgs(bringyourctlUsage, []string{"contracts", "close-due"}, "synthetic"); err == nil {
		t.Fatal("close-due parsed without a network")
	}
}

// Fake drain runs in order, the due contracts each summary reports, and the
// digest of the due set each digest read reports.
type fakePayerDueContractClose struct {
	summaries []int
	digests   []string
	runs      []*model.PayerDueContractClose
	errs      []error
	closes    int
	closeNows []time.Time
	bounds    []time.Time
}

func (self *fakePayerDueContractClose) ops() payerDueContractCloseOps {
	return payerDueContractCloseOps{
		summarize: func(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (*model.PayerDueContractSummary, error) {
			self.bounds = append(self.bounds, dueBefore)
			count := self.summaries[0]
			self.summaries = self.summaries[1:]
			return &model.PayerDueContractSummary{Count: count, ByteCount: model.ByteCount(count) * model.Mib}, nil
		},
		digest: func(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (int, string, error) {
			self.bounds = append(self.bounds, dueBefore)
			digest := self.digests[0]
			self.digests = self.digests[1:]
			return 0, digest, nil
		},
		close: func(ctx context.Context, payerNetworkId server.Id, now time.Time, more func() bool,
			settings *model.PayerDueContractCloseSettings) (*model.PayerDueContractClose, error) {
			index := self.closes
			self.closes++
			self.closeNows = append(self.closeNows, now)
			return self.runs[index], self.errs[index]
		},
	}
}

// The JSON lines of the output, and its other lines.
func payerDueContractCloseLines(t *testing.T, output string) ([]map[string]any, []string) {
	t.Helper()
	lines := []map[string]any{}
	text := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			text = append(text, line)
			continue
		}
		lines = append(lines, value)
	}
	return lines, text
}

// A dry run reports the payer's due contracts and their digest, prints the
// command that retires exactly them, and runs no drain.
func TestPayerDueContractCloseDryRunClosesNothing(t *testing.T) {
	fake := &fakePayerDueContractClose{summaries: []int{27}, digests: []string{"0123456789abcdef"}}
	var output bytes.Buffer
	networkId := server.NewId()
	before := time.Date(2026, 10, 29, 7, 59, 30, 0, time.UTC)
	if exitCode := runPayerDueContractClose(t.Context(), &output, networkId, before, false, "", fake.ops()); exitCode != 0 {
		t.Fatal("dry run failed", exitCode, output.String())
	}
	lines, text := payerDueContractCloseLines(t, output.String())
	if fake.closes != 0 || len(lines) != 1 || lines[0]["kind"] != "payer-due-contract-summary-v2" || lines[0]["due"] != float64(27) ||
		lines[0]["dry_run"] != true || lines[0]["digest"] != "0123456789abcdef" {
		t.Fatal("dry run did not only report", fake.closes, output.String())
	}
	want := "to retire exactly these: bringyourctl contracts close-due --network_id=" + networkId.String() +
		" --before=2026-10-29T07:59:30Z --commit --expect=0123456789abcdef"
	if len(text) != 1 || text[0] != want {
		t.Fatal("dry run did not print the commit command", text)
	}
}

// A commit whose due set changed after its dry run changes nothing.
func TestPayerDueContractCloseCommitRefusesAChangedSet(t *testing.T) {
	fake := &fakePayerDueContractClose{summaries: []int{25}, digests: []string{"fedcba9876543210"}}
	var output bytes.Buffer
	before := time.Date(2026, 10, 29, 7, 59, 30, 0, time.UTC)
	if exitCode := runPayerDueContractClose(t.Context(), &output, server.NewId(), before, true, "0123456789abcdef", fake.ops()); exitCode != 1 {
		t.Fatal("a commit of a changed set did not fail", exitCode, output.String())
	}
	lines, _ := payerDueContractCloseLines(t, output.String())
	if fake.closes != 0 || len(lines) != 1 || !strings.Contains(lines[0]["error"].(string), "no longer the ones the dry run reported") {
		t.Fatal("a commit of a changed set ran a drain or did not say why", fake.closes, output.String())
	}
}

// A commit retires the contracts due by the dry run's bound: runs continue
// while one closes something and reports more, then the command reports what
// is left. A failed run ends the command with exit code 1.
func TestPayerDueContractCloseRunsWhileTheDrainReportsMore(t *testing.T) {
	before := time.Date(2026, 10, 29, 7, 59, 30, 0, time.UTC)
	fake := &fakePayerDueContractClose{
		summaries: []int{5000, 0},
		digests:   []string{"0123456789abcdef"},
		runs: []*model.PayerDueContractClose{
			{Due: 4096, Closed: 4096, More: true},
			{Due: 904, Closed: 904},
		},
		errs: []error{nil, nil},
	}
	var output bytes.Buffer
	if exitCode := runPayerDueContractClose(t.Context(), &output, server.NewId(), before, true, "0123456789abcdef", fake.ops()); exitCode != 0 {
		t.Fatal("drain failed", exitCode, output.String())
	}
	lines, _ := payerDueContractCloseLines(t, output.String())
	if fake.closes != 2 || len(lines) != 4 || lines[0]["dry_run"] != false || lines[3]["kind"] != "payer-due-contract-left-v1" || lines[3]["due"] != float64(0) {
		t.Fatal("drain did not run until no more were due", fake.closes, output.String())
	}
	// every read and drain is bounded at the dry run's bound
	drainBound := before.Add(model.DefaultPayerDueContractCloseSettings().DueMargin)
	for _, now := range fake.closeNows {
		if !now.Equal(drainBound) {
			t.Fatal("a drain was not bounded at the dry run's bound", now, drainBound)
		}
	}
	for _, bound := range fake.bounds {
		if !bound.Equal(before) {
			t.Fatal("a read was not bounded at the dry run's bound", bound, before)
		}
	}

	failing := &fakePayerDueContractClose{
		summaries: []int{3, 3},
		digests:   []string{"0123456789abcdef"},
		runs:      []*model.PayerDueContractClose{{Due: 3, Failed: 3}},
		errs:      []error{errors.New("synthetic owner failure")},
	}
	output.Reset()
	if exitCode := runPayerDueContractClose(t.Context(), &output, server.NewId(), before, true, "0123456789abcdef", failing.ops()); exitCode != 1 {
		t.Fatal("a failed run did not fail the command", exitCode, output.String())
	}
	if lines, _ := payerDueContractCloseLines(t, output.String()); len(lines) != 3 || lines[1]["error"] != "synthetic owner failure" {
		t.Fatal("a failed run was not reported", output.String())
	}
}
