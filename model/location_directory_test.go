package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// A non-blocking directory refresh can outlive TestEnv teardown. Force its
// cache read to remain in flight until reset cancels the captured generation.
// It must not continue into the replacement database/shared-cache stages,
// publish a snapshot, or clear the replacement generation's load claim.
func TestLocationDirectoryResetCancelsInFlightLoad(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()

	state := newLocationDirectoryState()
	oldGeneration, started := state.startLoad()
	if !started {
		t.Fatal("old generation did not acquire the load claim")
	}

	oldLocationId := server.NewId()
	newLocationId := server.NewId()
	oldEntries := map[server.Id]*locationDirectoryEntry{
		oldLocationId: {
			Name:        "Old Fixture City",
			CountryCode: "xo",
		},
	}
	newEntries := map[server.Id]*locationDirectoryEntry{
		newLocationId: {
			Name:        "New Fixture City",
			CountryCode: "xn",
		},
	}

	oldLoadStarted := make(chan struct{})
	oldLoadDone := make(chan struct{})
	databaseTouched := make(chan struct{}, 1)
	sharedCacheTouched := make(chan struct{}, 1)
	go func() {
		defer close(oldLoadDone)
		defer state.finishLoad(oldGeneration)
		loadLocationDirectoryForGenerationWith(
			state,
			oldGeneration,
			func(ctx context.Context) map[server.Id]*locationDirectoryEntry {
				close(oldLoadStarted)
				<-ctx.Done()
				return nil
			},
			func(context.Context) map[server.Id]*locationDirectoryEntry {
				databaseTouched <- struct{}{}
				return oldEntries
			},
			func(context.Context, map[server.Id]*locationDirectoryEntry, time.Duration) {
				sharedCacheTouched <- struct{}{}
			},
		)
	}()
	select {
	case <-oldLoadStarted:
	case <-testCtx.Done():
		t.Fatalf("old generation did not enter the cache read: %v", testCtx.Err())
	}

	state.reset()
	select {
	case <-oldGeneration.ctx.Done():
	default:
		t.Fatal("reset did not cancel the old generation")
	}
	newGeneration, started := state.startLoad()
	if !started {
		t.Fatal("new generation did not acquire the load claim")
	}
	if !state.publish(newGeneration, newEntries) {
		t.Fatal("current generation did not publish")
	}

	select {
	case <-oldLoadDone:
	case <-testCtx.Done():
		t.Fatalf("canceled old generation did not return: %v", testCtx.Err())
	}
	select {
	case <-databaseTouched:
		t.Fatal("obsolete generation touched the database after reset")
	default:
	}
	select {
	case <-sharedCacheTouched:
		t.Fatal("obsolete generation touched the shared cache after reset")
	default:
	}
	if state.loading.Load() != newGeneration {
		t.Fatal("obsolete generation cleared the current load claim")
	}
	snapshot := state.snapshot.Load()
	if snapshot == nil {
		t.Fatal("current generation did not publish a snapshot")
	}
	if _, ok := snapshot.entries[newLocationId]; !ok {
		t.Fatal("current generation snapshot was replaced")
	}
	if _, ok := snapshot.entries[oldLocationId]; ok {
		t.Fatal("obsolete generation entered the current snapshot")
	}

	state.finishLoad(newGeneration)
	if state.loading.Load() != nil {
		t.Fatal("current generation did not release its load claim")
	}
}

// Cancellation cannot forcibly stop an external callback that has already
// entered its operation. Hold the old generation inside that database stage,
// reset and publish a replacement generation, then release the old result. The
// post-operation generation fence must discard it before either publication.
func TestLocationDirectoryResetFencesInFlightDatabaseResult(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()

	state := newLocationDirectoryState()
	oldGeneration, started := state.startLoad()
	if !started {
		t.Fatal("old generation did not acquire the load claim")
	}
	oldLocationId := server.NewId()
	newLocationId := server.NewId()
	oldEntries := map[server.Id]*locationDirectoryEntry{
		oldLocationId: {
			Name:        "Retired Fixture City",
			CountryCode: "xr",
		},
	}
	newEntries := map[server.Id]*locationDirectoryEntry{
		newLocationId: {
			Name:        "Replacement Fixture City",
			CountryCode: "xp",
		},
	}

	databaseStarted := make(chan struct{})
	releaseDatabase := make(chan struct{})
	oldLoadDone := make(chan struct{})
	sharedCacheTouched := make(chan struct{}, 1)
	released := false
	defer func() {
		if !released {
			close(releaseDatabase)
		}
	}()
	go func() {
		defer close(oldLoadDone)
		defer state.finishLoad(oldGeneration)
		loadLocationDirectoryForGenerationWith(
			state,
			oldGeneration,
			func(context.Context) map[server.Id]*locationDirectoryEntry { return nil },
			func(context.Context) map[server.Id]*locationDirectoryEntry {
				close(databaseStarted)
				<-releaseDatabase
				return oldEntries
			},
			func(context.Context, map[server.Id]*locationDirectoryEntry, time.Duration) {
				sharedCacheTouched <- struct{}{}
			},
		)
	}()
	select {
	case <-databaseStarted:
	case <-testCtx.Done():
		t.Fatalf("old generation did not enter the database read: %v", testCtx.Err())
	}

	state.reset()
	newGeneration, started := state.startLoad()
	if !started {
		t.Fatal("new generation did not acquire the load claim")
	}
	if !state.publish(newGeneration, newEntries) {
		t.Fatal("current generation did not publish")
	}

	close(releaseDatabase)
	released = true
	select {
	case <-oldLoadDone:
	case <-testCtx.Done():
		t.Fatalf("obsolete database result did not return: %v", testCtx.Err())
	}
	select {
	case <-sharedCacheTouched:
		t.Fatal("obsolete database result was written to the shared cache")
	default:
	}
	if state.loading.Load() != newGeneration {
		t.Fatal("obsolete database result cleared the current load claim")
	}
	snapshot := state.snapshot.Load()
	if snapshot == nil {
		t.Fatal("current generation snapshot is missing")
	}
	if _, ok := snapshot.entries[newLocationId]; !ok {
		t.Fatal("obsolete database result replaced the current snapshot")
	}
	if _, ok := snapshot.entries[oldLocationId]; ok {
		t.Fatal("obsolete database result entered the current snapshot")
	}

	state.finishLoad(newGeneration)
	if state.loading.Load() != nil {
		t.Fatal("current generation did not release its load claim")
	}
}

// Publishing the immutable local snapshot precedes the optional Redis write.
// Hold that write behind a channel and prove both the reader-visible snapshot
// and reset complete before it is released; no wall-clock scheduling luck or
// external service is involved.
func TestLocationDirectorySharedCacheWriteDoesNotBlockReadersOrReset(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()

	state := newLocationDirectoryState()
	generation, started := state.startLoad()
	if !started {
		t.Fatal("generation did not acquire the load claim")
	}
	locationId := server.NewId()
	entries := map[server.Id]*locationDirectoryEntry{
		locationId: {
			Name:        "Synthetic Fixture City",
			CountryCode: "xs",
		},
	}
	sharedWriteStarted := make(chan struct{})
	sharedWriteCanceled := make(chan struct{})
	loadDone := make(chan struct{})
	go func() {
		defer close(loadDone)
		defer state.finishLoad(generation)
		loadLocationDirectoryForGenerationWith(
			state,
			generation,
			func(context.Context) map[server.Id]*locationDirectoryEntry { return nil },
			func(context.Context) map[server.Id]*locationDirectoryEntry { return entries },
			func(ctx context.Context, _ map[server.Id]*locationDirectoryEntry, _ time.Duration) {
				close(sharedWriteStarted)
				<-ctx.Done()
				close(sharedWriteCanceled)
			},
		)
	}()
	select {
	case <-sharedWriteStarted:
	case <-testCtx.Done():
		t.Fatalf("shared cache write did not start: %v", testCtx.Err())
	}

	snapshot := state.snapshot.Load()
	if snapshot == nil || snapshot.entries[locationId] == nil {
		t.Fatal("local directory snapshot was not readable during shared cache write")
	}
	resetDone := make(chan struct{})
	go func() {
		state.reset()
		close(resetDone)
	}()
	select {
	case <-resetDone:
	case <-testCtx.Done():
		t.Fatalf("reset blocked behind shared cache I/O: %v", testCtx.Err())
	}
	if state.snapshot.Load() != nil {
		t.Fatal("reset retained the preceding generation's snapshot")
	}

	select {
	case <-sharedWriteCanceled:
	case <-testCtx.Done():
		t.Fatalf("reset did not cancel the shared cache write: %v", testCtx.Err())
	}
	select {
	case <-loadDone:
	case <-testCtx.Done():
		t.Fatalf("shared cache writer did not return: %v", testCtx.Err())
	}
}

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
	testingInsertLocationReliabilityConnected(
		ctx,
		clientId,
		cityLocationId,
		regionLocationId,
		countryLocationId,
		true,
	)
}

func testingInsertLocationReliabilityConnected(
	ctx context.Context,
	clientId server.Id,
	cityLocationId server.Id,
	regionLocationId server.Id,
	countryLocationId server.Id,
	connected bool,
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
				city_location_id, region_location_id, country_location_id,
				client_address_hash_count, location_count, connected
			)
			VALUES ($1, $2, $3, $4, $5, 1, 1, $6)
			`,
			clientId,
			1,
			nullable(cityLocationId),
			nullable(regionLocationId),
			nullable(countryLocationId),
			connected,
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
			WHERE valid AND connected AND city_location_id IS NOT NULL

			UNION

			SELECT DISTINCT region_location_id
			FROM network_client_location_reliability
			WHERE valid AND connected AND region_location_id IS NOT NULL

			UNION

			SELECT DISTINCT country_location_id
			FROM network_client_location_reliability
			WHERE valid AND connected AND country_location_id IS NOT NULL
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
				WHERE valid AND connected
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

func TestLocationDirectoryExcludesDisconnectedHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		live := &Location{
			LocationType: LocationTypeCity,
			City:         "Live City",
			Region:       "Live Region",
			Country:      "Live Country",
			CountryCode:  "LC",
		}
		CreateLocation(ctx, live)
		historical := &Location{
			LocationType: LocationTypeCity,
			City:         "Historical City",
			Region:       "Historical Region",
			Country:      "Historical Country",
			CountryCode:  "HC",
		}
		CreateLocation(ctx, historical)

		testingInsertLocationReliabilityConnected(
			ctx, server.NewId(), live.CityLocationId, live.RegionLocationId, live.CountryLocationId, true,
		)
		testingInsertLocationReliabilityConnected(
			ctx, server.NewId(), historical.CityLocationId, historical.RegionLocationId, historical.CountryLocationId, false,
		)

		entries := queryLocationDirectory(ctx)
		for _, id := range []server.Id{live.CityLocationId, live.RegionLocationId, live.CountryLocationId} {
			if _, ok := entries[id]; !ok {
				t.Fatalf("live directory is missing %s", id)
			}
		}
		for _, id := range []server.Id{historical.CityLocationId, historical.RegionLocationId, historical.CountryLocationId} {
			if _, ok := entries[id]; ok {
				t.Fatalf("directory retained disconnected historical location %s", id)
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
