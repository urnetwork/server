package localclient

import (
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// A hosted device's windows ask with ipv4 and any (FP2-PROTOCOL.md D14), and
// hosted discovery hands the request to the model through JSON (convert), so
// both selectors and every count reach FindProviders2 as sent, and the echo
// comes back to the window as the model wrote it.
func TestFindProviders2ConvertCarriesTheHostedSelectors(t *testing.T) {
	request, err := convert[model.FindProviders2Args](&connect.FindProviders2Args{
		RankMode: "quality",
		Select: &connect.FindProvidersSelect{
			Ip:                   connect.ProviderIpCapabilityIpv4,
			Protocol:             connect.ProviderProtocolCapabilityAny,
			TargetCount:          5,
			HeldCount:            3,
			DualstackTargetCount: 0,
			HeldDualstackCount:   1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	providerSelect := request.Select
	if providerSelect == nil || providerSelect.Ip == nil || *providerSelect.Ip != model.ProviderCapabilityIpIpv4 ||
		providerSelect.Protocol == nil || *providerSelect.Protocol != model.ProviderCapabilityProtocolAny ||
		providerSelect.TargetCount == nil || *providerSelect.TargetCount != 5 ||
		providerSelect.HeldCount == nil || *providerSelect.HeldCount != 3 ||
		providerSelect.DualstackTargetCount == nil || *providerSelect.DualstackTargetCount != 0 ||
		providerSelect.HeldDualstackCount == nil || *providerSelect.HeldDualstackCount != 1 {
		t.Fatalf("converted select %+v, want ipv4 and any with every count", providerSelect)
	}

	ip, protocol := model.ProviderCapabilityIpIpv4, model.ProviderCapabilityProtocolAny
	result, err := convert[connect.FindProviders2Result](&model.FindProviders2Result{
		Select: &model.FindProviders2Select{Ip: &ip, Protocol: &protocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Select == nil || result.Select.Ip != connect.ProviderIpCapabilityIpv4 || result.Select.Protocol != connect.ProviderProtocolCapabilityAny {
		t.Fatalf("converted echo %+v, want ipv4 and any", result.Select)
	}
}
