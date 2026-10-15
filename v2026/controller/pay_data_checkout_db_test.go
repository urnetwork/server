package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Database-backed tests for the buy-data flow: network name resolution and the
// webhook apply paths (Stripe, the legacy Coinbase metadata). These need the
// test database (server.DefaultTestEnv) and pro.yml. The USDC on Solana data
// pack credit is pay_data_solana_db_test.go.

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

// Supplies the configured sku used by the webhook without making this local
// database test depend on coinbase.yml.
func coinbaseBuyDataTestEnv(t testing.TB) {
	prevSkus := coinbaseSkusFunc
	coinbaseSkusFunc = func() map[string]*Sku {
		return map[string]*Sku{
			"1TiB": {FeeFraction: 0.3, BalanceByteCountHumanReadable: "1TiB"},
		}
	}
	t.Cleanup(func() {
		coinbaseSkusFunc = prevSkus
	})
}

// Captures purchase email templates so database tests never contact AWS and
// can verify that retries describe the already-applied credit correctly.
func payDataMessageTestEnv(t testing.TB) <-chan Template {
	messages := make(chan Template, 4)
	prevMessageSender := GetAWSMessageSender()
	SetMessageSender(&mockAWSMessageSender{
		SendMessageFunc: func(_ string, template Template, _ ...any) error {
			messages <- template
			return nil
		},
	})
	t.Cleanup(func() {
		SetMessageSender(prevMessageSender)
	})
	return messages
}

// TestStripeWebhookAppliesDataToNamedNetwork: a checkout.session.completed event
// from a buy-data checkout made FOR a network lands the data on that network,
// and a retry of the same event does not land it twice.
func TestStripeWebhookAppliesDataToNamedNetwork(t *testing.T) {
	skipWithoutProYml(t)
	sessionId := "cs_test_buydata_1"
	stripeBuyDataTestEnv(t, map[string][]*StripeLineItem{
		sessionId: {
			{Id: "li_1", AmountTotal: 500, Quantity: 1, Price: &StripeLineItemProduct{Product: "prod_1tib"}},
		},
	})
	messages := payDataMessageTestEnv(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		assertAppliedMessage := func() {
			select {
			case template := <-messages:
				_, ok := template.(*SubscriptionDataAppliedTemplate)
				connect.AssertEqual(t, ok, true)
			default:
				t.Fatal("expected a synchronous data-applied email")
			}
		}

		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "buydatastripe", userId)

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
		assertAppliedMessage()

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
		assertAppliedMessage()
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestCoinbaseWebhookAppliesDataToNamedNetwork: a charge:confirmed event whose
// metadata names a network lands the data there, with no email needed, and a
// retry does not land it twice. The hosted Coinbase checkout no longer creates
// such charges (crypto moved to USDC on Solana), but the webhook still honors
// the metadata for any charge that carries it.
func TestCoinbaseWebhookAppliesDataToNamedNetwork(t *testing.T) {
	skipWithoutProYml(t)
	coinbaseBuyDataTestEnv(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "buydatacoinbase", userId)

		skuName := "1TiB"
		if _, ok := coinbaseSkusFunc()[skuName]; !ok {
			t.Skip("coinbase.yml has no 1TiB sku in this environment")
		}

		event := &CoinbaseWebhookArgs{
			Event: &CoinbaseEvent{
				Id:   "evt_buydata_1",
				Type: "charge:confirmed",
				Data: &CoinbaseEventData{
					Id:   "charge_buydata_1",
					Name: skuName,
					Payments: []*CoinbaseEventDataPayment{
						{Net: &CoinbaseEventDataPaymentNet{
							Local: &CoinbaseEventDataPaymentAmount{Amount: "3.00", Currency: "USD"},
						}},
					},
					Metadata: &CoinbaseEventDataMetadata{
						ApplyToNetwork: payDataMetadataApplyYes,
						NetworkId:      networkId.String(),
						NetworkName:    "buydatacoinbase",
					},
				},
			},
		}

		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		_, err := CoinbaseWebhook(event, reconcileTestSession(t, ctx))
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
