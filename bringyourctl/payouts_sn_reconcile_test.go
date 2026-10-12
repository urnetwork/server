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

// RC-18, RC-20: the command is a dry run unless --send is given, and prints
// text unless --json is given.
func TestSnReconcileCommandLine(t *testing.T) {
	for _, args := range [][]string{
		{"payouts", "sn-reconcile"},
		{"payouts", "sn-reconcile", "--send"},
		{"payouts", "sn-reconcile", "--json"},
		{"payouts", "sn-reconcile", "--json", "--send"},
	} {
		opts, err := (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
		if err != nil {
			t.Fatal(args, err)
		}
		payouts, _ := opts.Bool("payouts")
		reconcile, _ := opts.Bool("sn-reconcile")
		send, _ := opts.Bool("--send")
		asJson, _ := opts.Bool("--json")
		if !payouts || !reconcile || send != slices.Contains(args, "--send") || asJson != slices.Contains(args, "--json") {
			t.Fatal("command line", args, opts)
		}
	}
	if _, err := (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, []string{"payouts", "sn-reconcile", "--apply"}, "test"); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}

// RC-20: the report prints as text or as one JSON document; a refusal prints
// its report before the error and exits non-zero.
func TestRunSnReconcilePrintsReport(t *testing.T) {
	report := &controller.SnReconcileReport{RunId: server.NewId(), Time: time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC), Mode: "send",
		Listing: &controller.SnReconcileListing{Complete: true}}
	run := func(send bool, asJson bool, result *controller.SnReconcileReport, err error) (int, string, string, []bool) {
		var stdout, stderr bytes.Buffer
		sends := []bool{}
		code := runSnReconcile(&stdout, &stderr, send, asJson, func(send bool) (*controller.SnReconcileReport, error) {
			sends = append(sends, send)
			return result, err
		})
		return code, stdout.String(), stderr.String(), sends
	}
	if code, stdout, stderr, sends := run(true, false, report, nil); code != 0 || stdout != report.Text() || stderr != "" || !slices.Equal(sends, []bool{true}) {
		t.Fatal("text report", code, stdout, stderr, sends)
	}
	code, stdout, stderr, sends := run(false, true, report, nil)
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil || code != 0 || stderr != "" || document["run_id"] != report.RunId.String() || !slices.Equal(sends, []bool{false}) {
		t.Fatal("JSON report", code, stdout, stderr, err)
	}
	if code, stdout, stderr, _ := run(true, false, report, controller.ErrSnReconcileUnfunded); code != 1 || stdout != report.Text() || stderr != controller.ErrSnReconcileUnfunded.Error()+"\n" {
		t.Fatal("refusal", code, stdout, stderr)
	}
	if code, stdout, stderr, _ := run(false, true, nil, errors.New("synthetic listing failure")); code != 1 || stdout != "" || stderr != "synthetic listing failure\n" {
		t.Fatal("failure", code, stdout, stderr)
	}
}
