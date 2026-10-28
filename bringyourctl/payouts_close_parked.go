// `payouts close-parked` (controller/account_payment_close_parked.go): closes
// the parked USDC payments once usdc_payout.yml turns payments off, forfeiting
// their obligations (SN-RECONCILE.md, "Closing the parked payments").
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

func payoutsCloseParkedCommand(opts docopt.Opts) int {
	apply, _ := opts.Bool("--apply")
	expect, _ := opts.String("--expect")
	ids, _ := opts.Bool("--ids")
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path, as for sn-reconcile.
	if !apply {
		server.RequirePgReadOnly()
	}
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	return runPayoutsCloseParked(os.Stdout, os.Stderr, apply, ids, func() (*controller.PayoutCloseParkedReport, error) {
		return controller.PayoutsCloseParked(clientSession, apply, expect)
	})
}

// Prints the report and returns the exit code. A refusal or a partial apply
// prints its report too, then its error, and exits non-zero. A dry run that
// found parked payments ends with the command that closes exactly them.
func runPayoutsCloseParked(
	stdout io.Writer,
	stderr io.Writer,
	apply bool,
	ids bool,
	closeParked func() (*controller.PayoutCloseParkedReport, error),
) int {
	report, err := closeParked()
	if report != nil {
		fmt.Fprint(stdout, report.Text(ids))
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	if !apply && report != nil && report.Parked != nil && 0 < report.Parked.Count {
		fmt.Fprintf(stdout, "to close exactly these: bringyourctl payouts close-parked --apply --expect=%s\n", report.Digest)
	}
	return 0
}
