// Tests of `payouts reimburse --allow_prior_reimbursement` on the command line.
package main

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

// --allow_prior_reimbursement is an option of payouts reimburse, for the dry run
// and the send alike; it does not stand in for --expect, and the command passes
// it to the controller.
func TestPayoutsReimburseAllowPriorCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	for _, args := range [][]string{
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--allow_prior_reimbursement"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--allow_prior_reimbursement", "--send", "--expect=digest"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--send", "--expect=digest", "--allow_prior_reimbursement"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		allowPriorReimbursement, err := opts.Bool("--allow_prior_reimbursement")
		send, _ := opts.Bool("--send")
		expect, _ := opts.String("--expect")
		if err != nil || allowPriorReimbursement != slices.Contains(args, "--allow_prior_reimbursement") || send != slices.Contains(args, "--send") || (expect == "digest") != send {
			t.Fatal("command line", args, opts, err)
		}
	}
	for _, args := range [][]string{
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--allow_prior_reimbursement", "--send"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--allow_prior_reimbursement", "--expect=digest"},
		{"payouts", "reimburse", "--allow_prior_reimbursement"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}

	source, err := os.ReadFile("payouts_reimburse.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, `allowPriorReimbursement, _ := opts.Bool("--allow_prior_reimbursement")`) ||
		!strings.Contains(text, "controller.PayoutsReimburse(clientSession, data, reason, send, expect, allowPriorReimbursement)") {
		t.Fatal("the command does not pass --allow_prior_reimbursement to the controller")
	}
}

// A clean dry run with --allow_prior_reimbursement ends with the send command
// that repeats the flag, which the digest binds.
func TestRunPayoutsReimburseRepeatsAllowPriorInTheSendCommand(t *testing.T) {
	report := &controller.PayoutReimbursementReport{RunId: server.NewId(), Time: time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC), Mode: "dry_run",
		Reason: "synthetic per-window reliability subsidy", AllowPriorReimbursement: true, Digest: strings.Repeat("cd", 32)}
	var stdout, stderr bytes.Buffer
	code := runPayoutsReimburse(&stdout, &stderr, "rows.csv", report.Reason, false, func(string) ([]byte, error) {
		return []byte("network_id,wallet_id,amount_usdc\n"), nil
	}, func([]byte) (*controller.PayoutReimbursementReport, error) {
		return report, nil
	})
	sendLine := "to send exactly this: bringyourctl payouts reimburse --file='rows.csv' --reason='synthetic per-window reliability subsidy' --allow_prior_reimbursement --send --expect=" + report.Digest + "\n"
	if code != 0 || stdout.String() != report.Text()+sendLine || stderr.Len() != 0 {
		t.Fatalf("dry run: %d %q %q", code, stdout.String(), stderr.String())
	}
}
