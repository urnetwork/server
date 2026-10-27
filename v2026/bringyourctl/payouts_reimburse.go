// `payouts reimburse` (controller/account_payment_reimbursement.go): one-off
// USDC reimbursements to pinned wallets through the ordinary Circle payout path.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
)

const payoutsReimburseMaxFileSize = 1024 * 1024

func payoutsReimburseCommand(opts docopt.Opts) int {
	file, _ := opts.String("--file")
	reason, _ := opts.String("--reason")
	send, _ := opts.Bool("--send")
	expect, _ := opts.String("--expect")
	// The dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path, as for sn-reconcile.
	if !send {
		server.RequirePgReadOnly()
	}
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	return runPayoutsReimburse(os.Stdout, os.Stderr, file, reason, send, os.ReadFile, func(data []byte) (*controller.PayoutReimbursementReport, error) {
		return controller.PayoutsReimburse(clientSession, data, reason, send, expect)
	})
}

// Prints the report and returns the exit code. A refusal prints its report too,
// then its error, and exits non-zero. A clean dry run ends with the command
// that sends exactly what it printed.
func runPayoutsReimburse(
	stdout io.Writer,
	stderr io.Writer,
	file string,
	reason string,
	send bool,
	readFile func(string) ([]byte, error),
	reimburse func([]byte) (*controller.PayoutReimbursementReport, error),
) int {
	data, err := readFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "payouts reimburse: reading %s: %s\n", file, err)
		return 1
	}
	if payoutsReimburseMaxFileSize < len(data) {
		fmt.Fprintf(stderr, "payouts reimburse: %s is larger than %d bytes\n", file, payoutsReimburseMaxFileSize)
		return 1
	}
	report, err := reimburse(data)
	if report != nil {
		fmt.Fprint(stdout, report.Text())
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	if !send && report != nil && report.Digest != "" {
		fmt.Fprintf(stdout, "to send exactly this: bringyourctl payouts reimburse --file=%s --reason=%s --send --expect=%s\n",
			payoutsReimburseShellQuote(file), payoutsReimburseShellQuote(reason), report.Digest)
	}
	return 0
}

// One POSIX shell word.
func payoutsReimburseShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
