package controller

// Owner classes distinguish probe return paths without exposing endpoint IDs or
// trusting a sender-supplied field. Both diagnostic families use the same bounds.

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestContractFailureDetailsSeparateDestinationOwner(t *testing.T) {
	for _, test := range []struct {
		owner model.NetworkClientSourceOwner
		label string
	}{
		{owner: model.NetworkClientSourceOwnerEgressProber, label: "egress_prober"},
		{owner: model.NetworkClientSourceOwnerOther, label: "other"},
		{owner: model.NetworkClientSourceOwnerUnknown, label: "unknown"},
		{owner: model.NetworkClientSourceOwner("synthetic-unbounded-owner"), label: "unknown"},
	} {
		for _, family := range []struct {
			counter *prometheus.CounterVec
			err     error
		}{
			{counter: missingOriginDetailsCounter, err: model.ErrMissingCompanionOrigin},
			{counter: inactiveDestinationDetailsCounter, err: errContractDestinationInactive},
		} {
			counter := family.counter.WithLabelValues("false", "absent", "other", test.label, "stream_fallback", "public", "active_top", "active_derived")
			before := testutil.ToFloat64(counter)
			recordContractFailureResolved(server.NewId(), server.NewId(), false, 1024, family.err, contractResolution{
				path:                 contractResolutionStreamFallback,
				relationship:         model.ProvideModePublic,
				sourceLifecycle:      model.NetworkClientLifecycleActiveTop,
				destinationLifecycle: model.NetworkClientLifecycleActiveDerived,
				sourceOwner:          model.NetworkClientSourceOwnerOther,
				destinationOwner:     test.owner,
			})
			if after := testutil.ToFloat64(counter); after != before+1 {
				t.Fatalf("destination owner %q: counter=%v, want %v", test.owner, after, before+1)
			}
		}
	}
}
