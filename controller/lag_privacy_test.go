package controller

import (
	"context"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tests that lag.dev keeps no visitor address (lag.dev/PLAN.md amendment 9):
// a track keeps only the network and city-level place of the address it was
// issued to, and a whole visit leaves no form of the address in any lag
// table.

// A new track's client fields are the autonomous system and the GeoLite2
// City place of the address it is issued to, looked up when it is issued,
// with the city's coordinates as GeoLite2 gives them. A place that names no
// city keeps no coordinates, and a lookup that cannot answer leaves its
// fields empty.
func TestLagTrackClientFields(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		useIpAsnDb(t, writeTestIpAsnDb(t, geoLite2AsnDatabaseType, map[string]IpAsn{
			"198.51.100.0/24": {Number: 64500, Organization: "Example Access"},
		}))
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")
		place := lagTestPlace("Lyon", "fr", 45.7485, 4.8467)
		place.Region = "Auvergne-Rhone-Alpes"
		places["198.51.100.30"] = place
		// GeoLite2 knows only the country here, and gives the country's
		// coordinates with it
		places["198.51.100.40"] = &model.LagPlace{CountryCode: "fr", CountryName: "France", Latitude: 46.0, Longitude: 2.0}

		issue := func(clientAddress string) *testLagTrackClientRow {
			clientSession := newLagTestGuestSession(t, ctx, clientAddress)
			result, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
				{ExtenderId: extender.extenderId},
			}}, clientSession, server.NowUtc(), settings)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, (*LagError)(nil))
			return getTestLagTrackClientRow(t, ctx, *result.TrackId)
		}

		row := issue("198.51.100.30:1234")
		connect.AssertEqual(t, *row.asn, int64(64500))
		connect.AssertEqual(t, *row.asnOrg, "Example Access")
		connect.AssertEqual(t, *row.countryCode, "fr")
		connect.AssertEqual(t, *row.region, "Auvergne-Rhone-Alpes")
		connect.AssertEqual(t, *row.city, "Lyon")
		connect.AssertEqual(t, *row.latitude, 45.7485)
		connect.AssertEqual(t, *row.longitude, 4.8467)

		// a country alone keeps no coordinates
		row = issue("198.51.100.40:1234")
		connect.AssertEqual(t, *row.countryCode, "fr")
		connect.AssertEqual(t, row.city, (*string)(nil))
		connect.AssertEqual(t, row.latitude, (*float64)(nil))
		connect.AssertEqual(t, row.longitude, (*float64)(nil))

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
// the session. Afterwards no lag table holds either address in any form: as
// text, as its /24 (/48), or as its bytes.
func TestLagStoresNoClientAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		useIpAsnDb(t, writeTestIpAsnDb(t, geoLite2AsnDatabaseType, map[string]IpAsn{
			"203.0.113.0/24": {Number: 64500, Organization: "Example Access"},
			"2001:db8::/32":  {Number: 64501, Organization: "Example Six"},
		}))
		places["203.0.113.77"] = lagTestPlace("Lyon", "fr", 45.7, 4.8)
		places["2001:db8::77"] = lagTestPlace("Osaka", "jp", 34.7, 135.5)
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1", "2001:db8:a::1")

		visits := []struct {
			addr   netip.Addr
			family uint32
			// the address's /24 (/48), as text and as bytes; a bytea column
			// reads back as \x and its hex, which no uuid or time contains
			prefixText  string
			prefixBytes string
		}{
			{netip.MustParseAddr("203.0.113.77"), 4, "203.0.113.", "xcb0071"},
			{netip.MustParseAddr("2001:db8::77"), 6, "2001:db8:", "x20010db80000"},
		}
		for _, visit := range visits {
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
				} {
					if strings.Contains(text, strings.ToLower(forbidden)) {
						t.Fatalf("%s holds the address %s as %q: %s", table, visit.addr, forbidden, text)
					}
				}
			}
		}
	})
}
