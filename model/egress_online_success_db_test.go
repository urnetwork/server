package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The URL-success rule against the database: the passes in the order the
// taskworker runs them, the published samples and counts, FindProviders2, the
// dashboard counts and the provider status. The first four tests use only
// names that predate the rule, so they run against the code before it. Each
// later test came with a follow-up fix and uses only names that predate that
// fix, so it runs against the commit before the fix.

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
// hard exclusion: force_minimum with quality or speed and an explicit client
// id still reach the provider, and its URL probes continue. force_minimum
// with online changes nothing, so that answer leaves the provider out.
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

		// force_minimum with quality or speed, and the explicit client id,
		// keep only the common gates
		forced := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, 10, true, server.NewId())
		if !slices.Contains(egressTestIds(forced), dead.clientId) {
			t.Error("force_minimum lost the provider the rule excludes")
		}
		// force_minimum with online changes nothing: the answer is the Online
		// pool, which the rule's exclusion is not in
		forcedOnline := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, true, server.NewId())
		assertOnlineSuccessTestAnswer(t, "forced online", forcedOnline, kept, []*egressTestProvider{dead})
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

// Provider status and inspection follow the rule as the score export applied
// it, its kill switch and its floor, never the reader's own provider.yml. A
// reader whose provider.yml differs from the export's, as during a rollout or
// after a rollback, still says what the export did.
func TestProviderStatusFollowsTheExportsUrlSuccessRule(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		// one of the two measured providers in us has a success
		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		dead := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, dead.clientId, server.NowUtc(), 5, 5)
		for _, provider := range []*egressTestProvider{healthy, dead} {
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
		}
		withProviderConfig := func(config string, callback func()) {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
			defer pop()
			callback()
		}

		// an export with the kill switch, read before the next export by a
		// reader whose provider.yml has the rule on again
		withProviderConfig("online_requires_url_success: false\n", func() {
			egressTestPasses(ctx, t)
		})
		connect.AssertEqual(t, GetClientProviderStatus(ctx, dead.networkId, dead.clientId).Reason, ProviderStatusReasonEgressFailing)
		connect.AssertEqual(t, InspectProviderEgress(ctx, dead.clientId).Reason, ProviderExcludedHealth)

		// an export with the rule on, read by a reader with the kill switch,
		// and by one whose floor would exempt the country
		egressTestPasses(ctx, t)
		for _, config := range []string{"online_requires_url_success: false\n", "online_success_country_floor: 0.9\n"} {
			withProviderConfig(config, func() {
				if reason := GetClientProviderStatus(ctx, dead.networkId, dead.clientId).Reason; reason != ProviderStatusReasonEgressNoSuccess {
					t.Errorf("reader provider.yml %q: status reason %s, want %s", config, reason, ProviderStatusReasonEgressNoSuccess)
				}
				inspection := InspectProviderEgress(ctx, dead.clientId)
				if inspection.Reason != ProviderExcludedNoSuccess || inspection.OnlineSuccessCountryExempt {
					t.Errorf("reader provider.yml %q: inspection reason %s exempt %t, want %s and not exempt", config, inspection.Reason, inspection.OnlineSuccessCountryExempt, ProviderExcludedNoSuccess)
				}
			})
		}
	})
}

// The floor's margin across real passes: a country each path exempted in its
// previous pass stays exempt until its share reaches the floor plus the
// margin, in the dashboard counts, the location counts and the score export
// that provider status reads, and the exemption ends once it does. Here the
// floor is 0.5 and the margin 0.25, over four measured providers.
func TestOnlineUrlSuccessCountryGuardMarginAcrossPasses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_success_country_floor: 0.5\nonline_success_country_floor_margin: 0.25\n"))
		defer pop()
		city := egressTestCity(ctx, "Hanoi", "Hanoi", "Vietnam", "vn")

		providers := []*egressTestProvider{}
		for range 4 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestHealth(ctx, provider.clientId, server.NowUtc().Add(-time.Minute), 5, 5)
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
			providers = append(providers, provider)
		}
		dead := providers[3]
		// each step: one more provider with a success, then the passes
		step := func(name string, succeeded *egressTestProvider, noSuccess int64, locationCount int, statusReason string) {
			egressTestHealth(ctx, succeeded.clientId, server.NowUtc(), 1, 0)
			egressTestPasses(ctx, t)
			if count := CountProviderEgress(ctx).ReasonCounts["no_success"]; count != noSuccess {
				t.Errorf("%s: the dashboard counts %d providers without a success excluded, want %d", name, count, noSuccess)
			}
			if count := egressTestLocationCount(ctx, t, city); count != locationCount {
				t.Errorf("%s: the location counts %d providers, want %d", name, count, locationCount)
			}
			if reason := GetClientProviderStatus(ctx, dead.networkId, dead.clientId).Reason; reason != statusReason {
				t.Errorf("%s: a provider without a success has status %s, want %s", name, reason, statusReason)
			}
		}
		// 1 of 4 is below the floor: exempt
		step("1 of 4", providers[0], 0, 4, ProviderStatusReasonEgressFailing)
		// 2 of 4 is the floor, inside the margin of an exempt country
		step("2 of 4", providers[1], 0, 4, ProviderStatusReasonEgressFailing)
		// 3 of 4 reaches the floor plus the margin: the exemption ends
		step("3 of 4", providers[2], 1, 3, ProviderStatusReasonEgressNoSuccess)
	})
}

// A process reads the published guard from Redis at most once a read window
// (30 seconds; these steps take milliseconds): a guard replaced or removed in
// Redis within the window is not read again, and a guard this process
// publishes is current at once.
func TestProviderOnlineSuccessBaselineIsReadOnceAWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		guard := func(floor float64) *providerOnlineSuccessBaseline {
			return &providerOnlineSuccessBaseline{
				computeTime:       server.NowUtc(),
				enabled:           true,
				floor:             floor,
				countryCodeCounts: map[string]providerOnlineSuccessCount{"us": {Measured: 2, Succeeded: 1, NoSuccess: 1}},
			}
		}
		write := func(floor float64) {
			value, err := encodeProviderOnlineSuccessBaseline(guard(floor), server.NowUtc())
			connect.AssertEqual(t, err, nil)
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, providerOnlineSuccessBaselineKey, value, time.Hour).Err())
			})
		}
		readFloor := func() float64 {
			baseline := getProviderOnlineSuccessBaseline(ctx)
			if baseline == nil {
				return -1
			}
			return baseline.floor
		}

		write(0.36)
		connect.AssertEqual(t, readFloor(), 0.36)
		// replaced in Redis within the window: not read again
		write(0.5)
		connect.AssertEqual(t, readFloor(), 0.36)
		// removed: the last read stands
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, providerOnlineSuccessBaselineKey).Err())
		})
		connect.AssertEqual(t, readFloor(), 0.36)
		// published by this process: current at once
		connect.AssertEqual(t, publishProviderOnlineSuccessBaseline(ctx, guard(0.7), time.Hour), nil)
		connect.AssertEqual(t, readFloor(), 0.7)
	})
}

// A provider.yml that gives the kill switch twice panics none of the passes
// or readers that apply the rule. Its kill switch cannot be read, so the rule
// is off in all of them: a provider without a success is online, counted and
// told it fails its network checks, not that it carries no traffic. The
// rollup runs first, under a readable file: it also reads the subscriber
// quality policy from provider.yml, which fails closed on a file it cannot
// decode.
func TestOnlineUrlSuccessPassesSurviveAKillSwitchGivenTwice(t *testing.T) {
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

		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_requires_url_success: true\nonline_requires_url_success: false\n"))
		defer pop()
		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
		connect.AssertEqual(t, UpdateClientLocations(ctx, time.Hour), nil)
		assertOnlineSuccessTestAnswer(t, "online", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			[]*egressTestProvider{healthy, dead}, nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 2)
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts["no_success"], int64(0))
		connect.AssertEqual(t, GetClientProviderStatus(ctx, dead.networkId, dead.clientId).Reason, ProviderStatusReasonEgressFailing)
		connect.AssertEqual(t, InspectProviderEgress(ctx, dead.clientId).Reason, ProviderExcludedHealth)
	})
}

// The floor's margin across processes. On Main each pass runs in whichever
// taskworker process claims it, so here every pass runs as a fresh process:
// server.Reset forgets everything a process holds. The margin still holds a
// country inside it, in the dashboard counts, the location counts and the
// score export that provider status reads, because a pass takes its previous
// state from the export's published guard, which every process reads. The
// floor is 0.5 and the margin 0.25, over four measured providers.
func TestOnlineUrlSuccessCountryGuardMarginAcrossProcesses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_success_country_floor: 0.5\nonline_success_country_floor_margin: 0.25\n"))
		defer pop()
		city := egressTestCity(ctx, "Hanoi", "Hanoi", "Vietnam", "vn")

		providers := []*egressTestProvider{}
		for range 4 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestHealth(ctx, provider.clientId, server.NowUtc().Add(-time.Minute), 5, 5)
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
			providers = append(providers, provider)
		}
		dead := providers[3]
		inFreshProcess := func(pass func()) {
			server.Reset()
			pass()
		}
		// each step: one more provider with a success, or none, then every
		// pass and reader in a process of its own
		step := func(name string, succeeded *egressTestProvider, noSuccess int64, locationCount int, statusReason string) {
			if succeeded != nil {
				egressTestHealth(ctx, succeeded.clientId, server.NowUtc(), 1, 0)
			}
			inFreshProcess(func() {
				now := server.NowUtc()
				UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
			})
			inFreshProcess(func() {
				connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
			})
			inFreshProcess(func() {
				connect.AssertEqual(t, UpdateClientLocations(ctx, time.Hour), nil)
			})
			inFreshProcess(func() {
				if count := CountProviderEgress(ctx).ReasonCounts["no_success"]; count != noSuccess {
					t.Errorf("%s: the dashboard counts %d providers without a success excluded, want %d", name, count, noSuccess)
				}
			})
			if count := egressTestLocationCount(ctx, t, city); count != locationCount {
				t.Errorf("%s: the location counts %d providers, want %d", name, count, locationCount)
			}
			inFreshProcess(func() {
				if reason := GetClientProviderStatus(ctx, dead.networkId, dead.clientId).Reason; reason != statusReason {
					t.Errorf("%s: a provider without a success has status %s, want %s", name, reason, statusReason)
				}
			})
		}
		// 1 of 4 is below the floor: exempt
		step("1 of 4", providers[0], 0, 4, ProviderStatusReasonEgressFailing)
		// 2 of 4 is the floor, inside the margin of an exempt country, in
		// every process that runs a pass
		step("2 of 4", providers[1], 0, 4, ProviderStatusReasonEgressFailing)
		step("2 of 4 again", nil, 0, 4, ProviderStatusReasonEgressFailing)
		// 3 of 4 reaches the floor plus the margin: the exemption ends
		step("3 of 4", providers[2], 1, 3, ProviderStatusReasonEgressNoSuccess)
	})
}

// A status request canceled while it reads the guard does not empty the
// process's guard for the read window: the read from Redis runs without the
// caller's cancellation, so the next caller gets the published guard.
func TestProviderOnlineSuccessBaselineSurvivesACanceledRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		value, err := encodeProviderOnlineSuccessBaseline(&providerOnlineSuccessBaseline{
			computeTime:       server.NowUtc(),
			enabled:           true,
			floor:             0.36,
			countryCodeCounts: map[string]providerOnlineSuccessCount{"us": {Measured: 2, Succeeded: 1, NoSuccess: 1}},
		}, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, providerOnlineSuccessBaselineKey, value, time.Hour).Err())
		})

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		getProviderOnlineSuccessBaseline(canceled)
		baseline := getProviderOnlineSuccessBaseline(ctx)
		if baseline == nil || baseline.floor != 0.36 {
			t.Fatalf("after a canceled read the guard is %v, want the published one", baseline)
		}
	})
}

// The states of the published guard a reader meets in the ordinary course of
// a rollout, none published yet and a version 1 guard from the code before,
// are logged as information, once, in words that read. A guard that cannot
// be trusted is an error, and a trusted one ends the window.
func TestProviderOnlineSuccessBaselineLogsExpectedStatesAsInformation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		reader := providerOnlineSuccessBaselineReader
		errorLogs, infoLogs := []string{}, []string{}
		clock := server.NowUtc()
		savedNow, savedErrorf, savedInfof := reader.now, reader.errorf, reader.infof
		reader.now = func() time.Time { return clock }
		reader.errorf = func(format string, args ...any) { errorLogs = append(errorLogs, fmt.Sprintf(format, args...)) }
		reader.infof = func(format string, args ...any) { infoLogs = append(infoLogs, fmt.Sprintf(format, args...)) }
		defer func() {
			reader.now, reader.errorf, reader.infof = savedNow, savedErrorf, savedInfof
		}()
		write := func(value []byte) {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, providerOnlineSuccessBaselineKey, value, time.Hour).Err())
			})
		}
		// one read past the window, then a new failure window
		readInNewWindow := func() *providerOnlineSuccessBaseline {
			reader.reset()
			clock = clock.Add(time.Minute)
			return getProviderOnlineSuccessBaseline(ctx)
		}
		expectLogs := func(state string, infoCount int, errorCount int, words string) {
			if len(infoLogs) != infoCount || len(errorLogs) != errorCount {
				t.Errorf("%s: %d information and %d error logs %v %v, want %d and %d", state, len(infoLogs), len(errorLogs), infoLogs, errorLogs, infoCount, errorCount)
				return
			}
			logs := append(append([]string{}, infoLogs...), errorLogs...)
			if last := logs[len(logs)-1]; !strings.Contains(last, words) || strings.Contains(last, "is none is") {
				t.Errorf("%s: log %q does not say %q in words that read", state, last, words)
			}
		}

		// none published yet
		if baseline := readInNewWindow(); baseline != nil {
			t.Fatalf("no guard read as %v", baseline)
		}
		expectLogs("none published", 1, 0, "none is published")

		// a version 1 guard, as the code before this one publishes
		write([]byte(`{"version":1,"compute_time":"` + clock.Format(time.RFC3339) + `","countries":{"us":{"measured":2,"succeeded":1,"no_success":1}}}`))
		readInNewWindow()
		expectLogs("version 1", 2, 0, "version 1")

		// a guard that cannot be trusted
		write([]byte(`{"version":2,`))
		readInNewWindow()
		expectLogs("not json", 2, 1, "not to be trusted")

		// a trusted guard ends the window
		value, err := encodeProviderOnlineSuccessBaseline(&providerOnlineSuccessBaseline{
			computeTime:       clock,
			enabled:           true,
			floor:             0.36,
			countryCodeCounts: map[string]providerOnlineSuccessCount{"us": {Measured: 2, Succeeded: 1, NoSuccess: 1}},
		}, clock)
		connect.AssertEqual(t, err, nil)
		write(value)
		clock = clock.Add(time.Minute)
		if baseline := getProviderOnlineSuccessBaseline(ctx); baseline == nil {
			t.Fatal("a trusted guard read as none")
		}
		expectLogs("trusted again", 3, 1, "trusted")
	})
}
