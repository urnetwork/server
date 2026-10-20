package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	servermonitor "github.com/urnetwork/server/monitor"
)

// The default focused CLI also runs Tier 0 vitals. Supply every source
// operation explicitly, and require admission before any synthetic contact.
type sharedSshOnceSource struct {
	t                              *testing.T
	admitted                       bool
	pgCalls, hostCalls, redisCalls int
}

// One synthetic gateway permits the pool probe to return unknown coverage.
func (self *sharedSshOnceSource) settings() servermonitor.SignalSettings {
	return servermonitor.SignalSettings{
		Environment: "synthetic", Source: self,
		Hosts: []servermonitor.HostSettings{{Name: "services-fixture.example", Roles: []string{"services"}}},
	}
}

// The setup context is already canceled when signal execution starts; source
// operations must receive the live observation context after admission returns.
func (self *sharedSshOnceSource) requireAdmission(ctx context.Context) {
	self.t.Helper()
	if !self.admitted || ctx.Err() != nil {
		self.t.Fatal("signal source ran without completed diagnostic admission and a live context")
	}
}

// A complete, consistent aggregate belongs only to connection-orphans.
func (self *sharedSshOnceSource) PostgreSQL(ctx context.Context, query string) ([]servermonitor.Row, error) {
	self.t.Helper()
	self.pgCalls++
	self.requireAdmission(ctx)
	if !strings.Contains(query, "FROM network_client_connection") || !strings.Contains(query, "stale_open_connection_count") {
		self.t.Fatal("diagnostic fixture received an unexpected PostgreSQL query")
	}
	return []servermonitor.Row{{"2", "0", "0", "0", "1", "0", "0"}}, nil
}

// An empty successful vector is unknown pool coverage, never synthetic health.
func (self *sharedSshOnceSource) Host(ctx context.Context, host servermonitor.HostSettings, command string) (string, error) {
	self.t.Helper()
	self.hostCalls++
	self.requireAdmission(ctx)
	if host.Name != "services-fixture.example" || !strings.Contains(command, "pgbouncer_pool_clients_waiting") || !strings.Contains(command, "--max-time 15") {
		self.t.Fatal("diagnostic fixture received an unexpected host operation")
	}
	return `{"status":"success","data":{"resultType":"vector","result":[]}}`, nil
}

// No selected vital reads Redis under this fixture's incomplete inventory.
func (self *sharedSshOnceSource) Redis(context.Context, servermonitor.HostSettings, int, ...string) (string, error) {
	self.redisCalls++
	return "", errors.New("unexpected diagnostic Redis operation")
}

// Keep default Tier 0 selection while proving setup precedes the selected
// settings reload and both actual source operations.
func TestSharedSshOnceUsesDiagnosticSetupBeforeSignals(t *testing.T) {
	source := &sharedSshOnceSource{t: t}
	loads := 0
	loader := func() (servermonitor.SignalSettings, error) {
		loads++
		if loads > 1 && !source.admitted {
			t.Fatal("selected settings signal ran before diagnostic admission")
		}
		return source.settings(), nil
	}
	called := 0
	var output bytes.Buffer
	err := runWithSettingsAndSshAdmission([]string{"-once", "-ssh-admission-dir", "/synthetic/private/ssh", "-include-signal", "settings-freshness"}, &output, loader,
		func(ctx context.Context, settings servermonitor.SignalSettings, directory string, once bool) (servermonitor.SignalSettings, error) {
			called++
			if _, bounded := ctx.Deadline(); !bounded || !once || directory != "/synthetic/private/ssh" || settings.Environment != "synthetic" {
				t.Fatal("CLI lost bounded diagnostic admission selection")
			}
			if loads != 1 || source.pgCalls != 0 || source.hostCalls != 0 || source.redisCalls != 0 {
				t.Fatal("signals ran before diagnostic admission setup")
			}
			source.admitted = true
			return settings, nil
		})
	if err != nil || called != 1 || !strings.Contains(output.String(), "monitor-alerts-complete") {
		t.Fatalf("bounded diagnostic did not finish through selected signal: setup=%d err=%v", called, err)
	}
	if loads != 2 || source.pgCalls != 1 || source.hostCalls != 1 || source.redisCalls != 0 {
		t.Fatalf("diagnostic source counts: loads=%d pg=%d host=%d redis=%d, want 2/1/1/0", loads, source.pgCalls, source.hostCalls, source.redisCalls)
	}
	for _, identity := range []string{
		"mimir/provider-connections|provider-connections-unobservable|synthetic/connect-fleet|",
		"mimir/pgbouncer-pool-starvation|pgbouncer-pools-unobservable|synthetic/pgbouncer-pools|",
	} {
		if !strings.Contains(output.String(), identity) {
			t.Fatal("diagnostic suppressed an unknown Tier 0 vital")
		}
	}
	t.Logf("setup=%d loads=%d pg=%d host=%d redis=%d unknown_vitals=2", called, loads, source.pgCalls, source.hostCalls, source.redisCalls)
}

// Admission refusal preserves its cause and reaches neither the settings
// signal's reload nor any source operation, despite default Tier 0 selection.
func TestSharedSshOnceSetupRefusalStopsBeforeSignals(t *testing.T) {
	want := errors.New("synthetic admission refusal")
	source := &sharedSshOnceSource{t: t}
	loads, called := 0, 0
	var output bytes.Buffer
	err := runWithSettingsAndSshAdmission([]string{"-once", "-ssh-admission-dir", "/synthetic/private/ssh", "-include-signal", "settings-freshness"}, &output,
		func() (servermonitor.SignalSettings, error) {
			loads++
			return source.settings(), nil
		}, func(context.Context, servermonitor.SignalSettings, string, bool) (servermonitor.SignalSettings, error) {
			called++
			return servermonitor.SignalSettings{}, want
		})
	if !errors.Is(err, want) || output.Len() != 0 {
		t.Fatal("admission refusal became a completed observation")
	}
	if loads != 1 || called != 1 || source.admitted || source.pgCalls != 0 || source.hostCalls != 0 || source.redisCalls != 0 {
		t.Fatal("admission refusal reached a signal, retried setup or changed source admission")
	}
	t.Logf("refusal_setup=%d loads=%d pg=%d host=%d redis=%d output_bytes=%d", called, loads, source.pgCalls, source.hostCalls, source.redisCalls, output.Len())
}

func TestSharedSshOncePreservesListingAndContinuousGuards(t *testing.T) {
	for _, args := range [][]string{
		{"-list-signals", "-ssh-admission-dir", "/synthetic/private/ssh"},
		{"-once", "-ssh-admission-dir", "/synthetic/private/ssh", "-pg-query-sample-continuous", "-min-probe-cadence", "15m"},
		{"-once", "-ssh-admission-dir", "/synthetic/private/ssh", "-min-probe-cadence", "15m"},
	} {
		err := runWithSettingsLoader(args, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
			t.Fatal("invalid mode loaded settings")
			return servermonitor.SignalSettings{}, nil
		})
		if err == nil {
			t.Fatal("shared diagnostic bypassed signal-list/cadence/PG continuous guard")
		}
	}
	if err := runWithSettingsLoader([]string{"-list-signals"}, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
		t.Fatal("plain signal listing loaded settings")
		return servermonitor.SignalSettings{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}
