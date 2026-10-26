package perfvar

import (
	"reflect"
	"testing"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

func TestNativeBrowsingColdLinkRetainsInitialProofUnderActualAdaptiveWindow(t *testing.T) {
	providers, exits, status, locations := sdkBrowsingSetupControlViews(t)
	initial := sdkBrowsingSetupSnapshotFrom(exits, status, locations, providers)
	current := initial
	current.Warnings, current.Target, current.InEvaluation = 2, 9, 1
	before := current
	if err := nativeBrowsingColdLinkStartPremise(initial, current, nil, providers, locations, false); err != nil {
		t.Fatal("COLD_LINK_DYNAMIC_WINDOW_REJECTED: retained strict setup and current owned cohort were rejected after page0: ", err)
	}
	if !reflect.DeepEqual(before, current) {
		t.Fatal("cold-edge validation normalized the actual warning/target observation")
	}
}

func TestNativeBrowsingColdLinkAllowsOwnedWindowAdaptation(t *testing.T) {
	for _, change := range []func(*sdkBrowsingSetupSnapshot){
		func(row *sdkBrowsingSetupSnapshot) { row.Warnings = 6; row.Target = 12; row.InEvaluation = 4 },
		func(row *sdkBrowsingSetupSnapshot) {
			row.Quality = 3
			row.ExitRows = 4
			row.DistinctLocal = 4
			row.Target = 7
		},
		func(row *sdkBrowsingSetupSnapshot) {
			row.Quality = 7
			row.ExitRows = 8
			row.DistinctLocal = 8
			row.Target = 9
		},
		func(row *sdkBrowsingSetupSnapshot) { row.Done = 1; row.Warnings = 1 },
	} {
		providers, exits, status, locations := sdkBrowsingSetupControlViews(t)
		initial := sdkBrowsingSetupSnapshotFrom(exits, status, locations, providers)
		current := initial
		change(&current)
		before := current
		if err := nativeBrowsingColdLinkStartPremise(initial, current, nil, providers, locations, false); err != nil {
			t.Fatal("ordinary owned window projection became a second setup gate: ", err)
		}
		if !reflect.DeepEqual(before, current) {
			t.Fatal("current projection was rewritten")
		}
	}
}

func TestNativeBrowsingColdLinkRejectsInvalidRetainedSetup(t *testing.T) {
	for _, change := range []func(*sdkBrowsingSetupSnapshot){
		func(row *sdkBrowsingSetupSnapshot) { row.Warnings = 1 },
		func(row *sdkBrowsingSetupSnapshot) { row.Target = 9 },
		func(row *sdkBrowsingSetupSnapshot) { row.Done = 1 },
		func(row *sdkBrowsingSetupSnapshot) { row.ForeignProviders = 1 },
		func(row *sdkBrowsingSetupSnapshot) { row.DistinctLocal = 5 },
		func(row *sdkBrowsingSetupSnapshot) { row.Generation = 0 },
	} {
		providers, exits, status, locations := sdkBrowsingSetupControlViews(t)
		initial := sdkBrowsingSetupSnapshotFrom(exits, status, locations, providers)
		current := initial
		change(&initial)
		if err := nativeBrowsingColdLinkStartPremise(initial, current, nil, providers, locations, false); err == nil {
			t.Fatal("cold edge accepted a fixture that never established strict startup")
		}
	}
}

func TestNativeBrowsingColdLinkRejectsChangedContextOrPhysicalCohort(t *testing.T) {
	for _, kind := range []string{"generation", "missing-original", "duplicate-original", "late-id", "late-started", "foreign", "unknown", "unavailable"} {
		providers, exits, status, locations := sdkBrowsingSetupControlViews(t)
		initial := sdkBrowsingSetupSnapshotFrom(exits, status, locations, providers)
		current, lateStarted := initial, false
		switch kind {
		case "generation":
			current.Generation++
		case "missing-original":
			providers[0] = clientconnect.Id{}
		case "duplicate-original":
			providers[1] = providers[0]
		case "late-id":
			providers[4] = clientconnect.NewId()
		case "late-started":
			lateStarted = true
		case "foreign":
			locations.Get(0).ClientId = sdk.NewId()
		case "unknown":
			locations.Get(0).ClientId = nil
		case "unavailable":
			locations = nil
		}
		if err := nativeBrowsingColdLinkStartPremise(initial, current, nil, providers, locations, lateStarted); err == nil {
			t.Fatalf("cold edge accepted lost ownership: %s", kind)
		}
	}
}

func TestNativeBrowsingColdLinkRetainsStartupAndAdaptiveWindowReport(t *testing.T) {
	providers, exits, status, locations := sdkBrowsingSetupControlViews(t)
	initial := sdkBrowsingSetupSnapshotFrom(exits, status, locations, providers)
	scenario := newSdkBrowsingScenario(nil)
	scenario.initial = initial
	witness := newNativeBrowsingColdLinkWitness(scenario, &nativeBrowsingPressureObservation{}, true)
	defer witness.close()
	current := initial
	current.Warnings, current.Target, current.InEvaluation = 2, 9, 1
	witness.recordColdEdgeWindow(current)
	row, err := witness.snapshot()
	if err != nil || row.InitialWindow != initial || row.ColdEdgeWindow != current || !row.ColdEdgeWindowObserved {
		t.Fatalf("cold report lost or normalized its actual startup/under-load window: row=%+v err=%v", row, err)
	}
}
