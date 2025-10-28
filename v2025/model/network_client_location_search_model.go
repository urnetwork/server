package model

import (
	"context"
	"sync"
	// "time"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/search"
	// "github.com/urnetwork/server/v2025/session"
	// "github.com/urnetwork/server/v2025/task"
)

func init() {
	server.OnWarmup(func() {
		locationSearch()
		//.WaitForInitialSync(context.Background())
		locationGroupSearch()
		//.WaitForInitialSync(context.Background())
	})
	server.OnReset(func() {
		locationSearch().Close()
		locationGroupSearch().Close()
		locationSearch = sync.OnceValue(createLocationSearch)
		locationGroupSearch = sync.OnceValue(createLocationGroupSearch)
	})
}

func createLocationSearch() *search.SearchLocal {
	return search.NewSearchLocalWithDefaults(
		context.Background(),
		search.NewSearchDbWithMinAliasLength("location_prefix", search.SearchTypePrefix, 3),
	)
}

func createLocationGroupSearch() *search.SearchLocal {
	return search.NewSearchLocalWithDefaults(
		context.Background(),
		search.NewSearchDbWithMinAliasLength("location_group_prefix", search.SearchTypePrefix, 3),
	)
}

var locationSearch = sync.OnceValue(createLocationSearch)
var locationGroupSearch = sync.OnceValue(createLocationGroupSearch)

func IndexSearchLocationsInTx(ctx context.Context, tx server.PgTx) {
	// locations
	result, err := tx.Query(ctx,
		`
		    SELECT
		    	location.location_id,
		    	location.location_type,
		    	location.country_code,

		    	location_city.location_id AS city_location_id,
		    	location_city.location_name AS city,

		    	location_region.location_id AS region_location_id,
		    	location_region.location_name AS region,

		    	location_country.location_id AS country_location_id,
		    	location_country.location_name AS country

		    FROM location

		    LEFT JOIN location location_city ON location_city.location_id = location.city_location_id
		    LEFT JOIN location location_region ON location_region.location_id = location.region_location_id
		    LEFT JOIN location location_country ON location_country.location_id = location.country_location_id
		`,
	)
	locations := map[server.Id]*Location{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			location := Location{}
			var cityLocationId *server.Id
			var city *string
			var regionLocationId *server.Id
			var region *string
			var countryLocationId *server.Id
			var country *string
			result.Scan(
				&location.LocationId,
				&location.LocationType,
				&location.CountryCode,
				&cityLocationId,
				&city,
				&regionLocationId,
				&region,
				&countryLocationId,
				&country,
			)
			if cityLocationId != nil {
				location.CityLocationId = *cityLocationId
				location.City = *city
			}
			if regionLocationId != nil {
				location.RegionLocationId = *regionLocationId
				location.Region = *region
			}
			if countryLocationId != nil {
				location.CountryLocationId = *countryLocationId
				location.Country = *country
			}
			locations[location.LocationId] = &location
		}
	})
	locationIds := maps.Keys(locations)
	for i, locationId := range locationIds {
		location := locations[locationId]
		locationSearch().RemoveInTx(ctx, locationId, tx)
		searchStrings := location.SearchStrings()
		for j, searchStr := range searchStrings {
			locationSearch().AddInTx(ctx, searchStr, locationId, j, tx)
			glog.Infof("[location]index %d/%d %d/%d: %s\n", i+1, len(locationIds), j+1, len(searchStrings), searchStr)
		}
	}

	// location groups
	locationGroupMembers := map[server.Id][]server.Id{}
	result, err = tx.Query(ctx,
		`
		    SELECT
		    	location_group_id,
		    	location_id
		    FROM location_group_member
		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var locationGroupId server.Id
			var locationId server.Id
			result.Scan(&locationGroupId, &locationId)
			locationGroupMembers[locationGroupId] = append(locationGroupMembers[locationGroupId], locationId)
		}
	})

	result, err = tx.Query(
		ctx,
		`
		    SELECT
		    	location_group_id,
		    	location_group_name,
		    	promoted
		    FROM location_group
		`,
	)
	locationGroups := map[server.Id]*LocationGroup{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			locationGroup := LocationGroup{}
			result.Scan(
				&locationGroup.LocationGroupId,
				&locationGroup.Name,
				&locationGroup.Promoted,
			)
			locationGroup.MemberLocationIds = locationGroupMembers[locationGroup.LocationGroupId]
			locationGroups[locationGroup.LocationGroupId] = &locationGroup
		}
	})
	locationGroupIds := maps.Keys(locationGroups)
	for i, locationGroupId := range locationGroupIds {
		locationGroup := locationGroups[locationGroupId]
		locationGroupSearch().RemoveInTx(ctx, locationGroupId, tx)
		searchStrings := locationGroup.SearchStrings()
		for j, searchStr := range searchStrings {
			locationGroupSearch().AddInTx(ctx, searchStr, locationGroupId, j, tx)
			glog.Infof("[location]index group %d/%d %d/%d: %s\n", i+1, len(locationGroupIds), j+1, len(searchStrings), searchStr)
		}
	}
}
