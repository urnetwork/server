package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// RC-18, RC-20, A7: the command is a dry run unless --send is given, --send
// needs the dry run's digest, and it prints text unless --json is given.
func TestSnReconcileCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	for _, args := range [][]string{
		{"payouts", "sn-reconcile"},
		{"payouts", "sn-reconcile", "--json"},
		{"payouts", "sn-reconcile", "--send", "--expect=digest"},
		{"payouts", "sn-reconcile", "--json", "--send", "--expect=digest"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		payouts, _ := opts.Bool("payouts")
		reconcile, _ := opts.Bool("sn-reconcile")
		send, _ := opts.Bool("--send")
		expect, _ := opts.String("--expect")
		asJson, _ := opts.Bool("--json")
		if !payouts || !reconcile || send != slices.Contains(args, "--send") || (expect == "digest") != send || asJson != slices.Contains(args, "--json") {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"payouts", "sn-reconcile", "--send"},
		{"payouts", "sn-reconcile", "--expect=digest"},
		{"payouts", "sn-reconcile", "--apply"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}
}

// RC-20: the report prints as text or as one JSON document; a refusal prints
// its report before the error and exits non-zero.
func TestRunSnReconcilePrintsReport(t *testing.T) {
	report := &controller.SnReconcileReport{RunId: server.NewId(), Time: time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC), Mode: "send", Digest: "digest",
		Listing: &controller.SnReconcileListing{Complete: true}}
	run := func(send bool, expect string, asJson bool, result *controller.SnReconcileReport, err error) (int, string, string, []string) {
		var stdout, stderr bytes.Buffer
		calls := []string{}
		code := runSnReconcile(&stdout, &stderr, send, expect, asJson, func(send bool, expect string) (*controller.SnReconcileReport, error) {
			calls = append(calls, map[bool]string{true: "send ", false: "dry "}[send]+expect)
			return result, err
		})
		return code, stdout.String(), stderr.String(), calls
	}
	if code, stdout, stderr, calls := run(true, "digest", false, report, nil); code != 0 || stdout != report.Text() || stderr != "" || !slices.Equal(calls, []string{"send digest"}) {
		t.Fatal("text report", code, stdout, stderr, calls)
	}
	code, stdout, stderr, calls := run(false, "", true, report, nil)
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil || code != 0 || stderr != "" || document["run_id"] != report.RunId.String() || !slices.Equal(calls, []string{"dry "}) {
		t.Fatal("JSON report", code, stdout, stderr, err)
	}
	if code, stdout, stderr, _ := run(true, "stale", false, report, controller.ErrSnReconcileDigestMismatch); code != 1 || stdout != report.Text() || stderr != controller.ErrSnReconcileDigestMismatch.Error()+"\n" {
		t.Fatal("refusal", code, stdout, stderr)
	}
	if code, stdout, stderr, _ := run(false, "", true, nil, errors.New("synthetic listing failure")); code != 1 || stdout != "" || stderr != "synthetic listing failure\n" {
		t.Fatal("failure", code, stdout, stderr)
	}
}
