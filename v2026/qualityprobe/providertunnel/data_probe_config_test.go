package providertunnel

import (
	"github.com/urnetwork/connect/v2026"
	"reflect"
	"testing"
)

func TestDataOnlyProbeSettingsAreExplicit(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		got := providerTunnelMultiClientSettingsForConfig(Config{DataOnlyProbe: enabled})
		want := connect.DefaultMultiClientSettings()
		want.ProviderProbe = false
		want.DataOnlyProviderProbe = enabled
		if reflect.ValueOf(got.SecurityPolicyGenerator).Pointer() != reflect.ValueOf(want.SecurityPolicyGenerator).Pointer() {
			t.Fatal("security generator changed")
		}
		got.SecurityPolicyGenerator = nil
		want.SecurityPolicyGenerator = nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("settings beyond explicit data mode changed: %t", enabled)
		}
	}
	if providerTunnelMultiClientSettings().DataOnlyProviderProbe {
		t.Fatal("standalone default changed")
	}
}
