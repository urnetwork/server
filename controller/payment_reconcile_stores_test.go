package controller

import (
	"slices"
	"testing"

	"github.com/urnetwork/server/model"
)

// The reconciler's legs are model.PaymentReconcileStores, the one list the
// payment monitor expects a fresh watermark from and accepts repairs from. A
// leg added without the list would run unwatched, and a listed store with no
// leg would page for a watermark nothing writes.
func TestPaymentReconcileStoreLegsAreTheReconcileStores(t *testing.T) {
	stores := []string{}
	for _, leg := range paymentReconcileStoreLegs() {
		if leg.hasCredentials == nil || leg.reconcile == nil {
			t.Errorf("%s leg is incomplete", leg.store)
		}
		stores = append(stores, leg.store)
	}
	if !slices.Equal(stores, model.PaymentReconcileStores) {
		t.Fatalf("reconciler legs = %v, want model.PaymentReconcileStores %v", stores, model.PaymentReconcileStores)
	}
	// TAO, Bitcoin and ETH have legs; Pay with Gamma has none
	for _, market := range []string{model.SubscriptionMarketBittensor, model.SubscriptionMarketBitcoin, model.SubscriptionMarketEthereum} {
		if !slices.Contains(stores, market) {
			t.Errorf("%s has no reconciliation leg", market)
		}
	}
	if slices.Contains(stores, model.SubscriptionMarketGamma) {
		t.Errorf("gamma has a reconciliation leg; the dashboard and SIGNALS.md say it has none")
	}
}
