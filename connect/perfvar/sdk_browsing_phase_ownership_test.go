package perfvar

import (
	"fmt"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

type sdkBrowsingPremisePhase uint8

const (
	sdkBrowsingPremiseCss sdkBrowsingPremisePhase = iota + 1
	sdkBrowsingPremiseTap
	sdkBrowsingPremiseAfterRefusal
)

func sdkBrowsingPhaseRegistrationError(phase sdkBrowsingPremisePhase, registered bool) error {
	if phase != sdkBrowsingPremiseCss && phase != sdkBrowsingPremiseTap && phase != sdkBrowsingPremiseAfterRefusal {
		return fmt.Errorf("unknown SDK browsing ownership phase %d", phase)
	}
	if registered != (phase == sdkBrowsingPremiseAfterRefusal) {
		return fmt.Errorf("SDK browsing phase %d has unexpected late constructor registration %t", phase, registered)
	}
	return nil
}

func sdkBrowsingAdmissionPhasePremise(initial, current sdkBrowsingSetupSnapshot, empty *sdkBrowsingSetupEmptyReceipt, providers [6]clientconnect.Id, locations *sdk.ConnectedProviderLocationList, phase sdkBrowsingPremisePhase, registered, physicalLate bool) error {
	if err := sdkBrowsingPhaseRegistrationError(phase, registered); err != nil {
		return err
	}
	if physicalLate {
		return fmt.Errorf("SDK browsing late provider existed before its registered construction")
	}
	return sdkBrowsingRetainedSetupOwnership(initial, current, empty, providers, locations)
}

// CSS/tap read physical absence while registration is excluded by this lock.
// After-refusal is called only by the sole registered constructor, before its
// own buildProvider(4); registration is expected in that phase.
func (self *sdkBrowsingScenario) phaseOwnershipState(phase sdkBrowsingPremisePhase) (sdkBrowsingSetupSnapshot, bool, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	registered := self.lateStarted
	if err := sdkBrowsingPhaseRegistrationError(phase, registered); err != nil {
		// Do not read a concurrently constructing provider's non-atomic fields.
		return self.initial, registered, false, err
	}
	late := &self.fixture.providers[4]
	return self.initial, registered, late.ready.Load() || late.client != nil, nil
}

func (self *sdkBrowsingScenario) validateOwnedPhase(current sdkBrowsingSetupSnapshot, locations *sdk.ConnectedProviderLocationList, phase sdkBrowsingPremisePhase) error {
	initial, registered, physicalLate, err := self.phaseOwnershipState(phase)
	if err != nil {
		return err
	}
	var providers [6]clientconnect.Id
	for ordinal := range self.fixture.providers {
		provider := &self.fixture.providers[ordinal]
		if provider.ready.Load() && provider.client != nil {
			providers[ordinal] = provider.id
		}
	}
	return sdkBrowsingAdmissionPhasePremise(initial, current, self.fixture.discovery.snapshot().InitialExcludedEmpty,
		providers, locations, phase, registered, physicalLate)
}

// Preserve the existing order and number of independent public SDK pulls;
// reuse their location view for physical-cohort validation.
func (self *sdkBrowsingScenario) snapshotForOwnership() (sdkBrowsingSetupSnapshot, *sdk.ConnectedProviderLocationList) {
	device := self.fixture.device
	exits, status := device.GetExits(), device.GetWindowStatus()
	locations := device.GetConnectedProviderLocations()
	return sdkBrowsingSetupSnapshotFrom(exits, status, locations, self.fixture.readyProviderIds()), locations
}
