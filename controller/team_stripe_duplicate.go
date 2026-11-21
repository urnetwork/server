package controller

// Paid invoices and the hourly reconciliation share one Team billing owner.
// A duplicate is recorded under the network's billing lock before Stripe is
// asked to cancel it. An ambiguous cancellation can therefore be retried by id.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var errDuplicateTeamSubscription = errors.New("duplicate Team subscription")

// The current local contract must be checked against Stripe outside the transaction.
type teamSubscriptionConflict struct {
	subscriptionId string
}

func (self *teamSubscriptionConflict) Error() string {
	return "the network has another Team subscription"
}

// The same decision is used by a real invoice, duplicate discovery and a dry
// run. A previously rejected contract stays rejected after later purchases.
func stripeTeamClaimInTx(ctx context.Context, tx server.PgTx, networkId server.Id, subscriptionId string, claimsPeriod bool) error {
	if model.IsDuplicateTeamSubscriptionInTx(ctx, tx, networkId, subscriptionId) {
		return errDuplicateTeamSubscription
	}
	if !claimsPeriod || model.GetTeamSubscriptionInTx(ctx, tx, networkId, subscriptionId) != nil {
		return nil
	}
	live := model.GetLiveTeamSubscriptionWithQuery(ctx, tx, networkId)
	if live == nil || live.SubscriptionId == subscriptionId {
		return nil
	}
	return &teamSubscriptionConflict{subscriptionId: live.SubscriptionId}
}

// The caller holds the Team billing lock and has checked the exact incumbent at Stripe.
func stripeTeamContractInTx(ctx context.Context, tx server.PgTx, networkId server.Id, subscriptionId string, confirmed string, claimsPeriod bool, now time.Time) error {
	decision := stripeTeamClaimInTx(ctx, tx, networkId, subscriptionId, claimsPeriod)
	var conflict *teamSubscriptionConflict
	if !errors.As(decision, &conflict) || conflict.subscriptionId != confirmed {
		return decision
	}
	server.Raise(model.AddPaymentReconciliationEventInTx(tx, ctx, &model.PaymentReconciliationEvent{
		RunId: server.NewId(), Store: model.SubscriptionMarketStripe, NetworkId: &networkId,
		Action: model.PaymentReconcileActionDuplicateTeamSubscription, Evidence: subscriptionId,
		Details: map[string]any{"retained_subscription": confirmed, "review_paid_invoices": true},
	}))
	// Failures recorded before contracts were distinguished must no longer hold
	// this network's real credit line or leave the owner's failed-payment notices due.
	reason := model.StripeInvoicePaymentFailureResolveSubscriptionEnded
	failures := model.ResolveStripeSubscriptionPaymentFailuresInTx(ctx, tx, subscriptionId, reason, now)
	for invoiceId := range failures {
		stripeWithdrawPaymentFailedNoticesInTx(ctx, tx, invoiceId, reason, now)
	}
	if len(failures) > 0 && !model.HasOpenStripeInvoicePaymentFailureInTx(ctx, tx, networkId) {
		model.RestoreTeamCreditLinesInTx(ctx, tx, networkId, now)
	}
	return errDuplicateTeamSubscription
}

// An operator's dry run must not advertise a grant for a duplicate paid invoice.
func stripeTeamDryRunInvoice(run *paymentReconcileRun, credit *stripeInvoiceCredit) (duplicate bool, complete bool, err error) {
	ctx := run.clientSession.Ctx
	var decision error
	server.TxInConn(ctx, run.connOwner, func(tx server.PgTx) {
		decision = stripeTeamClaimInTx(ctx, tx, credit.networkId, credit.subscriptionId, credit.team.hasBase)
	}, server.TxReadCommitted)
	var conflict *teamSubscriptionConflict
	if !errors.As(decision, &conflict) {
		return errors.Is(decision, errDuplicateTeamSubscription), true, nil
	}
	if !run.spend(model.SubscriptionMarketStripe) {
		return false, false, nil
	}
	sub, err := stripeDunningFetchSubscription(ctx, conflict.subscriptionId, stripeDunningSettingsFunc())
	if err != nil {
		return false, true, err
	}
	if !stripeTeamMarked(sub.Metadata) || sub.Metadata[stripeMetadataNetworkId] != credit.networkId.String() {
		return false, true, errors.New("the competing subscription does not name its Team")
	}
	return !stripeSubscriptionTerminal(sub.Status), true, nil
}

// A terminal contract can be replaced even when its deletion webhook was lost.
// The next transaction checks the exact incumbent again before classifying a duplicate.
func stripeCheckTeamIncumbent(connOwner server.PgConn, ctx context.Context, networkId server.Id, subscriptionId string, now time.Time) (string, error) {
	sub, err := stripeDunningFetchSubscription(ctx, subscriptionId, stripeDunningSettingsFunc())
	if err != nil {
		return "", err
	}
	if !stripeTeamMarked(sub.Metadata) || sub.Metadata[stripeMetadataNetworkId] != networkId.String() {
		return "", fmt.Errorf("the retained Team subscription does not name its network")
	}
	if stripeSubscriptionTerminal(sub.Status) {
		_, err := stripeEndTeamSubscriptionInConn(connOwner, ctx, networkId, subscriptionId, now)
		return "", err
	}
	return sub.Id, nil
}

// Only an exact, verified provider identity can be canceled. A lost response
// is harmless because a retry reads the same subscription's current status.
func stripeStopDuplicateTeam(connOwner server.PgConn, ctx context.Context, networkId server.Id, sub *stripeDunningSubscription) error {
	if !stripeTeamMarked(sub.Metadata) || sub.Metadata[stripeMetadataNetworkId] != networkId.String() {
		return errors.New("the duplicate Team subscription does not name its network")
	}
	if !stripeSubscriptionTerminal(sub.Status) {
		stopCtx, cancel := context.WithTimeout(ctx, stripeDunningSettingsFunc().StripeReadTimeout)
		defer cancel()
		if _, err := stripeCancelSubscriptionNow(stopCtx, sub.Id); err != nil {
			return err
		}
	}
	_, err := model.AddPaymentReconciliationEventOnceInConn(connOwner, ctx, &model.PaymentReconciliationEvent{
		RunId: server.NewId(), Store: model.SubscriptionMarketStripe, NetworkId: &networkId,
		Action: model.PaymentReconcileActionDuplicateTeamStopped, Evidence: sub.Id,
	})
	return err
}

// An interrupted cancellation still runs when its original Team has since ended.
func stripeReconcileRejectedTeamSubscriptions(run *paymentReconcileRun, pageCount int, rejected map[string]bool) bool {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketStripe
	var after server.Id
	for {
		pending := model.ListPendingDuplicateTeamSubscriptionsInConn(run.connOwner, ctx, after, pageCount)
		for _, event := range pending {
			if !run.spend(store) {
				return false
			}
			run.examine(store)
			rejected[event.Evidence] = true
			sub, err := stripeDunningFetchSubscription(ctx, event.Evidence, stripeDunningSettingsFunc())
			details := map[string]any{"leg": "team", "reason": model.PaymentReconcileActionDuplicateTeamSubscription}
			if err == nil {
				if run.dryRun {
					if !stripeSubscriptionTerminal(sub.Status) {
						run.record(store, model.PaymentReconcileActionWouldEnd, event.NetworkId, sub.Id, details)
					}
					continue
				}
				if !stripeSubscriptionTerminal(sub.Status) && !run.spend(store) {
					return false
				}
				err = stripeStopDuplicateTeam(run.connOwner, ctx, *event.NetworkId, sub)
			}
			if err != nil {
				details["error"] = err.Error()
				run.record(store, model.PaymentReconcileActionError, event.NetworkId, event.Evidence, details)
			} else {
				run.record(store, model.PaymentReconcileActionEnded, event.NetworkId, event.Evidence, details)
			}
		}
		if len(pending) < pageCount {
			return true
		}
		after = pending[len(pending)-1].EventId
	}
}

// Older purchases could use different customers. A paged search of the Team mark
// costs one request per hundred subscriptions and finds contracts missing locally.
func stripeReconcileOtherTeamSubscriptions(run *paymentReconcileRun, rejected map[string]bool) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketStripe
	lookupCtx, cancel := context.WithTimeout(ctx, stripeDunningSettingsFunc().StripeReadTimeout)
	defer cancel()
	subs, err := stripeSearchSubscriptionsByMetadata(lookupCtx, stripeMetadataTeam, stripeTeamMark, func() bool { return run.spend(store) })
	if errors.Is(err, errStripeSubscriptionSearchBudget) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	for _, listed := range subs {
		if rejected[listed.Id] || stripeSubscriptionTerminal(listed.Status) {
			continue
		}
		networkId, err := server.ParseId(listed.Metadata[stripeMetadataNetworkId])
		if err != nil {
			run.record(store, model.PaymentReconcileActionError, nil, listed.Id, map[string]any{"leg": "team_duplicates", "error": "subscription has no network"})
			continue
		}
		retained := model.GetLiveTeamSubscriptionInConn(run.connOwner, ctx, networkId)
		if retained == nil || listed.Id == retained.SubscriptionId {
			continue
		}
		run.examine(store)
		complete, err := stripeReconcileTeamConflict(run, retained, listed.Id)
		if err != nil {
			run.record(store, model.PaymentReconcileActionError, &networkId, listed.Id, map[string]any{"leg": "team_duplicates", "error": err.Error()})
		}
		if !complete {
			return false, nil
		}
	}
	return true, nil
}

// A search can be stale. Read both subscriptions directly and recheck the
// local choice under the Team lock before recording or canceling a duplicate.
func stripeReconcileTeamConflict(run *paymentReconcileRun, retained *model.TeamSubscription, candidateId string) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketStripe
	networkId := retained.NetworkId
	var target *stripeDunningSubscription
	for _, id := range []string{retained.SubscriptionId, candidateId} {
		if !run.spend(store) {
			return false, nil
		}
		sub, err := stripeDunningFetchSubscription(ctx, id, stripeDunningSettingsFunc())
		if err != nil {
			return true, err
		}
		if !stripeTeamMarked(sub.Metadata) || sub.Metadata[stripeMetadataNetworkId] != networkId.String() {
			return true, errors.New("conflicting subscriptions no longer name the same Team")
		}
		if stripeSubscriptionTerminal(sub.Status) {
			return true, nil
		}
		target = sub
	}
	details := map[string]any{"leg": "team", "retained_subscription": retained.SubscriptionId}
	if run.dryRun {
		run.record(store, model.PaymentReconcileActionWouldEnd, &networkId, candidateId, details)
		return true, nil
	}
	var decision error
	server.TxInConn(ctx, run.connOwner, func(tx server.PgTx) {
		decision = model.LockPaymentNetworkInTx(tx, ctx, networkId)
		if decision == nil && model.LockTeamBillingInTx(ctx, tx, networkId) {
			decision = stripeTeamContractInTx(ctx, tx, networkId, candidateId, retained.SubscriptionId, true, run.now)
		}
	}, server.TxReadCommitted)
	if !errors.Is(decision, errDuplicateTeamSubscription) {
		return true, nil
	}
	if !run.spend(store) {
		return false, nil
	}
	if err := stripeStopDuplicateTeam(run.connOwner, ctx, networkId, target); err != nil {
		return true, err
	}
	run.record(store, model.PaymentReconcileActionEnded, &networkId, candidateId, details)
	return true, nil
}
