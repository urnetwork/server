package server

import (
	// "bytes"
	// "encoding/hex"
	// "errors"
	"crypto/sha256"
	"fmt"
	// "io"
	"net"
	"net/netip"
	// "os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	// "sync"
	"math"

	// "database/sql/driver"

	// "github.com/jackc/pgx/v5/pgtype"
	// "github.com/oklog/ulid/v2"

	"github.com/urnetwork/glog/v2026"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"github.com/oschwald/maxminddb-golang/v2/mmdbdata"
)

func init() {
	OnWarmup(WarmupTargetIPDatabase, func() {
		db, _ := ipDb()
		// GeoLite2 must be kept current (connect/GEOMAP.md §3.1), so the
		// build date of the deployed file is logged with its type
		glog.Infof(
			"[ip]ip info database type: %s (build %s)\n",
			db.Metadata.DatabaseType,
			db.Metadata.BuildTime().UTC().Format(time.DateOnly),
		)

		arinDb()
	})
}

var clientIpHashPepper = sync.OnceValue(func() []byte {
	clientKeys := Vault.RequireSimpleResource("client.yml")
	pepper := clientKeys.RequireString("client_ip_hash_pepper")
	return []byte(pepper)
})

func ClientIpHash(clientIp string) ([32]byte, error) {
	addr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return [32]byte{}, err
	}
	return ClientIpHashForAddr(addr), nil
}

func ClientIpHashForAddr(addr netip.Addr) [32]byte {
	if addr.Is4() {
		// for ipv4, use the /29 network
		addr = netip.PrefixFrom(addr, 29).Masked().Addr()
	} else if addr.Is6() {
		// for ipv6, use the /56 network
		addr = netip.PrefixFrom(addr, 56).Masked().Addr()
	}
	h := sha256.New()
	h.Write(addr.AsSlice())
	h.Write(clientIpHashPepper())
	clientIpHash := h.Sum(nil)
	return [32]byte(clientIpHash)
}

// ClientIpHashForAddrPrefix is ClientIpHashForAddr with caller-chosen network
// prefixes instead of the fixed /29-v4 & /56-v6 bucketing. The `/verify` head
// routable-IP score (sn/VALIDATOR.md §8.1, D27) uses this because its "distinct
// IP" granularity is a subnet-configurable parameter (default /29 v4, /48 v6).
// Same keyed (peppered) hash, so raw egress ips are never stored.
func ClientIpHashForAddrPrefix(addr netip.Addr, v4Prefix int, v6Prefix int) [32]byte {
	addr = addr.Unmap()
	if addr.Is4() {
		addr = netip.PrefixFrom(addr, v4Prefix).Masked().Addr()
	} else if addr.Is6() {
		addr = netip.PrefixFrom(addr, v6Prefix).Masked().Addr()
	}
	h := sha256.New()
	h.Write(addr.AsSlice())
	h.Write(clientIpHashPepper())
	return [32]byte(h.Sum(nil))
}

// IpVersionForAddr is 4 or 6 for an address after unmapping the v4-mapped
// v6 form, and 0 for an invalid address. This is the "observed family" of a
// connection (connect/IPV6.md A3): what the packets actually arrived on.
func IpVersionForAddr(addr netip.Addr) int {
	addr = addr.Unmap()
	switch {
	case addr.Is4():
		return 4
	case addr.Is6():
		return 6
	default:
		return 0
	}
}

// ClientAddressIpVersion is IpVersionForAddr over a resolved client address
// ("ip:port", bracketed v6, or the malformed unbracketed v6 form). 0 when
// the address does not parse.
func ClientAddressIpVersion(clientAddress string) int {
	addrPort, err := ParseClientAddress(clientAddress)
	if err != nil {
		return 0
	}
	return IpVersionForAddr(addrPort.Addr())
}

func SplitClientAddress(clientAddress string) (host string, port int, err error) {
	columnCount := strings.Count(clientAddress, ":")
	bracketCount := strings.Count(clientAddress, "[")

	var portStr string
	if 1 < columnCount && bracketCount == 0 {
		// malformed ipv6. extract the address from the address:port string
		groups := malformedIPV6WithPort.FindStringSubmatch(clientAddress)
		if len(groups) != 3 {
			err = fmt.Errorf("Could not split malformed ipv6 client address.")
		} else {
			host = groups[1]
			portStr = groups[2]
		}
	} else {
		host, portStr, err = net.SplitHostPort(clientAddress)
	}
	if err != nil {
		// the client address might be just an ip
		_, parsedErr := netip.ParseAddr(clientAddress)
		if parsedErr == nil {
			host = clientAddress
			port = 0
			err = nil
		}
		return
	}
	port, err = strconv.Atoi(portStr)
	return
}

func ParseClientAddress(clientAddress string) (addrPort netip.AddrPort, err error) {
	var host string
	var port int
	host, port, err = SplitClientAddress(clientAddress)
	if err != nil {
		return
	}
	var addr netip.Addr
	addr, err = netip.ParseAddr(host)
	if err != nil {
		return
	}
	addrPort = netip.AddrPortFrom(addr, uint16(port))
	return
}

/*
func ParseClientAddress(clientAddress string) (ip string, port int, err error) {
	// ipv4:port
	// [ipv6]:port
	// ipv6:port

	ipv4 := regexp.MustCompile("^([0-9\\.]+):(\\d+)$")
	ipv6 := regexp.MustCompile("^\\[([0-9a-f:]+)\\]:(\\d+)$")
	// ip not properly escaped with [...]
	badIpv6 := regexp.MustCompile("^([0-9a-f:]+):(\\d+)$")

	groups := ipv4.FindStringSubmatch(clientAddress)
	if groups != nil {
		ip = groups[1]
		port, _ = strconv.Atoi(groups[2])
		return
	}

	groups = ipv6.FindStringSubmatch(clientAddress)
	if groups != nil {
		ip = groups[1]
		port, _ = strconv.Atoi(groups[2])
		return
	}

	groups = badIpv6.FindStringSubmatch(clientAddress)
	if groups != nil {
		ip = groups[1]
		port, _ = strconv.Atoi(groups[2])
		return
	}

	err = fmt.Errorf("Client address does not match ipv4 or ipv6 spec: %s", clientAddress)
	return
}
*/

// matches the first group to the IPV6 address when the input is <ipv6>:<port>
// example: 2001:5a8:4683:4e00:3a76:dcec:7cb:f180:40894
var malformedIPV6WithPort = regexp.MustCompile(`^(.+):(\d+)$`)

// Scrubbing

var errorIpv4 = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){3,3}`)
var errorIpv4Port = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){3,3}:[0-9]+`)
var errorIpv6 = regexp.MustCompile(`[0-9]+(?::[0-9]+){,15}(?:::[0-9]+)?`)
var errorIpv6Port = regexp.MustCompile(`[0-9]+(?::[0-9]+){,15}(?:::[0-9]+)?:[0-9]+`)

func ScrubIpPort(s string) string {
	s = errorIpv4Port.ReplaceAllString(s, `ipv4:port`)
	s = errorIpv4.ReplaceAllString(s, `ipv4`)
	s = errorIpv6Port.ReplaceAllString(s, `ipv6:port`)
	s = errorIpv6.ReplaceAllString(s, `ipv6`)
	return s
}

// Packaged ip metadata

type schemaType string

const (
	schemaTypeGeoLite2City schemaType = "GeoLite2-City"
	schemaTypeArinDb       schemaType = "urnetwork arindb"
)

// MaxMind GeoLite2 City (connect/GEOMAP.md §3), the one packaged source of
// location. It carries no ip-quality verdicts: hosting and proxy come only
// from the egress prober, and the foreign check from our own ARIN build
// (`arinDb`).
var ipDb = sync.OnceValues(loadIpDb)

// Opens the deployed GeoLite2 City file of the config resources.
func loadIpDb() (*mmdb.Reader, schemaType) {
	path, err := Config.ResourcePath("mmdb/geolite2.mmdb")
	if err != nil {
		panic(err)
	}
	return openIpDb(path)
}

// Opens the database at path and refuses any type but GeoLite2 City.
// geoLite2CityRecord describes that one record shape, and the struct decoder
// skips keys it does not know, so another type would not fail a lookup: it
// would answer every address empty, or with different data if it shares the
// GeoIP2 layout. Refusing it here fails the deploy at warmup instead.
func openIpDb(path string) (*mmdb.Reader, schemaType) {
	db, err := mmdb.Open(path)
	if err != nil {
		panic(err)
	}
	if databaseType := db.Metadata.DatabaseType; schemaType(databaseType) != schemaTypeGeoLite2City {
		db.Close()
		panic(fmt.Errorf("ip database %s has type \"%s\"; only \"%s\" is supported", path, databaseType, schemaTypeGeoLite2City))
	}
	return db, schemaTypeGeoLite2City
}

type IpInfo struct {
	// continent code is lowercase
	ContinentCode string
	Continent     string
	// country code is lowercase
	CountryCode string
	Country     string
	// the first subdivision. GeoLite2 lists subdivisions largest first
	// (England, then Barnet), so this is the one a location is filed under
	Region string
	// every subdivision, largest first. Regions[i] names subdivision i and is
	// empty when that subdivision has no English name
	Regions   []string
	City      string
	Longitude float64
	Latitude  float64
	// the radius around the coordinates within which MaxMind places the
	// address with 67% confidence, from a few km to 1000 for an address
	// located only to its country. It is the genesis confidence of
	// connect/GEOMAP.md §5. 0 when unknown
	AccuracyRadiusKm int
	Timezone         string
	// GeoNames ids of the city, the first subdivision (`Region`) and the
	// country; 0 when the record has no such place
	CityGeonameId    uint32
	RegionGeonameId  uint32
	CountryGeonameId uint32
}

// The part of a GeoLite2 City record that IpInfo keeps, for the library's
// struct decoder (https://dev.maxmind.com/geoip/docs/databases/city-and-country/).
// The decoder skips every key without a field here -- postal, registered and
// represented country, and every language but `en` of each `names` map --
// without decoding it. An integer stored at another unsigned width than the
// field's still decodes, and one too large for its field is an error. So is
// every other wrong type, except a place (or `names`) that is not a map,
// which maxminddb-golang v2.4.1 does not reliably reject; only a corrupt file
// has one (see TestIpInfoDecodeGeoLite2CityNonMapPlaceIsNotRejected).
type geoLite2CityRecord struct {
	City struct {
		GeonameId uint32        `maxminddb:"geoname_id"`
		Names     geoLite2Names `maxminddb:"names"`
	} `maxminddb:"city"`
	Continent struct {
		Code  string        `maxminddb:"code"`
		Names geoLite2Names `maxminddb:"names"`
	} `maxminddb:"continent"`
	Country struct {
		GeonameId uint32        `maxminddb:"geoname_id"`
		IsoCode   string        `maxminddb:"iso_code"`
		Names     geoLite2Names `maxminddb:"names"`
	} `maxminddb:"country"`
	Location struct {
		AccuracyRadius uint16  `maxminddb:"accuracy_radius"`
		Latitude       float64 `maxminddb:"latitude"`
		Longitude      float64 `maxminddb:"longitude"`
		TimeZone       string  `maxminddb:"time_zone"`
	} `maxminddb:"location"`
	// largest first
	Subdivisions []struct {
		GeonameId uint32        `maxminddb:"geoname_id"`
		Names     geoLite2Names `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
}

// A `names` map with only the English name decoded.
type geoLite2Names struct {
	En string `maxminddb:"en"`
}

// test/simulation ip overrides
//
// `ip_overrides` in settings (config or site settings.yml) defines subnets
// whose ip info is served from configuration instead of the packaged mmdb
// databases. Local simulation environments (e.g. sim-latency) use this to
// give fake testing subnets a location; production settings do not define
// the key, so the packaged databases serve every lookup.
//
//	ip_overrides:
//	  - subnet: "198.18.0.0/16"
//	    country_code: "zz"
//	    country: "Sim"
//	    region: "Sim"
//	    city: "Sim"
//
// optional fields: continent, continent_code, latitude, longitude, timezone,
// accuracy_radius_km, city_geoname_id, region_geoname_id, country_geoname_id.
// `hosting`, `privacy` and `virtual` are still accepted so that existing
// settings keep loading, but they are ignored: no ip lookup yields those
// verdicts any more (the egress prober is their only source). An overridden
// subnet also short-circuits `GetArinInfo` with a non-foreign org, so an
// overridden address never scores as foreign either. Malformed entries panic
// at first lookup: an override is only ever present deliberately, and a
// silently skipped entry would make a simulation quietly wrong.
type ipOverride struct {
	prefix netip.Prefix
	ipInfo IpInfo
}

var ipOverrides = sync.OnceValue(func() []*ipOverride {
	settingsObj, ok := GetSettings()["ip_overrides"]
	if !ok {
		return []*ipOverride{}
	}
	overrides := parseIpOverrides(settingsObj)
	if 0 < len(overrides) {
		glog.Infof("[ip]%d ip override subnets active\n", len(overrides))
	}
	return overrides
})

func parseIpOverrides(settingsObj any) []*ipOverride {
	entries, ok := settingsObj.([]any)
	if !ok {
		panic(fmt.Errorf("ip_overrides must be a list"))
	}
	overrides := []*ipOverride{}
	for _, entryObj := range entries {
		entry, ok := entryObj.(map[string]any)
		if !ok {
			panic(fmt.Errorf("ip_overrides entry must be a map"))
		}
		stringValue := func(key string) string {
			if v, ok := entry[key]; ok {
				if s, ok := v.(string); ok {
					return s
				}
				panic(fmt.Errorf("ip_overrides %s must be a string", key))
			}
			return ""
		}
		floatValue := func(key string) float64 {
			if v, ok := entry[key]; ok {
				switch f := v.(type) {
				case float64:
					return f
				case int:
					return float64(f)
				}
				panic(fmt.Errorf("ip_overrides %s must be a number", key))
			}
			return 0
		}
		intValue := func(key string) int {
			if v, ok := entry[key]; ok {
				if i, ok := v.(int); ok && 0 <= i {
					return i
				}
				panic(fmt.Errorf("ip_overrides %s must be a non-negative integer", key))
			}
			return 0
		}
		uint32Value := func(key string) uint32 {
			if v, ok := entry[key]; ok {
				if i, ok := v.(int); ok && 0 <= i && uint64(i) <= math.MaxUint32 {
					return uint32(i)
				}
				panic(fmt.Errorf("ip_overrides %s must be an integer from 0 to %d", key, uint64(math.MaxUint32)))
			}
			return 0
		}

		prefix, err := netip.ParsePrefix(stringValue("subnet"))
		if err != nil {
			panic(fmt.Errorf("ip_overrides subnet %q: %w", stringValue("subnet"), err))
		}

		region := stringValue("region")
		regions := []string{}
		if region != "" {
			regions = []string{region}
		}
		overrides = append(overrides, &ipOverride{
			prefix: prefix,
			ipInfo: IpInfo{
				ContinentCode:    strings.ToLower(stringValue("continent_code")),
				Continent:        stringValue("continent"),
				CountryCode:      strings.ToLower(stringValue("country_code")),
				Country:          stringValue("country"),
				Region:           region,
				Regions:          regions,
				City:             stringValue("city"),
				Longitude:        floatValue("longitude"),
				Latitude:         floatValue("latitude"),
				AccuracyRadiusKm: intValue("accuracy_radius_km"),
				Timezone:         stringValue("timezone"),
				CityGeonameId:    uint32Value("city_geoname_id"),
				RegionGeonameId:  uint32Value("region_geoname_id"),
				CountryGeonameId: uint32Value("country_geoname_id"),
			},
		})
	}
	return overrides
}

func ipOverrideFor(addr netip.Addr) *IpInfo {
	for _, override := range ipOverrides() {
		if override.prefix.Contains(addr) {
			// copy so callers cannot mutate the shared template
			ipInfo := override.ipInfo
			ipInfo.Regions = slices.Clone(ipInfo.Regions)
			return &ipInfo
		}
	}
	return nil
}

func GetIpInfoFromString(ip string) (*IpInfo, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil, err
	}
	return GetIpInfo(addr)
}

func GetIpInfoFromIp(ip net.IP) (*IpInfo, error) {
	if ipv4 := ip.To4(); ipv4 != nil {
		addr := netip.AddrFrom4([4]byte(ipv4))
		return GetIpInfo(addr)
	} else if ipv6 := ip.To16(); ipv6 != nil {
		addr := netip.AddrFrom16([16]byte(ipv6))
		return GetIpInfo(addr)
	} else {
		return nil, fmt.Errorf("Unknown ip size.")
	}
}

// Resolves addr from the `ip_overrides` settings when a subnet there covers
// it, and from GeoLite2 otherwise.
//
// An address GeoLite2 has no record for (reserved, private or unannounced
// space) is not an error. The result is an empty IpInfo, as it was with the
// databases before, and callers read its empty CountryCode as unknown;
// GetLocationForIp, which cannot classify an empty location, errors then.
func GetIpInfo(addr netip.Addr) (*IpInfo, error) {
	if ipInfo := ipOverrideFor(addr); ipInfo != nil {
		return ipInfo, nil
	}

	ipDb, schemaType := ipDb()
	return lookupIpInfo(ipDb, schemaType, addr)
}

// Reads addr from db, opened as schemaType by openIpDb.
func lookupIpInfo(db *mmdb.Reader, schemaType schemaType, addr netip.Addr) (*IpInfo, error) {
	switch schemaType {
	case schemaTypeGeoLite2City:
		// a record-less address leaves the record empty, and an empty
		// record is an empty IpInfo
		var record geoLite2CityRecord
		if err := db.Lookup(addr).Decode(&record); err != nil {
			return nil, err
		}
		ipInfo := &IpInfo{
			ContinentCode:    strings.ToLower(record.Continent.Code),
			Continent:        record.Continent.Names.En,
			CountryCode:      strings.ToLower(record.Country.IsoCode),
			Country:          record.Country.Names.En,
			City:             record.City.Names.En,
			Longitude:        record.Location.Longitude,
			Latitude:         record.Location.Latitude,
			AccuracyRadiusKm: int(record.Location.AccuracyRadius),
			Timezone:         record.Location.TimeZone,
			CityGeonameId:    record.City.GeonameId,
			CountryGeonameId: record.Country.GeonameId,
		}
		if 0 < len(record.Subdivisions) {
			regions := make([]string, len(record.Subdivisions))
			for i, subdivision := range record.Subdivisions {
				regions[i] = subdivision.Names.En
			}
			// the region and its id always describe the same subdivision,
			// even when that subdivision has no English name
			ipInfo.Region = regions[0]
			ipInfo.Regions = regions
			ipInfo.RegionGeonameId = record.Subdivisions[0].GeonameId
		}
		return ipInfo, nil
	default:
		return nil, fmt.Errorf("Unknown schema type: %s", schemaType)
	}
}

var HostLatituteLongitude = sync.OnceValues(func() (latitude float64, longitude float64) {
	settings := GetSettings()

	if s, ok := settings["latitude"]; ok {
		if v, ok := s.(float64); ok {
			latitude = v
		}
	}
	if s, ok := settings["longitude"]; ok {
		if v, ok := s.(float64); ok {
			longitude = v
		}
	}
	return
})

func DistanceMillis(
	lat1 float64,
	lon1 float64,
	lat2 float64,
	lon2 float64,
) float64 {
	km := DistanceKm(lat1, lon1, lat2, lon2)
	lightKmPerMillisecond := 299.792458
	millis := km / lightKmPerMillisecond
	return millis
}

// from https://github.com/umahmood/haversine
func DistanceKm(
	lat1 float64,
	lon1 float64,
	lat2 float64,
	lon2 float64,
) float64 {
	degreesToRadians := func(d float64) float64 {
		return d * math.Pi / 180.0
	}

	lat1 = degreesToRadians(lat1)
	lon1 = degreesToRadians(lon1)
	lat2 = degreesToRadians(lat2)
	lon2 = degreesToRadians(lon2)

	diffLat := lat2 - lat1
	diffLon := lon2 - lon1

	a := math.Pow(math.Sin(diffLat/2), 2) + math.Cos(lat1)*math.Cos(lat2)*
		math.Pow(math.Sin(diffLon/2), 2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	earthRadiusKm := 6371.0
	km := c * earthRadiusKm

	return km
}

var arinDb = sync.OnceValues(func() (*mmdb.Reader, schemaType) {
	path, err := Config.ResourcePath("arindb/arin.mmdb")
	if err != nil {
		panic(err)
	}

	db, err := mmdb.Open(path)
	if err != nil {
		panic(err)
	}

	return db, schemaType(db.Metadata.DatabaseType)
})

type ArinInfo struct {
	schemaType      schemaType
	OrgCountryCodes []string
}

func (self *ArinInfo) UnmarshalMaxMindDB(d *mmdbdata.Decoder) error {
	switch self.schemaType {
	case schemaTypeArinDb:
		return self.unmarshalArinDb(d)
	default:
		return fmt.Errorf("Unknown schema type: %s", self.schemaType)
	}
}

func (self *ArinInfo) unmarshalArinDb(d *mmdbdata.Decoder) error {
	mapIter, _, err := d.ReadMap()
	if err != nil {
		return err
	}
	for key, err := range mapIter {
		if err != nil {
			return err
		}

		switch string(key) {
		case "org_country_codes":
			iter, n, err := d.ReadSlice()
			if err != nil {
				return err
			}
			orgCountryCodes := make([]string, 0, n)
			for range iter {
				countryCode, err := d.ReadString()
				if err != nil {
					return err
				}
				orgCountryCodes = append(orgCountryCodes, strings.ToLower(countryCode))
			}
			self.OrgCountryCodes = orgCountryCodes
		default:
			// glog.Infof("[ip]decode skip key \"%s\"\n", key)
			if err := d.SkipValue(); err != nil {
				return err
			}
		}
	}

	return nil
}

func GetArinInfoFromString(ip string) (*ArinInfo, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil, err
	}
	return GetArinInfo(addr)
}

func GetArinInfoFromIp(ip net.IP) (*ArinInfo, error) {
	if ipv4 := ip.To4(); ipv4 != nil {
		addr := netip.AddrFrom4([4]byte(ipv4))
		return GetArinInfo(addr)
	} else if ipv6 := ip.To16(); ipv6 != nil {
		addr := netip.AddrFrom16([16]byte(ipv6))
		return GetArinInfo(addr)
	} else {
		return nil, fmt.Errorf("Unknown ip size.")
	}
}

func GetArinInfo(addr netip.Addr) (*ArinInfo, error) {
	if ipInfo := ipOverrideFor(addr); ipInfo != nil {
		// org matches the override country, so the net type is never foreign
		return &ArinInfo{
			OrgCountryCodes: []string{ipInfo.CountryCode},
		}, nil
	}

	arinDb, schemaType := arinDb()

	r := arinDb.Lookup(addr)
	arinInfo := ArinInfo{
		schemaType: schemaType,
	}
	err := r.Decode(&arinInfo)
	if err != nil {
		return nil, err
	}
	return &arinInfo, nil
}
