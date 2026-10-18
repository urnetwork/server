// The owner-facing projection uses observed vault carry, without inferring a
// missed root from an entitlement that remains funded.
package main

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
)

// Read the projection through the actual shared-summary boundary.
func stPayoutCarryProjectionLine(t testing.TB, result *controller.StPayoutDryRun) string {
	t.Helper()
	var out bytes.Buffer
	writeStPayoutDryRunSummary(&out, result)
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if strings.HasPrefix(line, "projected epoch total ") {
			return line
		}
	}
	t.Fatal("the summary omitted the projected total", out.String())
	return ""
}

// Funded is also the status of a committed root awaiting finalization and of
// a still-open commit window. The funding read cannot establish a missed root.
func TestStPayoutDryRunSummaryDoesNotAssumePreviousRootMissed(t *testing.T) {
	for _, c := range []struct {
		status uint8
		total  int64
		note   string
	}{
		{status: 0, total: 1_700, note: "accruing stake if the close is timely"},
		{status: 1, total: 1_300, note: "captured funding"},
	} {
		result, _ := newStPayoutDryRunSummaryFixture(t)
		result.Epoch = 2
		result.Preview = true
		result.Funding.Status = c.status
		result.Funding.FundedRao = big.NewInt(800)
		result.PreviousFunding.FundedRao = big.NewInt(7_300)
		want := fmt.Sprintf("projected epoch total %d rao (%s + observed carry)", c.total, c.note)
		if got := stPayoutCarryProjectionLine(t, result); got != want {
			t.Errorf("status %d: summary assumed the previous funded entitlement was missed: got %q, want %q", c.status, got, want)
		}
		if result.Funding.FundedRao.Int64() != 800 || result.Funding.CarryRao.Int64() != 500 || result.PreviousFunding.FundedRao.Int64() != 7_300 {
			t.Fatal("the projection mutated the observed funding")
		}
	}
}

// Once a missed or expired entitlement is reflected in carry, its funding is
// included exactly once, through the current vault observation.
func TestStPayoutDryRunSummaryCountsObservedCarryOnce(t *testing.T) {
	for _, status := range []uint8{3, 4} {
		result, _ := newStPayoutDryRunSummaryFixture(t)
		result.Epoch = 2
		result.Preview = true
		result.PreviousFunding.Status = status
		result.PreviousFunding.FundedRao = big.NewInt(7_300)
		result.PreviousFunding.TotalRao = big.NewInt(7_300)
		result.Funding.CarryRao = big.NewInt(7_800)
		wantPrefix := "projected epoch total 9000 rao ("
		if got := stPayoutCarryProjectionLine(t, result); !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("previous status %d: observed carry was lost or counted twice: got %q, want prefix %q", status, got, wantPrefix)
		}
	}
}
