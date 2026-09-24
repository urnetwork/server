package model

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/geo"
)

// The init task de-duplicates (connect/GEOMAP.md §4.2): every stored row is
// resolved to a place of the canonical place list by the rule of
// location_match.go -- its geoname id, else the place its name anchors to,
// else the single place within the matching distance -- and the rows that
// resolve to the same place are one place. Each group keeps one row: the row
// with the place's id, else the most referenced, else the oldest. The kept row
// takes the place's id (and a city its coordinate) when it lacks them; a row
// that resolves to nothing is left alone, as is a row that is alone in its
// place apart from taking the id. Every column that names a merged row is then
// repointed to the kept row, once per column for the whole run, the merged rows
// are deleted, and one more pass catches any reference written in between.
// Regions are merged before cities, since a city is resolved within the
// region its region row is, which must be settled first.

// How a merge repoints one column.
type locationReferenceKind int

const (
	// UPDATE the column from a member to the canonical row
	locationReferenceUpdate locationReferenceKind = iota
	// the column is part of the table's key: each member's row is copied to the
	// canonical row unless the canonical row already has it, then deleted
	locationReferenceKey
	// the location table's own hierarchy: the cities filed under a merged
	// region move to the canonical region
	locationReferenceHierarchy
	// the row's own id -- the location table's key, or a city's
	// city_location_id, which is itself -- deleted with the row
	locationReferenceSelf
	// only ever a country's id, and countries are never merged
	locationReferenceCountry
)

// One column that holds a location id.
type locationReference struct {
	table  string
	column string
	kind   locationReferenceKind
	// the other columns of the table's key, for locationReferenceKey
	keyColumns []string
	// the kinds of location the column can name, so a merge of regions does
	// not read a column that only ever names cities
	locationTypes []LocationType
}

// the kinds of location a column of locationReferences can name
var (
	cityLocationTypes     = []LocationType{LocationTypeCity}
	regionLocationTypes   = []LocationType{LocationTypeRegion}
	countryLocationTypes  = []LocationType{LocationTypeCountry}
	anyPlaceLocationTypes = []LocationType{LocationTypeCity, LocationTypeRegion, LocationTypeCountry}
)

// Every column that holds a location id, inventoried from db_migrations.go:
// every live uuid column named `location_id` or `*_location_id`, and
// `location_group_member.location_id`. There are no foreign key constraints,
// so nothing but this list repoints them.
// TestLocationReferencesCoverTheMigrations re-reads the migrations and fails
// on a column this list does not name, and DeduplicateLocations refuses to run
// when the live schema has one, so a new table cannot silently orphan rows.
var locationReferences = []locationReference{
	// location: every row's own id, a city's own id, the region a city is
	// filed under (a region names itself), and every row's country
	{table: "location", column: "location_id", kind: locationReferenceSelf, locationTypes: anyPlaceLocationTypes},
	{table: "location", column: "city_location_id", kind: locationReferenceSelf, locationTypes: cityLocationTypes},
	{table: "location", column: "region_location_id", kind: locationReferenceHierarchy, locationTypes: regionLocationTypes},
	{table: "location", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// the members of each location group, keyed (location_group_id, location_id)
	{table: "location_group_member", column: "location_id", kind: locationReferenceKey, keyColumns: []string{"location_group_id"}, locationTypes: anyPlaceLocationTypes},

	// each connection's location, keyed by connection_id. A location of
	// coarser granularity stores its coarsest id in the finer columns
	// (SetConnectionLocation), so a city column holds a city or a country and
	// a region column a region or a country. The location indexes on this
	// table were dropped, so each repoint reads it in full.
	{table: "network_client_location", column: "city_location_id", kind: locationReferenceUpdate, locationTypes: cityLocationTypes},
	{table: "network_client_location", column: "region_location_id", kind: locationReferenceUpdate, locationTypes: regionLocationTypes},
	{table: "network_client_location", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},
	// the location the connection's own lookup resolved to, its genesis (a
	// city, a region or a country), which the derive phase anchors to
	{table: "network_client_location", column: "genesis_location_id", kind: locationReferenceUpdate, locationTypes: anyPlaceLocationTypes},

	// the locations a network excludes, keyed (network_id, client_location_id)
	{table: "exclude_network_client_location", column: "client_location_id", kind: locationReferenceKey, keyColumns: []string{"network_id"}, locationTypes: anyPlaceLocationTypes},

	// each client's reliability location, keyed by client_id
	{table: "client_connection_reliability_score", column: "city_location_id", kind: locationReferenceUpdate, locationTypes: cityLocationTypes},
	{table: "client_connection_reliability_score", column: "region_location_id", kind: locationReferenceUpdate, locationTypes: regionLocationTypes},
	{table: "client_connection_reliability_score", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// each network's reliability by country
	{table: "network_connection_reliability_score", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// the per-client location reliability history, keyed by client_id; the
	// city and region indexes were dropped, so each repoint reads it in full
	{table: "network_client_location_reliability", column: "city_location_id", kind: locationReferenceUpdate, locationTypes: cityLocationTypes},
	{table: "network_client_location_reliability", column: "region_location_id", kind: locationReferenceUpdate, locationTypes: regionLocationTypes},
	{table: "network_client_location_reliability", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// the reliability multiplier of each country, keyed by country_location_id
	{table: "network_client_location_reliability_multiplier", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// each network's windowed reliability by country, keyed (network_id, country_location_id)
	{table: "network_connection_reliability_window_score", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// each provider's probed egress location (a city or a country), keyed by client_id
	{table: "provider_egress_location", column: "location_id", kind: locationReferenceUpdate, locationTypes: anyPlaceLocationTypes},

	// each extender's location at its last activation, keyed by extender_id
	{table: "network_extender", column: "location_id", kind: locationReferenceUpdate, locationTypes: anyPlaceLocationTypes},
	{table: "network_extender", column: "city_location_id", kind: locationReferenceUpdate, locationTypes: cityLocationTypes},
	{table: "network_extender", column: "region_location_id", kind: locationReferenceUpdate, locationTypes: regionLocationTypes},
	{table: "network_extender", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// the location of every extender activation, keyed by activation_id
	{table: "network_extender_activation", column: "location_id", kind: locationReferenceUpdate, locationTypes: anyPlaceLocationTypes},
	{table: "network_extender_activation", column: "city_location_id", kind: locationReferenceUpdate, locationTypes: cityLocationTypes},
	{table: "network_extender_activation", column: "region_location_id", kind: locationReferenceUpdate, locationTypes: regionLocationTypes},
	{table: "network_extender_activation", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},

	// the place each derived location maps to (connect/GEOMAP.md §6), keyed
	// (node_kind, node_id). The mapped place is always a city of the place
	// list, so location_id and city_location_id name the same city row; a
	// merged place repoints the rows here like everywhere else, and the next
	// derivation rewrites them from the list anyway.
	{table: "derived_location", column: "location_id", kind: locationReferenceUpdate, locationTypes: cityLocationTypes},
	{table: "derived_location", column: "city_location_id", kind: locationReferenceUpdate, locationTypes: cityLocationTypes},
	{table: "derived_location", column: "region_location_id", kind: locationReferenceUpdate, locationTypes: regionLocationTypes},
	{table: "derived_location", column: "country_location_id", kind: locationReferenceCountry, locationTypes: countryLocationTypes},
}

// Whether a merge of locationType rows must rewrite the column.
func (self *locationReference) repoints(locationType LocationType) bool {
	switch self.kind {
	case locationReferenceUpdate, locationReferenceKey, locationReferenceHierarchy:
		return slices.Contains(self.locationTypes, locationType)
	default:
		return false
	}
}

// Compares the inventory with the live schema: the location id columns the
// schema has and the inventory does not, and those the inventory names and
// the schema lacks.
func locationReferenceDriftInTx(ctx context.Context, tx server.PgTx) (unknownColumns []string, missingColumns []string) {
	inventoryColumns := map[string]bool{}
	for _, reference := range locationReferences {
		inventoryColumns[reference.table+"."+reference.column] = true
	}
	liveColumns := map[string]bool{}
	result, err := tx.Query(
		ctx,
		`
			SELECT
				columns.table_name,
				columns.column_name
			FROM information_schema.columns
			INNER JOIN information_schema.tables ON
				tables.table_schema = columns.table_schema AND
				tables.table_name = columns.table_name
			WHERE
				columns.table_schema = current_schema() AND
				tables.table_type = 'BASE TABLE' AND
				columns.data_type = 'uuid' AND
				columns.column_name ~ 'location_id$'
		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var table string
			var column string
			server.Raise(result.Scan(&table, &column))
			liveColumns[table+"."+column] = true
		}
	})
	for column := range liveColumns {
		if !inventoryColumns[column] {
			unknownColumns = append(unknownColumns, column)
		}
	}
	for column := range inventoryColumns {
		if !liveColumns[column] {
			missingColumns = append(missingColumns, column)
		}
	}
	slices.Sort(unknownColumns)
	slices.Sort(missingColumns)
	return
}

// The tunables of a de-duplication run.
type LocationDeduplicationSettings struct {
	// the rows of one statement's merge mapping or deletion; a mapping of this
	// many pairs is two arrays of 1.6 MB, and each statement over a history
	// table without a location index reads that table once
	MergeBatchSize int
}

// The settings DeduplicateLocations runs with.
func DefaultLocationDeduplicationSettings() *LocationDeduplicationSettings {
	return &LocationDeduplicationSettings{
		MergeBatchSize: 100000,
	}
}

// Counts what one de-duplication run did.
type LocationDeduplication struct {
	// places with more than one row, and the rows merged away
	RegionGroupCount     int
	RegionMergedRowCount int
	CityGroupCount       int
	CityMergedRowCount   int
	// rows without an id that took their place's
	RegionKeyedRowCount int
	CityKeyedRowCount   int
	// rows of other tables rewritten to name a kept row
	RepointedReferenceCount int64
}

// Runs DeduplicateLocationsWithSettings with the default settings.
func DeduplicateLocations(ctx context.Context) (*LocationDeduplication, error) {
	return DeduplicateLocationsWithSettings(ctx, DefaultLocationDeduplicationSettings())
}

// Resolves and merges the stored region and city rows against the process's
// place list, as deduplicateLocationsWithSettings does; with no place list it
// refuses to run and changes nothing.
func DeduplicateLocationsWithSettings(ctx context.Context, settings *LocationDeduplicationSettings) (*LocationDeduplication, error) {
	placeNames := currentLocationPlaceNames()
	if placeNames == nil {
		return nil, fmt.Errorf("no place list (%s) to resolve locations against", placesResource)
	}
	return deduplicateLocationsWithSettings(ctx, placeNames, settings)
}

// Runs deduplicateLocationsWithSettings with the default settings, for the
// seeder, which de-duplicates against the list it has just installed.
func deduplicateLocations(ctx context.Context, placeNames *locationPlaceNames) (*LocationDeduplication, error) {
	return deduplicateLocationsWithSettings(ctx, placeNames, DefaultLocationDeduplicationSettings())
}

// Resolves and merges the stored region and city rows against a place list.
// It refuses to run, and changes nothing, when the schema holds a location id
// column the inventory does not know; the caller logs that and carries on.
func deduplicateLocationsWithSettings(
	ctx context.Context,
	placeNames *locationPlaceNames,
	settings *LocationDeduplicationSettings,
) (*LocationDeduplication, error) {
	var unknownColumns []string
	var missingColumns []string
	server.Tx(ctx, func(tx server.PgTx) {
		unknownColumns, missingColumns = locationReferenceDriftInTx(ctx, tx)
	}, server.TxReadCommitted)
	if 0 < len(unknownColumns) || 0 < len(missingColumns) {
		return nil, fmt.Errorf(
			"location references are not inventoried (locationReferences): unknown columns [%s], missing columns [%s]",
			strings.Join(unknownColumns, ", "),
			strings.Join(missingColumns, ", "),
		)
	}

	deduplication := &LocationDeduplication{}

	regionMerges := planLocationMerges(loadLocationRows(ctx, LocationTypeRegion), func(row *locationRow) (placeKey, placeResolution, bool) {
		if row.geonameId != 0 {
			return placeKey{countryCode: row.countryCode, geonameId: row.geonameId}, placeResolution{}, true
		}
		resolution := placeNames.resolveRegion(row.countryCode, row.name)
		if !resolution.resolved() {
			return placeKey{}, resolution, false
		}
		// the region named for a country has no id and is keyed by the country
		return placeKey{countryCode: row.countryCode, geonameId: resolution.candidate.geonameId}, resolution, true
	})
	deduplication.RegionGroupCount, deduplication.RegionMergedRowCount, deduplication.RegionKeyedRowCount = applyLocationMerges(
		ctx,
		placeNames,
		LocationTypeRegion,
		regionMerges,
		deduplication,
		settings,
	)

	// the list's region of each region row as it now stands, the scope its
	// cities resolve in
	locationIdListRegions := map[server.Id]*geo.RegionNames{}
	for _, row := range loadLocationRows(ctx, LocationTypeRegion) {
		if region := placeNames.regionOfRow(row.countryCode, row.name, row.geonameId); region != nil {
			locationIdListRegions[row.locationId] = region
		}
	}
	cityMerges := planLocationMerges(loadLocationRows(ctx, LocationTypeCity), func(row *locationRow) (placeKey, placeResolution, bool) {
		if row.geonameId != 0 {
			return placeKey{countryCode: row.countryCode, geonameId: row.geonameId}, placeResolution{}, true
		}
		resolution := placeNames.resolveCity(row.countryCode, locationIdListRegions[row.regionLocationId], row.name)
		if !resolution.resolved() {
			return placeKey{}, resolution, false
		}
		return placeKey{countryCode: row.countryCode, geonameId: resolution.candidate.geonameId}, resolution, true
	})
	deduplication.CityGroupCount, deduplication.CityMergedRowCount, deduplication.CityKeyedRowCount = applyLocationMerges(
		ctx,
		placeNames,
		LocationTypeCity,
		cityMerges,
		deduplication,
		settings,
	)

	glog.Infof(
		"[loc]deduplicated %d region places (%d rows merged, %d keyed) and %d city places (%d rows merged, %d keyed); repointed %d references\n",
		deduplication.RegionGroupCount,
		deduplication.RegionMergedRowCount,
		deduplication.RegionKeyedRowCount,
		deduplication.CityGroupCount,
		deduplication.CityMergedRowCount,
		deduplication.CityKeyedRowCount,
		deduplication.RepointedReferenceCount,
	)
	return deduplication, nil
}

// Reads every row of a type.
func loadLocationRows(ctx context.Context, locationType LocationType) []*locationRow {
	rows := []*locationRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					location_id,
					location_name,
					country_code,
					region_location_id,
					geoname_id
				FROM location
				WHERE location_type = $1
			`,
			locationType,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &locationRow{}
				var regionLocationId *server.Id
				var geonameId *int64
				server.Raise(result.Scan(
					&row.locationId,
					&row.name,
					&row.countryCode,
					&regionLocationId,
					&geonameId,
				))
				row.countryCode = strings.ToLower(row.countryCode)
				if regionLocationId != nil {
					row.regionLocationId = *regionLocationId
				}
				row.geonameId = geonameIdValue(geonameId)
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// How many rows of each inventoried column name each of the given rows.
type locationReferenceCounts map[string]map[server.Id]int

// Counts, for each column a merge of locationType rows rewrites, the rows
// naming each of the given locations. Columns without an index are read in
// full, once each, joined against the ids.
func countLocationReferences(ctx context.Context, locationType LocationType, locationIds []server.Id) locationReferenceCounts {
	counts := locationReferenceCounts{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, reference := range locationReferences {
			if !reference.repoints(locationType) {
				continue
			}
			locationIdCounts := map[server.Id]int{}
			sql := fmt.Sprintf(
				`
					SELECT
						referencing.%[2]s,
						COUNT(*)
					FROM %[1]s AS referencing
					INNER JOIN unnest($1::uuid[]) AS merged(location_id) ON merged.location_id = referencing.%[2]s
					GROUP BY referencing.%[2]s
				`,
				reference.table,
				reference.column,
			)
			if reference.kind == locationReferenceHierarchy {
				// a region's own row names itself; only its cities count
				sql = fmt.Sprintf(
					`
						SELECT
							referencing.%[2]s,
							COUNT(*)
						FROM %[1]s AS referencing
						INNER JOIN unnest($1::uuid[]) AS merged(location_id) ON merged.location_id = referencing.%[2]s
						WHERE referencing.location_type = '%[3]s'
						GROUP BY referencing.%[2]s
					`,
					reference.table,
					reference.column,
					LocationTypeCity,
				)
			}
			result, err := conn.Query(ctx, sql, locationIds)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var locationId server.Id
					var count int
					server.Raise(result.Scan(&locationId, &count))
					locationIdCounts[locationId] = count
				}
			})
			counts[reference.table+"."+reference.column] = locationIdCounts
		}
	})
	return counts
}

// The rows of every column that name one location.
func (self locationReferenceCounts) total(locationId server.Id) int {
	total := 0
	for _, locationIdCounts := range self {
		total += locationIdCounts[locationId]
	}
	return total
}

// Carries out a plan: the kept rows take their place's id (and a city its
// coordinate), every reference to a merged row is repointed in one pass per
// column, the merged rows are deleted, and a second pass repoints any
// reference written in between. It returns the places with merged rows, the
// rows merged and the rows keyed.
func applyLocationMerges(
	ctx context.Context,
	placeNames *locationPlaceNames,
	locationType LocationType,
	merges []*locationMerge,
	deduplication *LocationDeduplication,
	settings *LocationDeduplicationSettings,
) (groupCount int, mergedRowCount int, keyedRowCount int) {
	// Which of several rows without the place's id is kept depends on their
	// references, which are counted only for those rows: counting reads the
	// connection history tables in full.
	unkeyedIds := []server.Id{}
	for _, merge := range merges {
		if 0 < len(merge.memberRows) && merge.canonicalRow.geonameId != merge.key.geonameId {
			unkeyedIds = append(unkeyedIds, merge.canonicalRow.locationId)
			for _, member := range merge.memberRows {
				unkeyedIds = append(unkeyedIds, member.locationId)
			}
		}
	}
	if 0 < len(unkeyedIds) {
		counts := countLocationReferences(ctx, locationType, unkeyedIds)
		for _, merge := range merges {
			if len(merge.memberRows) == 0 || merge.canonicalRow.geonameId == merge.key.geonameId {
				continue
			}
			groupRows := append([]*locationRow{merge.canonicalRow}, merge.memberRows...)
			for _, row := range groupRows {
				row.referenceCount = counts.total(row.locationId)
			}
			slices.SortFunc(groupRows, func(a *locationRow, b *locationRow) int {
				return compareCanonicalRows(merge.key, a, b)
			})
			merge.canonicalRow = groupRows[0]
			merge.memberRows = groupRows[1:]
		}
	}

	// Key the kept rows first, so a lookup of the place finds its row by id
	// from here on rather than resolving to a row about to be merged away.
	keyIds := []server.Id{}
	keyGeonameIds := []int64{}
	coordinateIds := []server.Id{}
	latitudes := []float64{}
	longitudes := []float64{}
	for _, merge := range merges {
		if merge.key.geonameId == 0 || merge.canonicalRow.geonameId == merge.key.geonameId {
			continue
		}
		keyIds = append(keyIds, merge.canonicalRow.locationId)
		keyGeonameIds = append(keyGeonameIds, int64(merge.key.geonameId))
		if locationType == LocationTypeCity {
			if place := placeNames.places.CityByGeonameId(merge.key.geonameId); place != nil {
				coordinateIds = append(coordinateIds, merge.canonicalRow.locationId)
				latitudes = append(latitudes, place.Latitude)
				longitudes = append(longitudes, place.Longitude)
			}
		}
	}
	for start := 0; start < len(keyIds); start += settings.MergeBatchSize {
		end := min(start+settings.MergeBatchSize, len(keyIds))
		// counted once the batch commits, not per attempt server.Tx may retry
		var batchKeyedCount int
		server.Tx(ctx, func(tx server.PgTx) {
			// another row already holding the id (bad data) leaves this one as
			// it is rather than fail on the unique index
			tag, err := tx.Exec(
				ctx,
				`
					UPDATE location
					SET geoname_id = keyed.geoname_id
					FROM unnest($1::uuid[], $2::bigint[]) AS keyed(location_id, geoname_id)
					WHERE
						location.location_id = keyed.location_id AND
						location.geoname_id IS NULL AND
						NOT EXISTS (
							SELECT 1
							FROM location AS holder
							WHERE holder.geoname_id = keyed.geoname_id
						)
				`,
				keyIds[start:end],
				keyGeonameIds[start:end],
			)
			server.Raise(err)
			batchKeyedCount = int(tag.RowsAffected())
		})
		keyedRowCount += batchKeyedCount
	}
	for start := 0; start < len(coordinateIds); start += settings.MergeBatchSize {
		end := min(start+settings.MergeBatchSize, len(coordinateIds))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE location
					SET
						latitude = listed.latitude,
						longitude = listed.longitude
					FROM unnest($1::uuid[], $2::double precision[], $3::double precision[]) AS listed(location_id, latitude, longitude)
					WHERE
						location.location_id = listed.location_id AND
						location.latitude IS NULL
				`,
				coordinateIds[start:end],
				latitudes[start:end],
				longitudes[start:end],
			))
		})
	}

	memberIds := []server.Id{}
	canonicalIds := []server.Id{}
	for _, merge := range merges {
		if len(merge.memberRows) == 0 {
			continue
		}
		groupCount += 1
		memberNames := make([]string, 0, len(merge.memberRows))
		for _, member := range merge.memberRows {
			memberIds = append(memberIds, member.locationId)
			canonicalIds = append(canonicalIds, merge.canonicalRow.locationId)
			memberNames = append(memberNames, fmt.Sprintf("%q (%s)", member.name, member.locationId))
		}
		glog.Infof(
			"[loc]merge %s %s into %q (%s), geoname %d\n",
			locationType,
			strings.Join(memberNames, ", "),
			merge.canonicalRow.name,
			merge.canonicalRow.locationId,
			merge.key.geonameId,
		)
	}
	if len(memberIds) == 0 {
		return
	}

	deduplication.RepointedReferenceCount += repointLocationReferences(ctx, locationType, memberIds, canonicalIds, settings)
	for start := 0; start < len(memberIds); start += settings.MergeBatchSize {
		end := min(start+settings.MergeBatchSize, len(memberIds))
		var batchDeletedCount int
		server.Tx(ctx, func(tx server.PgTx) {
			tag, err := tx.Exec(
				ctx,
				`DELETE FROM location WHERE location_id = ANY($1::uuid[])`,
				memberIds[start:end],
			)
			server.Raise(err)
			batchDeletedCount = int(tag.RowsAffected())
			for _, memberId := range memberIds[start:end] {
				locationSearch().RemoveInTx(ctx, memberId, tx)
			}
		})
		mergedRowCount += batchDeletedCount
	}
	// No lookup finds a deleted row, so a reference to one can only have been
	// written between the pass above and the deletion; this pass takes those.
	sweptCount := repointLocationReferences(ctx, locationType, memberIds, canonicalIds, settings)
	if 0 < sweptCount {
		glog.Infof("[loc]swept %d references to merged %s rows written during the merge\n", sweptCount, locationType)
	}
	deduplication.RepointedReferenceCount += sweptCount
	return
}

// Rewrites every column that can name the merged rows to name their kept rows
// instead: one statement per column for the whole mapping (per batch of it),
// each joining the column against the mapping, so a column without an index
// is read once, not once per place.
func repointLocationReferences(
	ctx context.Context,
	locationType LocationType,
	memberIds []server.Id,
	canonicalIds []server.Id,
	settings *LocationDeduplicationSettings,
) int64 {
	var repointedCount int64
	for _, reference := range locationReferences {
		if !reference.repoints(locationType) {
			continue
		}
		for start := 0; start < len(memberIds); start += settings.MergeBatchSize {
			end := min(start+settings.MergeBatchSize, len(memberIds))
			batchMemberIds := memberIds[start:end]
			batchCanonicalIds := canonicalIds[start:end]
			var rewrittenCount int64
			server.Tx(ctx, func(tx server.PgTx) {
				switch reference.kind {
				case locationReferenceUpdate, locationReferenceHierarchy:
					sql := fmt.Sprintf(
						`
							UPDATE %[1]s AS referencing
							SET %[2]s = merged.canonical_id
							FROM unnest($1::uuid[], $2::uuid[]) AS merged(member_id, canonical_id)
							WHERE referencing.%[2]s = merged.member_id
						`,
						reference.table,
						reference.column,
					)
					if reference.kind == locationReferenceHierarchy {
						// a region row names itself; only its cities move
						sql += fmt.Sprintf(` AND referencing.location_type = '%s'`, LocationTypeCity)
					}
					tag, err := tx.Exec(ctx, sql, batchMemberIds, batchCanonicalIds)
					server.Raise(err)
					rewrittenCount = tag.RowsAffected()
				case locationReferenceKey:
					joinedKeyColumns := strings.Join(reference.keyColumns, ", ")
					server.RaisePgResult(tx.Exec(
						ctx,
						fmt.Sprintf(
							`
								INSERT INTO %[1]s (%[3]s, %[2]s)
								SELECT %[3]s, merged.canonical_id
								FROM %[1]s AS referencing
								INNER JOIN unnest($1::uuid[], $2::uuid[]) AS merged(member_id, canonical_id) ON
									merged.member_id = referencing.%[2]s
								ON CONFLICT DO NOTHING
							`,
							reference.table,
							reference.column,
							joinedKeyColumns,
						),
						batchMemberIds,
						batchCanonicalIds,
					))
					tag, err := tx.Exec(
						ctx,
						fmt.Sprintf(
							`DELETE FROM %[1]s WHERE %[2]s = ANY($1::uuid[])`,
							reference.table,
							reference.column,
						),
						batchMemberIds,
					)
					server.Raise(err)
					rewrittenCount = tag.RowsAffected()
				}
			})
			repointedCount += rewrittenCount
		}
	}
	return repointedCount
}
