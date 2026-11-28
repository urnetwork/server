package perfvar

import (
	"fmt"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// The strict six-slot proof belongs to setup, before application traffic. At
// the cold edge the SDK may warn, resize or replace clients normally. Keep that
// live observation intact and prove the experiment's ownership separately.
func sdkBrowsingRetainedSetupOwnership(initial, current sdkBrowsingSetupSnapshot, empty *sdkBrowsingSetupEmptyReceipt, providers [sdkBrowsingProviderCount]clientconnect.Id, locations *sdk.ConnectedProviderLocationList) error {
	if ready, err := sdkBrowsingSetupReady(initial, empty); !ready || err != nil {
		return fmt.Errorf("cold-link retained initial public full window: %s: %v", initial, err)
	}
	if current.Generation != initial.Generation {
		return fmt.Errorf("cold-link connection generation changed: initial=%d current=%d", initial.Generation, current.Generation)
	}
	if providers[4] != (clientconnect.Id{}) {
		return fmt.Errorf("SDK browsing late physical provider existed before its owned construction")
	}
	known := make(map[string]bool, 6)
	for _, ordinal := range []int{0, 1, 2, 3, 5, 6} {
		id := providers[ordinal]
		if id == (clientconnect.Id{}) || known[id.String()] {
			return fmt.Errorf("cold-link original physical provider cohort unavailable at ordinal %d", ordinal)
		}
		known[id.String()] = true
	}
	if locations == nil {
		return fmt.Errorf("cold-link provider ownership view unavailable")
	}
	for index := 0; index < locations.Len(); index++ {
		location := locations.Get(index)
		if location == nil || location.ClientId == nil || !known[location.ClientId.String()] {
			return fmt.Errorf("cold-link current provider row %d is outside its original physical cohort", index)
		}
	}
	return nil
}

func nativeBrowsingColdLinkStartPremise(initial, current sdkBrowsingSetupSnapshot, empty *sdkBrowsingSetupEmptyReceipt, providers [sdkBrowsingProviderCount]clientconnect.Id, locations *sdk.ConnectedProviderLocationList, lateStarted bool) error {
	if err := sdkBrowsingRetainedSetupOwnership(initial, current, empty, providers, locations); err != nil {
		return err
	}
	if lateStarted {
		return fmt.Errorf("cold-link late provider existed before its actual refusal")
	}
	return nil
}

func (self *nativeBrowsingColdLinkWitness) validateInitialCohort(current sdkBrowsingSetupSnapshot, locations *sdk.ConnectedProviderLocationList) error {
	scenario, fixture := self.scenario, self.scenario.fixture
	scenario.stateLock.Lock()
	initial, lateStarted := scenario.initial, scenario.lateStarted
	lateStarted = lateStarted || fixture.providers[4].ready.Load()
	if !lateStarted {
		// Constructor registration takes this lock before writing the field.
		lateStarted = fixture.providers[4].client != nil
	}
	scenario.stateLock.Unlock()
	var providers [sdkBrowsingProviderCount]clientconnect.Id
	for ordinal := range fixture.providers {
		provider := &fixture.providers[ordinal]
		// buildProvider assigns each old ordinal once and publishes ready last;
		// these construction-owned IDs stay immutable during reconnect. The
		// retained setup snapshot contains counts, not an ID-equality proof.
		// IsConnected is discovery policy, not physical ownership.
		if provider.ready.Load() && provider.client != nil {
			providers[ordinal] = provider.id
		}
	}
	return nativeBrowsingColdLinkStartPremise(initial, current, fixture.discovery.snapshot().InitialExcludedEmpty,
		providers, locations, lateStarted)
}

func (self *nativeBrowsingColdLinkWitness) recordColdEdgeWindow(current sdkBrowsingSetupSnapshot) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.row.ColdEdgeWindow, self.row.ColdEdgeWindowObserved = current, true
}
