// The dual loader opens the old database only beside the resolved latest, and
// no old-slot problem can fail a process or a latest lookup.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const (
	testArinOldEpoch    = int64(1700000000)
	testArinLatestEpoch = int64(1700086400)
)

// A synthetic database of databaseType over documentation space only.
func testArinDatabaseBytes(t testing.TB, databaseType string, buildEpoch int64, records map[string]mmdbtype.Map) []byte {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            databaseType,
		BuildEpoch:              buildEpoch,
		IncludeReservedNetworks: true,
		RecordSize:              32,
		Description:             map[string]string{"en": "synthetic ARIN pair database"},
	})
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

// A policy-two record for 192.0.2.0/24 in state.
func testArinPairRecords(state string, risk bool) map[string]mmdbtype.Map {
	return map[string]mmdbtype.Map{
		"192.0.2.0/24": {
			"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
			"quality_state": mmdbtype.String(state), "non_quality": mmdbtype.Bool(state != "subscriber"), "risk": mmdbtype.Bool(risk),
		},
	}
}

// The manifest arindbctl writes beside a database: its hash and build time.
func testArinManifest(t testing.TB, content []byte, builtAt time.Time) []byte {
	t.Helper()
	sum := sha256.Sum256(content)
	manifest, err := json.Marshal(map[string]any{
		"source":   "synthetic",
		"built_at": builtAt.UTC(),
		"sha256":   map[string]string{"arin.mmdb": hex.EncodeToString(sum[:])},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeTestArinFile(t testing.TB, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A release directory: the latest with its manifest, and when old is set the
// old copy with oldManifest. A nil manifest leaves that file out.
func writeTestArinRelease(t testing.TB, dir string, latest []byte, latestManifest []byte, old []byte, oldManifest []byte) string {
	t.Helper()
	path := filepath.Join(dir, filepath.Base(arinDatabaseResource))
	writeTestArinFile(t, path, latest)
	if latestManifest != nil {
		writeTestArinFile(t, filepath.Join(dir, arinManifestName), latestManifest)
	}
	if old != nil {
		writeTestArinFile(t, filepath.Join(dir, arinOldDatabaseName), old)
	}
	if oldManifest != nil {
		writeTestArinFile(t, filepath.Join(dir, arinOldManifestName), oldManifest)
	}
	return path
}

func closeTestArinPair(pair *arinDatabasePair) {
	if pair == nil {
		return
	}
	if pair.latest != nil {
		pair.latest.Close()
	}
	if pair.old != nil {
		pair.old.Close()
	}
}

// A valid, strictly older copy loads, its manifest time counting by build
// second, and each reader answers with its own facts and epoch.
func TestArinDatabasePairLoadsAStrictlyOlderSibling(t *testing.T) {
	latest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, testArinPairRecords("unknown", false))
	old := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinOldEpoch, testArinPairRecords("subscriber", false))
	path := writeTestArinRelease(t, t.TempDir(),
		latest, testArinManifest(t, latest, time.Unix(testArinLatestEpoch, 0)),
		// arindbctl stamps nanoseconds; only the second must match
		old, testArinManifest(t, old, time.Unix(testArinOldEpoch, 500_000_000)))
	pair, err := openArinDatabasePair(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestArinPair(pair)
	if pair.old == nil || pair.oldIdentity.State != ArinDatabaseLoaded || pair.oldIdentity.BuildEpoch != testArinOldEpoch {
		t.Fatalf("older sibling not loaded: %+v", pair.oldIdentity)
	}
	oldSum := sha256.Sum256(old)
	latestSum := sha256.Sum256(latest)
	if pair.oldIdentity.Sha256 != hex.EncodeToString(oldSum[:]) || pair.latestIdentity.Sha256 != hex.EncodeToString(latestSum[:]) {
		t.Fatalf("slot identities lost their manifest hashes: latest=%+v old=%+v", pair.latestIdentity, pair.oldIdentity)
	}
	latestInfo, oldInfo, err := getArinInfoPairFromDatabases(pair, netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	if latestInfo.DatabaseBuildEpoch != testArinLatestEpoch || latestInfo.QualityVerified() || !latestInfo.NonQuality {
		t.Fatalf("latest facts are not the latest database's: %+v", latestInfo)
	}
	if oldInfo == nil || oldInfo.DatabaseBuildEpoch != testArinOldEpoch || !oldInfo.QualityVerified() {
		t.Fatalf("old facts are not the old database's: %+v", oldInfo)
	}
}

// No copy beside the latest leaves the process on the latest alone.
func TestArinDatabasePairWithoutOldFileServesTheLatestOnly(t *testing.T) {
	latest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, testArinPairRecords("subscriber", false))
	path := writeTestArinRelease(t, t.TempDir(), latest, testArinManifest(t, latest, time.Unix(testArinLatestEpoch, 0)), nil, nil)
	pair, err := openArinDatabasePair(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestArinPair(pair)
	if pair.old != nil || pair.oldIdentity.State != ArinDatabaseMissing || pair.latestIdentity.State != ArinDatabaseLoaded {
		t.Fatalf("missing old file: latest=%+v old=%+v", pair.latestIdentity, pair.oldIdentity)
	}
	_, oldInfo, err := getArinInfoPairFromDatabases(pair, netip.MustParseAddr("192.0.2.1"))
	if err != nil || oldInfo != nil {
		t.Fatalf("a missing old slot produced facts: %+v %v", oldInfo, err)
	}
}

// The latest build copied forward again is identical, not a second database.
func TestArinDatabasePairTreatsTheLatestAgainAsIdentical(t *testing.T) {
	latest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, testArinPairRecords("subscriber", false))
	manifest := testArinManifest(t, latest, time.Unix(testArinLatestEpoch, 0))
	path := writeTestArinRelease(t, t.TempDir(), latest, manifest, latest, manifest)
	pair, err := openArinDatabasePair(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestArinPair(pair)
	if pair.old != nil || pair.oldIdentity.State != ArinDatabaseIdentical || pair.oldIdentity.Sha256 != pair.latestIdentity.Sha256 {
		t.Fatalf("identical old file: latest=%+v old=%+v", pair.latestIdentity, pair.oldIdentity)
	}
}

// Every way an old copy can be unusable leaves it unloaded without failing
// the latest.
func TestArinDatabasePairRefusesEachInvalidOldFile(t *testing.T) {
	latest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, testArinPairRecords("unknown", false))
	latestManifest := testArinManifest(t, latest, time.Unix(testArinLatestEpoch, 0))
	older := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinOldEpoch, testArinPairRecords("subscriber", false))
	mistyped := testArinDatabaseBytes(t, string(schemaTypeGeoLite2City), testArinOldEpoch, map[string]mmdbtype.Map{
		"192.0.2.0/24": {"country": mmdbtype.Map{"iso_code": mmdbtype.String("US")}},
	})
	newer := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch+60, testArinPairRecords("subscriber", false))
	sameEpochOther := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, testArinPairRecords("subscriber", false))
	for _, c := range []struct {
		name           string
		latestManifest []byte
		old            []byte
		oldManifest    []byte
	}{
		{name: "unopenable", latestManifest: latestManifest, old: []byte("not an mmdb file"), oldManifest: testArinManifest(t, []byte("x"), time.Unix(testArinOldEpoch, 0))},
		{name: "mistyped", latestManifest: latestManifest, old: mistyped, oldManifest: testArinManifest(t, mistyped, time.Unix(testArinOldEpoch, 0))},
		{name: "no old manifest", latestManifest: latestManifest, old: older},
		{name: "unreadable old manifest", latestManifest: latestManifest, old: older, oldManifest: []byte("{not json")},
		{name: "old manifest without a hash", latestManifest: latestManifest, old: older, oldManifest: []byte(`{"built_at":"2023-11-14T22:13:20Z"}`)},
		{name: "old manifest without a build time", latestManifest: latestManifest, old: older, oldManifest: []byte(`{"sha256":{"arin.mmdb":"` + hex.EncodeToString(make([]byte, 32)) + `"}}`)},
		{name: "manifest time in another second", latestManifest: latestManifest, old: older, oldManifest: testArinManifest(t, older, time.Unix(testArinOldEpoch+1, 0))},
		{name: "not older", latestManifest: latestManifest, old: newer, oldManifest: testArinManifest(t, newer, time.Unix(testArinLatestEpoch+60, 0))},
		{name: "equal epoch with another hash", latestManifest: latestManifest, old: sameEpochOther, oldManifest: testArinManifest(t, sameEpochOther, time.Unix(testArinLatestEpoch, 0))},
		{name: "equal epoch beside an unknown latest hash", latestManifest: nil, old: latest, oldManifest: latestManifest},
	} {
		path := writeTestArinRelease(t, t.TempDir(), latest, c.latestManifest, c.old, c.oldManifest)
		pair, err := openArinDatabasePair(path)
		if err != nil {
			t.Errorf("%s: an old-slot problem failed the latest: %v", c.name, err)
			continue
		}
		if pair.old != nil || pair.oldIdentity.State != ArinDatabaseInvalid || pair.latestIdentity.State != ArinDatabaseLoaded {
			t.Errorf("%s: latest=%+v old=%+v, want the latest alone with an invalid old slot", c.name, pair.latestIdentity, pair.oldIdentity)
		}
		if _, oldInfo, err := getArinInfoPairFromDatabases(pair, netip.MustParseAddr("192.0.2.1")); err != nil || oldInfo != nil {
			t.Errorf("%s: an invalid old slot produced facts: %+v %v", c.name, oldInfo, err)
		}
		closeTestArinPair(pair)
	}
}

// Only the latest can fail a process: unopenable or mistyped is an error,
// whatever lies beside it.
func TestArinDatabasePairFailsOnlyOnTheLatest(t *testing.T) {
	old := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinOldEpoch, testArinPairRecords("subscriber", false))
	oldManifest := testArinManifest(t, old, time.Unix(testArinOldEpoch, 0))
	mistyped := testArinDatabaseBytes(t, string(schemaTypeGeoLite2City), testArinLatestEpoch, nil)
	for name, latest := range map[string][]byte{"unopenable": []byte("not an mmdb file"), "mistyped": mistyped} {
		path := writeTestArinRelease(t, t.TempDir(), latest, nil, old, oldManifest)
		if pair, err := openArinDatabasePair(path); err == nil {
			closeTestArinPair(pair)
			t.Errorf("%s latest opened: %+v", name, pair.latestIdentity)
		}
	}
}

// The resolver picks the highest version directory holding a name, so an old
// file resolved on its own pairs this release's latest with another release's
// copy. The loader reads only the file beside the latest it resolved.
func TestArinDatabasePairResolvesTheOldFileOnlyBesideTheLatest(t *testing.T) {
	root := t.TempDir()
	previous := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinOldEpoch-86400, testArinPairRecords("subscriber", false))
	decoyLatest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinOldEpoch, testArinPairRecords("subscriber", false))
	writeTestArinRelease(t, filepath.Join(root, "all", "arindb", "1.0.0"),
		decoyLatest, testArinManifest(t, decoyLatest, time.Unix(testArinOldEpoch, 0)),
		previous, testArinManifest(t, previous, time.Unix(testArinOldEpoch-86400, 0)))
	latest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, testArinPairRecords("unknown", false))
	writeTestArinRelease(t, filepath.Join(root, "all", "arindb", "2.0.0"), latest, testArinManifest(t, latest, time.Unix(testArinLatestEpoch, 0)), nil, nil)

	latestPaths, err := ResolveResourcePaths(root, "synthetic", arinDatabaseResource)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "all", "arindb", "2.0.0", "arin.mmdb"); latestPaths[0] != want {
		t.Fatalf("resolved latest %s, want %s", latestPaths[0], want)
	}
	// the hazard: resolving the old name finds the decoy release's copy
	oldPaths, err := ResolveResourcePaths(root, "synthetic", "arindb/"+arinOldDatabaseName)
	if err != nil || oldPaths[0] != filepath.Join(root, "all", "arindb", "1.0.0", arinOldDatabaseName) {
		t.Fatalf("decoy fixture does not resolve the other release's old file: %v %v", oldPaths, err)
	}
	pair, err := openArinDatabasePair(latestPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestArinPair(pair)
	if pair.old != nil || pair.oldIdentity.State != ArinDatabaseMissing || pair.oldIdentity.Path != filepath.Join(root, "all", "arindb", "2.0.0", arinOldDatabaseName) {
		t.Fatalf("old slot left the resolved latest's directory: %+v", pair.oldIdentity)
	}
}

// A failing old lookup makes that connection's old facts unavailable and
// never fails the latest lookup.
func TestArinDatabasePairOldLookupErrorNeverFailsTheLatest(t *testing.T) {
	latest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, testArinPairRecords("subscriber", false))
	// a malformed gate decodes as an error for 192.0.2.0/24 only
	old := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinOldEpoch, map[string]mmdbtype.Map{
		"192.0.2.0/24":    {"risk": mmdbtype.String("false")},
		"198.51.100.0/24": testArinPairRecords("excluded", false)["192.0.2.0/24"],
	})
	path := writeTestArinRelease(t, t.TempDir(),
		latest, testArinManifest(t, latest, time.Unix(testArinLatestEpoch, 0)),
		old, testArinManifest(t, old, time.Unix(testArinOldEpoch, 0)))
	pair, err := openArinDatabasePair(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestArinPair(pair)
	if pair.oldIdentity.State != ArinDatabaseLoaded {
		t.Fatalf("fixture old slot not loaded: %+v", pair.oldIdentity)
	}
	oldErrors := testutil.ToFloat64(arinLookupErrors.WithLabelValues("old"))
	latestLookups, oldLookups := testutil.ToFloat64(arinLookups.WithLabelValues("latest")), testutil.ToFloat64(arinLookups.WithLabelValues("old"))
	latestInfo, oldInfo, err := getArinInfoPairFromDatabases(pair, netip.MustParseAddr("192.0.2.1"))
	if err != nil || latestInfo == nil || !latestInfo.QualityVerified() || oldInfo != nil {
		t.Fatalf("old lookup error leaked: latest=%+v old=%+v err=%v", latestInfo, oldInfo, err)
	}
	if got := testutil.ToFloat64(arinLookupErrors.WithLabelValues("old")); got != oldErrors+1 {
		t.Fatalf("old lookup errors went %v to %v, want one more", oldErrors, got)
	}
	// every pair lookup counts in each slot it reads, so failures are a share
	if latest, old := testutil.ToFloat64(arinLookups.WithLabelValues("latest")), testutil.ToFloat64(arinLookups.WithLabelValues("old")); latest != latestLookups+1 || old != oldLookups+1 {
		t.Fatalf("lookups went %v/%v to %v/%v, want one more in each slot", latestLookups, oldLookups, latest, old)
	}
	// the same pair still answers both slots for another address
	latestInfo, oldInfo, err = getArinInfoPairFromDatabases(pair, netip.MustParseAddr("198.51.100.1"))
	if err != nil || oldInfo == nil || oldInfo.QualityState != "excluded" || latestInfo.DatabaseBuildEpoch != testArinLatestEpoch {
		t.Fatalf("pair lookup lost a healthy old record: latest=%+v old=%+v err=%v", latestInfo, oldInfo, err)
	}
}

// A latest record that fails to decode fails the lookup, and so the
// connection's location write: its provider never reaches the cohort the
// canary compares. Each failure is counted by slot so a release that drops
// providers this way shows.
func TestArinDatabasePairCountsLatestLookupErrors(t *testing.T) {
	latest := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinLatestEpoch, map[string]mmdbtype.Map{
		"192.0.2.0/24": {"risk": mmdbtype.String("false")},
	})
	old := testArinDatabaseBytes(t, string(schemaTypeArinDb), testArinOldEpoch, testArinPairRecords("subscriber", false))
	path := writeTestArinRelease(t, t.TempDir(),
		latest, testArinManifest(t, latest, time.Unix(testArinLatestEpoch, 0)),
		old, testArinManifest(t, old, time.Unix(testArinOldEpoch, 0)))
	pair, err := openArinDatabasePair(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestArinPair(pair)
	latestErrors, oldErrors := testutil.ToFloat64(arinLookupErrors.WithLabelValues("latest")), testutil.ToFloat64(arinLookupErrors.WithLabelValues("old"))
	if latestInfo, oldInfo, err := getArinInfoPairFromDatabases(pair, netip.MustParseAddr("192.0.2.1")); err == nil || latestInfo != nil || oldInfo != nil {
		t.Fatalf("a malformed latest record answered: latest=%+v old=%+v err=%v", latestInfo, oldInfo, err)
	}
	if got := testutil.ToFloat64(arinLookupErrors.WithLabelValues("latest")); got != latestErrors+1 {
		t.Fatalf("latest lookup errors went %v to %v, want one more", latestErrors, got)
	}
	if got := testutil.ToFloat64(arinLookupErrors.WithLabelValues("old")); got != oldErrors {
		t.Fatalf("a latest failure counted an old one: %v to %v", oldErrors, got)
	}
}

// Overrides query no database, so they attest neither slot.
func TestArinPairOverrideAttestsNeitherSlot(t *testing.T) {
	previous := ipOverrides
	t.Cleanup(func() { ipOverrides = previous })
	ipOverrides = func() []*ipOverride {
		return []*ipOverride{{prefix: netip.MustParsePrefix("198.18.0.0/16"), ipInfo: IpInfo{CountryCode: "zz"}}}
	}
	latest, old, err := GetArinInfoPair(netip.MustParseAddr("198.18.0.1"))
	if err != nil || latest == nil || latest.DatabaseBuildEpoch != 0 || old != nil {
		t.Fatalf("override pair = %+v %+v %v", latest, old, err)
	}
}
