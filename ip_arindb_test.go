// Synthetic ARIN exception records exercise the decoder used by index admission.
package server

import (
	"bytes"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// Builds a private database over documentation addresses only.
func testExceptionDatabase(t *testing.T, databaseType string, records map[string]mmdbtype.Map) []byte {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: databaseType, IncludeReservedNetworks: true, Description: map[string]string{"en": "synthetic IP database"}})
	if err != nil {
		t.Fatal(err)
	}
	for prefix, record := range records {
		_, network, err := net.ParseCIDR(prefix)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Insert(network, record); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if _, err := w.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Reads the same serialized fields that the service lookup consumes.
func decodeArinTestRecord(t *testing.T, record mmdbtype.Map) (ArinInfo, error) {
	t.Helper()
	db, err := mmdb.OpenBytes(testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	info := ArinInfo{schemaType: schemaTypeArinDb}
	err = db.Lookup(netip.MustParseAddr("192.0.2.1")).Decode(&info)
	return info, err
}

func TestArinExceptionDecoderKeepsRiskAndQualitySeparate(t *testing.T) {
	for _, c := range []struct{ risk, nonQuality bool }{
		{risk: true, nonQuality: false}, {risk: false, nonQuality: true},
		{risk: true, nonQuality: true}, {risk: false, nonQuality: false},
	} {
		info, err := decodeArinTestRecord(t, mmdbtype.Map{
			"risk": mmdbtype.Bool(c.risk), "non_quality": mmdbtype.Bool(c.nonQuality),
			"classifier_version": mmdbtype.Uint32(1),
			"org_country_codes":  mmdbtype.Slice{mmdbtype.String("US")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if info.Risk != c.risk || info.NonQuality != c.nonQuality || info.ClassifierVersion != 1 || len(info.OrgCountryCodes) != 1 || info.OrgCountryCodes[0] != "us" {
			t.Fatalf("exception decoder lost distinct gates: %+v", info)
		}
	}
}

func TestArinExceptionDecoderRejectsMalformedGate(t *testing.T) {
	for _, key := range []string{"risk", "non_quality"} {
		if _, err := decodeArinTestRecord(t, mmdbtype.Map{mmdbtype.String(key): mmdbtype.String("false")}); err == nil {
			t.Fatalf("malformed %s silently became no exception", key)
		}
	}
}

func TestArinExceptionMissingRecordHasNoException(t *testing.T) {
	db, err := mmdb.OpenBytes(testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{
		"192.0.2.0/24": {"risk": mmdbtype.Bool(true), "non_quality": mmdbtype.Bool(true)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	info := ArinInfo{schemaType: schemaTypeArinDb}
	if err := db.Lookup(netip.MustParseAddr("198.51.100.1")).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Risk || info.NonQuality {
		t.Fatalf("unlisted prefix was excluded: %+v", info)
	}
}

func TestArinExceptionRejectsUnknownClassifierVersion(t *testing.T) {
	if _, err := decodeArinTestRecord(t, mmdbtype.Map{"classifier_version": mmdbtype.Uint32(2)}); err == nil {
		t.Fatal("future classifier semantics silently accepted")
	}
}

// An absent exception is a real lookup, not an uninitialized classification.
func TestArinLookupBindsHitsAndMissingRecordsToDatabaseGeneration(t *testing.T) {
	db, err := mmdb.OpenBytes(testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{
		"192.0.2.0/24": {"classifier_version": mmdbtype.Uint32(1), "non_quality": mmdbtype.Bool(true)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, address := range []string{"192.0.2.1", "198.51.100.1"} {
		info, err := getArinInfoFromDatabase(db, schemaTypeArinDb, netip.MustParseAddr(address))
		if err != nil {
			t.Fatal(err)
		}
		if info.DatabaseBuildEpoch <= 0 || info.DatabaseBuildEpoch != db.Metadata.BuildTime().Unix() {
			t.Fatal("lookup lost the database generation")
		}
		if address == "198.51.100.1" && (info.Risk || info.NonQuality || info.ClassifierVersion != 0) {
			t.Fatal("no-record provenance fabricated a classification exception")
		}
	}
}

func TestArinLegacyRiskUsesDirectRegistration(t *testing.T) {
	for _, c := range []struct {
		countries  []string
		associated string
		risk       bool
	}{
		{countries: []string{"us", "ca"}, associated: "ca", risk: false},
		{countries: []string{"us", "ca"}, associated: "us", risk: true},
		{countries: []string{"us", ""}, associated: "ca", risk: false},
		{countries: []string{"us"}, associated: "", risk: false},
		{associated: "ca", risk: false},
	} {
		if actual := arinRegistrationRisk(c.countries, c.associated); actual != c.risk {
			t.Fatalf("registration=%v associated=%s risk=%t want=%t", c.countries, c.associated, actual, c.risk)
		}
	}
}

func TestIpInfoDatabasePartitionsRegistrationAtCountryBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geolite2.mmdb")
	content := testExceptionDatabase(t, string(schemaTypeGeoLite2City), map[string]mmdbtype.Map{
		"192.0.2.0/25":   {"country": mmdbtype.Map{"iso_code": mmdbtype.String("US")}},
		"192.0.2.128/25": {"country": mmdbtype.Map{"iso_code": mmdbtype.String("CA")}},
	})
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenIpInfoDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seen := map[string]string{}
	for prefix, err := range db.NetworksWithin(netip.MustParsePrefix("192.0.2.0/24")) {
		if err != nil {
			t.Fatal(err)
		}
		info, err := db.GetIpInfo(prefix.Addr())
		if err != nil {
			t.Fatal(err)
		}
		seen[prefix.String()] = info.CountryCode
	}
	if len(seen) != 2 || seen["192.0.2.0/25"] != "us" || seen["192.0.2.128/25"] != "ca" {
		t.Fatalf("country boundary lost: %v", seen)
	}
	for prefix, err := range db.NetworksWithin(netip.MustParsePrefix("192.0.2.0/26")) {
		if err != nil {
			t.Fatal(err)
		}
		if prefix != netip.MustParsePrefix("192.0.2.0/26") {
			t.Fatalf("lookup broadened registration to %s", prefix)
		}
	}
}
