package controller

import (
	"context"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tests that lag.dev keeps no visitor address (lag.dev/PLAN.md amendment 9):
// a track keeps only the network and city-level place of the address it was
// issued to, and a whole visit leaves no form of the address in any lag
// table. The visitor's place is looked up as production looks it up, in a
// GeoLite2 City database and the place list beside it, both built here: no
// test replaces the lookup.

// One GeoLite2 City record, in GeoLite2 City's own shape, for a block of a
// hand-built database. An empty city or region writes a record without one,
// as GeoLite2 does for a block it places only in a region or a country.
type lagTestCityBlock struct {
	countryCode      string
	country          string
	countryGeonameId uint32
	region           string
	regionGeonameId  uint32
	city             string
	cityGeonameId    uint32
	// where GeoLite2 places the block: a postal area's point within a city,
	// or a point of the region or country
	latitude  float64
	longitude float64
}

// Writes a GeoLite2 City database with one record per block and makes the
// GeoLite2 City lookup read it for the rest of the test.
func useLagTestCityDb(t testing.TB, blocks map[string]lagTestCityBlock) {
	t.Helper()
	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "GeoLite2-City",
		IncludeReservedNetworks: true,
		RecordSize:              28,
	})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, block := range blocks {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		names := func(name string) mmdbtype.Map {
			return mmdbtype.Map{"en": mmdbtype.String(name)}
		}
		record := mmdbtype.Map{
			"continent": mmdbtype.Map{
				"code":  mmdbtype.String("EU"),
				"names": names("Europe"),
			},
			"country": mmdbtype.Map{
				"geoname_id": mmdbtype.Uint32(block.countryGeonameId),
				"iso_code":   mmdbtype.String(strings.ToUpper(block.countryCode)),
				"names":      names(block.country),
			},
			"location": mmdbtype.Map{
				"accuracy_radius": mmdbtype.Uint16(20),
				"latitude":        mmdbtype.Float64(block.latitude),
				"longitude":       mmdbtype.Float64(block.longitude),
			},
		}
		if block.region != "" {
			record["subdivisions"] = mmdbtype.Slice{mmdbtype.Map{
				"geoname_id": mmdbtype.Uint32(block.regionGeonameId),
				"names":      names(block.region),
			}}
		}
		if block.city != "" {
			record["city"] = mmdbtype.Map{
				"geoname_id": mmdbtype.Uint32(block.cityGeonameId),
				"names":      names(block.city),
			}
		}
		if err := writer.Insert(network, record); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "geolite2.mmdb")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteTo(file); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	server.UseIpDbFileForTest(t, path)
}

// The place list beside the test databases: each city's own point, which is
// none of its blocks' points.
const lagTestPlaces = `
version: 1
source: test
build_epoch: 1
countries:
  de: {name: Germany, geoname_id: 2921044, continent_code: eu, continent: Europe}
  fr: {name: France, geoname_id: 3017382, continent_code: eu, continent: Europe}
  jp: {name: Japan, geoname_id: 1861060, continent_code: as, continent: Asia}
places:
  de:
    Land Berlin:
      Berlin: {geoname_id: 2950159, region_geoname_id: 2950157, latitude: 52.5196, longitude: 13.4069, spread_km: 21.6, time_zone: Europe/Berlin}
  fr:
    Auvergne-Rhone-Alpes:
      Lyon: {geoname_id: 2996944, region_geoname_id: 11071625, latitude: 45.7485, longitude: 4.8467, spread_km: 9.8, time_zone: Europe/Paris}
  jp:
    Osaka:
      Osaka: {geoname_id: 1853909, region_geoname_id: 1853904, latitude: 34.6937, longitude: 135.5022, spread_km: 14.2, time_zone: Asia/Tokyo}
`

// Two Berlin blocks, each at its own postal area's point as GeoLite2 City
// places it, and neither the city's point.
var (
	lagTestBerlinNorth = lagTestCityBlock{
		countryCode: "de", country: "Germany", countryGeonameId: 2921044,
		region: "Land Berlin", regionGeonameId: 2950157,
		city: "Berlin", cityGeonameId: 2950159,
		latitude: 52.5548, longitude: 13.3889,
	}
	lagTestBerlinSouth = lagTestCityBlock{
		countryCode: "de", country: "Germany", countryGeonameId: 2921044,
		region: "Land Berlin", regionGeonameId: 2950157,
		city: "Berlin", cityGeonameId: 2950159,
		latitude: 52.4436, longitude: 13.4495,
	}
)

// A new track's client fields are the autonomous system and the city-level
// place of the address it is issued to, looked up when it is issued, with
// the city's own point from the place list. Two addresses in different
// blocks of one city keep the same point, the city's, and never their
// block's. A place that names no city keeps no coordinates, nor does a city
// the place list does not hold or a deployment without a list, and a lookup
// that cannot answer leaves its fields empty.
func TestLagTrackClientFields(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		// the extender's place is the sample's, which these tests set
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		useIpAsnDb(t, writeTestIpAsnDb(t, geoLite2AsnDatabaseType, map[string]IpAsn{
			"198.51.100.0/24": {Number: 64500, Organization: "Example Access"},
		}))
		useLagTestCityDb(t, map[string]lagTestCityBlock{
			"198.51.100.0/25":   lagTestBerlinNorth,
			"198.51.100.128/26": lagTestBerlinSouth,
			// GeoLite2 knows only the region here, and gives a point in it
			"198.51.100.192/27": {
				countryCode: "de", country: "Germany", countryGeonameId: 2921044,
				region: "Land Berlin", regionGeonameId: 2950157,
				latitude: 52.5167, longitude: 13.4,
			},
			// only the country, with the country's point
			"198.51.100.224/28": {
				countryCode: "fr", country: "France", countryGeonameId: 3017382,
				latitude: 46.0, longitude: 2.0,
			},
			// a city the place list does not hold
			"198.51.100.240/28": {
				countryCode: "fr", country: "France", countryGeonameId: 3017382,
				region: "Auvergne-Rhone-Alpes", regionGeonameId: 11071625,
				city: "Villeurbanne", cityGeonameId: 2968254,
				latitude: 45.7719, longitude: 4.8902,
			},
		})
		defer model.Testing_PushPlaces(lagTestPlaces)()
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")

		issue := func(clientAddress string) *testLagTrackClientRow {
			clientSession := newLagTestGuestSession(t, ctx, clientAddress)
			result, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
				{ExtenderId: extender.extenderId},
			}}, clientSession, server.NowUtc(), settings)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, (*LagError)(nil))
			return getTestLagTrackClientRow(t, ctx, *result.TrackId)
		}

		// two blocks of one city: the city's point for both, never a block's
		north := issue("198.51.100.30:1234")
		south := issue("198.51.100.150:1234")
		for _, row := range []*testLagTrackClientRow{north, south} {
			connect.AssertEqual(t, *row.asn, int64(64500))
			connect.AssertEqual(t, *row.asnOrg, "Example Access")
			connect.AssertEqual(t, *row.countryCode, "de")
			connect.AssertEqual(t, *row.region, "Land Berlin")
			connect.AssertEqual(t, *row.city, "Berlin")
			connect.AssertEqual(t, *row.latitude, 52.5196)
			connect.AssertEqual(t, *row.longitude, 13.4069)
		}
		for _, block := range []lagTestCityBlock{lagTestBerlinNorth, lagTestBerlinSouth} {
			for _, row := range []*testLagTrackClientRow{north, south} {
				if *row.latitude == block.latitude || *row.longitude == block.longitude {
					t.Fatalf("a track keeps a block's own point (%v, %v)", block.latitude, block.longitude)
				}
			}
		}

		// a region alone keeps no coordinates
		row := issue("198.51.100.200:1234")
		connect.AssertEqual(t, *row.countryCode, "de")
		connect.AssertEqual(t, *row.region, "Land Berlin")
		connect.AssertEqual(t, row.city, (*string)(nil))
		connect.AssertEqual(t, row.latitude, (*float64)(nil))
		connect.AssertEqual(t, row.longitude, (*float64)(nil))

		// a country alone keeps no coordinates
		row = issue("198.51.100.230:1234")
		connect.AssertEqual(t, *row.countryCode, "fr")
		connect.AssertEqual(t, row.region, (*string)(nil))
		connect.AssertEqual(t, row.city, (*string)(nil))
		connect.AssertEqual(t, row.latitude, (*float64)(nil))
		connect.AssertEqual(t, row.longitude, (*float64)(nil))

		// a city the place list does not hold keeps its name, and no point
		// rather than its block's
		row = issue("198.51.100.245:1234")
		connect.AssertEqual(t, *row.city, "Villeurbanne")
		connect.AssertEqual(t, row.latitude, (*float64)(nil))
		connect.AssertEqual(t, row.longitude, (*float64)(nil))

		// without a place list, no city has a point, and none keeps its
		// block's
		func() {
			defer model.Testing_PushPlaces("version: 0\n")()
			row := issue("198.51.100.30:1234")
			connect.AssertEqual(t, *row.city, "Berlin")
			connect.AssertEqual(t, row.latitude, (*float64)(nil))
			connect.AssertEqual(t, row.longitude, (*float64)(nil))
		}()

		// neither lookup knows this address
		row = issue("203.0.113.9:1234")
		connect.AssertEqual(t, row.asn, (*int64)(nil))
		connect.AssertEqual(t, row.asnOrg, (*string)(nil))
		connect.AssertEqual(t, row.countryCode, (*string)(nil))
		connect.AssertEqual(t, row.region, (*string)(nil))
		connect.AssertEqual(t, row.city, (*string)(nil))
		connect.AssertEqual(t, row.latitude, (*float64)(nil))
		connect.AssertEqual(t, row.longitude, (*float64)(nil))
	})
}

type testLagTrackClientRow struct {
	asn         *int64
	asnOrg      *string
	countryCode *string
	region      *string
	city        *string
	latitude    *float64
	longitude   *float64
}

func getTestLagTrackClientRow(t testing.TB, ctx context.Context, trackId server.Id) *testLagTrackClientRow {
	t.Helper()
	row := &testLagTrackClientRow{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT client_asn, client_asn_org, client_country_code, client_region, client_city, client_latitude, client_longitude
			FROM lag_track
			WHERE track_id = $1
			`,
			trackId,
		).Scan(&row.asn, &row.asnOrg, &row.countryCode, &row.region, &row.city, &row.latitude, &row.longitude))
	})
	return row
}

// A whole visit, from a v4 and from a v6 address: the guest session, a track
// issued to the guest from the same address, and the extender's report of
// the session. The place is looked up as production looks it up. Afterwards
// no lag table holds either address in any form: as text, as its /24 (/48),
// as its bytes, or as its block's point. The track keeps its city's own
// point.
func TestLagStoresNoClientAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		// the extender's place is the sample's, which these tests set
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		// The IPv6 visitor is in 3fff::/20, the other documentation range:
		// the release gate's suite fixture lists 2001:db8::/32 in its
		// ip_overrides, which GetIpInfo answers before this test's GeoLite2.
		useIpAsnDb(t, writeTestIpAsnDb(t, geoLite2AsnDatabaseType, map[string]IpAsn{
			"203.0.113.0/24": {Number: 64500, Organization: "Example Access"},
			"3fff::/20":      {Number: 64501, Organization: "Example Six"},
		}))
		lyonBlock := lagTestCityBlock{
			countryCode: "fr", country: "France", countryGeonameId: 3017382,
			region: "Auvergne-Rhone-Alpes", regionGeonameId: 11071625,
			city: "Lyon", cityGeonameId: 2996944,
			latitude: 45.7597, longitude: 4.8422,
		}
		osakaBlock := lagTestCityBlock{
			countryCode: "jp", country: "Japan", countryGeonameId: 1861060,
			region: "Osaka", regionGeonameId: 1853904,
			city: "Osaka", cityGeonameId: 1853909,
			latitude: 34.6851, longitude: 135.5136,
		}
		useLagTestCityDb(t, map[string]lagTestCityBlock{
			"203.0.113.0/24": lyonBlock,
			"3fff::/20":      osakaBlock,
		})
		defer model.Testing_PushPlaces(lagTestPlaces)()
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1", "2001:db8:a::1")

		visits := []struct {
			addr   netip.Addr
			family uint32
			// the address's /24 (/48), as text and as bytes; a bytea column
			// reads back as \x and its hex, which no uuid or time contains
			prefixText  string
			prefixBytes string
			// the block's point, which GeoLite2 gives the address, and the
			// city's, which the track keeps
			block     lagTestCityBlock
			latitude  float64
			longitude float64
		}{
			{netip.MustParseAddr("203.0.113.77"), 4, "203.0.113.", "xcb0071", lyonBlock, 45.7485, 4.8467},
			{netip.MustParseAddr("3fff:db8::77"), 6, "3fff:db8:", "x3fff0db80000", osakaBlock, 34.6937, 135.5022},
		}
		for _, visit := range visits {
			// this test's GeoLite2 places the visitor, not an ip override
			if ipInfo, err := server.GetIpInfo(visit.addr); err != nil || ipInfo.City != visit.block.city || ipInfo.Latitude != visit.block.latitude {
				t.Fatalf("%s is not placed by this test's GeoLite2: %+v, %v", visit.addr, ipInfo, err)
			}
			clientAddress := netip.AddrPortFrom(visit.addr, 51000).String()

			guest := lagSessionFrom(t, ctx, clientAddress)
			if guest.Error != nil {
				t.Fatalf("the session from %s was refused: %+v", visit.addr, guest.Error)
			}
			byJwt, err := session.ParseByJwt(ctx, guest.ByClientJwt)
			connect.AssertEqual(t, err, nil)
			clientSession := session.NewLocalClientSession(ctx, clientAddress, byJwt)
			t.Cleanup(clientSession.Cancel)

			issuedAt := server.NowUtc().Truncate(time.Millisecond)
			track, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
				{ExtenderId: extender.extenderId},
			}}, clientSession, issuedAt, settings)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, track.Error, (*LagError)(nil))
			connect.AssertEqual(t, len(track.Tickets), 1)

			ticketBody := verifyLagTestTicket(t, track.Tickets[0].Ticket, rootPublicKey, issuedAt)
			ticket := model.GetLagTicket(ctx, server.RequireIdFromBytes(ticketBody.TicketId))
			body := newLagTestReportBody(extender, ticket, issuedAt.Add(30*time.Second))
			body.IpFamily = visit.family
			reported := postLagTestReports(t, extender, issuedAt.Add(2*time.Minute), signLagTestReport(t, body, extender.privateKey))
			connect.AssertEqual(t, reported.Accepted, 1)

			row := getTestLagTrackClientRow(t, ctx, *track.TrackId)
			connect.AssertEqual(t, *row.city, visit.block.city)
			connect.AssertEqual(t, *row.latitude, visit.latitude)
			connect.AssertEqual(t, *row.longitude, visit.longitude)
		}

		for _, table := range []string{"lag_guest", "lag_track", "lag_ticket", "lag_result"} {
			text := strings.ToLower(lagTestTableText(t, ctx, table))
			if text == "" {
				t.Fatalf("%s is empty, so it shows nothing", table)
			}
			for _, visit := range visits {
				for _, forbidden := range []string{
					visit.addr.String(),
					visit.addr.StringExpanded(),
					hex.EncodeToString(visit.addr.AsSlice()),
					visit.prefixText,
					visit.prefixBytes,
					strconv.FormatFloat(visit.block.latitude, 'f', -1, 64),
					strconv.FormatFloat(visit.block.longitude, 'f', -1, 64),
				} {
					if strings.Contains(text, strings.ToLower(forbidden)) {
						t.Fatalf("%s holds the address %s as %q: %s", table, visit.addr, forbidden, text)
					}
				}
			}
		}
	})
}
