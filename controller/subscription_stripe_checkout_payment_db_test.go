package controller

// Root-cause tests of fulfilling a data pack only once its payment clears
// (owner, 2026-11-13). checkout.session.completed fulfilled every session it
// was sent, so a data pack paid with a delayed method (a bank debit or
// transfer, offered on 35 of 45 data-pack checkouts) was delivered before its
// money arrived, and kept when the payment failed. Each case below runs
// through the public webhook and the balance-code ledger, and each fulfilment
// happens exactly once however often Stripe delivers. Needs the test database
// and redis (server.DefaultTestEnv), and pro.yml for the data codes' duration.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Delivers a checkout event of the session, as Stripe would.
func deliverStripeCheckoutTestEvent(t testing.TB, clientSession *session.ClientSession, eventType string, sessionId string, paymentStatus string, networkId server.Id) {
	result, err := StripeWebhook(stripeWebhookEvent(t, eventType, stripeCheckoutPaymentTestSession(sessionId, paymentStatus, networkId)), clientSession)
	if err != nil || result == nil {
		t.Fatalf("%s %s (%s): %+v %v", eventType, sessionId, paymentStatus, result, err)
	}
}

// What the session delivered: whether it has its balance code, and how many
// data balances the network holds.
func stripeCheckoutTestFulfilment(ctx context.Context, sessionId string, networkId server.Id) (coded bool, balanceCount int) {
	_, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, sessionId)
	return err == nil, len(model.GetActiveTransferBalances(ctx, networkId))
}

// A data pack paid with a delayed method: the completed checkout is unpaid
// and delivers nothing; the payment clearing (async_payment_succeeded)
// delivers it once, and every redelivery of either event, and the client's
// report, delivers nothing more.
func TestStripeDataPackPaidLaterIsFulfilledOnceItClears(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newStripeCheckoutPaymentTestApi(t, true)
		messages := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(messages)
		defer SetMessageSender(previousSender)
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		const sessionId = "cs_synthetic_paid_later"

		deliverStripeCheckoutTestEvent(t, clientSession, "checkout.session.completed", sessionId, "unpaid", networkId)
		if coded, balanceCount := stripeCheckoutTestFulfilment(ctx, sessionId, networkId); coded || balanceCount != 0 {
			t.Fatalf("an unpaid checkout was fulfilled before its payment cleared (code %t, %d balances)", coded, balanceCount)
		}

		deliverStripeCheckoutTestEvent(t, clientSession, "checkout.session.async_payment_succeeded", sessionId, "paid", networkId)
		if coded, balanceCount := stripeCheckoutTestFulfilment(ctx, sessionId, networkId); !coded || balanceCount != 1 {
			t.Fatalf("the cleared payment must be fulfilled once (code %t, %d balances)", coded, balanceCount)
		}

		for range 2 {
			deliverStripeCheckoutTestEvent(t, clientSession, "checkout.session.async_payment_succeeded", sessionId, "paid", networkId)
			deliverStripeCheckoutTestEvent(t, clientSession, "checkout.session.completed", sessionId, "unpaid", networkId)
		}
		if _, balanceCount := stripeCheckoutTestFulfilment(ctx, sessionId, networkId); balanceCount != 1 {
			t.Fatalf("redeliveries must not fulfil again, got %d balances", balanceCount)
		}
	})
}

// A data pack whose delayed payment fails: neither the completed checkout nor
// the failure delivers anything, and the failure is recorded once for
// support.
func TestStripeDataPackWhosePaymentFailsIsNeverFulfilled(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newStripeCheckoutPaymentTestApi(t, true)
		messages := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(messages)
		defer SetMessageSender(previousSender)
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		const sessionId = "cs_synthetic_payment_fails"

		deliverStripeCheckoutTestEvent(t, clientSession, "checkout.session.completed", sessionId, "unpaid", networkId)
		for range 3 {
			deliverStripeCheckoutTestEvent(t, clientSession, "checkout.session.async_payment_failed", sessionId, "unpaid", networkId)
		}
		if coded, balanceCount := stripeCheckoutTestFulfilment(ctx, sessionId, networkId); coded || balanceCount != 0 {
			t.Fatalf("a checkout whose payment failed was fulfilled (code %t, %d balances)", coded, balanceCount)
		}
		if records := countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, "payment_failed", sessionId); records != 1 {
			t.Fatalf("the failed payment must be recorded once, got %d records", records)
		}
		if sends := messages.sent(); len(sends) != 0 {
			t.Fatalf("a checkout whose payment failed sent %d messages", len(sends))
		}
	})
}

// A data pack paid by card clears at once: the completed checkout delivers it
// once, and its redeliveries deliver nothing more.
func TestStripeDataPackPaidAtOnceIsFulfilledOnce(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newStripeCheckoutPaymentTestApi(t, true)
		messages := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(messages)
		defer SetMessageSender(previousSender)
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		const sessionId = "cs_synthetic_paid_at_once"

		for range 3 {
			deliverStripeCheckoutTestEvent(t, clientSession, "checkout.session.completed", sessionId, "paid", networkId)
		}
		if coded, balanceCount := stripeCheckoutTestFulfilment(ctx, sessionId, networkId); !coded || balanceCount != 1 {
			t.Fatalf("a checkout paid at once must be fulfilled once (code %t, %d balances)", coded, balanceCount)
		}
	})
}
