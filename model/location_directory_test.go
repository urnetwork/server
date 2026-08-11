package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// testingInsertLocationReliability inserts one reliability row with the given
// location chain. Any of the ids may be the zero Id, which is written as NULL —
// the columns are nullable and the directory query must skip those.
func testingInsertLocationReliability(
	ctx context.Context,
	clientId server.Id,
	cityLocationId server.Id,
	regionLocationId server.Id,
	countryLocationId server.Id,
) {
	nullable := func(id server.Id) any {
		if id == (server.Id{}) {
			return nil
		}
		return id
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_client_location_reliability (
				client_id, update_block_number,
				city_location_id, region_location_id, country_location_id
			)
			VALUES ($1, $2, $3, $4, $5)
			`,
			clientId,
			1,
			nullable(cityLocationId),
			nullable(regionLocationId),
			nullable(countryLocationId),
		))
	})
}

// testingReferencedLocationIds runs a sql fragment that yields the referenced
// location id set, and returns it as a set.
func testingReferencedLocationIds(ctx context.Context, t testing.TB, sql string) map[server.Id]bool {
	ids := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, sql)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				ids[id] = true
			}
		})
	})
	return ids
}

// The location directory bounds itself to locations actually referenced by
// providers. That referenced set used to be collected as three separate
// `SELECT DISTINCT <col>` branches UNIONed together, which the planner ran as
// three independent parallel seq scans of the whole ~53M-row reliability table
// (2026-08-11: ~2% of all db time, 9.3B buffers). It is now collected as
// DISTINCT triples in ONE pass and unnested.
//
// This pins the rewrite: both forms must yield exactly the same set, including
// the cases that make set equality non-obvious — nulls in any column, an id that
// appears in more than one column, and ids that repeat across many rows.
func TestLocationDirectoryReferencedSetMatchesUnionForm(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		cityId := server.NewId()
		regionId := server.NewId()
		countryId := server.NewId()
		// an id used in two different columns: a city in one row that is also a
		// region in another
		sharedId := server.NewId()
		otherCountryId := server.NewId()

		// a full chain, repeated so the distinct-ness actually matters
		testingInsertLocationReliability(ctx, server.NewId(), cityId, regionId, countryId)
		testingInsertLocationReliability(ctx, server.NewId(), cityId, regionId, countryId)
		// partial chains: city-only and country-only, so nulls appear in every column
		testingInsertLocationReliability(ctx, server.NewId(), sharedId, server.Id{}, server.Id{})
		testingInsertLocationReliability(ctx, server.NewId(), server.Id{}, sharedId, otherCountryId)
		// an all-null row must contribute nothing
		testingInsertLocationReliability(ctx, server.NewId(), server.Id{}, server.Id{}, server.Id{})

		unionForm := `
			SELECT DISTINCT city_location_id
			FROM network_client_location_reliability
			WHERE city_location_id IS NOT NULL

			UNION

			SELECT DISTINCT region_location_id
			FROM network_client_location_reliability
			WHERE region_location_id IS NOT NULL

			UNION

			SELECT DISTINCT country_location_id
			FROM network_client_location_reliability
			WHERE country_location_id IS NOT NULL
		`
		// the shape queryLocationDirectory uses
		tripleForm := `
			SELECT DISTINCT loc.id
			FROM (
				SELECT DISTINCT
					city_location_id AS c,
					region_location_id AS r,
					country_location_id AS n
				FROM network_client_location_reliability
			) triples
			CROSS JOIN LATERAL unnest(ARRAY[triples.c, triples.r, triples.n]) AS loc(id)
			WHERE loc.id IS NOT NULL
		`

		union := testingReferencedLocationIds(ctx, t, unionForm)
		triple := testingReferencedLocationIds(ctx, t, tripleForm)

		// the fixture is meaningful only if it actually produced the ids
		for _, id := range []server.Id{cityId, regionId, countryId, sharedId, otherCountryId} {
			if !union[id] {
				t.Fatalf("fixture did not reach the union form: %s missing", id)
			}
		}
		connect.AssertEqual(t, len(triple), len(union))
		for id := range union {
			if !triple[id] {
				t.Fatalf("triple form is missing %s that the union form returns", id)
			}
		}
		for id := range triple {
			if !union[id] {
				t.Fatalf("triple form returns %s that the union form does not", id)
			}
		}
	})
}

// The directory is shared across the fleet through redis so the scan is paid
// once per staleness window instead of once per process per window (before
// 2026-08-11 every process ran it on its own timer: ~1,550 executions per 48h).
// A load must publish what it computed, and a later load must be able to take it
// without touching pg.
func TestLocationDirectoryIsSharedThroughRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "US",
		}
		CreateLocation(ctx, city)
		testingInsertLocationReliability(
			ctx,
			server.NewId(),
			city.CityLocationId,
			city.RegionLocationId,
			city.CountryLocationId,
		)

		// nothing published yet
		resetLocationDirectory()
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, locationDirectoryRedisKey).Err())
		})
		connect.AssertEqual(t, getLocationDirectoryCache(ctx) == nil, true)

		// a load computes and publishes
		loadLocationDirectory()
		computed := locationDirectory()
		if len(computed) == 0 {
			t.Fatal("loadLocationDirectory produced an empty directory")
		}

		// what another process would read, without querying pg
		shared := getLocationDirectoryCache(ctx)
		if shared == nil {
			t.Fatal("loadLocationDirectory did not publish the directory for the fleet")
		}
		connect.AssertEqual(t, len(shared), len(computed))
		for locationId, entry := range computed {
			sharedEntry, ok := shared[locationId]
			if !ok {
				t.Fatalf("shared directory is missing %s", locationId)
			}
			connect.AssertEqual(t, sharedEntry.Name, entry.Name)
			// the country code is lowercased on the way in and must survive the
			// round trip, since callers compare it lowercased
			connect.AssertEqual(t, sharedEntry.CountryCode, entry.CountryCode)
		}

		// the city chain is present and lowercased, which is what the directory
		// is consumed for
		cityEntry, ok := shared[city.CityLocationId]
		if !ok {
			t.Fatalf("shared directory is missing the city %s", city.CityLocationId)
		}
		connect.AssertEqual(t, cityEntry.CountryCode, "us")
	})
}

// A redis miss must fall back to pg rather than serving an empty directory —
// that is the pre-cache behavior and the safe failure mode.
func TestLocationDirectoryFallsBackWhenCacheMissing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Reykjavik",
			Region:       "Capital Region",
			Country:      "Iceland",
			CountryCode:  "IS",
		}
		CreateLocation(ctx, city)
		testingInsertLocationReliability(
			ctx,
			server.NewId(),
			city.CityLocationId,
			city.RegionLocationId,
			city.CountryLocationId,
		)

		resetLocationDirectory()
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, locationDirectoryRedisKey).Err())
		})

		loadLocationDirectory()

		entries := locationDirectory()
		if _, ok := entries[city.CityLocationId]; !ok {
			t.Fatalf("directory did not fall back to pg on a cache miss; %s missing", city.CityLocationId)
		}
	})
}
