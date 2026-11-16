package controller

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tests of my-ip-info's `asn` (lag.dev/PLAN.md A3), against hand-built
// databases in GeoLite2 ASN's own record shape. The lookup itself is
// server.GetIpAsn, tested with it.

// Writes a database of the given type with one record per network, in
// GeoLite2 ASN's shape. A zero number writes a record without one.
func writeTestIpAsnDb(t testing.TB, databaseType string, records map[string]IpAsn) string {
	t.Helper()
	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            databaseType,
		IncludeReservedNetworks: true,
		RecordSize:              24,
	})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, asn := range records {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		record := mmdbtype.Map{
			"autonomous_system_organization": mmdbtype.String(asn.Organization),
		}
		if asn.Number != 0 {
			record["autonomous_system_number"] = mmdbtype.Uint32(asn.Number)
		}
		if err := writer.Insert(network, record); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "geolite2-asn.mmdb")
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
	return path
}

// Makes server.GetIpAsn read the database at path for the rest of the test,
// or no database for an empty path, and restores the process's reader
// afterwards.
func useIpAsnDb(t testing.TB, path string) {
	t.Helper()
	restore, err := server.Testing_UseIpAsnDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
}

// `asn` is part of `info` when known and left out when not (MyIPInfoResult in
// connect/api/bringyour.yml).
func TestMyIpInfoAsnJson(t *testing.T) {
	location := &model.Location{
		LocationType:  model.LocationTypeCountry,
		Country:       "United States",
		CountryCode:   "us",
		Continent:     "North America",
		ContinentCode: "na",
		Latitude:      37.751,
		Longitude:     -97.822,
		Timezone:      "America/Chicago",
	}
	myInfo, err := NewMyInfo("192.0.2.7", location)
	connect.AssertEqual(t, err, nil)
	myInfo.Asn = &IpAsn{Number: 64500, Organization: "Example"}
	resultJson, err := json.Marshal(&MyIpInfoResult{Info: myInfo})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(resultJson), `{"info":{"ip":"192.0.2.7","location":{"coordinates":{"lat":37.751,"lon":-97.822},"country":{"code":"us","name":"United States"},"continent":{"code":"na","name":"North America"},"timezone":"America/Chicago"},"asn":{"number":64500,"organization":"Example"}},"connected_to_network":false}`)
	assertMyIpInfoJsonShape(t, resultJson)

	myInfo.Asn = nil
	resultJson, err = json.Marshal(&MyIpInfoResult{Info: myInfo})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(resultJson), `"asn"`), false)
	assertMyIpInfoJsonShape(t, resultJson)
}

// my-ip-info answers the caller's autonomous system from the deployed
// database, and leaves it out without one.
func TestGetMyIpInfoAsn(t *testing.T) {
	// a public address, so the location reads the GeoLite2 City database,
	// which the portable fixture does not have
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientIp := "65.19.157.62"
		clientSession := session.NewLocalClientSession(ctx, clientIp+":12345", nil)

		useIpAsnDb(t, writeTestIpAsnDb(t, server.GeoLite2AsnDatabaseType, map[string]IpAsn{
			"65.19.157.0/24": {Number: 6939, Organization: "Hurricane Electric LLC"},
		}))
		result, err := GetMyIpInfo(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Info.Asn, &IpAsn{Number: 6939, Organization: "Hurricane Electric LLC"})
		connect.AssertEqual(t, result.Info.Location.Country.Code, "us")
		resultJson, err := json.Marshal(result)
		connect.AssertEqual(t, err, nil)
		assertMyIpInfoJsonShape(t, resultJson)

		useIpAsnDb(t, "")
		result, err = GetMyIpInfo(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Info.Asn, (*IpAsn)(nil))
	})
}
