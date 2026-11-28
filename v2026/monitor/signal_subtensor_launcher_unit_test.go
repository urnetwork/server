package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Runs the signal over one observation with the host's launcher unit set.
func runSyntheticSubtensorWithLauncher(t *testing.T, observation subtensorObservation, launcherUnit string) (Alerts, string) {
	t.Helper()
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	sent := ""
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Subtensor == nil || !strings.Contains(command, subtensorMarker) {
			return "", errors.New("unexpected command")
		}
		sent = command
		return string(encoded), nil
	}}
	settings := subtensorSyntheticSettings(source)
	for i := range settings.Hosts {
		if settings.Hosts[i].Subtensor != nil {
			settings.Hosts[i].Subtensor.ExpectedSpecVersion = 452
			settings.Hosts[i].Subtensor.LauncherUnit = launcherUnit
		}
	}
	alerts, err := NewSubtensorSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts, sent
}

// On 2026-10-19 snow served its Finney archive through subtensor-mainnet.service
// while the publication run kept the retired testnet launcher subtensor.service
// stopped and disabled. Checking the hard-coded testnet unit paged
// subtensor-gateway (subtensor=inactive) for a healthy, published archive.
func TestSubtensorSignalChecksTheConfiguredLauncherUnit(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Units = map[string]string{
		"subtensor": "inactive", "subtensor-mainnet": "active", "nginx": "active", "openvpn@by-pre": "active",
	}
	alerts, command := runSyntheticSubtensorWithLauncher(t, observation, "subtensor-mainnet")
	for _, alert := range alerts {
		if alert.Class == "subtensor-gateway" && alert.Frame == "host" {
			t.Fatalf("the retired launcher paged although the configured one is active: %+v", alert)
		}
	}
	if !strings.Contains(command, `"launcher_unit":"subtensor-mainnet"`) {
		t.Fatalf("the host command does not carry the configured launcher unit: %s", command)
	}
}

// The configured launcher is the one that must be active, and the alert names it.
func TestSubtensorSignalNamesTheConfiguredLauncherWhenItStops(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Units = map[string]string{
		"subtensor": "active", "subtensor-mainnet": "inactive", "nginx": "active", "openvpn@by-pre": "active",
	}
	alerts, _ := runSyntheticSubtensorWithLauncher(t, observation, "subtensor-mainnet")
	gateway := requireAlertClass(t, alerts, "subtensor-gateway")
	if gateway.Frame != "host" || !strings.Contains(gateway.Observed, "subtensor-mainnet=inactive") {
		t.Fatalf("the stopped configured launcher was not named: %+v", gateway)
	}
	if strings.Contains(gateway.Observed, "subtensor=") {
		t.Fatalf("the alert judged a unit the host does not use: %+v", gateway)
	}
	for _, want := range []string{"subtensor-mainnet.service", "subtensor-mainnet, nginx, and openvpn@by-pre are active"} {
		if !strings.Contains(gateway.Mechanism+gateway.Baseline, want) {
			t.Errorf("gateway text lacks %q: %+v", want, gateway)
		}
	}
}

// An unset launcher keeps the original testnet archive's unit.
func TestSubtensorSignalDefaultsToTheTestnetLauncherUnit(t *testing.T) {
	observation := healthySubtensorObservation()
	observation.Units = map[string]string{"subtensor": "inactive", "nginx": "active", "openvpn@by-pre": "active"}
	alerts, command := runSyntheticSubtensorWithLauncher(t, observation, "")
	gateway := requireAlertClass(t, alerts, "subtensor-gateway")
	if !strings.Contains(gateway.Observed, "subtensor=inactive") {
		t.Fatalf("the default launcher was not judged: %+v", gateway)
	}
	if !strings.Contains(command, `"launcher_unit":"subtensor"`) {
		t.Fatalf("the host command does not carry the default launcher unit: %s", command)
	}
}

// The configured unit is a unit name, never a shell or systemd pattern: the
// host's settings are unobservable and the host is not contacted.
func TestSubtensorSettingsRejectAnInvalidLauncherUnit(t *testing.T) {
	for _, unit := range []string{"subtensor mainnet", "subtensor;id", "*", "../subtensor", "subtensor-mainnet.service"} {
		if err := validateSubtensorSettings(&SubtensorHostSettings{LauncherUnit: unit}); err == nil || !strings.Contains(err.Error(), "launcher unit") {
			t.Errorf("launcher unit %q: validation = %v, want a launcher unit error", unit, err)
		}
		contacted := false
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			contacted = true
			return "", fmt.Errorf("unexpected contact")
		}}
		settings := subtensorSyntheticSettings(source)
		for i := range settings.Hosts {
			if settings.Hosts[i].Subtensor != nil {
				settings.Hosts[i].Subtensor.LauncherUnit = unit
			}
		}
		alerts, err := NewSubtensorSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		visibility := requireAlertClass(t, alerts, "cannot-observe")
		if contacted || visibility.Target != "chain.example.test/subtensor" {
			t.Errorf("launcher unit %q: contacted=%t visibility=%+v", unit, contacted, visibility)
		}
	}
}
