package server

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mmdb "github.com/oschwald/maxminddb-golang/v2"

	"github.com/urnetwork/connect/v2026"
)

// Tests of the GeoLite2 City lookup: samples read from the deployed build when
// the checkout has it, and hand-built MaxMind DB files for the exact record
// decode and the database type check. Every sample address comes from
// MaxMind's own published test data (the MaxMind-DB repository), or is a
// documentation address where the lookup must find nothing.

// The GeoLite2 City build the expectations below were read from (build
// 2026-09-22), relative to this package. The tests that need it skip when it
// is absent, e.g. a checkout without the config repository. A newer build may
// move a city; these samples stay pinned to this file.
var geoLite2TestDatabase = filepath.Join("..", "config", "all", "mmdb", "2026.9.23", "geolite2.mmdb")

// The absolute path of geoLite2TestDatabase, skipping the test when it is
// absent.
func requireGeoLite2TestDatabase(t testing.TB) string {
	t.Helper()
	path, err := filepath.Abs(geoLite2TestDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("requires the GeoLite2 City database at %s: %s", path, err)
	}
	return path
}

// Makes GetIpInfo read the database at path for the rest of the test, resolved
// the way the runtime resolves it: a temporary WARP_CONFIG_HOME whose
// `all/mmdb/geolite2.mmdb` links to path, and a fresh `ipDb` once, restored
// afterwards so no other test sees this reader.
func useIpDbFile(t testing.TB, path string) {
	t.Helper()
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}

	// the overrides are a once too. Resolve them under the suite's own config
	// home first; resolved under the temporary home they would drop the
	// portable fixture's subnets for every later test in this process
	ipOverrides()

	home := t.TempDir()
	mmdbDir := filepath.Join(home, "all", "mmdb")
	if err := os.MkdirAll(mmdbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(mmdbDir, "geolite2.mmdb")
	if err := os.Symlink(path, linkPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_CONFIG_HOME", home)
	if resolvedPath, err := Config.ResourcePath("mmdb/geolite2.mmdb"); err != nil || resolvedPath != linkPath {
		t.Fatalf("the temporary config home resolves the database to %q (%v), not %q", resolvedPath, err, linkPath)
	}

	previousIpDb := ipDb
	var openedDb *mmdb.Reader
	ipDb = sync.OnceValues(func() (*mmdb.Reader, schemaType) {
		db, schemaType := loadIpDb()
		openedDb = db
		return db, schemaType
	})
	t.Cleanup(func() {
		ipDb = previousIpDb
		if openedDb != nil {
			openedDb.Close()
		}
	})
}

// An address and the IpInfo the test database resolves it to.
type geoLite2Sample struct {
	ip     string
	ipInfo IpInfo
}

// read from the 2026-09-22 build, at addresses of MaxMind's published test
// data. 81.2.69.142 and 2a02:d1c0::1 each have two subdivisions (a country
// part or region, then a borough or a province); 67.43.156.1 is located only
// to its country, at a 1000 km radius with no city or region
var geoLite2Samples = []geoLite2Sample{
	{
		ip: "81.2.69.142",
		ipInfo: IpInfo{
			ContinentCode:    "eu",
			Continent:        "Europe",
			CountryCode:      "gb",
			Country:          "United Kingdom",
			Region:           "England",
			Regions:          []string{"England", "Barnet"},
			City:             "East Finchley",
			Latitude:         51.5967,
			Longitude:        -0.1593,
			AccuracyRadiusKm: 200,
			Timezone:         "Europe/London",
			CityGeonameId:    2650444,
			RegionGeonameId:  6269131,
			CountryGeonameId: 2635167,
		},
	},
	{
		ip: "2a02:d1c0::1",
		ipInfo: IpInfo{
			ContinentCode:    "eu",
			Continent:        "Europe",
			CountryCode:      "it",
			Country:          "Italy",
			Region:           "Tuscany",
			Regions:          []string{"Tuscany", "Province of Massa-Carrara"},
			City:             "Aulla",
			Latitude:         44.2078,
			Longitude:        9.9753,
			AccuracyRadiusKm: 20,
			Timezone:         "Europe/Rome",
			CityGeonameId:    3182686,
			RegionGeonameId:  3165361,
			CountryGeonameId: 3175395,
		},
	},
	{
		ip: "67.43.156.1",
		ipInfo: IpInfo{
			ContinentCode:    "na",
			Continent:        "North America",
			CountryCode:      "us",
			Country:          "United States",
			Latitude:         37.751,
			Longitude:        -97.822,
			AccuracyRadiusKm: 1000,
			Timezone:         "America/Chicago",
			CountryGeonameId: 6252001,
		},
	},
}

// TEST-NET-3 (RFC 5737): never announced, so never in the database
const geoLite2NotFoundIp = "203.0.113.5"

// Each sample resolves to its pinned record, through every entry point and
// the v4-mapped form.
func TestIpInfoGeoLite2City(t *testing.T) {
	path := requireGeoLite2TestDatabase(t)
	useIpDbFile(t, path)

	for _, sample := range geoLite2Samples {
		addr := netip.MustParseAddr(sample.ip)
		if ipOverrideFor(addr) != nil {
			t.Fatalf("an ip_overrides subnet in this environment shadows %s", addr)
		}
		ipInfo, err := GetIpInfo(addr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *ipInfo, sample.ipInfo)
	}

	// the string and net.IP entry points reach the same record, and the
	// database aliases the v4-mapped v6 form to the v4 network
	for _, lookup := range []func() (*IpInfo, error){
		func() (*IpInfo, error) { return GetIpInfoFromString("81.2.69.142") },
		func() (*IpInfo, error) { return GetIpInfoFromIp(net.ParseIP("81.2.69.142")) },
		func() (*IpInfo, error) { return GetIpInfo(netip.MustParseAddr("::ffff:81.2.69.142")) },
	} {
		ipInfo, err := lookup()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *ipInfo, geoLite2Samples[0].ipInfo)
	}
}

// An address the database does not cover is not an error: it is an empty
// IpInfo, as it was with the databases GeoLite2 replaced. Callers read the
// empty country code as unknown (network_client_reliability_model,
// onboarding), and GetLocationForIp fails on it in GuessLocationType, so such
// a connection is located only by a fresh egress probe, if it has one.
func TestIpInfoGeoLite2CityNotFound(t *testing.T) {
	path := requireGeoLite2TestDatabase(t)
	useIpDbFile(t, path)

	addr := netip.MustParseAddr(geoLite2NotFoundIp)
	db, _ := ipDb()
	connect.AssertEqual(t, db.Lookup(addr).Found(), false)

	ipInfo, err := GetIpInfo(addr)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, *ipInfo, IpInfo{})
}

// The record for 81.2.69.142 names its country and continent in all eight
// database languages; the lookup keeps English and nothing else.
func TestIpInfoGeoLite2CityEnglishNamesOnly(t *testing.T) {
	path := requireGeoLite2TestDatabase(t)
	useIpDbFile(t, path)

	db, _ := ipDb()
	connect.AssertEqual(t, db.Metadata.Languages, []string{"de", "en", "es", "fr", "ja", "pt-BR", "ru", "zh-CN"})

	addr := netip.MustParseAddr("81.2.69.142")
	var countryLanguageNames map[string]string
	connect.AssertEqual(t, db.Lookup(addr).DecodePath(&countryLanguageNames, "country", "names"), nil)
	connect.AssertEqual(t, len(countryLanguageNames), 8)
	connect.AssertEqual(t, countryLanguageNames["de"], "UK")
	connect.AssertEqual(t, countryLanguageNames["fr"], "Royaume-Uni")
	var continentLanguageNames map[string]string
	connect.AssertEqual(t, db.Lookup(addr).DecodePath(&continentLanguageNames, "continent", "names"), nil)
	connect.AssertEqual(t, len(continentLanguageNames), 8)

	ipInfo, err := GetIpInfo(addr)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo.Country, countryLanguageNames["en"])
	connect.AssertEqual(t, ipInfo.Continent, continentLanguageNames["en"])
	connect.AssertEqual(t, ipInfo.Country, "United Kingdom")
	connect.AssertEqual(t, ipInfo.Continent, "Europe")
}

// A bound, like TestIpInfoPerf's, that only a pathological regression breaks
// (for example decoding every language of every name); the rate is printed
// for comparison. BenchmarkIpInfoGeoLite2City measures it properly.
func TestIpInfoGeoLite2CityPerf(t *testing.T) {
	path := requireGeoLite2TestDatabase(t)
	useIpDbFile(t, path)

	addrs := []netip.Addr{}
	for _, sample := range geoLite2Samples {
		addrs = append(addrs, netip.MustParseAddr(sample.ip))
	}
	addrs = append(addrs, netip.MustParseAddr(geoLite2NotFoundIp))
	for _, addr := range addrs {
		if _, err := GetIpInfo(addr); err != nil {
			t.Fatal(err)
		}
	}

	n := 100000
	startTime := time.Now()
	for index := range n {
		if _, err := GetIpInfo(addrs[index%len(addrs)]); err != nil {
			t.Fatal(err)
		}
	}
	duration := time.Since(startTime)
	fmt.Printf("[ip]geolite2 %d lookups per second (%s total)\n", int(float64(n)/duration.Seconds()), duration)
	connect.AssertEqual(t, duration <= 20*time.Second, true)
}

// The cost and allocations of a lookup over the samples.
func BenchmarkIpInfoGeoLite2City(b *testing.B) {
	path := requireGeoLite2TestDatabase(b)
	useIpDbFile(b, path)

	addrs := []netip.Addr{}
	for _, sample := range geoLite2Samples {
		addrs = append(addrs, netip.MustParseAddr(sample.ip))
	}
	b.ReportAllocs()
	for index := 0; b.Loop(); index += 1 {
		if _, err := GetIpInfo(addrs[index%len(addrs)]); err != nil {
			b.Fatal(err)
		}
	}
}

// Hand-built MaxMind DB data, encoded from the format's public specification
// (https://maxmind.github.io/MaxMind-DB/), so the record decode is tested
// against exact bytes and the database-type check runs without the real file.
// Only what these tests need is supported: sizes under 285, and pointers into
// the first 2 KiB of the data section.

// data section types, numbered as in the specification
const (
	mmdbTestTypePointer = 1
	mmdbTestTypeString  = 2
	mmdbTestTypeDouble  = 3
	mmdbTestTypeUint16  = 5
	mmdbTestTypeUint32  = 6
	mmdbTestTypeMap     = 7
	mmdbTestTypeUint64  = 9
	mmdbTestTypeArray   = 11
	mmdbTestTypeBool    = 14
)

// a map, in the key order it is written
type mmdbTestMap []mmdbTestPair

// One key and value of an mmdbTestMap.
type mmdbTestPair struct {
	key   string
	value any
}

// a pointer to a value already written at this data section offset
type mmdbTestPointer uint

// A data section being written.
type mmdbTestData struct {
	buffer []byte
}

// Writes value and returns the data section offset it was written at.
func (self *mmdbTestData) add(value any) uint {
	offset := uint(len(self.buffer))
	self.write(value)
	return offset
}

// Writes a control byte: the type in the top three bits and the size in the
// low five. Types above 7 are extended: the type bits are 0 and the next byte
// holds the type minus 7. A size from 29 is 29 plus one more byte.
func (self *mmdbTestData) control(typeNumber int, size int) {
	if 285 <= size {
		panic(fmt.Errorf("mmdb test data size %d is not supported", size))
	}
	sizeBits := min(size, 29)
	if typeNumber <= 7 {
		self.buffer = append(self.buffer, byte(typeNumber<<5|sizeBits))
	} else {
		self.buffer = append(self.buffer, byte(sizeBits), byte(typeNumber-7))
	}
	if 29 <= size {
		self.buffer = append(self.buffer, byte(size-29))
	}
}

// Encodes one value of the supported types, recursing into arrays and maps.
func (self *mmdbTestData) write(value any) {
	switch v := value.(type) {
	case string:
		self.control(mmdbTestTypeString, len(v))
		self.buffer = append(self.buffer, v...)
	case float64:
		self.control(mmdbTestTypeDouble, 8)
		self.buffer = binary.BigEndian.AppendUint64(self.buffer, math.Float64bits(v))
	case uint16:
		self.control(mmdbTestTypeUint16, 2)
		self.buffer = binary.BigEndian.AppendUint16(self.buffer, v)
	case uint32:
		self.control(mmdbTestTypeUint32, 4)
		self.buffer = binary.BigEndian.AppendUint32(self.buffer, v)
	case uint64:
		self.control(mmdbTestTypeUint64, 8)
		self.buffer = binary.BigEndian.AppendUint64(self.buffer, v)
	case bool:
		// a boolean has no payload; its size is its value
		size := 0
		if v {
			size = 1
		}
		self.control(mmdbTestTypeBool, size)
	case []any:
		self.control(mmdbTestTypeArray, len(v))
		for _, item := range v {
			self.write(item)
		}
	case mmdbTestMap:
		self.control(mmdbTestTypeMap, len(v))
		for _, pair := range v {
			self.write(pair.key)
			self.write(pair.value)
		}
	case mmdbTestPointer:
		// size bits 00vvv: a one-byte pointer whose offset is vvv followed
		// by the next byte
		if 2*1024 <= v {
			panic(fmt.Errorf("mmdb test pointer %d is not supported", v))
		}
		self.buffer = append(self.buffer, byte(mmdbTestTypePointer<<5|int(v>>8)), byte(v))
	default:
		panic(fmt.Errorf("mmdb test data type %T is not supported", value))
	}
}

// Lays out a whole database around one record, written into data after
// anything its pointers refer to: a one-node IPv4 search tree with 24-bit
// records, whose left half (0.0.0.0/1) holds the record and whose right half
// (128.0.0.0/1) is empty, then the 16-byte separator, the data section, the
// metadata marker and the metadata.
func mmdbTestFile(databaseType string, data *mmdbTestData, record any) []byte {
	recordOffset := data.add(record)

	nodeCount := uint32(1)
	// a tree record below node_count is a node, node_count itself is empty,
	// and above it is node_count + 16 + an offset into the data section
	treeRecord := func(value uint32) []byte {
		return binary.BigEndian.AppendUint32(nil, value)[1:]
	}
	fileBytes := []byte{}
	fileBytes = append(fileBytes, treeRecord(nodeCount+16+uint32(recordOffset))...)
	fileBytes = append(fileBytes, treeRecord(nodeCount)...)
	fileBytes = append(fileBytes, make([]byte, 16)...)
	fileBytes = append(fileBytes, data.buffer...)
	fileBytes = append(fileBytes, "\xAB\xCD\xEFMaxMind.com"...)
	metadata := &mmdbTestData{}
	metadata.write(mmdbTestMap{
		{key: "binary_format_major_version", value: uint16(2)},
		{key: "binary_format_minor_version", value: uint16(0)},
		{key: "build_epoch", value: uint64(1790110762)},
		{key: "database_type", value: databaseType},
		{key: "description", value: mmdbTestMap{{key: "en", value: "urnetwork test"}}},
		{key: "ip_version", value: uint16(4)},
		{key: "languages", value: []any{"en"}},
		{key: "node_count", value: nodeCount},
		{key: "record_size", value: uint16(24)},
	})
	fileBytes = append(fileBytes, metadata.buffer...)
	return fileBytes
}

// The 81.2.69.142 record in GeoLite2's own shape, including the keys the
// decoder skips. The country names are written once and shared through
// pointers, the way the database deduplicates them.
func geoLite2TestRecord(data *mmdbTestData) any {
	// eight languages, en neither first nor last, as GeoLite2 writes a country
	countryNames := mmdbTestPointer(data.add(mmdbTestMap{
		{key: "de", value: "UK"},
		{key: "en", value: "United Kingdom"},
		{key: "es", value: "Reino Unido"},
		{key: "fr", value: "Royaume-Uni"},
		{key: "ja", value: "英国"},
		{key: "pt-BR", value: "Reino Unido"},
		{key: "ru", value: "Британия"},
		{key: "zh-CN", value: "英国"},
	}))
	country := func() mmdbTestMap {
		return mmdbTestMap{
			{key: "geoname_id", value: uint32(2635167)},
			{key: "is_in_european_union", value: false},
			{key: "iso_code", value: "GB"},
			{key: "names", value: countryNames},
		}
	}
	return mmdbTestMap{
		{key: "city", value: mmdbTestMap{
			{key: "geoname_id", value: uint32(2650444)},
			{key: "names", value: mmdbTestMap{
				{key: "en", value: "East Finchley"},
				{key: "es", value: "East Finchley"},
				{key: "fr", value: "East Finchley"},
				{key: "ja", value: "イースト・フィンチリー"},
				{key: "ru", value: "Ист-Финчли"},
			}},
		}},
		{key: "continent", value: mmdbTestMap{
			{key: "code", value: "EU"},
			{key: "geoname_id", value: uint32(6255148)},
			{key: "names", value: mmdbTestMap{
				{key: "de", value: "Europa"},
				{key: "en", value: "Europe"},
				{key: "es", value: "Europa"},
				{key: "fr", value: "Europe"},
				{key: "ja", value: "ヨーロッパ"},
				{key: "pt-BR", value: "Europa"},
				{key: "ru", value: "Европа"},
				{key: "zh-CN", value: "欧洲"},
			}},
		}},
		{key: "country", value: country()},
		{key: "location", value: mmdbTestMap{
			{key: "accuracy_radius", value: uint16(200)},
			{key: "latitude", value: 51.5967},
			{key: "longitude", value: -0.1593},
			{key: "metro_code", value: uint16(820)},
			{key: "time_zone", value: "Europe/London"},
		}},
		{key: "postal", value: mmdbTestMap{{key: "code", value: "N2"}}},
		{key: "registered_country", value: country()},
		{key: "represented_country", value: mmdbTestMap{
			{key: "geoname_id", value: uint32(6252001)},
			{key: "iso_code", value: "US"},
			{key: "names", value: mmdbTestMap{{key: "en", value: "United States"}}},
			{key: "type", value: "military"},
		}},
		{key: "subdivisions", value: []any{
			mmdbTestMap{
				{key: "geoname_id", value: uint32(6269131)},
				{key: "iso_code", value: "ENG"},
				{key: "names", value: mmdbTestMap{
					{key: "de", value: "England"},
					{key: "en", value: "England"},
					{key: "es", value: "Inglaterra"},
					{key: "fr", value: "Angleterre"},
					{key: "ja", value: "イングランド"},
					{key: "pt-BR", value: "Inglaterra"},
					{key: "ru", value: "Англия"},
					{key: "zh-CN", value: "英格兰"},
				}},
			},
			mmdbTestMap{
				{key: "geoname_id", value: uint32(3333121)},
				{key: "iso_code", value: "BNE"},
				{key: "names", value: mmdbTestMap{
					{key: "de", value: "London Borough of Barnet"},
					{key: "en", value: "Barnet"},
					{key: "fr", value: "Barnet"},
				}},
			},
		}},
		// GeoIP2 Enterprise has traits; nothing in GeoLite2 City reads them
		{key: "traits", value: mmdbTestMap{{key: "is_anycast", value: true}}},
	}
}

// in 0.0.0.0/1, the half of an mmdbTestFile that holds its record
const geoLite2TestRecordIp = "81.2.69.142"

// Decodes record, written after whatever build wrote into the data section
// first, the way GetIpInfo does: from a one-record GeoLite2 City database,
// through lookupIpInfo.
func decodeGeoLite2TestRecord(data *mmdbTestData, record any) (IpInfo, error) {
	db, err := mmdb.OpenBytes(mmdbTestFile(string(schemaTypeGeoLite2City), data, record))
	if err != nil {
		return IpInfo{}, err
	}
	defer db.Close()
	ipInfo, err := lookupIpInfo(db, schemaTypeGeoLite2City, netip.MustParseAddr(geoLite2TestRecordIp))
	if err != nil {
		return IpInfo{}, err
	}
	return *ipInfo, nil
}

// The hand-built record decodes to the sample the real build gives for the
// same address.
func TestIpInfoDecodeGeoLite2City(t *testing.T) {
	data := &mmdbTestData{}
	ipInfo, err := decodeGeoLite2TestRecord(data, geoLite2TestRecord(data))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo, geoLite2Samples[0].ipInfo)
}

// A record without a city, subdivisions or English names decodes to what it
// has, and an empty record to an empty location.
func TestIpInfoDecodeGeoLite2CityPartialRecords(t *testing.T) {
	// no city and no subdivisions: a country-level answer
	data := &mmdbTestData{}
	ipInfo, err := decodeGeoLite2TestRecord(data, mmdbTestMap{
		{key: "country", value: mmdbTestMap{
			{key: "geoname_id", value: uint32(6252001)},
			{key: "iso_code", value: "US"},
			{key: "names", value: mmdbTestMap{{key: "de", value: "USA"}, {key: "en", value: "United States"}}},
		}},
		{key: "location", value: mmdbTestMap{
			{key: "accuracy_radius", value: uint16(1000)},
			{key: "latitude", value: 37.751},
			{key: "longitude", value: -97.822},
			{key: "time_zone", value: "America/Chicago"},
		}},
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo, IpInfo{
		CountryCode:      "us",
		Country:          "United States",
		Latitude:         37.751,
		Longitude:        -97.822,
		AccuracyRadiusKm: 1000,
		Timezone:         "America/Chicago",
		CountryGeonameId: 6252001,
	})

	// a place with no English name keeps its id, and an unnamed first
	// subdivision keeps Region and RegionGeonameId on the same subdivision
	// instead of promoting the second one's name
	data = &mmdbTestData{}
	ipInfo, err = decodeGeoLite2TestRecord(data, mmdbTestMap{
		{key: "city", value: mmdbTestMap{
			{key: "geoname_id", value: uint32(2651095)},
			{key: "names", value: mmdbTestMap{{key: "ru", value: "Доркинг"}}},
		}},
		{key: "subdivisions", value: []any{
			mmdbTestMap{
				{key: "geoname_id", value: uint32(6269131)},
				{key: "names", value: mmdbTestMap{{key: "de", value: "England"}}},
			},
			mmdbTestMap{
				{key: "geoname_id", value: uint32(2636512)},
				{key: "names", value: mmdbTestMap{{key: "en", value: "Surrey"}}},
			},
		}},
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo, IpInfo{
		Regions:         []string{"", "Surrey"},
		CityGeonameId:   2651095,
		RegionGeonameId: 6269131,
	})

	// an empty record is an empty location, not an error
	data = &mmdbTestData{}
	ipInfo, err = decodeGeoLite2TestRecord(data, mmdbTestMap{})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo, IpInfo{})
}

// geoname ids and the radius are read at any unsigned width the writer chose
func TestIpInfoDecodeGeoLite2CityIntegerWidths(t *testing.T) {
	data := &mmdbTestData{}
	ipInfo, err := decodeGeoLite2TestRecord(data, mmdbTestMap{
		{key: "city", value: mmdbTestMap{{key: "geoname_id", value: uint16(65535)}}},
		{key: "country", value: mmdbTestMap{{key: "geoname_id", value: uint64(4294967295)}}},
		{key: "location", value: mmdbTestMap{{key: "accuracy_radius", value: uint32(20000)}}},
		{key: "subdivisions", value: []any{mmdbTestMap{{key: "geoname_id", value: uint64(6269131)}}}},
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo.CityGeonameId, uint32(65535))
	connect.AssertEqual(t, ipInfo.CountryGeonameId, uint32(4294967295))
	connect.AssertEqual(t, ipInfo.RegionGeonameId, uint32(6269131))
	connect.AssertEqual(t, ipInfo.AccuracyRadiusKm, 20000)
	connect.AssertEqual(t, ipInfo.Regions, []string{""})
}

// A record whose values have the wrong type is an error, not an IpInfo. The
// one gap is a place map that is not a map, see
// TestIpInfoDecodeGeoLite2CityNonMapPlaceIsNotRejected.
func TestIpInfoDecodeGeoLite2CityRejectsMalformedRecords(t *testing.T) {
	for name, record := range map[string]any{
		"record not a map":         []any{"GB"},
		"geoname id as a string":   mmdbTestMap{{key: "city", value: mmdbTestMap{{key: "geoname_id", value: "2650444"}}}},
		"geoname id over 32 bits":  mmdbTestMap{{key: "country", value: mmdbTestMap{{key: "geoname_id", value: uint64(4294967296)}}}},
		"radius over 16 bits":      mmdbTestMap{{key: "location", value: mmdbTestMap{{key: "accuracy_radius", value: uint32(65536)}}}},
		"latitude not a double":    mmdbTestMap{{key: "location", value: mmdbTestMap{{key: "latitude", value: uint32(51)}}}},
		"subdivisions not a slice": mmdbTestMap{{key: "subdivisions", value: mmdbTestMap{}}},
	} {
		data := &mmdbTestData{}
		if _, err := decodeGeoLite2TestRecord(data, record); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}

	// a reader that was not opened as GeoLite2 City has no record shape to
	// decode into
	data := &mmdbTestData{}
	db, err := mmdb.OpenBytes(mmdbTestFile(string(schemaTypeGeoLite2City), data, geoLite2TestRecord(data)))
	connect.AssertEqual(t, err, nil)
	defer db.Close()
	ipInfo, err := lookupIpInfo(db, schemaType("GeoLite2-Country"), netip.MustParseAddr(geoLite2TestRecordIp))
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo == nil, true)
}

// maxminddb-golang v2.4.1 does not reject a struct-typed field (a `names` map,
// or city, continent, country, location) whose value is not a map: its struct
// decoder retries such a field from data section offset 0 instead of from the
// field. Usually the keys after it are then read out of step and the lookup
// fails, but a place that is the last key decodes from whatever is at offset
// 0 with no error, as here, where offset 0 is this record and has no `en`.
// MaxMind's GeoLite2 City writes every one of these as a map (all 338,904
// records of the 2026-09-22 build), and openIpDb loads no other type, so this
// is reachable only through a corrupt file. If this test starts failing
// because the decode errors, the library has fixed the retry: move this case
// into TestIpInfoDecodeGeoLite2CityRejectsMalformedRecords.
func TestIpInfoDecodeGeoLite2CityNonMapPlaceIsNotRejected(t *testing.T) {
	data := &mmdbTestData{}
	ipInfo, err := decodeGeoLite2TestRecord(data, mmdbTestMap{
		{key: "country", value: mmdbTestMap{{key: "names", value: "United Kingdom"}}},
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo, IpInfo{})
}

// Every database type but GeoLite2 City panics at open, naming the type.
func TestOpenIpDbRefusesOtherDatabaseTypes(t *testing.T) {
	dir := t.TempDir()
	for _, databaseType := range []string{
		"GeoLite2-Country",
		"GeoIP2-City",
		"DBIP-Location-ISP (compat=Enterprise)",
		"ipinfo bundle_location_core.mmdb",
		string(schemaTypeArinDb),
	} {
		path := filepath.Join(dir, "other.mmdb")
		data := &mmdbTestData{}
		if err := os.WriteFile(path, mmdbTestFile(databaseType, data, geoLite2TestRecord(data)), 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				r := recover()
				err, ok := r.(error)
				if !ok || !strings.Contains(err.Error(), fmt.Sprintf("\"%s\"", databaseType)) {
					t.Fatalf("opening a %s database: expected a panic naming the type, got %v", databaseType, r)
				}
			}()
			openIpDb(path)
		}()
	}
}

// The whole runtime path over a hand-built GeoLite2 City file, so it runs
// even where the real database is absent: resolution through the config
// home, the type check, the lookup, and a not-found address.
func TestGetIpInfoHandBuiltGeoLite2City(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geolite2.mmdb")
	data := &mmdbTestData{}
	fileBytes := mmdbTestFile(string(schemaTypeGeoLite2City), data, geoLite2TestRecord(data))
	if err := os.WriteFile(path, fileBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	useIpDbFile(t, path)

	// 0.0.0.0/1 holds the record
	addr := netip.MustParseAddr("81.2.69.142")
	connect.AssertEqual(t, ipOverrideFor(addr) == nil, true)
	ipInfo, err := GetIpInfo(addr)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, *ipInfo, geoLite2Samples[0].ipInfo)

	// 128.0.0.0/1 is empty
	ipInfo, err = GetIpInfo(netip.MustParseAddr(geoLite2NotFoundIp))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, *ipInfo, IpInfo{})

	// a lookup error is returned, not an empty location: this ipv4-only file
	// refuses a v6 address outright, so a documentation address will do
	ipInfo, err = GetIpInfo(netip.MustParseAddr("2001:db8::1"))
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, ipInfo == nil, true)
}
