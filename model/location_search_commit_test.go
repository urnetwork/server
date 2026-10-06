package model

// The location and location group searches change their in-memory indexes
// only once the transaction that writes their database index has committed.
// They used to change at the write, before the commit, so a transaction that
// then failed left this process's index holding a location or group the
// database never kept, or missing one it still holds, until a restart. The
// tests install in-memory searches that never load or poll, and a test
// trigger on the search's update log fails the write's transaction at its
// first update record. Each call runs under forcedFailureCallTimeout (see
// forced_statement_failure_test.go).

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/search"
)

// Replaces the location and location group searches with ones over the same
// database realms whose context is canceled before they start, so they never
// load or poll and only the writes under test change their in-memory
// indexes. Returns the func that restores the previous searches.
func inMemoryLocationSearches(ctx context.Context) (restore func()) {
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	inMemoryLocationSearch := search.NewSearchLocalWithDefaults(
		canceledCtx,
		search.NewSearchDbWithMinAliasLength("location_prefix", search.SearchTypePrefix, 3),
	)
	inMemoryLocationGroupSearch := search.NewSearchLocalWithDefaults(
		canceledCtx,
		search.NewSearchDbWithMinAliasLength("location_group_prefix", search.SearchTypePrefix, 3),
	)
	previousLocationSearch := locationSearch
	previousLocationGroupSearch := locationGroupSearch
	locationSearch = func() *search.SearchLocal {
		return inMemoryLocationSearch
	}
	locationGroupSearch = func() *search.SearchLocal {
		return inMemoryLocationGroupSearch
	}
	return func() {
		locationSearch = previousLocationSearch
		locationGroupSearch = previousLocationGroupSearch
	}
}

// Whether the in-memory index holds the id under exactly the search string.
func inMemorySearchHolds(ctx context.Context, searchLocal *search.SearchLocal, searchString string, valueId server.Id) bool {
	_, ok := searchLocal.AroundIds(ctx, searchString, 0)[valueId]
	return ok
}

// A location or location group whose creating transaction fails after its
// search statements leaves the in-memory index without it; one that commits
// is in the index.
func TestLocationSearchesIndexInMemoryAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restoreSearches := inMemoryLocationSearches(ctx)
		defer restoreSearches()

		createCountry := func(callCtx context.Context) server.Id {
			location := &Location{
				LocationType: LocationTypeCountry,
				Country:      "Iceland",
				CountryCode:  "is",
			}
			CreateLocation(callCtx, location)
			return location.LocationId
		}
		createGroup := func(name string) func(callCtx context.Context) server.Id {
			return func(callCtx context.Context) server.Id {
				locationGroup := &LocationGroup{
					Name: name,
				}
				CreateLocationGroup(callCtx, locationGroup)
				return locationGroup.LocationGroupId
			}
		}

		for _, c := range []struct {
			name string
			// the search the write indexes into, read when the case runs
			search       func() *search.SearchLocal
			searchString string
			create       func(callCtx context.Context) server.Id
			rollback     bool
		}{
			{
				name: "location rolled back",
				search: func() *search.SearchLocal {
					return locationSearch()
				},
				searchString: "Iceland (is)",
				create:       createCountry,
				rollback:     true,
			},
			{
				name: "location committed",
				search: func() *search.SearchLocal {
					return locationSearch()
				},
				searchString: "Iceland (is)",
				create:       createCountry,
				rollback:     false,
			},
			{
				name: "location group rolled back",
				search: func() *search.SearchLocal {
					return locationGroupSearch()
				},
				searchString: "Synthetic Rollback Group",
				create:       createGroup("Synthetic Rollback Group"),
				rollback:     true,
			},
			{
				name: "location group committed",
				search: func() *search.SearchLocal {
					return locationGroupSearch()
				},
				searchString: "Synthetic Commit Group",
				create:       createGroup("Synthetic Commit Group"),
				rollback:     false,
			},
		} {
			restore := func() {}
			if c.rollback {
				restore = forceStatementFailures(ctx, "search_value_update", "INSERT")
			}
			var createdId server.Id
			panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
				createdId = c.create(callCtx)
			})
			restore()

			indexed := c.search().AroundIds(ctx, c.searchString, 0)
			if c.rollback {
				if !isForcedFailure(panicValue, "P0001", "") {
					t.Errorf("%s: the create ended with %v, want the forced failure", c.name, panicValue)
				}
				if len(indexed) != 0 {
					t.Errorf("%s: the in-memory index holds %q for %d ids after the rollback, want none", c.name, c.searchString, len(indexed))
				}
				continue
			}
			if panicValue != nil {
				t.Errorf("%s: the create ended with %v", c.name, panicValue)
				continue
			}
			if _, ok := indexed[createdId]; !ok {
				t.Errorf("%s: the in-memory index does not hold %q for the created id after the commit", c.name, c.searchString)
			}
		}
	})
}

// The seeder renames a location keyed by its geoname id after the place list.
// A rename whose transaction fails leaves the location in the in-memory index
// under its old search strings; one that commits moves it to the new ones.
func TestLocationRenameMovesInMemoryAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restoreSearches := inMemoryLocationSearches(ctx)
		defer restoreSearches()

		seedCountry := func(callCtx context.Context, name string) server.Id {
			location := &Location{
				LocationType:     LocationTypeCountry,
				Country:          name,
				CountryCode:      "is",
				CountryGeonameId: 2629691,
			}
			seedLocation(callCtx, location)
			return location.LocationId
		}
		countryId := seedCountry(ctx, "Iceland")
		if !inMemorySearchHolds(ctx, locationSearch(), "Iceland (is)", countryId) {
			t.Fatalf("the in-memory index does not hold the seeded country")
		}

		restore := forceStatementFailures(ctx, "search_value_update", "INSERT")
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			seedCountry(callCtx, "Island")
		})
		restore()
		if !isForcedFailure(panicValue, "P0001", "") {
			t.Fatalf("the rename ended with %v, want the forced failure", panicValue)
		}
		if !inMemorySearchHolds(ctx, locationSearch(), "Iceland (is)", countryId) {
			t.Errorf("the in-memory index dropped the old name after the rename rolled back")
		}
		if inMemorySearchHolds(ctx, locationSearch(), "Island (is)", countryId) {
			t.Errorf("the in-memory index holds the new name after the rename rolled back")
		}

		if renamedId := seedCountry(ctx, "Island"); renamedId != countryId {
			t.Fatalf("the rename resolved to %s, want the country %s", renamedId, countryId)
		}
		if !inMemorySearchHolds(ctx, locationSearch(), "Island (is)", countryId) {
			t.Errorf("the in-memory index does not hold the new name after the rename committed")
		}
		if inMemorySearchHolds(ctx, locationSearch(), "Iceland (is)", countryId) {
			t.Errorf("the in-memory index still holds the old name after the rename committed")
		}
	})
}

// The index task rewrites a location whose search strings changed. An index
// whose transaction fails leaves the location in the in-memory index under
// its old search strings; one that commits moves it to the new ones.
func TestLocationReindexMovesInMemoryAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restoreSearches := inMemoryLocationSearches(ctx)
		defer restoreSearches()

		location := &Location{
			LocationType: LocationTypeCountry,
			Country:      "Norway",
			CountryCode:  "no",
		}
		CreateLocation(ctx, location)
		if !inMemorySearchHolds(ctx, locationSearch(), "Norway (no)", location.LocationId) {
			t.Fatalf("the in-memory index does not hold the created country")
		}
		// a rename that only the index brings to the search
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE location SET location_name = $2 WHERE location_id = $1`,
				location.LocationId,
				"Norge",
			))
		})
		// as the index task does
		index := func(callCtx context.Context) {
			var posts []server.PostFunction
			server.Tx(callCtx, func(tx server.PgTx) {
				posts = IndexSearchLocationsInTx(callCtx, tx)
			})
			server.RunPosts(callCtx, server.SequencePosts(posts...))
		}

		restore := forceStatementFailures(ctx, "search_value_update", "INSERT")
		panicValue := callWithForcedFailure(ctx, index)
		restore()
		if !isForcedFailure(panicValue, "P0001", "") {
			t.Fatalf("the index ended with %v, want the forced failure", panicValue)
		}
		if !inMemorySearchHolds(ctx, locationSearch(), "Norway (no)", location.LocationId) {
			t.Errorf("the in-memory index dropped the old name after the index rolled back")
		}
		if inMemorySearchHolds(ctx, locationSearch(), "Norge (no)", location.LocationId) {
			t.Errorf("the in-memory index holds the new name after the index rolled back")
		}

		index(ctx)
		if !inMemorySearchHolds(ctx, locationSearch(), "Norge (no)", location.LocationId) {
			t.Errorf("the in-memory index does not hold the new name after the index committed")
		}
		if inMemorySearchHolds(ctx, locationSearch(), "Norway (no)", location.LocationId) {
			t.Errorf("the in-memory index still holds the old name after the index committed")
		}
	})
}

// Deduplication deletes the duplicate rows it merges and removes them from
// the location search. A deletion whose transaction fails leaves the
// duplicate in the in-memory index, with its row; one that commits takes it
// out.
func TestLocationDeduplicationRemovesFromMemoryAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()
		restoreSearches := inMemoryLocationSearches(ctx)
		defer restoreSearches()

		canonical := &Location{
			LocationType:     LocationTypeCity,
			City:             "Dorking",
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			CityGeonameId:    2651095,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, canonical)
		// the old sources' spelling of the region, side by side with the
		// canonical row and in the search
		duplicateRegionId := insertTestLocation(ctx, LocationTypeRegion, "ENGLAND", "gb", canonical.CountryLocationId, nil, 0)
		locationSearch().Add(ctx, "ENGLAND, United Kingdom", duplicateRegionId, 0)

		restore := forceStatementFailures(ctx, "search_value_update", "INSERT")
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			_, err := DeduplicateLocations(callCtx)
			server.Raise(err)
		})
		restore()
		if !isForcedFailure(panicValue, "P0001", "") {
			t.Fatalf("the deduplication ended with %v, want the forced failure", panicValue)
		}
		if !testLocationExists(ctx, duplicateRegionId) {
			t.Fatalf("the failed deduplication deleted the duplicate row")
		}
		if !inMemorySearchHolds(ctx, locationSearch(), "ENGLAND, United Kingdom", duplicateRegionId) {
			t.Errorf("the in-memory index dropped the duplicate after its deletion rolled back")
		}

		deduplication, err := DeduplicateLocations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if deduplication.RegionMergedRowCount != 1 || testLocationExists(ctx, duplicateRegionId) {
			t.Fatalf("the deduplication merged %d region rows, want the duplicate deleted", deduplication.RegionMergedRowCount)
		}
		if inMemorySearchHolds(ctx, locationSearch(), "ENGLAND, United Kingdom", duplicateRegionId) {
			t.Errorf("the in-memory index still holds the duplicate after its deletion committed")
		}
		if !inMemorySearchHolds(ctx, locationSearch(), "England, United Kingdom", canonical.RegionLocationId) {
			t.Errorf("the in-memory index lost the canonical region")
		}
	})
}
