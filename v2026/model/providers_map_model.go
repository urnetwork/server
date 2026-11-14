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
//
// ExtenderCount is always present, never omitted: a region the feed reports
// with no extender_count at all is an old blob, and the site reads that as
// zero, so a present zero and an absent field must not mean different things
// (connect/EXTENDER.md M8).
type RegionProviders struct {
	ProviderCount int     `json:"provider_count"`
	ExtenderCount int     `json:"extender_count"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
}

// regionProviderCount is a minimal input row for buildProvidersMap, kept
// separate from ClientLocation so the reshaping logic is unit-testable. The
// same shape carries the extender aggregate, whose Region is the extender's
// region location name — or, for an extender located only to its country, that
// country location's own name, which centroidFor resolves to the country
// centroid.
type regionProviderCount struct {
	CountryCode string
	Region      string
	Count       int
}

// buildProvidersMap merges the two aggregates into
// country code -> region -> {provider_count, extender_count, lat, lon},
// attaching a centroid to each region. Regions with no known centroid (region
// or country) are skipped, and duplicate (country, region) rows are summed.
//
// The two aggregates are independent populations over the same key: a region
// with extenders and no providers is in the map with provider_count 0, and a
// region with providers and no extenders with extender_count 0. Neither side
// can drop a region the other found.
func buildProvidersMap(
	providerRows []regionProviderCount,
	extenderRows []regionProviderCount,
) map[string]map[string]*RegionProviders {
	out := map[string]map[string]*RegionProviders{}
	entry := func(row regionProviderCount) *RegionProviders {
		if row.Count <= 0 || row.Region == "" || row.CountryCode == "" {
			return nil
		}
		lat, lon, ok := centroidFor(row.CountryCode, row.Region)
		if !ok {
			return nil
		}
		byRegion, found := out[row.CountryCode]
		if !found {
			byRegion = map[string]*RegionProviders{}
			out[row.CountryCode] = byRegion
		}
		existing, found := byRegion[row.Region]
		if !found {
			existing = &RegionProviders{Lat: lat, Lon: lon}
			byRegion[row.Region] = existing
		}
		return existing
	}
	for _, row := range providerRows {
		if region := entry(row); region != nil {
			region.ProviderCount += row.Count
		}
	}
	for _, row := range extenderRows {
		if region := entry(row); region != nil {
			region.ExtenderCount += row.Count
		}
	}
	return out
}

// getOnlineExtendersByRegion aggregates the online extender population (M2:
// active, with at least one active address) by the location its last
// activation resolved (M1).
//
// An extender located to a region is keyed under the region's name. One
// located only to its country is keyed under the country location's own name,
// which centroidFor falls back to the country centroid for, so it is counted
// and hoverable rather than dropped. One with no location at all has no place
// on a map and is left out; it still counts in the by-country gauge and the
// population total.
func getOnlineExtendersByRegion(ctx context.Context, conn server.PgConn) []regionProviderCount {
	rows := []regionProviderCount{}
	result, err := conn.Query(
		ctx,
		`
			SELECT
				COALESCE(region.country_code, country.country_code),
				COALESCE(region.location_name, country.location_name),
				COUNT(*)

			FROM network_extender

			LEFT JOIN location AS region ON
				region.location_id = network_extender.region_location_id

			LEFT JOIN location AS country ON
				country.location_id = network_extender.country_location_id

			WHERE
				network_extender.active AND
				(
					network_extender.region_location_id IS NOT NULL OR
					network_extender.country_location_id IS NOT NULL
				) AND
				EXISTS (
					SELECT 1
					FROM network_extender_address
					WHERE
						network_extender_address.extender_id = network_extender.extender_id AND
						network_extender_address.active
				)

			GROUP BY 1, 2
		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var row regionProviderCount
			server.Raise(result.Scan(&row.CountryCode, &row.Region, &row.Count))
			rows = append(rows, row)
		}
	})
	return rows
}

// GetProvidersMap aggregates active top-level, connected, valid, scored
// providers by country -> region, attaching a representative centroid to each
// region, and merges the online extenders of the same regions into
// extender_count (connect/EXTENDER.md M8).
//
// This queries the reliability tables directly — the same population
// UpdateClientLocations counts — NOT the InitialClientLocations snapshot. That
// snapshot only ever contains COUNTRY entries (it seeds the connect app's
// initial location list; see the LocationTypeCountry filter where it is built),
// so reading it and filtering for region rows matched nothing and the exported
// map was permanently `{}` in every environment.
func GetProvidersMap(ctx context.Context) (map[string]map[string]*RegionProviders, error) {
	rows := []regionProviderCount{}
	extenderRows := []regionProviderCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					location.country_code,
					location.location_name,
					COUNT(DISTINCT network_client_location_reliability.client_id) AS provider_count

				FROM network_client_location_reliability

				INNER JOIN network_client ON
					network_client.client_id = network_client_location_reliability.client_id

				INNER JOIN client_connection_reliability_score ON
					client_connection_reliability_score.client_id = network_client_location_reliability.client_id AND
					client_connection_reliability_score.lookback_index = 0

				INNER JOIN location ON
					location.location_id = network_client_location_reliability.region_location_id

				WHERE
					network_client.active = true AND
					network_client.source_client_id IS NULL AND
					network_client_location_reliability.connected = true AND
					network_client_location_reliability.valid = true AND
					-- same Public-only rule as UpdateClientLocations
					-- (network_client_location_model.go): this map is public,
					-- so it must count only providers a stranger can actually
					-- reach. GetProvideRelationship returns ProvideModePublic
					-- for a cross-network pair, so a Public provide key is
					-- what makes a provider generally reachable; a
					-- ProvideModeNetwork provider is real supply only to its
					-- own network and is effectively private.
					--
					-- The Public rule is the ONLY rule shared with
					-- UpdateClientLocations. This map is deliberately NOT
					-- gated on egress health or on an observed egress country,
					-- so since that gate shipped it reports MORE providers
					-- than /network/provider-locations will let anyone pick
					-- from (~305 vs ~137 for the US on beta). That is
					-- acceptable because the map is an aggregate
					-- supply-footprint statistic -- where the fleet physically
					-- is -- not a pick list, and nothing selects from it.
					-- Revisit if it ever becomes a selection surface, or is
					-- shown next to the per-location provider_count, where the
					-- two numbers disagreeing would read as a bug.
					EXISTS (
						SELECT 1 FROM provide_key
						WHERE
							provide_key.client_id = network_client_location_reliability.client_id AND
							provide_key.provide_mode = $1
					)

				GROUP BY location.country_code, location.location_name
			`,
			ProvideModePublic,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row regionProviderCount
				server.Raise(result.Scan(&row.CountryCode, &row.Region, &row.Count))
				rows = append(rows, row)
			}
		})

		// the second aggregate of M8, on the same connection: the online
		// extenders by the region their activation located them in
		extenderRows = getOnlineExtendersByRegion(ctx, conn)
	})

	return buildProvidersMap(rows, extenderRows), nil
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
