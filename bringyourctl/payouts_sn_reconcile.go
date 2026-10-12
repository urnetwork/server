// The one-time legacy USDC reconciliation (SN-RECONCILE.md): a census and
// report, and with --send the revival of the payable rows for the sender.
package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

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
