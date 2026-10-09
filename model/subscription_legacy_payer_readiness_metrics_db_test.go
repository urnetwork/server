// Entry controls bind the counters to actual payer and dispatcher calls, not
// a standalone projection of a manually constructed readiness observation.
package model

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestLegacyPayerReadinessMetricCountsActualEntries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture, id := legacySettlementTestIntent(t, ctx)
		readyCount := func(caller string) float64 {
			return testutil.ToFloat64(legacyPayerReadinessCounter.WithLabelValues(caller, "ready", "false")) +
				testutil.ToFloat64(legacyPayerReadinessCounter.WithLabelValues(caller, "ready", "true"))
		}
		payerBefore, dispatcherBefore := readyCount("payer"), readyCount("dispatcher")
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		result, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: fixture.sourceNetworkId}, owner)
		if err != nil || result.Completed != 1 || readyCount("payer") != payerBefore+1 || readyCount("dispatcher") != dispatcherBefore {
			t.Fatal("actual payer entry did not record exactly its existing readiness observation", result, err)
		}
		_, readiness, err := DispatchLegacySettlementPayersWithReadiness(ctx, int(id[15])%LegacySettlementShardCount, nil, nil)
		if err != nil || readiness == nil || readiness.Outcome != "ready" || readyCount("dispatcher") != dispatcherBefore+1 || readyCount("payer") != payerBefore+1 {
			t.Fatal("actual dispatcher entry changed readiness attribution", readiness, err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
	})
}
