package controller

// Hermetic tests (no database) of the purchase report's answer for a trial
// refused for its card (subscription_verify_stripe_controller.go,
// subscription_stripe_card_check.go): the payment sheet's and Checkout's,
// whichever saw the card first, and the exact JSON the apps read. Stripe is
// the fake api of subscription_verify_stripe_test.go; the finalization and the
// invoice.paid handler are its seams.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Fails unless the report answered the card check's refusal.
func requireVerifyCardRefusal(t testing.TB, name string, result *VerifyStorePurchaseResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: err = %v, want the card refusal", name, err)
	}
	if result == nil || result.Status != VerifyPurchaseStatusInvalid || result.Error == nil ||
		result.Error.Code != PurchaseErrorCodeIncorrectCvc || result.Error.Message != purchaseIncorrectCvcMessage {
		t.Fatalf("%s: result = %+v, want invalid with incorrect_cvc", name, result)
	}
}

func TestVerifyStripeCardRefusal(t *testing.T) {
	env := newStripeVerifyTestEnv(t)
	networkId := server.NewId()
	clientSession := stripeVerifyTestSession(networkId)
	stamp := func(subscriptionId string) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		env.subscriptions[subscriptionId]["metadata"].(map[string]any)["card_check"] = "cvc_failed"
	}

	// a webhook refused the sheet's card first: its stamp answers, and nothing
	// is finalized or credited
	env.setPaymentSheetSubscription("sub_stamped", networkId, "trialing", "pending", "", "in_stamped")
	env.setInvoice("in_stamped", networkId, "paid", 0, "")
	stamp("sub_stamped")
	result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_stamped"}, clientSession)
	requireVerifyCardRefusal(t, "stamped sheet", result, err)
	if len(env.finalized) != 0 || len(env.ledger) != 0 {
		t.Fatalf("a refused sheet was finalized %v or credited %v", env.finalized, env.ledger)
	}

	// the report sees the card first: the finalization refuses it
	env.setPaymentSheetSubscription("sub_fresh", networkId, "trialing", "pending", "pm_bad", "in_fresh")
	env.setInvoice("in_fresh", networkId, "paid", 0, "")
	stripeVerifyFinalizeCustomerFunc = func(clientSession *session.ClientSession, customerId string, countryCode string) *stripeFinalizeResult {
		return &stripeFinalizeResult{Refused: map[string]bool{"sub_fresh": true}}
	}
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_fresh"}, clientSession)
	requireVerifyCardRefusal(t, "refused by the report", result, err)

	// the card could not be judged: a non-2xx, which the app retries
	env.setPaymentSheetSubscription("sub_unjudged", networkId, "trialing", "pending", "pm_unread", "in_unjudged")
	env.setInvoice("in_unjudged", networkId, "paid", 0, "")
	stripeVerifyFinalizeCustomerFunc = func(clientSession *session.ClientSession, customerId string, countryCode string) *stripeFinalizeResult {
		return &stripeFinalizeResult{Refused: map[string]bool{}, Err: errors.New("synthetic card read failure")}
	}
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_unjudged"}, clientSession)
	if err == nil {
		t.Fatalf("an unjudged card answered %+v, want a retryable error", result)
	}

	// a refused sheet replaced by a new one since: canceled, never credited
	env.setPaymentSheetSubscription("sub_replaced", networkId, "canceled", "pending", "", "in_replaced")
	env.setInvoice("in_replaced", networkId, "paid", 0, "")
	stamp("sub_replaced")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_replaced"}, clientSession)
	requireVerifyCardRefusal(t, "replaced refused sheet", result, err)
	// a canceled one with no stamp stays a plain invalid
	env.setPaymentSheetSubscription("sub_abandoned_canceled", networkId, "canceled", "pending", "", "in_abandoned_canceled")
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_abandoned_canceled"}, clientSession)
	requireVerifyStatus(t, "canceled without a refusal", result, err, VerifyPurchaseStatusInvalid)
	if result.Error != nil {
		t.Fatalf("a plain invalid carried %+v", result.Error)
	}

	// checkout: the invoice.paid handler refuses the trial
	env.setProCheckout("cs_test_refused", networkId, "complete", "in_checkout")
	env.setInvoice("in_checkout", networkId, "paid", 0, "")
	stripeVerifyCreditInvoiceFunc = func(invoice *StripeEventInvoiceObject, clientSession *session.ClientSession) (*OnboardingError, error) {
		return stripeCardCheckRefusalError(), nil
	}
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_refused"}, clientSession)
	requireVerifyCardRefusal(t, "refused checkout", result, err)
	// a step of the refusal left to retry (a cancel Stripe did not take) is
	// still the refusal for the buyer
	stripeVerifyCreditInvoiceFunc = func(invoice *StripeEventInvoiceObject, clientSession *session.ClientSession) (*OnboardingError, error) {
		return stripeCardCheckRefusalError(), errors.New("synthetic cancel failure")
	}
	result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_refused"}, clientSession)
	requireVerifyCardRefusal(t, "refused checkout, a step to retry", result, err)

	// the JSON the apps read: status invalid, which every client stops at,
	// and the error beside it
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"status":"invalid","error":{"code":"incorrect_cvc","message":"Your card's security code (CVC) didn't match. Check the card details and try again."}}`
	if string(data) != want {
		t.Fatalf("refusal json = %s\nwant %s", data, want)
	}
	if data, _ := json.Marshal(verifyStatusResult(VerifyPurchaseStatusPending)); string(data) != `{"status":"pending"}` {
		t.Fatalf("a plain answer = %s", data)
	}
}
