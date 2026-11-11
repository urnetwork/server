package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// A plan paid with gamma renews from the deposit: subscription details ask
// the gamma lookup with the renewal's intent id instead of reporting a
// one-time payment that never renews, and cancel points at the settings.
func TestGammaSubscriptionDetailsReportTheRenewal(t *testing.T) {
	now := time.Now().UTC()
	renewals := []*model.ActiveSubscriptionRenewal{
		subscriptionTestRenewal(model.SubscriptionMarketGamma, now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), "intent_1", ""),
	}
	asked := []string{}
	lookups := &subscriptionStoreLookupSet{
		gamma: func(ctx context.Context, intentId string) (*subscriptionStoreState, error) {
			asked = append(asked, intentId)
			return &subscriptionStoreState{AutoRenew: boolPtr(true)}, nil
		},
	}
	result := buildSubscriptionDetails(context.Background(), renewals, nil, "", lookups)
	if len(result.Subscriptions) != 1 {
		t.Fatalf("expected one entry, got %d", len(result.Subscriptions))
	}
	detail := result.Subscriptions[0]
	if detail.Store != model.SubscriptionMarketGamma || detail.TransactionId != "intent_1" || detail.ManageUrl != gammaManageUrl {
		t.Fatalf("gamma entry %+v", detail)
	}
	if len(asked) != 1 || asked[0] != "intent_1" {
		t.Fatalf("gamma lookup asked %v", asked)
	}
	if detail.AutoRenew == nil || !*detail.AutoRenew || detail.CancelAtPeriodEnd {
		t.Fatalf("a renewing gamma plan reads auto_renew %v cancel_at_period_end %v", detail.AutoRenew, detail.CancelAtPeriodEnd)
	}

	// without the lookup the answer is unknown, never "does not renew"
	result = buildSubscriptionDetails(context.Background(), renewals, nil, "", &subscriptionStoreLookupSet{})
	if result.Subscriptions[0].AutoRenew != nil {
		t.Fatalf("unknown auto_renew read as %v", *result.Subscriptions[0].AutoRenew)
	}

	refusal := subscriptionCancelRefusal(model.SubscriptionMarketGamma)
	if refusal == nil || refusal.Error == nil || refusal.Error.Code != SubscriptionErrorCodeNotCancelable || refusal.ManageUrl != gammaManageUrl {
		t.Fatalf("gamma cancel refusal %+v", refusal)
	}
}

// The services document SN25's on-chain service record points at names the
// spec where ur.io publishes it (/openapi.yml; /api/bringyour.yml is a 404),
// our endpoint, and the publisher from gamma.yml.
func TestGammaServicesDocumentPointsAtThePublishedSpec(t *testing.T) {
	skipWithoutProYml(t)
	settings := gammaTestSettings(t)
	defer setGammaSettingsForTest(settings, nil)()
	document, err := GammaServices(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Services) != 1 || document.Services[0].Type != "openapi" || document.Services[0].Spec != "https://ur.io/openapi.yml" || document.Services[0].Endpoint != "https://api.bringyour.com" {
		t.Fatalf("services %+v", document.Services)
	}
	if len(document.Publishers) != 1 || document.Publishers[0].Netuid != 25 || document.Publishers[0].Hotkey != settings.ServiceAddress {
		t.Fatalf("publishers %+v", document.Publishers)
	}
	if len(document.Payments) != 2 || document.Payments[0].Type != "gamma" || document.Payments[0].Terms != "https://api.bringyour.com/pay/gamma/terms" || document.Payments[1].Type != "x402" {
		t.Fatalf("payments %+v", document.Payments)
	}
}
