package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Hermetic tests for /pay/data/checkout: the request validation, the url and
// metadata shapes both providers carry, and the Coinbase Commerce charge call.
// The database-backed paths (network name resolution, the webhook apply path)
// are in pay_data_checkout_db_test.go.

func payDataTestSession(t testing.TB) *session.ClientSession {
	clientId := server.NewId()
	return session.Testing_CreateClientSession(context.Background(), &jwt.ByJwt{
		NetworkId: server.NewId(),
		ClientId:  &clientId,
		UserId:    server.NewId(),
	})
}

func payDataTestUrls() StripeCheckoutUrls {
	return StripeCheckoutUrls{
		SuccessUrl: "https://ur.io/checkout/success?session_id={CHECKOUT_SESSION_ID}",
		CancelUrl:  "https://ur.io/checkout/cancel",
	}
}

func TestPayDataValidate(t *testing.T) {
	_, _, errMessage := payDataValidate(&PayDataCheckoutArgs{ItemId: "data_7tib", Provider: "stripe"})
	connect.AssertEqual(t, errMessage, "Unknown item.")

	_, _, errMessage = payDataValidate(&PayDataCheckoutArgs{ItemId: StripeItemProMonthly, Provider: "stripe"})
	connect.AssertEqual(t, errMessage, "Unknown item.")

	_, _, errMessage = payDataValidate(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, Provider: "paypal"})
	connect.AssertEqual(t, errMessage, "Unknown provider.")

	_, _, errMessage = payDataValidate(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, Provider: "coinbase", Email: "not an email"})
	connect.AssertEqual(t, errMessage, "That email address does not look right.")

	target, provider, errMessage := payDataValidate(&PayDataCheckoutArgs{
		ItemId:   " data_10tib ",
		Provider: " Coinbase ",
		Email:    " buyer@example.com ",
	})
	connect.AssertEqual(t, errMessage, "")
	connect.AssertEqual(t, provider, PayDataProviderCoinbase)
	connect.AssertEqual(t, target.ItemId, StripeItemData10Tib)
	connect.AssertEqual(t, target.ByteCount, 10*model.Tib)
	connect.AssertEqual(t, target.Email, "buyer@example.com")
	connect.AssertEqual(t, target.applyToNetwork(), false)
}

func TestPayDataValidEmail(t *testing.T) {
	connect.AssertEqual(t, payDataValidEmail("buyer@example.com"), true)
	connect.AssertEqual(t, payDataValidEmail("a+b@sub.example.io"), true)
	connect.AssertEqual(t, payDataValidEmail("buyer"), false)
	connect.AssertEqual(t, payDataValidEmail("buyer@"), false)
	connect.AssertEqual(t, payDataValidEmail("@example.com"), false)
	connect.AssertEqual(t, payDataValidEmail("buyer@localhost"), false)
	connect.AssertEqual(t, payDataValidEmail("buy er@example.com"), false)
}

// TestPayDataSuccessUrl pins the success-url query: the configured url keeps its
// Stripe session placeholder and gains `item` and, for a network purchase,
// `network`.
func TestPayDataSuccessUrl(t *testing.T) {
	connect.AssertEqual(
		t,
		payDataSuccessUrl("https://ur.io/checkout/success?session_id={CHECKOUT_SESSION_ID}", StripeItemData1Tib, "my-net"),
		"https://ur.io/checkout/success?session_id={CHECKOUT_SESSION_ID}&item=data_1tib&network=my-net",
	)
	connect.AssertEqual(
		t,
		payDataSuccessUrl("https://ur.io/checkout/success", StripeItemData10Tib, ""),
		"https://ur.io/checkout/success?item=data_10tib",
	)
	// a name that needs escaping survives the round trip
	connect.AssertEqual(
		t,
		payDataSuccessUrl("https://ur.io/checkout/success", StripeItemData1Tib, "a&b=c"),
		"https://ur.io/checkout/success?item=data_1tib&network=a%26b%3Dc",
	)
}

func TestPayDataStripeSessionParams(t *testing.T) {
	networkId := server.NewId()

	target := payDataTarget{
		ItemId:      StripeItemData1Tib,
		ByteCount:   1 * model.Tib,
		NetworkId:   &networkId,
		NetworkName: "my-net",
		Email:       "buyer@example.com",
	}
	params := payDataStripeSessionParams(target, payDataTestUrls(), nil)

	connect.AssertEqual(t, *params.Mode, "payment")
	connect.AssertEqual(t, *params.ClientReferenceID, networkId.String())
	connect.AssertEqual(t, *params.CustomerEmail, "buyer@example.com")
	connect.AssertEqual(t, *params.SuccessURL, "https://ur.io/checkout/success?session_id={CHECKOUT_SESSION_ID}&item=data_1tib&network=my-net")
	connect.AssertEqual(t, *params.CancelURL, "https://ur.io/checkout/cancel")
	connect.AssertEqual(t, params.Metadata[payDataMetadataApplyToNetwork], "1")
	connect.AssertEqual(t, params.Metadata[payDataMetadataNetworkId], networkId.String())
	connect.AssertEqual(t, params.Metadata[payDataMetadataNetworkName], "my-net")
	connect.AssertEqual(t, params.Metadata[payDataMetadataItemId], StripeItemData1Tib)

	// the webhook reads the applied network back out of exactly this shape
	completed := &StripeEventCheckoutCompleteObject{
		Id:                "cs_test_1",
		ClientReferenceId: *params.ClientReferenceID,
		Metadata:          params.Metadata,
	}
	connect.AssertEqual(t, stripeAppliedNetworkName(completed), "my-net")

	// no network: a code by email. No client reference, no apply flag, and Stripe
	// collects the email itself.
	codeTarget := payDataTarget{ItemId: StripeItemData10Tib, ByteCount: 10 * model.Tib}
	params = payDataStripeSessionParams(codeTarget, payDataTestUrls(), nil)
	connect.AssertEqual(t, params.ClientReferenceID == nil, true)
	connect.AssertEqual(t, params.CustomerEmail == nil, true)
	connect.AssertEqual(t, *params.SuccessURL, "https://ur.io/checkout/success?session_id={CHECKOUT_SESSION_ID}&item=data_10tib")
	_, hasApply := params.Metadata[payDataMetadataApplyToNetwork]
	connect.AssertEqual(t, hasApply, false)
	connect.AssertEqual(t, stripeAppliedNetworkName(&StripeEventCheckoutCompleteObject{Metadata: params.Metadata}), "")
}

func TestStripeAppliedNetworkName(t *testing.T) {
	networkId := server.NewId()
	// the flag without a client reference is not a network purchase: the
	// fulfilment has nothing to redeem into
	connect.AssertEqual(t, stripeAppliedNetworkName(&StripeEventCheckoutCompleteObject{
		Metadata: map[string]string{payDataMetadataApplyToNetwork: "1", payDataMetadataNetworkName: "x"},
	}), "")
	// a signed-in checkout has a client reference and no metadata
	connect.AssertEqual(t, stripeAppliedNetworkName(&StripeEventCheckoutCompleteObject{
		ClientReferenceId: networkId.String(),
	}), "")
	connect.AssertEqual(t, stripeAppliedNetworkName(&StripeEventCheckoutCompleteObject{
		ClientReferenceId: networkId.String(),
		Metadata:          map[string]string{payDataMetadataApplyToNetwork: "0", payDataMetadataNetworkName: "x"},
	}), "")
	connect.AssertEqual(t, stripeAppliedNetworkName(&StripeEventCheckoutCompleteObject{
		ClientReferenceId: networkId.String(),
		Metadata:          map[string]string{payDataMetadataApplyToNetwork: "1", payDataMetadataNetworkName: "x"},
	}), "x")
}

func TestCoinbaseAppliedNetwork(t *testing.T) {
	networkId := server.NewId()

	redeemNetworkId, name := coinbaseAppliedNetwork(nil)
	connect.AssertEqual(t, redeemNetworkId == nil, true)
	connect.AssertEqual(t, name, "")

	redeemNetworkId, name = coinbaseAppliedNetwork(&CoinbaseEventDataMetadata{Email: "buyer@example.com"})
	connect.AssertEqual(t, redeemNetworkId == nil, true)
	connect.AssertEqual(t, name, "")

	// the flag with a malformed id delivers as a code, never as a crash
	redeemNetworkId, name = coinbaseAppliedNetwork(&CoinbaseEventDataMetadata{
		ApplyToNetwork: "1", NetworkId: "nonsense", NetworkName: "x",
	})
	connect.AssertEqual(t, redeemNetworkId == nil, true)
	connect.AssertEqual(t, name, "")

	redeemNetworkId, name = coinbaseAppliedNetwork(&CoinbaseEventDataMetadata{
		ApplyToNetwork: "1", NetworkId: networkId.String(), NetworkName: "my-net",
	})
	connect.AssertEqual(t, redeemNetworkId != nil, true)
	connect.AssertEqual(t, *redeemNetworkId, networkId)
	connect.AssertEqual(t, name, "my-net")
}

func TestCoinbaseSkuForItem(t *testing.T) {
	sku, ok := coinbaseSkuForItem(StripeItemData1Tib)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, sku, "1TiB")
	sku, ok = coinbaseSkuForItem(StripeItemData10Tib)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, sku, "10TiB")
	_, ok = coinbaseSkuForItem(StripeItemProMonthly)
	connect.AssertEqual(t, ok, false)
}

// TestCoinbaseChargeRequest pins the charge body: the sku name the webhook looks
// up, a fixed USD price from the same source Stripe charges, and the metadata the
// webhook reads back.
func TestCoinbaseChargeRequest(t *testing.T) {
	networkId := server.NewId()
	target := payDataTarget{
		ItemId:      StripeItemData10Tib,
		ByteCount:   10 * model.Tib,
		NetworkId:   &networkId,
		NetworkName: "my-net",
		Email:       "buyer@example.com",
	}
	request := coinbaseChargeRequest(target, "10TiB", 30, payDataTestUrls())

	requestBytes, err := json.Marshal(request)
	connect.AssertEqual(t, err, nil)
	var body map[string]any
	connect.AssertEqual(t, json.Unmarshal(requestBytes, &body), nil)

	connect.AssertEqual(t, body["name"], "10TiB")
	connect.AssertEqual(t, body["pricing_type"], "fixed_price")
	localPrice := body["local_price"].(map[string]any)
	connect.AssertEqual(t, localPrice["amount"], "30.00")
	connect.AssertEqual(t, localPrice["currency"], "USD")
	metadata := body["metadata"].(map[string]any)
	connect.AssertEqual(t, metadata["apply_to_network"], "1")
	connect.AssertEqual(t, metadata["network_id"], networkId.String())
	connect.AssertEqual(t, metadata["network_name"], "my-net")
	connect.AssertEqual(t, metadata["email"], "buyer@example.com")
	connect.AssertEqual(t, metadata["item_id"], StripeItemData10Tib)
	// Coinbase does not fill Stripe's placeholder: it is stripped, the rest kept
	connect.AssertEqual(t, body["redirect_url"], "https://ur.io/checkout/success?item=data_10tib&network=my-net")
	connect.AssertEqual(t, body["cancel_url"], "https://ur.io/checkout/cancel")
	connect.AssertEqual(t, strings.Contains(body["description"].(string), "10 TiB"), true)
	connect.AssertEqual(t, strings.Contains(body["description"].(string), "my-net"), true)

	// the webhook reads the applied network back out of exactly this metadata
	metadataJson, _ := json.Marshal(request.Metadata)
	var eventMetadata CoinbaseEventDataMetadata
	connect.AssertEqual(t, json.Unmarshal(metadataJson, &eventMetadata), nil)
	redeemNetworkId, appliedName := coinbaseAppliedNetwork(&eventMetadata)
	connect.AssertEqual(t, *redeemNetworkId, networkId)
	connect.AssertEqual(t, appliedName, "my-net")
	connect.AssertEqual(t, eventMetadata.Email, "buyer@example.com")

	// no network: the email is the delivery mechanism and there is no apply flag
	codeTarget := payDataTarget{ItemId: StripeItemData1Tib, ByteCount: 1 * model.Tib, Email: "buyer@example.com"}
	request = coinbaseChargeRequest(codeTarget, "1TiB", 5, payDataTestUrls())
	connect.AssertEqual(t, request.Name, "1TiB")
	connect.AssertEqual(t, request.LocalPrice.Amount, "5.00")
	_, hasApply := request.Metadata[payDataMetadataApplyToNetwork]
	connect.AssertEqual(t, hasApply, false)
	connect.AssertEqual(t, request.Metadata[payDataMetadataEmail], "buyer@example.com")
	connect.AssertEqual(t, request.RedirectUrl, "https://ur.io/checkout/success?item=data_1tib")
}

func TestCoinbaseRedirectBase(t *testing.T) {
	connect.AssertEqual(t, coinbaseRedirectBase("https://ur.io/checkout/success?session_id={CHECKOUT_SESSION_ID}"), "https://ur.io/checkout/success")
	connect.AssertEqual(t, coinbaseRedirectBase("https://ur.io/checkout/success"), "https://ur.io/checkout/success")
	connect.AssertEqual(t, coinbaseRedirectBase("https://ur.io/checkout/success?keep=1"), "https://ur.io/checkout/success?keep=1")
}

// TestCoinbaseCreateCharge runs the charge call against a fake Commerce api:
// the headers Coinbase authenticates with, the json body, and the hosted url
// that comes back.
func TestCoinbaseCreateCharge(t *testing.T) {
	var seenHeader http.Header
	var seenPath string
	var seenBody CoinbaseChargeRequest
	responseStatus := http.StatusOK
	responseBody := `{"data":{"id":"charge_1","code":"ABCDEF","hosted_url":"https://commerce.coinbase.com/charges/ABCDEF"}}`

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Clone()
		seenPath = r.URL.Path
		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &seenBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(responseStatus)
		io.WriteString(w, responseBody)
	}))
	defer testServer.Close()

	prevBase := coinbaseCommerceApiBase
	coinbaseCommerceApiBase = testServer.URL
	defer func() { coinbaseCommerceApiBase = prevBase }()

	ctx := context.Background()
	target := payDataTarget{ItemId: StripeItemData1Tib, ByteCount: 1 * model.Tib, Email: "buyer@example.com"}
	request := coinbaseChargeRequest(target, "1TiB", 5, payDataTestUrls())

	charge, err := coinbaseCreateCharge(ctx, "cc_test_key", request)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, charge.Id, "charge_1")
	connect.AssertEqual(t, charge.HostedUrl, "https://commerce.coinbase.com/charges/ABCDEF")

	connect.AssertEqual(t, seenPath, "/charges")
	connect.AssertEqual(t, seenHeader.Get("X-CC-Api-Key"), "cc_test_key")
	connect.AssertEqual(t, seenHeader.Get("X-CC-Version"), coinbaseCommerceApiVersion)
	connect.AssertEqual(t, strings.Contains(seenHeader.Get("Content-Type"), "application/json"), true)
	connect.AssertEqual(t, seenBody.Name, "1TiB")
	connect.AssertEqual(t, seenBody.PricingType, "fixed_price")
	connect.AssertEqual(t, seenBody.LocalPrice.Amount, "5.00")
	connect.AssertEqual(t, seenBody.Metadata[payDataMetadataEmail], "buyer@example.com")

	// no hosted url is a failure, not a blank checkout link
	responseBody = `{"data":{"id":"charge_2"}}`
	_, err = coinbaseCreateCharge(ctx, "cc_test_key", request)
	connect.AssertEqual(t, err != nil, true)

	// a rejected charge (bad key, bad price) is a failure
	responseStatus = http.StatusUnauthorized
	responseBody = `{"error":{"type":"authentication_error","message":"invalid api key"}}`
	_, err = coinbaseCreateCharge(ctx, "cc_test_key", request)
	connect.AssertEqual(t, err != nil, true)
}

// TestPayDataCoinbaseNotConfigured: with no Commerce api key in the vault, crypto
// checkout is refused with the message the page shows, not a panic.
func TestPayDataCoinbaseNotConfigured(t *testing.T) {
	prevKeyFunc := coinbaseCommerceApiKeyFunc
	coinbaseCommerceApiKeyFunc = func() string { return "" }
	defer func() { coinbaseCommerceApiKeyFunc = prevKeyFunc }()

	_, errMessage := payDataCoinbaseCheckout(context.Background(), payDataTarget{
		ItemId: StripeItemData1Tib, ByteCount: 1 * model.Tib, Email: "buyer@example.com",
	})
	connect.AssertEqual(t, errMessage, "Crypto checkout is not configured")

	// through the endpoint: a 200 with error.message
	result, err := PayDataCheckout(&PayDataCheckoutArgs{
		ItemId:   StripeItemData1Tib,
		Provider: PayDataProviderCoinbase,
		Email:    "buyer@example.com",
	}, payDataTestSession(t))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Url, "")
	connect.AssertEqual(t, result.Error != nil, true)
	connect.AssertEqual(t, result.Error.Message, "Crypto checkout is not configured")
}

// TestPayDataCheckoutEarlyErrors pins the refusals that happen before any lookup
// or provider call.
func TestPayDataCheckoutEarlyErrors(t *testing.T) {
	clientSession := payDataTestSession(t)

	result, err := PayDataCheckout(&PayDataCheckoutArgs{ItemId: "data_2tib", Provider: "stripe"}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "Unknown item.")

	result, err = PayDataCheckout(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, Provider: "cash"}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "Unknown provider.")

	// coinbase with neither a network nor an email has nowhere to deliver
	result, err = PayDataCheckout(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, Provider: PayDataProviderCoinbase}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.HasPrefix(result.Error.Message, "Enter the email"), true)

	// an empty lookup is simply "no"
	lookup, err := PayDataNetworkLookup(&PayDataNetworkLookupArgs{NetworkName: "  "}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, lookup.Exists, false)
	connect.AssertEqual(t, lookup.NetworkName, "")
}

func TestPayDataIpLimiter(t *testing.T) {
	limiter := &payDataIpLimiter{limitPerMinute: 3}
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	a := [32]byte{1}
	b := [32]byte{2}

	connect.AssertEqual(t, limiter.allowHash(a, now), true)
	connect.AssertEqual(t, limiter.allowHash(a, now.Add(time.Second)), true)
	connect.AssertEqual(t, limiter.allowHash(a, now.Add(2*time.Second)), true)
	connect.AssertEqual(t, limiter.allowHash(a, now.Add(3*time.Second)), false)
	// another address has its own budget
	connect.AssertEqual(t, limiter.allowHash(b, now.Add(3*time.Second)), true)
	// the window rolls over
	connect.AssertEqual(t, limiter.allowHash(a, now.Add(time.Minute)), true)

	// a session with no client address is not limited (tests, local runs)
	connect.AssertEqual(t, limiter.allow(&session.ClientSession{}), true)
	connect.AssertEqual(t, limiter.allow(nil), true)
}
