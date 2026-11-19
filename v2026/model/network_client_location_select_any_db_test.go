package model

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// A request for protocol any (owner, 2026-11-13: "hosted proxies should use
// main window as the limited window, so that dualproto and dualstack
// requirements are not applied on the main window for hosted proxies, and
// there is no need for a separate limited window") draws every class as one
// pool ranked by its rank mode. Its tcp providers come at their own tier with
// no overflow step, whatever the target or the held count, where the same
// supply asked for dualproto gives them only as overflow; ip ipv4 has no IPv6
// need; the echo and the request counter carry any; and the family filter
// still narrows the pool.
func TestFindProviders2SelectAnyAnswersEveryClassInOnePool(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newCapabilityTestFixture(ctx, t, "AnyProtocol")
		overflowTier := findProviders2OverflowBand * egressTestBackfillOffset()
		dualstackDualproto := capabilityTestAnswer{ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolDualproto}
		ipv4Dualproto := capabilityTestAnswer{ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolDualproto}
		dualstackTcp := capabilityTestAnswer{ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolTcp}
		ipv4Tcp := capabilityTestAnswer{ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolTcp}
		everyClass := map[server.Id]capabilityTestAnswer{
			fixture.a:              dualstackDualproto,
			fixture.b:              ipv4Dualproto,
			fixture.bFromDualstack: ipv4Dualproto,
			fixture.c:              dualstackTcp,
			fixture.d:              ipv4Tcp,
		}

		offlineTestForEachReader(t, func(nativeReader bool) {
			label := fmt.Sprintf("reader=%t", nativeReader)

			// the hosted windows' request, with a target the dualproto
			// providers reach and one they do not, and with a full window
			for _, counts := range [][2]int{{2, 0}, {10, 0}, {5, 5}} {
				requests := testutil.ToFloat64(findProviders2SelectRequests.WithLabelValues(RankModeQuality, ProviderCapabilityIpIpv4, ProviderCapabilityProtocolAny))
				overflowRequests := testutil.ToFloat64(findProviders2SelectOverflow.WithLabelValues(RankModeQuality))
				result := fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("ipv4", "any", counts[0], counts[1], 0, 0)})
				if got := capabilityTestAnswers(t, result.Providers); !capabilityTestAnswersEqual(got, everyClass) {
					t.Fatalf("%s: ipv4 and any with target %d and %d held answered %v, want every class at its own tier", label, counts[0], counts[1], got)
				}
				connect.AssertEqual(t, *result.Select.Ip, ProviderCapabilityIpIpv4)
				connect.AssertEqual(t, *result.Select.Protocol, ProviderCapabilityProtocolAny)
				connect.AssertEqual(t, *result.Select.TargetCount, counts[0])
				connect.AssertEqual(t, *result.Select.HeldCount, counts[1])
				connect.AssertEqual(t, *result.Select.DualstackTargetCount, 0)
				connect.AssertEqual(t, testutil.ToFloat64(findProviders2SelectRequests.WithLabelValues(RankModeQuality, ProviderCapabilityIpIpv4, ProviderCapabilityProtocolAny)), requests+1)
				connect.AssertEqual(t, testutil.ToFloat64(findProviders2SelectOverflow.WithLabelValues(RankModeQuality)), overflowRequests)
			}

			// the same supply asked for dualproto, as before: tcp providers only
			// when the target is out of reach, and then at the overflow band
			result := fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("ipv4", "dualproto", 2, 0, 0, 0)})
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.a:              dualstackDualproto,
				fixture.b:              ipv4Dualproto,
				fixture.bFromDualstack: ipv4Dualproto,
			})
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("ipv4", "dualproto", 10, 0, 0, 0)})
			overflowAnswers := capabilityTestAnswers(t, result.Providers)
			connect.AssertEqual(t, overflowAnswers[fixture.c], capabilityTestAnswer{tier: overflowTier, ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolTcp, overflowStep: 1})
			connect.AssertEqual(t, overflowAnswers[fixture.d], capabilityTestAnswer{tier: overflowTier, ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolTcp, overflowStep: 1})

			// dualstack and any: the IPv6 need from A and C as one pool, first,
			// then the rest, none of it overflow
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("dualstack", "any", 5, 0, 1, 0)})
			if got := capabilityTestAnswers(t, result.Providers); !capabilityTestAnswersEqual(got, everyClass) {
				t.Fatalf("%s: dualstack and any answered %v, want every class at its own tier", label, got)
			}
			connect.AssertEqual(t, result.Providers[0].Capabilities.Ip, ProviderCapabilityIpDualstack)

			// the family filter narrows the one pool to the ipv4 classes
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, IpFamily: "v4-only", Select: capabilityTestSelect("ipv4", "any", 5, 0, 0, 0)})
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.b:              ipv4Dualproto,
				fixture.bFromDualstack: ipv4Dualproto,
				fixture.d:              ipv4Tcp,
			})
		})

		// online: the pool's Online members, the tcp ones without overflow
		result := fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeOnline, Select: capabilityTestSelect("ipv4", "any", 10, 0, 0, 0)})
		answers := capabilityTestAnswers(t, result.Providers)
		connect.AssertEqual(t, len(answers), 5)
		for clientId, answer := range answers {
			if answer.overflowStep != 0 || overflowTier <= answer.tier {
				t.Errorf("online any answered %s with overflow step %d at tier %d", clientId, answer.overflowStep, answer.tier)
			}
		}

		// force_minimum: today's forced pool, every class
		result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeSpeed, ForceMinimum: true, Select: capabilityTestSelect("ipv4", "any", 5, 0, 0, 0)})
		answers = capabilityTestAnswers(t, result.Providers)
		connect.AssertEqual(t, len(answers), 5)
		for clientId, answer := range answers {
			if answer.overflowStep != 0 {
				t.Errorf("forced any answered %s with overflow step %d", clientId, answer.overflowStep)
			}
		}
	})
}

// Whether two answers hold the same providers with the same labels, tier and
// overflow step.
func capabilityTestAnswersEqual(got map[server.Id]capabilityTestAnswer, want map[server.Id]capabilityTestAnswer) bool {
	if len(got) != len(want) {
		return false
	}
	for clientId, answer := range want {
		if gotAnswer, ok := got[clientId]; !ok || gotAnswer != answer {
			return false
		}
	}
	return true
}
