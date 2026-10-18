package controller

import (
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	mmdb "github.com/oschwald/maxminddb-golang/v2"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type MyIpInfoResult struct {
	Info               MyInfo `json:"info"`
	ConnectedToNetwork bool   `json:"connected_to_network"`
}

func GetMyIpInfo(session *session.ClientSession) (*MyIpInfoResult, error) {
	clientIp, _, err := session.ClientIpPort()
	if err != nil {
		return nil, err
	}

	location, _, err := GetLocationForIp(session.Ctx, clientIp)
	if err != nil {
		return nil, err
	}

	myInfo, err := NewMyInfo(clientIp, location)
	if err != nil {
		return nil, err
	}
	if addr, err := netip.ParseAddr(clientIp); err == nil {
		myInfo.Asn = GetIpAsn(addr)
	}

	return &MyIpInfoResult{
		Info:               myInfo,
		ConnectedToNetwork: model.IsIpConnectedToNetwork(session.Ctx, clientIp),
	}, nil
}

// Only what GeoLite2 can back for an ip (connect/GEOMAP.md §3.3). There is
// deliberately no privacy (vpn/proxy/hosting) verdict: GeoLite2 has none, and
// the egress prober's verdicts belong to probed provider connections, not to
// a caller's ip.
type MyInfo struct {
	IP       string      `json:"ip"`
	Location *IpLocation `json:"location,omitempty"`
	// the network the ip is announced by, from GeoLite2 ASN; left out when
	// the database is not deployed or has no record for the ip
	// (lag.dev/PLAN.md A3)
	Asn *IpAsn `json:"asn,omitempty"`
}

// An autonomous system (lag.dev/PLAN.md A3), as GeoLite2 ASN names it. Both
// fields are always written, so a reader never tests for a missing name.
type IpAsn struct {
	Number       uint32 `json:"number"`
	Organization string `json:"organization"`
}

type IpLocation struct {
	Coordinates *Coordinates `json:"coordinates,omitempty"`
	City        string       `json:"city,omitempty"`
	Region      string       `json:"region,omitempty"`
	Country     *IpCountry   `json:"country,omitempty"`
	Continent   *IpContinent `json:"continent,omitempty"`
	Timezone    string       `json:"timezone,omitempty"`
}

type IpContinent struct {
	Code string `json:"code,omitempty"`
	Name string `json:"name,omitempty"`
}

type IpCountry struct {
	Code string `json:"code,omitempty"`
	Name string `json:"name,omitempty"`
}

// The my-ip-info body of an address and the location it resolved to.
func NewMyInfo(clientIp string, location *model.Location) (MyInfo, error) {
	return MyInfo{
		IP: clientIp,
		Location: &IpLocation{
			Coordinates: &Coordinates{
				Longitude: location.Longitude,
				Latitude:  location.Latitude,
			},
			City:   location.City,
			Region: location.Region,
			Country: &IpCountry{
				Code: location.CountryCode,
				Name: location.Country,
			},
			Continent: &IpContinent{
				Code: location.ContinentCode,
				Name: location.Continent,
			},
			Timezone: location.Timezone,
		},
	}, nil
}

type Coordinates struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
}

func ParseCoordinates(ipInfoString string) (Coordinates, error) {
	parts := strings.Split(ipInfoString, ",")
	if len(parts) != 2 {
		return Coordinates{}, fmt.Errorf("invalid coordinates string: %q", ipInfoString)
	}

	lat, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return Coordinates{}, fmt.Errorf("failed to parse latitude %q: %w", parts[0], err)
	}

	lon, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return Coordinates{}, fmt.Errorf("failed to parse longitude %q: %w", parts[1], err)
	}

	return Coordinates{
		Latitude:  lat,
		Longitude: lon,
	}, nil
}

const (
	EarthRadiusMeters = 6371000.0
	SpeedOfLight      = 299792458.0
)

var EarthRadiusInLightSeconds = float64(EarthRadiusMeters) / float64(SpeedOfLight)

func (c Coordinates) CalculateDistanceInLightSecondsOnEarthSurface(other Coordinates) float64 {
	latA := degreesToRadians(c.Latitude)
	lonA := degreesToRadians(c.Longitude)
	latB := degreesToRadians(other.Latitude)
	lonB := degreesToRadians(other.Longitude)

	dLat := latB - latA
	dLon := lonB - lonA

	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(latA)*math.Cos(latB)*math.Sin(dLon/2)*math.Sin(dLon/2)
	d := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return EarthRadiusInLightSeconds * d
}

func degreesToRadians(degrees float64) float64 {
	return degrees * (math.Pi / 180.0)
}

// GeoLite2 ASN (lag.dev/PLAN.md A3): the autonomous system an address is
// announced by, which lag.dev shows as the visitor's network. It is a config
// mmdb deployed beside GeoLite2 City (`mmdb/geolite2.mmdb`), and optional: a
// deployment without it answers my-ip-info without `asn`.
//
// This product includes GeoLite2 data created by MaxMind, available from
// https://www.maxmind.com.

// The resource the database is deployed as, resolved like the City database.
const geoLite2AsnResourcePath = "mmdb/geolite2-asn.mmdb"

// The one database type the reader decodes. The record shape below is this
// type's, and the struct decoder would read another type's records as empty
// rather than fail, so another type is refused when the file is opened.
const geoLite2AsnDatabaseType = "GeoLite2-ASN"

// The part of a GeoLite2 ASN record the answer keeps.
type geoLite2AsnRecord struct {
	AutonomousSystemNumber       uint32 `maxminddb:"autonomous_system_number"`
	AutonomousSystemOrganization string `maxminddb:"autonomous_system_organization"`
}

// The deployed database, opened once per process, or nil when there is none.
var ipAsnDb = sync.OnceValue(loadIpAsnDb)

// Opens the deployed GeoLite2 ASN file. An absent or unusable file is logged
// and leaves the answer without `asn`, rather than failing the process: the
// City database is what my-ip-info cannot do without.
func loadIpAsnDb() *mmdb.Reader {
	path, err := server.Config.ResourcePath(geoLite2AsnResourcePath)
	if err != nil {
		glog.Infof("[ip]no GeoLite2 ASN database (%s); my-ip-info answers without asn\n", err)
		return nil
	}
	db, err := openIpAsnDb(path)
	if err != nil {
		glog.Errorf("[ip]GeoLite2 ASN database not used: %s\n", err)
		return nil
	}
	glog.Infof(
		"[ip]ip asn database type: %s (build %s)\n",
		db.Metadata.DatabaseType,
		db.Metadata.BuildTime().UTC().Format(time.DateOnly),
	)
	return db
}

// Opens the database at path and refuses any type but GeoLite2 ASN.
func openIpAsnDb(path string) (*mmdb.Reader, error) {
	db, err := mmdb.Open(path)
	if err != nil {
		return nil, err
	}
	if databaseType := db.Metadata.DatabaseType; databaseType != geoLite2AsnDatabaseType {
		db.Close()
		return nil, fmt.Errorf("ip asn database %s has type %q; only %q is supported", path, databaseType, geoLite2AsnDatabaseType)
	}
	return db, nil
}

// The autonomous system of addr in db, or nil when db is nil, the address has
// no record, or the record has no number.
func lookupIpAsn(db *mmdb.Reader, addr netip.Addr) *IpAsn {
	if db == nil || !addr.IsValid() {
		return nil
	}
	var record geoLite2AsnRecord
	if err := db.Lookup(addr.Unmap().WithZone("")).Decode(&record); err != nil {
		return nil
	}
	if record.AutonomousSystemNumber == 0 {
		return nil
	}
	return &IpAsn{
		Number:       record.AutonomousSystemNumber,
		Organization: record.AutonomousSystemOrganization,
	}
}

// The autonomous system of an address, or nil when it is unknown
// (lag.dev/PLAN.md A3, A7).
func GetIpAsn(addr netip.Addr) *IpAsn {
	return lookupIpAsn(ipAsnDb(), addr)
}
