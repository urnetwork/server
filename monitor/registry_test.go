package monitor

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

var signalKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
var catalogProbePattern = regexp.MustCompile("(?m)^Probe: `([a-z][a-z0-9]*(?:-[a-z0-9]+)*)`$")

func TestRegisteredSignalsFollowNamedFileConvention(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	seenNumbers := map[string]bool{}
	seenKeys := map[string]bool{}
	for _, signal := range NewSignals() {
		if seenNumbers[signal.Number()] {
			t.Fatalf("signal number %s registered twice", signal.Number())
		}
		seenNumbers[signal.Number()] = true
		if !signalKeyPattern.MatchString(signal.Key()) {
			t.Fatalf("signal %s has invalid short key %q", signal.Number(), signal.Key())
		}
		if seenKeys[signal.Key()] {
			t.Fatalf("signal key %s registered twice", signal.Key())
		}
		seenKeys[signal.Key()] = true
		if signal.ID() == "" || signal.Name() == "" || signal.Cadence() <= 0 {
			t.Fatalf("incomplete signal registration: number=%q key=%q id=%q name=%q cadence=%s",
				signal.Number(), signal.Key(), signal.ID(), signal.Name(), signal.Cadence())
		}

		stem := "signal_" + strings.ReplaceAll(signal.Key(), "-", "_")
		for _, path := range []string{stem + ".go", stem + "_test.go"} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("registered SIGNALS.md §%s (%s) must have %s: %v", signal.Number(), signal.Key(), path, err)
			}
		}
		source, err := os.ReadFile(stem + ".go")
		if err == nil && !strings.Contains(string(source), "SIGNALS.md §"+signal.Number()) {
			t.Errorf("%s.go must link back to SIGNALS.md §%s in a comment", stem, signal.Number())
		}

		heading := "### " + signal.Number() + " "
		start := strings.Index(catalog, heading)
		if start < 0 {
			t.Errorf("SIGNALS.md has no heading for registered §%s", signal.Number())
			continue
		}
		section := catalog[start:]
		if next := strings.Index(section[1:], "\n### "); next >= 0 {
			section = section[:next+1]
		}
		if !strings.Contains(section, "Probe: `"+signal.Key()+"`") {
			t.Errorf("SIGNALS.md §%s must declare Probe: `%s`", signal.Number(), signal.Key())
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "signal_") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(name, "signal_"), ".go")
		key = strings.ReplaceAll(key, "_", "-")
		if !seenKeys[key] {
			t.Errorf("%s has semantic key %q but is not registered by NewSignals", name, key)
		}
	}
	for _, match := range catalogProbePattern.FindAllStringSubmatch(catalog, -1) {
		if !seenKeys[match[1]] {
			t.Errorf("SIGNALS.md declares Probe: `%s` but NewSignals does not register it", match[1])
		}
	}
}

func TestMonitorRunPreservesProbeFailureAsVisibilityAlert(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, errors.New("synthetic pg unavailable")
	}}
	monitor := NewWithSignals(syntheticSettings(source), NewContractRateSignal())
	alerts, err := monitor.Run(context.Background())
	if err == nil {
		t.Fatal("Run error = nil")
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestVisibilityAlertClassifiesSSHAdmissionResetAtTheHostBoundary(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	alert := visibilityAlert(
		settings,
		NewMigrationsSignal(),
		errors.New("by-us-fmt-5-edge-2: exit status 255: kex_exchange_identification: read: Connection reset by peer\nConnection reset by 172.28.208.182 port 22"),
	)
	if alert.Class != "ssh-admission-reset" {
		t.Fatalf("class = %q, want ssh-admission-reset", alert.Class)
	}
	if alert.Target != "by-us-fmt-5-edge-2" {
		t.Fatalf("target = %q, want by-us-fmt-5-edge-2", alert.Target)
	}
	for name, value := range map[string]string{
		"mechanism": alert.Mechanism,
		"action":    alert.Action,
		"verify":    alert.Verify,
	} {
		if !strings.Contains(value, "MaxStartups") {
			t.Errorf("%s does not preserve the MaxStartups discriminator: %q", name, value)
		}
	}
	if !strings.Contains(alert.Action, "Do not blame PostgreSQL") {
		t.Fatalf("action does not prevent false database attribution: %q", alert.Action)
	}
	if !strings.Contains(alert.Context, "failed_signal=migrations") {
		t.Fatalf("context does not retain the failed probe: %q", alert.Context)
	}
}

func TestVisibilityAlertKeepsRemoteCommandFailureGeneric(t *testing.T) {
	alert := visibilityAlert(
		syntheticSettings(&syntheticSource{}),
		NewMigrationsSignal(),
		errors.New("by-us-fmt-5-edge-2: exit status 1: psql: syntax error"),
	)
	if alert.Class != "cannot-observe" {
		t.Fatalf("class = %q, want cannot-observe", alert.Class)
	}
}

func TestMonitorSignalsReturnsCopy(t *testing.T) {
	monitor := New(SignalSettings{})
	first := monitor.Signals()
	want := len(first)
	first = first[:0]
	if got := len(monitor.Signals()); got != want {
		t.Fatalf("mutating returned slice changed registry: got %d, want %d", got, want)
	}
}

func TestExcludeSignalsExcludesOnlyNamedSignal(t *testing.T) {
	all := NewSignals()
	selected, err := ExcludeSignals(all, "edge-ipv6")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != len(all)-1 {
		t.Fatalf("selected %d signals from %d, want exactly one exclusion", len(selected), len(all))
	}
	for _, signal := range selected {
		if signal.Key() == "edge-ipv6" {
			t.Fatal("edge-ipv6 remained selected")
		}
	}
}

func TestExcludeSignalsRejectsUnknownIdentifier(t *testing.T) {
	if _, err := ExcludeSignals(NewSignals(), "not-a-signal"); err == nil {
		t.Fatal("unknown excluded signal was accepted")
	}
}
