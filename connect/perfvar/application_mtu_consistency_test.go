// Ordinary measurements use the same 1280-byte packet boundary as devices.
// Modeled physical outer MTUs and explicit boundary witnesses stay independent.
package perfvar

import (
	"fmt"
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

// All registered workloads inherit the device packet boundary, including
// low-rate and focused outer-path profiles.
func TestRegisteredMeasurementProfilesUseDevicePacketMtu(t *testing.T) {
	if clientconnect.DefaultMtu != 1280 {
		t.Fatalf("device packet MTU=%d want1280", clientconnect.DefaultMtu)
	}
	for name, profile := range allNetworkProfiles(20261109) {
		if profile.InnerMtu != clientconnect.DefaultMtu {
			t.Errorf("profile %s application MTU=%d want=%d", name, profile.InnerMtu, clientconnect.DefaultMtu)
		}
	}
}

// Capture the real direct-path constructors. The old profile used 1440 here
// while the SDK application helper resolved 1280, invalidating MTU parity.
func TestDirectTunAndSdkApplicationMtuAgree(t *testing.T) {
	profile := initialNetworkProfiles(20261109)["clean-lan"]
	resources := defaultTunResourceProfile()
	observed := map[bool]int{}
	path, err := newTunPathWithSettings(t.Context(), profile, resources, func(left bool, settings *clientconnect.TunSettings) {
		observed[left] = settings.Mtu
	})
	if err != nil {
		t.Fatal(err)
	}
	defer path.close()
	for _, left := range []bool{true, false} {
		actual, found := observed[left]
		if !found || actual != 1280 || actual != resolvedFullTunApplicationMtu(profile, resources) {
			t.Errorf("direct endpoint left=%t found=%t MTU=%d SDK=%d want1280", left, found, actual, resolvedFullTunApplicationMtu(profile, resources))
		}
	}
}

// An outer-path MTU change keeps the application boundary fixed while
// preserving distinct physical link sizes for fragmentation and PMTU controls.
func TestOuterMtuChangesDoNotResizeApplicationPacketMtu(t *testing.T) {
	profiles := allNetworkProfiles(20261109)
	for _, outer := range []int{1280, 1400, 1500} {
		name := fmt.Sprintf("mtu-%d", outer)
		profile := profiles[name]
		if profile.InnerMtu != 1280 || profile.Forward.OuterMtu != outer || profile.Reverse.OuterMtu != outer {
			t.Errorf("profile %s lost independent application/physical MTU: %+v", name, profile)
		}
	}
}
