//go:build linux

// File custody tests force path, ambiguity, ownership and publication conflicts
// directly; no timing assumptions or externally owned files are involved.
package strecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every store filename is admitted, so extra evidence cannot silently vanish
// behind a permissive glob or a truncated directory listing.
func TestCensusStoreRejectsMissingMalformedAndUnclassifiedFiles(t *testing.T) {
	cases := []struct {
		name   string
		change func(testing.TB, *Config)
	}{
		{name: "missing directory", change: func(t testing.TB, c *Config) { c.Stores[0].Directory = filepath.Join(censusTestDir(t), "absent") }},
		{name: "extra unknown file", change: func(t testing.TB, c *Config) {
			if err := os.WriteFile(filepath.Join(c.Stores[0].Directory, "unknown.bin"), []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "name hash mismatch", change: func(t testing.TB, c *Config) {
			if err := os.WriteFile(filepath.Join(c.Stores[0].Directory, strings.Repeat("d", 64)+".rlp"), []byte("malformed original"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "prefix alias", change: func(t testing.TB, c *Config) {
			if err := os.WriteFile(filepath.Join(c.Stores[0].Directory, "0x"+strings.Repeat("d", 64)+".rlp"), []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file count bound", change: func(_ testing.TB, c *Config) { c.Limits.MaximumAttempts = 2 }},
	}
	for _, item := range cases {
		config, reader := censusTestFixture(t)
		item.change(t, &config)
		archive, err := Collect(context.Background(), config, reader)
		if err == nil || archive != nil {
			t.Fatalf("%s returned usable custody", item.name)
		}
	}
}

// A valid signature under the wrong canonical filename is conflicting source
// evidence, even though both the filename syntax and signature are valid alone.
func TestCensusStoreBindsCanonicalFilenameToOriginalHash(t *testing.T) {
	config, reader := censusTestFixture(t)
	attempt := reader.images["database-a"].Attempts[1]
	wrong := strings.Repeat("d", 64) + ".rlp"
	if err := os.WriteFile(filepath.Join(config.Stores[0].Directory, wrong), attempt.Raw, 0600); err != nil {
		t.Fatal(err)
	}
	if archive, err := Collect(context.Background(), config, reader); err == nil || archive != nil || !strings.Contains(err.Error(), wrong) {
		t.Fatalf("filename transplant lost conflict evidence: %v", err)
	}
}

// The directory reader fetches one record beyond the configured limit so a
// truncated listing cannot masquerade as a complete two-file evidence store.
func TestCensusStoreRefusesEnumerationTruncation(t *testing.T) {
	config, _ := censusTestFixture(t)
	limits := config.Limits
	limits.MaximumAttempts = 2
	if files, err := readStore(context.Background(), config.Stores[0], limits); err == nil || files != nil {
		t.Fatal("three-file store returned truncated success")
	}
	limits.MaximumAttempts = 3
	if files, err := readStore(context.Background(), config.Stores[0], limits); err != nil || len(files) != 3 {
		t.Fatalf("positive exact-bound store: %v", err)
	}
}

// File and ancestor links are rejected by descriptor-relative opens. Even a
// harmless target cannot stand in for the independently selected physical path.
func TestCensusCustodyRefusesSymlinksHardlinksAndPublicPermissions(t *testing.T) {
	root := censusTestDir(t)
	path := filepath.Join(root, "original")
	if err := os.WriteFile(path, []byte("synthetic original"), 0600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readPrivateFile(context.Background(), path, 1024); err != nil || string(raw) != "synthetic original" {
		t.Fatalf("positive original: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(context.Background(), alias, 1024); err == nil {
		t.Fatal("symlink admitted")
	}
	parentAlias := filepath.Join(censusTestDir(t), "parent")
	if err := os.Symlink(root, parentAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(context.Background(), filepath.Join(parentAlias, "original"), 1024); err == nil {
		t.Fatal("ancestor symlink admitted")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(context.Background(), path, 1024); err == nil {
		t.Fatal("hardlinked original admitted")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(context.Background(), path, 1024); err == nil {
		t.Fatal("public custody file admitted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(context.Background(), path, 4); err == nil {
		t.Fatal("oversized custody file admitted")
	}
}

// Unknown/case-duplicate keys cannot redirect database selection through the
// standard JSON decoder's otherwise permissive last-key-wins behavior.
func TestCensusConfigRejectsAmbiguousSourceSelection(t *testing.T) {
	config, _ := censusTestFixture(t)
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(censusTestDir(t), "config.json")
	for _, wire := range [][]byte{append([]byte(`{"SCHEMA":"changed",`), raw[1:]...), append([]byte(`{"unknown":true,`), raw[1:]...), append(bytes.Clone(raw), []byte(` {}`)...)} {
		if err := os.WriteFile(path, wire, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(context.Background(), path); err == nil {
			t.Fatal("ambiguous source config admitted")
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(context.Background(), path); err != nil {
		t.Fatalf("positive strict config: %v", err)
	}
}

// A conflicting output archive and a held restoration owner cannot be replaced
// by another collection, and rejected commands preserve the previous evidence.
func TestCensusPublicationPreservesConflictAndExclusiveOwner(t *testing.T) {
	config, reader := censusTestFixture(t)
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	root := censusTestDir(t)
	path := filepath.Join(root, "archive.json")
	original := []byte("synthetic previous evidence")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteArchive(context.Background(), path, archive); err == nil {
		t.Fatal("conflicting archive overwritten")
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, original) {
		t.Fatal("original archive changed")
	}
	destination := censusTestDir(t)
	directory, err := openPrivatePath(destination, true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	unlock, err := lockDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), archive, destination, archive.CensusHash); err == nil {
		t.Fatal("second restoration owner admitted")
	}
	unlock()
	if _, err := Restore(context.Background(), archive, destination, archive.CensusHash); err != nil {
		t.Fatalf("fresh owner after release: %v", err)
	}
}

// Missing/pin-mismatched connection input refuses before URL parsing or any
// connection attempt and never echoes the synthetic secret into diagnostics.
func TestCensusDatabaseConnectionPinRefusesBeforeOpening(t *testing.T) {
	config, _ := censusTestFixture(t)
	source := config.Databases[0]
	secret := []byte("synthetic-private-connection-material")
	if err := os.WriteFile(source.Connection.Path, secret, 0600); err != nil {
		t.Fatal(err)
	}
	reader := PostgresReader{}
	if image, err := reader.Snapshot(context.Background(), source, config.Limits); err == nil || image != nil || strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("pin mismatch did not refuse safely: %v", err)
	}
	source.Connection.Sha256 = digest(secret)
	if image, err := reader.Snapshot(context.Background(), source, config.Limits); err == nil || image != nil || strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("invalid URL did not refuse safely: %v", err)
	}
}
