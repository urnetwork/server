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
	"time"

	"github.com/urnetwork/server/v2026/geo"
)

// Reads the same canonical export used by arindbctl.
func exportPlaces(mmdbPath string) (*geo.Export, error) {
	return geo.ReadMmdbExport(mmdbPath)
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
