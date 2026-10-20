package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The real settings loader reads only synthetic, in-memory resources under
// private test roots. No native database, host transport or Vault is used.
func pgbouncerStarvationSettingsResources(t *testing.T) {
	t.Helper()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	t.Setenv("WARP_SITE_HOME", t.TempDir())
	t.Cleanup(server.Vault.PushSimpleResource("services.yml", []byte("domain: synthetic.example\nversions:\n  - services:\n      api:\n        blocks:\n          - synthetic: 100\n")))
	t.Cleanup(server.Vault.PushSimpleResource("pg.yml", []byte("db: app\nuser: synthetic\npassword: synthetic-test-only\n")))
}

// An explicitly configured host count survives YAML loading, SignalSettings,
// the internal config and the public source adapter; malformed counts remain
// local visibility gaps, leaving independent positive pool evidence intact.
func TestPgBouncerPoolStarvationAuthoritySettingsLoaderKeepsCensusAuthority(t *testing.T) {
	pgbouncerStarvationSettingsResources(t)
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		declaration string
		complete    bool
	}{
		{declaration: "    pgbouncer_instances: 2\n", complete: true},
		{declaration: "", complete: false},
		{declaration: "    pgbouncer_instances: 0\n", complete: false},
		{declaration: "    pgbouncer_instances: -2\n", complete: false},
		{declaration: "    pgbouncer_instances: 2.5\n", complete: false},
		{declaration: "    pgbouncer_instances: [2]\n", complete: false},
		{declaration: "    pgbouncer_instances: private-census-canary\n", complete: false},
		{declaration: "    pgbouncer_instances: 9999999999999999999999999999999\n", complete: false},
	} {
		monitor := "ssh:\n  user: synthetic\nhosts:\n  - name: db-1\n    roles: [pg-primary]\n" + c.declaration + "  - name: metrics-1\n    roles: [services]\n"
		pop := server.Vault.PushSimpleResource("monitor.yml", []byte(monitor))
		settings, err := LoadSignalSettings()
		pop()
		if err != nil {
			t.Fatal("invalid pool census escaped the owning visibility boundary")
		}
		payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1), pgbouncerStarvationHealthy(2)})
		settings.Environment = "synthetic"
		settings.Now = func() time.Time { return now }
		settings.Source = &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				t.Fatal("settings fixture contacted a non-gateway host")
			}
			return pgbouncerStarvationResponseForQuery(t, payload, command), nil
		}}
		alerts, err := pgbouncerPoolStarvationSignalForTest(t).Run(context.Background(), settings)
		if err != nil {
			t.Fatal("loaded synthetic census did not reach the public signal")
		}
		if c.complete {
			requireNoPgBouncerStarvationAlerts(t, alerts)
		} else {
			unknown := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
			if !strings.Contains(unknown.Observed, "instance_census_unconfigured") {
				t.Fatal("missing or invalid configured census was silently inferred")
			}
		}
		requirePgBouncerStarvationPrivate(t, alerts, "private-census-canary")
		payload = pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)})
		alerts, err = pgbouncerPoolStarvationSignalForTest(t).Run(context.Background(), settings)
		if err != nil {
			t.Fatal("loaded census failed the independent positive control")
		}
		requireAlertClass(t, alerts, "pgbouncer-pools-starved")
	}
}

// Changing an intended pool count creates a new settings generation; it
// cannot silently redefine a running watcher's recovery denominator.
func TestPgBouncerPoolStarvationAuthorityCensusChangesSettingsGeneration(t *testing.T) {
	pgbouncerStarvationSettingsResources(t)
	load := func(count string) SignalSettings {
		pop := server.Vault.PushSimpleResource("monitor.yml", []byte("ssh:\n  user: synthetic\nhosts:\n  - name: db-1\n    roles: [pg-primary]\n    pgbouncer_instances: "+count+"\n  - name: metrics-1\n    roles: [services]\n"))
		defer pop()
		settings, err := LoadSignalSettings()
		if err != nil {
			t.Fatal("could not load synthetic census generation")
		}
		return settings
	}
	startup, current := load("2"), load("3")
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	unchanged, err := check(context.Background(), startup)
	if err != nil || unchanged {
		t.Fatal("a changed pool census did not invalidate the running generation")
	}
}
