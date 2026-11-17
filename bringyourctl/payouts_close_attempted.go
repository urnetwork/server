// `bringyourctl payouts close-attempted` (controller/account_payment_attempted_close.go):
// the one-time pass that closes, or records paid, the canceled USDC payments
// an attempt of which reached Circle and that the USDC close-out never closed
// (owner decision 2026-11-13). With `--forfeit=<payment_ids>` it closes only
// the named payments as the owner forfeited them, without reading Circle. It
// goes once the owner has applied it.
package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

func payoutsCloseAttemptedCommand(opts docopt.Opts, stdout io.Writer, stderr io.Writer) int {
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	var forfeit []server.Id
	// present and empty is a refusal, never the Circle pass
	if forfeitStr, ok := opts["--forfeit"].(string); ok {
		paymentIds, err := parsePayoutsForfeitIds(forfeitStr)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return 1
		}
		forfeit = paymentIds
	}
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path, and it reads Circle with GETs only.
	if !commit {
		server.RequirePgReadOnly()
	}
	return runPayoutsCloseAttempted(stdout, stderr, func() (*controller.UsdcAttemptedReport, error) {
		if forfeit != nil {
			return controller.UsdcAttemptedForfeit(context.Background(), forfeit, commit, expect)
		}
		return controller.UsdcAttemptedClose(context.Background(), commit, expect)
	})
}

// The payment ids of `--forfeit`, comma separated, in their order. An empty
// list, an empty or malformed entry and a repeated id are refused, so a typo
// never forfeits a different set.
func parsePayoutsForfeitIds(forfeitStr string) ([]server.Id, error) {
	paymentIds := []server.Id{}
	seen := map[server.Id]bool{}
	for _, part := range strings.Split(forfeitStr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("--forfeit: an empty payment id in %q", forfeitStr)
		}
		paymentId, err := server.ParseId(part)
		if err != nil {
			return nil, fmt.Errorf("--forfeit: %q: %s", part, err)
		}
		if seen[paymentId] {
			return nil, fmt.Errorf("--forfeit: payment %s is named twice", paymentId)
		}
		seen[paymentId] = true
		paymentIds = append(paymentIds, paymentId)
	}
	return paymentIds, nil
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
