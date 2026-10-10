package controller

// App Store offer-code redemptions (UPGRADE.md A1). A code redeemed through
// AppStore.presentOfferCodeRedeemSheet or an apps.apple.com/redeem link is
// bought outside the app's purchase call, so the transaction carries no
// appAccountToken and nothing in it names a network. The welcome offer codes
// are issued by this server, one per network (model.IssueOnboardingOffer), so
// a redemption of that offer is bound through the issuance:
//
//   - only through the authenticated verify endpoint, for the session network;
//   - only an offer-code purchase (offerType 3) whose offerIdentifier names the
//     welcome offer (the configured reference name, or the code the network
//     was issued);
//   - only when the network was issued a welcome offer with an App Store code,
//     has not redeemed it, and the purchase happened while it was unexpired;
//   - only when the subscription (original transaction id) is not bound yet.
//
// The binding, the offer redemption and the credit (through the
// apple_subscription_transaction ledger gate) commit together or not at all.
// Renewals of a bound subscription resolve to the same network, in the verify
// endpoint, the notification webhook and the reconciler. Anything else stays
// `invalid`; there is no binding by email or any other guess.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// appleOfferCodeIssueSkew allows for clock skew between Apple's purchaseDate
// and our issued_at.
const appleOfferCodeIssueSkew = 5 * time.Minute

// appleOfferCodeBindingTx is the transactional state the binding rules read and
// write. The production implementation is one db transaction; tests use a fake.
type appleOfferCodeBindingTx interface {
	// BoundNetworkId is the network the original transaction id is bound to
	BoundNetworkId(originalTransactionId string) (server.Id, bool)
	// NetworkExists locks the network row for the rest of the tx
	NetworkExists(networkId server.Id) bool
	// OfferForUpdate reads the network's welcome offer and locks it
	OfferForUpdate(networkId server.Id) *model.OnboardingOffer
	// Bind records originalTransactionId -> networkId; false if already bound
	Bind(transaction *validatedAppleTransaction, networkId server.Id) bool
	// RedeemOffer stamps the offer redeemed on apple at `at`; false if it was
	// not eligible at `at`
	RedeemOffer(networkId server.Id, at time.Time) bool
	// Credit is appleCreditSubscriptionTransactionInTx
	Credit(transaction *validatedAppleTransaction) bool
}

type appleOfferCodeBindingResult struct {
	status string
	// reason is why a report was refused (logged)
	reason string
	// commit is false when the tx must roll back (nothing may persist)
	commit bool
	err    error
}

func appleOfferCodeRefused(reason string) appleOfferCodeBindingResult {
	return appleOfferCodeBindingResult{status: VerifyPurchaseStatusInvalid, reason: reason}
}

// appleOfferIdentifierMatches: the transaction's offerIdentifier names the
// welcome offer, by the offer code's reference name or by the code the network
// was issued.
func appleOfferIdentifierMatches(offerIdentifier string, referenceName string, offer *model.OnboardingOffer) bool {
	offerIdentifier = strings.TrimSpace(offerIdentifier)
	if offerIdentifier == "" {
		return false
	}
	if name := strings.TrimSpace(referenceName); name != "" && strings.EqualFold(offerIdentifier, name) {
		return true
	}
	if offer != nil && offer.AppleOfferCode != nil {
		if code := strings.TrimSpace(*offer.AppleOfferCode); code != "" && strings.EqualFold(offerIdentifier, code) {
			return true
		}
	}
	return false
}

// appleBindOfferCodeTransaction applies the binding rules to a verified
// transaction that carries no appAccountToken, reported by sessionNetworkId.
// On success the transaction's networkId is the session network.
func appleBindOfferCodeTransaction(
	store appleOfferCodeBindingTx,
	transaction *validatedAppleTransaction,
	sessionNetworkId server.Id,
	referenceName string,
) appleOfferCodeBindingResult {
	if !transaction.unboundAccountToken || transaction.originalTransactionId == "" {
		return appleOfferCodeRefused("not an unbound transaction")
	}
	if !store.NetworkExists(sessionNetworkId) {
		return appleOfferCodeBindingResult{err: errors.New("session network does not exist")}
	}

	if boundNetworkId, ok := store.BoundNetworkId(transaction.originalTransactionId); ok {
		if boundNetworkId != sessionNetworkId {
			return appleOfferCodeRefused("subscription is bound to another network")
		}
		// a renewal (or a repeated report) of a subscription bound to this
		// network: the ledger gate decides credited vs already_credited
		transaction.networkId = sessionNetworkId
		if store.Credit(transaction) {
			return appleOfferCodeBindingResult{status: VerifyPurchaseStatusCredited, commit: true}
		}
		return appleOfferCodeBindingResult{status: VerifyPurchaseStatusAlreadyCredited, commit: true}
	}

	if transaction.offerType != appleOfferTypeOfferCode {
		return appleOfferCodeRefused("no account token and not an offer-code purchase")
	}
	offer := store.OfferForUpdate(sessionNetworkId)
	if offer == nil {
		return appleOfferCodeRefused("network was never issued a welcome offer")
	}
	if offer.AppleOfferCode == nil || strings.TrimSpace(*offer.AppleOfferCode) == "" {
		return appleOfferCodeRefused("network was never issued an App Store offer code")
	}
	if !appleOfferIdentifierMatches(transaction.offerIdentifier, referenceName, offer) {
		return appleOfferCodeRefused("offer code is not the welcome offer")
	}
	if offer.RedeemedAt != nil {
		return appleOfferCodeRefused("welcome offer already redeemed")
	}
	// the purchase must fall inside the offer's window: Apple's signed
	// purchaseDate, not the time of the report (a report can be retried days
	// after the purchase)
	if !transaction.purchaseTime.Before(offer.ExpiresAt) {
		return appleOfferCodeRefused("welcome offer expired before the purchase")
	}
	if transaction.purchaseTime.Before(offer.IssuedAt.Add(-appleOfferCodeIssueSkew)) {
		return appleOfferCodeRefused("purchase predates the welcome offer")
	}

	if !store.Bind(transaction, sessionNetworkId) {
		return appleOfferCodeRefused("subscription is already bound")
	}
	if !store.RedeemOffer(sessionNetworkId, transaction.purchaseTime) {
		// roll back the binding
		return appleOfferCodeRefused("welcome offer could not be redeemed")
	}
	transaction.networkId = sessionNetworkId
	if !store.Credit(transaction) {
		// the transaction is already in the ledger under some network: it was
		// never an unbound redemption of this offer; roll everything back
		return appleOfferCodeRefused("transaction already credited")
	}
	return appleOfferCodeBindingResult{status: VerifyPurchaseStatusCredited, commit: true}
}

// ----- db implementation -----

type appleOfferCodeBindingDbTx struct {
	tx  server.PgTx
	ctx context.Context
}

func (self *appleOfferCodeBindingDbTx) BoundNetworkId(originalTransactionId string) (server.Id, bool) {
	return model.GetAppleOfferCodeBindingNetworkIdInTx(self.tx, self.ctx, originalTransactionId)
}

func (self *appleOfferCodeBindingDbTx) NetworkExists(networkId server.Id) bool {
	return appleNetworkExistsInTx(self.tx, self.ctx, networkId)
}

func (self *appleOfferCodeBindingDbTx) OfferForUpdate(networkId server.Id) *model.OnboardingOffer {
	return model.GetOnboardingOfferForUpdateInTx(self.tx, self.ctx, networkId)
}

func (self *appleOfferCodeBindingDbTx) Bind(transaction *validatedAppleTransaction, networkId server.Id) bool {
	return model.BindAppleOfferCodeTransactionInTx(
		self.tx,
		self.ctx,
		transaction.originalTransactionId,
		transaction.transactionId,
		networkId,
		transaction.offerIdentifier,
	)
}

func (self *appleOfferCodeBindingDbTx) RedeemOffer(networkId server.Id, at time.Time) bool {
	return model.RedeemOnboardingOfferInTx(self.tx, self.ctx, networkId, model.OnboardingStoreApple, at)
}

func (self *appleOfferCodeBindingDbTx) Credit(transaction *validatedAppleTransaction) bool {
	// the ledger row's notification_uuid is only a provenance pointer; a
	// client report has no notification
	return appleCreditSubscriptionTransactionInTx(self.tx, self.ctx, server.NewId(), transaction)
}

type appleOfferCodeBindingRollback struct{}

// appleOfferCodeBindingRunFunc runs the rules in one transaction, rolled back
// unless the result says commit. Tests replace it with a fake store.
var appleOfferCodeBindingRunFunc = func(
	ctx context.Context,
	callback func(store appleOfferCodeBindingTx) appleOfferCodeBindingResult,
) (result appleOfferCodeBindingResult) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(appleOfferCodeBindingRollback); ok {
				return
			}
			panic(r)
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		result = callback(&appleOfferCodeBindingDbTx{tx: tx, ctx: ctx})
		if !result.commit {
			// a panic is the tx helper's rollback path
			panic(appleOfferCodeBindingRollback{})
		}
	})
	return
}

var appleOfferReferenceNameFunc = func() string {
	return model.Onboarding().Offer.Apple.ReferenceName()
}

var appleOfferCodeProRefreshFunc = func(ctx context.Context, networkId server.Id) {
	model.UpdateProNetwork(ctx, networkId)
}

// verifyAppleOfferCodeTransaction is the verify endpoint's path for a
// transaction without an appAccountToken.
func verifyAppleOfferCodeTransaction(
	ctx context.Context,
	transaction *validatedAppleTransaction,
	sessionNetworkId server.Id,
) (*VerifyStorePurchaseResult, error) {
	referenceName := appleOfferReferenceNameFunc()
	result := appleOfferCodeBindingRunFunc(ctx, func(store appleOfferCodeBindingTx) appleOfferCodeBindingResult {
		return appleBindOfferCodeTransaction(store, transaction, sessionNetworkId, referenceName)
	})
	if result.err != nil {
		return nil, result.err
	}
	if result.status == VerifyPurchaseStatusInvalid || result.status == "" {
		glog.Infof(
			"[sub]verify apple offer-code transaction %s (original %s, offer %q) for network %s refused: %s\n",
			transaction.transactionId,
			transaction.originalTransactionId,
			transaction.offerIdentifier,
			sessionNetworkId,
			result.reason,
		)
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if result.status == VerifyPurchaseStatusCredited {
		glog.Infof(
			"[sub]verify apple offer-code transaction %s (original %s, offer %q) credited to network %s\n",
			transaction.transactionId,
			transaction.originalTransactionId,
			transaction.offerIdentifier,
			sessionNetworkId,
		)
		appleOfferCodeProRefreshFunc(ctx, sessionNetworkId)
	}
	expiryTime := transaction.expiresTime
	return &VerifyStorePurchaseResult{Status: result.status, ExpiryTime: &expiryTime}, nil
}

// appleOfferCodeBindingLookupFunc resolves a bound original transaction id.
// Tests replace it.
var appleOfferCodeBindingLookupFunc = func(connOwner server.PgConn, ctx context.Context, originalTransactionId string) (server.Id, bool) {
	return model.GetAppleOfferCodeBindingNetworkIdInConn(connOwner, ctx, originalTransactionId)
}

// validateAppleTransactionBound is validateAppleTransaction for the paths that
// cannot bind (the notification webhook, the reconciler): a transaction
// without an appAccountToken resolves to the network its subscription was
// bound to, and is invalid when it was never bound.
func validateAppleTransactionBound(
	ctx context.Context,
	notification AppleNotificationDecodedPayload,
	allowedProductIds []string,
	requireEntitlementFields bool,
) (*validatedAppleTransaction, error) {
	return validateAppleTransactionBoundInConn(nil, ctx, notification, allowedProductIds, requireEntitlementFields)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func validateAppleTransactionBoundInConn(connOwner server.PgConn,
	ctx context.Context,
	notification AppleNotificationDecodedPayload,
	allowedProductIds []string,
	requireEntitlementFields bool,
) (*validatedAppleTransaction, error) {
	transaction, err := validateAppleTransactionAccount(notification, allowedProductIds, requireEntitlementFields, true)
	if err != nil {
		return nil, err
	}
	if transaction.unboundAccountToken {
		networkId, ok := appleOfferCodeBindingLookupFunc(connOwner, ctx, transaction.originalTransactionId)
		if !ok {
			return nil, errors.New("invalid App Store account token")
		}
		transaction.networkId = networkId
	}
	return transaction, nil
}
