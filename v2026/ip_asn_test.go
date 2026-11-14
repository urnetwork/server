package server

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"

	"github.com/urnetwork/connect/v2026"
)

// Tests of the GeoLite2 ASN lookup behind my-ip-info's `asn`
// (lag.dev/PLAN.md A3) and the extender places (A4), against hand-built
// databases in GeoLite2 ASN's own record shape.

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

// Makes GetIpAsn read the database at path for the rest of the test, or no
// database for an empty path.
func useTestIpAsnDb(t testing.TB, path string) {
	t.Helper()
	restore, err := Testing_UseIpAsnDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
}

var testIpAsnRecords = map[string]IpAsn{
	"192.0.2.0/24":   {Number: 64500, Organization: "Example"},
	"2001:db8::/32":  {Number: 64501, Organization: "Example Six"},
	"203.0.113.0/24": {Organization: "No number"},
}

// Each address resolves to the record of its network, in either family and
// through the v4-mapped form, and an address without a record, or with a
// record that has no number, has no autonomous system.
func TestIpAsnLookup(t *testing.T) {
	useTestIpAsnDb(t, writeTestIpAsnDb(t, GeoLite2AsnDatabaseType, testIpAsnRecords))

	connect.AssertEqual(t, IpAsnDatabaseDeployed(), true)
	for ip, want := range map[string]*IpAsn{
		"192.0.2.7":        {Number: 64500, Organization: "Example"},
		"::ffff:192.0.2.7": {Number: 64500, Organization: "Example"},
		"2001:db8::1":      {Number: 64501, Organization: "Example Six"},
		"198.51.100.1":     nil,
		"2001:db9::1":      nil,
		"203.0.113.5":      nil,
	} {
		connect.AssertEqual(t, GetIpAsn(netip.MustParseAddr(ip)), want)
	}
	connect.AssertEqual(t, GetIpAsn(netip.Addr{}), (*IpAsn)(nil))
}

// Without a database every address is unknown.
func TestIpAsnLookupWithoutDatabase(t *testing.T) {
	useTestIpAsnDb(t, "")
	connect.AssertEqual(t, IpAsnDatabaseDeployed(), false)
	connect.AssertEqual(t, GetIpAsn(netip.MustParseAddr("192.0.2.7")), (*IpAsn)(nil))
}

// A database of another type is refused when opened: its records would
// decode as empty rather than fail.
func TestOpenIpAsnDbRefusesOtherDatabaseTypes(t *testing.T) {
	path := writeTestIpAsnDb(t, "GeoLite2-City", testIpAsnRecords)
	db, err := openIpAsnDb(path)
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, db, (*mmdb.Reader)(nil))

	restore, err := Testing_UseIpAsnDatabase(path)
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, restore == nil, true)
}

// The database is resolved as a config resource beside the City database,
// and an absent one is no database rather than an error.
func TestLoadIpAsnDbFromConfig(t *testing.T) {
	path := writeTestIpAsnDb(t, GeoLite2AsnDatabaseType, testIpAsnRecords)

	home := t.TempDir()
	mmdbDir := filepath.Join(home, "all", "mmdb")
	if err := os.MkdirAll(mmdbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_CONFIG_HOME", home)
	connect.AssertEqual(t, loadIpAsnDb(), (*mmdb.Reader)(nil))

	if err := os.Symlink(path, filepath.Join(mmdbDir, "geolite2-asn.mmdb")); err != nil {
		t.Fatal(err)
	}
	db := loadIpAsnDb()
	if db == nil {
		t.Fatal("the deployed database was not loaded")
	}
	defer db.Close()
	connect.AssertEqual(t, lookupIpAsn(db, netip.MustParseAddr("192.0.2.7")), &IpAsn{Number: 64500, Organization: "Example"})
}
