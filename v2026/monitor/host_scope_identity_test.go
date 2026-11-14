package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Exercise the real snapshot aggregator and registered probes, not a rendered
// string deduper. Each probe owns different denied-source coverage.
func TestHostScopeSnapshotPreservesDistinctProbeIdentities(t *testing.T) {
	const (
		excludedService = "paused-service.example.test"
		excludedChain   = "paused-chain.example.test"
		allowedService  = "allowed.example.test"
		unknownService  = "unavailable.example.test"
	)
	stableIdentities := map[string]string{}
	for _, test := range []struct {
		name          string
		excluded      []string
		unknown       bool
		allowedFault  bool
		blockedCounts map[string]int
	}{
		{name: "two distinct denied sources", excluded: []string{excludedService, excludedChain}, unknown: true, allowedFault: true,
			blockedCounts: map[string]int{"log-shipper": 2, "journal-buffer": 1, "settings-freshness": 0}},
		{name: "changed counts preserve identities", excluded: []string{excludedService}, unknown: true, allowedFault: true,
			blockedCounts: map[string]int{"log-shipper": 1, "journal-buffer": 1, "settings-freshness": 0}},
		{name: "unscoped source failures remain unknown", unknown: true, allowedFault: true},
		{name: "healthy unscoped control"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := map[string]int{}
			var callsLock sync.Mutex
			source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
				callsLock.Lock()
				calls[host.Name]++
				callsLock.Unlock()
				if test.unknown && host.Name == unknownService {
					return "", errors.New("synthetic-private-command-failure")
				}
				switch {
				case strings.Contains(command, logShipperMarker):
					if test.allowedFault && host.Name == allowedService {
						return logShipperFixture(map[string]string{"nofile_soft": "1024"}), nil
					}
					return logShipperFixture(nil), nil
				case strings.Contains(command, journalBufferMarker):
					return journalBufferFixture(nil), nil
				default:
					return "", errors.New("unexpected synthetic source operation")
				}
			}}
			settings := syntheticSettings(source)
			settings.Hosts = []HostSettings{
				{Name: excludedService, Roles: []string{"services"}},
				{Name: excludedChain, Roles: []string{"subtensor"}},
				{Name: allowedService, Roles: []string{"services"}},
				{Name: unknownService, Roles: []string{"services"}},
			}
			settings.LogServices = []string{"api", "proxy"}
			settings.LogServiceBlocks = map[string][]string{"api": {"blue"}, "proxy": {"green"}}
			settings.ProxyPathExpectedHosts = 7
			before := settings
			var err error
			if len(test.excluded) != 0 {
				settings, err = ExcludeHosts(settings, test.excluded...)
				if err != nil {
					t.Fatal(err)
				}
			}
			signals := []Signal{NewLogShipperSignal(), NewJournalBufferSignal(), NewSettingsFreshnessSignal()}
			monitor := NewWithSignals(settings, signals...)
			alerts, err := monitor.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(settings.Hosts, before.Hosts) || !reflect.DeepEqual(settings.LogServices, before.LogServices) ||
				!reflect.DeepEqual(settings.LogServiceBlocks, before.LogServiceBlocks) || settings.ProxyPathExpectedHosts != before.ProxyPathExpectedHosts {
				t.Fatal("snapshot changed desired topology, service streams, or expected denominator")
			}
			for _, name := range test.excluded {
				if calls[name] != 0 {
					t.Fatal("snapshot contacted an excluded synthetic host")
				}
			}
			if calls[allowedService] != 2 || calls[unknownService] != 2 {
				t.Fatal("snapshot skipped a permitted independent source")
			}
			if len(test.excluded) == 0 && (calls[excludedService] != 2 || calls[excludedChain] != 1) {
				t.Fatal("unscoped healthy control did not contact every intended source")
			}

			scope := map[string]Alert{}
			identities := map[string]bool{}
			unknownCount, allowedFaultCount := 0, 0
			for _, alert := range alerts {
				if identities[alert.Identity()] {
					t.Fatal("different probe coverage facts collided in the snapshot identity")
				}
				identities[alert.Identity()] = true
				switch alert.Class {
				case "monitor-host-scope-partial":
					blocked, expected := test.blockedCounts[alert.SignalKey]
					if !expected || alert.Frame != alert.SignalKey || alert.Severity != SeverityWarn || alert.PageSustain != 0 {
						t.Fatal("scope warning lost its owning probe frame or operational severity")
					}
					if alert.SignalID != "monitor/host-scope" || alert.Target != "monitor-host-scope" ||
						!strings.Contains(alert.Observed, fmt.Sprintf("blocked_hosts=%d ", blocked)) {
						t.Fatal("scope identity or per-probe denied-host count changed")
					}
					if prior := stableIdentities[alert.SignalKey]; prior != "" && prior != alert.Identity() {
						t.Fatal("coverage counts changed the owning probe identity")
					}
					stableIdentities[alert.SignalKey] = alert.Identity()
					if !strings.Contains(alert.Markdown(), alert.Identity()) {
						t.Fatal("Markdown lost the distinct coverage identity")
					}
					var jsonLines strings.Builder
					if err := (Alerts{alert}).WriteJSONL(&jsonLines); err != nil {
						t.Fatal(err)
					}
					var decoded Alert
					if err := json.Unmarshal([]byte(jsonLines.String()), &decoded); err != nil {
						t.Fatal(err)
					}
					if decoded.Frame != alert.SignalKey || decoded.Identity() != alert.Identity() {
						t.Fatal("JSONL lost the distinct coverage identity")
					}
					requireAlertOmits(t, alert, excludedService, excludedChain, "synthetic-private-command-failure")
					scope[alert.SignalKey] = alert
				case "cannot-observe":
					unknownCount++
					requireAlertOmits(t, alert, "synthetic-private-command-failure")
				case "log-shipper-fd-budget":
					allowedFaultCount++
				default:
					t.Fatalf("unexpected synthetic alert class %s", alert.Class)
				}
			}
			wantUnknown, wantAllowedFault := 0, 0
			if test.unknown {
				wantUnknown = 2
			}
			if test.allowedFault {
				wantAllowedFault = 1
			}
			if len(scope) != len(test.blockedCounts) || unknownCount != wantUnknown || allowedFaultCount != wantAllowedFault {
				t.Fatal("snapshot lost per-probe coverage or an independent source finding")
			}

			// The continuous scheduler uses these same Alert identities. A quiet
			// sibling cannot clear another probe; a persistent pause never pages.
			gate := newCadenceAlertGate()
			for tick := 0; tick < 8; tick++ {
				readyIdentities := map[string]bool{}
				for _, signal := range signals {
					alert, exists := scope[signal.Key()]
					if !exists {
						continue
					}
					ready := gate.filter(signal, Alerts{alert})
					if len(ready) != 1 || ready[0].Severity != SeverityWarn || ready[0].Identity() != alert.Identity() {
						t.Fatal("persistent partial coverage lost its stable operational warning")
					}
					readyIdentities[ready[0].Identity()] = true
				}
				if len(readyIdentities) != len(scope) {
					t.Fatal("continuous output merged independent probe coverage")
				}
				gate.filter(signals[0], nil)
			}
		})
	}
}

func TestHostScopeFrameOnlyFillsItsOwnedEmptyFrame(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	for _, test := range []struct {
		name      string
		modify    func(*finding)
		wantFrame string
	}{
		{name: "owned empty frame", modify: func(*finding) {}, wantFrame: "synthetic-probe"},
		{name: "preserve explicit frame", modify: func(f *finding) { f.frame = "existing-frame" }, wantFrame: "existing-frame"},
		{name: "other probe ID", modify: func(f *finding) { f.probeId = "synthetic/other" }},
		{name: "other class", modify: func(f *finding) { f.class = "synthetic-other-class" }},
		{name: "other target", modify: func(f *finding) { f.target = "synthetic-other-target" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := hostScopeCoverageFinding(settings, 1)
			test.modify(&observed)
			alert := alertFromFinding(settings, "1.6", "synthetic-probe", "Synthetic probe", observed)
			if alert.Frame != test.wantFrame || alert.SignalID != observed.probeId || alert.Class != observed.class || alert.Target != observed.target {
				t.Fatal("scope attribution changed an unrelated identity dimension")
			}
		})
	}
}
