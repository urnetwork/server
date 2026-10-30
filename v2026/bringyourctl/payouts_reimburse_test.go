package main

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

// The command is a dry run unless --send is given, and --send needs the dry
// run's digest.
func TestPayoutsReimburseCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	for _, args := range [][]string{
		{"payouts", "reimburse", "--file=rows.csv", "--reason=synthetic reimbursement"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=synthetic reimbursement", "--send", "--expect=digest"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		payouts, _ := opts.Bool("payouts")
		reimburse, _ := opts.Bool("reimburse")
		file, _ := opts.String("--file")
		reason, _ := opts.String("--reason")
		send, _ := opts.Bool("--send")
		expect, _ := opts.String("--expect")
		if !payouts || !reimburse || file != "rows.csv" || reason != "synthetic reimbursement" || send != slices.Contains(args, "--send") || (expect == "digest") != send {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"payouts", "reimburse"},
		{"payouts", "reimburse", "--file=rows.csv"},
		{"payouts", "reimburse", "--reason=r"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--send"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--expect=digest"},
		{"payouts", "reimburse", "--file=rows.csv", "--reason=r", "--dry-run"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}
}

// The report prints before any error; a clean dry run ends with the exact send
// command, with the file and reason quoted for the shell.
func TestRunPayoutsReimbursePrintsReport(t *testing.T) {
	report := &controller.PayoutReimbursementReport{RunId: server.NewId(), Time: time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC), Mode: "dry_run",
		Reason: "it's a synthetic reimbursement", Digest: strings.Repeat("ab", 32)}
	run := func(send bool, result *controller.PayoutReimbursementReport, err error, readErr error) (int, string, string, []string) {
		var stdout, stderr bytes.Buffer
		reads := []string{}
		code := runPayoutsReimburse(&stdout, &stderr, "/tmp/rows file.csv", report.Reason, send, func(path string) ([]byte, error) {
			reads = append(reads, path)
			return []byte("network_id,wallet_id,amount_usdc\n"), readErr
		}, func(data []byte) (*controller.PayoutReimbursementReport, error) {
			if string(data) != "network_id,wallet_id,amount_usdc\n" {
				t.Fatal("the command did not pass the file's bytes")
			}
			return result, err
		})
		return code, stdout.String(), stderr.String(), reads
	}

	code, stdout, stderr, reads := run(false, report, nil, nil)
	sendLine := "to send exactly this: bringyourctl payouts reimburse --file='/tmp/rows file.csv' --reason='it'\\''s a synthetic reimbursement' --send --expect=" + report.Digest + "\n"
	if code != 0 || stdout != report.Text()+sendLine || stderr != "" || !slices.Equal(reads, []string{"/tmp/rows file.csv"}) {
		t.Fatalf("dry run: %d %q %q", code, stdout, stderr)
	}
	sent := *report
	sent.Mode = "send"
	if code, stdout, stderr, _ := run(true, &sent, nil, nil); code != 0 || stdout != sent.Text() || stderr != "" {
		t.Fatalf("send: %d %q %q", code, stdout, stderr)
	}
	if code, stdout, stderr, _ := run(true, &sent, controller.ErrPayoutReimbursementDigestMismatch, nil); code != 1 || stdout != sent.Text() || stderr != controller.ErrPayoutReimbursementDigestMismatch.Error()+"\n" {
		t.Fatalf("refusal: %d %q %q", code, stdout, stderr)
	}
	if code, stdout, stderr, _ := run(false, nil, nil, errors.New("synthetic missing file")); code != 1 || stdout != "" || !strings.Contains(stderr, "synthetic missing file") {
		t.Fatalf("unreadable file: %d %q %q", code, stdout, stderr)
	}
}

// The dry run's process is read only before it opens a database session, as
// for sn-reconcile; --send runs in a writable process.
func TestPayoutsReimburseDryRunProcessIsReadOnly(t *testing.T) {
	source, err := os.ReadFile("payouts_reimburse.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	readOnly := strings.Index(text, "if !send {\n\t\tserver.RequirePgReadOnly()\n\t}")
	session := strings.Index(text, "session.NewLocalClientSession(")
	run := strings.Index(text, "controller.PayoutsReimburse(")
	if readOnly < 0 || session < readOnly || run < readOnly {
		t.Fatal("the dry run opens a database session before its process is read only")
	}
}
