package controller

// Late payments to invoices the retention unlink has passed
// (cryptoEvaluateIntent): what the late-payment rules credit is held when the
// credit has nowhere to go. No database. These tests name only what existed
// before the rule, so they also run, and fail, against the code before it.

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// A late payment that covers the price at its first-seen rate, to an invoice
// the retention unlink passed, is held for an operator as unlinked, and keeps
// the invoice in review on every later judgment. Root cause (2026-11-13): the
// judge credited it, though the unlink had forgotten the invoice's network,
// so the credit failed five scans in a row before an operator heard of it as
// a failure; with every issued address now read, such payments are reachable
// on every coin.
func TestCryptoLatePaymentToAnUnlinkedInvoiceIsHeld(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	policy := cryptoIntentPolicyFor(cryptoTestConfig(), model.CryptoAssetXmr)
	// the coin is worth twice its quote now: the late payment covers the price
	rate := 555.36 * 2
	latePayment := func(intent *model.CryptoPaymentIntent) *model.CryptoPaymentReceipt {
		receipt := cryptoTestReceipt(intent, "late-"+intent.Delivery, intent.AmountAtomic, 1, now.Add(-time.Hour), model.CryptoReceiptStateConfirmed)
		receipt.FirstSeenRateUsd = &rate
		return receipt
	}
	unlinkedAt := now.Add(-9 * 24 * time.Hour)

	intent := cryptoTestIntent(model.CryptoAssetXmr, 4, now.Add(-40*24*time.Hour))
	intent.UnlinkedAt = &unlinkedAt
	intent.NetworkId = nil
	receipt := latePayment(intent)
	view := cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{receipt}, policy, now)
	connect.AssertEqual(t, view.Action, cryptoIntentActionReview)
	connect.AssertEqual(t, view.ActionReason, "unlinked")
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, view.ReviewReason, "unlinked")
	connect.AssertEqual(t, view.ReviewReceipts, []model.CryptoPaymentReceiptKey{receipt.Key()})
	connect.AssertEqual(t, len(view.CreditReceipts), 0)
	connect.AssertEqual(t, view.CreditUsd, model.NanoCents(0))

	// held, it keeps the invoice in review: never credited by the scanner
	receipt.State = model.CryptoReceiptStateReview
	receipt.ReviewReason = "unlinked"
	view = cryptoEvaluateIntent(intent, []*model.CryptoPaymentReceipt{receipt}, policy, now.Add(time.Hour))
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, view.ReviewReason, "unlinked")

	// a code invoice still credits late: its buyer reads the code with the
	// status token, which the unlink keeps
	code := cryptoTestIntent(model.CryptoAssetXmr, 5, now.Add(-40*24*time.Hour))
	code.Delivery = model.CryptoDeliveryCode
	code.NetworkId = nil
	code.UnlinkedAt = &unlinkedAt
	view = cryptoEvaluateIntent(code, []*model.CryptoPaymentReceipt{latePayment(code)}, policy, now)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, true)
	connect.AssertEqual(t, view.CreditUsd, code.PriceUsdNanoCents)

	// and an invoice the unlink has not passed credits late as before
	linked := cryptoTestIntent(model.CryptoAssetXmr, 6, now.Add(-10*24*time.Hour))
	view = cryptoEvaluateIntent(linked, []*model.CryptoPaymentReceipt{latePayment(linked)}, policy, now)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, view.Late, true)
}
