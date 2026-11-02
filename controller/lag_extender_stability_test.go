package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tests of the restart loop rule of the extender sample (lag.dev/PLAN.md A4):
// an extender in a restart loop is not offered, and the connect behaviours that
// post activations without a restart, or a failed uptime probe, leave an
// extender offered.

// One activation row per family of `ipVersions` at each of `ago`, the
// families of one pass 2 seconds apart, as an activation pass writes them.
func addLagTestPasses(ctx context.Context, extender *lagTestExtender, now time.Time, ipVersions []int, ago ...time.Duration) {
	for _, a := range ago {
		for i, ipVersion := range ipVersions {
			model.Testing_AddNetworkExtenderActivation(ctx, extender.extenderId, ipVersion, now.Add(-a).Add(time.Duration(i)*2*time.Second))
		}
	}
}

// The extender ids of a sample, sorted.
func lagTestSampleIds(result *LagExtendersResult) []server.Id {
	ids := []server.Id{}
	for _, extender := range result.Extenders {
		ids = append(ids, extender.ExtenderId)
	}
	slices.SortFunc(ids, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return ids
}

func lagTestSortedIds(extenders ...*lagTestExtender) []server.Id {
	ids := []server.Id{}
	for _, extender := range extenders {
		ids = append(ids, extender.extenderId)
	}
	slices.SortFunc(ids, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return ids
}

// A restart loop keeps an extender out of the sample and out of the tickets'
// set. A healthy extender stays in, and so do the activations that are not
// restarts: one family re-posted on its backoff while the other is refused, on
// an IPv6-only or a dual-stack extender, full passes an hour apart from
// connect's address check, and re-activations after the probes revoked the
// extender. A failed latest uptime probe alone leaves an extender in.
func TestLagExtendersLeaveOutRestartLoops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc()
		minutes := func(m float64) time.Duration {
			return time.Duration(m * float64(time.Minute))
		}

		// restarted three times in an hour
		looping := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.31", "2001:db8:31::1")
		addLagTestPasses(ctx, looping, now, []int{4, 6}, minutes(80), minutes(50), minutes(20))

		// its daily pass
		healthy := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.32", "2001:db8:32::1")
		addLagTestPasses(ctx, healthy, now, []int{4, 6}, minutes(180))

		// IPv4 refused since a start 75 minutes ago: IPv6 is re-posted 10,
		// 20 and 40 minutes apart, and the extender has only that address
		backoffIpv6Only := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hong Kong", "hk", 22.3, 114.2), "2001:db8:33::1")
		addLagTestPasses(ctx, backoffIpv6Only, now, []int{6}, minutes(75), minutes(65), minutes(45), minutes(5))

		// the same on an extender with an address of each family
		backoffDualStack := newLagServingTestExtender(t, ctx, places, lagTestPlace("Tokyo", "jp", 35.7, 139.7), "192.0.2.34", "2001:db8:34::1")
		addLagTestPasses(ctx, backoffDualStack, now, []int{6}, minutes(75), minutes(65), minutes(45), minutes(5))

		// a full pass at each hourly address check, a little under an hour
		// apart, when the caller address flips between families
		hourly := newLagServingTestExtender(t, ctx, places, lagTestPlace("Berlin", "de", 52.5, 13.4), "192.0.2.35", "2001:db8:35::1")
		addLagTestPasses(ctx, hourly, now, []int{4, 6}, minutes(119.5), minutes(59.75), minutes(0.5))

		// re-activated 10 minutes after each of three revocations by its
		// probes
		reactivated := newLagServingTestExtender(t, ctx, places, lagTestPlace("Riga", "lv", 56.9, 24.1), "192.0.2.37", "2001:db8:37::1")
		for _, ago := range []time.Duration{minutes(80), minutes(50), minutes(20)} {
			model.Testing_AddNetworkExtenderRevocation(ctx, reactivated.extenderId, now.Add(-ago-minutes(10)))
			addLagTestPasses(ctx, reactivated, now, []int{4, 6}, ago)
		}

		// the latest probes of both its addresses failed: an uptime probe
		// forwards through the extender, which a measurement does not use
		probeFailed := newLagServingTestExtender(t, ctx, places, lagTestPlace("Kuala Lumpur", "my", 3.1, 101.7), "192.0.2.36", "2001:db8:36::1")
		addLagTestPasses(ctx, probeFailed, now, []int{4, 6}, minutes(240))
		model.RecordNetworkExtenderProbeResult(ctx, probeFailed.extenderId, 4, true, now.Add(-minutes(7)), 6, nil)
		model.RecordNetworkExtenderProbeResult(ctx, probeFailed.extenderId, 4, false, now.Add(-minutes(2)), 6, nil)
		model.RecordNetworkExtenderProbeResult(ctx, probeFailed.extenderId, 6, false, now.Add(-minutes(2)), 6, nil)
		Testing_ResetLagServing()

		result, err := LagExtenders(newLagTestGuestSession(t, ctx, "198.51.100.30:1234"))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), lagTestSortedIds(healthy, backoffIpv6Only, backoffDualStack, hourly, reactivated, probeFailed))

		// the tickets (A6) read the same set
		set := getLagServing(ctx, server.NowUtc(), DefaultLagSettings().ExtenderSampleCache)
		connect.AssertEqual(t, set.get(looping.extenderId), (*lagServingExtender)(nil))
		connect.AssertEqual(t, set.get(probeFailed.extenderId) != nil, true)
		connect.AssertEqual(t, set.get(healthy.extenderId) != nil, true)
	})
}

// When the restart loops cannot be read, every serving extender is offered,
// as before the rule.
func TestLagExtendersOfferEveryExtenderWhenRestartLoopsAreUnreadable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc()

		looping := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.41", "2001:db8:41::1")
		addLagTestPasses(ctx, looping, now, []int{4, 6}, 80*time.Minute, 50*time.Minute, 20*time.Minute)
		healthy := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.42")

		previous := lagReadRestartLoops
		lagReadRestartLoops = func(
			ctx context.Context,
			extenderIds []server.Id,
			now time.Time,
			rule *model.LagExtenderRestartLoopRule,
		) map[server.Id]time.Time {
			server.Raise(errors.New("the restart loop query failed"))
			return nil
		}
		t.Cleanup(func() {
			lagReadRestartLoops = previous
		})
		Testing_ResetLagServing()

		result, err := LagExtenders(newLagTestGuestSession(t, ctx, "198.51.100.30:1234"))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), lagTestSortedIds(looping, healthy))

		// with the read restored, the loop is left out
		lagReadRestartLoops = previous
		Testing_ResetLagServing()
		result, err = LagExtenders(newLagTestGuestSession(t, ctx, "198.51.100.30:1234"))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), lagTestSortedIds(healthy))
	})
}
