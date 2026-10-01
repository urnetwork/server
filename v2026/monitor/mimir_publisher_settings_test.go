package monitor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMimirPublisherSettingsReadOnlyOwnedPreferenceFields(t *testing.T) {
	settings := parseMimirPublisherSettings([]byte(`dbs:
  hosts:
    database.invalid:
      fluent_bit_grafana_preferred_host: front-a.invalid
      private_fixture: never-retain-this
  vars:
    private_fixture: never-retain-this
redis:
  hosts:
    cache.invalid:
      fluent_bit_grafana_preferred_host: front-b.invalid
`))
	want := MimirPublisherSettings{LoadState: "ready", PreferredFronts: map[string]string{
		"database.invalid": "front-a.invalid", "cache.invalid": "front-b.invalid",
	}}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("settings retained unexpected data: %+v", settings)
	}
}

func TestMimirPublisherSettingsMalformedConflictingAndOversizedFailClosed(t *testing.T) {
	for _, fixture := range []string{
		"", "dbs: [never-retain-this", "dbs:\n  hosts: never-retain-this\n",
		"dbs:\n  hosts:\n    database.invalid:\n      fluent_bit_grafana_preferred_host: front-a.invalid\nredis:\n  hosts:\n    database.invalid:\n      fluent_bit_grafana_preferred_host: front-b.invalid\n",
		strings.Repeat("x", mimirPublisherInventoryLimit+1),
	} {
		settings := parseMimirPublisherSettings([]byte(fixture))
		if settings.LoadState != "invalid" || settings.PreferredFronts != nil {
			t.Fatalf("malformed source became observable or retained private data: %+v", settings)
		}
	}
}

func TestMimirPublisherSettingsLoaderUsesScopedInventoryAndHandlesMissingFile(t *testing.T) {
	directory := t.TempDir()
	if settings := loadMimirPublisherSettings(directory, "synthetic"); settings.LoadState != "unavailable" {
		t.Fatalf("missing inventory state=%q", settings.LoadState)
	}
	if settings := loadMimirPublisherSettings(directory, "../other"); settings.LoadState != "invalid" {
		t.Fatalf("unsafe environment state=%q", settings.LoadState)
	}
	path := filepath.Join(directory, "xops", "synthetic", "ansible", "inventory.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("dbs:\n  hosts:\n    database.invalid:\n      fluent_bit_grafana_preferred_host: front-a.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := loadMimirPublisherSettings(directory, "synthetic")
	if settings.LoadState != "ready" || settings.PreferredFronts["database.invalid"] != "front-a.invalid" {
		t.Fatalf("scoped inventory wasn't loaded: %+v", settings)
	}
}

func TestMimirPublisherSettingsSnapshotClonesAndDetectsPreferenceChanges(t *testing.T) {
	settings := mimirPublisherSettings(&syntheticSource{})
	cfg := configFromSignalSettings(settings)
	settings.MimirPublishers.PreferredFronts["database.invalid"] = "front-b.invalid"
	if cfg.mimirPublishers.PreferredFronts["database.invalid"] != "front-a.invalid" {
		t.Fatal("caller mutation changed immutable monitor snapshot")
	}
	startup := mimirPublisherSettings(&syntheticSource{})
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return settings, nil })
	current, err := check(context.Background(), startup)
	if err != nil || current {
		t.Fatalf("changed preference didn't make generation stale: current=%t err=%v", current, err)
	}
}
