package controller

import (
	"testing"

	"github.com/urnetwork/server/model"
)

// An activation's bootstrap holds each network to the operator's sample share
// (model.ExtenderSampleSettings): its default is the model's, a quarter, so of
// ExtenderBootstrapCount records one network holds two while others can fill
// the rest. The draw itself is model.GetRandomActiveNetworkExtendersUnderNetworkQuota's
// (TestActivationBootstrapHoldsEachNetworkToItsQuota).
func TestExtenderActivateBootstrapNetworkShareIsTheSampleShare(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	if settings.BootstrapMaxNetworkShare != model.DefaultExtenderSampleSettings().MaxNetworkShare {
		t.Fatalf("the bootstrap's network share is %v, want the sample share %v", settings.BootstrapMaxNetworkShare, model.DefaultExtenderSampleSettings().MaxNetworkShare)
	}
	if quota := model.ExtenderSampleNetworkQuota(ExtenderBootstrapCount, settings.BootstrapMaxNetworkShare); quota != 2 {
		t.Fatalf("one network's quota of a bootstrap of %d is %d, want 2", ExtenderBootstrapCount, quota)
	}
}
