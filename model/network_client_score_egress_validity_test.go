// How long a score export's URL verdict vouches for its evidence
// (ClientScore.EgressValidUntil, clientScoreEgressValidUntil), pure:
// providers exported as UpdateClientScores ranks and caches them, decoded as
// a request reads the cache, and judged by the request's reader. filterPool
// demotes a native to Online exactly when the reader finds its evidence
// expired (TestFp2CachedEvidenceExpiresBeforeCacheTtl covers the demotion
// itself against the cache).
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The export's read of the evidence window, fixed, and before the modern
// provider gate's default start (2026-10-21), so the gate decides nothing
// here.
var egressValidityTestSnapshot = time.Date(2026, time.October, 15, 19, 0, 0, 0, time.UTC)

// One provider's accepted URL outcomes in the evidence window the export read:
// its newest and oldest run, each by its age at the snapshot, and the tally.
// No tally is an unprobed provider.
type egressValidityTestProvider struct {
	name      string
	newestAge time.Duration
	oldestAge time.Duration
	urlOk     int
	urlTotal  int
}

// The reader of the live api (fdf347a3, filterPool and readExclusions),
// verbatim: whether the URL evidence a cached score vouches for has expired at
// now, which takes away its native membership and measured URL weight.
// Main's urlEvidenceCurrent is its negation.
func egressValidityTestExpired(clientScore *ClientScore, now time.Time) bool {
	return clientScore.EgressValidUntil == nil || !now.Before(*clientScore.EgressValidUntil)
}

// Exports the providers, all published in one country, as UpdateClientScores
// does under settings with the evidence window read at the snapshot: a source
// row per reliability lookback, both passing their floors, the target maps,
// the shared egress decision and the ranking. Then encodes the country's pool
// in mode as the cache holds it and decodes it as a request reads it: the
// union pages and the native facet pages, by provider name.
func egressValidityTestExport(
	t testing.TB,
	settings *EgressIndexSettings,
	mode RankMode,
	providers []*egressValidityTestProvider,
) (union map[string]*ClientScore, native map[string]*ClientScore) {
	t.Helper()
	snapshot := egressValidityTestSnapshot
	countFilter := providerCountFilter{
		now:                     snapshot,
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		healthCounts:            map[server.Id]ProviderEgressHealthCounts{},
		healthWindowEnd:         snapshot,
		countryCodes:            map[server.Id]string{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		modernEvidence:          map[server.Id]*ProviderModernEvidenceRecord{},
	}
	countryLocationId := server.NewId()
	countryCode := "us"
	names := map[server.Id]string{}
	rows := []clientScoreSourceRow{}
	for _, provider := range providers {
		clientId := server.NewId()
		networkId := server.NewId()
		names[clientId] = provider.name
		var egressIndex *int
		var egressQuality *bool
		if 0 < provider.urlTotal {
			counts := ProviderEgressHealthCounts{
				MeasuredAt:      snapshot.Add(-provider.newestAge),
				FirstMeasuredAt: snapshot.Add(-provider.oldestAge),
				OKCount:         provider.urlOk,
				Total:           provider.urlTotal,
			}
			countFilter.healthCounts[clientId] = counts
			index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: counts.MeasuredAt, OkCount: counts.OKCount, Total: counts.Total}, snapshot, settings)
			egressIndex = &index.Index
			egressQuality = index.QualityVerdict()
		}
		for lookbackIndex, weights := range map[int][2]float64{1: {0.9, 0.99}, 2: {0.9, 0.9}} {
			score := &ClientScore{
				ClientId:                     clientId,
				LookbackIndex:                lookbackIndex,
				NetworkId:                    networkId,
				IpFamilies:                   ClientScoreIpFamilyV4,
				ReliabilityWeight:            weights[0],
				IndependentReliabilityWeight: weights[1],
				MinRelativeLatencyMillis:     10,
				MaxBytesPerSecond:            50 * Mib,
				HasLatencyTest:               true,
				HasSpeedTest:                 true,
				Scores:                       map[string]int{},
				Tiers:                        map[string]int{},
			}
			rankModeBases, rankModeMinimumBases := clientScoreRankModeBases(0, 0, egressIndex)
			setClientScoreRanks(score, rankModeBases, rankModeMinimumBases, 10, 50*Mib, true, true)
			rows = append(rows, clientScoreSourceRow{
				score:     score,
				countryId: &countryLocationId,
				egress: &clientScoreEgress{
					egressIndex:          egressIndex,
					egressQuality:        egressQuality,
					publishedCountryCode: &countryCode,
				},
			})
		}
	}

	targetMaps := newClientScoreTargetMaps(rows, nil, countFilter.hasHardEgressFailure)
	clientScores := targetMaps.locationClientScores[countryLocationId]
	minimums := map[int]float64{1: 0.95, 2: 0.7, 3: 0.6}
	for clientId, clientScore := range clientScores {
		egress := targetMaps.clientIdEgresses[clientId]
		decision := decideProviderEgress(
			countFilter.egressFacts(clientId, egress.publishedCountryCode, egress.egressIndex, egress.egressQuality, settings),
			false,
		)
		rankClientScore(clientScore, decision, countFilter, settings, minimums)
	}

	payload := exportClientScores(false, mode, clientScores)
	decode := func(counts []int, encodeSample func(int) []byte, pool map[string]*ClientScore) {
		for i := range counts {
			sample, err := decodeClientScoreSample(encodeSample(i))
			if err != nil {
				t.Fatal(err)
			}
			for _, clientScore := range sample {
				pool[names[clientScore.ClientId]] = clientScore
			}
		}
	}
	union = map[string]*ClientScore{}
	decode(payload.counts, payload.encodeSample, union)
	native = map[string]*ClientScore{}
	for _, facet := range ipFamilyFacets {
		decode(payload.nativeFacets[facet].counts, payload.nativeFacets[facet].encodeSample, native)
	}
	if len(union) != len(providers) {
		t.Fatalf("the exported pool holds %d providers, want %d", len(union), len(providers))
	}
	return union, native
}

// A native measured all window long, whose oldest run leaves the window five
// minutes after the export read it: the next export decides it again within
// minutes, and it stays native until then. The verdict used to end with the
// oldest run, so for the minutes until the next export the native read as
// Online. A measured Online provider keeps its measured URL weight the same
// way, and an unprobed one vouches for nothing.
func TestScoreExportNativeOutlivesItsOldestRun(t *testing.T) {
	snapshot := egressValidityTestSnapshot
	for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
		union, native := egressValidityTestExport(t, DefaultEgressIndexSettings(), mode, []*egressValidityTestProvider{
			{name: "native", newestAge: 3 * time.Minute, oldestAge: ProviderEgressHealthMaxAge - 5*time.Minute, urlOk: 20, urlTotal: 20},
			// 2 of 10 is below the 4 in 5 the native buckets require
			{name: "measured_online", newestAge: 3 * time.Minute, oldestAge: ProviderEgressHealthMaxAge - 5*time.Minute, urlOk: 2, urlTotal: 10},
			{name: "unprobed"},
		})

		nativeScore := union["native"]
		if !nativeScore.PassesMinimums[RankModeQuality] || !nativeScore.PassesMinimums[RankModeSpeed] || native["native"] == nil {
			t.Fatalf("mode=%s the native was exported with native=%v, in the native pages %t", mode, nativeScore.PassesMinimums, native["native"] != nil)
		}
		onlineScore := union["measured_online"]
		if !onlineScore.Online || onlineScore.PassesMinimums[RankModeQuality] || onlineScore.PassesMinimums[RankModeSpeed] || native["measured_online"] != nil {
			t.Fatalf("mode=%s the measured provider was exported with online=%t native=%v", mode, onlineScore.Online, onlineScore.PassesMinimums)
		}
		if weight := onlineScore.UrlProbeSuccessWeight; weight < 0.27 || 0.29 < weight {
			t.Fatalf("mode=%s the measured provider's URL weight is %v, want 0.1 + 0.9 * 2/10", mode, weight)
		}

		for name, clientScore := range map[string]*ClientScore{"native": nativeScore, "native page": native["native"], "measured_online": onlineScore} {
			if clientScore.EgressValidUntil == nil {
				t.Fatalf("mode=%s %s vouches for no evidence", mode, name)
			}
			if want := snapshot.Add(clientScoreEgressMaxStaleness); !clientScore.EgressValidUntil.Equal(want) {
				t.Errorf("mode=%s %s is valid until %s, want %s, the bounded staleness past the export's read", mode, name, clientScore.EgressValidUntil.Format(time.RFC3339), want.Format(time.RFC3339))
			}
			// before and after the oldest run left the window
			for _, age := range []time.Duration{4 * time.Minute, 10 * time.Minute} {
				if egressValidityTestExpired(clientScore, snapshot.Add(age)) {
					t.Errorf("mode=%s %s expired %s after the export, which the reader demotes to Online until the next export", mode, name, age)
				}
			}
		}
		if union["unprobed"].EgressValidUntil != nil {
			t.Fatalf("mode=%s the unprobed provider vouches for evidence until %s", mode, union["unprobed"].EgressValidUntil)
		}
	}
}

// An export stall past the bound demotes every native the stalled export
// published, an old one whose oldest run left the window minutes after the
// export as well as a new one whose oldest run had hours left: nothing has
// looked at their evidence since. The exact bound is stale.
func TestScoreExportVerdictEndsAtTheStalenessBound(t *testing.T) {
	snapshot := egressValidityTestSnapshot
	bound := snapshot.Add(clientScoreEgressMaxStaleness)
	union, native := egressValidityTestExport(t, DefaultEgressIndexSettings(), RankModeQuality, []*egressValidityTestProvider{
		{name: "measured_all_window", newestAge: 3 * time.Minute, oldestAge: ProviderEgressHealthMaxAge - 5*time.Minute, urlOk: 20, urlTotal: 20},
		{name: "new", newestAge: 3 * time.Minute, oldestAge: time.Hour, urlOk: 3, urlTotal: 3},
	})
	for _, name := range []string{"measured_all_window", "new"} {
		clientScore := union[name]
		if !clientScore.PassesMinimums[RankModeQuality] || native[name] == nil {
			t.Fatalf("%s was exported with native=%v", name, clientScore.PassesMinimums)
		}
		for _, c := range []struct {
			at      time.Time
			expired bool
		}{
			{at: bound.Add(-time.Nanosecond)},
			{at: bound, expired: true},
			{at: bound.Add(time.Minute), expired: true},
		} {
			if expired := egressValidityTestExpired(clientScore, c.at); expired != c.expired {
				t.Errorf("%s at the export + %s: expired=%t, want %t", name, c.at.Sub(snapshot), expired, c.expired)
			}
		}
	}
}

// A verdict never outlives its evidence: once the newest run it was decided on
// leaves the window, none is left, and a fresh export finds no evidence
// (ComputeEgressIndex) at the same instant the cached verdict ends, under the
// default evidence age and one past the eight-hour cap alike.
func TestScoreExportVerdictEndsWithItsNewestRun(t *testing.T) {
	snapshot := egressValidityTestSnapshot
	for _, evidenceMaxAge := range []time.Duration{ProviderEgressHealthMaxAge, 12 * time.Hour} {
		settings := DefaultEgressIndexSettings()
		settings.EvidenceMaxAge = evidenceMaxAge
		// the newest run leaves the eight-hour window ten minutes after the
		// export, so the runs left in it span less than Quality's thirty
		// minutes (egress_native_min_runs.go): the verdict is Speed's
		newest := &egressValidityTestProvider{name: "fading", newestAge: ProviderEgressHealthMaxAge - 10*time.Minute, oldestAge: ProviderEgressHealthMaxAge - time.Minute, urlOk: 5, urlTotal: 5}
		union, native := egressValidityTestExport(t, settings, RankModeSpeed, []*egressValidityTestProvider{newest})
		clientScore := union["fading"]
		if !clientScore.PassesMinimums[RankModeSpeed] || native["fading"] == nil {
			t.Fatalf("evidence max age %s: the provider was exported with native=%v", evidenceMaxAge, clientScore.PassesMinimums)
		}
		end := snapshot.Add(-newest.newestAge).Add(ProviderEgressHealthMaxAge)
		if clientScore.EgressValidUntil == nil || !clientScore.EgressValidUntil.Equal(end) {
			t.Fatalf("evidence max age %s: valid until %v, want %s, when its newest run leaves the window", evidenceMaxAge, clientScore.EgressValidUntil, end.Format(time.RFC3339))
		}
		run := &EgressHealthRun{MeasuredAt: snapshot.Add(-newest.newestAge), OkCount: newest.urlOk, Total: newest.urlTotal}
		for _, at := range []time.Time{end.Add(-time.Nanosecond), end} {
			evidence := ComputeEgressIndex(run, at, settings).Evidence
			if expired := egressValidityTestExpired(clientScore, at); expired == evidence {
				t.Errorf("evidence max age %s, at the export + %s: the cached verdict expired=%t while a fresh export finds evidence=%t", evidenceMaxAge, at.Sub(snapshot), expired, evidence)
			}
		}
	}
}

// Under a shorter evidence_max_age the verdict ends as the index's evidence
// does, the newest run that much older, and otherwise at the bound. Ending
// with the oldest run, a native measured all window long was already expired
// when the export published it.
func TestScoreExportVerdictUnderShorterEvidenceMaxAge(t *testing.T) {
	snapshot := egressValidityTestSnapshot
	settings := DefaultEgressIndexSettings()
	settings.EvidenceMaxAge = 4 * time.Hour
	union, native := egressValidityTestExport(t, settings, RankModeQuality, []*egressValidityTestProvider{
		{name: "measured_all_window", newestAge: 3 * time.Minute, oldestAge: ProviderEgressHealthMaxAge - 5*time.Minute, urlOk: 20, urlTotal: 20},
		// the newest run is four hours old ten minutes after the export
		{name: "fading", newestAge: 4*time.Hour - 10*time.Minute, oldestAge: ProviderEgressHealthMaxAge - 5*time.Minute, urlOk: 20, urlTotal: 20},
	})
	for name, want := range map[string]time.Time{
		"measured_all_window": snapshot.Add(clientScoreEgressMaxStaleness),
		"fading":              snapshot.Add(10 * time.Minute),
	} {
		clientScore := union[name]
		if !clientScore.PassesMinimums[RankModeQuality] || native[name] == nil {
			t.Fatalf("%s was exported with native=%v", name, clientScore.PassesMinimums)
		}
		if clientScore.EgressValidUntil == nil || !clientScore.EgressValidUntil.Equal(want) {
			t.Errorf("%s is valid until %v, want %s", name, clientScore.EgressValidUntil, want.Format(time.RFC3339))
		}
		if egressValidityTestExpired(clientScore, snapshot.Add(5*time.Minute)) {
			t.Errorf("%s expired five minutes after the export", name)
		}
	}
}
