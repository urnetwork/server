package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Team's Stripe events (QUOTA-M6-02; DESIGN.md 20.5's Becoming Team, 20.9's Post-paid;
// plan-QUOTA-team-renewal-window). A Team subscription is one POST /network/team-checkout made:
// its metadata carries the network and the team mark (team_checkout_controller.go), and
// StripeWebhook's handlers branch on the mark:
//
//	invoice.paid                   stripeResolveInvoiceCredit reads the base item's period, since a
//	                               Team invoice's first line may be last period's metered item, and
//	                               stripeHandleInvoicePaidInConn branches here before the supporter
//	                               credit, so a Team invoice never writes a supporter renewal or a
//	                               Pro balance. Under the stripe_invoice ledger, which still gates
//	                               redelivery, it writes the period's team renewal. The first
//	                               invoice's period, from the payment to the anchor, has no grant
//	                               yet, so it writes the team grant and the credit line
//	                               (model.AddTeamGrantInTx). A cycle invoice's period was granted
//	                               at its anchor by RefreshTeamTransferBalances, so it records the
//	                               renewal's revenue, clears dunning and restores a line a failure
//	                               ended. A paid period the job never granted, because a failure
//	                               stood open at its anchor, is granted here, or the job, which
//	                               writes only the period after the newest renewal, never would.
//	invoice.payment_failed         the dunning records the failure and owes its notices
//	                               (subscription_stripe_dunning.go); the open failure stops the next
//	                               grant, and the lines in window end, so overage refuses as an
//	                               empty balance while the included bytes run out
//	customer.subscription.deleted  the lines end, every period no invoice paid for ends, and the
//	                               subscription's team_subscription row ends, so the job writes it
//	                               nothing more; a paid period keeps its grant to its end, so lapse
//	                               follows Pro. The hourly reconciler ends a live row whose
//	                               subscription Stripe shows deleted the same way, for an event
//	                               that was lost (reconcileStripeTeamSubscriptions)
//
// A Team's first period runs from the payment to the anchor, the first 00:00:00 UTC after the
// checkout session's expiry, so its renewal ends on the anchor and every later period starts at
// 00:00:00 UTC. The first paid invoice writes the subscription's team_subscription row in the
// transaction that credits it (QUOTA-M6-07, owner decision 199): the subscription and customer
// ids, the anchor (Stripe's billing_cycle_anchor, else the first period's end) and the start (the
// first period's start). The grant job and the meter reports read it.
//
// The tier is refreshed after each commit (model.UpdateProNetwork), never inside a transaction.

// What a Team invoice bills.
type stripeTeamInvoice struct {
	// The invoice has a line of the base item: it buys a period. One without, the final
	// invoice of a subscription's metered usage, buys none.
	hasBase bool
	// The base item's period, the period the invoice buys.
	periodStart time.Time
	periodEnd   time.Time
	// What the invoice's lines bill for transfer and storage overage, in cents. The credit line
	// already carries the transfer overage's revenue, at the overage rate, so the renewal's
	// revenue leaves these out.
	overageAmount int64
}

// The share of an invoice's amount the payment's fees take, as Pro's invoice credit counts it.
const stripeTeamFeeFraction = 0.3

// Reads what a Team invoice bills from its lines. An invoice whose lines are not all on the page
// and show no base line is an error, since the base line may be on a page not read.
func stripeTeamInvoiceOf(invoiceId string, fullInvoice *StripeInvoiceExpanded) (*stripeTeamInvoice, error) {
	prices := stripeTeamPrices()
	teamInvoice := &stripeTeamInvoice{}
	if fullInvoice.Lines == nil {
		return nil, fmt.Errorf("team invoice %s has no lines", invoiceId)
	}
	var basePeriod *StripeLineItemPeriod
	for _, lineItem := range fullInvoice.Lines.Data {
		if lineItem == nil {
			continue
		}
		switch priceId := lineItem.priceId(); {
		case priceId == "":
		case priceId == prices.Base:
			// the newest period when a proration splits the base into several lines
			if period := lineItem.Period; period != nil && period.Start < period.End {
				if basePeriod == nil || basePeriod.End < period.End || (basePeriod.End == period.End && basePeriod.Start < period.Start) {
					basePeriod = period
				}
			}
		case priceId == prices.TransferOverage, priceId == prices.StorageOverage:
			teamInvoice.overageAmount += lineItem.Amount
		}
	}
	if basePeriod == nil {
		if prices.Base == "" {
			return nil, fmt.Errorf("team invoice %s: no team base price is configured", invoiceId)
		}
		if fullInvoice.Lines.HasMore {
			return nil, fmt.Errorf("team invoice %s: the base line is not among the lines read", invoiceId)
		}
		return teamInvoice, nil
	}
	teamInvoice.hasBase = true
	teamInvoice.periodStart = time.Unix(basePeriod.Start, 0).UTC()
	teamInvoice.periodEnd = time.Unix(basePeriod.End, 0).UTC()
	return teamInvoice, nil
}

// The renewal's revenue: the invoice's amount less its overage lines, less fees.
func stripeTeamInvoiceNetRevenue(invoice *StripeEventInvoiceObject, teamInvoice *stripeTeamInvoice) model.NanoCents {
	amount := int64(invoice.Total) - teamInvoice.overageAmount
	if amount <= 0 {
		return 0
	}
	return model.UsdToNanoCents((1.0 - stripeTeamFeeFraction) * float64(amount) / 100.0)
}

// A Team invoice whose network is not a team: nothing is credited.
var errStripeTeamInvoiceNotTeam = errors.New("team invoice names a network that is not a team")

// invoice.paid of a Team subscription, for the webhook and the reconciler alike. An error means
// nothing was written, and Stripe delivers the event again.
func stripeHandleTeamInvoicePaidInConn(
	connOwner server.PgConn,
	invoice *StripeEventInvoiceObject,
	credit *stripeInvoiceCredit,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	ctx := clientSession.Ctx
	if !server.SidecarMg6Linked() {
		// a release that links MG6 takes the redelivery
		return nil, fmt.Errorf("team invoice %s: this binary does not link MG6", invoice.Id)
	}

	// an invoice paid after its subscription ended buys no line and no later period
	settings := stripeDunningSettingsFunc()
	sub, err := stripeDunningFetchSubscription(ctx, credit.subscriptionId, settings)
	if err != nil {
		return nil, err
	}
	subscriptionEnded := stripeSubscriptionTerminal(sub.Status)
	now := stripeDunningNow()
	subscription := stripeTeamSubscriptionOf(credit, invoice, sub)

	var credited bool
	var returnErr error
	if r := server.HandleError(func() {
		server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
			credited, returnErr = stripeCreditTeamInvoicePaidInTx(tx, ctx, invoice, credit, subscription, subscriptionEnded, now)
		}, server.TxReadCommitted, server.OptNoRetry())
	}); r != nil {
		return nil, fmt.Errorf("team invoice %s: %v", invoice.Id, r)
	}
	if errors.Is(returnErr, errStripeTeamInvoiceNotTeam) {
		// never credited to anyone; on file for support, as an invoice that names no network is
		added, err := stripeRecordUnresolvedInvoice(ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketStripe,
			Action:    model.PaymentReconcileActionCreditUnfulfillable,
			NetworkId: &credit.networkId,
			Evidence:  invoice.Id,
			Details: map[string]any{
				"reason":       "team_invoice_not_a_team_network",
				"invoice":      invoice.Id,
				"subscription": credit.subscriptionId,
				"amount_total": invoice.Total,
				"currency":     invoice.Currency,
			},
		})
		if err != nil {
			return nil, err
		}
		if added {
			glog.Errorf("[sub][team]invoice %s (subscription %s) is paid for network %s, which is not a team; not credited, recorded for support\n", invoice.Id, credit.subscriptionId, credit.networkId)
		}
		return &StripeWebhookResult{}, nil
	}
	if returnErr != nil {
		return nil, returnErr
	}
	if credited {
		model.UpdateProNetworkInConn(connOwner, ctx, credit.networkId)
		clearSubscriptionDetailsCached(ctx, credit.networkId)
	}
	return &StripeWebhookResult{}, nil
}

// The team_subscription row a paid invoice of the subscription writes if the network has none for
// it: the anchor is Stripe's billing_cycle_anchor, else the end of the invoice's base period, which
// for the first invoice runs from the payment to the anchor; the start is the base period's start,
// the first renewal's, so the job's read of this subscription's renewals takes the first. nil for
// an invoice with no base line, which buys no period and dates nothing.
func stripeTeamSubscriptionOf(credit *stripeInvoiceCredit, invoice *StripeEventInvoiceObject, sub *stripeDunningSubscription) *model.TeamSubscription {
	teamInvoice := credit.team
	if teamInvoice == nil || !teamInvoice.hasBase {
		return nil
	}
	anchorTime := teamInvoice.periodEnd
	if sub != nil && 0 < sub.BillingCycleAnchor {
		anchorTime = time.Unix(sub.BillingCycleAnchor, 0).UTC()
	}
	customerId := invoice.Customer
	if customerId == "" && sub != nil {
		customerId = stripeCustomerIdOf(sub.Customer)
	}
	return &model.TeamSubscription{
		NetworkId:      credit.networkId,
		SubscriptionId: credit.subscriptionId,
		CustomerId:     customerId,
		AnchorTime:     anchorTime,
		StartTime:      teamInvoice.periodStart,
	}
}

// A customer field as Stripe gives it, an id or the expanded object, as its id; empty for neither.
func stripeCustomerIdOf(customer json.RawMessage) string {
	if len(customer) == 0 {
		return ""
	}
	var customerId string
	if err := json.Unmarshal(customer, &customerId); err == nil {
		return customerId
	}
	var expanded struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(customer, &expanded); err == nil {
		return expanded.Id
	}
	return ""
}

// One attempt of a Team invoice's credit, in the caller's no-retry ReadCommitted transaction.
// credited is false for an invoice the ledger already holds. A raise rolls the ledger row back
// with everything else. subscription is the row the invoice writes if the network has none for
// its subscription (stripeTeamSubscriptionOf), nil for an invoice that buys no period.
func stripeCreditTeamInvoicePaidInTx(
	tx server.PgTx,
	ctx context.Context,
	invoice *StripeEventInvoiceObject,
	credit *stripeInvoiceCredit,
	subscription *model.TeamSubscription,
	subscriptionEnded bool,
	now time.Time,
) (credited bool, returnErr error) {
	networkId := credit.networkId
	if err := model.LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
		return false, err
	}
	if model.GetNetworkTeamInTx(ctx, tx, networkId) == nil {
		return false, errStripeTeamInvoiceNotTeam
	}

	ledgerTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO stripe_invoice (invoice_id, network_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
		`,
		invoice.Id,
		networkId,
	))
	if ledgerTag.RowsAffected() == 0 {
		glog.Infof("[sub][team]invoice %s already credited; ignoring redelivery\n", invoice.Id)
		return false, nil
	}

	// the invoice is paid: its failure, if one is on file, is over
	if resolvedNetworkId := model.ResolveStripeInvoicePaymentFailureInTx(ctx, tx, invoice.Id, model.StripeInvoicePaymentFailureResolvePaid, now); resolvedNetworkId != nil {
		stripeWithdrawPaymentFailedNoticesInTx(ctx, tx, invoice.Id, model.StripeInvoicePaymentFailureResolvePaid, now)
	}

	teamInvoice := credit.team
	if !teamInvoice.hasBase {
		// the final invoice of a subscription's metered usage: revenue, and no period
		glog.Infof("[sub][team]invoice %s of network %s bills no period\n", invoice.Id, networkId)
		return true, nil
	}

	netRevenue := stripeTeamInvoiceNetRevenue(invoice, teamInvoice)
	if err := model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeTeam,
		StartTime:          teamInvoice.periodStart,
		EndTime:            teamInvoice.periodEnd,
		NetRevenue:         netRevenue,
		SubscriptionMarket: model.SubscriptionMarketStripe,
		TransactionId:      invoice.Id,
	}); err != nil {
		server.Raise(err)
	}
	// the first paid invoice of the subscription records it; a later one, or a redelivery whose
	// ledger row this transaction wrote again, changes nothing
	if subscription != nil && subscription.CustomerId != "" {
		model.AddTeamSubscriptionInTx(ctx, tx, subscription)
	} else if subscription != nil {
		glog.Errorf("[sub][team]invoice %s of network %s names no customer; its subscription is not recorded\n", invoice.Id, networkId)
	}

	switch {
	case subscriptionEnded:
		// Stripe ended the subscription before its invoice was credited: the paid period is the
		// last, the row ends, and the job writes nothing more. A lost customer.subscription.deleted
		// leaves the lines to end here.
		model.EndTeamSubscriptionInTx(ctx, tx, networkId, credit.subscriptionId, now)
	case !model.HasTeamGrantInTx(ctx, tx, networkId, teamInvoice.periodStart):
		if now.Before(teamInvoice.periodEnd) {
			grant, err := model.AddTeamGrantInTx(ctx, tx, networkId, teamInvoice.periodStart, teamInvoice.periodEnd, netRevenue)
			if err != nil {
				server.Raise(err)
			}
			if grant.LineBalanceId == nil {
				glog.Infof("[sub][team]invoice %s of network %s: period granted, credit line withheld (%s)\n", invoice.Id, networkId, grant.LineWithheld)
			}
		}
	case !model.HasOpenStripeInvoicePaymentFailureInTx(ctx, tx, networkId):
		// the job granted the period at its anchor; a line a failure ended comes back
		model.RestoreTeamCreditLinesInTx(ctx, tx, networkId, now)
	}

	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		notice := stripeInvoiceSalesNotice(networkId, invoice, teamInvoice.periodStart, teamInvoice.periodEnd)
		notice.Item = "Team"
		return notice
	})
	return true, nil
}

// invoice.payment_failed of a Team subscription, once the dunning holds the failure open: ends
// the network's credit lines. An error means Stripe delivers the event again; a redelivery ends
// nothing twice.
func stripeTeamPaymentFailed(ctx context.Context, networkId server.Id, now time.Time) error {
	if !server.SidecarMg6Linked() {
		return fmt.Errorf("team payment failure of network %s: this binary does not link MG6", networkId)
	}
	ended := 0
	var returnErr error
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			if returnErr = model.LockPaymentNetworkInTx(tx, ctx, networkId); returnErr != nil {
				return
			}
			ended = len(model.EndTeamCreditLinesInTx(ctx, tx, networkId, now))
		}, server.TxReadCommitted, server.OptNoRetry())
	}); r != nil {
		return fmt.Errorf("team payment failure of network %s: %v", networkId, r)
	}
	if errors.Is(returnErr, model.ErrPaymentNetworkNotFound) {
		// the network is gone, and its balances with it
		return nil
	}
	if returnErr != nil {
		return returnErr
	}
	if 0 < ended {
		glog.Infof("[stripe][team]network %s: a renewal failed, %d credit line(s) ended\n", networkId, ended)
	}
	return nil
}

// customer.subscription.deleted of a Team subscription. Nothing happens for a subscription
// without the team mark, for one Stripe does not report ended, or for one with no live
// team_subscription row: it never paid an invoice, so nothing was granted, or it ended already. An
// error means Stripe or the database could not be read or a balance was busy, and Stripe delivers
// the event again.
func stripeHandleTeamSubscriptionDeleted(object json.RawMessage, clientSession *session.ClientSession) error {
	var deleted struct {
		Id       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(object, &deleted); err != nil {
		glog.Warningf("[stripe][team]could not parse the deleted subscription: %s\n", err)
		return nil
	}
	if deleted.Id == "" || !stripeTeamMarked(deleted.Metadata) {
		return nil
	}
	ctx := clientSession.Ctx
	if !server.SidecarMg6Linked() {
		return errors.New("a team subscription's end: this binary does not link MG6")
	}

	// the current state, read from Stripe, decides, as the dunning's does
	settings := stripeDunningSettingsFunc()
	sub, err := stripeDunningFetchSubscription(ctx, deleted.Id, settings)
	if err != nil {
		return err
	}
	if !stripeTeamMarked(sub.Metadata) || !stripeSubscriptionTerminal(sub.Status) {
		glog.Infof("[stripe][team]a deleted subscription is %s; nothing ends\n", sub.Status)
		return nil
	}
	networkId, err := server.ParseId(sub.Metadata[stripeMetadataNetworkId])
	if err != nil {
		glog.Errorf("[stripe][team]a subscription ended and names no network\n")
		return nil
	}
	_, err = stripeEndTeamSubscriptionInConn(nil, ctx, networkId, deleted.Id, stripeDunningNow())
	return err
}

// Ends the network's Team subscription as customer.subscription.deleted does, through the
// caller's connection (nil: an acquisition of its own): the lines, the unpaid periods and the
// team_subscription row (model.EndTeamSubscriptionInTx), then the tier after the commit. ended is
// false when the subscription has no live row, which ends nothing. A gone network ends nothing and
// is no error.
func stripeEndTeamSubscriptionInConn(connOwner server.PgConn, ctx context.Context, networkId server.Id, subscriptionId string, now time.Time) (ended bool, returnErr error) {
	endedBalanceCount := 0
	if r := server.HandleError(func() {
		server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
			ended = false
			endedBalanceCount = 0
			if returnErr = model.LockPaymentNetworkInTx(tx, ctx, networkId); returnErr != nil {
				return
			}
			var endedBalanceIds []server.Id
			ended, endedBalanceIds = model.EndTeamSubscriptionInTx(ctx, tx, networkId, subscriptionId, now)
			endedBalanceCount = len(endedBalanceIds)
		}, server.TxReadCommitted, server.OptNoRetry())
	}); r != nil {
		return false, fmt.Errorf("a team subscription's end for network %s: %v", networkId, r)
	}
	if errors.Is(returnErr, model.ErrPaymentNetworkNotFound) {
		return false, nil
	}
	if returnErr != nil {
		return false, returnErr
	}
	if ended {
		glog.Infof("[stripe][team]network %s's subscription ended: %d balance(s) ended\n", networkId, endedBalanceCount)
		model.UpdateProNetworkInConn(connOwner, ctx, networkId)
		clearSubscriptionDetailsCached(ctx, networkId)
	}
	return ended, nil
}

// The reconciler's Team leg.
type stripeTeamReconcileSettings struct {
	// The live subscriptions one page of the leg reads.
	PageCount int
}

func defaultStripeTeamReconcileSettings() *stripeTeamReconcileSettings {
	return &stripeTeamReconcileSettings{
		// ~1,000 Teams in year two (DESIGN.md 20.9's cost row): ten pages
		PageCount: 100,
	}
}

var stripeTeamReconcileSettingsFunc = defaultStripeTeamReconcileSettings

// reconcileStripe's third leg (QUOTA-M6-07; owner decision 199): each live team_subscription row is
// checked against Stripe, and one whose subscription Stripe shows ended is ended as
// customer.subscription.deleted would end it, so a lost event is repaired within one run. Stripe's
// read of each spends the run's budget; complete is false when the budget ran out first. A dry run
// records what it would end and ends nothing.
func reconcileStripeTeamSubscriptions(run *paymentReconcileRun) (complete bool, returnErr error) {
	if !server.SidecarMg6Linked() {
		return true, nil
	}
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketStripe
	dunningSettings := stripeDunningSettingsFunc()
	settings := stripeTeamReconcileSettingsFunc()
	var after *server.Id
	for {
		page := model.ListLiveTeamSubscriptionsInConn(run.connOwner, ctx, after, settings.PageCount)
		for _, subscription := range page {
			run.examine(store)
			if !run.spend(store) {
				return false, nil
			}
			networkId := subscription.NetworkId
			sub, err := stripeDunningFetchSubscription(ctx, subscription.SubscriptionId, dunningSettings)
			if err != nil {
				run.record(store, model.PaymentReconcileActionError, &networkId, subscription.SubscriptionId, map[string]any{"error": err.Error(), "leg": "team"})
				continue
			}
			if !stripeSubscriptionTerminal(sub.Status) {
				continue
			}
			details := map[string]any{"subscription_status": sub.Status, "leg": "team"}
			if run.dryRun {
				run.record(store, model.PaymentReconcileActionWouldEnd, &networkId, subscription.SubscriptionId, details)
				continue
			}
			ended, err := stripeEndTeamSubscriptionInConn(run.connOwner, ctx, networkId, subscription.SubscriptionId, run.now)
			if err != nil {
				run.record(store, model.PaymentReconcileActionError, &networkId, subscription.SubscriptionId, map[string]any{"error": err.Error(), "leg": "team"})
				continue
			}
			if ended {
				run.record(store, model.PaymentReconcileActionEnded, &networkId, subscription.SubscriptionId, details)
			}
		}
		if len(page) < settings.PageCount {
			return true, nil
		}
		last := page[len(page)-1].NetworkId
		after = &last
	}
}
