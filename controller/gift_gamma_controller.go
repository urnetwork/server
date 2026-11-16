package controller

// Paying for a gift order with Gamma (mmm/GIFTOFPRIVACY.md §10.2, mmm/BTCPAY.md
// §18; the owner, 2026-11-14: "yes add gamma for gift payments"): POST
// /pay/gift/gamma-intent, and the gift branch of the Gamma biller's credit.
//
// A gift order's Gamma intent is a purchase like any other the caller's
// network makes with Gamma (gamma_controller.go): paid from the network's
// deposit hotkey, its own 32-byte reference, charged by the biller like any
// purchase (gamma_biller.go) and credited only on our finalized GammaSpent. It
// is authenticated as POST /pay/gamma/intent is, by a network JWT or an API
// key, and a guest network may buy, since the codes are the delivery. Its item
// is gift_pro_year and its delivery code, as a coin's gift invoice: no network
// is credited. It is priced at the order's total by the gift's quantity tiers,
// with no price tier and no welcome offer, one gamma per dollar, and capped by
// gift.crypto_max_invoice_usd ($500), the cap of a gift order paid in a coin or
// with Gamma. It is open for at most two hours (giftGammaIntentMaxTtl). Each
// order has an intent of its own: an open intent is never handed out again,
// for this order or another. Its status is POST
// /pay/gamma/status's, with the intent's status token; the order is read with
// POST /pay/gift/order and its token.
//
// The order is stored first, under the buyer's daily friend emails, and the
// intent is recorded with the order's link in one transaction
// (model.SetGiftOrderGammaIntentIdInTx): a deposit that already holds the
// price is charged on the biller's next run, so an intent is never recorded
// without its order. A refusal after the order is stored removes it, never
// started.
//
// An account has one open gift purchase at a time, since all of them draw on
// its one deposit hotkey: a buyer who changes the gift (a new order) or leaves
// it, and mints later, must never be charged for the purchase left behind, or
// twice. The same transaction voids the account's other open gift purchases
// (model.VoidOpenGammaGiftIntentsInTx; never a plan or a data purchase), and
// POST /pay/gift/gamma-cancel voids an order's (GiftGammaCancel), for "pay
// another way". Neither voids a purchase whose spend is in flight (in a spend
// batch that can still land, or spent and not yet credited): a new purchase is
// refused meanwhile, and so is the cancel (payment_in_progress). A voided
// purchase is never charged, and its order, which stays pending, no longer
// counts against the buyer's daily friend emails.
//
// The credit (gammaCreditIntent hands a gift intent here) wins the intent's
// one-shot, then, in the same transaction, fulfils the order with the purchase
// event base gamma/<intent id> (codes gamma/<intent id>/gift/<n>) and the price
// as the codes' revenue, schedules the delivery, and posts the sales notice.
// It records no grant on the intent, so the intent's status never shows a
// code (the order's cards carry them), and nothing of the intent names a code
// once the 30-day unlink has passed the order (model.UnlinkGammaGiftOrders). A
// gift never enrolls the account in Gamma's automatic charges: no renewal,
// top-up or seat upgrade is recorded for it, and no gamma_receipt is sent; the
// gift's own buyer email is the receipt.
//
// A gift paid with Gamma is final sale (the owner, 2026-11-14): Gamma cannot be
// refunded, so the refund and dispute paths refuse or skip it
// (model.ErrGiftOrderFinalSale), and, as for a coin, its friends' emails go out
// with no card hold.
//
// Deploy order: apply migration 977 (gift_gamma_schema.go) on Main, deploy the
// taskworker, then the api and alt. An older biller charges a gift intent as
// it charges any open intent, and its credit then fails on gift_pro_year (no
// data pack), leaving a final-sale payment uncredited until this taskworker
// retries it from review. For the same reason, never roll the taskworker back
// past this build once a Gamma gift has been sold.

import (
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// GiftGammaItem is the intent item of a gift order paid with Gamma: the same
// item as a coin's gift invoice. The monitor tells gift intents apart by it.
const GiftGammaItem = model.GammaGiftItem

// The refusal of a new gift purchase, and of a cancel, while an earlier gift
// purchase's Gamma spend is in flight: it can still land, so it cannot be
// voided, and a new purchase beside it could be charged as well.
const GiftErrorCodePaymentInProgress = "payment_in_progress"

const giftGammaPaymentInProgressMessage = "A Gamma payment for an earlier gift is still being processed. Try again in a few minutes."

// giftGammaIntentMaxTtl is the longest a gift order's Gamma purchase stays
// open: min(gamma.yml's intent_ttl, which every other purchase keeps, a day by
// default; this). Every purchase of an account draws on its one deposit
// hotkey, and the biller charges an account's open purchases oldest first
// (model.ListChargeableGammaPaymentIntents). So a buyer who closed the gift
// dialog and paid by card, then minted for a plan within the day, would have
// the abandoned gift charged first, as a final sale. Two hours is ample to
// mint, which lands in a block. Gamma is priced in US dollars (amount_gamma is
// the order's total), so a short life carries no price risk; an expired
// purchase is never charged, and unspent gamma stays in the deposit.
const giftGammaIntentMaxTtl = 2 * time.Hour

// GiftGammaIntentArgs is POST /pay/gift/gamma-intent: the order's fields, the
// Bittensor footer, and the coldkey that will mint.
type GiftGammaIntentArgs struct {
	GiftPurchaseArgs
	// "Made with Bittensor" on the cards and the friend emails
	BittensorFooter bool `json:"bittensor_footer,omitempty"`
	// the SS58 coldkey that will mint, as POST /pay/gamma/intent takes it:
	// picks a deposit hotkey whose deposit that coldkey can open or already
	// owns
	Coldkey string `json:"coldkey,omitempty"`
}

// GiftGammaIntentResult is POST /pay/gift/gamma-intent: the intent as POST
// /pay/gamma/intent answers it, with item_id gift_pro_year and delivery code,
// plus the order. Its automatic_notice is empty and its
// auto_min_daily_cap_gamma is its min_daily_cap_gamma: a gift starts no
// automatic charge.
type GiftGammaIntentResult struct {
	GammaPaymentIntentResult
	// the order, and its token: the bearer secret that reads the order's
	// status and downloads its cards
	OrderId    string `json:"order_id,omitempty"`
	OrderToken string `json:"order_token,omitempty"`
	// the request's bittensor_footer, as the order stores it
	BittensorFooter bool `json:"bittensor_footer"`
}

func giftGammaIntentError(code string, message string) *GiftGammaIntentResult {
	return giftGammaIntentRefusal(gammaIntentError(code, message))
}

// A refusal of the shared Gamma checks, as the gift route answers it.
func giftGammaIntentRefusal(refusal *GammaPaymentIntentResult) *GiftGammaIntentResult {
	return &GiftGammaIntentResult{GammaPaymentIntentResult: *refusal}
}

// GiftGammaIntent answers POST /pay/gift/gamma-intent: a Pay with Gamma intent
// for a new gift order, from the caller's network's deposit hotkey. Every
// answer counts in urnetwork_gift_purchases_total (gift_purchase_metrics.go).
func GiftGammaIntent(
	args *GiftGammaIntentArgs,
	clientSession *session.ClientSession,
) (*GiftGammaIntentResult, error) {
	result, err := giftGammaIntent(args, clientSession)
	refused, code := false, ""
	if result != nil && result.Error != nil {
		refused, code = true, result.Error.Code
	}
	countGiftPurchase(giftPurchaseRailGamma, err, result != nil, refused, code)
	return result, err
}

// giftGammaIntent is GiftGammaIntent's answer, before it is counted.
func giftGammaIntent(
	args *GiftGammaIntentArgs,
	clientSession *session.ClientSession,
) (*GiftGammaIntentResult, error) {
	giftConfig := model.Gift()
	if !giftConfig.Enabled {
		return giftGammaIntentError(GiftErrorCodeSalesClosed, giftSalesClosedMessage), nil
	}
	order, errCode, errMessage := giftValidatePurchase(&args.GiftPurchaseArgs, giftConfig, server.NowUtc())
	if errMessage != "" {
		return giftGammaIntentError(errCode, errMessage), nil
	}
	coldkey, ok := gammaColdkeyArg(args.Coldkey)
	if !ok {
		return giftGammaIntentError(PurchaseErrorCodeInvalidRequest, gammaInvalidColdkeyMessage), nil
	}

	view, refusal := gammaPurchaseView(clientSession)
	if refusal != nil {
		return giftGammaIntentRefusal(refusal), nil
	}
	// the cap of a gift order paid in a coin or with Gamma
	priceUsd := float64(order.TotalCents) / 100.0
	if giftConfig.CryptoMaxInvoiceUsd <= 0 || giftConfig.CryptoMaxInvoiceUsd < priceUsd {
		return giftGammaIntentError(PurchaseErrorCodeItemUnavailable, "This gift order cannot be paid with Gamma now."), nil
	}

	if !giftPurchaseLimiter.allow(clientSession) {
		return giftGammaIntentError(
			PurchaseErrorCodeRateLimited,
			"Too many payment attempts from this address. Try again in a minute.",
		), nil
	}
	networkId := clientSession.ByJwt.NetworkId
	// Gamma's own per-network limit, as POST /pay/gamma/intent's
	if !allowNetworkIntent(networkId) {
		return giftGammaIntentError(PurchaseErrorCodeRateLimited, "Too many payment attempts. Try again in a minute."), nil
	}

	ctx := clientSession.Ctx
	// an earlier gift's spend in flight can still land: refused before
	// anything is stored, and again under the void's locks
	if model.HasGammaGiftChargeInFlight(ctx, networkId) {
		return giftGammaIntentError(GiftErrorCodePaymentInProgress, giftGammaPaymentInProgressMessage), nil
	}
	// the buyer's daily friend emails, before anything is stored and after the
	// order is (gift_rails_controller.go), without the orders of the open gift
	// purchases this one voids
	if errCode, errMessage := giftFriendEmailLimitRefusalReplacing(ctx, order, giftConfig, server.NowUtc(), &networkId); errMessage != "" {
		return giftGammaIntentError(errCode, errMessage), nil
	}

	// the deposit hotkey, minting and the open purchases' limit, as every
	// Gamma purchase, before the order is stored: a refusal here stores
	// nothing. A gift order's intent delivers codes, so no open intent is
	// handed out again.
	start, refusal := gammaStartPurchase(clientSession, view, &gammaPurchase{
		item:       GiftGammaItem,
		delivery:   model.CryptoDeliveryCode,
		priceUsd:   priceUsd,
		regularUsd: priceUsd,
		coldkey:    coldkey,
		maxTtl:     giftGammaIntentMaxTtl,
	})
	if refusal != nil {
		return giftGammaIntentRefusal(refusal), nil
	}

	order.Rail = model.GiftRailGamma
	order.BittensorFooter = args.BittensorFooter
	order.ReferrerNetworkId = giftReferrerNetworkId(&args.GiftPurchaseArgs, clientSession)
	if err := model.CreateGiftOrder(ctx, order); err != nil {
		glog.Errorf("[gift]could not store a gift order paid with gamma: %s\n", err)
		return giftGammaIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage), nil
	}
	// refused here, the order is removed and no intent is ever recorded
	if errCode, errMessage := giftKeepFriendEmailLimitReplacing(ctx, order, giftConfig, server.NowUtc(), &networkId); errMessage != "" {
		return giftGammaIntentError(errCode, errMessage), nil
	}

	// the account's other open gift purchases voided, the intent and the
	// order's link to it, committed together or not at all
	result := start.issue(clientSession, func(tx server.PgTx, intent *model.GammaPaymentIntent) (*GammaPaymentIntentResult, error) {
		voided, inFlight := model.VoidOpenGammaGiftIntentsInTx(tx, ctx, networkId, intent.CreateTime)
		if inFlight {
			return gammaIntentError(GiftErrorCodePaymentInProgress, giftGammaPaymentInProgressMessage), nil
		}
		for _, voidedIntentId := range voided {
			glog.Infof("[gift]gamma intent %s voided: a newer gift purchase replaces it\n", voidedIntentId)
		}
		return nil, model.SetGiftOrderGammaIntentIdInTx(tx, ctx, order.GiftOrderId, intent.IntentId)
	})
	if result.Error != nil {
		// no intent was recorded, so the order was never started
		if !model.RemoveUnstartedGiftOrder(ctx, order.GiftOrderId) {
			glog.Errorf("[gift]gift order %s paid with gamma was never started and could not be removed\n", order.GiftOrderId)
		}
		return giftGammaIntentRefusal(result), nil
	}
	// a gift starts no automatic charge: the deposit needs a cap for this
	// purchase alone, and there is nothing automatic to describe
	result.AutomaticNotice = ""
	result.AutoMinDailyCapGamma = result.MinDailyCapGamma

	// The log never names an intent and its gift order together: the 30-day
	// unlink forgets which order an intent paid for
	// (model.UnlinkGammaGiftOrders), and a log line must not remember it
	glog.Infof(
		"[gift]gamma intent %s started for a gift order: %d gifts, %d cents\n",
		result.IntentId, order.Quantity, order.TotalCents,
	)
	return &GiftGammaIntentResult{
		GammaPaymentIntentResult: *result,
		OrderId:                  order.GiftOrderId.String(),
		OrderToken:               order.OrderToken,
		BittensorFooter:          order.BittensorFooter,
	}, nil
}

// GiftGammaCancelResult is POST /pay/gift/gamma-cancel.
type GiftGammaCancelResult struct {
	// the order's Gamma purchase is voided and can never be charged: now, or
	// before (by a newer gift purchase or an earlier cancel), or the order has
	// none left that can be
	Cancelled bool `json:"cancelled"`
	// the order's status, as POST /pay/gift/order answers it: pending | paid |
	// refunded. A cancelled order stays pending. "" when no order matches the
	// id and token.
	Status string     `json:"status"`
	Error  *GiftError `json:"error,omitempty"`
}

// The cancel's own limit, at the order status's (giftReadLimiter's) rate, with
// counts of its own: a dialog polling the order never uses up its cancel.
var giftGammaCancelLimiter = &payDataIpLimiter{limitPerMinute: payDataLookupIpLimitPerMinute}

// GiftGammaCancel answers POST /pay/gift/gamma-cancel {order_id, token}: the
// order's Gamma purchase is voided, unless its spend is in flight
// (payment_in_progress) or the order is paid (cancelled false, with the paid
// status). The order's token is the credential, as for POST /pay/gift/order;
// an order it does not open, and an order not paid with Gamma, are
// invalid_request. Rate limited per address as POST /pay/gift/order is.
func GiftGammaCancel(
	args *GiftOrderStatusArgs,
	clientSession *session.ClientSession,
) (*GiftGammaCancelResult, error) {
	if !giftGammaCancelLimiter.allow(clientSession) {
		return nil, errGiftReadRateLimited
	}
	order := giftOrderForToken(clientSession, args)
	if order == nil {
		return &GiftGammaCancelResult{Error: &GiftError{
			Code:    PurchaseErrorCodeInvalidRequest,
			Message: "No gift order matches this id and token.",
		}}, nil
	}
	if order.Rail != model.GiftRailGamma {
		return &GiftGammaCancelResult{
			Status: order.Status,
			Error: &GiftError{
				Code:    PurchaseErrorCodeInvalidRequest,
				Message: "This gift order is not paid with Gamma.",
			},
		}, nil
	}
	if order.Status != model.GiftOrderStatusPending || order.GammaIntentId == nil {
		// paid (or refunded): nothing to cancel. Pending with no purchase
		// left (the 30-day unlink passed one that could no longer be
		// charged): nothing can charge it
		return &GiftGammaCancelResult{
			Cancelled: order.Status == model.GiftOrderStatusPending,
			Status:    order.Status,
		}, nil
	}

	ctx := clientSession.Ctx
	switch outcome := model.CancelGammaGiftIntent(ctx, *order.GammaIntentId, gammaNow()); outcome {
	case model.GammaGiftCancelVoided:
		// never the order's id beside the intent's (GiftGammaIntent)
		glog.Infof("[gift]gamma intent %s voided: its order was cancelled\n", *order.GammaIntentId)
		return &GiftGammaCancelResult{Cancelled: true, Status: order.Status}, nil
	case model.GammaGiftCancelClosed:
		return &GiftGammaCancelResult{Cancelled: true, Status: order.Status}, nil
	case model.GammaGiftCancelInFlight:
		return &GiftGammaCancelResult{
			Status: order.Status,
			Error: &GiftError{
				Code:    GiftErrorCodePaymentInProgress,
				Message: "This gift's Gamma payment is being processed, so it can't be cancelled. Check the order again in a few minutes.",
			},
		}, nil
	default:
		// credited since the order was read: paid now
		status := order.Status
		if read := model.GetGiftOrder(ctx, order.GiftOrderId); read != nil {
			status = read.Status
		}
		return &GiftGammaCancelResult{Status: status}, nil
	}
}

// gammaGiftIntent reports a gift order's intent.
func gammaGiftIntent(intent *model.GammaPaymentIntent) bool {
	return intent.Item == GiftGammaItem
}

// gammaCreditGiftIntent is the Gamma biller's credit of a gift order's intent
// from its spent charge (gammaCreditIntent hands it here): one ReadCommitted
// transaction (gammaCreditGiftIntentInTx). credited = false when another
// credit got there first. An error leaves the intent uncredited, and the
// biller's credit failures hold it for review: no order, or a price that is
// not the order's total, credits nothing.
func gammaCreditGiftIntent(clientSession *session.ClientSession, intent *model.GammaPaymentIntent) (credited bool, returnErr error) {
	ctx := clientSession.Ctx
	// read before the transaction: the link is recorded with the intent and
	// never changes before the 30-day unlink, which never passes an intent
	// whose spend is not credited
	giftOrderId := model.GetGiftOrderIdForGammaIntent(ctx, intent.IntentId)
	if giftOrderId == nil {
		return false, fmt.Errorf("gift intent %s has no gift order", intent.IntentId)
	}
	order := model.GetGiftOrder(ctx, *giftOrderId)
	if order == nil {
		return false, fmt.Errorf("the gift order of intent %s was not found", intent.IntentId)
	}
	// the spend took the intent's price, which was the order's total when both
	// were made; a price that is anything else fulfils nothing
	if order.Rail != model.GiftRailGamma || model.UsdToNanoCents(float64(order.TotalCents)/100.0) != intent.PriceUsdNanoCents {
		return false, fmt.Errorf("intent %s spent %s gamma, which is not the total of its gift order paid with gamma", intent.IntentId, gammaFormatUnits(intent.AmountGamma()))
	}

	now := gammaNow()
	codeCount := 0
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			credited, codeCount = gammaCreditGiftIntentInTx(tx, clientSession, intent, *giftOrderId, now)
		}, server.TxReadCommitted)
	}, func(err error) {
		credited = false
		// the biller logs it beside the intent's id
		returnErr = giftHideOrderId(err, *giftOrderId)
	})
	if returnErr != nil || !credited {
		return false, returnErr
	}
	gammaCredits.WithLabelValues(intent.Purpose, intent.Delivery).Inc()
	// never the gift order's id beside the intent's (GiftGammaIntent)
	glog.Infof("[gamma]credited gift intent %s: %d codes\n", intent.IntentId, codeCount)
	return true, nil
}

// gammaCreditGiftIntentInTx is one attempt of a gift intent's credit in the
// caller's transaction: the intent's one-shot, then the order's codes, its
// delivery and the sales notice. Past the one-shot every failure raises, so
// the transaction rolls back rather than commit a credited intent without its
// codes. Nothing about the account's Gamma payments is recorded: no renewal
// plan, no automatic switches, no receipt. (false, 0) when another credit won
// the one-shot.
func gammaCreditGiftIntentInTx(
	tx server.PgTx,
	clientSession *session.ClientSession,
	intent *model.GammaPaymentIntent,
	giftOrderId server.Id,
	now time.Time,
) (credited bool, codeCount int) {
	ctx := clientSession.Ctx
	// the intent's one-shot first: when it loses, nothing was written
	if !model.MarkGammaPaymentIntentCreditedInTx(tx, ctx, intent.IntentId, now) {
		return false, 0
	}
	codes, newlyPaid, err := model.FulfillGiftOrderInTx(
		tx,
		ctx,
		giftOrderId,
		model.GammaPaymentPurchaseEventId(intent.IntentId),
		intent.PriceUsdNanoCents,
		now,
	)
	// the errors name the intent, never its order beside it (GiftGammaIntent)
	if errors.Is(err, model.ErrGiftOrderRefunded) {
		// final sale: nothing refunds a Gamma order, so this must not happen
		server.Raise(fmt.Errorf("the gift order of intent %s was refunded, so its codes stay void; settle this payment by hand: %w", intent.IntentId, err))
	}
	if err != nil {
		server.Raise(fmt.Errorf("could not fulfil the gift order of intent %s: %s", intent.IntentId, giftWithoutOrderId(err, giftOrderId)))
	}
	if len(codes) == 0 {
		server.Raise(fmt.Errorf("the gift order of intent %s has no codes after its fulfilment", intent.IntentId))
	}
	if newlyPaid {
		// the cards and the emails, committed with the codes or not at all
		if err := ScheduleGiftDeliveryInTx(tx, ctx, giftOrderId); err != nil {
			server.Raise(fmt.Errorf("could not schedule the delivery of the gift order of intent %s: %s", intent.IntentId, giftWithoutOrderId(err, giftOrderId)))
		}
	}
	// no grant is recorded on the intent: its status never shows a code, and
	// nothing of it names one once the order is unlinked
	quantity := len(codes)
	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		return giftGammaSalesNotice(intent, quantity)
	})
	return true, quantity
}

// giftGammaSalesNotice is the sales notice of a gift order paid with Gamma. A
// gift's payment is for no network, as on the other gift rails: its codes are
// redeemed later, wherever the friends choose.
func giftGammaSalesNotice(intent *model.GammaPaymentIntent, quantity int) *model.SalesPaymentNotice {
	return &model.SalesPaymentNotice{
		RecordKey: "gamma_intent/" + intent.IntentId.String(),
		Item:      giftSalesItem(quantity),
		Kind:      model.SalesPaymentKindOneTime,
		Amount:    fmt.Sprintf("%s gamma (burned on SN25, credited as %s)", gammaFormatUnits(intent.AmountGamma()), salesNanoCentsUsd(intent.PriceUsdNanoCents)),
		Method:    "SN25 Gamma",
	}
}
