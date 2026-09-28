// Command tests exercise public parsing and offline archive/restart behavior
// with complete empty synthetic sources and no network-capable reader.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/strecovery"
)

// The only external port available to command tests is a counted empty snapshot.
type commandReader struct {
	calls int
	fail  bool
}

// A source failure cannot become a successful empty database result.
func (self *commandReader) Snapshot(context.Context, strecovery.DatabaseSource, strecovery.Limits) (*strecovery.DatabaseSnapshot, error) {
	self.calls++
	if self.fail {
		return nil, errors.New("synthetic complete source unavailable")
	}
	return &strecovery.DatabaseSnapshot{Intents: []strecovery.Intent{}, Attempts: []strecovery.Attempt{}}, nil
}

// Explicit permissions remove ambient umask from custody acceptance.
func commandDirectory(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Empty expected ranges still require both complete database sources and a store.
func commandConfig(t *testing.T) (string, string, string) {
	t.Helper()
	root, store := commandDirectory(t), commandDirectory(t)
	config := strecovery.Config{Schema: strecovery.ConfigSchema, ChainId: 31337, Genesis: "0x" + strings.Repeat("a", 64),
		Roles: []strecovery.Role{{Id: "synthetic-operator", Address: "0x" + strings.Repeat("b", 40)}},
		Databases: []strecovery.DatabaseSource{{Id: "operator-a", Connection: strecovery.FileReference{Path: filepath.Join(root, "a.url"), Sha256: "sha256:" + strings.Repeat("a", 64)}, Roles: []string{"synthetic-operator"}},
			{Id: "operator-b", Connection: strecovery.FileReference{Path: filepath.Join(root, "b.url"), Sha256: "sha256:" + strings.Repeat("b", 64)}, Roles: []string{"synthetic-operator"}}},
		Stores: []strecovery.StoreSource{{Id: "retained-store", Directory: store, Roles: []string{"synthetic-operator"}}},
		Limits: strecovery.Limits{MaximumIntents: 10, MaximumAttempts: 10, MaximumTransactionBytes: 4096, MaximumTotalBytes: 8192}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(root, "archive.json"), store
}

// Public collection, offline review and exact-seal restoration form a complete
// local workflow; later commands never reopen the original database sources.
func TestRecoveryCommandCollectInspectAndRestoreOffline(t *testing.T) {
	config, archive, store := commandConfig(t)
	reader := &commandReader{}
	var output bytes.Buffer
	if err := run(context.Background(), []string{"collect", "--config", config, "--archive", archive}, &output, reader); err != nil {
		t.Fatal(err)
	}
	var inspection strecovery.Inspection
	if err := json.Unmarshal(output.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 2 || inspection.CensusHash == "" || inspection.SpendingAuthorized || inspection.ActualFeesReconciled || inspection.CanonicalReceiptsReconciled {
		t.Fatalf("invalid collection projection: %+v", inspection)
	}
	reader.fail = true
	output.Reset()
	if err := run(context.Background(), []string{"inspect", "--archive", archive}, &output, reader); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(context.Background(), []string{"restore", "--archive", archive, "--store", store, "--accept-census-hash", inspection.CensusHash}, &output, reader); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 2 {
		t.Fatal("offline operation reopened database custody")
	}
}

// Unknown actions, ignored flags and incomplete selections are refused before
// any source is contacted. A failing source never publishes a partial archive.
func TestRecoveryCommandRejectsInvalidInputsAndPartialSource(t *testing.T) {
	config, archive, _ := commandConfig(t)
	reader := &commandReader{}
	for _, args := range [][]string{nil, {"send"}, {"collect", "--archive", archive}, {"inspect", "--archive", archive, "--config", config}, {"collect", "--config", config, "--archive", archive, "--timeout", "31m"}, {"restore", "--archive", archive}} {
		if err := run(context.Background(), args, new(bytes.Buffer), reader); err == nil {
			t.Fatalf("invalid command admitted: %v", args)
		}
	}
	if reader.calls != 0 {
		t.Fatal("invalid command reached database source")
	}
	reader.fail = true
	if err := run(context.Background(), []string{"collect", "--config", config, "--archive", archive}, new(bytes.Buffer), reader); err == nil {
		t.Fatal("source failure returned success")
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial source published an archive")
	}
}
