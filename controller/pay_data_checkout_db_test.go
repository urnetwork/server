package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Database-backed tests for the buy-data flow: network name resolution and the
// webhook apply path for both providers. These need the test database
// (server.DefaultTestEnv) and pro.yml.

// TestFindNetworkByName pins the resolution the buy-data page relies on:
// case-insensitive, exact, and NOT the sign-up similarity check.
func TestFindNetworkByName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "buydatatarget", userId)

		foundId, name := model.FindNetworkByName(ctx, "BuyDataTarget")
		connect.AssertEqual(t, foundId != nil, true)
		connect.AssertEqual(t, *foundId, networkId)
		connect.AssertEqual(t, name, "buydatatarget")

		foundId, _ = model.FindNetworkByName(ctx, "  buydatatarget  ")
		connect.AssertEqual(t, *foundId, networkId)

		// one character off is a DIFFERENT network, not this one
		foundId, name = model.FindNetworkByName(ctx, "buydatatarget2")
		connect.AssertEqual(t, foundId == nil, true)
		connect.AssertEqual(t, name, "")

		// too short to be a name at all
		foundId, _ = model.FindNetworkByName(ctx, "ab")
		connect.AssertEqual(t, foundId == nil, true)

		clientSession := payDataTestSession(t)

		lookup, err := PayDataNetworkLookup(&PayDataNetworkLookupArgs{NetworkName: "BUYDATATARGET"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lookup.Exists, true)
		connect.AssertEqual(t, lookup.NetworkName, "buydatatarget")

		lookup, err = PayDataNetworkLookup(&PayDataNetworkLookupArgs{NetworkName: "buydatatarget2"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lookup.Exists, false)
		connect.AssertEqual(t, lookup.NetworkName, "")

		// checkout with an unknown name refuses before any provider call
		result, err := PayDataCheckout(&PayDataCheckoutArgs{
			ItemId:      StripeItemData1Tib,
			Provider:    PayDataProviderStripe,
			NetworkName: "buydatatarget2",
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Message, "No network named buydatatarget2")
	})
}

// stripeBuyDataTestEnv is a fake Stripe api that serves the line items of a
// checkout session, which is the one call checkout.session.completed makes.
func stripeBuyDataTestEnv(t testing.TB, lineItems map[string][]*StripeLineItem) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/checkout/sessions/{sessionId}/line_items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": lineItems[r.PathValue("sessionId")]})
	})
	testServer := httptest.NewServer(mux)

	prevBaseUrl := stripeApiBaseUrl
	prevTokenFunc := stripeApiTokenFunc
	prevSkus := stripeSkusFunc
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_buydata" }
	stripeSkusFunc = func() map[string]*Sku {
		return map[string]*Sku{
			"prod_1tib": {FeeFraction: 0.3, BalanceByteCountHumanReadable: "1TiB"},
		}
	}
	t.Cleanup(func() {
		stripeApiBaseUrl = prevBaseUrl
		stripeApiTokenFunc = prevTokenFunc
		stripeSkusFunc = prevSkus
		testServer.Close()
	})
}

// TestStripeWebhookAppliesDataToNamedNetwork: a checkout.session.completed event
// from a buy-data checkout made FOR a network lands the data on that network,
// and a retry of the same event does not land it twice.
func TestStripeWebhookAppliesDataToNamedNetwork(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "buydatastripe", userId)

		sessionId := "cs_test_buydata_1"
		stripeBuyDataTestEnv(t, map[string][]*StripeLineItem{
			sessionId: {
				{Id: "li_1", AmountTotal: 500, Quantity: 1, Price: &StripeLineItemProduct{Product: "prod_1tib"}},
			},
		})

		target := payDataTarget{
			ItemId:      StripeItemData1Tib,
			ByteCount:   1 * model.Tib,
			NetworkId:   &networkId,
			NetworkName: "buydatastripe",
		}
		params := payDataStripeSessionParams(target, payDataTestUrls(), nil)

		// the event Stripe sends back for that session
		completed := map[string]any{
			"id":                  sessionId,
			"object":              "checkout.session",
			"amount_total":        500,
			"payment_status":      "paid",
			"client_reference_id": *params.ClientReferenceID,
			"metadata":            params.Metadata,
			"customer_details":    map[string]any{"email": "buyer@bringyour.com"},
		}

		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		_, err := StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", completed), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].BalanceByteCount, 1*model.Tib)
		connect.AssertEqual(t, balances[0].Pro, false)

		// the code behind the purchase is redeemed, so it cannot be used again
		balanceCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, stripeCheckoutPurchaseEventId(sessionId, 0))
		connect.AssertEqual(t, err, nil)
		balanceCode, err := model.GetBalanceCode(ctx, balanceCodeId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, balanceCode.RedeemTime.IsZero(), false)

		// Stripe retries: nothing lands twice
		_, err = StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", completed), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestCoinbaseWebhookAppliesDataToNamedNetwork: a charge:confirmed event whose
// metadata names a network (a buy-data crypto checkout) lands the data there,
// with no email needed, and a retry does not land it twice.
func TestCoinbaseWebhookAppliesDataToNamedNetwork(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "buydatacoinbase", userId)

		target := payDataTarget{
			ItemId:      StripeItemData1Tib,
			ByteCount:   1 * model.Tib,
			NetworkId:   &networkId,
			NetworkName: "buydatacoinbase",
		}
		request := coinbaseChargeRequest(target, "1TiB", 5, payDataTestUrls())
		if _, ok := coinbaseSkus()[request.Name]; !ok {
			t.Skip("coinbase.yml has no 1TiB sku in this environment")
		}

		// the event Coinbase sends back for that charge, metadata round-tripped
		metadataJson, err := json.Marshal(request.Metadata)
		connect.AssertEqual(t, err, nil)
		var metadata CoinbaseEventDataMetadata
		connect.AssertEqual(t, json.Unmarshal(metadataJson, &metadata), nil)

		event := &CoinbaseWebhookArgs{
			Event: &CoinbaseEvent{
				Id:   "evt_buydata_1",
				Type: "charge:confirmed",
				Data: &CoinbaseEventData{
					Id:   "charge_buydata_1",
					Name: request.Name,
					Payments: []*CoinbaseEventDataPayment{
						{Net: &CoinbaseEventDataPaymentNet{
							Local: &CoinbaseEventDataPaymentAmount{Amount: request.LocalPrice.Amount, Currency: "USD"},
						}},
					},
					Metadata: &metadata,
				},
			},
		}

		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		_, err = CoinbaseWebhook(event, reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].BalanceByteCount, 1*model.Tib)

		// Coinbase retries: nothing lands twice
		_, err = CoinbaseWebhook(event, reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)

		// the same event with NO network and NO email is still refused
		event.Event.Data.Id = "charge_buydata_2"
		event.Event.Data.Metadata = &CoinbaseEventDataMetadata{}
		_, err = CoinbaseWebhook(event, reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err != nil, true)
	})
}
