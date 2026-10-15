package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	servermonitor "github.com/urnetwork/server/v2026/monitor"
)

func TestAPIReleaseProofCLIAdmission(t *testing.T) {
	if opts, err := parseMonitorOptions(nil); err != nil || opts.apiReleaseProofFile != "" {
		t.Fatal("release proof is not default-off")
	}
	if _, err := parseMonitorOptions([]string{"-list-signals", "-api-release-proof", "/private/expectation"}); err == nil {
		t.Fatal("list-only mode accepted a release expectation")
	}
	now := time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)
	p := servermonitor.APIReleaseProofSettings{Environment: "synthetic", Revision: strings.Repeat("a", 40), Version: "2026.10.1+123",
		ImageDigests: []string{"sha256:" + strings.Repeat("b", 64)}, SelectionFloors: map[string]time.Time{"g1": now.Add(-time.Hour)}, ExpiresAt: now.Add(time.Hour)}
	raw, _ := json.Marshal(p)
	path := filepath.Join(t.TempDir(), "expectation.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-api-release-proof", path, "-exclude-signal", "api-release-proof"},
		{"-api-release-proof", path, "-include-signal", "provider-picker"},
		{"-api-release-proof", path + "-missing"},
	} {
		calls := 0
		err := runWithSettingsLoader(args, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
			calls++
			return servermonitor.SignalSettings{}, nil
		})
		if err == nil || calls != 0 {
			t.Fatal("invalid or excluded expectation reached settings/contact preparation")
		}
	}
}

// The CLI captures the expectation before invoking the loader. A later file
// edit does not hot-reload an isolated part of the watcher policy.
func TestAPIReleaseProofCLIUsesCapturedExpectation(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)
	p := servermonitor.APIReleaseProofSettings{Environment: "synthetic", Revision: strings.Repeat("a", 40), Version: "2026.10.1+123",
		ImageDigests: []string{"sha256:" + strings.Repeat("b", 64)}, SelectionFloors: map[string]time.Time{"g1": now.Add(-time.Hour)}, ExpiresAt: now.Add(time.Hour)}
	raw, _ := json.Marshal(p)
	dir := t.TempDir()
	path := filepath.Join(dir, "expectation.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runWithSettingsLoader([]string{"-once", "-include-signal", "api-release-proof", "-api-release-proof", path}, &out,
		func() (servermonitor.SignalSettings, error) {
			if err := os.WriteFile(path, []byte("invalid edited policy"), 0600); err != nil {
				t.Fatal(err)
			}
			return servermonitor.SignalSettings{Environment: "synthetic", SSHUser: "synthetic", Hosts: []servermonitor.HostSettings{{Name: "synthetic"}},
				StateDir: dir, Now: func() time.Time { return now }}, nil
		})
	if err != nil || !strings.Contains(out.String(), "inventory-unavailable-or-over-bound") {
		t.Fatal("CLI lost its captured policy or contacted an unspecified inventory")
	}
	if _, err := os.Stat(filepath.Join(dir, "api-release-proof", "latest.json")); err != nil {
		t.Fatal("one-shot did not preserve a finite unavailable observation")
	}
}
