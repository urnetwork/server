package controller

// Account deletion stops every store renewal the server can stop before it
// removes the network, and refuses deletion for a renewal it cannot stop.
// Stripe and Google Play subscriptions are cancelled through their APIs. An
// App Store subscription can only be cancelled by the customer, so deletion
// waits until the App Store reports that it will not renew.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const networkRemoveStripeFailedMessage = "Failed to unsubscribe Stripe"
const networkRemovePlayFailedMessage = "Could not cancel your Google Play subscription. Please try again."
const networkRemoveAppleRenewingMessage = "Your subscription is billed through the App Store and cannot be cancelled by URnetwork. Cancel it in your App Store subscriptions (Settings > your name > Subscriptions), then delete your account."

// networkRemoveSteps are the store and storage steps of an account deletion.
// NetworkRemove runs them in this order: the App Store check, Stripe
// cancellation, Google Play cancellation, then the network removal.
type networkRemoveSteps struct {
	// reports whether an App Store subscription may still renew; an error
	// counts as renewing
	appleRenewing func(clientSession *session.ClientSession) (bool, error)
	// cancels and closes the network's Stripe subscriptions
	unsubscribeStripe func(clientSession *session.ClientSession) error
	// cancels the renewal of the network's Google Play subscriptions
	cancelPlay func(clientSession *session.ClientSession) error
	// removes the network; returns the removed user auths
	removeNetwork func(clientSession *session.ClientSession) (bool, map[string]bool)
	// schedules the product-update removals for the removed user auths
	scheduleRemoveProductUpdates func(clientSession *session.ClientSession, userAuths map[string]bool)
}

var defaultNetworkRemoveSteps = networkRemoveSteps{
	appleRenewing:     networkRemoveAppleRenewing,
	unsubscribeStripe: UnsubscribeStripe,
	cancelPlay:        networkRemoveCancelPlay,
	removeNetwork: func(clientSession *session.ClientSession) (bool, map[string]bool) {
		return model.RemoveNetwork(
			clientSession.Ctx,
			clientSession.ByJwt.NetworkId,
			&clientSession.ByJwt.UserId,
		)
	},
	scheduleRemoveProductUpdates: func(clientSession *session.ClientSession, userAuths map[string]bool) {
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			for userAuth, _ := range userAuths {
				ScheduleRemoveProductUpdates(clientSession, userAuth, tx)
			}
		})
	},
}

func networkRemoveError(message string) *NetworkRemoveResult {
	return &NetworkRemoveResult{
		Error: &NetworkRemoveResultError{
			Message: message,
		},
	}
}

// networkRemoveWithSteps runs an authorized deletion. Each store step is a
// fail-closed prerequisite: any failure returns before the network is
// removed, so the customer can retry and nothing keeps billing a deleted
// account.
func networkRemoveWithSteps(
	clientSession *session.ClientSession,
	steps *networkRemoveSteps,
) (*NetworkRemoveResult, error) {
	// checked first: it changes nothing, so a refusal leaves every store
	// subscription as it was
	appleRenewing, err := steps.appleRenewing(clientSession)
	if err != nil {
		glog.Errorf("[network]remove could not check the App Store subscription: %v\n", err)
		appleRenewing = true
	}
	if appleRenewing {
		return networkRemoveError(networkRemoveAppleRenewingMessage), nil
	}

	if err := steps.unsubscribeStripe(clientSession); err != nil {
		glog.Errorf("Failed to unsubscribe Stripe: %v", err)
		return networkRemoveError(networkRemoveStripeFailedMessage), nil
	}

	if err := steps.cancelPlay(clientSession); err != nil {
		glog.Errorf("[network]remove failed to cancel Google Play: %v\n", err)
		return networkRemoveError(networkRemovePlayFailedMessage), nil
	}

	success, userAuths := steps.removeNetwork(clientSession)
	if success {
		steps.scheduleRemoveProductUpdates(clientSession, userAuths)
		return &NetworkRemoveResult{}, nil
	}

	return nil, fmt.Errorf("Could not remove network")
}

func networkRemoveActiveRenewals(
	clientSession *session.ClientSession,
	market model.SubscriptionMarket,
) []*model.ActiveSubscriptionRenewal {
	renewals := []*model.ActiveSubscriptionRenewal{}
	for _, renewal := range model.GetActiveSubscriptionRenewals(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		model.SubscriptionTypeSupporter,
	) {
		if renewal.Market == market {
			renewals = append(renewals, renewal)
		}
	}
	return renewals
}

func networkRemoveAppleRenewing(clientSession *session.ClientSession) (bool, error) {
	originalTransactionIds := []string{}
	for _, renewal := range networkRemoveActiveRenewals(clientSession, model.SubscriptionMarketApple) {
		originalTransactionIds = append(originalTransactionIds, renewal.TransactionId)
	}
	return appleSubscriptionsRenewing(
		clientSession.Ctx,
		originalTransactionIds,
		subscriptionStoreLookups.apple,
	)
}

// appleSubscriptionsRenewing reports whether any of the App Store
// subscriptions behind the active renewals may still bill. Only an App Store
// answer of "will not renew" clears a subscription; a missing transaction id
// or a failed lookup counts as renewing.
func appleSubscriptionsRenewing(
	ctx context.Context,
	originalTransactionIds []string,
	lookup func(ctx context.Context, originalTransactionId string) (*subscriptionStoreState, error),
) (bool, error) {
	seen := map[string]bool{}
	for _, originalTransactionId := range originalTransactionIds {
		if seen[originalTransactionId] {
			continue
		}
		seen[originalTransactionId] = true
		if originalTransactionId == "" {
			return true, errors.New("active App Store renewal has no original transaction id")
		}
		if lookup == nil {
			return true, errors.New("no App Store lookup")
		}
		lookupCtx, cancel := context.WithTimeout(ctx, subscriptionStoreLookupTimeout)
		state, err := lookup(lookupCtx, originalTransactionId)
		cancel()
		if err != nil {
			return true, fmt.Errorf("App Store subscription status: %w", err)
		}
		if state == nil || state.AutoRenew == nil || *state.AutoRenew {
			return true, nil
		}
	}
	return false, nil
}

func networkRemoveCancelPlay(clientSession *session.ClientSession) error {
	purchaseTokens := []string{}
	for _, renewal := range networkRemoveActiveRenewals(clientSession, model.SubscriptionMarketGoogle) {
		purchaseTokens = append(purchaseTokens, renewal.PurchaseToken)
	}
	return playCancelDeletionSubscriptions(clientSession.Ctx, purchaseTokens)
}

// playSubscriptionRenews reports whether Google Play will bill the purchase
// again: a live state with an auto-renewing line item that is switched on.
// Prepaid plans never renew (and Play does not cancel them).
func playSubscriptionRenews(sub *PlaySubscription) bool {
	if sub == nil {
		return false
	}
	switch sub.SubscriptionState {
	case "SUBSCRIPTION_STATE_ACTIVE",
		"SUBSCRIPTION_STATE_IN_GRACE_PERIOD",
		"SUBSCRIPTION_STATE_ON_HOLD",
		"SUBSCRIPTION_STATE_PAUSED",
		"SUBSCRIPTION_STATE_PENDING":
	default:
		// canceled, expired, pending purchase canceled, unspecified
		return false
	}
	for _, item := range sub.LineItems {
		if item != nil && item.AutoRenewingPlan != nil && item.AutoRenewingPlan.AutoRenewEnabled {
			return true
		}
	}
	return false
}

type playCancelSubscriptionRequest struct {
	CancellationContext *playCancellationContext `json:"cancellationContext"`
}

type playCancellationContext struct {
	CancellationType string `json:"cancellationType"`
}

// The account is being deleted, so the cancellation must not be restorable
// by the (soon nonexistent) customer.
const playCancellationTypeDeveloperStopPayments = "DEVELOPER_REQUESTED_STOP_PAYMENTS"

// playCancelDeletionSubscriptions stops the renewal of every Google Play
// subscription that still renews. This is cancel, not revoke: the paid period
// is neither refunded nor cut short, matching the Stripe deletion, which
// cancels without a refund.
//
// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2/get
// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2/cancel
func playCancelDeletionSubscriptions(ctx context.Context, purchaseTokens []string) error {
	seen := map[string]bool{}
	for _, purchaseToken := range purchaseTokens {
		if seen[purchaseToken] {
			continue
		}
		seen[purchaseToken] = true
		if purchaseToken == "" {
			return errors.New("active Google Play renewal has no purchase token")
		}

		subUrl := fmt.Sprintf(
			"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
			playPublisherApiBaseUrl,
			url.PathEscape(playPackageNameFunc()),
			url.PathEscape(purchaseToken),
		)
		sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
			ctx,
			subUrl,
			func(header http.Header) {
				playAuthHeaderFunc(ctx, header)
			},
			server.ResponseJsonObject[*PlaySubscription],
		)
		if err != nil {
			var statusErr *server.HttpStatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusGone {
				// the purchase no longer exists in Play; nothing renews
				continue
			}
			return fmt.Errorf("get Google Play subscription: %w", err)
		}
		if !playSubscriptionRenews(sub) {
			continue
		}

		_, err = server.HttpPostRequireStatusOk[[]byte](
			ctx,
			subUrl+":cancel",
			&playCancelSubscriptionRequest{
				CancellationContext: &playCancellationContext{
					CancellationType: playCancellationTypeDeveloperStopPayments,
				},
			},
			func(header http.Header) {
				playAuthHeaderFunc(ctx, header)
			},
			func(response *http.Response, responseBodyBytes []byte) ([]byte, error) {
				return responseBodyBytes, nil
			},
		)
		if err != nil {
			return fmt.Errorf("cancel Google Play subscription: %w", err)
		}
	}
	return nil
}
