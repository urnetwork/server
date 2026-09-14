package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// clientLocationReliabilityRowForTest is the reliability row as the
// aggregation tests read it back.
type clientLocationReliabilityRowForTest struct {
	valid          bool
	connected      bool
	ipv4Proven     bool
	ipv6Proven     bool
	hashCount      int
	locationCount  int
	cityLocationId *server.Id
}

func readClientLocationReliabilityForTest(ctx context.Context, clientId server.Id) (row clientLocationReliabilityRowForTest, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				valid,
				connected,
				ipv4_proven,
				ipv6_proven,
				client_address_hash_count,
				location_count,
				city_location_id
			FROM network_client_location_reliability
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&row.valid,
					&row.connected,
					&row.ipv4Proven,
					&row.ipv6Proven,
					&row.hashCount,
					&row.locationCount,
					&row.cityLocationId,
				))
				ok = true
			}
		})
	})
	return
}

// The per-client aggregation of the proof rule and the validity fix
// (connect/IPV6.md A8): one hash per family is valid, the location comes from
// the v4 side when both exist, and legacy counts as v4.
func TestClientLocationReliabilityIpFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cityA := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, cityA)
		cityB := &Location{
			LocationType: LocationTypeCity,
			City:         "San Jose",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, cityB)

		networkId := server.NewId()
		type testConnection struct {
			address    string
			intent     int
			locationId server.Id
		}
		connectClient := func(connections ...testConnection) server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			handlerId := CreateNetworkClientHandler(ctx)
			for _, c := range connections {
				connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, clientId, c.address, handlerId, c.intent)
				connect.AssertEqual(t, err, nil)
				err = SetConnectionLocation(ctx, connectionId, c.locationId, &ConnectionLocationScores{})
				connect.AssertEqual(t, err, nil)
			}
			return clientId
		}

		dualstackClientId := connectClient(
			testConnection{"10.1.2.3:20000", 4, cityA.LocationId},
			testConnection{"[2001:db8::1]:20000", 6, cityB.LocationId},
		)
		// the same pair connected in the other order: the location must not
		// depend on row order
		dualstackV6FirstClientId := connectClient(
			testConnection{"[2001:db8::2]:20000", 6, cityB.LocationId},
			testConnection{"10.1.2.4:20000", 4, cityA.LocationId},
		)
		sameFamilyTwiceClientId := connectClient(
			testConnection{"10.1.2.5:20000", 0, cityA.LocationId},
			testConnection{"10.99.2.5:20000", 0, cityA.LocationId},
		)
		legacyOverV6ClientId := connectClient(
			testConnection{"[2001:db8::3]:20000", 0, cityB.LocationId},
		)
		mismatchClientId := connectClient(
			testConnection{"10.1.2.6:20000", 6, cityA.LocationId},
		)
		v6OnlyClientId := connectClient(
			testConnection{"[2001:db8::4]:20000", 6, cityB.LocationId},
		)

		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now.Add(time.Hour))

		expect := func(clientId server.Id, valid bool, ipv4Proven bool, ipv6Proven bool, hashCount int, cityLocationId server.Id) {
			t.Helper()
			row, ok := readClientLocationReliabilityForTest(ctx, clientId)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, row.connected, true)
			connect.AssertEqual(t, row.valid, valid)
			connect.AssertEqual(t, row.ipv4Proven, ipv4Proven)
			connect.AssertEqual(t, row.ipv6Proven, ipv6Proven)
			connect.AssertEqual(t, row.hashCount, hashCount)
			connect.AssertEqual(t, row.locationCount, 1)
			connect.AssertEqual(t, row.cityLocationId != nil, true)
			connect.AssertEqual(t, *row.cityLocationId, cityLocationId)
		}

		// one hash per family is valid; the location is the v4 side's
		expect(dualstackClientId, true, true, true, 1, cityA.LocationId)
		expect(dualstackV6FirstClientId, true, true, true, 1, cityA.LocationId)
		// two hashes within one family is still invalid
		expect(sameFamilyTwiceClientId, false, true, false, 2, cityA.LocationId)
		// legacy proves v4 whatever it arrived on
		expect(legacyOverV6ClientId, true, true, false, 1, cityB.LocationId)
		// a mismatch proves nothing but is otherwise a normal connection
		expect(mismatchClientId, true, false, false, 1, cityA.LocationId)
		// only v6 proven: the location comes from the v6 side
		expect(v6OnlyClientId, true, false, true, 1, cityB.LocationId)

		// the stored columns
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT ip_version, ip_family_intent
				FROM network_client_connection
				WHERE client_id = $1
				`,
				mismatchClientId,
			)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, result.Next(), true)
				var ipVersion int
				var ipFamilyIntent int
				server.Raise(result.Scan(&ipVersion, &ipFamilyIntent))
				connect.AssertEqual(t, ipVersion, 4)
				connect.AssertEqual(t, ipFamilyIntent, 6)
			})
		})
	})
}
