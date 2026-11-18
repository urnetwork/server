package model

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Whom each refusal reason is evidence against (connect/GEOMAP.md §5.5), the
// one rule the per-target tally's evidence count and the derive's refusal
// rates read. A refusal of the claim -- a round trip below the one observed, a
// bad nonce, the wrong extender, a bad signature -- is evidence against both
// parties. A refusal with no reason, an acceptance whose co-signature did not
// verify, is evidence against the target only. An unknown pinger, a rate
// limit and a reason no target sends are evidence against no one. Pure.
func TestNetworkPingRefusalCountsAgainstItsParties(t *testing.T) {
	type parties struct {
		target bool
		pinger bool
	}
	reasonParties := map[int]parties{
		int(connect.ExtenderProbeVerdictReasonOk):               {target: true},
		int(connect.ExtenderProbeVerdictReasonRttBelowObserved): {target: true, pinger: true},
		int(connect.ExtenderProbeVerdictReasonNonce):            {target: true, pinger: true},
		int(connect.ExtenderProbeVerdictReasonWrongExtender):    {target: true, pinger: true},
		int(connect.ExtenderProbeVerdictReasonUnknownPinger):    {},
		int(connect.ExtenderProbeVerdictReasonBadSignature):     {target: true, pinger: true},
		int(connect.ExtenderProbeVerdictReasonRateLimited):      {},
		// reasons no target sends
		7:    {},
		77:   {},
		-1:   {},
		9001: {},
	}
	// every reason a target sends is judged here
	for _, reason := range NetworkPingRefusalReasons {
		if _, ok := reasonParties[reason]; !ok {
			t.Fatalf("reason %d is not judged", reason)
		}
	}
	for reason, want := range reasonParties {
		got := parties{
			target: NetworkPingRefusalCountsAgainstTarget(reason),
			pinger: NetworkPingRefusalCountsAgainstPinger(reason),
		}
		if got != want {
			t.Errorf("reason %d counts against %+v, want %+v", reason, got, want)
		}
	}
}
