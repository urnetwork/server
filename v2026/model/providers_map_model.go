package model

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/urnetwork/server/v2026"
)

// Static admin-1 (region) + country centroids used to place provider-density
// dots on the /ip map. The location table stores no coordinates, so region
// coordinates come from this embedded dataset (keyed by lowercased country
// code + region name/native/subdivision-code, with a country-centroid fallback).
// Regenerate from a public states/provinces dataset if regions are missing.
//
//go:embed region_centroids.json
var regionCentroidsJson []byte

type regionCentroids struct {
	// country code -> region key -> [lat, lon]
	Regions map[string]map[string][2]float64 `json:"regions"`
	// country code -> [lat, lon] (fallback when the region is unknown)
	Countries map[string][2]float64 `json:"countries"`
}

var loadRegionCentroids = sync.OnceValue(func() *regionCentroids {
	var c regionCentroids
	if err := json.Unmarshal(regionCentroidsJson, &c); err != nil {
		panic(fmt.Errorf("failed to parse region_centroids.json: %w", err))
	}
	return &c
})

// centroidFor returns a representative lat/lon for a (countryCode, region),
// trying the region centroid first and falling back to the country centroid.
func centroidFor(countryCode string, region string) (lat float64, lon float64, ok bool) {
	c := loadRegionCentroids()
	cc := strings.ToLower(strings.TrimSpace(countryCode))
	if byRegion, found := c.Regions[cc]; found {
		if ll, found := byRegion[strings.ToLower(strings.TrimSpace(region))]; found {
			return ll[0], ll[1], true
		}
	}
	if ll, found := c.Countries[cc]; found {
		return ll[0], ll[1], true
	}
	return 0, 0, false
}

// RegionProviders is the per-region entry in the providers map.
type RegionProviders struct {
	ProviderCount int     `json:"provider_count"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
}

// regionProviderCount is a minimal input row for buildProvidersMap, kept
// separate from ClientLocation so the reshaping logic is unit-testable.
type regionProviderCount struct {
	CountryCode string
	Region      string
	Count       int
}

// buildProvidersMap reshapes region provider counts into
// country code -> region -> {provider_count, lat, lon}, attaching a centroid to
// each region. Regions with no known centroid (region or country) are skipped,
// and duplicate (country, region) rows are summed.
func buildProvidersMap(rows []regionProviderCount) map[string]map[string]*RegionProviders {
	out := map[string]map[string]*RegionProviders{}
	for _, row := range rows {
		if row.Count <= 0 || row.Region == "" || row.CountryCode == "" {
			continue
		}
		lat, lon, ok := centroidFor(row.CountryCode, row.Region)
		if !ok {
			continue
		}
		byRegion, found := out[row.CountryCode]
		if !found {
			byRegion = map[string]*RegionProviders{}
			out[row.CountryCode] = byRegion
		}
		if existing, found := byRegion[row.Region]; found {
			existing.ProviderCount += row.Count
		} else {
			byRegion[row.Region] = &RegionProviders{ProviderCount: row.Count, Lat: lat, Lon: lon}
		}
	}
	return out
}

// GetProvidersMap aggregates connected, valid, scored providers by
// country -> region, attaching a representative centroid to each region.
//
// This queries the reliability tables directly — the same population
// UpdateClientLocations counts — NOT the InitialClientLocations snapshot. That
// snapshot only ever contains COUNTRY entries (it seeds the connect app's
// initial location list; see the LocationTypeCountry filter where it is built),
// so reading it and filtering for region rows matched nothing and the exported
// map was permanently `{}` in every environment.
func GetProvidersMap(ctx context.Context) (map[string]map[string]*RegionProviders, error) {
	rows := []regionProviderCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					location.country_code,
					location.location_name,
					COUNT(DISTINCT network_client_location_reliability.client_id) AS provider_count

				FROM network_client_location_reliability

				INNER JOIN client_connection_reliability_score ON
					client_connection_reliability_score.client_id = network_client_location_reliability.client_id AND
					client_connection_reliability_score.lookback_index = 0

				INNER JOIN location ON
					location.location_id = network_client_location_reliability.region_location_id

				WHERE
					network_client_location_reliability.connected = true AND
					network_client_location_reliability.valid = true

				GROUP BY location.country_code, location.location_name
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row regionProviderCount
				server.Raise(result.Scan(&row.CountryCode, &row.Region, &row.Count))
				rows = append(rows, row)
			}
		})
	})

	return buildProvidersMap(rows), nil
}

const providersMapRedisKey = "stats.providers-map"

// ExportProvidersMap computes the providers map and stores it in redis under
// stats.providers-map (no ttl), mirroring ExportStats. Served by the thin
// StatsProvidersMap handler.
func ExportProvidersMap(ctx context.Context) error {
	providersMap, err := GetProvidersMap(ctx)
	if err != nil {
		return err
	}
	providersMapJson, err := json.Marshal(providersMap)
	if err != nil {
		return err
	}
	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.Set(ctx, providersMapRedisKey, providersMapJson, 0).Result()
		server.Raise(err)
	})
	return nil
}

// GetExportedProvidersMapJson reads the cached providers-map blob, or nil if it
// has not been exported yet.
func GetExportedProvidersMapJson(ctx context.Context) *string {
	var providersMapJson *string
	server.Redis(ctx, func(client server.RedisClient) {
		value, err := client.Get(ctx, providersMapRedisKey).Result()
		if err == nil {
			providersMapJson = &value
		}
	})
	return providersMapJson
}
