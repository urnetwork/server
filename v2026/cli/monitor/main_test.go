package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	servermonitor "github.com/urnetwork/server/v2026/monitor"
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
		}},
	}
	load := func() (servermonitor.SignalSettings, error) { return current, nil }
	opts := monitorOptions{
		mode:                  string(servermonitor.AddressModeLAN),
		keys:                  stringFlags{"testdata/key-one", "testdata/key-two"},
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
		len(startup.Hosts[0].EdgeIPv6) != 0 {
		t.Fatalf("CLI overrides not applied exactly: %+v", startup)
	}

	current.PublicDomain = "changed.example.test"
	matched, err = check(context.Background(), startup)
	if err != nil || matched {
		t.Fatalf("underlying generation change matched=%t err=%v", matched, err)
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
