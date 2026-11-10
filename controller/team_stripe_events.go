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
//	                               subscription is marked ended, so the job writes it nothing more;
//	                               a paid period keeps its grant to its end, so lapse follows Pro
//
// A Team's first period runs from the payment to the anchor, the first 00:00:00 UTC after the
// checkout session's expiry, so its renewal ends on the anchor and every later period starts at
// 00:00:00 UTC.
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

	var credited bool
	var returnErr error
	if r := server.HandleError(func() {
		server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
			credited, returnErr = stripeCreditTeamInvoicePaidInTx(tx, ctx, invoice, credit, subscriptionEnded, now)
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

// One attempt of a Team invoice's credit, in the caller's no-retry ReadCommitted transaction.
// credited is false for an invoice the ledger already holds. A raise rolls the ledger row back
// with everything else.
func stripeCreditTeamInvoicePaidInTx(
	tx server.PgTx,
	ctx context.Context,
	invoice *StripeEventInvoiceObject,
	credit *stripeInvoiceCredit,
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

	switch {
	case subscriptionEnded:
		// the job reads no live subscription behind this renewal
		model.MarkTeamSubscriptionEndedInTx(ctx, tx, networkId, credit.subscriptionId, now)
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
// without the team mark, for one Stripe does not report ended, or for one that is not behind the
// network's newest Team renewal. An error means Stripe or the database could not be read or a
// balance was busy, and Stripe delivers the event again.
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
		return fmt.Errorf("team subscription %s: this binary does not link MG6", deleted.Id)
	}

	// the current state, read from Stripe, decides, as the dunning's does
	settings := stripeDunningSettingsFunc()
	sub, err := stripeDunningFetchSubscription(ctx, deleted.Id, settings)
	if err != nil {
		return err
	}
	if !stripeTeamMarked(sub.Metadata) || !stripeSubscriptionTerminal(sub.Status) {
		glog.Infof("[stripe][team]subscription %s is %s; nothing ends\n", deleted.Id, sub.Status)
		return nil
	}
	networkId, err := server.ParseId(sub.Metadata[stripeMetadataNetworkId])
	if err != nil {
		glog.Errorf("[stripe][team]subscription %s ended and names no network\n", deleted.Id)
		return nil
	}

	// only the subscription that bills the network now ends its balances
	renewal := model.GetLastSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeTeam, model.SubscriptionMarketStripe)
	if renewal == nil || renewal.TransactionId == "" {
		// it never paid an invoice: nothing was granted
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, settings.StripeReadTimeout)
	defer cancel()
	billingSub, err := stripeSubscriptionFromInvoice(readCtx, renewal.TransactionId)
	if err != nil {
		return err
	}
	if billingSub.Id != deleted.Id {
		glog.Infof("[stripe][team]subscription %s ended; network %s is billed by %s\n", deleted.Id, networkId, billingSub.Id)
		return nil
	}

	now := stripeDunningNow()
	ended := 0
	var returnErr error
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			ended = 0
			if returnErr = model.LockPaymentNetworkInTx(tx, ctx, networkId); returnErr != nil {
				return
			}
			ended = len(model.EndTeamSubscriptionInTx(ctx, tx, networkId, deleted.Id, now))
		}, server.TxReadCommitted, server.OptNoRetry())
	}); r != nil {
		return fmt.Errorf("team subscription %s: %v", deleted.Id, r)
	}
	if errors.Is(returnErr, model.ErrPaymentNetworkNotFound) {
		return nil
	}
	if returnErr != nil {
		return returnErr
	}
	glog.Infof("[stripe][team]subscription %s of network %s ended: %d balance(s) ended\n", deleted.Id, networkId, ended)
	model.UpdateProNetwork(ctx, networkId)
	clearSubscriptionDetailsCached(ctx, networkId)
	return nil
}
