// Mixed-generation SQL writes must revoke current Quality eligibility even
// while a previously published score cache and rollup still admit the provider.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Pin the complete server-0b8e connection upsert, which knows the lookup epoch
// and risk flags but omits the positive fact and its successor's write token.
const subscriberLegacyLocationUpsertSql = `
	INSERT INTO network_client_location (
		connection_id, client_id, city_location_id, region_location_id, country_location_id,
		net_type_hosting, net_type_privacy, net_type_virtual, net_type_foreign, network_id,
		accuracy_km, genesis_location_id, arin_risk, arin_non_quality, arin_lookup_at, arin_database_build_epoch
	)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
	ON CONFLICT (connection_id) DO UPDATE SET
		client_id=$2, city_location_id=$3, region_location_id=$4, country_location_id=$5,
		net_type_hosting=$6, net_type_privacy=$7, net_type_virtual=$8, net_type_foreign=$9,
		network_id=$10, accuracy_km=$11, genesis_location_id=$12, arin_risk=$13,
		arin_non_quality=$14, arin_lookup_at=$15, arin_database_build_epoch=$16
`

// Force new -> old -> new transitions against the real writer, live-connection
// guard, warm cache, and rollup. Identical legacy values still revoke evidence.
func TestSubscriberQualityMixedWritersInvalidateWarmRequestAndRollup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		cacheClock, cacheNow := subscriberCacheTestClock()
		previousCache := providerSubscriberNegativeCache
		providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, cacheNow)
		t.Cleanup(func() { providerSubscriberNegativeCache = previousCache })
		ctx := t.Context()
		firstCity := egressTestCity(ctx, "First Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		secondCity := egressTestCity(ctx, "Second Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, firstCity, egressTestFast, nil, nil)
		egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		egressTestPasses(ctx, t)
		var lastToken server.Id
		lastWanted := true
		assertFact := func(want bool, fresh bool) {
			server.Db(ctx, func(conn server.PgConn) {
				var verified bool
				var token server.Id
				server.Raise(conn.QueryRow(ctx, `SELECT arin_quality_verified, arin_quality_write_token
					FROM network_client_location WHERE connection_id=$1`, provider.connectionId).Scan(&verified, &token))
				if verified != want || (token != lastToken) != fresh {
					t.Fatalf("verified=%t want=%t; token changed=%t want=%t", verified, want, token != lastToken, fresh)
				}
				lastToken = token
			}, server.OptReadOnly(), server.OptNoRetry())
			if want && !lastWanted {
				// New evidence may remain negatively cached for at most one
				// second. Revocation in the opposite direction stays immediate.
				cacheClock.Add(time.Second.Nanoseconds())
			}
			lastWanted = want
			excluded, err := getProviderRequestExclusions(ctx, []server.Id{provider.clientId}, RankModeQuality)
			if err != nil || excluded[provider.clientId] == want {
				t.Fatalf("live Quality guard excluded=%t want=%t error=%v", excluded[provider.clientId], !want, err)
			}
			for _, force := range []bool{false, true} {
				found := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &provider.clientId}}, RankModeQuality, 1, force, server.NewId())
				if (len(found) == 1) != want {
					t.Fatalf("named Quality force=%t returned=%d want eligible=%t", force, len(found), want)
				}
			}
		}
		assertFact(true, true)
		for _, city := range []*Location{firstCity, secondCity} {
			// Reclassification of identical facts must still rotate the token.
			if err := SetConnectionLocation(ctx, provider.connectionId, firstCity.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}); err != nil {
				t.Fatal(err)
			}
			assertFact(true, true)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, subscriberLegacyLocationUpsertSql,
					provider.connectionId, provider.clientId, city.CityLocationId, city.RegionLocationId, city.CountryLocationId,
					0, 0, 0, 0, provider.networkId, nil, nil, false, false, nil, int64(0)))
			})
			assertFact(false, false)
			// The old score publication is deliberately still warm here.
			if found := egressTestFind(ctx, t, egressTestLocationSpec(firstCity), RankModeQuality, 1, true, server.NewId()); len(found) != 0 {
				t.Fatal("warm native/fallback Quality cache retained a revoked subscriber")
			}
			egressTestPasses(ctx, t)
			server.Db(ctx, func(conn server.PgConn) {
				var nonQuality bool
				server.Raise(conn.QueryRow(ctx, `SELECT arin_non_quality FROM network_client_location_reliability
					WHERE client_id=$1`, provider.clientId).Scan(&nonQuality))
				if !nonQuality {
					t.Fatal("rollup retained legacy-carried subscriber evidence")
				}
			})
			if found := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &provider.clientId}}, RankModeSpeed, 1, true, server.NewId()); len(found) != 1 {
				t.Fatal("subscriber revocation changed independent Speed eligibility")
			}
		}
		for _, verified := range []bool{true, false, true} {
			if err := SetConnectionLocation(ctx, provider.connectionId, secondCity.LocationId, &ConnectionLocationScores{ArinQualityVerified: verified}); err != nil {
				t.Fatal(err)
			}
			assertFact(verified, true)
		}
	})
}

// An old transaction may begin before a newer classification commits. Its
// later conflict update must invalidate the committed row, not its earlier view.
func TestSubscriberQualityOldTransactionAfterNewAttestation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		cacheClock, cacheNow := subscriberCacheTestClock()
		previousCache := providerSubscriberNegativeCache
		providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, cacheNow)
		t.Cleanup(func() { providerSubscriberNegativeCache = previousCache })
		ctx := t.Context()
		city := egressTestCity(ctx, "Transaction City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var before bool
			server.Raise(tx.QueryRow(ctx, `SELECT arin_quality_verified FROM network_client_location WHERE connection_id=$1`, provider.connectionId).Scan(&before))
			if before {
				t.Fatal("legacy transaction did not begin from unknown evidence")
			}
			if err := SetConnectionLocation(ctx, provider.connectionId, city.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}); err != nil {
				t.Fatal(err)
			}
			server.RaisePgResult(tx.Exec(ctx, subscriberLegacyLocationUpsertSql,
				provider.connectionId, provider.clientId, city.CityLocationId, city.RegionLocationId, city.CountryLocationId,
				0, 0, 0, 0, provider.networkId, nil, nil, false, false, nil, int64(0)))
			server.Raise(tx.Commit(ctx))
		}, server.OptNoRetry())
		excluded, err := getProviderRequestExclusions(ctx, []server.Id{provider.clientId}, RankModeQuality)
		if err != nil || !excluded[provider.clientId] {
			t.Fatalf("older transaction retained newer attestation: excluded=%t error=%v", excluded[provider.clientId], err)
		}
		if err := SetConnectionLocation(ctx, provider.connectionId, city.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}); err != nil {
			t.Fatal(err)
		}
		excluded, err = getProviderRequestExclusions(ctx, []server.Id{provider.clientId}, RankModeQuality)
		if err != nil || !excluded[provider.clientId] {
			t.Fatalf("bounded negative changed before its expiry: excluded=%t error=%v", excluded[provider.clientId], err)
		}
		cacheClock.Add(time.Second.Nanoseconds())
		excluded, err = getProviderRequestExclusions(ctx, []server.Id{provider.clientId}, RankModeQuality)
		if err != nil || excluded[provider.clientId] {
			t.Fatalf("fresh attestation after old commit was not accepted: excluded=%t error=%v", excluded[provider.clientId], err)
		}
	})
}
