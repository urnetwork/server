package model

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Records `total` accepted URL runs of clientId from its country list,
// `failed` of them failed, the failures earliest, as a country site that has
// graduated into scoring would store them: scored (not on probation) but not
// general. No release graduates a country site yet; this is the outcome the
// Online rule and its country guard must never read once one does (D4).
func egressTestGraduatedCountryHealth(ctx context.Context, clientId server.Id, measuredAt time.Time, total int, failed int) {
	server.Tx(ctx, func(tx server.PgTx) {
		for index := range total {
			at := egressTestRunAt(measuredAt, index, total)
			ok := 0
			if index >= failed {
				ok = 1
			}
			evidence := fp2TestUrlProbeEvidence(at, ok == 1)
			evidence.Destination.Name = "country-site"
			evidence.Destination.Url = "https://country-site.example/"
			evidence.Security[0].Destination = evidence.Destination
			server.Raise(evidence.ValidateOutcome(ok, 1, false))
			encoded, err := json.Marshal(evidence)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe_evidence,url_probe_policy_version,url_source,url_probation)
				VALUES($1,$2,$3,$4,1,'{}',false,$5::jsonb,$6,$7,false)`,
				server.NewId(), clientId, at.UTC(), ok, string(encoded), evidence.PolicyVersion, int16(ProviderEgressUrlSourceCountry)))
		}
	})
}

// D4: the Online rule, its URL ratio and its country guard read the general
// catalog's outcomes alone, whatever else scores. Outcomes from a country list
// that has graduated into scoring still count where scored outcomes count,
// Quality and Speed among them, but they can neither end a country's
// exemption nor decide a provider's Online membership:
//
//   - in a country where the general catalog fails structurally (ge here, as
//     bw, kz and ge on Main), a working country list gave every provider a
//     success, which ended the exemption, while the general half still failed,
//     which put every provider under the 0.8 ratio and out of Online;
//   - a failing country site put a provider whose general outcomes all pass
//     under the ratio and out of Online;
//   - a passing country list lifted a provider whose general outcomes mostly
//     fail to the ratio, and kept it in Online.
//
// The country floor is off: us's one native is too few to keep it from
// admitting the providers this test holds below the bar.
func TestOnlineUrlRuleAndCountryGuardReadGeneralOutcomesOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_url_success_ratio: 0.8\n"+egressTestCountryFloorOffConfig)))
		usCity := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		geCity := egressTestCity(ctx, "Tbilisi", "Tbilisi", "Georgia", "ge")
		now := server.NowUtc()

		// ge: the general catalog fails for every provider, the country list
		// works for every one
		geProviders := []*egressTestProvider{}
		for range 3 {
			provider := egressTestConnect(ctx, t, geCity, egressTestFast, nil, nil)
			egressTestHealth(ctx, provider.clientId, now, 5, 5)
			egressTestGraduatedCountryHealth(ctx, provider.clientId, now, 5, 0)
			geProviders = append(geProviders, provider)
		}

		// us: the general catalog works
		usHealthy := egressTestConnect(ctx, t, usCity, egressTestFast, nil, nil)
		egressTestProbed(ctx, usHealthy, usCity, 0, "us")
		// every general outcome passes, every country outcome fails
		usBrokenCountrySite := egressTestConnect(ctx, t, usCity, egressTestFast, nil, nil)
		egressTestHealth(ctx, usBrokenCountrySite.clientId, now, 10, 0)
		egressTestGraduatedCountryHealth(ctx, usBrokenCountrySite.clientId, now, 10, 10)
		// two general outcomes in ten pass, and thirty country outcomes
		usMasked := egressTestConnect(ctx, t, usCity, egressTestFast, nil, nil)
		egressTestHealth(ctx, usMasked.clientId, now, 10, 8)
		egressTestGraduatedCountryHealth(ctx, usMasked.clientId, now, 30, 0)
		egressTestPasses(ctx, t)

		// the guard exempts ge, where no measured provider has a general
		// success, and not us
		exempt := map[string]bool{}
		if exemptCountries, ok := GetProviderOnlineSuccessExemptCountries(ctx); ok {
			for _, country := range exemptCountries {
				exempt[country.CountryCode] = true
			}
		}
		if !exempt["ge"] || exempt["us"] {
			t.Errorf("exempt countries %v, want ge alone", exempt)
		}

		geAnswer := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(geCity), RankModeOnline, 10, false, server.NewId()))
		for _, provider := range geProviders {
			if !slices.Contains(geAnswer, provider.clientId) {
				t.Errorf("a ge provider the exemption keeps is not online")
			}
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, geCity), 3)

		usAnswer := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(usCity), RankModeOnline, 10, false, server.NewId()))
		if !slices.Contains(usAnswer, usHealthy.clientId) {
			t.Error("the healthy us provider is not online")
		}
		if !slices.Contains(usAnswer, usBrokenCountrySite.clientId) {
			t.Error("a failing country site took a provider whose general outcomes all pass out of Online")
		}
		if slices.Contains(usAnswer, usMasked.clientId) {
			t.Error("a passing country list kept a provider below the ratio on its general outcomes in Online")
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, usCity), 2)

		// the graduated country outcomes still score where scored outcomes
		// count: half of the provider's twenty outcomes failed, so it is
		// online but not Quality
		score, ok := egressTestCachedScores(ctx, t, usCity, RankModeQuality, false)[usBrokenCountrySite.clientId]
		if !ok || !score.Online || score.PassesMinimums[RankModeQuality] {
			t.Errorf("the provider with a failing country site is not online-only: %+v", score)
		}
		reasons := CountProviderEgress(ctx).ReasonCounts
		connect.AssertEqual(t, reasons[ProviderExcludedLowSuccess], int64(1))
		connect.AssertEqual(t, reasons[ProviderExcludedNoSuccess], int64(0))

		inspection := InspectProviderEgress(ctx, usMasked.clientId)
		if inspection.Reason != ProviderExcludedLowSuccess || !inspection.BelowOnlineUrlRatio {
			t.Errorf("inspection reason %s below ratio %t, want %s on its general outcomes", inspection.Reason, inspection.BelowOnlineUrlRatio, ProviderExcludedLowSuccess)
		}
	})
}
