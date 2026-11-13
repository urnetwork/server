// SPDX-License-Identifier: MPL-2.0

// Command gencountry writes the two country tables the campaign emails fall
// back to when a reader's own time zone or language is unknown
// (onboarding/country_zone.go, onboarding/mail_language.go):
//
//   - country_zone_table.go: for each ISO 3166-1 alpha-2 country, the zone its
//     sends are placed in and, for a country whose populated zones keep
//     different clocks and none of which holds the dominant share of its
//     people, how much later the send window opens and how much earlier it
//     closes in that zone, so that a send inside the narrowed window is inside
//     the window in every populated zone of the country. Built from IANA's
//     zone1970.tab and its Link lines (which zones serve a country), GeoNames'
//     cities15000 (how many people each zone serves) and Go's own zoneinfo.zip
//     (the offsets, hour by hour over two years, from the same database the
//     server embeds, server/tzdata.go).
//   - country_language_table.go: for each territory, the languages CLDR says
//     its people use (territoryInfo languagePopulation, first and second
//     language at the fluency a computer or phone needs), with their shares.
//     The rule that reads them, and its thresholds, live in settings
//     (onboarding.MailLanguageSettings), not here.
//
// The inputs are downloads, read from one directory, never fetched here:
//
//	mkdir inputs && cd inputs
//	curl -O https://data.iana.org/time-zones/releases/tzdata2026c.tar.gz
//	mkdir tzdata && tar -xzf tzdata2026c.tar.gz -C tzdata
//	curl -O https://download.geonames.org/export/dump/cities15000.zip
//	unzip cities15000.zip
//	curl -o supplementalData.xml https://raw.githubusercontent.com/unicode-org/cldr/release-49/common/supplemental/supplementalData.xml
//	cd .. && URNETWORK_COUNTRY_INPUTS=$PWD/inputs go generate ./onboarding/
//
// The tzdata release must be the one Go's lib/time/zoneinfo.zip carries
// (lib/time/update.bash, DATA=), so the tables agree with the offsets the
// server computes. Regenerate when the Go release's tzdata changes.
//
// Data sources and licenses: IANA tz database (public domain); GeoNames
// (https://www.geonames.org/, Creative Commons Attribution 4.0); Unicode CLDR
// (Unicode License v3). The generated files carry the same attribution and
// the SHA-256 of every input.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	inputs := flag.String("inputs", "", "the directory holding tzdata/, cities15000.txt and supplementalData.xml")
	zoneinfo := flag.String("zoneinfo", "", "Go's lib/time/zoneinfo.zip (default: $GOROOT/lib/time/zoneinfo.zip)")
	zonesOut := flag.String("zones", "country_zone_table.go", "the country zone table to write")
	languagesOut := flag.String("languages", "country_language_table.go", "the country language table to write")
	dominantShare := flag.Float64("dominant", 0.9, "the share of a country's people one zone must serve for the country to map to it directly")
	languageFloor := flag.Float64("language-floor", 5, "the least population percent a language needs to be listed for its territory")
	startYear := flag.Int("year", 2026, "the first of the two years whose offsets are sampled")
	flag.Parse()
	if *inputs == "" {
		fail("-inputs is required (see the doc comment for the downloads)")
	}
	if *zoneinfo == "" {
		*zoneinfo = filepath.Join(goroot(), "lib", "time", "zoneinfo.zip")
	}

	tzdataDir := filepath.Join(*inputs, "tzdata")
	zone1970Path := filepath.Join(tzdataDir, "zone1970.tab")
	citiesPath := filepath.Join(*inputs, "cities15000.txt")
	cldrPath := filepath.Join(*inputs, "supplementalData.xml")

	tzdataVersion := strings.TrimSpace(string(readFile(filepath.Join(tzdataDir, "version"))))
	zoneinfoZones := readZoneinfo(*zoneinfo)
	countryZoneNames := readZone1970(zone1970Path)
	linkTargets := readLinks(tzdataDir)
	cities := readCities(citiesPath)

	zoneSource := fmt.Sprintf(
		"// Inputs: tzdata %s zone1970.tab (sha256 %s) and Link lines; GeoNames\n// cities15000.txt (sha256 %s); Go zoneinfo.zip (sha256 %s).\n",
		tzdataVersion,
		fileSha256(zone1970Path),
		fileSha256(citiesPath),
		fileSha256(*zoneinfo),
	)
	writeGo(*zonesOut, countryZoneTable(countryZoneNames, linkTargets, cities, zoneinfoZones, *dominantShare, *startYear, zoneSource))

	languageSource := fmt.Sprintf("// Input: CLDR supplementalData.xml (sha256 %s).\n", fileSha256(cldrPath))
	writeGo(*languagesOut, countryLanguageTable(readTerritoryLanguages(cldrPath), *languageFloor, languageSource))
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "gencountry: "+format+"\n", a...)
	os.Exit(1)
}

func goroot() string {
	if root := os.Getenv("GOROOT"); root != "" {
		return root
	}
	// `go run` and `go generate` put the toolchain's bin first on PATH
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
			return filepath.Dir(dir)
		}
	}
	fail("cannot find GOROOT; pass -zoneinfo")
	return ""
}

func readFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fail("%s", err)
	}
	return data
}

func fileSha256(path string) string {
	sum := sha256.Sum256(readFile(path))
	return hex.EncodeToString(sum[:])
}

func writeGo(path string, source []byte) {
	formatted, err := format.Source(source)
	if err != nil {
		fail("format %s: %s", path, err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		fail("%s", err)
	}
}

// readZoneinfo loads every zone of a zoneinfo.zip as Go's time package
// would from the embedded database.
func readZoneinfo(path string) map[string]*time.Location {
	reader, err := zip.OpenReader(path)
	if err != nil {
		fail("%s", err)
	}
	defer reader.Close()
	zoneLocations := map[string]*time.Location{}
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		in, err := file.Open()
		if err != nil {
			fail("%s: %s", file.Name, err)
		}
		data, err := io.ReadAll(in)
		in.Close()
		if err != nil {
			fail("%s: %s", file.Name, err)
		}
		location, err := time.LoadLocationFromTZData(file.Name, data)
		if err != nil {
			continue
		}
		zoneLocations[file.Name] = location
	}
	return zoneLocations
}

// readZone1970 is each country's zones in the file's order: by the file's
// own rule, the most populous first where geography allows.
func readZone1970(path string) map[string][]string {
	countryZoneNames := map[string][]string{}
	scanner := bufio.NewScanner(bytes.NewReader(readFile(path)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			fail("zone1970.tab: %q", line)
		}
		for _, country := range strings.Split(fields[0], ",") {
			countryZoneNames[country] = append(countryZoneNames[country], fields[2])
		}
	}
	return countryZoneNames
}

// readLinks maps each Link name of the tzdata sources to its target zone
// (Europe/Copenhagen to Europe/Berlin, which zone1970.tab lists for Denmark).
// backzone is not read: its zones are history before 1970.
func readLinks(dir string) map[string]string {
	linkTargets := map[string]string{}
	for _, name := range []string{"africa", "antarctica", "asia", "australasia", "europe", "northamerica", "southamerica", "etcetera", "backward"} {
		scanner := bufio.NewScanner(bytes.NewReader(readFile(filepath.Join(dir, name))))
		for scanner.Scan() {
			line := scanner.Text()
			if i := strings.Index(line, "#"); 0 <= i {
				line = line[:i]
			}
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "Link" {
				linkTargets[fields[2]] = fields[1]
			}
		}
	}
	return linkTargets
}

type city struct {
	country    string
	population int64
	zone       string
}

// readCities reads GeoNames' tab-separated cities: country code (column 9),
// population (15) and IANA zone (18).
func readCities(path string) []city {
	cities := []city{}
	scanner := bufio.NewScanner(bytes.NewReader(readFile(path)))
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 18 {
			fail("cities15000.txt: a row of %d fields", len(fields))
		}
		population, err := strconv.ParseInt(fields[14], 10, 64)
		if err != nil {
			fail("cities15000.txt: population %q", fields[14])
		}
		if fields[8] == "" || fields[17] == "" || population <= 0 {
			continue
		}
		cities = append(cities, city{country: fields[8], population: population, zone: fields[17]})
	}
	return cities
}

// resolveLink follows Link lines to the zone a name stands for.
func resolveLink(name string, linkTargets map[string]string) string {
	for range 8 {
		target, ok := linkTargets[name]
		if !ok {
			return name
		}
		name = target
	}
	return name
}

// countryZoneTable builds the zone table. Per country: the zones zone1970.tab
// lists for it and the zones of its cities, each city counted under the zone
// it resolves to. Zones whose offsets agree at every hour of the two years
// keep one clock (Brazil's UTC-3 zones, the US Eastern ones). The country's
// zone is the most populous zone of the clock that serves the most people,
// named as most of its people's cities name it (Europe/Copenhagen for
// Denmark, though zone1970.tab files it under Europe/Berlin). A country with
// one clock, or whose clock serves the dominant share, maps to that zone
// alone. Every other country keeps the zone and narrows the window: it opens
// later by the most the zone is ever ahead of the country's westernmost
// populated zone, and closes earlier by the most the easternmost is ever
// ahead of it. Those are read only at hours when the zone's own clock shows
// 06:00 to 23:00, the hours a send can go: the night hours when one zone has
// changed for daylight saving and another not yet would narrow the window all
// year for a gap no send falls in. A country with no cities maps to its first
// listed zone.
func countryZoneTable(
	countryZoneNames map[string][]string,
	linkTargets map[string]string,
	cities []city,
	zoneinfoZones map[string]*time.Location,
	dominantShare float64,
	startYear int,
	source string,
) []byte {
	type zonePeople struct {
		population int64
		// population by the name the cities give the zone
		namePopulations map[string]int64
	}
	countryZonePeople := map[string]map[string]*zonePeople{}
	for _, c := range cities {
		zone := resolveLink(c.zone, linkTargets)
		if _, ok := zoneinfoZones[zone]; !ok {
			continue
		}
		zonePeoples, ok := countryZonePeople[c.country]
		if !ok {
			zonePeoples = map[string]*zonePeople{}
			countryZonePeople[c.country] = zonePeoples
		}
		people, ok := zonePeoples[zone]
		if !ok {
			people = &zonePeople{namePopulations: map[string]int64{}}
			zonePeoples[zone] = people
		}
		people.population += c.population
		if _, ok := zoneinfoZones[c.zone]; ok {
			people.namePopulations[c.zone] += c.population
		}
	}

	countries := []string{}
	for country := range countryZoneNames {
		countries = append(countries, country)
	}
	for country := range countryZonePeople {
		if _, ok := countryZoneNames[country]; !ok {
			countries = append(countries, country)
		}
	}
	sort.Strings(countries)

	// hourly samples over two years: every transition of every zone
	samples := []time.Time{}
	for at := time.Date(startYear, 1, 1, 0, 0, 0, 0, time.UTC); at.Before(time.Date(startYear+2, 1, 1, 0, 0, 0, 0, time.UTC)); at = at.Add(time.Hour) {
		samples = append(samples, at)
	}
	offset := func(zone string, at time.Time) int {
		_, seconds := at.In(zoneinfoZones[zone]).Zone()
		return seconds
	}

	out := &bytes.Buffer{}
	fmt.Fprintf(out, "// Code generated by onboarding/gencountry; DO NOT EDIT.\n\n")
	fmt.Fprintf(out, "// SPDX-License-Identifier: MPL-2.0\n\n")
	fmt.Fprintf(out, "// Built from the IANA tz database (public domain) and GeoNames city\n// populations (https://www.geonames.org/, licensed under Creative Commons\n// Attribution 4.0). See onboarding/gencountry for the method.\n//\n")
	out.WriteString(source)
	fmt.Fprintf(out, "\npackage onboarding\n\n")
	fmt.Fprintf(out, "// countryCodeZones maps an alpha-2 country to the zone its readers' sends\n// are placed in when their own is unknown (CountrySendZone).\n")
	fmt.Fprintf(out, "var countryCodeZones = map[string]countryZone{\n")
	for _, country := range countries {
		zonePeoples := countryZonePeople[country]
		names := []string{}
		for _, name := range countryZoneNames[country] {
			if _, ok := zoneinfoZones[name]; ok {
				names = append(names, name)
			}
		}
		for zone := range zonePeoples {
			if !slices.Contains(names, zone) {
				names = append(names, zone)
			}
		}
		if len(names) == 0 {
			continue
		}
		populatedZones := []string{}
		var total int64
		for _, zone := range names {
			if people, ok := zonePeoples[zone]; ok && 0 < people.population {
				populatedZones = append(populatedZones, zone)
				total += people.population
			}
		}
		if len(populatedZones) == 0 {
			fmt.Fprintf(out, "\t%q: {zone: %q},\n", country, names[0])
			continue
		}
		sort.SliceStable(populatedZones, func(i int, j int) bool {
			return zonePeoples[populatedZones[i]].population > zonePeoples[populatedZones[j]].population
		})

		// group the zones by clock, each clock's people summed
		sameClock := func(a string, b string) bool {
			for _, at := range samples {
				if offset(a, at) != offset(b, at) {
					return false
				}
			}
			return true
		}
		clockZones := [][]string{}
		clockPopulations := []int64{}
		for _, zone := range populatedZones {
			grouped := false
			for i, zones := range clockZones {
				if sameClock(zones[0], zone) {
					clockZones[i] = append(zones, zone)
					clockPopulations[i] += zonePeoples[zone].population
					grouped = true
					break
				}
			}
			if !grouped {
				clockZones = append(clockZones, []string{zone})
				clockPopulations = append(clockPopulations, zonePeoples[zone].population)
			}
		}
		mainClock := 0
		for i := range clockZones {
			if clockPopulations[mainClock] < clockPopulations[i] {
				mainClock = i
			}
		}
		// the clock's zones are in population order
		main := clockZones[mainClock][0]
		mainName := main
		var mainNamePopulation int64
		for name, population := range zonePeoples[main].namePopulations {
			if mainNamePopulation < population || (population == mainNamePopulation && name < mainName) {
				mainName, mainNamePopulation = name, population
			}
		}
		share := float64(clockPopulations[mainClock]) / float64(total)

		comment := ""
		if len(clockZones) > 1 {
			comment = fmt.Sprintf(" // %d clocks, %.1f%% on %s's", len(clockZones), 100*share, main)
		}
		if len(clockZones) == 1 || dominantShare <= share {
			fmt.Fprintf(out, "\t%q: {zone: %q},%s\n", country, mainName, comment)
			continue
		}
		openLater, closeEarlier := 0, 0
		for _, at := range samples {
			if hour := at.In(zoneinfoZones[main]).Hour(); hour < 6 || 23 <= hour {
				continue
			}
			mainOffset := offset(main, at)
			westOffset, eastOffset := mainOffset, mainOffset
			for _, zones := range clockZones {
				zoneOffset := offset(zones[0], at)
				westOffset = min(westOffset, zoneOffset)
				eastOffset = max(eastOffset, zoneOffset)
			}
			openLater = max(openLater, mainOffset-westOffset)
			closeEarlier = max(closeEarlier, eastOffset-mainOffset)
		}
		fmt.Fprintf(out, "\t%q: {zone: %q, openLaterMinutes: %d, closeEarlierMinutes: %d},%s\n", country, mainName, openLater/60, closeEarlier/60, comment)
	}
	fmt.Fprintf(out, "}\n")
	return out.Bytes()
}

type supplementalData struct {
	Territories []struct {
		Type      string `xml:"type,attr"`
		Languages []struct {
			Type              string `xml:"type,attr"`
			PopulationPercent string `xml:"populationPercent,attr"`
			OfficialStatus    string `xml:"officialStatus,attr"`
		} `xml:"languagePopulation"`
	} `xml:"territoryInfo>territory"`
}

type languageShare struct {
	language string
	percent  float64
	official bool
}

func readTerritoryLanguages(path string) map[string][]languageShare {
	var data supplementalData
	if err := xml.Unmarshal(readFile(path), &data); err != nil {
		fail("%s: %s", path, err)
	}
	territoryLanguageShares := map[string][]languageShare{}
	for _, territory := range data.Territories {
		if len(territory.Type) != 2 {
			continue
		}
		for _, language := range territory.Languages {
			percent, err := strconv.ParseFloat(language.PopulationPercent, 64)
			if err != nil {
				fail("%s %s: populationPercent %q", territory.Type, language.Type, language.PopulationPercent)
			}
			// CLDR writes zh_Hant; the tables keep BCP 47
			territoryLanguageShares[territory.Type] = append(territoryLanguageShares[territory.Type], languageShare{
				language: strings.ReplaceAll(language.Type, "_", "-"),
				percent:  percent,
				// official, de_facto_official or official_regional
				official: language.OfficialStatus != "",
			})
		}
	}
	return territoryLanguageShares
}

// countryLanguageTable lists each territory's languages at or above the
// floor, the largest share first. "und" (no language known) is left out.
func countryLanguageTable(territoryLanguageShares map[string][]languageShare, floor float64, source string) []byte {
	territories := []string{}
	for territory := range territoryLanguageShares {
		territories = append(territories, territory)
	}
	sort.Strings(territories)

	out := &bytes.Buffer{}
	fmt.Fprintf(out, "// Code generated by onboarding/gencountry; DO NOT EDIT.\n\n")
	fmt.Fprintf(out, "// SPDX-License-Identifier: MPL-2.0\n\n")
	fmt.Fprintf(out, "// Built from Unicode CLDR territoryInfo (Unicode License v3). See\n// onboarding/gencountry for the method.\n//\n")
	out.WriteString(source)
	fmt.Fprintf(out, "\npackage onboarding\n\n")
	fmt.Fprintf(out, "// countryCodeLanguageShares maps an alpha-2 country to the languages CLDR\n// says its people use, first and second language, with the percent of the\n// population that uses each and whether it has an official standing there\n// (official, de facto or regional), the largest first; languages under %g%%\n// are left out.\n", floor)
	fmt.Fprintf(out, "var countryCodeLanguageShares = map[string][]languageShare{\n")
	for _, territory := range territories {
		shares := []languageShare{}
		for _, share := range territoryLanguageShares[territory] {
			if share.language != "und" && floor <= share.percent {
				shares = append(shares, share)
			}
		}
		if len(shares) == 0 {
			continue
		}
		sort.SliceStable(shares, func(i int, j int) bool {
			if shares[i].percent != shares[j].percent {
				return shares[i].percent > shares[j].percent
			}
			return shares[i].language < shares[j].language
		})
		parts := []string{}
		for _, share := range shares {
			official := ""
			if share.official {
				official = ", official: true"
			}
			parts = append(parts, fmt.Sprintf("{language: %q, percent: %s%s}", share.language, strconv.FormatFloat(share.percent, 'f', -1, 64), official))
		}
		fmt.Fprintf(out, "\t%q: {%s},\n", territory, strings.Join(parts, ", "))
	}
	fmt.Fprintf(out, "}\n")
	return out.Bytes()
}
