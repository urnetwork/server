package controller

// Compute's store subscriptions at an account's deletion (DESIGN-COMPUTE §9.5, §9.12; BAL-M12-14), stopped as Pro's are
// (network_remove_providers.go), before the compute balance is closed:
//
//	networkRemoveComputeStores          one read of the network's compute store subscriptions
//	                                    (sidecar/controller's ComputeStoreSubscriptionsOf): its monthly row's, an App
//	                                    Store one by its original transaction id and a Play one by its purchase token,
//	                                    and each App Store monthly grant of the last months, by its transaction id
//	networkRemoveComputeAppleRenewing   the App Store check, beside Pro's, before anything is cancelled: the App Store
//	                                    answers for each transaction the statuses of its customer's subscriptions, and
//	                                    only an answer that every compute subscription among them will not bill again
//	                                    clears it, since only its holder can cancel one; a failed lookup, an answer with
//	                                    no compute subscription in it, and a read with more grants than it may ask about
//	                                    count as renewing, so the deletion waits
//
// The Play subscriptions are cancelled by Pro's own step (playCancelDeletionSubscriptions), and the closure is handed
// the read, refusing where its network names a subscription the read did not (ErrComputeCloseStoresChanged), as Pro's
// removal refuses a renewal credited during it.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const networkRemoveComputeAppleRenewingMessage = "Your compute credit subscription is billed through the App Store and cannot be cancelled by URnetwork. Cancel it in your App Store subscriptions (Settings > your name > Subscriptions), then delete your account."

// The App Store's subscription statuses that bill nothing more: identifiers of its API (appleLastTransaction's).
const (
	computeAppleStatusExpired = 2
	computeAppleStatusRevoked = 5
)

// The read step of network removal (see the file header): none where this binary links no MG-C1, the ledger's tables,
// or the network holds no compute ledger state.
func networkRemoveComputeStores(clientSession *session.ClientSession) (*sidecarcontroller.ComputeStoreSubscriptions, error) {
	if !server.SidecarMgC1Linked() {
		return &sidecarcontroller.ComputeStoreSubscriptions{}, nil
	}
	has, err := computeNetworkHasLedger(clientSession.Ctx, clientSession.ByJwt.NetworkId)
	if err != nil {
		return nil, err
	}
	if !has {
		return &sidecarcontroller.ComputeStoreSubscriptions{}, nil
	}
	return sidecarcontroller.ComputeStoreSubscriptionsOf(networkRemoveComputeEnv(), clientSession.Ctx, sidecarmodel.DefaultComputeLedgerSettings(), clientSession.ByJwt.NetworkId)
}

// Whether networkId holds compute ledger state a store subscription could stand in, a compute account (every credit
// makes one) or an auto-renew row, read through server's own pool, so a network that never held compute credit reads
// none whatever the process's env holds, as the closure step reads it (computeNetworkHasAccount).
func computeNetworkHasLedger(ctx context.Context, networkId server.Id) (bool, error) {
	has := false
	var readErr error
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM compute_account WHERE network_id = $1)
					OR EXISTS (SELECT 1 FROM compute_auto_renew WHERE network_id = $1)
			`, networkId).Scan(&has))
		})
	}, func(err error) {
		readErr = err
	})
	return has, readErr
}

// The App Store step of network removal (see the file header).
func networkRemoveComputeAppleRenewing(clientSession *session.ClientSession, transactionIds []string) (bool, error) {
	return computeAppleSubscriptionsRenewing(clientSession.Ctx, transactionIds, computeAppleSubscriptionStatuses)
}

// The App Store's statuses of the subscriptions of transactionId's customer, production first, then the sandbox where
// apple.yml accepts it, as Pro's lookup asks them (appleLookupSubscriptionState).
func computeAppleSubscriptionStatuses(ctx context.Context, transactionId string) (*appleSubscriptionStatusesResponse, error) {
	creds := appleReconcileCredentialsFunc()
	if creds == nil {
		return nil, errors.New("no App Store Server API credentials")
	}
	statuses, err := appleGetSubscriptionStatuses(ctx, appleAppStoreServerApiBaseUrl, transactionId)
	if appleTransactionIdNotFound(err) && creds.allowsEnvironment(appleEnvironmentSandbox) {
		statuses, err = appleGetSubscriptionStatuses(ctx, appleAppStoreServerApiSandboxBaseUrl, transactionId)
		if err != nil {
			err = fmt.Errorf("App Store sandbox: %w", err)
		}
	}
	return statuses, err
}

// Whether any compute subscription the App Store holds for the customers of transactionIds may still bill (see the
// file header); an error counts as renewing.
func computeAppleSubscriptionsRenewing(
	ctx context.Context,
	transactionIds []string,
	statuses func(ctx context.Context, transactionId string) (*appleSubscriptionStatusesResponse, error),
) (bool, error) {
	seen := map[string]bool{}
	for _, transactionId := range transactionIds {
		if seen[transactionId] {
			continue
		}
		seen[transactionId] = true
		if transactionId == "" {
			return true, errors.New("a compute App Store subscription has no transaction id")
		}
		if statuses == nil {
			return true, errors.New("no App Store lookup")
		}
		lookupCtx, cancel := context.WithTimeout(ctx, subscriptionStoreLookupTimeout)
		answer, err := statuses(lookupCtx, transactionId)
		cancel()
		if err != nil {
			return true, fmt.Errorf("App Store subscription statuses: %w", err)
		}
		renewing, err := computeAppleStatusesRenewing(answer)
		if err != nil || renewing {
			return true, err
		}
	}
	return false, nil
}

// Whether a compute subscription among statuses may still bill: one active, in billing retry or in its grace period
// whose renewal info does not say its auto-renew is off. Pro's subscriptions in the answer are passed over, so another
// network's Pro plan bought with the same Apple Account holds nothing here. An answer naming no compute subscription,
// or one whose transaction cannot be read, counts as renewing.
func computeAppleStatusesRenewing(statuses *appleSubscriptionStatusesResponse) (bool, error) {
	if err := validateAppleSubscriptionStatuses(statuses); err != nil {
		return true, err
	}
	found := false
	for _, group := range statuses.Data {
		for _, transaction := range group.LastTransactions {
			claims, err := appleDecodeJwsPayload(transaction.SignedTransactionInfo)
			if err != nil {
				return true, fmt.Errorf("an App Store subscription's transaction: %w", err)
			}
			productId, _ := appleControllerStringClaim(claims, "productId")
			if !sidecar.ComputeStoreProductFamilyOf(productId) {
				continue
			}
			found = true
			if transaction.Status == computeAppleStatusExpired || transaction.Status == computeAppleStatusRevoked {
				continue
			}
			if computeAppleAutoRenewOff(transaction.SignedRenewalInfo) {
				continue
			}
			return true, nil
		}
	}
	if !found {
		return true, errors.New("the App Store answered no compute subscription")
	}
	return false, nil
}

// Whether a subscription's renewal info says its holder turned auto-renew off; renewal info that is absent or cannot
// be read says nothing, so the subscription counts as renewing.
func computeAppleAutoRenewOff(signedRenewalInfo string) bool {
	if signedRenewalInfo == "" {
		return false
	}
	claims, err := appleDecodeJwsPayload(signedRenewalInfo)
	if err != nil {
		return false
	}
	status, ok := claims["autoRenewStatus"].(float64)
	return ok && status == 0
}
