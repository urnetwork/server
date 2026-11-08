package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// One stored URL outcome of clientId on the named destination, of source and,
// for a country site, of countryCode.
func countrySiteDbTestStore(t testing.TB, clientId server.Id, at time.Time, name string, source ProviderEgressUrlSource, countryCode string, ok bool) ProviderEgressHealthStored {
	evidence := fp2TestUrlProbeEvidence(at, ok)
	destination := egresshealth.Destination{Name: name, Class: egresshealth.ClassSite, Url: "https://" + name + ".example/"}
	evidence.Destination = destination
	evidence.Security = []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}}
	okCount := 0
	if ok {
		okCount = 1
	}
	return StoreProviderEgressHealth(t.Context(), &ProviderEgressHealth{
		RunId: server.NewId(), ClientId: clientId, MeasuredAt: at, Total: 1, OKCount: okCount,
		ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: okCount, Total: 1}},
		UrlProbeEvidence: evidence, UrlSource: source, UrlCountryCode: countryCode,
	})
}

// The ingest pairs each measured country outcome with the same provider's
// scored general outcomes of the eight hours before it; a provider with too
// few of them is tallied unpaired. The refresh reads the pairs per network,
// without the providers a TLS finding judged their own excludes. A country
// site scores from the moment its state says it graduated.
func TestProviderEgressCountrySiteTallyPairsOutcomesWithTheirProvidersGeneralOutcomes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		networkId := server.NewId()
		paired, unpaired := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), paired, "", "")
		Testing_CreateDevice(ctx, networkId, server.NewId(), unpaired, "", "")
		at := now.Add(-time.Hour)
		next := func() time.Time {
			at = at.Add(time.Second)
			return at
		}
		// three of four general outcomes pass, then one country success
		for _, ok := range []bool{true, true, false, true} {
			countrySiteDbTestStore(t, paired, next(), "general-site", ProviderEgressUrlSourceGeneral, "", ok)
		}
		stored := countrySiteDbTestStore(t, paired, next(), "de-site", ProviderEgressUrlSourceCountry, "de", true)
		if !stored.Stored || !stored.Probation {
			t.Fatalf("a country outcome of a site without a state stored %+v, want on probation", stored)
		}
		// two general outcomes are too few to pair against
		for range 2 {
			countrySiteDbTestStore(t, unpaired, next(), "general-site", ProviderEgressUrlSourceGeneral, "", true)
		}
		countrySiteDbTestStore(t, unpaired, next(), "de-site", ProviderEgressUrlSourceCountry, "de", false)

		type tally struct {
			loads, ok, pairedLoads, pairedOk int
			expected                         float64
		}
		read := func(clientId server.Id) tally {
			var row tally
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT load_count, ok_count, paired_load_count, paired_ok_count, expected_ok_sum
					FROM provider_egress_country_site_tally WHERE client_id = $1 AND name = 'de-site' AND country_code = 'de'`, clientId).Scan(
					&row.loads, &row.ok, &row.pairedLoads, &row.pairedOk, &row.expected))
			})
			return row
		}
		if got := read(paired); got != (tally{loads: 1, ok: 1, pairedLoads: 1, pairedOk: 1, expected: 0.75}) {
			t.Fatalf("paired tally %+v", got)
		}
		if got := read(unpaired); got != (tally{loads: 1, ok: 0}) {
			t.Fatalf("unpaired tally %+v", got)
		}

		key := ProviderEgressCountrySiteKey{Name: "de-site", CountryCode: "de"}
		starts := map[ProviderEgressCountrySiteKey]time.Time{key: now.Add(-24 * time.Hour)}
		evidence := GetProviderEgressCountrySiteEvidence(ctx, starts, ProviderEgressCountrySiteRecentStart(now), nil)
		if networks := evidence[key]; len(networks) != 1 || networks[0].NetworkId != networkId ||
			networks[0].PairedLoads != 1 || networks[0].PairedOk != 1 || networks[0].ExpectedOk != 0.75 || networks[0].Providers != 1 ||
			networks[0].RecentPairedLoads != 1 {
			t.Fatalf("evidence %+v", networks)
		}
		if networks := GetProviderEgressCountrySiteEvidence(ctx, starts, ProviderEgressCountrySiteRecentStart(now), []server.Id{paired})[key]; len(networks) != 1 || networks[0].PairedLoads != 0 {
			t.Fatalf("evidence without the excluded provider %+v", networks)
		}

		// graduated: the next outcome scores
		SetProviderEgressCountrySites(ctx, []*ProviderEgressCountrySite{{
			ProviderEgressCountrySiteKey: key, Url: "https://de-site.example/", Scored: true, ProbationSince: now.Add(-8 * 24 * time.Hour),
		}}, []ProviderEgressCountrySiteKey{key})
		scoredClientId := server.NewId()
		stored = countrySiteDbTestStore(t, scoredClientId, now.Add(-time.Minute), "de-site", ProviderEgressUrlSourceCountry, "de", true)
		if !stored.Stored || stored.Probation {
			t.Fatalf("a graduated site's outcome stored %+v, want scored", stored)
		}
		if counts := GetAllProviderEgressHealthCounts(ctx)[scoredClientId]; counts.OKCount != 1 || counts.Total != 1 {
			t.Fatalf("a graduated site's outcome counted %d/%d, want 1/1", counts.OKCount, counts.Total)
		}
		// the same name listed for another country has a state of its own
		if stored := countrySiteDbTestStore(t, server.NewId(), now.Add(-time.Minute), "de-site", ProviderEgressUrlSourceCountry, "at", true); !stored.Probation {
			t.Fatalf("another country's list took de's graduation: %+v", stored)
		}

		// a state of a site no longer listed is removed
		SetProviderEgressCountrySites(ctx, nil, nil)
		if states := GetProviderEgressCountrySites(ctx); len(states) != 0 {
			t.Fatalf("an unlisted site kept its state: %+v", states)
		}
	})
}
