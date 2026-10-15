package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestRolloutGuardSignalSyntheticUnsafeGuards(t *testing.T) {
	tests := []struct {
		guard        string
		class        string
		disabled     int64
		disabledName string
	}{
		{guard: rolloutGuardDrainOnly, class: "rollout-guard-stale"},
		{guard: rolloutGuardDisabled, class: "rollout-guard-disabled", disabled: 1, disabledName: "warp-synthetic-api-0.service"},
		{guard: rolloutGuardMissing, class: "rollout-guard-unverified"},
		{guard: rolloutGuardUnknown, class: "rollout-guard-unverified"},
	}
	for _, test := range tests {
		t.Run(test.guard, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
				if !strings.Contains(command, rolloutGuardMarker) {
					return "", errors.New("unexpected synthetic host command")
				}
				if !strings.Contains(command, "rollout_unit_pattern='warp-synthetic-*.service'") {
					return "", fmt.Errorf("command did not scope units to synthetic environment: %s", command)
				}
				return rolloutGuardFixture(rolloutGuardSample{
					managedHost:              true,
					enabledUnits:             20,
					runningUnits:             20,
					guard:                    test.guard,
					binaryChangeEpoch:        1788190000,
					oldestWorkerStartEpoch:   1788190100,
					newestWorkerStartEpoch:   1788190200,
					guardDisabledUnits:       test.disabled,
					guardDisabledWorkerNames: test.disabledName,
				}), nil
			}}
			settings := syntheticSettings(source)
			settings.Hosts = []HostSettings{{Name: "edge-1", Roles: []string{"services"}}}

			alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 1 {
				t.Fatalf("alerts = %d, want 1: %+v", len(alerts), alerts)
			}
			alert := requireAlertClass(t, alerts, test.class)
			if alert.SignalNumber != "8.11" || alert.SignalKey != "rollout-guard" || alert.SignalID != "deploy/rollout-guard" {
				t.Fatalf("wrong rollout guard identity: %+v", alert)
			}
			for _, want := range []string{
				"rollout_guard=" + test.guard,
				rolloutGuardCommit,
				rolloutGuardValidatedCommit,
				"restart every running Warp service worker",
				"software deployment gate",
				"does not create RAM",
				"Do not validate this by launching a full proxy-fleet rollout",
			} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("%s alert missing %q:\n%s", test.guard, want, alert.Markdown())
				}
			}
		})
	}
}

func TestRolloutGuardSignalSyntheticHealthyAndServiceHostScope(t *testing.T) {
	var lock sync.Mutex
	called := map[string]int{}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		lock.Lock()
		called[host.Name]++
		lock.Unlock()
		if !strings.Contains(command, rolloutGuardMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		switch host.Name {
		case "service-full":
			return rolloutGuardFixture(rolloutGuardSample{
				managedHost:            true,
				enabledUnits:           4,
				runningUnits:           4,
				guard:                  rolloutGuardFull,
				binaryChangeEpoch:      1788190000,
				oldestWorkerStartEpoch: 1788190001,
				newestWorkerStartEpoch: 1788190004,
			}), nil
		case "service-empty":
			return "managed_host 0\n", nil
		default:
			return "", fmt.Errorf("non-services host %s was contacted", host.Name)
		}
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "service-full", Roles: []string{"services"}},
		{Name: "redis-only", Roles: []string{"redis-cluster"}},
		{Name: "service-empty", Roles: []string{"services"}},
	}

	alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy rollout guards alerted: %+v", alerts)
	}
	if called["service-full"] != 1 || called["service-empty"] != 1 || called["redis-only"] != 0 {
		t.Fatalf("wrong host scope: %+v", called)
	}
}

func TestRolloutGuardSignalSyntheticStaleWorkers(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, rolloutGuardMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		return rolloutGuardFixture(rolloutGuardSample{
			managedHost:             true,
			enabledUnits:            4,
			runningUnits:            4,
			guard:                   rolloutGuardFull,
			binaryChangeEpoch:       1788190000,
			oldestWorkerStartEpoch:  1788180000,
			newestWorkerStartEpoch:  1788190100,
			staleWorkerUnits:        2,
			unverifiableWorkerUnits: 1,
			staleWorkerNames:        "warp-synthetic-api-0.service,warp-synthetic-connect-0.service",
			unverifiableWorkerNames: "warp-synthetic-taskworker-0.service",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge-3", Roles: []string{"services"}}}

	alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "rollout-guard-workers-stale")
	for _, want := range []string{
		"binary_change_epoch=1788190000",
		"oldest_worker_start_epoch=1788180000",
		"stale_worker_units=2",
		"unverifiable_worker_units=1",
		"warp-synthetic-api-0.service",
		"code already mapped by an older worker",
		"reinstalling the same binary without worker restarts is insufficient",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stale-worker alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestRolloutGuardSignalSyntheticPartialHostFailure(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, rolloutGuardMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		if host.Name == "edge-unreachable" {
			return "", errors.New("synthetic SSH timeout")
		}
		return rolloutGuardFixture(rolloutGuardSample{
			managedHost:            true,
			enabledUnits:           2,
			runningUnits:           2,
			guard:                  rolloutGuardDrainOnly,
			binaryChangeEpoch:      1788190000,
			oldestWorkerStartEpoch: 1788190100,
			newestWorkerStartEpoch: 1788190200,
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "edge-drain-only", Roles: []string{"services"}},
		{Name: "edge-unreachable", Roles: []string{"services"}},
	}

	alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("one host failure discarded partial results: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want concrete plus visibility: %+v", len(alerts), alerts)
	}
	requireAlertClass(t, alerts, "rollout-guard-stale")
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if visibility.Target != "edge-unreachable/rollout-guard" {
		t.Fatalf("visibility target = %q", visibility.Target)
	}
}

func rolloutGuardFixture(sample rolloutGuardSample) string {
	if !sample.managedHost {
		return "managed_host 0\n"
	}
	stringValue := func(value string) string {
		if value == "" {
			return "-"
		}
		return value
	}
	return fmt.Sprintf(
		"managed_host 1\n"+
			"enabled_units %d\n"+
			"running_units %d\n"+
			"rollout_guard %s\n"+
			"guard_disabled_units %d\n"+
			"guard_disabled_worker_names %s\n"+
			"binary_change_epoch %d\n"+
			"oldest_worker_start_epoch %d\n"+
			"newest_worker_start_epoch %d\n"+
			"stale_worker_units %d\n"+
			"unverifiable_worker_units %d\n"+
			"stale_worker_names %s\n"+
			"unverifiable_worker_names %s\n",
		sample.enabledUnits,
		sample.runningUnits,
		sample.guard,
		sample.guardDisabledUnits,
		stringValue(sample.guardDisabledWorkerNames),
		sample.binaryChangeEpoch,
		sample.oldestWorkerStartEpoch,
		sample.newestWorkerStartEpoch,
		sample.staleWorkerUnits,
		sample.unverifiableWorkerUnits,
		stringValue(sample.staleWorkerNames),
		stringValue(sample.unverifiableWorkerNames),
	)
}
