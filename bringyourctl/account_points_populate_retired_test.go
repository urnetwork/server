// bringyourctl account-points populate --plan_id is retired (owner, 2026-11-13:
// "yes retire it"). It gave points in proportion to a completed USDC payout
// plan's payout, so its points followed revenue; USDC payout plans are no
// longer created, and a rerun would award a plan's points twice. Operator block
// points (the daily task and account-points backfill-blocks) award by bytes
// served.
package main

import (
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
)

// The usage refuses account-points populate in every form, so main never
// dispatches it; account-points backfill-blocks still parses.
func TestAccountPointsPopulateIsRetired(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	planId := server.NewId().String()
	for _, args := range [][]string{
		{"account-points", "populate", "--plan_id=" + planId},
		{"account-points", "populate", "--plan_id", planId},
		{"account-points", "populate"},
	} {
		if _, err := parser.ParseArgs(bringyourctlUsage, args, "synthetic"); err == nil {
			t.Errorf("bringyourctl %s parsed", strings.Join(args, " "))
		}
	}
	if _, err := parser.ParseArgs(bringyourctlUsage, []string{"account-points", "backfill-blocks"}, "synthetic"); err != nil {
		t.Fatal("account-points backfill-blocks does not parse:", err)
	}
}
