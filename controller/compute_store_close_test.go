package controller

// Compute's store subscriptions at an account's deletion (BAL-M12-14; DESIGN-COMPUTE §9.12), hermetic: the App Store
// check reads compute's subscriptions alone and fails closed; the removal checks compute's App Store subscriptions
// beside Pro's before anything is cancelled, cancels compute's Play subscription after Pro's, and hands the closure the
// read it checked, asking for a retry where the closure finds a subscription the read did not name.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"

	"github.com/urnetwork/server/session"
)

// One subscription's last transaction as the App Store's status API answers it: its status, its product, and its
// renewal info's autoRenewStatus, nil for no renewal info.
func computeAppleTestLast(t testing.TB, status int, productId string, autoRenewStatus *int) *appleLastTransaction {
	last := &appleLastTransaction{
		Status:                status,
		OriginalTransactionId: "synthetic-original",
		SignedTransactionInfo: appleTestJws(t, map[string]any{"productId": productId, "originalTransactionId": "synthetic-original"}),
	}
	if autoRenewStatus != nil {
		last.SignedRenewalInfo = appleTestJws(t, map[string]any{"autoRenewStatus": *autoRenewStatus, "autoRenewProductId": productId})
	}
	return last
}

// An answer of one subscription group a transaction.
func computeAppleTestStatuses(lasts ...*appleLastTransaction) *appleSubscriptionStatusesResponse {
	statuses := &appleSubscriptionStatusesResponse{}
	for _, last := range lasts {
		statuses.Data = append(statuses.Data, &appleSubscriptionGroup{SubscriptionGroupIdentifier: "synthetic-group", LastTransactions: []*appleLastTransaction{last}})
	}
	return statuses
}

// A compute subscription renews while it is active, in billing retry or in grace with its auto-renew not off; an
// expired or revoked one, or one turned off, renews nothing; Pro's subscriptions in the answer are passed over; an
// answer with no compute subscription, or one that cannot be read, counts as renewing.
func TestComputeAppleStatusesRenewingReadsComputeSubscriptionsAlone(t *testing.T) {
	on, off := 1, 0
	compute := sidecar.ComputeStoreProductId(true, 25)
	malformed := computeAppleTestLast(t, 1, compute, &on)
	malformed.SignedTransactionInfo = "not a jws"
	for name, c := range map[string]struct {
		statuses *appleSubscriptionStatusesResponse
		renewing bool
		err      bool
	}{
		"active":                  {computeAppleTestStatuses(computeAppleTestLast(t, 1, compute, &on)), true, false},
		"active, turned off":      {computeAppleTestStatuses(computeAppleTestLast(t, 1, compute, &off)), false, false},
		"active, no renewal info": {computeAppleTestStatuses(computeAppleTestLast(t, 1, compute, nil)), true, false},
		"billing retry":           {computeAppleTestStatuses(computeAppleTestLast(t, 3, compute, &on)), true, false},
		"grace":                   {computeAppleTestStatuses(computeAppleTestLast(t, 4, compute, &on)), true, false},
		"expired":                 {computeAppleTestStatuses(computeAppleTestLast(t, 2, compute, &on)), false, false},
		"revoked":                 {computeAppleTestStatuses(computeAppleTestLast(t, 5, compute, &on)), false, false},
		"Pro renews beside an expired compute plan": {
			computeAppleTestStatuses(computeAppleTestLast(t, 1, "supporter", &on), computeAppleTestLast(t, 2, compute, &on)), false, false,
		},
		"Pro alone":             {computeAppleTestStatuses(computeAppleTestLast(t, 1, "supporter", &on)), true, true},
		"a transaction unread":  {computeAppleTestStatuses(malformed), true, true},
		"a status not Apple's":  {computeAppleTestStatuses(computeAppleTestLast(t, 9, compute, &on)), true, true},
		"no answer":             {nil, true, true},
		"an answer of no group": {&appleSubscriptionStatusesResponse{}, true, true},
	} {
		renewing, err := computeAppleStatusesRenewing(c.statuses)
		if renewing != c.renewing || (err != nil) != c.err {
			t.Errorf("%s: renewing=%v err=%v, want renewing=%v err=%v", name, renewing, err, c.renewing, c.err)
		}
	}
}

// Each transaction is asked about once; one renewing subscription, a failed lookup, an empty id or no lookup counts as
// renewing.
func TestComputeAppleSubscriptionsRenewingFailsClosed(t *testing.T) {
	on, off := 1, 0
	compute := sidecar.ComputeStoreProductId(true, 25)
	asked := []string{}
	lookup := func(ctx context.Context, transactionId string) (*appleSubscriptionStatusesResponse, error) {
		asked = append(asked, transactionId)
		switch transactionId {
		case "on":
			return computeAppleTestStatuses(computeAppleTestLast(t, 1, compute, &on)), nil
		case "off":
			return computeAppleTestStatuses(computeAppleTestLast(t, 1, compute, &off)), nil
		}
		return nil, errors.New("synthetic lookup failure")
	}
	for _, c := range []struct {
		ids      []string
		renewing bool
		err      bool
	}{
		{ids: nil, renewing: false},
		{ids: []string{"off", "off"}, renewing: false},
		{ids: []string{"off", "on"}, renewing: true},
		{ids: []string{"failing"}, renewing: true, err: true},
		{ids: []string{""}, renewing: true, err: true},
	} {
		asked = asked[:0]
		renewing, err := computeAppleSubscriptionsRenewing(context.Background(), c.ids, lookup)
		if renewing != c.renewing || (err != nil) != c.err {
			t.Errorf("ids %v: renewing=%v err=%v, want renewing=%v err=%v", c.ids, renewing, err, c.renewing, c.err)
		}
		if slices.Equal(c.ids, []string{"off", "off"}) && len(asked) != 1 {
			t.Errorf("a transaction named twice was asked about %d times", len(asked))
		}
	}
	if renewing, err := computeAppleSubscriptionsRenewing(context.Background(), []string{"on"}, nil); !renewing || err == nil {
		t.Errorf("no lookup: renewing=%v err=%v", renewing, err)
	}
}

// The removal's steps with compute's added: a fixed read, a fixed App Store answer and a fixed closure, each recorded.
type networkRemoveComputeFake struct {
	*networkRemoveStepsFake
	stores        *sidecarcontroller.ComputeStoreSubscriptions
	storesErr     error
	computeApple  bool
	closeErr      error
	appleAsked    []string
	playCancelled [][]string
	handed        *sidecarcontroller.ComputeStoreSubscriptions
}

func newNetworkRemoveComputeFake(stores *sidecarcontroller.ComputeStoreSubscriptions) *networkRemoveComputeFake {
	return &networkRemoveComputeFake{networkRemoveStepsFake: &networkRemoveStepsFake{}, stores: stores}
}

func (self *networkRemoveComputeFake) steps() *networkRemoveSteps {
	steps := self.networkRemoveStepsFake.steps()
	cancelPlay := steps.cancelPlay
	steps.cancelPlay = func(clientSession *session.ClientSession, purchaseTokens []string) error {
		self.playCancelled = append(self.playCancelled, purchaseTokens)
		return cancelPlay(clientSession, purchaseTokens)
	}
	steps.computeStores = func(clientSession *session.ClientSession) (*sidecarcontroller.ComputeStoreSubscriptions, error) {
		self.calls = append(self.calls, "compute-stores")
		return self.stores, self.storesErr
	}
	steps.computeAppleRenewing = func(clientSession *session.ClientSession, transactionIds []string) (bool, error) {
		self.calls = append(self.calls, "compute-apple")
		self.appleAsked = transactionIds
		return self.computeApple, nil
	}
	steps.closeCompute = func(clientSession *session.ClientSession, computeStores *sidecarcontroller.ComputeStoreSubscriptions) error {
		self.calls = append(self.calls, "compute-close")
		self.handed = computeStores
		return self.closeErr
	}
	return steps
}

// compute's App Store subscriptions are checked beside Pro's, before Stripe or Play is touched, so their refusal
// cancels nothing; compute's Play subscription is cancelled after Pro's, and the closure is handed the read the store
// steps checked.
func TestNetworkRemoveStopsComputeStoreSubscriptionsAsPros(t *testing.T) {
	stores := &sidecarcontroller.ComputeStoreSubscriptions{
		AppStoreTransactionIds:   []string{"synthetic-original", "synthetic-grant"},
		GooglePlayPurchaseTokens: []string{"synthetic-compute-token"},
	}
	fake := newNetworkRemoveComputeFake(stores)
	fake.storeSnapshot = nil
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("remove result=%+v err=%v, want success", result, err)
	}
	want := []string{"last-owner", "snapshot", "apple", "compute-stores", "compute-apple", "stripe", "play", "play", "compute-close", "remove", "product-updates"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("steps = %v, want %v", fake.calls, want)
	}
	if !reflect.DeepEqual(fake.appleAsked, stores.AppStoreTransactionIds) {
		t.Fatalf("compute's App Store check got %v", fake.appleAsked)
	}
	if len(fake.playCancelled) != 2 || !reflect.DeepEqual(fake.playCancelled[1], stores.GooglePlayPurchaseTokens) {
		t.Fatalf("Play cancellations %v", fake.playCancelled)
	}
	if fake.handed != stores {
		t.Fatalf("the closure was handed %+v, not the read the store steps checked", fake.handed)
	}

	// a renewing compute App Store subscription refuses the deletion before anything is cancelled or closed
	refusing := newNetworkRemoveComputeFake(stores)
	refusing.computeApple = true
	result, err = networkRemoveWithSteps(networkRemoveStepsTestSession(), refusing.steps())
	if err != nil || result == nil || result.Error == nil || result.Error.Message != networkRemoveComputeAppleRenewingMessage {
		t.Fatalf("a renewing compute subscription: %+v %v", result, err)
	}
	if !reflect.DeepEqual(refusing.calls, []string{"last-owner", "snapshot", "apple", "compute-stores", "compute-apple"}) || refusing.removed {
		t.Fatalf("steps after compute's refusal: %v", refusing.calls)
	}
}

// A read with more App Store grants than it may ask about waits without asking; a network with no compute store
// subscription asks the App Store nothing and cancels no second Play; a failed read refuses before any cancellation.
func TestNetworkRemoveComputeStoreReadFailsClosed(t *testing.T) {
	truncated := newNetworkRemoveComputeFake(&sidecarcontroller.ComputeStoreSubscriptions{AppStoreTransactionIds: []string{"synthetic-grant"}, Truncated: true})
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), truncated.steps())
	if err != nil || result == nil || result.Error == nil || result.Error.Message != networkRemoveComputeAppleRenewingMessage {
		t.Fatalf("a truncated read: %+v %v", result, err)
	}
	if slices.Contains(truncated.calls, "compute-apple") || slices.Contains(truncated.calls, "stripe") {
		t.Fatalf("a truncated read ran %v", truncated.calls)
	}

	none := newNetworkRemoveComputeFake(&sidecarcontroller.ComputeStoreSubscriptions{})
	result, err = networkRemoveWithSteps(networkRemoveStepsTestSession(), none.steps())
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("no compute store subscription: %+v %v", result, err)
	}
	if slices.Contains(none.calls, "compute-apple") || len(none.playCancelled) != 1 {
		t.Fatalf("no compute store subscription ran %v, cancelled %v", none.calls, none.playCancelled)
	}

	failing := newNetworkRemoveComputeFake(nil)
	failing.storesErr = errors.New("synthetic read failure")
	result, err = networkRemoveWithSteps(networkRemoveStepsTestSession(), failing.steps())
	if err != nil || result == nil || result.Error == nil || result.Error.Message != networkRemoveComputeFailedMessage {
		t.Fatalf("a failed read: %+v %v", result, err)
	}
	if slices.Contains(failing.calls, "stripe") || failing.removed {
		t.Fatalf("a failed read ran %v", failing.calls)
	}
}

// A closure that finds a store subscription the read did not name keeps the network and asks for a retry, whose store
// steps read and check it.
func TestNetworkRemoveComputeStoreChangedAsksForRetry(t *testing.T) {
	fake := newNetworkRemoveComputeFake(&sidecarcontroller.ComputeStoreSubscriptions{})
	fake.closeErr = sidecarcontroller.ErrComputeCloseStoresChanged
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil || result == nil || result.Error == nil || result.Error.Message != networkRemoveStoreRenewalChangedMessage {
		t.Fatalf("a store subscription since the read: %+v %v", result, err)
	}
	if fake.removed || slices.Contains(fake.calls, "remove") {
		t.Fatalf("the network was removed: %v", fake.calls)
	}
}
