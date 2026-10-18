package model

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Connect's fixed-IP redial (connect ip_remote_multi_client_sticky.go,
// ApiMultiClientGenerator.NextDestinationsForClientId) asks again for the exit
// it lost, by client id, in the window's rank mode. A named Quality request
// keeps the live-connection requirement, the common exclusions, live ARIN risk
// and the request's own exclusions, but not ARIN quality verification (owner
// decision 2026-10-15, FP2FIX.md "Named Quality requests").

// The two rank modes a Quality redial arrives with: explicit, and empty.
var namedRedialTestQualityModes = []RankMode{RankModeQuality, ""}

// One request as the redial sends it: one client-id spec, count 1, the window's
// rank mode and exclusions.
func namedRedialTestFind(
	ctx context.Context,
	t testing.TB,
	clientId server.Id,
	rankMode RankMode,
	excludeClientIds []server.Id,
	excludeDestinations [][]server.Id,
) []*FindProvidersProvider {
	t.Helper()
	clientSession := testingCreateProviderSearchSession(
		ctx,
		session.NewByJwt(server.NewId(), server.NewId(), "named-redial-test", false, false),
	)
	result, err := FindProviders2(&FindProviders2Args{
		Specs:               []*ProviderSpec{{ClientId: &clientId}},
		ExcludeClientIds:    excludeClientIds,
		ExcludeDestinations: excludeDestinations,
		Count:               1,
		RankMode:            rankMode,
	}, clientSession)
	if err != nil {
		t.Fatalf("rank_mode=%q: named request failed: %v", rankMode, err)
	}
	return result.Providers
}

// The selection outcomes of named Quality requests so far: answered, and
// refused (direct_excluded).
func namedRedialTestOutcomes() (answered float64, refused float64) {
	answered = testutil.ToFloat64(findProviders2SelectionOutcomes.WithLabelValues("direct", "default_minimum", "any", RankModeQuality, "nonempty", "returned_small_direct"))
	refused = testutil.ToFloat64(findProviders2SelectionOutcomes.WithLabelValues("direct", "default_minimum", "any", RankModeQuality, "zero", "direct_excluded"))
	return
}

// Asserts that a named Quality request answers the provider, in both rank
// modes, and records it as answered rather than direct_excluded.
func assertNamedRedialAnswered(ctx context.Context, t testing.TB, provider *egressTestProvider, what string, excludeClientIds []server.Id, excludeDestinations [][]server.Id) {
	t.Helper()
	for _, rankMode := range namedRedialTestQualityModes {
		answeredBefore, refusedBefore := namedRedialTestOutcomes()
		providers := namedRedialTestFind(ctx, t, provider.clientId, rankMode, excludeClientIds, excludeDestinations)
		answered, refused := namedRedialTestOutcomes()
		if !slices.Equal(egressTestIds(providers), []server.Id{provider.clientId}) {
			t.Fatalf("rank_mode=%q: named Quality request refused %s (direct_excluded +%g)", rankMode, what, refused-refusedBefore)
		}
		if answered-answeredBefore != 1 || refused != refusedBefore {
			t.Fatalf("rank_mode=%q: %s answered, but recorded answered +%g, direct_excluded +%g", rankMode, what, answered-answeredBefore, refused-refusedBefore)
		}
	}
}

// Asserts that a named Quality request refuses the provider, in both rank
// modes, as direct_excluded.
func assertNamedRedialRefused(ctx context.Context, t testing.TB, provider *egressTestProvider, what string, excludeClientIds []server.Id, excludeDestinations [][]server.Id) {
	t.Helper()
	for _, rankMode := range namedRedialTestQualityModes {
		_, refusedBefore := namedRedialTestOutcomes()
		if providers := namedRedialTestFind(ctx, t, provider.clientId, rankMode, excludeClientIds, excludeDestinations); len(providers) != 0 {
			t.Fatalf("rank_mode=%q: named Quality request answered %s", rankMode, what)
		}
		if _, refused := namedRedialTestOutcomes(); refused-refusedBefore != 1 {
			t.Fatalf("rank_mode=%q: %s refused, but recorded direct_excluded +%g", rankMode, what, refused-refusedBefore)
		}
	}
}

// Live connections that are not ARIN quality verified, the ARIN non-quality
// finding that most Speed providers carry. Discovery borrows such a provider.
func namedRedialTestNonQuality() *ConnectionLocationScores {
	return &ConnectionLocationScores{ArinQualityVerified: true, ArinNonQuality: true}
}

// The defect: a named Quality request refused a live, healthy provider whose
// connection is not ARIN-quality verified, so the redial fell back to discovery
// and the user's exit IP changed. It is answered, ARIN non-quality or
// unverified, with an explicit or an empty rank mode; a verified provider is
// answered as before.
func TestFindProviders2NamedQualityAnswersLiveNonQualityProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Redial City", "Redial Region", "United States", "us")
		verified := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		nonQuality := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		unverified := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		for _, provider := range []*egressTestProvider{verified, nonQuality, unverified} {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		egressTestPasses(ctx, t)
		// Premise: the native Quality guard refuses exactly the two, and they
		// pass every common exclusion.
		for _, provider := range []*egressTestProvider{verified, nonQuality, unverified} {
			if refused := subscriberQualityTestRefused(ctx, t, provider.clientId); refused == (provider == verified) {
				t.Fatalf("fixture: native Quality guard refused=%t for verified=%t", refused, provider == verified)
			}
		}
		common, err := getProviderHardExclusions(ctx, []server.Id{verified.clientId, nonQuality.clientId, unverified.clientId})
		if err != nil || len(common) != 0 {
			t.Fatalf("fixture: common exclusions %v error=%v", common, err)
		}

		assertNamedRedialAnswered(ctx, t, verified, "a live, healthy, verified provider", nil, nil)
		assertNamedRedialAnswered(ctx, t, nonQuality, "a live, healthy, ARIN non-quality provider", nil, nil)
		assertNamedRedialAnswered(ctx, t, unverified, "a live, healthy, unverified provider", nil, nil)
	})
}

// The live-connection requirement holds: a provider whose connection closed,
// or whose connection's handler stopped heartbeating, is refused, verified or
// not. A refusal is not cached: the provider is answered once it reconnects,
// before its new connection is classified, and refused again when that
// classification finds ARIN risk. With subscriber policy two off, a named
// request has no live requirement, as before.
func TestFindProviders2NamedQualityStillNeedsLiveConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Live City", "Live Region", "United States", "us")
		closed := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		closedVerified := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		stale := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		providers := []*egressTestProvider{closed, closedVerified, stale}
		for _, provider := range providers {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		egressTestPasses(ctx, t)
		for _, provider := range providers {
			assertNamedRedialAnswered(ctx, t, provider, "a live provider", nil, nil)
		}

		for _, provider := range []*egressTestProvider{closed, closedVerified} {
			if err := DisconnectNetworkClient(ctx, provider.connectionId); err != nil {
				t.Fatal(err)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_handler SET heartbeat_time = $2
				WHERE handler_id = (SELECT handler_id FROM network_client_connection WHERE connection_id = $1)`,
				stale.connectionId, server.NowUtc().Add(-3*NetworkClientHandlerHeartbeatTimeout)))
		})
		assertNamedRedialRefused(ctx, t, closed, "a provider with its only connection closed", nil, nil)
		assertNamedRedialRefused(ctx, t, closedVerified, "a verified provider with its only connection closed", nil, nil)
		assertNamedRedialRefused(ctx, t, stale, "a provider whose handler stopped heartbeating", nil, nil)

		// Reconnects at once, before the announce classifies the connection:
		// unknown is not risk, as the common risk gate reads it.
		handler := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, closed.clientId, "192.0.2.241:0", handler)
		if err != nil {
			t.Fatal(err)
		}
		assertNamedRedialAnswered(ctx, t, closed, "a provider that reconnected", nil, nil)
		if err := SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{ArinRisk: true}); err != nil {
			t.Fatal(err)
		}
		assertNamedRedialRefused(ctx, t, closed, "a provider that reconnected with ARIN risk", nil, nil)

		t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("subscriber_quality_policy_version: 0\n")))
		assertNamedRedialAnswered(ctx, t, closedVerified, "a closed provider under the legacy policy", nil, nil)
	})
}

// The common exclusions hold, for providers a named Quality request would
// otherwise answer (live, healthy, ARIN non-quality): reliability under each
// lookback's floor, ARIN risk in the published rollup, ARIN risk found on the
// live connection after that publication, and a TLS finding.
func TestFindProviders2NamedQualityKeepsCommonExclusions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Gate City", "Gate Region", "United States", "us")
		control := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		floors := providerReliabilityMinimums()
		unreliable := map[int]*egressTestProvider{}
		for lookbackIndex := 1; lookbackIndex <= 3; lookbackIndex++ {
			unreliable[lookbackIndex] = egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		}
		risky := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinNonQuality: true, ArinRisk: true})
		lateRisk := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		tls := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		providers := []*egressTestProvider{control, risky, lateRisk, tls}
		for _, provider := range unreliable {
			providers = append(providers, provider)
		}
		for _, provider := range providers {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		for lookbackIndex, provider := range unreliable {
			egressTestReliability(ctx, provider.clientId, lookbackIndex, floors[lookbackIndex]-0.05, 1)
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:                 tls.clientId,
			MeasuredAt:               server.NowUtc(),
			OKCount:                  5,
			Total:                    5,
			ClassResults:             map[string]ProviderEgressHealthClassResult{"site": {OK: 5, Total: 5}},
			TLSAuthenticationFailure: true,
		})
		egressTestPasses(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_risk = true WHERE connection_id = $1`, lateRisk.connectionId))
		})
		common, err := getProviderHardExclusions(ctx, []server.Id{control.clientId, lateRisk.clientId})
		if err != nil || common[control.clientId] || common[lateRisk.clientId] {
			t.Fatalf("fixture: the published common exclusions must not hold the late risk: %v error=%v", common, err)
		}

		for lookbackIndex, provider := range unreliable {
			assertNamedRedialRefused(ctx, t, provider, fmt.Sprintf("a provider under the reliability floor of lookback %d", lookbackIndex), nil, nil)
		}
		assertNamedRedialRefused(ctx, t, risky, "a provider with published ARIN risk", nil, nil)
		assertNamedRedialRefused(ctx, t, lateRisk, "a provider with ARIN risk on its live connection", nil, nil)
		assertNamedRedialRefused(ctx, t, tls, "a provider with a TLS finding", nil, nil)
		// Not the non-quality finding: the same provider without a gate.
		assertNamedRedialAnswered(ctx, t, control, "a live, healthy, ARIN non-quality provider", nil, nil)
	})
}

// The request's own exclusions hold: its excluded client ids, and its excluded
// destinations ending at the provider, whatever hops precede it.
func TestFindProviders2NamedQualityKeepsRequestExclusions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Exclusion City", "Exclusion Region", "United States", "us")
		provider := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		egressTestPasses(ctx, t)
		other, intermediary := server.NewId(), server.NewId()

		assertNamedRedialRefused(ctx, t, provider, "an excluded client id", []server.Id{other, provider.clientId}, nil)
		assertNamedRedialRefused(ctx, t, provider, "an excluded destination", nil, [][]server.Id{{provider.clientId}})
		assertNamedRedialRefused(ctx, t, provider, "an excluded multi-hop destination", nil, [][]server.Id{{intermediary, provider.clientId}})
		// Exclusions of other providers, or of a destination it only relays.
		assertNamedRedialAnswered(ctx, t, provider, "a provider the request does not exclude", []server.Id{other}, [][]server.Id{{provider.clientId, other}})
	})
}

// Discovery is unchanged: a location or best-available Quality request answers
// native Quality first and borrows the live non-quality providers only at the
// lower Speed tier, force_minimum Quality leaves them out, and a named spec
// inside a discovery request keeps the native Quality policy.
func TestFindProviders2NamedQualityLeavesDiscoveryUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Discovery City", "Discovery Region", "United States", "us")
		elsewhere := egressTestCity(ctx, "Elsewhere City", "Elsewhere Region", "Elsewhere Country", "zz")
		resetCountryCodeLocationIds()
		verified := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		nonQuality := egressTestConnect(ctx, t, city, egressTestFast, nil, namedRedialTestNonQuality())
		unverified := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		remote := egressTestConnect(ctx, t, elsewhere, egressTestFast, nil, namedRedialTestNonQuality())
		for _, provider := range []*egressTestProvider{verified, nonQuality, unverified, remote} {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		egressTestPasses(ctx, t)

		for _, specs := range [][]*ProviderSpec{egressTestLocationSpec(city), {{BestAvailable: true}}} {
			providers := egressTestFind(ctx, t, specs, RankModeQuality, 10, false, server.NewId())
			if len(providers) != 3 || providers[0].ClientId != verified.clientId || providers[0].Tier != 0 {
				t.Fatalf("Quality discovery lost native priority: %v", egressTestIds(providers))
			}
			for _, provider := range providers[1:] {
				if provider.ClientId == verified.clientId || provider.Tier < egressTestBackfillOffset() {
					t.Fatal("non-quality access entered the native Quality tier")
				}
			}
			forced := egressTestFind(ctx, t, specs, RankModeQuality, 10, true, server.NewId())
			if !slices.Equal(egressTestIds(forced), []server.Id{verified.clientId}) {
				t.Fatalf("force_minimum Quality returned non-quality access: %v", egressTestIds(forced))
			}
		}
		mixed := []*ProviderSpec{{LocationId: &city.LocationId}, {ClientId: &remote.clientId}}
		for _, forceMinimum := range []bool{false, true} {
			providers := egressTestFind(ctx, t, mixed, RankModeQuality, 10, forceMinimum, server.NewId())
			if len(providers) == 0 || slices.Contains(egressTestIds(providers), remote.clientId) {
				t.Fatalf("force=%t: a named spec in a discovery request skipped the native Quality policy: %v", forceMinimum, egressTestIds(providers))
			}
		}
	})
}
