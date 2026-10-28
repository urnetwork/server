// The capability probe's sweep page and eligibility against the database.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A provider row of the sweep's population: a network client with the
// given activity and parent, a location reliability row connected or not
// (always valid), and a public provide key when public.
func capabilityProbeTestProvider(ctx context.Context, clientId server.Id, active bool, sourceClientId *server.Id, connected bool, public bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`INSERT INTO network_client (client_id, network_id, active, source_client_id) VALUES ($1, $2, $3, $4)`,
			clientId, server.NewId(), active, sourceClientId,
		))
		server.RaisePgResult(tx.Exec(ctx,
			`INSERT INTO network_client_location_reliability (client_id, update_block_number, country_location_id, client_address_hash_count, location_count, connected) VALUES ($1, 0, $2, 1, 1, $3)`,
			clientId, server.NewId(), connected,
		))
		if public {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO provide_key (client_id, provide_mode, secret_key) VALUES ($1, $2, $3)`,
				clientId, ProvideModePublic, []byte("synthetic-public-secret"),
			))
		}
	})
}

// The sweep pages the connected, valid providers in client id order after
// the cursor, every one of them, and marks due exactly the active top-level
// public providers without evidence, with modern evidence older than the
// modern refresh time, or with other evidence older than the other refresh
// time. A short page is the population's end.
func TestCapabilityProbePageMarksDueProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		ids := make([]server.Id, 9)
		for index := range ids {
			ids[index] = server.NewId()
		}
		slices.SortFunc(ids, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		unevidenced, freshModern, oldModern, legacy, networkOnly, derived, inactive, disconnected, freshLegacy :=
			ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8]
		for _, clientId := range []server.Id{unevidenced, freshModern, oldModern, legacy, freshLegacy} {
			capabilityProbeTestProvider(ctx, clientId, true, nil, true, true)
		}
		capabilityProbeTestProvider(ctx, networkOnly, true, nil, true, false)
		parent := server.NewId()
		capabilityProbeTestProvider(ctx, derived, true, &parent, true, true)
		capabilityProbeTestProvider(ctx, inactive, false, nil, true, true)
		capabilityProbeTestProvider(ctx, disconnected, true, nil, false, true)
		record := func(clientId server.Id, postQuantum bool, windowResizable bool, age time.Duration) {
			connect.AssertEqual(t, RecordProviderModernEvidence(ctx, clientId, modernGateTestEvidence(postQuantum, windowResizable, now.Add(-age))), nil)
		}
		record(freshModern, true, true, time.Hour)
		record(oldModern, true, true, 80*time.Hour)
		record(legacy, false, false, 30*time.Hour)
		record(freshLegacy, false, true, time.Hour)

		modernRefreshBefore := now.Add(-72 * time.Hour)
		otherRefreshBefore := now.Add(-24 * time.Hour)
		page := GetProviderCapabilityProbePage(ctx, server.Id{}, 100, modernRefreshBefore, otherRefreshBefore)
		want := []ProviderCapabilityProbeCandidate{
			{ClientId: unevidenced, Due: true},
			{ClientId: freshModern, Due: false},
			{ClientId: oldModern, Due: true},
			{ClientId: legacy, Due: true},
			{ClientId: networkOnly, Due: false},
			{ClientId: derived, Due: false},
			{ClientId: inactive, Due: false},
			{ClientId: freshLegacy, Due: false},
		}
		connect.AssertEqual(t, page, want)

		// non-modern evidence refreshed on the modern schedule is not due yet
		later := GetProviderCapabilityProbePage(ctx, server.Id{}, 100, modernRefreshBefore, modernRefreshBefore)
		connect.AssertEqual(t, later[3], ProviderCapabilityProbeCandidate{ClientId: legacy, Due: false})

		// pages continue after the cursor, and a short page is the end
		first := GetProviderCapabilityProbePage(ctx, server.Id{}, 3, modernRefreshBefore, otherRefreshBefore)
		connect.AssertEqual(t, first, want[:3])
		second := GetProviderCapabilityProbePage(ctx, first[len(first)-1].ClientId, 3, modernRefreshBefore, otherRefreshBefore)
		connect.AssertEqual(t, second, want[3:6])
		third := GetProviderCapabilityProbePage(ctx, second[len(second)-1].ClientId, 3, modernRefreshBefore, otherRefreshBefore)
		connect.AssertEqual(t, third, want[6:])
		connect.AssertEqual(t, len(GetProviderCapabilityProbePage(ctx, third[len(third)-1].ClientId, 3, modernRefreshBefore, otherRefreshBefore)), 0)

		// a page size outside the bounds is clamped, never unbounded
		connect.AssertEqual(t, len(GetProviderCapabilityProbePage(ctx, server.Id{}, 0, modernRefreshBefore, otherRefreshBefore)), 1)
	})
}

// The providers of the eligibility tests below, connected and measured under
// an Online URL ratio of 0.5. In us, where the probe works, a healthy
// provider, one without a URL success, one below the ratio, one whose
// failing ratio is at it, one with no URL run, one with a single passing run,
// and two hard exclusions with passing runs, ARIN risk and a failed hour of
// reliability; in vn, where the probe fails for every measured provider, one
// without a URL success.
type capabilityProbeOnlineTestProviders struct {
	usCity     *Location
	vnCity     *Location
	healthy    server.Id
	dead       server.Id
	low        server.Id
	failing    server.Id
	unprobed   server.Id
	warming    server.Id
	risky      server.Id
	unreliable server.Id
	vnDead     server.Id
}

// Connects the providers and runs the passes in the order the taskworker
// runs them, so the score export has published its samples and its
// URL-success guard; provider.yml sets the ratio until the test ends.
func newCapabilityProbeOnlineTestProviders(ctx context.Context, t testing.TB) *capabilityProbeOnlineTestProviders {
	t.Helper()
	t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_url_success_ratio: 0.5\n")))
	usCity := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
	vnCity := egressTestCity(ctx, "Hanoi", "Hanoi", "Vietnam", "vn")
	now := server.NowUtc()
	connected := func(city *Location, scores *ConnectionLocationScores) server.Id {
		return egressTestConnect(ctx, t, city, egressTestFast, nil, scores).clientId
	}
	providers := &capabilityProbeOnlineTestProviders{
		usCity:     usCity,
		vnCity:     vnCity,
		healthy:    connected(usCity, nil),
		dead:       connected(usCity, nil),
		low:        connected(usCity, nil),
		failing:    connected(usCity, nil),
		unprobed:   connected(usCity, nil),
		warming:    connected(usCity, nil),
		risky:      connected(usCity, &ConnectionLocationScores{ArinQualityVerified: true, ArinRisk: true}),
		unreliable: connected(usCity, nil),
		vnDead:     connected(vnCity, nil),
	}
	egressTestHealth(ctx, providers.healthy, now, 60, 0)
	egressTestHealth(ctx, providers.dead, now, 5, 5)
	// one of five is below the ratio, three of five at it
	egressTestHealth(ctx, providers.low, now, 5, 4)
	egressTestHealth(ctx, providers.failing, now, 5, 2)
	egressTestHealth(ctx, providers.warming, now, 1, 0)
	egressTestHealth(ctx, providers.risky, now, 60, 0)
	egressTestHealth(ctx, providers.unreliable, now, 60, 0)
	egressTestReliability(ctx, providers.unreliable, 1, 0.1, 0.1)
	egressTestHealth(ctx, providers.vnDead, now, 5, 5)
	egressTestPasses(ctx, t)
	return providers
}

// The cohort, in its order.
func (self *capabilityProbeOnlineTestProviders) clientIds() []server.Id {
	return []server.Id{
		self.healthy,
		self.dead,
		self.low,
		self.failing,
		self.unprobed,
		self.warming,
		self.risky,
		self.unreliable,
		self.vnDead,
	}
}

// Review finding 2 (FP2-MODERN-REVIEW.md): judged with Online alone, the
// widened probe would have measured providers the score export places in no
// bucket, since its filter had no published country and no URL-success
// guard, and a filter without a guard exempts every country. Through the
// production loader, read as a taskworker other than the export's reads it
// (from the guard the export published in Redis), probe eligibility is the
// export's Online membership, as the Online bucket's pages hold it: not the
// provider without a URL success in us, the one below the Online URL ratio,
// or either hard exclusion; the healthy provider, the failing ratio that
// stays Online, the provider with no URL run, the one with a single run and
// the provider without a success in exempt vn. The provider without a
// success in us and the one in vn differ only in their published country.
func TestCapabilityProbeEligibilityIsTheExportsOnlineMembership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		providers := newCapabilityProbeOnlineTestProviders(ctx, t)
		// the export's guard, as published: a probe pass in this process reads
		// Redis like any other
		providerOnlineSuccessBaselineReader.reset()

		eligibility := FilterProviderCapabilityProbeEligible(ctx, providers.clientIds())
		connect.AssertEqual(t, eligibility.Unguarded, false)
		connect.AssertEqual(t, eligibility.ClientIds, []server.Id{
			providers.healthy,
			providers.failing,
			providers.unprobed,
			providers.warming,
			providers.vnDead,
		})

		// Online reads the non-forced Quality pages (onlineClientScoreSourceRankMode)
		exportOnlineClientIds := map[server.Id]bool{}
		for _, city := range []*Location{providers.usCity, providers.vnCity} {
			for clientId, clientScore := range egressTestCachedScores(ctx, t, city, onlineClientScoreSourceRankMode, false) {
				if clientScore.Online {
					exportOnlineClientIds[clientId] = true
				}
			}
		}
		exportOnline := []server.Id{}
		for _, clientId := range providers.clientIds() {
			if exportOnlineClientIds[clientId] {
				exportOnline = append(exportOnline, clientId)
			}
		}
		connect.AssertEqual(t, eligibility.ClientIds, exportOnline)

		// the rules took exactly those two out of every bucket, one for each
		// reason
		counts := CountProviderEgress(ctx)
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedNoSuccess], int64(1))
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedLowSuccess], int64(1))
	})
}

// Without a trusted guard, none published or one past its max age, every
// country is exempt, as provider status reads it: the probe then measures
// the providers the guard took out too, and says it judged them unguarded.
// The hard exclusions stay out whatever the guard, and an empty cohort is
// judged unguarded by nothing.
func TestCapabilityProbeEligibilityWithoutAPublishedGuard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		providers := newCapabilityProbeOnlineTestProviders(ctx, t)
		everyOnline := []server.Id{
			providers.healthy,
			providers.dead,
			providers.low,
			providers.failing,
			providers.unprobed,
			providers.warming,
			providers.vnDead,
		}
		published := getProviderOnlineSuccessBaseline(ctx)
		if published == nil {
			t.Fatal("the export published no guard")
		}

		// past its max age
		expired, err := encodeProviderOnlineSuccessBaseline(published, server.NowUtc().Add(-providerOnlineSuccessBaselineMaxAge-time.Minute))
		connect.AssertEqual(t, err, nil)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, providerOnlineSuccessBaselineKey, expired, time.Hour).Err())
		})
		providerOnlineSuccessBaselineReader.reset()
		connect.AssertEqual(t, FilterProviderCapabilityProbeEligible(ctx, providers.clientIds()), ProviderCapabilityProbeEligibility{
			ClientIds: everyOnline,
			Unguarded: true,
		})

		// none published
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, providerOnlineSuccessBaselineKey).Err())
		})
		providerOnlineSuccessBaselineReader.reset()
		connect.AssertEqual(t, FilterProviderCapabilityProbeEligible(ctx, providers.clientIds()), ProviderCapabilityProbeEligibility{
			ClientIds: everyOnline,
			Unguarded: true,
		})

		connect.AssertEqual(t, FilterProviderCapabilityProbeEligible(ctx, nil), ProviderCapabilityProbeEligibility{ClientIds: []server.Id{}})
	})
}

// Through the database (review F8): a newer reading that is not modern does
// not replace modern evidence that is still fresh until the caller confirms
// the downgrade, and nothing about the row changes while it is held. A
// stale modern row, a row that is not modern, and a provider without a row
// take the reading at once; a modern reading is never held, and a reading
// at the row's own instant keeps the less modern of the two as before.
func TestRecordProviderModernEvidenceHoldsAnUnconfirmedDowngrade(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		read := func(clientId server.Id) *ProviderModernEvidenceRecord {
			return getProviderModernEvidenceRecords(ctx, []server.Id{clientId})[clientId]
		}
		record := func(clientId server.Id, postQuantum bool, windowResizable bool, observedAt time.Time, confirmed bool) bool {
			held, err := RecordProviderModernEvidenceHoldingDowngrade(ctx, clientId, modernGateTestEvidence(postQuantum, windowResizable, observedAt), confirmed)
			if err != nil {
				t.Fatal(err)
			}
			return held
		}
		freshModernAt := now.Add(-80 * time.Hour)

		modern := server.NewId()
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, modern, modernGateTestEvidence(true, true, freshModernAt)), nil)
		before := read(modern)
		for _, negative := range []struct {
			postQuantum     bool
			windowResizable bool
		}{
			{postQuantum: false, windowResizable: true},
			{postQuantum: true, windowResizable: false},
			{postQuantum: false, windowResizable: false},
		} {
			if !record(modern, negative.postQuantum, negative.windowResizable, now.Add(-time.Minute), false) {
				t.Fatalf("an unconfirmed %+v reading replaced fresh modern evidence", negative)
			}
			connect.AssertEqual(t, read(modern), before)
		}
		// a modern reading is never held, and refreshes the row
		connect.AssertEqual(t, record(modern, true, true, now.Add(-2*time.Minute), false), false)
		if refreshed := read(modern); !refreshed.PostQuantum || !refreshed.WindowResizable || !refreshed.MeasuredAt.Equal(now.Add(-2*time.Minute)) {
			t.Fatalf("a modern reading stored %+v", refreshed)
		}
		// a confirmed downgrade is written
		connect.AssertEqual(t, record(modern, false, true, now.Add(-time.Minute), true), false)
		downgraded := read(modern)
		if downgraded.PostQuantum || !downgraded.WindowResizable || !downgraded.MeasuredAt.Equal(now.Add(-time.Minute)) || downgraded.RunCount != 3 {
			t.Fatalf("a confirmed downgrade stored %+v", downgraded)
		}

		// stale modern evidence, other evidence and no evidence take the reading
		staleModern, legacy, unevidenced := server.NewId(), server.NewId(), server.NewId()
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, staleModern, modernGateTestEvidence(true, true, now.Add(-ProviderModernEvidenceMaxAge-time.Hour))), nil)
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, legacy, modernGateTestEvidence(false, true, freshModernAt)), nil)
		for _, clientId := range []server.Id{staleModern, legacy, unevidenced} {
			if record(clientId, false, false, now.Add(-time.Minute), false) {
				t.Fatal("a reading was held back from evidence that is not fresh and modern")
			}
			if stored := read(clientId); stored == nil || stored.PostQuantum || stored.WindowResizable || !stored.MeasuredAt.Equal(now.Add(-time.Minute)) {
				t.Fatalf("an unheld reading stored %+v", stored)
			}
		}

		// a conflicting reading at the modern row's own instant is not a
		// downgrade to hold: each capability keeps the less modern reading
		sameInstant := server.NewId()
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, sameInstant, modernGateTestEvidence(true, true, freshModernAt)), nil)
		connect.AssertEqual(t, record(sameInstant, true, false, freshModernAt, false), false)
		if stored := read(sameInstant); !stored.PostQuantum || stored.WindowResizable || stored.RunCount != 1 {
			t.Fatalf("a same-instant reading stored %+v", stored)
		}
	})
}
