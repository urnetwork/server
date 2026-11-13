// `bringyourctl payouts close-attempted` (controller/account_payment_attempted_close.go):
// the one-time pass that closes, or records paid, the canceled USDC payments
// an attempt of which reached Circle and that the USDC close-out never closed
// (owner decision 2026-11-13). It goes once the owner has applied it.
package main

import (
	"context"
	"fmt"
	"io"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

func payoutsCloseAttemptedCommand(opts docopt.Opts, stdout io.Writer, stderr io.Writer) int {
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path, and it reads Circle with GETs only.
	if !commit {
		server.RequirePgReadOnly()
	}
	return runPayoutsCloseAttempted(stdout, stderr, func() (*controller.UsdcAttemptedReport, error) {
		return controller.UsdcAttemptedClose(context.Background(), commit, expect)
	})
}

// Prints the decisions and returns the exit code: 0 for a dry run and for a
// commit that wrote, 1 for a refusal or a failure.
func runPayoutsCloseAttempted(stdout io.Writer, stderr io.Writer, run func() (*controller.UsdcAttemptedReport, error)) int {
	report, err := run()
	if report != nil {
		fmt.Fprint(stdout, report.Text())
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	return 0
}
