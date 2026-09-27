// Writes the canonical place list (connect/GEOMAP.md §4.1) from a
// GeoLite2-City database: every city keyed by its GeoNames id with the
// coordinate most of its networks carry, and every country the database
// names. xops/mmdb/update.sh runs it after each database update, into the
// same dated directory, so a lookup and a seeded place always come from the
// same file:
//
//	go run ./cli/geolite2export -mmdb <dir>/geolite2.mmdb -out <dir>/places.yml
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	maxminddb "github.com/oschwald/maxminddb-golang/v2"

	"github.com/urnetwork/server/v2026/geo"
)

// The localized names of a record's place; the export reads the English one.
type geoLite2Names struct {
	En string `maxminddb:"en"`
}

// A city or subdivision of a record.
type geoLite2Place struct {
	GeonameId uint32        `maxminddb:"geoname_id"`
	Names     geoLite2Names `maxminddb:"names"`
}

// The country of a record.
type geoLite2Country struct {
	IsoCode   string        `maxminddb:"iso_code"`
	GeonameId uint32        `maxminddb:"geoname_id"`
	Names     geoLite2Names `maxminddb:"names"`
}

// The continent of a record.
type geoLite2Continent struct {
	Code      string        `maxminddb:"code"`
	GeonameId uint32        `maxminddb:"geoname_id"`
	Names     geoLite2Names `maxminddb:"names"`
}

// the part of a GeoLite2-City record the export reads
type geoLite2CityRecord struct {
	City      geoLite2Place     `maxminddb:"city"`
	Continent geoLite2Continent `maxminddb:"continent"`
	Country   geoLite2Country   `maxminddb:"country"`
	Location  struct {
		// pointers: a record without a location must not read as 0,0
		Latitude       *float64 `maxminddb:"latitude"`
		Longitude      *float64 `maxminddb:"longitude"`
		AccuracyRadius uint16   `maxminddb:"accuracy_radius"`
		TimeZone       string   `maxminddb:"time_zone"`
	} `maxminddb:"location"`
	// largest first; the first is the one a location is filed under
	Subdivisions []geoLite2Place `maxminddb:"subdivisions"`
}

// Walks every network of the database into the place list.
func exportPlaces(mmdbPath string) (*geo.Export, error) {
	db, err := maxminddb.Open(mmdbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// the paid GeoIP2-City has the same record shape
	if !strings.HasSuffix(db.Metadata.DatabaseType, "-City") {
		return nil, fmt.Errorf("%s is a %q database, not a City database", mmdbPath, db.Metadata.DatabaseType)
	}

	// Networks share records: ~5.8M networks point at ~340k distinct records.
	// Count the networks behind each record in one walk, then decode each
	// record once. The first-seen order of the walk is deterministic, so the
	// aggregation sees the same sequence on every run (and its result does not
	// depend on the order anyway).
	offsetNetworkCounts := map[uintptr]int{}
	offsets := []uintptr{}
	for result := range db.Networks() {
		if err := result.Err(); err != nil {
			return nil, err
		}
		offset := result.Offset()
		if _, ok := offsetNetworkCounts[offset]; !ok {
			offsets = append(offsets, offset)
		}
		offsetNetworkCounts[offset] += 1
	}

	// the record as the export reads it; a record without both coordinates
	// has none
	exportNetwork := func(record *geoLite2CityRecord) *geo.ExportNetwork {
		network := &geo.ExportNetwork{
			CityGeonameId:    record.City.GeonameId,
			City:             record.City.Names.En,
			CountryCode:      record.Country.IsoCode,
			CountryGeonameId: record.Country.GeonameId,
			Country:          record.Country.Names.En,
			ContinentCode:    record.Continent.Code,
			Continent:        record.Continent.Names.En,
			AccuracyRadiusKm: record.Location.AccuracyRadius,
			TimeZone:         record.Location.TimeZone,
		}
		if 0 < len(record.Subdivisions) {
			network.RegionGeonameId = record.Subdivisions[0].GeonameId
			network.Region = record.Subdivisions[0].Names.En
		}
		if record.Location.Latitude != nil && record.Location.Longitude != nil {
			network.HasCoordinates = true
			network.Latitude = *record.Location.Latitude
			network.Longitude = *record.Location.Longitude
		}
		return network
	}
	exporter := geo.NewExporter()
	for _, offset := range offsets {
		var record geoLite2CityRecord
		if err := db.LookupOffset(offset).Decode(&record); err != nil {
			return nil, err
		}
		exporter.Add(exportNetwork(&record), offsetNetworkCounts[offset])
	}
	return exporter.Export(db.Metadata.DatabaseType, uint64(db.Metadata.BuildEpoch))
}

// Replaces path only with a complete file, so an interrupted run never leaves
// a truncated places.yml beside its database.
func writeFileAtomic(path string, content []byte) (returnErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			os.Remove(file.Name())
		}
	}()
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; the export is shared config
	if err := os.Chmod(file.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// Exports the database at mmdbPath to outPath, after checking that the output
// loads, and writes a summary line to log.
func run(mmdbPath string, outPath string, log io.Writer) error {
	export, err := exportPlaces(mmdbPath)
	if err != nil {
		return err
	}
	placesBytes, err := export.Marshal()
	if err != nil {
		return err
	}

	// Read the output back before it replaces anything: a places.yml the
	// seeder cannot load must never land beside its database.
	places, err := geo.LoadPlaces(placesBytes)
	if err != nil {
		return fmt.Errorf("the export does not load: %w", err)
	}
	if places.CityCount() != export.Summary.CityCount || len(places.Countries()) != export.Summary.CountryCount {
		return fmt.Errorf(
			"the export loads %d cities and %d countries, want %d and %d",
			places.CityCount(),
			len(places.Countries()),
			export.Summary.CityCount,
			export.Summary.CountryCount,
		)
	}

	if err := writeFileAtomic(outPath, placesBytes); err != nil {
		return err
	}

	summary := export.Summary
	fmt.Fprintf(
		log,
		"geolite2export: %s build %s: networks=%d city_networks=%d cities=%d countries=%d multi_coordinate_cities=%d collisions=%d region_collisions=%d regionless_cities=%d skipped_cities=%d max_spread_km=%.1f -> %s (%d bytes)\n",
		places.Source,
		time.Unix(int64(places.BuildEpoch), 0).UTC().Format(time.RFC3339),
		summary.NetworkCount,
		summary.CityNetworkCount,
		summary.CityCount,
		summary.CountryCount,
		summary.MultiCoordinateCityCount,
		summary.NameCollisionCount,
		summary.RegionNameCollisionCount,
		summary.RegionlessCityCount,
		summary.SkippedCityCount,
		summary.MaxSpreadKm,
		outPath,
		len(placesBytes),
	)
	return nil
}

// Parses -mmdb and -out and runs the export, exiting 2 on a usage error and 1
// on a failed export.
func main() {
	mmdbPath := flag.String("mmdb", "", "the GeoLite2-City database to export")
	outPath := flag.String("out", "", "the places.yml to write")
	flag.Parse()
	if *mmdbPath == "" || *outPath == "" || 0 < flag.NArg() {
		fmt.Fprintln(os.Stderr, "usage: geolite2export -mmdb <geolite2.mmdb> -out <places.yml>")
		os.Exit(2)
	}
	if err := run(*mmdbPath, *outPath, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "geolite2export: %v\n", err)
		os.Exit(1)
	}
}
