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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// The command is a dry run unless --apply is given, and --apply needs the dry
// run's digest.
func TestPayoutsCloseParkedCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	for _, args := range [][]string{
		{"payouts", "close-parked"},
		{"payouts", "close-parked", "--ids"},
		{"payouts", "close-parked", "--apply", "--expect=digest"},
		{"payouts", "close-parked", "--apply", "--expect=digest", "--ids"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		payouts, _ := opts.Bool("payouts")
		closeParked, _ := opts.Bool("close-parked")
		apply, _ := opts.Bool("--apply")
		expect, _ := opts.String("--expect")
		ids, _ := opts.Bool("--ids")
		if !payouts || !closeParked || apply != slices.Contains(args, "--apply") || (expect == "digest") != apply || ids != slices.Contains(args, "--ids") {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"payouts", "close-parked", "--apply"},
		{"payouts", "close-parked", "--expect=digest"},
		{"payouts", "close-parked", "--send", "--expect=digest"},
		{"payouts", "close-parked", "--dry-run"},
		{"payouts", "close-parked", "--json"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}
}

// The report prints before any error; a dry run that found parked payments ends
// with the exact apply command.
func TestRunPayoutsCloseParkedPrintsReport(t *testing.T) {
	parked := &controller.PayoutCloseParkedSet{Count: 2, Reasons: []*controller.PayoutCloseParkedTotal{}, Weeks: []*controller.PayoutCloseParkedTotal{},
		ManualReimbursements: &controller.PayoutCloseParkedTotal{}, Rows: []*controller.PayoutCloseParkedRow{{PaymentId: server.NewId(), Week: "2026-10-19", Reason: "other", PayoutNanoCents: 1}}}
	report := &controller.PayoutCloseParkedReport{RunId: server.NewId(), Time: time.Date(2026, 10, 27, 23, 0, 0, 0, time.UTC), Mode: "dry_run",
		UsdcPayments: "off", Digest: strings.Repeat("ab", 32), Parked: parked}
	run := func(apply bool, ids bool, result *controller.PayoutCloseParkedReport, err error) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := runPayoutsCloseParked(&stdout, &stderr, apply, ids, func() (*controller.PayoutCloseParkedReport, error) {
			return result, err
		})
		return code, stdout.String(), stderr.String()
	}

	applyLine := "to close exactly these: bringyourctl payouts close-parked --apply --expect=" + report.Digest + "\n"
	if code, stdout, stderr := run(false, false, report, nil); code != 0 || stdout != report.Text(false)+applyLine || stderr != "" {
		t.Fatalf("dry run: %d %q %q", code, stdout, stderr)
	}
	if code, stdout, _ := run(false, true, report, nil); code != 0 || stdout != report.Text(true)+applyLine || !strings.Contains(stdout, parked.Rows[0].PaymentId.String()) {
		t.Fatalf("dry run with ids: %d %q", code, stdout)
	}
	none := *report
	none.Parked = &controller.PayoutCloseParkedSet{Reasons: []*controller.PayoutCloseParkedTotal{}, Weeks: []*controller.PayoutCloseParkedTotal{},
		ManualReimbursements: &controller.PayoutCloseParkedTotal{}, Rows: []*controller.PayoutCloseParkedRow{}}
	if code, stdout, _ := run(false, false, &none, nil); code != 0 || stdout != none.Text(false) {
		t.Fatalf("dry run without parked payments: %d %q", code, stdout)
	}
	applied := *report
	applied.Mode = "apply"
	applied.Closed = parked
	if code, stdout, stderr := run(true, false, &applied, nil); code != 0 || stdout != applied.Text(false) || stderr != "" {
		t.Fatalf("apply: %d %q %q", code, stdout, stderr)
	}
	if code, stdout, stderr := run(true, false, &applied, controller.ErrPayoutCloseParkedDigestMismatch); code != 1 || stdout != applied.Text(false) ||
		stderr != controller.ErrPayoutCloseParkedDigestMismatch.Error()+"\n" {
		t.Fatalf("refusal: %d %q %q", code, stdout, stderr)
	}
	if code, stdout, stderr := run(false, false, nil, errors.New("synthetic census failure")); code != 1 || stdout != "" || !strings.Contains(stderr, "synthetic census failure") {
		t.Fatalf("failure: %d %q %q", code, stdout, stderr)
	}
}

// The dry run's process is read only before it opens a database session, as
// for sn-reconcile; --apply runs in a writable process.
func TestPayoutsCloseParkedDryRunProcessIsReadOnly(t *testing.T) {
	source, err := os.ReadFile("payouts_close_parked.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	readOnly := strings.Index(text, "if !apply {\n\t\tserver.RequirePgReadOnly()\n\t}")
	session := strings.Index(text, "session.NewLocalClientSession(")
	run := strings.Index(text, "controller.PayoutsCloseParked(")
	if readOnly < 0 || session < readOnly || run < readOnly {
		t.Fatal("the dry run opens a database session before its process is read only")
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "os.Exit(payoutsCloseParkedCommand(opts))") {
		t.Fatal("main does not run the close-parked command")
	}
}
