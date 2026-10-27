// The one-time legacy USDC reconciliation (SN-RECONCILE.md): a census and
// report, and with --send the revival of the payable rows for the sender.
package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

// SN-RECONCILE.md §2 (RC-18): the dry run writes nothing. Its process is read
// only, so PostgreSQL refuses any write on the census path. --send writes the
// revivals.
func snReconcileRequireReadOnly(send bool) {
	if !send {
		server.RequirePgReadOnly()
	}
}

// Prints the report, as text or as one JSON document, and returns the exit
// code. A refusal prints its report too, then its error, and exits non-zero.
func runSnReconcile(stdout io.Writer, stderr io.Writer, send bool, expect string, asJson bool, reconcile func(bool, string) (*controller.SnReconcileReport, error)) int {
	report, err := reconcile(send, expect)
	if report != nil {
		if asJson {
			data, jsonErr := json.MarshalIndent(report, "", "  ")
			server.Raise(jsonErr)
			fmt.Fprintf(stdout, "%s\n", data)
		} else {
			fmt.Fprint(stdout, report.Text())
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	return 0
}
