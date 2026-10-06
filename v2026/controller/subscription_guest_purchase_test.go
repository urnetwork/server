package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Hermetic tests (no database) for the purchase guard. The auth-method lookup
// is answered in memory, and every refusal returns before Stripe, the price
// tier or the database is touched.

// Answers the auth-method lookup for the session's user with
// hasAnyAuthMethod, and returns a session carrying guestMode.
func guestPurchaseTestSession(t *testing.T, hasAnyAuthMethod bool, guestMode bool) *session.ClientSession {
	t.Helper()
	userId := server.NewId()
	previous := purchaseHasAnyAuthMethod
	purchaseHasAnyAuthMethod = func(_ context.Context, id server.Id) bool {
		if id != userId {
			t.Errorf("auth methods looked up for %s, want the session user %s", id, userId)
		}
		return hasAnyAuthMethod
	}
	t.Cleanup(func() {
		purchaseHasAnyAuthMethod = previous
	})
	return session.NewLocalClientSession(context.Background(), "127.0.0.1:1", &jwt.ByJwt{
		NetworkId: server.NewId(),
		UserId:    userId,
		GuestMode: guestMode,
	})
}

// A refreshed legacy guest (no login method; RefreshToken cleared the
// GuestMode claim) gets the guest refusal from every server-created checkout
// and payment intent, with nothing to pay against. Before the guard each of
// these went on to create the checkout, so a pre-conversion app build could
// sell a plan to a network nothing can sign back in to.
func TestGuestPurchaseRefusedAtEveryEntry(t *testing.T) {
	// marshals a result as the api sends it and returns its `error` object
	wireErrorOf := func(entry string, result any) map[string]any {
		t.Helper()
		b, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("%s: marshal: %v", entry, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: unmarshal: %v", entry, err)
		}
		wireError, _ := m["error"].(map[string]any)
		if wireError == nil {
			t.Fatalf("%s: no error on the wire: %s", entry, b)
		}
		return wireError
	}

	for _, entry := range []struct {
		name string
		// calls the entry, checks the result carries nothing to pay against,
		// and returns it
		purchase func(t *testing.T, clientSession *session.ClientSession) any
	}{
		{name: "checkout session", purchase: func(t *testing.T, clientSession *session.ClientSession) any {
			result, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
				ItemId: StripeItemProYearly,
			}, clientSession)
			if err != nil || result == nil {
				t.Fatalf("checkout session: result=%v err=%v, want a refusal", result, err)
			}
			if result.CheckoutUrl != "" || result.ClientSecret != "" || result.SessionId != "" {
				t.Fatalf("checkout session: refusal carries a checkout: %+v", result)
			}
			return result
		}},
		{name: "payment sheet", purchase: func(t *testing.T, clientSession *session.ClientSession) any {
			result, err := StripePaymentSheet(&StripePaymentSheetArgs{
				Plan: model.PlanYearly,
			}, clientSession)
			if err != nil || result == nil {
				t.Fatalf("payment sheet: result=%v err=%v, want a refusal", result, err)
			}
			if result.CustomerId != "" || result.SubscriptionId != "" || result.SetupIntentClientSecret != "" || result.PaymentIntentClientSecret != "" {
				t.Fatalf("payment sheet: refusal carries a payment: %+v", result)
			}
			return result
		}},
		{name: "payment intent", purchase: func(t *testing.T, clientSession *session.ClientSession) any {
			result, err := StripeCreatePaymentIntent(&StripeCreatePaymentIntentArgs{}, clientSession)
			if err != nil || result == nil {
				t.Fatalf("payment intent: result=%v err=%v, want a refusal", result, err)
			}
			if len(result.PaymentIntents) != 0 || result.CustomerId != nil || result.EphemeralKey != nil {
				t.Fatalf("payment intent: refusal carries a payment: %+v", result)
			}
			return result
		}},
		{name: "solana payment intent", purchase: func(t *testing.T, clientSession *session.ClientSession) any {
			result, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
				Reference: "synthetic-guest-reference",
				Plan:      model.SolanaPlanMonthly,
			}, clientSession)
			if err != nil || result == nil {
				t.Fatalf("solana payment intent: result=%v err=%v, want a refusal", result, err)
			}
			if result.AmountUsd != 0 || result.Recipient != "" {
				t.Fatalf("solana payment intent: refusal carries a quote: %+v", result)
			}
			return result
		}},
	} {
		clientSession := guestPurchaseTestSession(t, false, false)
		wireError := wireErrorOf(entry.name, entry.purchase(t, clientSession))
		if wireError["code"] != PurchaseErrorCodeGuestSignInRequired {
			t.Errorf("%s: error code %v, want %s", entry.name, wireError["code"], PurchaseErrorCodeGuestSignInRequired)
		}
		if wireError["message"] != "Add a sign-in to your account before buying a plan." {
			t.Errorf("%s: error message %q", entry.name, wireError["message"])
		}
	}
}

// A network with a login method passes the guard even while its jwt still
// carries a stale GuestMode claim (a guest that just added a sign-in): the
// entries answer with their own validation and its code, not the guest code.
func TestGuestPurchaseAllowedWithSignIn(t *testing.T) {
	clientSession := guestPurchaseTestSession(t, true, true)

	checkout, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
		ItemId: StripeItemProYearly,
		UiMode: "synthetic-unknown",
	}, clientSession)
	if err != nil || checkout == nil || checkout.Error == nil {
		t.Fatalf("checkout session: result=%v err=%v, want the ui mode error", checkout, err)
	}
	if checkout.Error.Code != PurchaseErrorCodeInvalidRequest || checkout.Error.Message != "Unknown ui mode." {
		t.Fatalf("checkout session: error %+v, want the ui mode error", checkout.Error)
	}

	sheet, err := StripePaymentSheet(&StripePaymentSheetArgs{
		Plan: "synthetic-unknown",
	}, clientSession)
	if err != nil || sheet == nil || sheet.Error == nil {
		t.Fatalf("payment sheet: result=%v err=%v, want the plan error", sheet, err)
	}
	if sheet.Error.Code != PurchaseErrorCodeInvalidRequest || sheet.Error.Message != "Unknown plan." {
		t.Fatalf("payment sheet: error %+v, want the plan error", sheet.Error)
	}

	// the code rides beside the unchanged message, so a client that reads only
	// the message reads the refusal as before
	b, err := json.Marshal(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"error":{"code":"invalid_request","message":"Unknown ui mode."}}` {
		t.Fatalf("checkout error marshals as %s", b)
	}
}
