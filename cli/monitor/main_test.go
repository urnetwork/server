package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	servermonitor "github.com/urnetwork/server/monitor"
)

func TestEffectiveSettingsGenerationReappliesCLIOverridesAndExclusions(t *testing.T) {
	current := servermonitor.SignalSettings{
		Environment:  "synthetic",
		PublicDomain: "service.example.test",
		AddressMode:  servermonitor.AddressModeOverlay,
		SSHKeyPaths:  []string{"testdata/default-key"},
		Hosts: []servermonitor.HostSettings{{
			Name:     "edge-a.example.test",
			EdgeIPv6: []servermonitor.EdgeIPv6InterfaceSettings{{Interface: "public-a"}},
		}, {
			Name: "paused.example.test", Roles: []string{"subtensor"},
		}},
	}
	load := func() (servermonitor.SignalSettings, error) { return current, nil }
	opts := monitorOptions{
		mode:                  string(servermonitor.AddressModeLAN),
		keys:                  stringFlags{"testdata/key-one", "testdata/key-two"},
		excludedHosts:         stringFlags{"paused.example.test", "paused.example.test"},
		excludedEdgeIPv6Hosts: stringFlags{"edge-a.example.test"},
	}
	loadEffective := func() (servermonitor.SignalSettings, error) {
		settings, err := load()
		if err != nil {
			return servermonitor.SignalSettings{}, err
		}
		return applyMonitorSettingsOptions(settings, opts)
	}
	startup, err := loadEffective()
	if err != nil {
		t.Fatal(err)
	}
	check := servermonitor.NewSettingsGenerationCheck(loadEffective)
	startup.SettingsGenerationCheck = check

	matched, err := check(context.Background(), startup)
	if err != nil || !matched {
		t.Fatalf("unchanged settings with CLI overrides matched=%t err=%v", matched, err)
	}
	if startup.AddressMode != servermonitor.AddressModeLAN ||
		!reflect.DeepEqual(startup.SSHKeyPaths, []string{"testdata/key-one", "testdata/key-two"}) ||
		len(startup.Hosts[0].EdgeIPv6) != 0 || len(startup.Hosts) != 2 ||
		!reflect.DeepEqual(startup.ExcludedHosts, []string{"paused.example.test"}) {
		t.Fatalf("CLI overrides not applied exactly: %+v", startup)
	}

	current.PublicDomain = "changed.example.test"
	matched, err = check(context.Background(), startup)
	if err != nil || matched {
		t.Fatalf("underlying generation change matched=%t err=%v", matched, err)
	}
}

// Repeated host policies preserve exact parsed names and coexist with the
// narrower IPv6 pause; the full inventory remains unchanged.
func TestParseMonitorOptionsAcceptsRepeatableHostExclusions(t *testing.T) {
	opts, err := parseMonitorOptions([]string{"-exclude-host", "a.example.test", "-exclude-host", "b.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(opts.excludedHosts), []string{"a.example.test", "b.example.test"}) {
		t.Fatalf("host selectors changed: %v", opts.excludedHosts)
	}
	settings, err := applyMonitorSettingsOptions(
		servermonitor.SignalSettings{Hosts: []servermonitor.HostSettings{{Name: "a.example.test", EdgeIPv6: []servermonitor.EdgeIPv6InterfaceSettings{{Interface: "public"}}}}},
		monitorOptions{excludedHosts: stringFlags{"a.example.test"}, excludedEdgeIPv6Hosts: stringFlags{"a.example.test"}},
	)
	if err != nil || len(settings.Hosts) != 1 || len(settings.Hosts[0].EdgeIPv6) != 0 || len(settings.ExcludedHosts) != 1 {
		t.Fatalf("combined same-host pauses are not deterministic: %v", err)
	}
}

// A current loader failure or disappearing exact selector must be unknown;
// it must not clear a policy or capture a second baseline during comparison.
func TestEffectiveHostScopeGenerationFailsClosedOnReloadErrors(t *testing.T) {
	current := servermonitor.SignalSettings{Hosts: []servermonitor.HostSettings{{Name: "a.example.test"}}}
	var loadErr error
	opts := monitorOptions{excludedHosts: stringFlags{"a.example.test"}}
	loadEffective := func() (servermonitor.SignalSettings, error) {
		if loadErr != nil {
			return servermonitor.SignalSettings{}, loadErr
		}
		return applyMonitorSettingsOptions(current, opts)
	}
	startup, err := loadEffective()
	if err != nil {
		t.Fatal(err)
	}
	check := servermonitor.NewSettingsGenerationCheck(loadEffective)
	current.Hosts = []servermonitor.HostSettings{{Name: "b.example.test"}}
	if matched, err := check(context.Background(), startup); err == nil || matched {
		t.Fatalf("removed exact selector was silently ignored: matched=%t err=%v", matched, err)
	}
	loadErr = errors.New("synthetic-secret-reload-failure")
	if matched, err := check(context.Background(), startup); err == nil || matched {
		t.Fatalf("failed loader matched startup: %t %v", matched, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if matched, err := check(ctx, startup); !errors.Is(err, context.Canceled) || matched {
		t.Fatalf("canceled scope reload matched startup: %t %v", matched, err)
	}
}

func TestEffectiveSettingsGenerationRejectsUnknownExcludedHost(t *testing.T) {
	_, err := applyMonitorSettingsOptions(
		servermonitor.SignalSettings{Hosts: []servermonitor.HostSettings{{Name: "edge-a.example.test"}}},
		monitorOptions{excludedEdgeIPv6Hosts: stringFlags{"edge-b.example.test"}},
	)
	if err == nil || !strings.Contains(err.Error(), "is not configured") {
		t.Fatalf("unknown exclusion error = %v", err)
	}
}

func TestRunListsSignalsWithoutLoadingSettings(t *testing.T) {
	loadCalls := 0
	loader := func() (servermonitor.SignalSettings, error) {
		loadCalls++
		return servermonitor.SignalSettings{}, errors.New("settings must not be loaded")
	}
	var first bytes.Buffer
	if err := runWithSettingsLoader([]string{"-list-signals"}, &first, loader); err != nil {
		t.Fatal(err)
	}
	if loadCalls != 0 {
		t.Fatalf("settings loader called %d times, want 0", loadCalls)
	}

	var second bytes.Buffer
	if err := runWithSettingsLoader([]string{"-list-signals"}, &second, loader); err != nil {
		t.Fatal(err)
	}
	if loadCalls != 0 {
		t.Fatalf("settings loader called %d times, want 0", loadCalls)
	}
	if first.String() != second.String() {
		t.Fatal("signal list is not deterministic")
	}
	lines := strings.Split(strings.TrimSuffix(first.String(), "\n"), "\n")
	signals := servermonitor.NewSignals()
	if len(lines) != len(signals)+1 {
		t.Fatalf("signal list has %d lines, want %d", len(lines), len(signals)+1)
	}
	if lines[0] != "NUMBER\tKEY\tID\tNAME" {
		t.Fatalf("signal list header = %q", lines[0])
	}
	firstSignal := signals[0]
	wantFirst := firstSignal.Number() + "\t" + firstSignal.Key() + "\t" + firstSignal.ID() + "\t" + firstSignal.Name()
	if lines[1] != wantFirst {
		t.Fatalf("first listed signal = %q, want %q", lines[1], wantFirst)
	}
}

func TestParseMonitorOptionsAcceptsRepeatableIncludesAndJSONL(t *testing.T) {
	opts, err := parseMonitorOptions([]string{
		"-include-signal", "1.1",
		"-include-signal", "redis-cluster",
		"-format", "jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(opts.includedSignals), []string{"1.1", "redis-cluster"}) {
		t.Fatalf("included signals = %v", opts.includedSignals)
	}
	if opts.format != alertFormatJSONL {
		t.Fatalf("format = %q, want jsonl", opts.format)
	}
}

func TestRunRejectsIncludeAndExcludeBeforeLoadingSettings(t *testing.T) {
	loadCalls := 0
	loader := func() (servermonitor.SignalSettings, error) {
		loadCalls++
		return servermonitor.SignalSettings{}, nil
	}
	err := runWithSettingsLoader([]string{
		"-include-signal", "contract-rate",
		"-exclude-signal", "redis-cluster",
	}, &bytes.Buffer{}, loader)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("run error = %v, want mutually exclusive selectors", err)
	}
	if loadCalls != 0 {
		t.Fatalf("settings loader called %d times, want 0", loadCalls)
	}
}

func TestRunRejectsUnknownIncludeBeforeLoadingSettings(t *testing.T) {
	loadCalls := 0
	loader := func() (servermonitor.SignalSettings, error) {
		loadCalls++
		return servermonitor.SignalSettings{}, nil
	}
	err := runWithSettingsLoader([]string{"-include-signal", "not-a-signal"}, &bytes.Buffer{}, loader)
	if err == nil || !strings.Contains(err.Error(), `included signal "not-a-signal" is not registered`) {
		t.Fatalf("run error = %v, want unknown include rejection", err)
	}
	if loadCalls != 0 {
		t.Fatalf("settings loader called %d times, want 0", loadCalls)
	}
}

func TestWriteAlertsUsesRequestedFormat(t *testing.T) {
	alert := servermonitor.Alert{
		SignalNumber: "1.1", SignalKey: "alpha", SignalID: "probe/alpha", SignalName: "Alpha",
		Severity: servermonitor.SeverityWarn, Class: "slow", Target: "alpha-1", Environment: "synthetic",
		ObservedAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), Symptom: "alpha slow",
	}
	var markdown bytes.Buffer
	if err := writeAlerts(&markdown, alertFormatMarkdown, servermonitor.Alerts{alert}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(markdown.String(), "# Monitor alerts\n") {
		t.Fatalf("Markdown output = %q", markdown.String())
	}
	var jsonl bytes.Buffer
	if err := writeAlerts(&jsonl, alertFormatJSONL, servermonitor.Alerts{alert}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(jsonl.String(), `{"signal_number":"1.1"`) || !strings.HasSuffix(jsonl.String(), "\n") {
		t.Fatalf("JSONL output = %q", jsonl.String())
	}
}

func TestParseMonitorOptionsRejectsContinuousOutput(t *testing.T) {
	if _, err := parseMonitorOptions([]string{"-output", "synthetic-alerts.md"}); err == nil || !strings.Contains(err.Error(), "requires -once") {
		t.Fatalf("continuous output error = %v", err)
	}
}

func TestOpenAlertOutputWritesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.md")
	output, closeOutput, err := openAlertOutput(&bytes.Buffer{}, path)
	if err != nil {
		t.Fatal(err)
	}
	alerts := servermonitor.Alerts{}
	for index := 0; index < 96; index++ {
		alerts = append(alerts, servermonitor.Alert{
			SignalNumber: "1.1", SignalKey: "synthetic", SignalID: "synthetic/probe", SignalName: "Synthetic",
			Severity: servermonitor.SeverityWarn, Class: "large-output", Target: fmt.Sprintf("synthetic-target-%d", index), Environment: "synthetic",
			ObservedAt: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC), Symptom: "synthetic output",
			Mechanism: strings.Repeat("synthetic-detail ", 128), Baseline: "complete synthetic output",
			Observed: "synthetic=true", Action: "verify direct output", Verify: "complete marker remains present",
		})
	}
	if err := writeAlerts(output, alertFormatMarkdown, alerts); err != nil {
		t.Fatal(err)
	}
	if err := closeOutput(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) < 128*1024 || !strings.HasSuffix(string(contents), "<!-- monitor-alerts-complete -->\n") {
		t.Fatalf("large direct output was incomplete: bytes=%d terminal=%q", len(contents), contents[max(0, len(contents)-64):])
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %o, want 600", info.Mode().Perm())
	}
}
