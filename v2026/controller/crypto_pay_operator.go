package controller

// Operator tools for the crypto payment rail (mmm/ZCASH-CONTRACT.md
// §3.10, §3.13), behind `bringyourctl crypto`: list and show invoices, credit
// one by hand after review (including one whose credit was taken back when
// its payment seemed to leave the chain), void an open one, and refund one
// once (the entitlement ends and the balance is clawed back like the Stripe
// refund path; the coins are sent by hand from the cold wallet, decision 8).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// CryptoOperatorInvoice is an invoice as the operator tools show it.
type CryptoOperatorInvoice struct {
	Intent   *model.CryptoPaymentIntent
	Receipts []*model.CryptoPaymentReceipt
	// the status the buyer sees (§2.5)
	Status       string
	ReviewReason string
}

// CryptoOperatorListInvoices lists invoices, newest first, with their status.
// asset "" is every coin; status "" is every status.
func CryptoOperatorListInvoices(ctx context.Context, asset string, status string, limit int) []*CryptoOperatorInvoice {
	config := cryptoPayConfigFunc()
	now := server.NowUtc()
	invoices := []*CryptoOperatorInvoice{}
	for _, intent := range model.ListCryptoPaymentIntents(ctx, asset, limit) {
		invoice := cryptoOperatorInvoice(ctx, config, intent, now)
		if status == "" || invoice.Status == status {
			invoices = append(invoices, invoice)
		}
	}
	return invoices
}

func cryptoOperatorInvoice(ctx context.Context, config *CryptoPayConfig, intent *model.CryptoPaymentIntent, now time.Time) *CryptoOperatorInvoice {
	receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
	view := cryptoEvaluateIntent(intent, receipts, cryptoIntentPolicyFor(config, intent.Asset), now)
	return &CryptoOperatorInvoice{
		Intent:       intent,
		Receipts:     receipts,
		Status:       view.Status,
		ReviewReason: view.ReviewReason,
	}
}

// CryptoOperatorShowInvoice is one invoice, or nil.
func CryptoOperatorShowInvoice(ctx context.Context, intentId server.Id) *CryptoOperatorInvoice {
	intent := model.GetCryptoPaymentIntent(ctx, intentId)
	if intent == nil {
		return nil
	}
	return cryptoOperatorInvoice(ctx, cryptoPayConfigFunc(), intent, server.NowUtc())
}

// CryptoOperatorUnmatchedReceipts lists the receipts paid to an address no
// invoice was given (and the change of our own offline spends, which lands on
// the Monero primary address).
func CryptoOperatorUnmatchedReceipts(ctx context.Context, asset string, limit int) []*model.CryptoPaymentReceipt {
	receipts := []*model.CryptoPaymentReceipt{}
	for _, receipt := range model.ListCryptoPaymentReceiptsByStateInConn(nil, ctx, asset, []string{model.CryptoReceiptStateReview}, limit) {
		if receipt.IntentId == nil {
			receipts = append(receipts, receipt)
		}
	}
	return receipts
}

// CryptoOperatorCredit credits an invoice by hand after review. Every receipt
// paid to it counts, at its value (its share of the quoted price when on
// time, its first-seen rate when late), capped at the price, except one that
// never counts (ignored), one that left the chain before any credit
// (orphaned), and a mempool sighting the wallet flags as a double spend. It
// also grants again an invoice whose credit was taken back because its
// payment seemed to leave the chain (its receipts in review/orphaned, and any
// that stayed credited), once the operator has seen the payment on the chain
// (mmm/ZCASH-CONTRACT.md §3.13.2). A voided invoice is never credited. USDC is credited
// only by its webhook and reconciler, which know the paying transaction.
func CryptoOperatorCredit(clientSession *session.ClientSession, intentId server.Id) (bool, error) {
	ctx := clientSession.Ctx
	intent := model.GetCryptoPaymentIntent(ctx, intentId)
	if intent == nil {
		return false, errors.New("no such invoice")
	}
	if intent.CreditedAt != nil {
		return false, errors.New("the invoice is already credited")
	}
	if intent.VoidedAt != nil {
		return false, errors.New("the invoice is void: it is never credited; return the coins by hand")
	}
	if intent.Asset == model.CryptoAssetUsdcSolana {
		return false, errors.New("a USDC invoice is credited by its Solana payment (the Helius webhook or `payments reconcile`)")
	}
	if intent.Delivery != model.CryptoDeliveryCode && intent.NetworkId == nil {
		return false, errors.New("the invoice is unlinked from its network (retention): grant by hand")
	}
	decimals := cryptoAssetDecimals(intent.Asset)
	credit := &cryptoCredit{receiptUsd: map[model.CryptoPaymentReceiptKey]model.NanoCents{}}
	for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, intentId) {
		switch {
		case receipt.State == model.CryptoReceiptStateIgnored, receipt.State == model.CryptoReceiptStateOrphaned:
			continue
		case receipt.State == model.CryptoReceiptStateSeen && receipt.ReviewReason == model.CryptoReviewReasonDoubleSpend:
			continue
		}
		var value model.NanoCents
		if !receipt.FirstSeenAt.After(intent.ExpiresAt) {
			value = cryptoOnTimeShareNanoCents(intent, receipt.AmountAtomic)
		} else if receipt.FirstSeenRateUsd != nil {
			value = cryptoAtomicUsdValueNanoCents(receipt.AmountAtomic, decimals, *receipt.FirstSeenRateUsd)
			credit.late = true
		}
		credit.receipts = append(credit.receipts, receipt.Key())
		credit.receiptUsd[receipt.Key()] = value
		credit.usdNanoCents += value
	}
	if len(credit.receipts) == 0 {
		return false, errors.New("the invoice has no receipt to credit")
	}
	credit.usdNanoCents = min(credit.usdNanoCents, intent.PriceUsdNanoCents)
	return cryptoCreditIntent(clientSession, intent, credit)
}

// CryptoOperatorVoid closes an open invoice: a payment that still arrives is
// held for review.
func CryptoOperatorVoid(ctx context.Context, intentId server.Id) (bool, error) {
	if model.GetCryptoPaymentIntent(ctx, intentId) == nil {
		return false, errors.New("no such invoice")
	}
	return model.VoidCryptoPaymentIntent(ctx, intentId, server.NowUtc()), nil
}

// CryptoOperatorRefundResult is what a refund ended and what to send back.
type CryptoOperatorRefundResult struct {
	Intent          *model.CryptoPaymentIntent
	EndedNetworkIds []server.Id
	ReceivedAtomic  int64
	// the amount received, in coin units, for the operator's transfer
	Received string
}

// cryptoValidRefundAddress is a shape check of an address the account holder
// gave: alphanumeric (base58 or bech32) and of a plausible length. The coins
// are sent by hand, and the operator's wallet validates it again.
func cryptoValidRefundAddress(asset string, address string) bool {
	if !cryptoUriAddressOk(address) {
		return false
	}
	switch asset {
	case model.CryptoAssetXmr:
		return len(address) == 95 || len(address) == 106
	case model.CryptoAssetZec:
		return 26 <= len(address)
	case model.CryptoAssetUsdcSolana:
		return 32 <= len(address) && len(address) <= 44
	}
	return false
}

// A refund that would end nothing: everything the credit granted already
// ended (or was redeemed and used up), so there is nothing to take back.
var errCryptoRefundEndedNothing = errors.New("nothing the credit granted is still running: there is nothing to end, so no refund was recorded")

// A refund of an invoice that was refunded (or unlinked) in the meantime.
var errCryptoRefundRefused = errors.New("the invoice was refunded already")

// CryptoOperatorRefund ends what a credited invoice granted, at now, and
// records the refund (with the address the coins go to) as a reconciliation
// event, once: refunded_at is the one-shot (mmm/ZCASH-CONTRACT.md §3.13.8). It refuses
// an invoice unlinked from its network (the grant can no longer be found)
// and one whose grant ended already. The coins themselves are sent by hand
// from the cold wallet.
func CryptoOperatorRefund(clientSession *session.ClientSession, intentId server.Id, payoutAddress string) (*CryptoOperatorRefundResult, error) {
	ctx := clientSession.Ctx
	intent := model.GetCryptoPaymentIntent(ctx, intentId)
	if intent == nil {
		return nil, errors.New("no such invoice")
	}
	if intent.CreditedAt == nil {
		return nil, errors.New("the invoice was never credited: void it instead")
	}
	if intent.UnlinkedAt != nil {
		return nil, errors.New("the invoice is unlinked from its network (retention): its grant can no longer be found; refund by hand")
	}
	if intent.RefundedAt != nil {
		return nil, errCryptoRefundRefused
	}
	payoutAddress = strings.TrimSpace(payoutAddress)
	if !cryptoValidRefundAddress(intent.Asset, payoutAddress) {
		return nil, fmt.Errorf("that does not look like a %s address", intent.Asset)
	}
	result := &CryptoOperatorRefundResult{Intent: intent}
	for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, intentId) {
		if receipt.State == model.CryptoReceiptStateCredited {
			result.ReceivedAtomic += receipt.AmountAtomic
		}
	}
	if intent.Asset == model.CryptoAssetUsdcSolana {
		result.ReceivedAtomic = intent.AmountAtomic
	}
	result.Received = cryptoFormatAtomic(result.ReceivedAtomic, cryptoAssetDecimals(intent.Asset))

	market, transactionId := model.CryptoPaymentRenewalHandle(intent)
	plan := slices.Contains(cryptoPayKnownPlans, intent.Item)
	now := server.NowUtc()
	var refundErr error
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			if !model.MarkCryptoPaymentIntentRefundedInTx(tx, ctx, intent.IntentId, now) {
				server.Raise(errCryptoRefundRefused)
			}
			endedNetworkIds, ended := model.EndCryptoPaymentGrantInTx(tx, ctx, intent, market, transactionId, plan, now)
			if !ended {
				server.Raise(errCryptoRefundEndedNothing)
			}
			result.EndedNetworkIds = endedNetworkIds
			server.Raise(model.AddPaymentReconciliationEventInTx(tx, ctx, &model.PaymentReconciliationEvent{
				RunId:     server.NewId(),
				Store:     market,
				NetworkId: intent.NetworkId,
				Action:    model.PaymentReconcileActionRefunded,
				Evidence:  intent.IntentId.String(),
				Details: map[string]any{
					"asset":           intent.Asset,
					"item":            intent.Item,
					"received_atomic": fmt.Sprintf("%d", result.ReceivedAtomic),
					"payout_address":  payoutAddress,
					"leg":             "operator_refund",
				},
			}))
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) {
		refundErr = err
	})
	if refundErr != nil {
		return nil, refundErr
	}
	for _, networkId := range result.EndedNetworkIds {
		model.UpdateProNetwork(ctx, networkId)
	}
	return result, nil
}
