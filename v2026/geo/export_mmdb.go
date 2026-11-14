// GeoLite2 place export shared by the standalone exporter and arindbctl.
package geo

import (
	"fmt"
	maxminddb "github.com/oschwald/maxminddb-golang/v2"
	"strings"
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
func ReadMmdbExport(mmdbPath string) (*Export, error) {
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
	exportNetwork := func(record *geoLite2CityRecord) *ExportNetwork {
		network := &ExportNetwork{
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
	exporter := NewExporter()
	for _, offset := range offsets {
		var record geoLite2CityRecord
		if err := db.LookupOffset(offset).Decode(&record); err != nil {
			return nil, err
		}
		exporter.Add(exportNetwork(&record), offsetNetworkCounts[offset])
	}
	return exporter.Export(db.Metadata.DatabaseType, uint64(db.Metadata.BuildEpoch))
}
