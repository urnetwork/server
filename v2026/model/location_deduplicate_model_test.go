package model

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Tests of the location de-duplication: the reference inventory against the
// migrations, and merges of duplicate rows with every reference repointed.

// the migration SQL the inventory test replays, statement by statement
var (
	migrationSqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	migrationSqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	migrationCreateTable     = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\((.*)\)\s*$`)
	migrationDropTable       = regexp.MustCompile(`(?is)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	migrationAlterTable      = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?(\w+)\s+(.*)$`)
	migrationLocationColumn  = regexp.MustCompile(`(?i)\b(\w*location_id)\s+uuid\b`)
	migrationAddColumn       = regexp.MustCompile(`(?i)ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w*location_id)\s+uuid\b`)
	migrationDropColumn      = regexp.MustCompile(`(?i)DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(\w*location_id)\b`)
	migrationRenameTable     = regexp.MustCompile(`(?is)^RENAME\s+TO\s+(\w+)`)
	migrationRenameColumn    = regexp.MustCompile(`(?i)RENAME\s+(?:COLUMN\s+)?(\w+)\s+TO\s+(\w+)`)
)

// Every column the migrations leave holding a location id is one the
// de-duplication repoints, and the inventory names nothing the migrations do
// not define. A migration that adds such a column fails here until the column
// is added to locationReferences with its repoint.
func TestLocationReferencesCoverTheMigrations(t *testing.T) {
	// Replays db_migrations.go the way the migrations apply: every SQL string
	// in the file, in order, statement by statement, tracking CREATE TABLE,
	// DROP TABLE, and ALTER TABLE's ADD COLUMN, DROP COLUMN and RENAME. It
	// returns the uuid columns named location_id or *_location_id that the
	// migrated schema ends with, as table.column.
	migrationLocationReferenceColumns := func() map[string]bool {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "db_migrations.go"), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		statements := []string{}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			sql, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			sql = migrationSqlBlockComment.ReplaceAllString(migrationSqlLineComment.ReplaceAllString(sql, ""), "")
			statements = append(statements, strings.Split(sql, ";")...)
			return true
		})

		tableColumns := map[string]map[string]bool{}
		for _, statement := range statements {
			if match := migrationCreateTable.FindStringSubmatch(statement); match != nil {
				columns := map[string]bool{}
				for _, column := range migrationLocationColumn.FindAllStringSubmatch(match[2], -1) {
					columns[strings.ToLower(column[1])] = true
				}
				tableColumns[strings.ToLower(match[1])] = columns
				continue
			}
			if match := migrationDropTable.FindStringSubmatch(statement); match != nil {
				delete(tableColumns, strings.ToLower(match[1]))
				continue
			}
			if match := migrationAlterTable.FindStringSubmatch(statement); match != nil {
				table := strings.ToLower(match[1])
				alteration := strings.TrimSpace(match[2])
				if rename := migrationRenameTable.FindStringSubmatch(alteration); rename != nil {
					tableColumns[strings.ToLower(rename[1])] = tableColumns[table]
					delete(tableColumns, table)
					continue
				}
				columns, ok := tableColumns[table]
				if !ok {
					columns = map[string]bool{}
					tableColumns[table] = columns
				}
				for _, column := range migrationAddColumn.FindAllStringSubmatch(alteration, -1) {
					columns[strings.ToLower(column[1])] = true
				}
				for _, column := range migrationDropColumn.FindAllStringSubmatch(alteration, -1) {
					delete(columns, strings.ToLower(column[1]))
				}
				for _, rename := range migrationRenameColumn.FindAllStringSubmatch(alteration, -1) {
					from := strings.ToLower(rename[1])
					if columns[from] {
						delete(columns, from)
						if strings.HasSuffix(strings.ToLower(rename[2]), "location_id") {
							columns[strings.ToLower(rename[2])] = true
						}
					}
				}
			}
		}

		referenceColumns := map[string]bool{}
		for table, columns := range tableColumns {
			for column := range columns {
				referenceColumns[table+"."+column] = true
			}
		}
		return referenceColumns
	}
	migratedColumns := migrationLocationReferenceColumns()
	if len(migratedColumns) < 10 {
		t.Fatalf("the migrations parse to only %d location reference columns: %v", len(migratedColumns), migratedColumns)
	}
	inventoriedColumns := map[string]bool{}
	for _, reference := range locationReferences {
		column := reference.table + "." + reference.column
		if inventoriedColumns[column] {
			t.Errorf("locationReferences names %s twice", column)
		}
		inventoriedColumns[column] = true
		if reference.kind == locationReferenceKey && len(reference.keyColumns) == 0 {
			t.Errorf("%s is a key reference with no key columns", column)
		}
		if len(reference.locationTypes) == 0 {
			t.Errorf("%s holds no kind of location", column)
		}
	}
	missingColumns := []string{}
	for column := range migratedColumns {
		if !inventoriedColumns[column] {
			missingColumns = append(missingColumns, column)
		}
	}
	staleColumns := []string{}
	for column := range inventoriedColumns {
		if !migratedColumns[column] {
			staleColumns = append(staleColumns, column)
		}
	}
	slices.Sort(missingColumns)
	slices.Sort(staleColumns)
	if 0 < len(missingColumns) {
		t.Errorf("columns hold a location id that DeduplicateLocations does not repoint; add them to locationReferences: %s", strings.Join(missingColumns, ", "))
	}
	if 0 < len(staleColumns) {
		t.Errorf("locationReferences names columns the migrations do not define: %s", strings.Join(staleColumns, ", "))
	}

	// a region merge rewrites the region columns and the any-kind columns,
	// never a city-only or country-only one
	for _, reference := range locationReferences {
		if reference.column == "city_location_id" && reference.repoints(LocationTypeRegion) {
			t.Errorf("a region merge would rewrite %s.%s", reference.table, reference.column)
		}
		if reference.kind == locationReferenceCountry && (reference.repoints(LocationTypeRegion) || reference.repoints(LocationTypeCity)) {
			t.Errorf("a merge would rewrite the country column %s.%s", reference.table, reference.column)
		}
	}
}

// Writes a location row directly, bypassing CreateLocation's matching, the way
// rows from the old sources came to exist side by side.
func insertTestLocation(
	ctx context.Context,
	locationType LocationType,
	name string,
	countryCode string,
	countryLocationId server.Id,
	regionLocationId *server.Id,
	geonameId uint32,
) server.Id {
	locationId := server.NewId()
	var cityLocationId *server.Id
	fullName := fmt.Sprintf("%s, %s", name, countryCode)
	switch locationType {
	case LocationTypeRegion:
		regionLocationId = &locationId
	case LocationTypeCity:
		cityLocationId = &locationId
		fullName = fmt.Sprintf("%s, %s, %s", name, locationName(ctx, nil, *regionLocationId), countryCode)
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
				INSERT INTO location (
					location_id,
					location_type,
					location_name,
					city_location_id,
					region_location_id,
					country_location_id,
					country_code,
					location_full_name,
					geoname_id
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			`,
			locationId,
			locationType,
			name,
			cityLocationId,
			regionLocationId,
			countryLocationId,
			countryCode,
			fullName,
			geonameIdArg(geonameId),
		))
	})
	return locationId
}

// Whether a location row with the id exists.
func testLocationExists(ctx context.Context, locationId server.Id) bool {
	return 0 < countLocations(ctx, `location_id = $1`, locationId)
}

// The count a `SELECT COUNT(*)` statement returns.
func testCountRows(ctx context.Context, sql string, args ...any) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, sql, args...)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// A duplicate region and a duplicate city from the old sources resolve to the
// places of the rows with geoname ids and merge into them, and every table that
// named a duplicate names the kept row after. A city that resolves to no place
// is left alone, and moves with its merged region.
func TestDeduplicateLocationsMergesDuplicatesAndRepointsReferences(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		canonical := &Location{
			LocationType:     LocationTypeCity,
			City:             "Dorking",
			Region:           "England",
			Country:          "United Kingdom",
			CountryCode:      "gb",
			Latitude:         51.2344,
			Longitude:        -0.3336,
			CityGeonameId:    2651095,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		}
		CreateLocation(ctx, canonical)

		// the old sources' spellings, side by side with the canonical rows
		legacyRegionId := insertTestLocation(ctx, LocationTypeRegion, "ENGLAND", "gb", canonical.CountryLocationId, nil, 0)
		legacyCityId := insertTestLocation(ctx, LocationTypeCity, "DORKING", "gb", canonical.CountryLocationId, &legacyRegionId, 0)
		// a city of the duplicate region that the list does not have moves with
		// its region and stays
		otherCityId := insertTestLocation(ctx, LocationTypeCity, "Mickleham", "gb", canonical.CountryLocationId, &legacyRegionId, 0)

		groupId := server.NewId()
		bothGroupId := server.NewId()
		networkId := server.NewId()
		connectionId := server.NewId()
		clientId := server.NewId()
		providerId := server.NewId()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO location_group_member (location_group_id, location_id)
				VALUES ($1, $3), ($2, $3), ($2, $4)
			`, groupId, bothGroupId, legacyRegionId, canonical.RegionLocationId))
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO exclude_network_client_location (network_id, client_location_id)
				VALUES ($1, $2)
			`, networkId, legacyCityId))
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO network_client_location (connection_id, client_id, city_location_id, region_location_id, country_location_id)
				VALUES ($1, $2, $3, $4, $5)
			`, connectionId, clientId, legacyCityId, legacyRegionId, canonical.CountryLocationId))
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO network_client_location_reliability (client_id, update_block_number, city_location_id, region_location_id, country_location_id)
				VALUES ($1, 1, $2, $3, $4)
			`, clientId, legacyCityId, legacyRegionId, canonical.CountryLocationId))
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO client_connection_reliability_score (
					client_id,
					independent_reliability_score,
					reliability_score,
					reliability_weight,
					city_location_id,
					region_location_id,
					country_location_id
				)
				VALUES ($1, 0, 0, 0, $2, $3, $4)
			`, clientId, legacyCityId, legacyRegionId, canonical.CountryLocationId))
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO provider_egress_location (client_id, location_id, country_code, observed_at, update_time)
				VALUES ($1, $2, 'gb', now(), now())
			`, providerId, legacyCityId))
		})

		deduplication, err := DeduplicateLocations(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, deduplication.RegionGroupCount, 1)
		connect.AssertEqual(t, deduplication.RegionMergedRowCount, 1)
		connect.AssertEqual(t, deduplication.CityGroupCount, 1)
		connect.AssertEqual(t, deduplication.CityMergedRowCount, 1)
		// both kept rows already carried their ids
		connect.AssertEqual(t, deduplication.RegionKeyedRowCount+deduplication.CityKeyedRowCount, 0)

		// the duplicates are gone, the canonical rows and the moved city stay
		connect.AssertEqual(t, testLocationExists(ctx, legacyRegionId), false)
		connect.AssertEqual(t, testLocationExists(ctx, legacyCityId), false)
		connect.AssertEqual(t, testLocationExists(ctx, canonical.LocationId), true)
		connect.AssertEqual(t, testLocationExists(ctx, canonical.RegionLocationId), true)
		other := GetLocation(ctx, otherCityId)
		connect.AssertEqual(t, other.RegionLocationId, canonical.RegionLocationId)

		// every reference names the canonical rows
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM location_group_member WHERE location_id = $1`, legacyRegionId), 0)
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM location_group_member WHERE location_group_id = $1 AND location_id = $2`, groupId, canonical.RegionLocationId), 1)
		// the group that had both keeps one membership
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM location_group_member WHERE location_group_id = $1`, bothGroupId), 1)
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM exclude_network_client_location WHERE network_id = $1 AND client_location_id = $2`, networkId, canonical.LocationId), 1)
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM network_client_location WHERE connection_id = $1 AND city_location_id = $2 AND region_location_id = $3`, connectionId, canonical.LocationId, canonical.RegionLocationId), 1)
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM network_client_location_reliability WHERE client_id = $1 AND city_location_id = $2 AND region_location_id = $3`, clientId, canonical.LocationId, canonical.RegionLocationId), 1)
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM client_connection_reliability_score WHERE client_id = $1 AND city_location_id = $2 AND region_location_id = $3`, clientId, canonical.LocationId, canonical.RegionLocationId), 1)
		connect.AssertEqual(t, testCountRows(ctx, `SELECT COUNT(*) FROM provider_egress_location WHERE client_id = $1 AND location_id = $2`, providerId, canonical.LocationId), 1)

		// the canonical city keeps its own coordinate and geoname id
		city := GetLocation(ctx, canonical.LocationId)
		connect.AssertEqual(t, city.Latitude, 51.2344)
		connect.AssertEqual(t, city.CityGeonameId, uint32(2651095))

		// and a second run finds nothing to merge
		again, err := DeduplicateLocations(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.RegionGroupCount+again.CityGroupCount, 0)
		connect.AssertEqual(t, again.RepointedReferenceCount, int64(0))
	})
}

// Rows merge only when they resolve to one place: a misspelling within reach
// of that place alone, or the same name spelled two ways. A row named for
// another place stays apart however alike ("São Pedro" is not "São Paulo"), as
// does a row of another geoname id; a row alone in its place takes the place's
// id; and the row kept for a place without one takes its coordinate from the
// list when it has none.
func TestDeduplicateLocationsMergesRowsThatResolveToOnePlace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(matchTestPlacesYaml)()

		campinas := &Location{
			LocationType:     LocationTypeCity,
			City:             "Campinas",
			Region:           "São Paulo",
			Country:          "Brazil",
			CountryCode:      "br",
			CityGeonameId:    3467865,
			RegionGeonameId:  3448433,
			CountryGeonameId: 3469034,
		}
		CreateLocation(ctx, campinas)
		regionId := campinas.RegionLocationId
		countryId := campinas.CountryLocationId

		// one edit from Campinas, and within reach of no other place: merged
		campinazId := insertTestLocation(ctx, LocationTypeCity, "Campinaz", "br", countryId, &regionId, 0)
		// named for places of their own: kept, and keyed
		santanaId := insertTestLocation(ctx, LocationTypeCity, "Santana", "br", countryId, &regionId, 0)
		saoPedroId := insertTestLocation(ctx, LocationTypeCity, "Sao Pedro", "br", countryId, &regionId, 0)
		saoPauloId := insertTestLocation(ctx, LocationTypeCity, "Sao Paulo", "br", countryId, &regionId, 0)
		// the same place spelled two ways, neither with an id: the more
		// referenced is kept, though it is the younger, and takes the id
		santosId := insertTestLocation(ctx, LocationTypeCity, "Santos", "br", countryId, &regionId, 0)
		santosUpperId := insertTestLocation(ctx, LocationTypeCity, "SANTOS", "br", countryId, &regionId, 0)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO location_group_member (location_group_id, location_id)
				VALUES ($1, $2), ($3, $2)
			`, server.NewId(), santosUpperId, server.NewId()))
		})
		// two rows of different geoname ids, one edit apart: apart
		springfieldId := insertTestLocation(ctx, LocationTypeCity, "Springfield", "br", countryId, &regionId, 4200000011)
		springfeldId := insertTestLocation(ctx, LocationTypeCity, "Springfeld", "br", countryId, &regionId, 4200000012)
		// in reach of no place: left alone
		atlantisId := insertTestLocation(ctx, LocationTypeCity, "Atlantis", "br", countryId, &regionId, 0)

		deduplication, err := DeduplicateLocations(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, deduplication.CityGroupCount, 2)
		connect.AssertEqual(t, deduplication.CityMergedRowCount, 2)
		// Santana, São Pedro, São Paulo, and "SANTOS" for its group
		connect.AssertEqual(t, deduplication.CityKeyedRowCount, 4)

		connect.AssertEqual(t, testLocationExists(ctx, campinazId), false)
		connect.AssertEqual(t, testLocationExists(ctx, campinas.LocationId), true)
		connect.AssertEqual(t, testLocationExists(ctx, santosId), false)
		for _, keptId := range []server.Id{santanaId, saoPedroId, saoPauloId, santosUpperId, springfieldId, springfeldId, atlantisId} {
			connect.AssertEqual(t, testLocationExists(ctx, keptId), true)
		}
		connect.AssertEqual(t, storedGeonameId(ctx, santanaId), uint32(8535094))
		connect.AssertEqual(t, storedGeonameId(ctx, saoPedroId), uint32(3448403))
		connect.AssertEqual(t, storedGeonameId(ctx, saoPauloId), uint32(3448439))
		connect.AssertEqual(t, storedGeonameId(ctx, santosUpperId), uint32(3449433))
		connect.AssertEqual(t, storedGeonameId(ctx, atlantisId), uint32(0))

		// the kept "SANTOS" had no coordinate and takes the list's
		santos := GetLocation(ctx, santosUpperId)
		connect.AssertEqual(t, santos.Latitude, -23.9569)
		connect.AssertEqual(t, santos.Longitude, -46.3446)

		// idempotent
		again, err := DeduplicateLocations(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.RegionGroupCount+again.CityGroupCount, 0)
		connect.AssertEqual(t, again.RegionKeyedRowCount+again.CityKeyedRowCount, 0)
	})
}

// A location id column the inventory does not know stops the
// de-duplication before it changes anything.
func TestDeduplicateLocationsRefusesAnUninventoriedReference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer pushTestPlaces(testPlacesYaml)()

		london := &Location{
			LocationType: LocationTypeCity,
			City:         "London",
			Region:       "England",
			Country:      "United Kingdom",
			CountryCode:  "gb",
		}
		CreateLocation(ctx, london)
		duplicateId := insertTestLocation(ctx, LocationTypeCity, "LONDON", "gb", london.CountryLocationId, &london.RegionLocationId, 0)

		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TABLE test_widget (widget_id uuid NOT NULL, widget_location_id uuid NULL)`))
		})
		_, err := DeduplicateLocations(ctx)
		if err == nil || !strings.Contains(err.Error(), "test_widget.widget_location_id") {
			t.Fatalf("an uninventoried reference was not refused: %v", err)
		}
		connect.AssertEqual(t, testLocationExists(ctx, duplicateId), true)
	})
}
