package controller

// The compute branch of the Stripe webhook (DESIGN-COMPUTE §9.5, §9.11, §9.12, CF-4, D-BAL-10; BAL-M12-08):
// ComputeStripeWebhook, which StripeWebhook asks first, takes the events of compute's own purchases, told apart by
// the metadata compute's checkout writes (urnetwork_compute) and checked against the compute product's price before
// any capture, never by amount; every other event goes on to Pro's handlers as before. So a compute event never
// reaches a Pro handler and grants no Pro, and Pro's events never credit compute.
//
//	checkout.session.*                     a compute session: acknowledged; compute credits by its PaymentIntent
//	payment_intent.amount_capturable_updated
//	                                       the card authorized and held: its fingerprint read, its payer's keyed hash
//	                                       made and the payer's limits and the network's run (ComputePurchaseCheck),
//	                                       then the intent captured, and credited at once, or cancelled uncaptured,
//	                                       a card it saved detached. Capture and cancel go to Stripe under keys of the
//	                                       intent's id, so a repeat is the same call; an intent already decided is
//	                                       left as it stands.
//	payment_intent.succeeded               the money final: the lot credited (kind purchase, market stripe, market_ref
//	                                       the PaymentIntent's id) through ComputeCreditLot, whose (market, market_ref)
//	                                       key credits a replay once; the compute terms recorded with the purchase;
//	                                       the card a threshold checkout saved handed to auto-renew. A deleted
//	                                       network's closed account credits nothing, and the payment is refunded.
//	payment_intent.canceled                a card the intent saved detached
//	charge.refunded                        on a compute intent: a refund the ledger made itself (its metadata's
//	                                       compute_refund) books nothing more; one made outside it reverses the lot
//	                                       when the charge is refunded in full, and alarms when in part
//	charge.dispute.created, .funds_withdrawn
//	                                       a chargeback: the lot reversed, the network's compute suspended and its
//	                                       promotions lapsed (ComputeReverseLot); an inquiry takes no funds and
//	                                       reverses nothing
//	charge.dispute.closed                  a dispute won alarms for a person's review, who restores what it took
//	refund.created, refund.updated, charge.refund.updated, charge.dispute.updated, charge.dispute.funds_reinstated
//	                                       on a compute intent: acknowledged, charge.refunded and the dispute events
//	                                       doing the work
//
// A charge, refund or dispute names its PaymentIntent, not always compute's metadata, so where its own metadata does
// not mark it the intent is compute's when the ledger credited a lot for it, or when the intent's own metadata, read
// from Stripe, marks it; a charge on an invoice is Pro's (compute's packs make none). A process where compute is not
// active (ComputeStripeDeps.Active: its routes unmounted, or a binary without MG-C1) judges by metadata alone, reading
// no ledger table and asking Stripe nothing for an event that is not compute's, so Pro's paths run as they did; it
// leaves a compute authorization uncaptured, and answers an error for compute money it cannot credit, so Stripe
// retries.
//
// What a purchase cannot become is answered 200 and alarmed, since a retry does no better: a compute object whose
// metadata no checkout of ours wrote, a price that is not the compute product's, a payment with no card. A failure of
// Stripe or the ledger answers an error, and Stripe retries.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/glog"
	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server/session"
)

// The alarms compute's Stripe branch raises, by kind: identifiers.
const (
	// An authorization refused at capture: its payer or network past a limit, no card, or no key to hash it with.
	ComputeAlarmPurchaseRefused = "purchase_refused"
	// A compute object no checkout of ours made: its metadata, amount or line is not compute's.
	ComputeAlarmPurchaseMismatch = "purchase_mismatch"
	// A compute charge refunded in part outside the ledger, which a person settles with an adjustment.
	ComputeAlarmPurchaseRefundPartial = "purchase_refund_partial"
	// A compute dispute won: what its chargeback reversed waits for a person's review.
	ComputeAlarmPurchaseDisputeWon = "purchase_dispute_won"
	// A threshold checkout saved a card auto-renew could not take: detached.
	ComputeAlarmPurchaseCardDetached = "purchase_card_detached"
	// A compute payment for a deleted network: refunded.
	ComputeAlarmPurchaseClosedRefunded = "purchase_closed_refunded"
)

// A compute event's object as the branch reads it: the fields of a session, an intent, a charge, a refund or a
// dispute it judges by, each absent where the object has none.
type computeStripeEventObject struct {
	Id             string            `json:"id"`
	Metadata       map[string]string `json:"metadata"`
	PaymentIntent  json.RawMessage   `json:"payment_intent"`
	Invoice        json.RawMessage   `json:"invoice"`
	Charge         json.RawMessage   `json:"charge"`
	Amount         int64             `json:"amount"`
	AmountRefunded int64             `json:"amount_refunded"`
	Status         string            `json:"status"`
}

// What the process's webhook asks first: compute's branch (see the file header). handled false sends the event on to
// Pro's handlers untouched.
func ComputeStripeWebhook(event *StripeWebhookArgs, clientSession *session.ClientSession) (handled bool, result *StripeWebhookResult, returnErr error) {
	return computeStripeProcess().Webhook(clientSession.Ctx, event)
}

// The branch over this instance (see the file header).
func (self *ComputeServerStripe) Webhook(ctx context.Context, event *StripeWebhookArgs) (bool, *StripeWebhookResult, error) {
	if event == nil || event.Data == nil || len(event.Data.Object) == 0 {
		return false, nil, nil
	}
	object := &computeStripeEventObject{}
	if err := json.Unmarshal(event.Data.Object, object); err != nil {
		// not compute's to judge: Pro's handlers parse it and answer as they do
		return false, nil, nil
	}
	answer := func(err error) (bool, *StripeWebhookResult, error) {
		if err != nil {
			return true, nil, err
		}
		return true, &StripeWebhookResult{}, nil
	}
	switch event.Type {
	case "checkout.session.completed", "checkout.session.expired", "checkout.session.async_payment_succeeded", "checkout.session.async_payment_failed":
		if !computeStripeMarked(object.Metadata) {
			return false, nil, nil
		}
		return answer(nil)
	case "payment_intent.amount_capturable_updated":
		if !computeStripeMarked(object.Metadata) {
			return false, nil, nil
		}
		return answer(self.authorized(ctx, object.Id))
	case "payment_intent.succeeded":
		if !computeStripeMarked(object.Metadata) {
			return false, nil, nil
		}
		return answer(self.succeeded(ctx, object.Id))
	case "payment_intent.canceled":
		if !computeStripeMarked(object.Metadata) {
			return false, nil, nil
		}
		return answer(self.canceled(ctx, object.Id))
	case "charge.refunded", "charge.dispute.created", "charge.dispute.funds_withdrawn", "charge.dispute.closed",
		"refund.created", "refund.updated", "charge.refund.updated", "charge.dispute.updated", "charge.dispute.funds_reinstated":
		intentId := stripeIdFromRaw(object.PaymentIntent)
		compute, err := self.intentCompute(ctx, object.Metadata, intentId, stripeIdFromRaw(object.Invoice))
		if err != nil {
			// whose event this is cannot be told now: Stripe retries, Pro's handlers included
			return answer(err)
		}
		if !compute {
			return false, nil, nil
		}
		switch event.Type {
		case "charge.refunded":
			return answer(self.refunded(ctx, object, intentId))
		case "charge.dispute.created", "charge.dispute.funds_withdrawn":
			return answer(self.disputed(ctx, object, intentId))
		case "charge.dispute.closed":
			if object.Status == stripeDisputeStatusWon {
				sidecarcontroller.ComputeRaiseAlarm(self.deps.Env(), ComputeAlarmPurchaseDisputeWon)
			}
		}
		return answer(nil)
	}
	return false, nil, nil
}

// Whether the PaymentIntent intentId of a charge, refund or dispute is compute's (see the file header).
func (self *ComputeServerStripe) intentCompute(ctx context.Context, metadata map[string]string, intentId string, invoiceId string) (bool, error) {
	if computeStripeMarked(metadata) {
		return true, nil
	}
	if intentId == "" || invoiceId != "" || !self.deps.Active() {
		return false, nil
	}
	lot, err := self.lotOf(ctx, intentId)
	if err != nil {
		return false, err
	}
	if lot != nil {
		return true, nil
	}
	client, err := self.client()
	if err != nil {
		return false, err
	}
	intent, err := client.V1PaymentIntents.Retrieve(ctx, intentId, nil)
	if err != nil {
		return false, fmt.Errorf("compute stripe: the intent %s: %w", intentId, err)
	}
	return computeStripeMarked(intent.Metadata), nil
}

// The ledger's Stripe lot of intentId, nil where none was credited.
func (self *ComputeServerStripe) lotOf(ctx context.Context, intentId string) (*model.ComputeLot, error) {
	env := self.deps.Env()
	db, err := env.Db()
	if err != nil {
		return nil, err
	}
	var lot *model.ComputeLot
	err = db.Tx(ctx, func(tx operator.DbTx) error {
		var err error
		lot, err = model.ComputeLotByMarketRefInTx(env, ctx, tx, model.ComputeMarketStripe, intentId, false)
		return err
	}, operator.DbIsolationReadCommitted, operator.DbReadOnly)
	return lot, err
}

// Compute is not active in this process: compute money waits for one where it is.
var errComputeStripeInactive = errors.New("compute stripe: compute is not active in this process, so compute money waits for one where it is")

// intentId as Stripe holds it now, its payment method and latest charge expanded.
func (self *ComputeServerStripe) intent(ctx context.Context, client *stripe.Client, intentId string) (*stripe.PaymentIntent, error) {
	params := &stripe.PaymentIntentRetrieveParams{}
	params.AddExpand("payment_method")
	params.AddExpand("latest_charge")
	intent, err := client.V1PaymentIntents.Retrieve(ctx, intentId, params)
	if err != nil {
		return nil, fmt.Errorf("compute stripe: the intent %s: %w", intentId, err)
	}
	return intent, nil
}

// The fingerprint of the card intent was paid with: its payment method's, else its latest charge's; empty for a
// payment that is not a card's.
func computeStripeFingerprint(intent *stripe.PaymentIntent) string {
	if intent.PaymentMethod != nil && intent.PaymentMethod.Card != nil && intent.PaymentMethod.Card.Fingerprint != "" {
		return intent.PaymentMethod.Card.Fingerprint
	}
	if intent.LatestCharge != nil && intent.LatestCharge.PaymentMethodDetails != nil && intent.LatestCharge.PaymentMethodDetails.Card != nil {
		return intent.LatestCharge.PaymentMethodDetails.Card.Fingerprint
	}
	return ""
}

// Whether the compute session that made intentId sold purchase's pack: one line, the compute product, a dollar a
// credit, quantity one. No such session is a mismatch.
func (self *ComputeServerStripe) priceMatches(ctx context.Context, client *stripe.Client, intentId string, purchase *computeStripePurchase) (bool, error) {
	config, err := self.deps.Config()
	if err != nil {
		return false, err
	}
	for checkoutSession, err := range client.V1CheckoutSessions.List(ctx, &stripe.CheckoutSessionListParams{PaymentIntent: stripe.String(intentId)}) {
		if err != nil {
			return false, fmt.Errorf("compute stripe: the session of %s: %w", intentId, err)
		}
		if !computeStripeMarked(checkoutSession.Metadata) {
			continue
		}
		lines := []*stripe.LineItem{}
		for line, err := range client.V1CheckoutSessions.ListLineItems(ctx, &stripe.CheckoutSessionListLineItemsParams{Session: stripe.String(checkoutSession.ID)}) {
			if err != nil {
				return false, fmt.Errorf("compute stripe: the lines of %s: %w", checkoutSession.ID, err)
			}
			lines = append(lines, line)
			if 1 < len(lines) {
				break
			}
		}
		if len(lines) != 1 {
			return false, nil
		}
		line := lines[0]
		return line.Quantity == 1 && line.Price != nil && line.Price.Product != nil &&
			line.Price.Product.ID == config.ProductId && line.Price.UnitAmount == purchase.packCredits*computeStripeCentsPerCredit, nil
	}
	return false, nil
}

// payment_intent.amount_capturable_updated (see the file header).
func (self *ComputeServerStripe) authorized(ctx context.Context, intentId string) error {
	client, err := self.client()
	if err != nil {
		return err
	}
	intent, err := self.intent(ctx, client, intentId)
	if err != nil {
		return err
	}
	switch intent.Status {
	case stripe.PaymentIntentStatusRequiresCapture:
	case stripe.PaymentIntentStatusCanceled:
		// decided before: a card it saved and a failed detach left attached goes now
		return self.release(ctx, client, intent)
	default:
		return nil
	}
	env := self.deps.Env()
	refuse := func(kind string) error {
		sidecarcontroller.ComputeRaiseAlarm(env, kind)
		return self.cancel(ctx, client, intent)
	}
	purchase, err := computeStripePurchaseOf(self.deps.Ledger, intent.Metadata)
	if err != nil {
		return refuse(ComputeAlarmPurchaseMismatch)
	}
	if !self.deps.Active() {
		// no ledger to check or credit against: the card is never charged
		return refuse(ComputeAlarmPurchaseRefused)
	}
	if intent.Currency != stripe.Currency(computeStripeCurrency) || intent.AmountCapturable < purchase.packCredits*computeStripeCentsPerCredit {
		return refuse(ComputeAlarmPurchaseMismatch)
	}
	priced, err := self.priceMatches(ctx, client, intentId, purchase)
	if err != nil {
		return err
	}
	if !priced {
		return refuse(ComputeAlarmPurchaseMismatch)
	}
	fingerprint := computeStripeFingerprint(intent)
	if fingerprint == "" {
		return refuse(ComputeAlarmPurchaseRefused)
	}
	secrets := self.deps.Secrets()
	payerHash, err := sidecarcontroller.ComputePayerHash(secrets, sidecarcontroller.ComputePayerStripeCard, fingerprint)
	if errors.Is(err, sidecarcontroller.ErrComputeLedgerSecretsUnset) {
		// no key to hash the payer with: every purchase refuses
		return refuse(ComputeAlarmPurchaseRefused)
	}
	if err != nil {
		return err
	}
	check, err := sidecarcontroller.ComputePurchaseCheck(env, ctx, self.deps.Ledger, secrets, &sidecarcontroller.ComputePurchaseRequest{
		NetworkId: purchase.networkId, PayerHash: payerHash, AmountNanoUsd: purchase.amountNanoUsd(),
	})
	if err != nil {
		return err
	}
	if !check.Allowed {
		return refuse(ComputeAlarmPurchaseRefused)
	}
	params := &stripe.PaymentIntentCaptureParams{}
	params.SetIdempotencyKey("compute_capture_" + intent.ID)
	captured, err := client.V1PaymentIntents.Capture(ctx, intent.ID, params)
	if err != nil {
		return fmt.Errorf("compute stripe: the capture of %s: %w", intent.ID, err)
	}
	if captured.Status != stripe.PaymentIntentStatusSucceeded {
		// payment_intent.succeeded credits it once the money is final
		return nil
	}
	return self.credit(ctx, client, intent, payerHash)
}

// Cancels intent uncaptured, under a key of its id, then detaches a card it saved.
func (self *ComputeServerStripe) cancel(ctx context.Context, client *stripe.Client, intent *stripe.PaymentIntent) error {
	params := &stripe.PaymentIntentCancelParams{}
	params.SetIdempotencyKey("compute_cancel_" + intent.ID)
	if _, err := client.V1PaymentIntents.Cancel(ctx, intent.ID, params); err != nil {
		return fmt.Errorf("compute stripe: the cancel of %s: %w", intent.ID, err)
	}
	return self.release(ctx, client, intent)
}

// Detaches the card intent saved for off-session use, where it saved one that is attached still: a payment that did
// not go through saves nothing for auto-renew.
func (self *ComputeServerStripe) release(ctx context.Context, client *stripe.Client, intent *stripe.PaymentIntent) error {
	if intent.SetupFutureUsage != stripe.PaymentIntentSetupFutureUsageOffSession || intent.PaymentMethod == nil ||
		intent.PaymentMethod.ID == "" || intent.PaymentMethod.Customer == nil || intent.PaymentMethod.Customer.ID == "" {
		return nil
	}
	if _, err := client.V1PaymentMethods.Detach(ctx, intent.PaymentMethod.ID, &stripe.PaymentMethodDetachParams{}); err != nil {
		return fmt.Errorf("compute stripe: the detach of %s's card: %w", intent.ID, err)
	}
	return nil
}

// payment_intent.succeeded (see the file header).
func (self *ComputeServerStripe) succeeded(ctx context.Context, intentId string) error {
	client, err := self.client()
	if err != nil {
		return err
	}
	intent, err := self.intent(ctx, client, intentId)
	if err != nil {
		return err
	}
	if intent.Status != stripe.PaymentIntentStatusSucceeded {
		return nil
	}
	var payerHash []byte
	if fingerprint := computeStripeFingerprint(intent); fingerprint != "" {
		payerHash, err = sidecarcontroller.ComputePayerHash(self.deps.Secrets(), sidecarcontroller.ComputePayerStripeCard, fingerprint)
		if err != nil && !errors.Is(err, sidecarcontroller.ErrComputeLedgerSecretsUnset) {
			return err
		}
		// unkeyed, the money is final all the same: credited, counted to no payer
	}
	return self.credit(ctx, client, intent, payerHash)
}

// Credits intent's lot once (see the file header).
func (self *ComputeServerStripe) credit(ctx context.Context, client *stripe.Client, intent *stripe.PaymentIntent, payerHash []byte) error {
	env := self.deps.Env()
	purchase, err := computeStripePurchaseOf(self.deps.Ledger, intent.Metadata)
	if err != nil {
		// taken, and nothing names what it bought: a person refunds it; a retry does no better
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseMismatch)
		glog.Errorf("[compute]stripe payment %s is compute's and names no purchase: %s\n", intent.ID, err)
		return nil
	}
	if !self.deps.Active() {
		return errComputeStripeInactive
	}
	answer, err := sidecarcontroller.ComputeCreditLot(env, ctx, self.deps.Ledger, &sidecarcontroller.ComputeCreditRequest{
		NetworkId:     purchase.networkId,
		Kind:          model.ComputeLotKindPurchase,
		Market:        model.ComputeMarketStripe,
		MarketRef:     intent.ID,
		AmountNanoUsd: purchase.amountNanoUsd(),
		PayerHash:     payerHash,
	})
	if errors.Is(err, sidecarcontroller.ErrComputeAccountClosed) {
		// a deleted network credits nothing; the card gets its money back (§9.5)
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseClosedRefunded)
		params := &stripe.RefundCreateParams{
			PaymentIntent: stripe.String(intent.ID),
			Metadata:      map[string]string{ComputeStripeMetadataRefund: "closed"},
		}
		params.SetIdempotencyKey("compute_refund_closed_" + intent.ID)
		if _, err := client.V1Refunds.Create(ctx, params); err != nil {
			return fmt.Errorf("compute stripe: the refund of %s: %w", intent.ID, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := sidecarcontroller.ComputeRecordPurchaseTerms(env, ctx, purchase.networkId, purchase.memberId); err != nil {
		return err
	}
	if purchase.autoRenew != 0 {
		if err := self.cardSaved(ctx, client, env, intent, purchase); err != nil {
			return err
		}
	}
	if answer.Credited && intent.LatestCharge != nil && 0 < intent.LatestCharge.Amount && intent.LatestCharge.Amount <= intent.LatestCharge.AmountRefunded {
		// refunded before its lot stood: the refund's own event found no lot to reverse
		return self.refunded(ctx, &computeStripeEventObject{Id: intent.LatestCharge.ID, Amount: intent.LatestCharge.Amount, AmountRefunded: intent.LatestCharge.AmountRefunded}, intent.ID)
	}
	return nil
}

// Hands auto-renew the card intent saved for the mode its payer turned on, or detaches it where auto-renew takes none.
func (self *ComputeServerStripe) cardSaved(ctx context.Context, client *stripe.Client, env operator.Env, intent *stripe.PaymentIntent, purchase *computeStripePurchase) error {
	if intent.SetupFutureUsage != stripe.PaymentIntentSetupFutureUsageOffSession || intent.PaymentMethod == nil || intent.PaymentMethod.ID == "" ||
		intent.Customer == nil || intent.Customer.ID == "" {
		// a mode was named and no card saved for it: nothing for auto-renew to take
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseCardDetached)
		return nil
	}
	err := sidecarcontroller.ComputeCardSavedForAutoRenew(env, ctx, &sidecarcontroller.ComputeCardSaved{
		NetworkId:             purchase.networkId,
		PayerMemberId:         purchase.memberId,
		Mode:                  purchase.autoRenew,
		StripeCustomerId:      intent.Customer.ID,
		StripePaymentMethodId: intent.PaymentMethod.ID,
		PaymentIntentId:       intent.ID,
		BelowNanoUsd:          purchase.belowNanoUsd,
		PackCredits:           purchase.topUpPackCredits,
		MonthLimitNanoUsd:     purchase.monthLimitNanoUsd,
	})
	if errors.Is(err, sidecarcontroller.ErrComputeAutoRenewUnbuilt) {
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseCardDetached)
		return self.release(ctx, client, intent)
	}
	return err
}

// payment_intent.canceled: a card the intent saved detached.
func (self *ComputeServerStripe) canceled(ctx context.Context, intentId string) error {
	client, err := self.client()
	if err != nil {
		return err
	}
	intent, err := self.intent(ctx, client, intentId)
	if err != nil {
		return err
	}
	return self.release(ctx, client, intent)
}

// Whether every refund of chargeId is one the ledger made itself: its metadata names compute_refund. A charge with no
// listed refund has none of the ledger's.
func (self *ComputeServerStripe) refundsByLedger(ctx context.Context, client *stripe.Client, chargeId string) (bool, error) {
	params := &stripe.RefundListParams{Charge: stripe.String(chargeId)}
	params.Limit = stripe.Int64(self.settings.RefundListMaxCount)
	read := int64(0)
	for refund, err := range client.V1Refunds.List(ctx, params) {
		if err != nil {
			return false, fmt.Errorf("compute stripe: the refunds of %s: %w", chargeId, err)
		}
		read += 1
		if refund.Metadata[ComputeStripeMetadataRefund] == "" {
			return false, nil
		}
		if self.settings.RefundListMaxCount <= read {
			break
		}
	}
	return 0 < read, nil
}

// charge.refunded on a compute intent (see the file header).
func (self *ComputeServerStripe) refunded(ctx context.Context, charge *computeStripeEventObject, intentId string) error {
	if !self.deps.Active() {
		return errComputeStripeInactive
	}
	if intentId == "" {
		return nil
	}
	client, err := self.client()
	if err != nil {
		return err
	}
	byLedger, err := self.refundsByLedger(ctx, client, charge.Id)
	if err != nil {
		return err
	}
	if byLedger {
		return nil
	}
	env := self.deps.Env()
	if charge.Amount <= 0 || charge.AmountRefunded < charge.Amount {
		// in part, outside the ledger: a person settles what the lot keeps with an adjustment
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseRefundPartial)
		return nil
	}
	_, err = sidecarcontroller.ComputeReverseLot(env, ctx, self.deps.Ledger, &sidecarcontroller.ComputeReverseRequest{
		Market: model.ComputeMarketStripe, MarketRef: intentId,
	})
	return err
}

// charge.dispute.created or .funds_withdrawn on a compute intent: a chargeback, which reverses the lot; an inquiry
// takes no funds and reverses nothing.
func (self *ComputeServerStripe) disputed(ctx context.Context, dispute *computeStripeEventObject, intentId string) error {
	if strings.HasPrefix(dispute.Status, stripeDisputeStatusInquiryPrefix) || intentId == "" {
		return nil
	}
	if !self.deps.Active() {
		return errComputeStripeInactive
	}
	_, err := sidecarcontroller.ComputeReverseLot(self.deps.Env(), ctx, self.deps.Ledger, &sidecarcontroller.ComputeReverseRequest{
		Market: model.ComputeMarketStripe, MarketRef: intentId, Chargeback: true,
	})
	return err
}
