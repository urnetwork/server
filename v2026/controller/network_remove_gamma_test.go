package controller

import (
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// A gamma charge in flight keeps the network and asks the customer to retry
// in a few minutes, once the biller has credited it; the account is not
// removed, and no product-update removal is scheduled for it.
func TestNetworkRemoveGammaChargeInFlightAsksForRetry(t *testing.T) {
	fake := &networkRemoveStepsFake{removeOutcome: model.RemoveNetworkOutcome("gamma_charge_in_flight")}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil {
		t.Fatalf("remove err = %v, want a result error", err)
	}
	if fake.removed {
		t.Fatalf("network removed while a gamma charge is in flight (steps %v)", fake.calls)
	}
	if result == nil || result.Error == nil || !strings.Contains(result.Error.Message, "Gamma payment") || !strings.Contains(result.Error.Message, "try again") {
		t.Fatalf("remove result = %+v, want the Gamma retry message", result)
	}
	if want := []string{"snapshot", "apple", "stripe", "play", "remove"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("steps = %v, want %v (no product-update removal)", fake.calls, want)
	}
}
