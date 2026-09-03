package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Hermetic tests for /pay/data/checkout: the request validation and the url and
// metadata shapes the Stripe checkout carries. The database-backed paths
// (network name resolution, the webhook apply paths) are in
// pay_data_checkout_db_test.go; the USDC on Solana path is in
// pay_data_solana_test.go.

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

	// coinbase was dropped: crypto is USDC on Solana (pay_data_solana_controller.go)
	_, _, errMessage = payDataValidate(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, Provider: "coinbase"})
	connect.AssertEqual(t, errMessage, "Unknown provider.")

	_, _, errMessage = payDataValidate(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib, Provider: "stripe", Email: "not an email"})
	connect.AssertEqual(t, errMessage, "That email address does not look right.")

	// an empty provider is stripe, so a request written before the field
	// existed keeps working
	_, provider, errMessage := payDataValidate(&PayDataCheckoutArgs{ItemId: StripeItemData1Tib})
	connect.AssertEqual(t, errMessage, "")
	connect.AssertEqual(t, provider, PayDataProviderStripe)

	target, provider, errMessage := payDataValidate(&PayDataCheckoutArgs{
		ItemId:   " data_10tib ",
		Provider: " Stripe ",
		Email:    " buyer@example.com ",
	})
	connect.AssertEqual(t, errMessage, "")
	connect.AssertEqual(t, provider, PayDataProviderStripe)
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
