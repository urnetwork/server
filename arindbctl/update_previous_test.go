package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// A synthetic ARIN-shaped database over documentation space, built at epoch.
func testPreviousArinDatabase(t *testing.T, databaseType string, epoch int64, state string) []byte {
	t.Helper()
	writer, err := mmdbwriter.New(mmdbwriter.Options{
		BuildEpoch: epoch, DatabaseType: databaseType, IncludeReservedNetworks: true, RecordSize: 32,
		Description: map[string]string{"en": "synthetic previous release"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, network, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(network, mmdbtype.Map{
		"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
		"quality_state": mmdbtype.String(state), "non_quality": mmdbtype.Bool(state != "subscriber"), "risk": mmdbtype.Bool(false),
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := writer.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// The manifest arindbctl writes beside a database.
func testPreviousArinManifest(t *testing.T, content []byte, builtAt time.Time) []byte {
	t.Helper()
	sum := sha256.Sum256(content)
	manifest, err := json.MarshalIndent(map[string]any{
		"source": "synthetic previous release", "built_at": builtAt.UTC(),
		"sha256": map[string]string{"arin.mmdb": hex.EncodeToString(sum[:])},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(manifest, '\n')
}

// A release directory with arin.mmdb and, when manifest is set, manifest.json.
func writeTestPreviousRelease(t *testing.T, dir string, database []byte, manifest []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "arin.mmdb")
	if err := os.WriteFile(path, database, 0o644); err != nil {
		t.Fatal(err)
	}
	if manifest != nil {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

var (
	testPreviousNewEpoch = time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC).Unix()
	testPreviousOldEpoch = time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC).Unix()
)

// A staged release directory holding the new build.
func testNewArinRelease(t *testing.T) (string, []byte) {
	t.Helper()
	database := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousNewEpoch, "unknown")
	dir := filepath.Join(t.TempDir(), "stage", "arindb")
	writeTestPreviousRelease(t, dir, database, testPreviousArinManifest(t, database, time.Unix(testPreviousNewEpoch, 0)))
	return dir, database
}

func requireNoPreviousCopy(t *testing.T, arinDir string, name string) {
	t.Helper()
	for _, file := range []string{"arin-old.mmdb", "manifest-old.json"} {
		if _, err := os.Stat(filepath.Join(arinDir, file)); !os.IsNotExist(err) {
			t.Errorf("%s: %s was left in the release", name, file)
		}
	}
}

// The copy is exactly the file the environment's processes would open: the
// runtime resolver's highest release beneath the config root, byte for byte
// with its manifest, recorded with its source, hash, epoch and size.
func TestCopyPreviousArinDatabaseCopiesTheResolvedRelease(t *testing.T) {
	root := t.TempDir()
	older := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousOldEpoch-86400, "subscriber")
	writeTestPreviousRelease(t, filepath.Join(root, "all", "arindb", "1.0.0"), older, testPreviousArinManifest(t, older, time.Unix(testPreviousOldEpoch-86400, 0)))
	previous := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousOldEpoch, "subscriber")
	// arindbctl stamps a sub-second build time; only the second must match
	previousManifest := testPreviousArinManifest(t, previous, time.Unix(testPreviousOldEpoch, 250_000_000))
	previousPath := writeTestPreviousRelease(t, filepath.Join(root, "all", "arindb", "1.1.0"), previous, previousManifest)
	arinDir, _ := testNewArinRelease(t)

	record := copyPreviousArinDatabase(t.Context(), commandOptions{previousConfig: root, previousEnv: "main"}, arinDir)
	sum := sha256.Sum256(previous)
	if record["status"] != previousArinDatabaseCopied || record["source"] != previousPath || record["sha256"] != hex.EncodeToString(sum[:]) ||
		record["build_epoch"] != testPreviousOldEpoch || record["size"] != int64(len(previous)) || record["selected_by"] != "previous-config" || record["env"] != "main" {
		t.Fatalf("copy record %+v", record)
	}
	copied, err := os.ReadFile(filepath.Join(arinDir, "arin-old.mmdb"))
	if err != nil || !bytes.Equal(copied, previous) {
		t.Fatalf("arin-old.mmdb is not a byte copy of the resolved release: %v", err)
	}
	copiedManifest, err := os.ReadFile(filepath.Join(arinDir, "manifest-old.json"))
	if err != nil || !bytes.Equal(copiedManifest, previousManifest) {
		t.Fatalf("manifest-old.json is not a byte copy of the release's manifest: %v", err)
	}
	if summary := previousArinDatabaseSummary(record); !strings.Contains(summary, "canary: previous database copied") {
		t.Fatalf("summary %q", summary)
	}
}

// The new build copied forward again would not be a second database.
func TestCopyPreviousArinDatabaseSkipsAnIdenticalDatabase(t *testing.T) {
	arinDir, database := testNewArinRelease(t)
	previousPath := writeTestPreviousRelease(t, filepath.Join(t.TempDir(), "previous"), database, testPreviousArinManifest(t, database, time.Unix(testPreviousNewEpoch, 0)))
	record := copyPreviousArinDatabase(t.Context(), commandOptions{previousDatabase: previousPath}, arinDir)
	if record["status"] != previousArinDatabaseIdentical || record["selected_by"] != "previous-database" {
		t.Fatalf("identical record %+v", record)
	}
	requireNoPreviousCopy(t, arinDir, "identical")
	if summary := previousArinDatabaseSummary(record); !strings.HasPrefix(summary, "no canary: previous database identical") {
		t.Fatalf("summary %q", summary)
	}
}

// Every unverified previous database is refused and leaves both files out,
// without failing the release.
func TestCopyPreviousArinDatabaseRefusesUnverifiedFiles(t *testing.T) {
	previous := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousOldEpoch, "subscriber")
	other := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousOldEpoch, "excluded")
	geolite := testPreviousArinDatabase(t, "GeoLite2-City", testPreviousOldEpoch, "subscriber")
	newer := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousNewEpoch+3600, "subscriber")
	sameEpoch := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousNewEpoch, "subscriber")
	for _, c := range []struct {
		name     string
		database []byte
		manifest []byte
		reason   string
	}{
		{name: "a hash mismatch", database: previous, manifest: testPreviousArinManifest(t, other, time.Unix(testPreviousOldEpoch, 0)), reason: "SHA-256"},
		{name: "a wrong build epoch", database: previous, manifest: testPreviousArinManifest(t, previous, time.Unix(testPreviousOldEpoch+1, 0)), reason: "built_at"},
		{name: "a wrong type", database: geolite, manifest: testPreviousArinManifest(t, geolite, time.Unix(testPreviousOldEpoch, 0)), reason: "expected ARIN database"},
		{name: "a newer build", database: newer, manifest: testPreviousArinManifest(t, newer, time.Unix(testPreviousNewEpoch+3600, 0)), reason: "not older"},
		{name: "the same epoch with other bytes", database: sameEpoch, manifest: testPreviousArinManifest(t, sameEpoch, time.Unix(testPreviousNewEpoch, 0)), reason: "not older"},
		{name: "no manifest", database: previous, manifest: nil, reason: "manifest"},
		{name: "a manifest without a hash", database: previous, manifest: []byte(`{"built_at":"2026-10-13T06:00:00Z"}`), reason: "SHA-256"},
	} {
		arinDir, _ := testNewArinRelease(t)
		previousPath := writeTestPreviousRelease(t, filepath.Join(t.TempDir(), "previous"), c.database, c.manifest)
		record := copyPreviousArinDatabase(t.Context(), commandOptions{previousDatabase: previousPath}, arinDir)
		if record["status"] != previousArinDatabaseRefused || !strings.Contains(record["reason"].(string), c.reason) {
			t.Errorf("%s: record %+v, want refused for %q", c.name, record, c.reason)
		}
		requireNoPreviousCopy(t, arinDir, c.name)
	}
	arinDir, _ := testNewArinRelease(t)
	record := copyPreviousArinDatabase(t.Context(), commandOptions{previousDatabase: filepath.Join(t.TempDir(), "missing", "arin.mmdb")}, arinDir)
	if record["status"] != previousArinDatabaseRefused {
		t.Fatalf("a missing pinned file: %+v", record)
	}
	requireNoPreviousCopy(t, arinDir, "a missing pinned file")
}

// Without a named previous database, or with nothing beneath the config root,
// the release ships without a canary and says so.
func TestCopyPreviousArinDatabaseWithoutAPreviousDatabase(t *testing.T) {
	arinDir, _ := testNewArinRelease(t)
	for name, options := range map[string]commandOptions{
		"none named":           {},
		"an empty config root": {previousConfig: t.TempDir(), previousEnv: "main"},
		"another resource only": {previousConfig: func() string {
			root := t.TempDir()
			writeTestPreviousRelease(t, filepath.Join(root, "all", "mmdb", "1.0.0"), []byte("synthetic other resource"), nil)
			return root
		}(), previousEnv: "main"},
	} {
		record := copyPreviousArinDatabase(t.Context(), options, arinDir)
		if record["status"] != previousArinDatabaseNone {
			t.Errorf("%s: record %+v", name, record)
		}
		if summary := previousArinDatabaseSummary(record); !strings.HasPrefix(summary, "no canary: previous database none") {
			t.Errorf("%s: summary %q", name, summary)
		}
	}
	requireNoPreviousCopy(t, arinDir, "none")
}

// The full update places the copy in the published release, records it in
// the update manifest and names it in the release summary.
func TestUpdateCopiesThePreviousDatabaseIntoTheRelease(t *testing.T) {
	fixture := newUpdateFixture(t)
	root := filepath.Join(fixture.dir, "previous-config")
	previousEpoch := fixture.at.Add(-24 * time.Hour).Unix()
	previous := testPreviousArinDatabase(t, "urnetwork arindb", previousEpoch, "subscriber")
	writeTestPreviousRelease(t, filepath.Join(root, "all", "arindb", "0.9.0"), previous, testPreviousArinManifest(t, previous, time.Unix(previousEpoch, 0)))
	output := filepath.Join(fixture.dir, "bundle")
	if err := fixture.runWith(t, output, fixture.catalog, "--previous-config", root, "--previous-env", "main"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(output, "arindb"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"arin-old.mmdb", "arin.mmdb", "manifest-old.json", "manifest.json", "registration-manifest.json", "update-manifest.json"}) {
		t.Fatalf("arindb resource contents: %v", names)
	}
	var manifest struct {
		Previous map[string]any `json:"previous_database"`
		Summary  string         `json:"summary"`
	}
	content, err := os.ReadFile(filepath.Join(output, "arindb", "update-manifest.json"))
	if err != nil || json.Unmarshal(content, &manifest) != nil {
		t.Fatalf("update manifest: %v %s", err, content)
	}
	if manifest.Previous["status"] != previousArinDatabaseCopied || manifest.Previous["build_epoch"] != float64(previousEpoch) || !strings.Contains(manifest.Summary, "canary: previous database copied") {
		t.Fatalf("update manifest previous_database %+v summary %q", manifest.Previous, manifest.Summary)
	}
	summary, err := os.ReadFile(filepath.Join(output, "update-summary.txt"))
	if err != nil || !strings.Contains(string(summary), "canary: previous database copied") {
		t.Fatalf("release summary %q %v", summary, err)
	}
}

// A refused copy ships the release without a canary instead of failing it.
func TestUpdateShipsWithoutACanaryWhenThePreviousDatabaseIsRefused(t *testing.T) {
	fixture := newUpdateFixture(t)
	pinned := filepath.Join(fixture.dir, "pinned")
	previous := testPreviousArinDatabase(t, "urnetwork arindb", fixture.at.Add(-24*time.Hour).Unix(), "subscriber")
	other := testPreviousArinDatabase(t, "urnetwork arindb", fixture.at.Add(-24*time.Hour).Unix(), "excluded")
	pinnedPath := writeTestPreviousRelease(t, pinned, previous, testPreviousArinManifest(t, other, fixture.at.Add(-24*time.Hour)))
	output := filepath.Join(fixture.dir, "bundle")
	if err := fixture.runWith(t, output, fixture.catalog, "--previous-database", pinnedPath); err != nil {
		t.Fatalf("a refused copy failed the release: %v", err)
	}
	requireNoPreviousCopy(t, filepath.Join(output, "arindb"), "refused")
	summary, err := os.ReadFile(filepath.Join(output, "update-summary.txt"))
	if err != nil || !strings.Contains(string(summary), "no canary: previous database refused") {
		t.Fatalf("release summary %q %v", summary, err)
	}
}

// The previous-database flags belong to update, a resolution needs its root,
// and the pin cannot be combined with a resolution.
func TestPreviousDatabaseFlagsApplyOnlyToUpdate(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"another command":       {"build", "--output", filepath.Join(dir, "a"), "--previous-config", dir},
		"an env without a root": {"update", "--output", filepath.Join(dir, "b"), "--previous-env", "main"},
		"a pin and a root":      {"update", "--output", filepath.Join(dir, "c"), "--previous-database", filepath.Join(dir, "arin.mmdb"), "--previous-config", dir},
	} {
		if err := runCommandWithDependencies(t.Context(), args, io.Discard, commandDependencies{}); err == nil || !strings.Contains(err.Error(), "previous") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(args[2]); !os.IsNotExist(err) {
			t.Errorf("%s: a refused command published %s", name, args[2])
		}
	}
}
