package nginx

// Certificate store tests: each rule of the findLatestTls port, with
// placeholder files, since the lookup reads only names; then the store's
// checks over generated certificates, each refusal naming the server name,
// and the leaves it serves though they aren't valid now, as nginx does.

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// <vault>/<env>, <vault>/all and <vault>, the homes LoadCertificateStore
// searches, under a new temporary vault.
func newTestTlsHomes(t *testing.T) (envHome string, allHome string, flatHome string) {
	t.Helper()
	vaultDir := t.TempDir()
	return filepath.Join(vaultDir, "test"), filepath.Join(vaultDir, "all"), vaultDir
}

// Writes placeholder <dirName>.pem and <dirName>.key, which is all the
// lookup reads.
func writeTestTlsPlaceholders(t *testing.T, home string, version string, dirName string) {
	t.Helper()
	writeTestTlsFile(t, home, version, dirName, ".pem", []byte("pem"))
	writeTestTlsFile(t, home, version, dirName, ".key", []byte("key"))
}

// The version directory findLatestTls picks for name, or the error.
func findTestTlsVersion(t *testing.T, tlsHomes []string, name string) (home string, version string, dirName string, err error) {
	t.Helper()
	tlsFiles, err := findLatestTls(tlsHomes, name)
	if err != nil {
		return "", "", "", err
	}
	dirPath := filepath.Dir(tlsFiles.pemPath)
	dirName = filepath.Base(dirPath)
	if tlsFiles.pemPath != filepath.Join(dirPath, dirName+".pem") || tlsFiles.keyPath != filepath.Join(dirPath, dirName+".key") {
		t.Fatalf("findLatestTls(%q) = %s and %s, want <dir>/<dir>.pem and .key", name, tlsFiles.pemPath, tlsFiles.keyPath)
	}
	versionPath := filepath.Dir(dirPath)
	return filepath.Dir(filepath.Dir(versionPath)), filepath.Base(versionPath), dirName, nil
}

// Versions compare as numbers, not as strings, and directories that are
// not semver, and files, are no candidates.
func TestFindLatestTlsTakesTheHighestVersionByNumber(t *testing.T) {
	envHome, allHome, flatHome := newTestTlsHomes(t)
	tlsHomes := []string{envHome, allHome, flatHome}
	for _, version := range []string{"2026.9.30", "2026.10.2", "2026.10.16", "latest", "2027.1", "v2027.1.1"} {
		writeTestTlsPlaceholders(t, envHome, version, "api.service.example")
	}
	// a semver name that is a file, not a directory
	if err := os.WriteFile(filepath.Join(envHome, "tls", "2028.1.1"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	// and one that is a symlink to a complete version, which warpctl's
	// DirEntry.IsDir doesn't count as a directory
	if err := os.Symlink(filepath.Join(envHome, "tls", "2026.10.16"), filepath.Join(envHome, "tls", "2029.1.1")); err != nil {
		t.Fatal(err)
	}

	_, version, _, err := findTestTlsVersion(t, tlsHomes, "api.service.example")
	if err != nil {
		t.Fatal(err)
	}
	if version != "2026.10.16" {
		t.Fatalf("picked %s, want 2026.10.16", version)
	}
}

// Same Major.Minor.Patch orders by Metadata in string order, and PreRelease
// counts for nothing, as warpctl's semverCmpWithBuild orders them. Where
// semver would rank a release above its pre-release, warpctl doesn't.
func TestFindLatestTlsOrdersMetadataAndIgnoresPreRelease(t *testing.T) {
	cases := []struct {
		versions    []string
		wantVersion string
	}{
		{versions: []string{"1.0.0", "1.0.0+a", "1.0.0+b"}, wantVersion: "1.0.0+b"},
		// semver ranks 1.0.0+a above 1.0.0-rc.1+b; Metadata decides here
		{versions: []string{"1.0.0+a", "1.0.0-rc.1+b"}, wantVersion: "1.0.0-rc.1+b"},
		{versions: []string{"1.9.9+z", "2.0.0-rc.1"}, wantVersion: "2.0.0-rc.1"},
		// a tie, which warpctl breaks by map order, goes to the greater
		// directory name, the one warpctl keeps for equal versions
		{versions: []string{"3.0.0", "3.0.0-rc.1"}, wantVersion: "3.0.0-rc.1"},
		{versions: []string{"4.01.0", "4.1.0"}, wantVersion: "4.1.0"},
	}
	for _, c := range cases {
		envHome, allHome, flatHome := newTestTlsHomes(t)
		for _, version := range c.versions {
			writeTestTlsPlaceholders(t, envHome, version, "api.service.example")
		}
		_, version, _, err := findTestTlsVersion(t, []string{envHome, allHome, flatHome}, "api.service.example")
		if err != nil {
			t.Fatal(err)
		}
		if version != c.wantVersion {
			t.Errorf("from %v picked %s, want %s", c.versions, version, c.wantVersion)
		}
	}
}

// A version directory missing either file is no candidate for the name, and
// with none complete the name has no pair.
func TestFindLatestTlsRequiresBothFiles(t *testing.T) {
	envHome, allHome, flatHome := newTestTlsHomes(t)
	tlsHomes := []string{envHome, allHome, flatHome}
	writeTestTlsPlaceholders(t, envHome, "2026.10.2", "api.service.example")
	writeTestTlsFile(t, envHome, "2026.10.16", "api.service.example", ".pem", []byte("pem"))
	writeTestTlsFile(t, envHome, "2026.10.17", "api.service.example", ".key", []byte("key"))

	_, version, _, err := findTestTlsVersion(t, tlsHomes, "api.service.example")
	if err != nil {
		t.Fatal(err)
	}
	if version != "2026.10.2" {
		t.Fatalf("picked %s, want the complete 2026.10.2", version)
	}

	writeTestTlsFile(t, envHome, "2026.10.2", "test-api.service.example", ".pem", []byte("pem"))
	writeTestTlsFile(t, envHome, "2026.10.16", "test-api.service.example", ".key", []byte("key"))
	if _, _, _, err := findTestTlsVersion(t, tlsHomes, "test-api.service.example"); err == nil {
		t.Fatal("found a pair split across two versions")
	}
}

// Each name takes its own latest version, so two names can come from
// different versions.
func TestFindLatestTlsTakesEachNamesOwnLatest(t *testing.T) {
	envHome, allHome, flatHome := newTestTlsHomes(t)
	tlsHomes := []string{envHome, allHome, flatHome}
	writeTestTlsPlaceholders(t, envHome, "2026.10.2", "api.service.example")
	writeTestTlsPlaceholders(t, envHome, "2026.10.2", "mcp.service.example")
	writeTestTlsPlaceholders(t, envHome, "2026.10.16", "api.service.example")

	for name, wantVersion := range map[string]string{"api.service.example": "2026.10.16", "mcp.service.example": "2026.10.2"} {
		_, version, _, err := findTestTlsVersion(t, tlsHomes, name)
		if err != nil {
			t.Fatal(err)
		}
		if version != wantVersion {
			t.Errorf("%s picked %s, want %s", name, version, wantVersion)
		}
	}
}

// A wildcard *.x looks only in star.x: neither x nor star.<parent of x>
// stands in.
func TestFindLatestTlsLooksUpAWildcardOnlyInItsStarDir(t *testing.T) {
	envHome, allHome, flatHome := newTestTlsHomes(t)
	tlsHomes := []string{envHome, allHome, flatHome}
	writeTestTlsPlaceholders(t, envHome, "2026.10.16", "connect.service.example")
	writeTestTlsPlaceholders(t, envHome, "2026.10.16", "star.service.example")
	if _, _, _, err := findTestTlsVersion(t, tlsHomes, "*.connect.service.example"); err == nil {
		t.Fatal("a wildcard found a pair outside its star directory")
	}

	writeTestTlsPlaceholders(t, envHome, "2026.10.2", "star.connect.service.example")
	_, version, dirName, err := findTestTlsVersion(t, tlsHomes, "*.connect.service.example")
	if err != nil {
		t.Fatal(err)
	}
	if dirName != "star.connect.service.example" || version != "2026.10.2" {
		t.Fatalf("picked %s/%s, want 2026.10.2/star.connect.service.example", version, dirName)
	}
}

// A name of more than two labels falls back to star.<parent>, then to
// <parent>; its own directory comes first whatever the versions. A name of
// two labels has no fallback.
func TestFindLatestTlsFallsBackToStarParentThenParent(t *testing.T) {
	envHome, allHome, flatHome := newTestTlsHomes(t)
	tlsHomes := []string{envHome, allHome, flatHome}
	writeTestTlsPlaceholders(t, envHome, "2026.10.2", "service.example")
	pick := func(name string) string {
		t.Helper()
		_, version, dirName, err := findTestTlsVersion(t, tlsHomes, name)
		if err != nil {
			t.Fatal(err)
		}
		return version + "/" + dirName
	}

	if got := pick("api.service.example"); got != "2026.10.2/service.example" {
		t.Fatalf("picked %s, want the parent 2026.10.2/service.example", got)
	}
	writeTestTlsPlaceholders(t, envHome, "2026.10.2", "star.service.example")
	if got := pick("api.service.example"); got != "2026.10.2/star.service.example" {
		t.Fatalf("picked %s, want 2026.10.2/star.service.example before the parent", got)
	}
	writeTestTlsPlaceholders(t, envHome, "2026.9.30", "api.service.example")
	writeTestTlsPlaceholders(t, envHome, "2026.10.16", "star.service.example")
	if got := pick("api.service.example"); got != "2026.9.30/api.service.example" {
		t.Fatalf("picked %s, want the name's own older 2026.9.30/api.service.example", got)
	}

	writeTestTlsPlaceholders(t, envHome, "2026.10.16", "star.example")
	writeTestTlsPlaceholders(t, envHome, "2026.10.16", "example")
	if _, _, _, err := findTestTlsVersion(t, tlsHomes, "other.example"); err == nil {
		t.Fatal("a name of two labels fell back to its parent")
	}
}

// The homes are searched in order, <vault>/<env>, <vault>/all and the flat
// host layout <vault>, and the first holding a directory decides it, whatever
// the versions; the directory order still comes first.
func TestFindLatestTlsSearchesEnvThenAllThenTheFlatLayout(t *testing.T) {
	envHome, allHome, flatHome := newTestTlsHomes(t)
	tlsHomes := []string{envHome, allHome, flatHome}
	pickHome := func(name string) string {
		t.Helper()
		home, version, dirName, err := findTestTlsVersion(t, tlsHomes, name)
		if err != nil {
			t.Fatal(err)
		}
		switch home {
		case envHome:
			return "env " + version + "/" + dirName
		case allHome:
			return "all " + version + "/" + dirName
		case flatHome:
			return "flat " + version + "/" + dirName
		default:
			t.Fatalf("picked a file under %s", home)
			return ""
		}
	}

	writeTestTlsPlaceholders(t, flatHome, "2026.10.2", "api.service.example")
	if got := pickHome("api.service.example"); got != "flat 2026.10.2/api.service.example" {
		t.Fatalf("picked %s, want the flat layout's", got)
	}
	writeTestTlsPlaceholders(t, allHome, "2026.9.30", "api.service.example")
	if got := pickHome("api.service.example"); got != "all 2026.9.30/api.service.example" {
		t.Fatalf("picked %s, want all's before the flat layout's", got)
	}
	writeTestTlsPlaceholders(t, envHome, "2026.9.1", "api.service.example")
	if got := pickHome("api.service.example"); got != "env 2026.9.1/api.service.example" {
		t.Fatalf("picked %s, want the env's before all's", got)
	}

	// the name's own directory in the flat layout beats star.<parent> in the
	// env
	writeTestTlsPlaceholders(t, flatHome, "2026.10.2", "mcp.service.example")
	writeTestTlsPlaceholders(t, envHome, "2026.10.16", "star.service.example")
	if got := pickHome("mcp.service.example"); got != "flat 2026.10.2/mcp.service.example" {
		t.Fatalf("picked %s, want the name's own directory first", got)
	}
}

// warpctl's flat-file fallback, a pair at <home>/<dir>/ with no version
// directory, is not ported.
func TestFindLatestTlsIgnoresUnversionedPairs(t *testing.T) {
	envHome, allHome, flatHome := newTestTlsHomes(t)
	tlsHomes := []string{envHome, allHome, flatHome}
	for _, home := range tlsHomes {
		dirPath := filepath.Join(home, "api.service.example")
		if err := os.MkdirAll(dirPath, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{".pem", ".key"} {
			if err := os.WriteFile(filepath.Join(dirPath, "api.service.example"+suffix), []byte("file"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// warpctl took the fallback only when <home>/tls existed
	if err := os.MkdirAll(filepath.Join(envHome, "tls"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := findTestTlsVersion(t, tlsHomes, "api.service.example"); err == nil {
		t.Fatal("found an unversioned pair")
	}
}

// Every served name gets the pair findLatestTls picks: an lb name its own,
// the rest the star.<parent> fallback, the wildcard its star directory.
// Names under a served wildcard get its pair, by nginx's rule, and no other
// name gets one.
func TestLoadCertificateStoreLoadsEveryServedName(t *testing.T) {
	vault := newTestTlsVault(t)
	certificateStore := vault.load(t)
	cases := []struct {
		name        string
		wantDirName string
	}{
		{name: "test-lb.service.example", wantDirName: "test-lb.service.example"},
		{name: "test-lb.other.example", wantDirName: "star.other.example"},
		{name: "api.service.example", wantDirName: "star.service.example"},
		{name: "old-api.service.example", wantDirName: "star.service.example"},
		{name: "test-api.other.example", wantDirName: "star.other.example"},
		{name: "connect.service.example", wantDirName: "star.service.example"},
		{name: "*.connect.service.example", wantDirName: "star.connect.service.example"},
		{name: "test-mcp.service.example", wantDirName: "star.service.example"},
		// covered by the served wildcard, one label or several
		{name: "g1.connect.service.example", wantDirName: "star.connect.service.example"},
		{name: "a.g1.connect.service.example", wantDirName: "star.connect.service.example"},
		{name: "unknown.example"},
		{name: "service.example"},
		{name: "x.api.service.example"},
		{name: "connect.service.example.example"},
	}
	for _, c := range cases {
		certificate, ok := certificateStore.certificate(c.name)
		if c.wantDirName == "" {
			if ok {
				t.Errorf("certificate(%q) found one, want none", c.name)
			}
			continue
		}
		if !ok {
			t.Errorf("certificate(%q) found none, want %s", c.name, c.wantDirName)
			continue
		}
		if !certificate.Leaf.Equal(vault.dirPairs[c.wantDirName].leaf) {
			t.Errorf("certificate(%q) = the leaf for %v, want %s's", c.name, certificate.Leaf.DNSNames, c.wantDirName)
		}
		if len(certificate.Certificate) != 2 {
			t.Errorf("certificate(%q) has %d certificates, want the leaf and its chain", c.name, len(certificate.Certificate))
		}
	}
	for _, name := range vault.routeTable.ServedNames() {
		if _, ok := certificateStore.certificate(name); !ok {
			t.Errorf("served name %q has no certificate", name)
		}
	}
	// names whose files are the same share one loaded pair
	apiCertificate, _ := certificateStore.certificate("api.service.example")
	mcpCertificate, _ := certificateStore.certificate("test-mcp.service.example")
	if apiCertificate != mcpCertificate {
		t.Errorf("two names from star.service.example hold two copies of its pair")
	}
}

// A host's vault is flat: xops merges all and the env into its root, so
// <vault>/tls holds every pair.
func TestLoadCertificateStoreReadsTheFlatHostVault(t *testing.T) {
	vault := newTestTlsVault(t)
	if err := os.Rename(filepath.Join(vault.envHome(), "tls"), filepath.Join(vault.vaultDir, "tls")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(vault.envHome()); err != nil {
		t.Fatal(err)
	}
	certificateStore := vault.load(t)
	certificate, ok := certificateStore.certificate("test-api.service.example")
	if !ok || !certificate.Leaf.Equal(vault.dirPairs["star.service.example"].leaf) {
		t.Fatalf("test-api.service.example didn't get star.service.example's leaf from the flat vault")
	}
}

// A service that isn't exposed needs no certificate (LBALT-IMPL.md §7): the
// fixture vault holds none for its names, its redirect alias or its
// wildcard, and the store loads without them.
func TestLoadCertificateStoreNeedsNoCertificateForAnUnexposedService(t *testing.T) {
	vault := newTestTlsVault(t)
	vault.routeTable = newTestUnexposedRouteTable(t)
	certificateStore := vault.load(t)
	for _, name := range []string{"mcp.hidden.example", "old-mcp.hidden.example", "*.g1.connect.service.example"} {
		if _, ok := certificateStore.nameCertificates[name]; ok {
			t.Errorf("the store holds a certificate for %s, of the unexposed mcp", name)
		}
	}
}

// The earliest NotAfter over every leaf, whichever name it serves.
func TestCertificateStoreEarliestExpiry(t *testing.T) {
	vault := newTestTlsVault(t)
	certificateStore := vault.load(t)
	want := vault.dirPairs["star.other.example"].leaf.NotAfter
	for dirName, pair := range vault.dirPairs {
		if pair.leaf.NotAfter.Before(want) {
			t.Fatalf("the fixture's %s expires before star.other.example", dirName)
		}
	}
	if got := certificateStore.EarliestExpiry(); !got.Equal(want) {
		t.Fatalf("EarliestExpiry() = %s, want %s", got, want)
	}
}

// NewFront reports its store's earliest expiry in the front's certificate
// expiry metric, a lapsed NotAfter included, so an alert can lead a lapse
// the store serves rather than refuses.
func TestNewFrontReportsTheEarliestCertificateExpiry(t *testing.T) {
	now := time.Now()
	vault := newTestTlsVault(t)
	lapsed := vault.authority.issue(t, []string{"*.other.example"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	writeTestTlsPair(t, vault.envHome(), "2026.10.17", "star.other.example", lapsed)
	front := newTestCloseFront(t, vault.routeTable, vault.load(t), testFrontSettings())
	if got, want := testutil.ToFloat64(front.metrics.certificateExpiry), float64(lapsed.leaf.NotAfter.Unix()); got != want {
		t.Fatalf("the certificate expiry metric is %v, want the lapsed NotAfter %v", got, want)
	}
}

// Loads the fixture vault after change edits it, and returns the refusal.
func loadChangedTestTlsVault(t *testing.T, change func(vault *testTlsVault)) error {
	t.Helper()
	vault := newTestTlsVault(t)
	change(vault)
	certificateStore, err := LoadCertificateStore(vault.vaultDir, vault.env, vault.routeTable)
	if err == nil {
		t.Fatalf("loaded a store from a vault that can't serve every name")
	}
	if certificateStore != nil {
		t.Fatalf("returned a store with the refusal %v", err)
	}
	return err
}

// Checks that err names the server name it was refused for, along with
// detail.
func checkTestTlsRefusal(t *testing.T, err error, name string, detail string) {
	t.Helper()
	if !strings.Contains(err.Error(), "certificate for "+name+":") || !strings.Contains(err.Error(), detail) {
		t.Fatalf("refusal %q, want one naming %s with %q", err, name, detail)
	}
}

// A served name with no complete pair anywhere is refused by name.
func TestLoadCertificateStoreRefusesANameWithNoPair(t *testing.T) {
	err := loadChangedTestTlsVault(t, func(vault *testTlsVault) {
		if err := os.Remove(filepath.Join(vault.envHome(), "tls", testTlsVersion, "star.connect.service.example", "star.connect.service.example.key")); err != nil {
			t.Fatal(err)
		}
	})
	checkTestTlsRefusal(t, err, "*.connect.service.example", "star.connect.service.example")
}

// A pair findLatestTls picked but that can't be read, here a key that is a
// directory, which warpctl's existence check passes, is refused by name.
func TestLoadCertificateStoreRefusesAPairItCantRead(t *testing.T) {
	err := loadChangedTestTlsVault(t, func(vault *testTlsVault) {
		writeTestTlsFile(t, vault.envHome(), "2026.10.17", "star.other.example", ".pem", vault.dirPairs["star.other.example"].pemBytes)
		keyPath := filepath.Join(vault.envHome(), "tls", "2026.10.17", "star.other.example", "star.other.example.key")
		if err := os.MkdirAll(keyPath, 0o755); err != nil {
			t.Fatal(err)
		}
	})
	checkTestTlsRefusal(t, err, "test-lb.other.example", "star.other.example.key")
}

// A certificate whose SANs don't cover the name is refused by name, wherever
// findLatestTls found it: in the name's own directory, in its parent's, or
// for a wildcard whose certificate lists names under it but not the wildcard
// itself.
func TestLoadCertificateStoreRefusesACertificateThatDoesntCoverTheName(t *testing.T) {
	err := loadChangedTestTlsVault(t, func(vault *testTlsVault) {
		pair := vault.authority.issueValid(t, 24*time.Hour, "other-lb.service.example")
		writeTestTlsPair(t, vault.envHome(), "2026.10.17", "test-lb.service.example", pair)
	})
	checkTestTlsRefusal(t, err, "test-lb.service.example", "doesn't cover the name")

	// the parent fallback: service.example lists only some names under it
	err = loadChangedTestTlsVault(t, func(vault *testTlsVault) {
		if err := os.RemoveAll(filepath.Join(vault.envHome(), "tls", testTlsVersion, "star.service.example")); err != nil {
			t.Fatal(err)
		}
		pair := vault.authority.issueValid(t, 24*time.Hour, "service.example", "test-api.service.example")
		writeTestTlsPair(t, vault.envHome(), testTlsVersion, "service.example", pair)
	})
	checkTestTlsRefusal(t, err, "api.service.example", "doesn't cover the name")

	err = loadChangedTestTlsVault(t, func(vault *testTlsVault) {
		pair := vault.authority.issueValid(t, 24*time.Hour, "g1.connect.service.example", "g2.connect.service.example")
		writeTestTlsPair(t, vault.envHome(), "2026.10.17", "star.connect.service.example", pair)
	})
	checkTestTlsRefusal(t, err, "*.connect.service.example", "lists no subject alternative name *.connect.service.example")
}

// The parent fallback serves every name its certificate lists.
func TestLoadCertificateStoreAcceptsAParentCertificateListingTheName(t *testing.T) {
	vault := newTestTlsVault(t)
	if err := os.RemoveAll(filepath.Join(vault.envHome(), "tls", testTlsVersion, "star.service.example")); err != nil {
		t.Fatal(err)
	}
	dnsNames := []string{"service.example"}
	for _, name := range vault.routeTable.ServedNames() {
		if strings.HasSuffix(name, ".service.example") && !strings.HasPrefix(name, "*.") {
			dnsNames = append(dnsNames, name)
		}
	}
	pair := vault.authority.issueValid(t, 24*time.Hour, dnsNames...)
	writeTestTlsPair(t, vault.envHome(), testTlsVersion, "service.example", pair)
	certificate, ok := vault.load(t).certificate("test-mcp.service.example")
	if !ok || !certificate.Leaf.Equal(pair.leaf) {
		t.Fatal("test-mcp.service.example didn't get its parent's certificate")
	}
}

// A key that isn't the certificate's is refused by name.
func TestLoadCertificateStoreRefusesAKeyThatDoesntMatch(t *testing.T) {
	err := loadChangedTestTlsVault(t, func(vault *testTlsVault) {
		otherPair := vault.authority.issueValid(t, 24*time.Hour, "*.service.example")
		writeTestTlsFile(t, vault.envHome(), testTlsVersion, "star.service.example", ".key", otherPair.keyBytes)
	})
	checkTestTlsRefusal(t, err, "api.service.example", "private key does not match public key")
}

// A leaf that has expired, or isn't valid yet, loads and serves its names,
// as nginx loads and serves it: only those names' clients fail to verify it,
// while every other name verifies, so one lapsed pair never stops the whole
// store. The earliest expiry shows a lapsed NotAfter, and NotValidNames
// lists the names of such a leaf. That holds for a lapsed pair in a name's
// own directory too, which findLatestTls picks over a valid star.<parent>,
// as warpctl picks it.
func TestLoadCertificateStoreServesALeafNotValidNow(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name    string
		dirName string
		pair    func(vault *testTlsVault) *testTlsPair
		// the served names the pair serves
		wantNotValidNames []string
		wantExpired       bool
	}{
		{
			name:    "expired",
			dirName: "star.other.example",
			pair: func(vault *testTlsVault) *testTlsPair {
				return vault.authority.issue(t, []string{"*.other.example"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
			},
			wantNotValidNames: []string{"test-api.other.example", "test-connect.other.example", "test-lb.other.example", "test-mcp.other.example"},
			wantExpired:       true,
		},
		{
			name:    "not yet valid",
			dirName: "star.other.example",
			pair: func(vault *testTlsVault) *testTlsPair {
				return vault.authority.issue(t, []string{"*.other.example"}, now.Add(time.Hour), now.Add(48*time.Hour))
			},
			wantNotValidNames: []string{"test-api.other.example", "test-connect.other.example", "test-lb.other.example", "test-mcp.other.example"},
		},
		{
			name:    "an expired exact pair beside a valid wildcard",
			dirName: "old-api.service.example",
			pair: func(vault *testTlsVault) *testTlsPair {
				return vault.authority.issue(t, []string{"old-api.service.example"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
			},
			wantNotValidNames: []string{"old-api.service.example"},
			wantExpired:       true,
		},
	} {
		vault := newTestTlsVault(t)
		pair := c.pair(vault)
		writeTestTlsPair(t, vault.envHome(), "2026.10.17", c.dirName, pair)
		certificateStore, err := LoadCertificateStore(vault.vaultDir, vault.env, vault.routeTable)
		if err != nil {
			t.Errorf("%s: the store refused the leaf: %v", c.name, err)
			continue
		}
		if got := certificateStore.NotValidNames(now); !slices.Equal(got, c.wantNotValidNames) {
			t.Errorf("%s: NotValidNames() = %v, want %v", c.name, got, c.wantNotValidNames)
		}
		if c.wantExpired && !certificateStore.EarliestExpiry().Equal(pair.leaf.NotAfter) {
			t.Errorf("%s: EarliestExpiry() = %s, want the lapsed %s", c.name, certificateStore.EarliestExpiry(), pair.leaf.NotAfter)
		}

		tlsConfig, _ := newTlsConfigs(certificateStore, newNameMatcher(vault.routeTable), vault.routeTable.Lb.Names[0], DefaultTlsSettings())
		listener := listenTestTls(t, tlsConfig)
		for _, serverName := range []string{c.wantNotValidNames[0], "test-api.service.example", "test-lb.service.example"} {
			_, err := listener.exchange(t, &tls.Config{
				ServerName: serverName,
				RootCAs:    vault.authority.pool(),
				NextProtos: []string{"h2", "http/1.1"},
			})
			var invalidError x509.CertificateInvalidError
			if serverName == c.wantNotValidNames[0] {
				if !errors.As(err, &invalidError) || invalidError.Reason != x509.Expired {
					t.Errorf("%s: a client of %s got %v, want its certificate's validity refused", c.name, serverName, err)
				}
			} else if err != nil {
				t.Errorf("%s: a client of %s, whose leaf is valid, got %v", c.name, serverName, err)
			}
		}
	}
}

// A served name's leaf is listed while it isn't valid, and only then: at its
// NotBefore and its NotAfter it is valid, as crypto/x509 verifies it.
func TestCertificateStoreNotValidNamesFollowsTheValidityWindow(t *testing.T) {
	vault := newTestTlsVault(t)
	certificateStore := vault.load(t)
	leaf := vault.dirPairs["test-lb.service.example"].leaf
	for _, c := range []struct {
		at   time.Time
		want []string
	}{
		{at: leaf.NotBefore.Add(-time.Second), want: []string{"test-lb.service.example"}},
		{at: leaf.NotBefore, want: []string{}},
		{at: leaf.NotAfter, want: []string{}},
		{at: leaf.NotAfter.Add(time.Second), want: []string{"test-lb.service.example"}},
	} {
		notValidNames := []string{}
		for _, name := range certificateStore.NotValidNames(c.at) {
			if name == "test-lb.service.example" {
				notValidNames = append(notValidNames, name)
			}
		}
		if !slices.Equal(notValidNames, c.want) {
			t.Errorf("at %s: NotValidNames lists %v of test-lb.service.example's names, want %v", c.at, notValidNames, c.want)
		}
	}
}

// An env that isn't one directory name is refused: it would name the vault
// itself, or search outside it. Pairs sit where each would otherwise find
// them, in the vault's flat layout and in the directory above the vault.
func TestLoadCertificateStoreRefusesAnEnvOutsideTheVault(t *testing.T) {
	vault := newTestTlsVault(t)
	vaultDir := filepath.Join(vault.vaultDir, "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(vault.envHome(), "tls"), filepath.Join(vaultDir, "tls")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(vault.vaultDir, "tls"), os.DirFS(filepath.Join(vaultDir, "tls"))); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertificateStore(vaultDir, "test", vault.routeTable); err != nil {
		t.Fatalf("the control env didn't load: %v", err)
	}
	for _, env := range []string{"", ".", "..", "test/..", "../test", `test\x`, "test/../.."} {
		if _, err := LoadCertificateStore(vaultDir, env, vault.routeTable); err == nil {
			t.Errorf("LoadCertificateStore with env %q loaded", env)
		}
	}
}

// The store searches <vault>/<env>, then <vault>/all, then the flat host
// layout, and the first holding a name's directory decides it, whatever the
// versions.
func TestLoadCertificateStoreSearchesEnvThenAllThenTheFlatLayout(t *testing.T) {
	vault := newTestTlsVault(t)
	allPair := vault.authority.issueValid(t, 24*time.Hour, "*.service.example")
	writeTestTlsPair(t, filepath.Join(vault.vaultDir, "all"), "2027.1.1", "star.service.example", allPair)
	flatPair := vault.authority.issueValid(t, 24*time.Hour, "*.service.example")
	writeTestTlsPair(t, vault.vaultDir, "2028.1.1", "star.service.example", flatPair)
	loadedLeaf := func() string {
		t.Helper()
		certificate, ok := vault.load(t).certificate("test-api.service.example")
		if !ok {
			t.Fatal("test-api.service.example has no certificate")
		}
		switch {
		case certificate.Leaf.Equal(vault.dirPairs["star.service.example"].leaf):
			return "env"
		case certificate.Leaf.Equal(allPair.leaf):
			return "all"
		case certificate.Leaf.Equal(flatPair.leaf):
			return "flat"
		default:
			return "another"
		}
	}

	if got := loadedLeaf(); got != "env" {
		t.Fatalf("loaded %s's leaf, want the env's before newer versions in all and the flat layout", got)
	}
	if err := os.RemoveAll(filepath.Join(vault.envHome(), "tls", testTlsVersion, "star.service.example")); err != nil {
		t.Fatal(err)
	}
	if got := loadedLeaf(); got != "all" {
		t.Fatalf("loaded %s's leaf, want all's before a newer version in the flat layout", got)
	}
	if err := os.RemoveAll(filepath.Join(vault.vaultDir, "all")); err != nil {
		t.Fatal(err)
	}
	if got := loadedLeaf(); got != "flat" {
		t.Fatalf("loaded %s's leaf, want the flat layout's", got)
	}
}

// A 64 KiB name under the served wildcard gets its pair, and the store walks
// only the bases as long as the longest served one, as the matcher does.
func TestCertificateStoreWalksOnlyBasesAsLongAsTheLongestServedOne(t *testing.T) {
	vault := newTestTlsVault(t)
	certificateStore := vault.load(t)
	if want := len("connect.service.example"); certificateStore.maxWildcardBaseLength != want {
		t.Fatalf("the bound is %d bytes, want the served wildcard base's %d", certificateStore.maxWildcardBaseLength, want)
	}
	longPrefix := newTestLongNamePrefix()
	certificate, ok := certificateStore.certificate(longPrefix + "g1.connect.service.example")
	if !ok || !certificate.Leaf.Equal(vault.dirPairs["star.connect.service.example"].leaf) {
		t.Fatal("a 64 KiB name under *.connect.service.example didn't get its pair")
	}
	if _, ok := certificateStore.certificate(longPrefix + "unknown.example"); ok {
		t.Fatal("a 64 KiB unknown name got a pair")
	}
	// the bases certificate may look up: one per dot within the bound
	lookupCount := 0
	for range wildcardBases(longPrefix+"unknown.example", certificateStore.maxWildcardBaseLength) {
		lookupCount += 1
	}
	if certificateStore.maxWildcardBaseLength+1 < lookupCount {
		t.Fatalf("a 64 KiB name may look up %d bases, want at most %d", lookupCount, certificateStore.maxWildcardBaseLength+1)
	}
}
