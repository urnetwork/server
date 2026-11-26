package monitor

// SIGNALS.md §32.3 (CSRV-M12-06): the policy's and the tickets' pages at their lines and not before.

import (
	"testing"
)

// The policy 13 days from its not_after pages and 15 days does not; a ticket 6 days from its horizon pages and 8
// days does not.
func TestComputePolicyAndTicketPagesAtTheirHorizons(t *testing.T) {
	day := 24 * 60 * 60.0
	for _, c := range []struct {
		policy, ticket float64
		pages          bool
	}{{policy: 13 * day, ticket: 6 * day, pages: true}, {policy: 15 * day, ticket: 8 * day, pages: false}} {
		findings := computeFamilyJudge(computePolicyFamily(), computeTestSettings("policy"), []*sidecarStorageSample{
			computeTestLive(),
			computeTestSample("compute-policy-expiring", "left", c.policy, nil),
			computeTestSample("compute-ticket-horizon", "left", c.ticket, nil),
		})
		policy := computeTestFindings(findings, "compute-policy-expiring")["compute"]
		ticket := computeTestFindings(findings, "compute-ticket-horizon")["compute"]
		if policy.healthy == c.pages || ticket.healthy == c.pages {
			t.Errorf("policy %.0f d, ticket %.0f d: %+v %+v", c.policy/day, c.ticket/day, policy, ticket)
		}
	}
}
