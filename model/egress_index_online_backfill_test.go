// Independently sampled mode caches must not hide eligible online backfill.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A bounded synthetic cache page, admitted by the exporter before this request.
func onlineBackfillScore(online bool, reliabilityWeight float64) *ClientScore {
	validUntil := server.NowUtc().Add(time.Hour)
	return &ClientScore{
		EgressValidUntil:  &validUntil,
		PassesMinimums:    map[string]bool{RankModeQuality: !online, RankModeSpeed: !online},
		ClientId:          server.NewId(),
		NetworkId:         server.NewId(),
		IpFamilies:        ClientScoreIpFamilyV4,
		Online:            online,
		ReliabilityWeight: reliabilityWeight,
		Scores:            map[string]int{RankModeQuality: 0, RankModeSpeed: 0},
		Tiers:             map[string]int{RankModeQuality: 0, RankModeSpeed: 0},
		ScaledWeights:     map[string]float32{RankModeQuality: 1, RankModeSpeed: 1},
	}
}

// Synthetic cache members still require affirmative current subscriber facts.
// Insert only absent fixture rows so a deliberately revoked fact stays revoked.
func writeSubscriberFactsForScores(ctx context.Context, scores []*ClientScore) {
	if len(scores) == 0 {
		return
	}
	ids := make([]server.Id, 0, len(scores))
	for _, score := range scores {
		ids = append(ids, score.ClientId)
	}
	handler := CreateNetworkClientHandler(ctx)
	location := server.NewId()
	connectionTime := server.NowUtc()
	// Each batch retains atomic connection/location facts while releasing the
	// real session trigger's endpoint fences before the next batch. Callers
	// publish their cache only after the complete original population exists.
	for first := 0; first < len(ids); first += arinRemotePopulationBatchSize {
		batchIds := ids[first:min(first+arinRemotePopulationBatchSize, len(ids))]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO network_client_connection
				(client_id, connection_id, connect_time, connection_host, connection_service, connection_block, handler_id)
			SELECT DISTINCT id, id, $3::timestamp, 'synthetic', 'synthetic', 'synthetic', $2::uuid
			FROM unnest($1::uuid[]) AS id ON CONFLICT (connection_id) DO NOTHING
		`, batchIds, handler, connectionTime))
			server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO network_client_location
				(client_id, connection_id, city_location_id, region_location_id, country_location_id, arin_quality_verified, arin_quality_write_token)
			SELECT DISTINCT id, id, $2::uuid, $2::uuid, $2::uuid, true, id
			FROM unnest($1::uuid[]) AS id ON CONFLICT (connection_id) DO NOTHING
		`, batchIds, location))
		})
	}
}

// Each mode deliberately has one different page. No random page draw or
// exporter timing race controls this fixture's candidate population.
func writeOnlineBackfillSample(ctx context.Context, t testing.TB, locationId server.Id, rankMode RankMode, forceMinimum bool, scores []*ClientScore) {
	t.Helper()
	writeSubscriberFactsForScores(ctx, scores)
	callerLocationIds := []server.Id{{}}
	for _, countryLocationId := range countryCodeLocationIds() {
		callerLocationIds = append(callerLocationIds, countryLocationId)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		for _, callerLocationId := range callerLocationIds {
			server.Raise(r.Set(ctx,
				clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, callerLocationId),
				gobEncodeForTest(t, []int{len(scores)}), time.Minute).Err())
			server.Raise(r.Set(ctx,
				clientScoreLocationSampleKey(forceMinimum, rankMode, locationId, callerLocationId, 0),
				gobEncodeForTest(t, scores), time.Minute).Err())
		}
		server.Raise(r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember).Err())
	})
}

// An online provider found only in the other mode is still usable when the
// requested mode's independently sampled page contains no eligible candidates.
func TestBackfillOnlineUsesOtherModeSample(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			otherRankMode, _ := backfillRankMode(rankMode)
			locationId := server.NewId()
			online := onlineBackfillScore(true, 1)
			writeOnlineBackfillSample(ctx, t, locationId, rankMode, false, nil)
			writeOnlineBackfillSample(ctx, t, locationId, otherRankMode, false, []*ClientScore{online})
			providers := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &locationId}}, rankMode, 1, false, server.NewId())
			if len(providers) != 1 || providers[0].ClientId != online.ClientId || providers[0].Tier != 2*egressTestBackfillOffset() {
				t.Fatalf("%s ignored eligible other-mode online sample: returned=%d want=1", rankMode, len(providers))
			}
		}
	})
}

// Online backfill unions the filtered pages, keeps the requested page's copy
// on overlap, and never repeats a provider already selected from either mode.
func TestBackfillOnlineUnionsSamplesWithoutRepeatingEarlierTiers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		locationId := server.NewId()
		native := onlineBackfillScore(false, 1)
		borrowed := onlineBackfillScore(false, 1)
		shared := onlineBackfillScore(true, 2)
		ownOnly := onlineBackfillScore(true, 3)
		otherOnly := onlineBackfillScore(true, 1)
		staleNative, staleBorrowed, otherShared := *native, *borrowed, *shared
		staleNative.Online, staleBorrowed.Online = true, true
		staleNative.PassesMinimums, staleBorrowed.PassesMinimums = nil, nil
		otherShared.ReliabilityWeight = 9
		writeOnlineBackfillSample(ctx, t, locationId, RankModeQuality, false, []*ClientScore{native, shared, ownOnly, &staleBorrowed})
		writeOnlineBackfillSample(ctx, t, locationId, RankModeSpeed, false, []*ClientScore{borrowed, &otherShared, otherOnly, &staleNative})
		providers := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &locationId}}, RankModeQuality, 5, false, server.NewId())
		wantIds := []server.Id{native.ClientId, borrowed.ClientId, ownOnly.ClientId, shared.ClientId, otherOnly.ClientId}
		if !slices.Equal(egressTestIds(providers), wantIds) {
			t.Fatalf("asymmetric online union lost a candidate, repeated a selected tier, or replaced the primary record: returned=%d want=%d", len(providers), len(wantIds))
		}
		assertEgressTestNoRepeats(t, providers)
		assertEgressTestTiersKeepOrder(t, providers)
		for i, provider := range providers {
			wantTier := 2 * egressTestBackfillOffset()
			if i == 0 {
				wantTier = 0
			} else if i == 1 {
				wantTier = egressTestBackfillOffset()
			}
			if provider.Tier != wantTier {
				t.Fatalf("answer %d tier=%d want=%d", i, provider.Tier, wantTier)
			}
		}
	})
}

// The additional online sample is usable only after the same hard, network,
// explicit-destination, and family exclusions as the original mode's sample.
func TestBackfillOnlineOtherModePreservesRequestFilters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		locationId, callerNetworkId := server.NewId(), server.NewId()
		allowed := onlineBackfillScore(true, 2)
		sameNetwork := onlineBackfillScore(true, 1)
		sameNetwork.NetworkOnly, sameNetwork.NetworkId = true, callerNetworkId
		otherNetwork := onlineBackfillScore(true, 10)
		otherNetwork.NetworkOnly = true
		dark, intercepted := onlineBackfillScore(true, 10), onlineBackfillScore(true, 10)
		explicit, finalHop := onlineBackfillScore(true, 10), onlineBackfillScore(true, 10)
		wrongFamily := onlineBackfillScore(true, 10)
		wrongFamily.IpFamilies = ClientScoreIpFamilyV6
		writeOnlineBackfillSample(ctx, t, locationId, RankModeQuality, false, nil)
		writeOnlineBackfillSample(ctx, t, locationId, RankModeSpeed, false, []*ClientScore{
			allowed, sameNetwork, otherNetwork, dark, intercepted, explicit, finalHop, wrongFamily,
		})
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, providerHardExclusionsKey, dark.ClientId.String(), intercepted.ClientId.String()).Err())
		})
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(callerNetworkId, server.NewId(), "online-backfill-test", false, false))
		result, err := FindProviders2(&FindProviders2Args{
			Specs:               []*ProviderSpec{{LocationId: &locationId}},
			RankMode:            RankModeQuality,
			Count:               8,
			ForceCount:          true,
			IpFamily:            "v4-capable",
			ExcludeClientIds:    []server.Id{explicit.ClientId},
			ExcludeDestinations: [][]server.Id{{finalHop.ClientId}},
		}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(egressTestIds(result.Providers), []server.Id{allowed.ClientId, sameNetwork.ClientId}) {
			t.Fatalf("other-mode online eligibility changed: returned=%d want=2", len(result.Providers))
		}
	})
}

// force_minimum keeps its existing requested-cache-only contract even when the
// ordinary other-mode sample contains an otherwise eligible online provider.
func TestBackfillOnlineForceMinimumDoesNotBorrow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		locationId := server.NewId()
		native, otherOnline := onlineBackfillScore(false, 1), onlineBackfillScore(true, 1)
		writeOnlineBackfillSample(ctx, t, locationId, RankModeQuality, true, []*ClientScore{native})
		writeOnlineBackfillSample(ctx, t, locationId, RankModeSpeed, false, []*ClientScore{otherOnline})
		providers := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &locationId}}, RankModeQuality, 2, true, server.NewId())
		if len(providers) != 1 || providers[0].ClientId != native.ClientId {
			t.Fatalf("forced minimum unexpectedly borrowed from another mode: returned=%d want=1", len(providers))
		}
	})
}
