// Provider construction must keep research recovery scoped to its owning plan.
package perfvar

import (
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

// The actual provider settings constructor runs before any network workload.
// An Auto-only carrier choice cannot silently select multi-packet recovery.
func TestSdkBrowsingProviderRecoveryUsesConstructorDefault(t *testing.T) {
	if clientconnect.DefaultLocalUserNatSettings().TcpBufferSettings.ReturnRecoveryMaxPacketCount != 1 {
		t.Fatal("shipping provider constructor default changed")
	}
	for _, autoH1Only := range []bool{false, true} {
		for _, override := range []int{0, -1} {
			plan := newNativeBrowsingPressurePlan()
			plan.autoH1OnlyForTest = autoH1Only
			plan.providerReturnRecoveryMaxPacketCountForTest = override
			settings := sdkBrowsingProviderNatSettings(plan)
			if count := settings.TcpBufferSettings.ReturnRecoveryMaxPacketCount; count != 1 {
				t.Errorf("SDK_BROWSING_PROVIDER_DEFAULT_RECOVERY_CHANGED: auto_h1_only=%t override=%d actual=%d want_constructor_default=1", autoH1Only, override, count)
			}
		}
	}
}

// Initial providers and a subsequently constructed late provider use the same
// plan, while every returned settings object and ordinary plan remains local.
func TestSdkBrowsingProviderRecoveryResearchOptInIsOwnerLocal(t *testing.T) {
	plan := newNativeBrowsingPressurePlan()
	plan.autoH1OnlyForTest = true
	plan.providerReturnRecoveryMaxPacketCountForTest = 4
	initial := sdkBrowsingProviderNatSettings(plan)
	if count := initial.TcpBufferSettings.ReturnRecoveryMaxPacketCount; count != 4 {
		t.Fatalf("explicit research provider recovery=%d want=4", count)
	}
	initial.TcpBufferSettings.ReturnRecoveryMaxPacketCount = 7
	late := sdkBrowsingProviderNatSettings(plan)
	ordinary := sdkBrowsingProviderNatSettings(newNativeBrowsingPressurePlan())
	if late.TcpBufferSettings == initial.TcpBufferSettings ||
		late.TcpBufferSettings.ReturnRecoveryMaxPacketCount != 4 ||
		ordinary.TcpBufferSettings.ReturnRecoveryMaxPacketCount != 1 ||
		clientconnect.DefaultLocalUserNatSettings().TcpBufferSettings.ReturnRecoveryMaxPacketCount != 1 {
		t.Fatal("research recovery escaped its plan or reused another provider's mutable settings")
	}
}
