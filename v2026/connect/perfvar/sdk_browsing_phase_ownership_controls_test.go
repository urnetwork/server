package perfvar

import (
	"context"
	"errors"
	"sync"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

func sdkBrowsingAdaptivePhaseControl(t *testing.T, phase sdkBrowsingPremisePhase) {
	t.Helper()
	providers, exits, status, locations := sdkBrowsingSetupControlViews(t)
	initial := sdkBrowsingSetupSnapshotFrom(exits, status, locations, providers)
	current := initial
	current.Warnings, current.Target, current.InEvaluation = 2, 9, 1
	registered := phase == sdkBrowsingPremiseAfterRefusal
	if err := sdkBrowsingAdmissionPhasePremise(initial, current, nil, providers, locations, phase, registered, false); err != nil {
		t.Fatalf("SDK_ADMISSION_ADAPTIVE_PHASE_REJECTED phase=%d: %v", phase, err)
	}
}

func TestSdkBrowsingAdmissionCssAllowsAdaptiveWindow(t *testing.T) {
	sdkBrowsingAdaptivePhaseControl(t, sdkBrowsingPremiseCss)
}

func TestSdkBrowsingAdmissionTapAllowsAdaptiveWindow(t *testing.T) {
	sdkBrowsingAdaptivePhaseControl(t, sdkBrowsingPremiseTap)
}

func TestSdkBrowsingAdmissionRegisteredRefusalAllowsAdaptiveWindow(t *testing.T) {
	sdkBrowsingAdaptivePhaseControl(t, sdkBrowsingPremiseAfterRefusal)
}

func TestSdkBrowsingAdmissionRejectsWrongPhaseOrLostOwnership(t *testing.T) {
	for _, phase := range []sdkBrowsingPremisePhase{sdkBrowsingPremiseCss, sdkBrowsingPremiseTap, sdkBrowsingPremiseAfterRefusal} {
		for _, kind := range []string{"registration", "physical-late", "published-late", "bad-startup", "generation", "missing-original", "foreign", "unknown", "unavailable"} {
			providers, exits, status, locations := sdkBrowsingSetupControlViews(t)
			initial := sdkBrowsingSetupSnapshotFrom(exits, status, locations, providers)
			current, registered, physicalLate := initial, phase == sdkBrowsingPremiseAfterRefusal, false
			switch kind {
			case "registration":
				registered = !registered
			case "physical-late":
				physicalLate = true
			case "published-late":
				providers[4] = clientconnect.NewId()
			case "bad-startup":
				initial.Warnings = 1
			case "generation":
				current.Generation++
			case "missing-original":
				providers[0] = clientconnect.Id{}
			case "foreign":
				locations.Get(0).ClientId = sdk.NewId()
			case "unknown":
				locations.Get(0).ClientId = nil
			case "unavailable":
				locations = nil
			}
			if err := sdkBrowsingAdmissionPhasePremise(initial, current, nil, providers, locations, phase, registered, physicalLate); err == nil {
				t.Fatalf("phase=%d accepted lost authority/cohort: %s", phase, kind)
			}
		}
	}
	if err := sdkBrowsingPhaseRegistrationError(0, false); err == nil {
		t.Fatal("unknown phase acquired constructor authority")
	}
}

func TestSdkBrowsingAdmissionRegisteredWriterExcludesUnsafeClientRead(t *testing.T) {
	scenario := newSdkBrowsingScenario(nil)
	scenario.fixture = &sdkBrowsingSetupFixture{}
	registered, begin, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var beginOnce sync.Once
	release := func() { beginOnce.Do(func() { close(begin) }) }
	t.Cleanup(func() { release(); <-done })
	go func() {
		defer close(done)
		scenario.stateLock.Lock()
		scenario.lateStarted = true
		scenario.stateLock.Unlock()
		close(registered)
		<-begin
		// Publication has not happened yet. CSS/tap must reject registration
		// without reading this constructor-owned, non-atomic field.
		for range 100 {
			scenario.fixture.providers[4].client = new(clientconnect.Client)
		}
	}()
	<-registered
	release()
	for range 100 {
		if _, _, _, err := scenario.phaseOwnershipState(sdkBrowsingPremiseCss); err == nil {
			t.Fatal("CSS observed an already-registered constructor as absent")
		}
		if _, _, _, err := scenario.phaseOwnershipState(sdkBrowsingPremiseTap); err == nil {
			t.Fatal("tap observed an already-registered constructor as absent")
		}
	}
	<-done
	// After-refusal belongs to that sole constructor; before writing, it must
	// reject an already-present physical provider rather than reject its own
	// expected registration.
	_, owner, physical, err := scenario.phaseOwnershipState(sdkBrowsingPremiseAfterRefusal)
	if err != nil || !owner || !physical {
		t.Fatalf("registered construction phase lost its physical-presence evidence: owner=%t physical=%t err=%v", owner, physical, err)
	}
}

func TestSdkBrowsingAdmissionCanceledRefusalOwnerStopsBeforeSdkReads(t *testing.T) {
	scenario := newSdkBrowsingScenario(nil)
	scenario.lateStarted = true
	close(scenario.firstRefusal)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// No fixture/device is installed. A canceled owner must return before any
	// SDK snapshot or provider construction, even when refusal is also ready.
	scenario.buildLate(ctx)
	select {
	case <-scenario.lateDone:
	default:
		t.Fatal("canceled registered constructor did not close its join edge")
	}
	if !errors.Is(scenario.firstErr, context.Canceled) {
		t.Fatalf("canceled owner was replaced by a later SDK/panic failure: %v", scenario.firstErr)
	}
}
