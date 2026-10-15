package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// Five desired hosts with five blocks each; the last host is explicitly offline.
// Rows use private identities so the fixtures exercise the actual reducers, not
// just the aggregate paired-process count shown in alerts.
func providerInventoryTestSettings() SignalSettings {
	settings := syntheticSettings(&syntheticSource{})
	settings.Environment, settings.Now = "synthetic", pickerTestNow
	settings.Hosts = nil
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {}}
	settings.LogServiceBlocks = map[string][]string{"api": {"beta", "g1", "g2", "g3", "g4"}}
	for i := range 5 {
		h := HostSettings{Name: fmt.Sprintf("private-api-%d", i), Roles: []string{"services"}}
		settings.LogServiceHosts["api"] = append(settings.LogServiceHosts["api"], h.Name)
		if i == 4 {
			settings.disabledHosts = []HostSettings{h}
		} else {
			settings.Hosts = append(settings.Hosts, h)
		}
	}
	return settings
}

func providerInventoryTestSlots(hosts int) []pickerProcessKey {
	slots := []pickerProcessKey{}
	for i := range hosts {
		for _, block := range []string{"beta", "g1", "g2", "g3", "g4"} {
			slots = append(slots, pickerProcessKey{host: fmt.Sprintf("private-api-%d", i), block: block, instance: "private-generation-1"})
		}
	}
	return slots
}

func providerInventoryTestRows(selection, failure bool, slots []pickerProcessKey) []map[string]any {
	rows := []map[string]any{}
	for _, slot := range slots {
		var next []map[string]any
		if selection {
			reason := "returned"
			if failure {
				reason = "eligible_not_selected"
			}
			next = selectionTestRows(reason, 3)
		} else {
			outcome := "initial/nonempty"
			if failure {
				outcome = "initial/empty"
			}
			next = pickerTestRows(map[string]float64{outcome: 2})
		}
		for _, row := range next {
			labels := row["metric"].(map[string]string)
			labels["host"], labels["block"], labels["instance"] = slot.host, slot.block, slot.instance
		}
		rows = append(rows, next...)
	}
	return rows
}

func providerInventoryTestEnv(t testing.TB, settings SignalSettings) *probeEnv {
	t.Helper()
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func requireProviderInventoryCoverage(t testing.TB, settings SignalSettings, slots []pickerProcessKey, expected, paired int, complete bool) {
	t.Helper()
	scope := pickerScope(providerInventoryTestEnv(t, settings))
	picker := parseProviderPicker(pickerTestPayload(t, providerInventoryTestRows(false, false, slots)), "synthetic", pickerTestNow(), scope)
	selection := parseProviderSelection(pickerTestPayload(t, providerInventoryTestRows(true, false, slots)), "synthetic", pickerTestNow(), scope)
	if picker.expected != expected || picker.paired != paired || picker.complete != complete || picker.reason == "invalid-source-response" {
		t.Fatalf("picker expected=%d paired=%d complete=%t reason=%s; want %d/%d/%t", picker.expected, picker.paired, picker.complete, picker.reason, expected, paired, complete)
	}
	if selection.expected != expected || selection.paired != paired || selection.complete != complete || selection.shapeComplete != complete || selection.reason == "invalid-source-response" {
		t.Fatalf("selection expected=%d paired=%d complete=%t shape_complete=%t reason=%s; want %d/%d/%t", selection.expected, selection.paired, selection.complete, selection.shapeComplete, selection.reason, expected, paired, complete)
	}
	if pickerFindingHealthy(providerPickerFindings(picker), "provider-picker-unobservable") != complete {
		t.Fatal("picker visibility recovery did not require complete enabled identity coverage and traffic")
	}
	if findings := providerSelectionFindings(selection); (len(findings) == 0) != complete {
		t.Fatal("selection visibility did not retain incomplete enabled identity coverage")
	}
	if !complete {
		for _, f := range providerPickerFindings(picker) {
			if f.healthy {
				t.Fatal("incomplete enabled identity coverage cleared a picker class")
			}
		}
	}
}

func TestProviderInventoryDisabledHostIsOutsideExpectedCohort(t *testing.T) {
	settings := providerInventoryTestSettings()
	env := providerInventoryTestEnv(t, settings)
	scope := pickerScope(env)
	if len(scope.slots) != 20 || len(scope.hosts) != 4 || len(env.cfg.logServiceHosts["api"]) != 5 || len(settings.LogServiceHosts["api"]) != 5 {
		t.Fatal("enabled scope did not omit exactly the five disabled slots while retaining global placement")
	}
	for _, slot := range providerInventoryTestSlots(5)[20:] {
		if _, exists := scope.slots[slot.host+"\x00"+slot.block]; exists {
			t.Fatal("disabled host remained in expected slot identities")
		}
	}
	for _, query := range []string{providerPickerQuery("synthetic", scope), providerSelectionQuery("synthetic", scope)} {
		if strings.Contains(query, "private-api-4") || !strings.Contains(query, `host=~"private-api-0|private-api-1|private-api-2|private-api-3"`) {
			t.Fatal("source selector lost enabled identity scope or contacted the disabled host")
		}
	}
	requireProviderInventoryCoverage(t, settings, providerInventoryTestSlots(4), 20, 20, true)
}

func TestProviderInventoryReenableRestoresEveryBlockRequirement(t *testing.T) {
	settings := providerInventoryTestSettings()
	settings.Hosts = append(settings.Hosts, settings.disabledHosts...)
	settings.disabledHosts = nil
	// Existing twenty windows cannot establish coverage of a newly enabled host.
	requireProviderInventoryCoverage(t, settings, providerInventoryTestSlots(4), 25, 20, false)
	requireProviderInventoryCoverage(t, settings, providerInventoryTestSlots(5), 25, 25, true)
}

func TestProviderInventoryMissingEnabledIdentityCannotBeReplaced(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplicate=%t", duplicate), func(t *testing.T) {
			slots := providerInventoryTestSlots(4)[:19]
			if duplicate {
				extra := slots[0]
				extra.instance = "private-generation-2"
				slots = append(slots, extra)
			}
			// With duplicate=true, paired equals expected (20), but one enabled
			// host/block is missing and another has two complete generations.
			requireProviderInventoryCoverage(t, providerInventoryTestSettings(), slots, 20, len(slots), false)
		})
	}
}

func TestProviderInventoryUnobservableEnabledHostsStayRequired(t *testing.T) {
	for _, condition := range []string{"temporary-exclusion", "unenrolled", "contradictory-inventory"} {
		t.Run(condition, func(t *testing.T) {
			settings := providerInventoryTestSettings()
			switch condition {
			case "temporary-exclusion":
				settings.ExcludedHosts = []string{"private-api-3"}
			case "unenrolled":
				settings.Hosts = settings.Hosts[:3]
			case "contradictory-inventory":
				settings.disabledHosts = append(settings.disabledHosts, settings.Hosts[3])
			}
			scope := pickerScope(providerInventoryTestEnv(t, settings))
			if len(scope.hosts) != 3 || len(scope.slots) != 20 {
				t.Fatal("unobservable enabled host silently reduced the expected denominator")
			}
			requireProviderInventoryCoverage(t, settings, providerInventoryTestSlots(3), 20, 15, false)
		})
	}
}

func TestProviderInventoryOutOfScopeRowsCannotReplaceMissingEnabledSlot(t *testing.T) {
	settings := providerInventoryTestSettings()
	scope := pickerScope(providerInventoryTestEnv(t, settings))
	slots := append(providerInventoryTestSlots(4)[:19], providerInventoryTestSlots(5)[20])
	picker := parseProviderPicker(pickerTestPayload(t, providerInventoryTestRows(false, false, slots)), "synthetic", pickerTestNow(), scope)
	selection := parseProviderSelection(pickerTestPayload(t, providerInventoryTestRows(true, false, slots)), "synthetic", pickerTestNow(), scope)
	if picker.complete || picker.reason != "invalid-source-response" || selection.complete || selection.reason != "invalid-source-response" {
		t.Fatal("disabled source identity was accepted in place of a missing enabled slot")
	}
}

func TestProviderInventoryPartialCoverageRetainsObservedFailures(t *testing.T) {
	scope := pickerScope(providerInventoryTestEnv(t, providerInventoryTestSettings()))
	slots := providerInventoryTestSlots(4)[:19]
	picker := parseProviderPicker(pickerTestPayload(t, providerInventoryTestRows(false, true, slots)), "synthetic", pickerTestNow(), scope)
	selection := parseProviderSelection(pickerTestPayload(t, providerInventoryTestRows(true, true, slots)), "synthetic", pickerTestNow(), scope)
	for _, control := range []struct {
		findings []finding
		failure  string
		unknown  string
	}{
		{providerPickerFindings(picker), "provider-picker-effective-empty", "provider-picker-unobservable"},
		{providerSelectionFindings(selection), "provider-selection-empty-despite-eligible", "provider-selection-unavailable"},
	} {
		page, unknown := false, false
		for _, f := range control.findings {
			page = page || (f.class == control.failure && f.tier == tierPage && !f.healthy)
			unknown = unknown || (f.class == control.unknown && !f.healthy)
		}
		if !page || !unknown {
			t.Fatal("missing enabled slot erased a known failing subset or certified coverage")
		}
	}
}

func TestProviderInventorySignalAdaptersUseEnabledCohort(t *testing.T) {
	for _, selection := range []bool{false, true} {
		t.Run(fmt.Sprintf("selection=%t", selection), func(t *testing.T) {
			settings := providerInventoryTestSettings()
			raw := pickerTestPayload(t, providerInventoryTestRows(selection, false, providerInventoryTestSlots(4)))
			var calls atomic.Int32
			settings.Source = &syntheticSource{hostFn: func(h HostSettings, command string) (string, error) {
				calls.Add(1)
				if h.Name == "private-api-4" || strings.Contains(command, "private-api-4") || !strings.Contains(command, "--max-time 15 --max-filesize 4194304") {
					return "", fmt.Errorf("unexpected source boundary")
				}
				return raw, nil
			}}
			signal := NewProviderPickerSignal()
			if selection {
				signal = NewProviderSelectionSignal()
			}
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 || calls.Load() != 1 {
				t.Fatalf("enabled cohort failed adapter: alerts=%d calls=%d err=%v", len(alerts), calls.Load(), err)
			}
		})
	}
}

func TestProviderInventoryEmptyEnabledCohortStaysUnavailable(t *testing.T) {
	settings := providerInventoryTestSettings()
	settings.disabledHosts = append(settings.disabledHosts, settings.Hosts...)
	settings.Hosts = nil
	var calls atomic.Int32
	settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("empty cohort must not query a source")
	}}
	env := providerInventoryTestEnv(t, settings)
	if scope := pickerScope(env); len(scope.slots) != 0 || len(scope.hosts) != 0 {
		t.Fatal("all-disabled inventory retained an expected or contactable host")
	}
	picker, pickerErr := (providerPickerProbe{}).check(context.Background(), env)
	selection, selectionErr := (providerSelectionProbe{}).check(context.Background(), env)
	if pickerErr != nil || selectionErr != nil || calls.Load() != 0 || len(picker) != 1 || len(selection) != 1 {
		t.Fatal("empty enabled cohort contacted a source or omitted its visibility finding")
	}
	if picker[0].healthy || picker[0].class != "provider-picker-unobservable" || selection[0].healthy || selection[0].class != "provider-selection-unavailable" {
		t.Fatal("empty enabled cohort became healthy zero")
	}
}
