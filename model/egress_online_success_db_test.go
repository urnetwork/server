package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The URL-success rule against the database: the passes in the order the
// taskworker runs them, the published samples and counts, FindProviders2, the
// dashboard counts and the provider status. These tests use only names that
// predate the rule, so the same file runs against the code before it.

// Fails unless every provider in want is in the answer and none in unwanted is.
func assertOnlineSuccessTestAnswer(t testing.TB, where string, providers []*FindProvidersProvider, want []*egressTestProvider, unwanted []*egressTestProvider) {
	t.Helper()
	answeredClientIds := egressTestIds(providers)
	for _, provider := range want {
		if !slices.Contains(answeredClientIds, provider.clientId) {
			t.Errorf("%s: a provider the rule keeps is missing from the answer", where)
		}
	}
	for _, provider := range unwanted {
		if slices.Contains(answeredClientIds, provider.clientId) {
			t.Errorf("%s: a provider measured without one URL success answered", where)
		}
	}
}

// A provider measured repeatedly without one success, in a country where the
// probe works, is in no non-forced sample, no Online, Quality or Speed answer
// and no count. Unprobed and warming providers stay online. The rule is no
// hard exclusion: force_minimum and an explicit client id still reach the
// provider, and its URL probes continue.
func TestOnlineUrlSuccessRuleLeavesEveryBucketAndCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		// two of the four measured providers in us have a success, so the
		// probe works there
		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		healthy2 := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy2, city, 0, "us")
		dead := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, dead.clientId, server.NowUtc(), 5, 5)
		warming := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, warming.clientId, server.NowUtc(), 1, 1)
		unprobed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestPasses(ctx, t)

		kept := []*egressTestProvider{healthy, healthy2, warming, unprobed}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			clientScores := egressTestCachedScores(ctx, t, city, rankMode, false)
			if _, ok := clientScores[dead.clientId]; ok {
				t.Errorf("%s: the dead provider is in the non-forced sample", rankMode)
			}
			for name, provider := range map[string]*egressTestProvider{"warming": warming, "unprobed": unprobed} {
				if score, ok := clientScores[provider.clientId]; !ok || !score.Online {
					t.Errorf("%s: the %s provider is not online", rankMode, name)
				}
			}
			assertOnlineSuccessTestAnswer(t, string(rankMode), egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, false, server.NewId()),
				kept, []*egressTestProvider{dead})
		}
		assertOnlineSuccessTestAnswer(t, RankModeOnline, egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			kept, []*egressTestProvider{dead})
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 4)

		counts := CountProviderEgress(ctx)
		connect.AssertEqual(t, counts.ReasonCounts["no_success"], int64(1))
		onlineCount := int64(0)
		for _, count := range counts.BucketIndexCounts[ProviderEgressBucketOnline] {
			onlineCount += count
		}
		connect.AssertEqual(t, onlineCount, int64(4))

		// force_minimum and the explicit client id keep only the common gates
		forced := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, 10, true, server.NewId())
		if !slices.Contains(egressTestIds(forced), dead.clientId) {
			t.Error("force_minimum lost the provider the rule excludes")
		}
		named := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &dead.clientId}}, RankModeSpeed, 1, false, server.NewId())
		connect.AssertEqual(t, egressTestIds(named), []server.Id{dead.clientId})
		// and the probe still claims it, the only way it can come back
		due := ClaimProviderUrlProbeDue(ctx, server.NowUtc(), 100, 0, 1)
		if !slices.ContainsFunc(due, func(provider ProviderUrlProbeDue) bool { return provider.ClientId == dead.clientId }) {
			t.Error("the provider the rule excludes is no longer probed")
		}
	})
}

// The first success brings the provider back on the next pass, to Online and
// the counts, however many failures it follows.
func TestOnlineUrlSuccessRuleOneSuccessRestoresTheProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		recovering := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, recovering.clientId, server.NowUtc().Add(-time.Minute), 10, 10)
		egressTestPasses(ctx, t)
		assertOnlineSuccessTestAnswer(t, "before", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			[]*egressTestProvider{healthy}, []*egressTestProvider{recovering})
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 1)

		egressTestHealth(ctx, recovering.clientId, server.NowUtc(), 1, 0)
		egressTestPasses(ctx, t)
		assertOnlineSuccessTestAnswer(t, "after", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			[]*egressTestProvider{healthy, recovering}, nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 2)
		// one success in eleven is still a failing ratio: online, not native
		score, ok := egressTestCachedScores(ctx, t, city, RankModeQuality, false)[recovering.clientId]
		if !ok || !score.Online || score.PassesMinimums[RankModeQuality] {
			t.Fatal("the recovered provider is not online-only")
		}
	})
}

// The country guard keeps the dead providers of a country where the probe
// fails for most providers, while a country where the probe works loses its
// own. The kill switch returns every dead provider to Online.
func TestOnlineUrlSuccessCountryGuardAndKillSwitch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usCity := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		vnCity := egressTestCity(ctx, "Hanoi", "Hanoi", "Vietnam", "vn")

		usHealthy := egressTestConnect(ctx, t, usCity, egressTestFast, nil, nil)
		egressTestProbed(ctx, usHealthy, usCity, 0, "us")
		usDead := egressTestConnect(ctx, t, usCity, egressTestFast, nil, nil)
		egressTestHealth(ctx, usDead.clientId, server.NowUtc(), 5, 5)
		// one of five measured providers in vn carries traffic
		vnWorking := egressTestConnect(ctx, t, vnCity, egressTestFast, nil, nil)
		egressTestHealth(ctx, vnWorking.clientId, server.NowUtc(), 5, 4)
		vnDead := []*egressTestProvider{}
		for range 4 {
			provider := egressTestConnect(ctx, t, vnCity, egressTestFast, nil, nil)
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 5)
			vnDead = append(vnDead, provider)
		}
		egressTestPasses(ctx, t)

		assertOnlineSuccessTestAnswer(t, "us", egressTestFind(ctx, t, egressTestLocationSpec(usCity), RankModeOnline, 10, false, server.NewId()),
			[]*egressTestProvider{usHealthy}, []*egressTestProvider{usDead})
		assertOnlineSuccessTestAnswer(t, "vn", egressTestFind(ctx, t, egressTestLocationSpec(vnCity), RankModeOnline, 10, false, server.NewId()),
			append([]*egressTestProvider{vnWorking}, vnDead...), nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, usCity), 1)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, vnCity), 5)
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts["no_success"], int64(1))

		t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_requires_url_success: false\n")))
		egressTestPasses(ctx, t)
		assertOnlineSuccessTestAnswer(t, "us disabled", egressTestFind(ctx, t, egressTestLocationSpec(usCity), RankModeOnline, 10, false, server.NewId()),
			[]*egressTestProvider{usHealthy, usDead}, nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, usCity), 2)
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts["no_success"], int64(0))
	})
}

// A provider the rule excludes is told why and what to fix, from the guard
// the score export published; with the kill switch it is online-only for its
// failing ratio again.
func TestProviderStatusExplainsTheUrlSuccessRule(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		dead := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, dead.clientId, server.NowUtc(), 5, 5)
		for _, provider := range []*egressTestProvider{healthy, dead} {
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
		}
		egressTestPasses(ctx, t)

		status := GetClientProviderStatus(ctx, dead.networkId, dead.clientId)
		if status == nil || status.Reason != "egress_no_success" || status.ReasonText == "" {
			t.Fatalf("status %+v, want the no-success reason with its text", status)
		}
		connect.AssertEqual(t, GetClientProviderStatus(ctx, healthy.networkId, healthy.clientId).Reason, ProviderStatusReasonNone)

		t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_requires_url_success: false\n")))
		egressTestPasses(ctx, t)
		connect.AssertEqual(t, GetClientProviderStatus(ctx, dead.networkId, dead.clientId).Reason, ProviderStatusReasonEgressFailing)
	})
}
