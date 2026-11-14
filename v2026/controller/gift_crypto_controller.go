package controller

// Paying for a gift order with a coin (mmm/GIFTOFPRIVACY.md §10.2, §10.6
// part 2): POST /pay/gift/crypto-intent, and the gift branch of the crypto
// rail's one credit.
//
// A gift order's invoice is an invoice of the crypto rail like any other:
// item gift_pro_year and delivery code, since the codes are the delivery and
// no network is credited, as for a data code bought without one. It is priced
// at the order's total and capped by gift.crypto_max_invoice_usd ($500, an
// order of 20 at $23.99 is $479.80) in place of the quote's max_invoice_usd,
// which keeps the data rail at $100. Its status (/pay/crypto/status, which the
// site's CryptoPayPanel polls), its re-quote, the scanner, the reconcile legs
// and the operator tools are the rail's own; the coins that take one are the
// coins on offer that sell balance codes. Each order has its own invoice:
// open invoices are never reused across orders.
//
// Its credit (cryptoCreditIntentInConn hands a gift invoice here) wins the
// invoice's one-shot, then, in the same transaction, fulfils the order with
// the purchase event base crypto/<intent id> (codes crypto/<intent id>/gift/<n>)
// and the credited USD value as the codes' revenue, schedules the delivery
// when that fulfilment paid the order, and records the first code as the
// invoice's grant. The grant is what an operator refund and an orphaned
// payment's end (model.EndCryptoPaymentGrantInTx) claw back by, and they void
// every code of the invoice by its prefix. The invoice's status never shows a
// gift code: the order's cards carry them.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The crypto invoice item of a gift order. Not a data pack or a plan:
// crypto_pay.yml's item lists do not name it, and every coin that sells
// balance codes takes it.
const giftCryptoItem = "gift_pro_year"

// GiftCryptoIntentArgs is POST /pay/gift/crypto-intent: the order's fields,
// the coin, and the Bittensor footer.
type GiftCryptoIntentArgs struct {
	GiftPurchaseArgs
	// usdc_solana | zec | xmr | tao | btc | eth | sol: one of
	// GET /pay/gift/config's crypto_assets
	Asset string `json:"asset"`
	// "Made with Bittensor" on the cards and the friend emails
	BittensorFooter bool `json:"bittensor_footer,omitempty"`
}

// GiftCryptoIntentResult is POST /pay/gift/crypto-intent: the crypto invoice
// as POST /pay/crypto/intent answers it, which the site's CryptoPayPanel
// renders and follows with its status token, plus the order.
type GiftCryptoIntentResult struct {
	CryptoPaymentIntentResult
	// the order, and its token: the bearer secret that reads the order's
	// status and downloads its cards
	OrderId    string `json:"order_id,omitempty"`
	OrderToken string `json:"order_token,omitempty"`
	// the request's bittensor_footer, as the order stores it
	BittensorFooter bool `json:"bittensor_footer"`
}

func giftCryptoIntentError(code string, message string) *GiftCryptoIntentResult {
	return &GiftCryptoIntentResult{
		CryptoPaymentIntentResult: CryptoPaymentIntentResult{
			Error: &CryptoPaymentError{Code: code, Message: message},
		},
	}
}

// GiftCryptoIntent answers POST /pay/gift/crypto-intent: a crypto invoice for
// a new gift order. The invoice is made first, so a coin that cannot be
// quoted now stores no order; the order and its link to the invoice are
// stored before the invoice is handed out, so its credit always finds the
// order.
func GiftCryptoIntent(
	args *GiftCryptoIntentArgs,
	clientSession *session.ClientSession,
) (*GiftCryptoIntentResult, error) {
	giftConfig := model.Gift()
	if !giftConfig.Enabled {
		return giftCryptoIntentError(GiftErrorCodeSalesClosed, giftSalesClosedMessage), nil
	}
	order, errCode, errMessage := giftValidatePurchase(&args.GiftPurchaseArgs, giftConfig, server.NowUtc())
	if errMessage != "" {
		return giftCryptoIntentError(errCode, errMessage), nil
	}

	config := cryptoPayConfigFunc()
	asset := strings.TrimSpace(args.Asset)
	if !slices.Contains(cryptoPayAssets, asset) {
		return giftCryptoIntentError(PurchaseErrorCodeInvalidRequest, "Unknown asset."), nil
	}
	if !cryptoAssetEnabled(config, asset) {
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedDisabled).Inc()
		return giftCryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	if !config.Asset(asset).CodeDeliveryEnabled() {
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedNoCodes).Inc()
		return giftCryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	if giftConfig.CryptoMaxInvoiceUsd <= 0 {
		return giftCryptoIntentError(PurchaseErrorCodeItemUnavailable, "Gift orders cannot be paid with a coin now."), nil
	}

	if !giftPurchaseLimiter.allow(clientSession) {
		return giftCryptoIntentError(
			PurchaseErrorCodeRateLimited,
			"Too many payment attempts from this address. Try again in a minute.",
		), nil
	}
	// each invoice draws a pool address that stays taken until its quote
	// ends, as a data code's does
	if !cryptoCodeIntentLimiter.allow(clientSession) {
		return giftCryptoIntentError(
			PurchaseErrorCodeRateLimited,
			"Too many payment attempts from this address. Try again later.",
		), nil
	}

	// the buyer's daily friend emails, before the invoice is made and after
	// the order is stored (gift_rails_controller.go)
	if errCode, errMessage := giftFriendEmailLimitRefusal(clientSession.Ctx, order, giftConfig, server.NowUtc()); errMessage != "" {
		return giftCryptoIntentError(errCode, errMessage), nil
	}

	// the order keeps the buyer's email; the invoice needs none, since its
	// credit never emails a code (the delivery does)
	priceUsd := float64(order.TotalCents) / 100.0
	invoice := cryptoStartIntent(clientSession, config, &cryptoIntentRequest{
		asset:         asset,
		item:          giftCryptoItem,
		delivery:      model.CryptoDeliveryCode,
		priceUsd:      priceUsd,
		regularUsd:    priceUsd,
		maxInvoiceUsd: giftConfig.CryptoMaxInvoiceUsd,
	})
	if invoice.Error != nil {
		return &GiftCryptoIntentResult{CryptoPaymentIntentResult: *invoice}, nil
	}
	intentId, err := server.ParseId(invoice.IntentId)
	if err != nil {
		glog.Errorf("[gift]the %s invoice for a gift order has an unreadable id %q\n", asset, invoice.IntentId)
		return giftCryptoIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage), nil
	}

	ctx := clientSession.Ctx
	order.Rail = model.GiftRailCrypto
	order.BittensorFooter = args.BittensorFooter
	order.ReferrerNetworkId = giftReferrerNetworkId(&args.GiftPurchaseArgs, clientSession)
	if err := model.CreateGiftOrder(ctx, order); err != nil {
		glog.Errorf("[gift]could not store the gift order of invoice %s: %s\n", intentId, err)
		return giftCryptoIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage), nil
	}
	// refused here, the invoice is never handed out and expires unpaid
	if errCode, errMessage := giftKeepFriendEmailLimit(ctx, order, giftConfig, server.NowUtc()); errMessage != "" {
		return giftCryptoIntentError(errCode, errMessage), nil
	}
	if err := model.SetGiftOrderCryptoIntentId(ctx, order.GiftOrderId, intentId); err != nil {
		// never hand out an invoice whose credit could not find its order;
		// the invoice expires unpaid
		glog.Errorf("[gift]could not link invoice %s to its gift order: %s\n", intentId, err)
		return giftCryptoIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage), nil
	}

	// The log never names an invoice and its gift order together: the 30-day
	// unlink forgets which order an invoice paid for (model.UnlinkCryptoPaymentIntents),
	// and a log line must not remember it
	glog.Infof(
		"[gift]invoice %s (%s) started for a gift order: %d gifts, %d cents\n",
		intentId, asset, order.Quantity, order.TotalCents,
	)
	return &GiftCryptoIntentResult{
		CryptoPaymentIntentResult: *invoice,
		OrderId:                   order.GiftOrderId.String(),
		OrderToken:                order.OrderToken,
		BittensorFooter:           order.BittensorFooter,
	}, nil
}

// cryptoGiftInvoice reports a gift order's invoice.
func cryptoGiftInvoice(intent *model.CryptoPaymentIntent) bool {
	return intent.Item == giftCryptoItem
}

// cryptoCreditGiftIntentInConn is the crypto rail's credit of a gift order's
// invoice (cryptoCreditIntentInConn hands it here): one ReadCommitted
// OptNoRetry transaction, on the caller's PostgreSQL session when it has one
// (nil selects the outer acquisition boundary). No retry: a credit again
// restores redeemed codes' years, whose balances are admitted to the
// transaction (model.RestoreCryptoGiftOrderInTx), which only such a
// transaction may do; a failed attempt is retried by the scan or the
// operator, as any failed credit is.
func cryptoCreditGiftIntentInConn(
	connOwner server.PgConn,
	clientSession *session.ClientSession,
	intent *model.CryptoPaymentIntent,
	credit *cryptoCredit,
) (credited bool, returnErr error) {
	ctx := clientSession.Ctx
	if cryptoAssetMarket(intent.Asset) == "" {
		return false, fmt.Errorf("invoice %s has an unknown asset", intent.IntentId)
	}
	if intent.Asset == model.CryptoAssetUsdcSolana && (intent.SolanaReference == nil || credit.solanaSignature == "") {
		return false, fmt.Errorf("USDC invoice %s needs its Solana reference and the paying signature", intent.IntentId)
	}
	// read before the transaction: the link is written before the invoice is
	// handed out and never changes
	giftOrderId := model.GetGiftOrderIdForCryptoIntent(ctx, intent.IntentId)
	if giftOrderId == nil {
		// the invoice stays open, and the credit failures put it in review
		return false, fmt.Errorf("gift invoice %s has no gift order", intent.IntentId)
	}

	now := server.NowUtc()
	var outcome *cryptoGiftCredit
	server.HandleError(func() {
		server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
			outcome = cryptoCreditGiftIntentInTx(tx, clientSession, intent, credit, *giftOrderId, now)
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) {
		outcome = nil
		returnErr = err
	})
	if returnErr != nil {
		return false, returnErr
	}
	if outcome == nil {
		return false, nil
	}
	for _, networkId := range outcome.restoredNetworkIds {
		// a redeemed code's year came back
		model.UpdateProNetworkInConn(connOwner, ctx, networkId)
	}
	cryptoPayCredits.WithLabelValues(intent.Asset, intent.Delivery, cryptoCreditTiming(credit.late)).Inc()
	// never the gift order's id beside the invoice's (GiftCryptoIntent)
	if outcome.restored {
		glog.Infof("[cryptopay]credited gift invoice %s (%s) again: %d codes and %d years restored\n", intent.IntentId, intent.Asset, outcome.restoredCodes, outcome.restoredYears)
	} else {
		glog.Infof("[cryptopay]credited gift invoice %s (%s): %d codes\n", intent.IntentId, intent.Asset, outcome.codeCount)
	}
	return true, nil
}

// What a gift invoice's credit did.
type cryptoGiftCredit struct {
	codeCount int
	// credited again: what the orphan leg took was restored
	restored           bool
	restoredCodes      int
	restoredYears      int
	restoredNetworkIds []server.Id
}

// cryptoCreditGiftIntentInTx is one attempt of a gift invoice's credit in the
// caller's transaction: the invoice one-shot, then the order. Past the
// one-shot every failure raises, so the transaction rolls back rather than
// commit a credited invoice without its codes.
//
// An invoice is credited again after its payment left the chain and came
// back: the reconcile's orphan leg ended the first credit, voiding the order's
// unredeemed codes and ending its redeemed codes' years, and took the credit
// back (the order stays paid). That credit restores exactly what the orphan
// leg took (model.RestoreCryptoGiftOrderInTx), the same codes as on the
// order's cards, and owes again the friends' emails withdrawn meanwhile. A
// refunded order is never fulfilled again: the credit fails, its codes stay
// void, and the payment is left to an operator (after the credit's failures,
// in review). nil when another credit won the one-shot.
func cryptoCreditGiftIntentInTx(
	tx server.PgTx,
	clientSession *session.ClientSession,
	intent *model.CryptoPaymentIntent,
	credit *cryptoCredit,
	giftOrderId server.Id,
	now time.Time,
) *cryptoGiftCredit {
	ctx := clientSession.Ctx
	// the invoice one-shot first: when it loses, nothing was written
	if !cryptoClaimIntentInTx(tx, clientSession, intent, credit, now) {
		return nil
	}
	outcome := &cryptoGiftCredit{}
	codes, newlyPaid, err := model.FulfillGiftOrderInTx(
		tx,
		ctx,
		giftOrderId,
		model.CryptoPaymentPurchaseEventId(intent.IntentId),
		credit.usdNanoCents,
		now,
	)
	// the errors name the invoice, never its order beside it
	// (GiftCryptoIntent)
	if errors.Is(err, model.ErrGiftOrderRefunded) {
		server.Raise(fmt.Errorf("the gift order of invoice %s was refunded, so its codes stay void; return this payment by hand: %w", intent.IntentId, err))
	}
	if err != nil {
		server.Raise(fmt.Errorf("could not fulfil the gift order of invoice %s: %s", intent.IntentId, giftWithoutOrderId(err, giftOrderId)))
	}
	if len(codes) == 0 {
		server.Raise(fmt.Errorf("the gift order of invoice %s has no codes after its fulfilment", intent.IntentId))
	}
	if newlyPaid {
		// the cards and the emails, committed with the codes or not at all
		if err := ScheduleGiftDeliveryInTx(tx, ctx, giftOrderId); err != nil {
			server.Raise(fmt.Errorf("could not schedule the delivery of the gift order of invoice %s: %s", intent.IntentId, giftWithoutOrderId(err, giftOrderId)))
		}
	} else if restoration := model.RestoreCryptoGiftOrderInTx(tx, ctx, giftOrderId); restoration != nil && 0 < len(restoration.Codes) {
		// credited again: the codes the orphan leg voided, and the redeemed
		// codes' years it ended, are good again, and the friends' emails
		// withdrawn while they were void are owed again
		outcome.restored = true
		outcome.restoredCodes, outcome.restoredYears = giftRestorationCounts(restoration)
		outcome.restoredNetworkIds = restoration.NetworkIds
		restoreKey := fmt.Sprintf("orphan/%d", restoration.Codes[0].VoidTime.UnixMicro())
		giftReoweFriendEmailsInTx(ctx, tx, restoration, restoreKey, now)
	}
	// the invoice's grant, the order's first code, as the invoice's status
	// reads it; a refund or an orphaned payment finds the order by the
	// invoice instead (model.RefundCryptoGiftOrderInTx,
	// model.EndCryptoPaymentGrantInTx)
	firstCodeId := codes[0].BalanceCodeId
	model.SetCryptoPaymentIntentGrantInTx(tx, ctx, intent.IntentId, nil, &firstCodeId)
	cryptoCreditReceiptsInTx(tx, ctx, intent, credit)
	// this call won the invoice one-shot: a new payment, posted once the
	// transaction commits
	quantity := len(codes)
	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		notice := cryptoSalesNotice(intent, credit, false, 0)
		notice.Item = giftSalesItem(quantity)
		return notice
	})
	outcome.codeCount = quantity
	return outcome
}

// giftWithoutOrderId is an error's text with the gift order's id taken out,
// for an error that also names the order's invoice.
func giftWithoutOrderId(err error, giftOrderId server.Id) string {
	return strings.ReplaceAll(err.Error(), giftOrderId.String(), "<gift order>")
}

// giftOrderIdForCryptoRefund is the gift order of a gift invoice an operator
// refunds, else nil. Read before the refund's transaction.
func giftOrderIdForCryptoRefund(clientSession *session.ClientSession, intent *model.CryptoPaymentIntent) *server.Id {
	if !cryptoGiftInvoice(intent) {
		return nil
	}
	return model.GetGiftOrderIdForCryptoIntent(clientSession.Ctx, intent.IntentId)
}
