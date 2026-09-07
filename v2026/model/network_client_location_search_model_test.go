package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/search"
	// "github.com/urnetwork/server/v2026/jwt"
	// "github.com/urnetwork/server/v2026/session"
)

// TODO
// index locations
// search index to see that locations return correctly

func TestLocationsSearch(t *testing.T) {
	// create locations

	// index locations

	// search around for lcoation and group

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		server.Tx(ctx, func(tx server.PgTx) {

			locationSanFrancisco := &Location{
				LocationType: LocationTypeCity,
				City:         "San Francisco",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			}
			CreateLocation(ctx, locationSanFrancisco)
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Mateo",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Leandro",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Diego",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Luis Obispo",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "Tokyo San Fran",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "Lima San Fran",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})

			locationGroupWhosTheBest := &LocationGroup{
				Name:     "Who's the best",
				Promoted: false,
			}
			CreateLocationGroup(ctx, locationGroupWhosTheBest)
			CreateLocationGroup(ctx, &LocationGroup{
				Name:     "What's the best",
				Promoted: false,
			})
			CreateLocationGroup(ctx, &LocationGroup{
				Name:     "Best who's the",
				Promoted: false,
			})

			IndexSearchLocationsInTx(ctx, tx)

			r1 := locationSearch().AroundIds(ctx, "san fra", 0)
			connect.AssertEqual(t, len(r1), 3)
			connect.AssertEqual(t, 0, r1[locationSanFrancisco.LocationId].ValueDistance)

			r2 := locationSearch().AroundIds(ctx, "san frn", 1)
			connect.AssertEqual(t, len(r2), 3)
			connect.AssertEqual(t, 1, r2[locationSanFrancisco.LocationId].ValueDistance)

			r3 := locationGroupSearch().AroundIds(ctx, "who's the", 0)
			connect.AssertEqual(t, len(r3), 2)
			connect.AssertEqual(t, 0, r3[locationGroupWhosTheBest.LocationGroupId].ValueDistance)

		})

	})

}

func TestIndexSearchLocationsSkipUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		locationSanFrancisco := &Location{
			LocationType: LocationTypeCity,
			City:         "San Francisco",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, locationSanFrancisco)

		locationGroupWhosTheBest := &LocationGroup{
			Name:     "Who's the best",
			Promoted: false,
		}
		CreateLocationGroup(ctx, locationGroupWhosTheBest)

		index := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				IndexSearchLocationsInTx(ctx, tx)
			})
		}

		// max update id and row count of `search_value_update`,
		// and the physical row versions of `search_projection`.
		// `Add`/`Remove` write `search_value_update` on every real write,
		// and a delete plus re-insert always changes the (ctid, xmin) row versions
		searchWriteState := func() (maxUpdateId int64, updateCount int64, projectionRowVersions []string) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT
						COALESCE(MAX(update_id), 0),
						COUNT(*)
					FROM search_value_update
					`,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&maxUpdateId, &updateCount))
					}
				})

				result, err = conn.Query(
					ctx,
					`
					SELECT ctid::text || ':' || xmin::text
					FROM search_projection
					ORDER BY 1
					`,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var rowVersion string
						server.Raise(result.Scan(&rowVersion))
						projectionRowVersions = append(projectionRowVersions, rowVersion)
					}
				})
			})
			return
		}

		index()
		maxUpdateId1, updateCount1, projectionRowVersions1 := searchWriteState()
		connect.AssertEqual(t, true, 0 < len(projectionRowVersions1))

		// nothing changed, so the next index must write nothing
		index()
		maxUpdateId2, updateCount2, projectionRowVersions2 := searchWriteState()
		connect.AssertEqual(t, maxUpdateId1, maxUpdateId2)
		connect.AssertEqual(t, updateCount1, updateCount2)
		connect.AssertEqual(t, projectionRowVersions1, projectionRowVersions2)

		// the skipped values are still searchable
		r1 := locationSearch().AroundIds(ctx, "san francisco, united states", 0)
		_, ok := r1[locationSanFrancisco.LocationId]
		connect.AssertEqual(t, true, ok)
		r2 := locationGroupSearch().AroundIds(ctx, "who's the", 0)
		_, ok = r2[locationGroupWhosTheBest.LocationGroupId]
		connect.AssertEqual(t, true, ok)

		// rename the city, which changes the search strings of the city location only
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE location
				SET location_name = $2
				WHERE location_id = $1
				`,
				locationSanFrancisco.LocationId,
				"Saint Francisco",
			))
		})

		// the changed value must be rewritten
		index()
		maxUpdateId3, updateCount3, projectionRowVersions3 := searchWriteState()
		connect.AssertEqual(t, true, maxUpdateId2 < maxUpdateId3)
		connect.AssertNotEqual(t, projectionRowVersions2, projectionRowVersions3)

		// stored alias 0 of variant 0 is the new normalized search string
		var storedValue string
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT value
				FROM search_value
				WHERE
					realm = $1 AND
					value_id = $2 AND
					value_variant = 0 AND
					alias = 0
				`,
				locationSearch().Realm(),
				locationSanFrancisco.LocationId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&storedValue))
				}
			})
		})
		connect.AssertEqual(t, search.NormalizeForSearch("Saint Francisco, United States"), storedValue)

		// the new name is searchable and the old name is not
		r3 := locationSearch().AroundIds(ctx, "saint francisco, united states", 0)
		_, ok = r3[locationSanFrancisco.LocationId]
		connect.AssertEqual(t, true, ok)
		r4 := locationSearch().AroundIds(ctx, "san francisco, united states", 0)
		_, ok = r4[locationSanFrancisco.LocationId]
		connect.AssertEqual(t, false, ok)

		// nothing changed again, so the next index must write nothing
		index()
		maxUpdateId4, updateCount4, projectionRowVersions4 := searchWriteState()
		connect.AssertEqual(t, maxUpdateId3, maxUpdateId4)
		connect.AssertEqual(t, updateCount3, updateCount4)
		connect.AssertEqual(t, projectionRowVersions3, projectionRowVersions4)
	})
}
