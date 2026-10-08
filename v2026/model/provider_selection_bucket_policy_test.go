package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
)

// Real connection facts, measured history, publication, picker reads and FP2
// must agree that an empty native Quality bucket still has lower-tier supply.
func TestFindProviders2FallbackUsesSelectedBucketPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Fallback City", "Synthetic Region", "United States", "us")
		elsewhere := egressTestCity(ctx, "Other City", "Synthetic Region", "United States", "us")
		speedUnknown := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		speedExcluded := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinNonQuality: true})
		onlineUnknown := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		onlineFailed := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		risky := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinRisk: true})
		intercepted := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		unreliable := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		private := egressTestConnect(ctx, t, city, egressTestFast, map[ProvideMode][]byte{ProvideModeNetwork: []byte("synthetic-private")}, nil)
		otherTarget := egressTestConnect(ctx, t, elsewhere, egressTestFast, nil, nil)
		for _, provider := range []*egressTestProvider{speedUnknown, speedExcluded, risky, intercepted, unreliable, private, otherTarget} {
			// The inclusive 4/5 boundary remains native Speed membership.
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 1)
		}
		// Two thirds stays Online, as does the completely unmeasured provider.
		egressTestHealth(ctx, onlineFailed.clientId, server.NowUtc(), 3, 1)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{ClientId: intercepted.clientId, MeasuredAt: server.NowUtc(), TLSAuthenticationFailure: true})
		egressTestReliability(ctx, unreliable.clientId, 1, 0, 0)
		egressTestPasses(ctx, t)
		clientSession := testingCreateProviderSearchSession(ctx, jwt.NewByJwt(server.NewId(), server.NewId(), "bucket-policy-test", false, false))

		// Initial GET and blank search share the public native-or-Online filter;
		// the legacy Quality key name must not hide a zero-Quality country.
		for _, read := range []func() (*FindLocationsResult, error){
			func() (*FindLocationsResult, error) { return GetProviderLocations(clientSession) },
			func() (*FindLocationsResult, error) {
				return FindProviderLocations(&FindLocationsArgs{}, clientSession)
			},
		} {
			result, err := read()
			found := false
			if result != nil {
				for _, location := range result.Locations {
					found = found || location.LocationId == city.CountryLocationId
				}
			}
			if err != nil || !found {
				t.Fatal("public fallback country was lost from the geographic picker")
			}
		}

		for _, nativeReader := range []bool{false, true} {
			func() {
				pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(fmt.Sprintf("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: %t\n", nativeReader)))
				defer pop()
				requestEgressIndexSettingsSnapshot.Store(nil)
				defer requestEgressIndexSettingsSnapshot.Store(nil)
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					result, err := FindProviders2(&FindProviders2Args{
						Specs: egressTestLocationSpec(city), RankMode: mode, Count: 10, ForceCount: true,
					}, clientSession)
					if err != nil || result == nil || len(result.Providers) != 4 {
						t.Fatalf("reader=%t mode=%s: selected-bucket fallback lost supply or crossed common gates", nativeReader, mode)
					}
					assertEgressTestNoRepeats(t, result.Providers)
					assertEgressTestTiersKeepOrder(t, result.Providers)
					for index, provider := range result.Providers {
						speed := provider.ClientId == speedUnknown.clientId || provider.ClientId == speedExcluded.clientId
						online := provider.ClientId == onlineUnknown.clientId || provider.ClientId == onlineFailed.clientId
						wantTier := 2 * egressTestBackfillOffset()
						if speed {
							wantTier = 0
							if mode == RankModeQuality {
								wantTier = egressTestBackfillOffset()
							}
						}
						if (!speed && !online) || speed != (index < 2) || provider.Tier != wantTier {
							t.Fatal("fallback changed native priority, target, ratio or common exclusion")
						}
					}
					// Explicit exclusions apply to every borrowed bucket.
					filtered, err := FindProviders2(&FindProviders2Args{
						Specs: egressTestLocationSpec(city), RankMode: mode, Count: 10, ForceCount: true,
						ExcludeClientIds: []server.Id{speedUnknown.clientId}, ExcludeDestinations: [][]server.Id{{onlineUnknown.clientId}},
					}, clientSession)
					if err != nil || filtered == nil || len(filtered.Providers) != 2 {
						t.Fatal("borrowed providers bypassed explicit request exclusions")
					}
					for _, provider := range filtered.Providers {
						if provider.ClientId != speedExcluded.clientId && provider.ClientId != onlineFailed.clientId {
							t.Fatal("fallback returned an explicitly excluded provider")
						}
					}
				}
			}()
		}
	})
}

// A warm Quality label may become invalid after publication. Both requested
// modes must demote it, while an independently eligible lower tier survives.
func TestFindProviders2FallbackRevalidatesBorrowedQuality(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		ctx := t.Context()
		location := server.NewId()
		stale := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		scores := map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {stale}}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_quality_verified=false WHERE connection_id=$1`, stale.ClientId))
		})
		for _, nativeReader := range []bool{false, true} {
			func() {
				pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(fmt.Sprintf("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: %t\n", nativeReader)))
				defer pop()
				requestEgressIndexSettingsSnapshot.Store(nil)
				defer requestEgressIndexSettingsSnapshot.Store(nil)
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					providers := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &location}}, mode, 10, false, server.NewId())
					if len(providers) != 1 || providers[0].ClientId != stale.ClientId || providers[0].Tier != 2*egressTestBackfillOffset() {
						t.Fatalf("reader=%t mode=%s: revoked Quality evidence was either promoted or removed from Online", nativeReader, mode)
					}
				}
			}()
		}
		// A common refusal still overrides all buckets after the same cache was
		// read successfully. No fallback may interpret it as Quality-only.
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, providerHardExclusionsKey, stale.ClientId.String()).Err())
		})
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			if providers := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &location}}, mode, 10, false, server.NewId()); len(providers) != 0 {
				t.Fatal("common exclusion survived into a borrowed answer")
			}
		}
	})
}

// Risk discovered by the current Quality read cannot become a lower-tier
// answer while the common Redis snapshot is older. Repeated and coalesced
// callers must retain the reason without caching positive eligibility.
func TestFindProviders2FallbackKeepsObservedRiskOutOfEveryTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		ctx := t.Context()
		location := server.NewId()
		good := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		risky := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		scores := map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {good, risky}}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_risk=true WHERE connection_id=$1`, risky.ClientId))
		})
		common, err := getProviderHardExclusions(ctx, []server.Id{risky.ClientId})
		if err != nil || common[risky.ClientId] {
			t.Fatal("fixture must retain the older common snapshot before live risk is read")
		}
		find := func(mode RankMode) error {
			clientSession := testingCreateProviderSearchSession(ctx, jwt.NewByJwt(server.NewId(), server.NewId(), "live-risk-fallback-test", false, false))
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: mode, Count: 10, ForceCount: true}, clientSession)
			wantTier := 0
			if mode == RankModeSpeed {
				wantTier = egressTestBackfillOffset()
			}
			if err != nil || result == nil || len(result.Providers) != 1 || result.Providers[0].ClientId != good.ClientId || result.Providers[0].Tier != wantTier {
				return errors.New("observed risk became fallback or prevented the independent valid native answer")
			}
			return nil
		}
		for range 2 {
			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				if err := find(mode); err != nil {
					t.Fatal(err)
				}
			}
		}
		const callers = 8
		start, finished := make(chan struct{}), make(chan error, callers)
		for index := range callers {
			go func() {
				<-start
				mode := RankModeQuality
				if index%2 == 0 {
					mode = RankModeSpeed
				}
				var findErr error
				if panicErr := server.HandleError(func() { findErr = find(mode) }); panicErr != nil {
					findErr = errors.New("concurrent fallback request panicked")
				}
				finished <- findErr
			}()
		}
		close(start)
		var combined error
		for range callers {
			combined = errors.Join(combined, <-finished)
		}
		if combined != nil {
			t.Fatal(combined)
		}
	})
}

// A later Quality read can reject a provider already selected as Speed or by
// name. Remove that earlier selection before filling the remaining quota.
func TestFindProviders2FallbackRevokesEarlierSelectedRisk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		ctx := t.Context()
		location := server.NewId()
		speed := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
		risky := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
		risky.PassesMinimums[RankModeQuality] = true
		qualityA := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		qualityB := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		online := onlineBackfillScore(true, 1)
		scores := map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {speed, risky, qualityA, qualityB, online}}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_risk=true WHERE connection_id=$1`, risky.ClientId))
		})
		common, err := getProviderHardExclusions(ctx, []server.Id{risky.ClientId})
		if err != nil || common[risky.ClientId] {
			t.Fatal("fixture must discover risk in the later Quality read")
		}
		observed := func() (uint64, float64, float64) {
			metric := &dto.Metric{}
			if err := findProviders2BackfillProviders.WithLabelValues(RankModeSpeed).(prometheus.Metric).Write(metric); err != nil {
				t.Fatal(err)
			}
			answered := testutil.ToFloat64(findProviders2AnsweredProviders.WithLabelValues(RankModeSpeed))
			return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum(), answered
		}
		for _, nativeReader := range []bool{false, true} {
			func() {
				pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(fmt.Sprintf("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: %t\n", nativeReader)))
				defer pop()
				requestEgressIndexSettingsSnapshot.Store(nil)
				defer requestEgressIndexSettingsSnapshot.Store(nil)
				for _, named := range []bool{false, true} {
					specs := []*ProviderSpec{{LocationId: &location}}
					if named {
						specs = append(specs, &ProviderSpec{ClientId: &risky.ClientId})
					}
					clientSession := testingCreateProviderSearchSession(ctx, jwt.NewByJwt(server.NewId(), server.NewId(), "late-risk-fallback-test", false, false))
					answers, borrowed, answered := observed()
					result, err := FindProviders2(&FindProviders2Args{Specs: specs, RankMode: RankModeSpeed, Count: 4, ForceCount: true}, clientSession)
					if err != nil || result == nil || len(result.Providers) != 4 {
						t.Fatalf("reader=%t named=%t: late risk removal did not refill available supply", nativeReader, named)
					}
					assertEgressTestNoRepeats(t, result.Providers)
					assertEgressTestTiersKeepOrder(t, result.Providers)
					for index, provider := range result.Providers {
						wantTier := egressTestBackfillOffset()
						valid := provider.ClientId == qualityA.ClientId || provider.ClientId == qualityB.ClientId
						if index == 0 {
							wantTier, valid = 0, provider.ClientId == speed.ClientId
						} else if index == 3 {
							wantTier, valid = 2*egressTestBackfillOffset(), provider.ClientId == online.ClientId
						}
						if !valid || provider.Tier != wantTier {
							t.Fatalf("reader=%t named=%t: later risk survived an earlier selection or changed fallback order", nativeReader, named)
						}
					}
					nextAnswers, nextBorrowed, nextAnswered := observed()
					if nextAnswers-answers != 1 || nextBorrowed-borrowed != 3 || nextAnswered-answered != 4 {
						t.Fatal("late risk removal left rejected providers in selection metrics")
					}
				}
			}()
		}
	})
}
