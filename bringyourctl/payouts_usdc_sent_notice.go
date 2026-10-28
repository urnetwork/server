// `payouts usdc-sent-notice` (controller/account_payment_usdc_sent_notice.go):
// emails each listed payment's network admin that its USDC payout was sent and
// that USDC payouts are ending. The dry run reads only; --send sends each due
// notice once.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

// at most this many payments in one run
const payoutsUsdcSentNoticeMaxPayments = 1024

func payoutsUsdcSentNoticeCommand(opts docopt.Opts) int {
	file, _ := opts.String("--file")
	send, _ := opts.Bool("--send")
	// The dry run writes nothing: its process is read only.
	if !send {
		server.RequirePgReadOnly()
	}
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	return runPayoutsUsdcSentNotice(os.Stdout, os.Stderr, file, os.ReadFile, func(paymentIds []server.Id) (*controller.PayoutUsdcSentNoticeReport, error) {
		return controller.PayoutsUsdcSentNotice(clientSession, paymentIds, send)
	})
}

// Prints the report and returns the exit code: non-zero when the file is
// unreadable or the notice fails.
func runPayoutsUsdcSentNotice(
	stdout io.Writer,
	stderr io.Writer,
	file string,
	readFile func(string) ([]byte, error),
	notice func([]server.Id) (*controller.PayoutUsdcSentNoticeReport, error),
) int {
	data, err := readFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "payouts usdc-sent-notice: reading %s: %s\n", file, err)
		return 1
	}
	paymentIds, err := parsePayoutsUsdcSentNoticeIds(data)
	if err != nil {
		fmt.Fprintf(stderr, "payouts usdc-sent-notice: %s\n", err)
		return 1
	}
	report, err := notice(paymentIds)
	if err != nil {
		fmt.Fprintf(stderr, "payouts usdc-sent-notice: %s\n", err)
		return 1
	}
	fmt.Fprint(stdout, report.Text())
	return 0
}

// One payment id per line; blank lines and # comments are skipped. A
// malformed or repeated id refuses the whole file.
func parsePayoutsUsdcSentNoticeIds(data []byte) ([]server.Id, error) {
	paymentIds := []server.Id{}
	seen := map[server.Id]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		paymentId, err := server.ParseId(line)
		if err != nil {
			return nil, fmt.Errorf("line %d is not a payment id", i+1)
		}
		if seen[paymentId] {
			return nil, fmt.Errorf("line %d repeats a payment id", i+1)
		}
		seen[paymentId] = true
		paymentIds = append(paymentIds, paymentId)
	}
	if len(paymentIds) == 0 {
		return nil, fmt.Errorf("no payment ids")
	}
	if payoutsUsdcSentNoticeMaxPayments < len(paymentIds) {
		return nil, fmt.Errorf("more than %d payment ids", payoutsUsdcSentNoticeMaxPayments)
	}
	return paymentIds, nil
}
