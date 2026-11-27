package controller

// ComputeAppleStore (DESIGN-COMPUTE §9.5, §9.11, §9.12, §14.3, D-BAL-8; BAL-M12-09): the App Store's compute.*
// products, whose purchases reach the server two ways, each already verified against Apple's pinned roots before any
// claim here is read:
//
//	the app's report     POST api/subscription/verify-apple-transaction (VerifyAppleTransactionClaims routes a
//	                     compute.* product here): the transaction credited once by its transaction id, and only then
//	                     answered credited or already_credited, the terminal answers on which the app calls finish();
//	                     an error answers no status, so the app reports it again at its next launch and nothing is
//	                     credited twice
//	the notifications    ProcessAppleNotification routes a compute.* product here: ONE_TIME_CHARGE (a consumable),
//	                     SUBSCRIBED and DID_RENEW (a month's grant under its own transaction id), DID_CHANGE_RENEWAL_PREF
//	                     with UPGRADE (an upgrade taking effect at once), each credited once, whichever of it and the
//	                     app's report comes first; REFUND and REVOKE reversing the transaction's lot; every other type
//	                     acknowledged, since a downgrade takes effect at the next renewal, whose DID_RENEW grants it
//	the plan             each grant of a subscription, and DID_CHANGE_RENEWAL_PREF's downgrade, DID_CHANGE_RENEWAL_STATUS,
//	                     DID_FAIL_TO_RENEW, GRACE_PERIOD_EXPIRED and EXPIRED, reported to auto-renew by its original
//	                     transaction id (sidecar/controller's ComputeMonthlyStoreReport): the plan that renews next and
//	                     whether it renews, as the renewal info says, so GET api/compute/auto-renew shows it and a
//	                     closure finds it (compute_store_close.go); the app's report credits alone, its notification
//	                     carrying the renewal info
//
// The network is the one the transaction's appAccountToken names, which the app sets to the network id at purchase. A
// purchase that names none, such as an offer code redeemed in the store, binds as Pro's offer codes bind
// (apple_offer_code_binding, keyed on the original transaction id, so its renewals and plan changes follow the
// chain): to the network whose app reports it first, and a notification of one no report has bound waits for the
// report, answering an error so Apple sends it again. A report from another network than the token names is
// wrong_network. A network that is gone credits nothing: only Apple refunds, so the payment is recorded for support.
//
// The payer is the keyed hash of the transaction's appTransactionId, one Apple Account in the app (§9.11); the store
// took the money before the payer was known, so the purchase is credited whatever the limits say, and flagged past
// one. A transaction is credited by its transaction id alone, never its original transaction id, which renewals
// share. An upgrade grants the new plan's credits less the old plan's for the part of its period Apple's prorated
// refund gives back (computeStorePlanChange), the old transaction read from the App Store's history of the
// subscription; four plan changes in one sitting grant about the largest plan's credits. A family-shared transaction
// is not its holder's purchase and credits nothing; a sandbox one (TestFlight, App Review) credits only where the
// settings say so, since no money moved.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The App Store compute paths' settings.
type ComputeAppleStoreSettings struct {
	// Credit a sandbox purchase (TestFlight, App Review, a sandbox tester): false, since no money moves and a credit
	// would be compute for free; a staging env whose test app adds credit in the sandbox sets it (§19's M12a exit).
	SandboxCredit bool
	// The most pages of a subscription's history a plan change reads to find the transaction it replaces: 4, past a
	// month's renewals and a sitting's changes. A reading: §9.5 names no figure.
	HistoryPageMaxCount int
}

// The defaults, the only place one is set.
func DefaultComputeAppleStoreSettings() *ComputeAppleStoreSettings {
	return &ComputeAppleStoreSettings{
		SandboxCredit:       false,
		HistoryPageMaxCount: 4,
	}
}

// The App Store's record of one subscription's transactions, read by a plan change (§9.5).
type ComputeAppleTransactions interface {
	// The transactions of the subscription transactionId belongs to, newest first, each as the claims of an
	// authenticated pull from Apple, in the environment the transaction names.
	History(ctx context.Context, environment string, transactionId string, pageMaxCount int) ([]map[string]any, error)
}

// The App Store's compute paths (see the file header). Safe for concurrent use.
type ComputeAppleStore struct {
	settings     *ComputeAppleStoreSettings
	deps         ComputeStoreDeps
	transactions ComputeAppleTransactions
}

// The paths over settings, deps and transactions, nil taking the defaults, the process's own and the App Store Server
// API's history.
func NewComputeAppleStore(settings *ComputeAppleStoreSettings, deps *ComputeStoreDeps, transactions ComputeAppleTransactions) *ComputeAppleStore {
	if settings == nil {
		settings = DefaultComputeAppleStoreSettings()
	}
	if transactions == nil {
		transactions = computeAppleServerTransactions{}
	}
	return &ComputeAppleStore{settings: settings, deps: computeStoreDepsOf(deps), transactions: transactions}
}

// The process's paths, which the verify route and the notification webhook share. Replaceable only by the hermetic
// tests; production never mutates it.
var computeAppleStoreProcess = sync.OnceValue(func() *ComputeAppleStore {
	return NewComputeAppleStore(nil, nil, nil)
})

// The App Store's words the paths read: identifiers of its API.
const (
	computeAppleTypeConsumable   = "Consumable"
	computeAppleTypeSubscription = "Auto-Renewable Subscription"
	computeAppleOwnershipShared  = "FAMILY_SHARED"
	computeAppleReasonRenewal    = "RENEWAL"
	computeAppleSubtypeUpgrade   = "UPGRADE"
	// What a binding made by a compute purchase records as its offer: an identifier.
	computeAppleBindingOffer = "compute"
)

// One compute transaction as its verified claims name it.
type computeAppleTransaction struct {
	transactionId         string
	originalTransactionId string
	product               *sidecar.ComputeStoreProduct
	// The network the appAccountToken names; nil where it names none.
	networkId *server.Id
	// One Apple Account in the app: the payer (§9.11); empty where the claims carry none.
	appTransactionId string
	purchaseTime     time.Time
	// A subscription's period end; zero for a consumable.
	expiresTime       time.Time
	revoked           bool
	sandbox           bool
	shared            bool
	transactionReason string
	environment       string
}

// The compute transaction claims name, refused where they are not one of the compute products whole.
func (self *ComputeAppleStore) transactionOf(claims map[string]any) (*computeAppleTransaction, error) {
	transaction := &computeAppleTransaction{}
	transaction.transactionId, _ = appleControllerStringClaim(claims, "transactionId")
	if transaction.transactionId == "" || 128 < len(transaction.transactionId) {
		return nil, errors.New("compute apple: no transaction id")
	}
	transaction.originalTransactionId, _ = appleControllerStringClaim(claims, "originalTransactionId")
	if transaction.originalTransactionId == "" {
		transaction.originalTransactionId = transaction.transactionId
	}
	if 128 < len(transaction.originalTransactionId) {
		return nil, errors.New("compute apple: an original transaction id past its bound")
	}
	productId, _ := appleControllerStringClaim(claims, "productId")
	product, ok := sidecar.ComputeStoreProductOf(self.deps.Products, productId)
	if !ok {
		return nil, fmt.Errorf("compute apple: %q is no compute product sold", productId)
	}
	transaction.product = product
	if kind, ok := appleControllerStringClaim(claims, "type"); ok {
		if (kind == computeAppleTypeConsumable) == product.Monthly || (kind == computeAppleTypeSubscription) != product.Monthly {
			return nil, fmt.Errorf("compute apple: a %s product of type %q", productId, kind)
		}
	}
	if token, _ := appleControllerStringClaim(claims, "appAccountToken"); token != "" {
		networkId, err := server.ParseId(token)
		if err != nil || networkId == (server.Id{}) {
			return nil, errors.New("compute apple: an account token that names no network")
		}
		transaction.networkId = &networkId
	}
	transaction.appTransactionId, _ = appleControllerStringClaim(claims, "appTransactionId")
	purchaseMillis, ok := appleControllerInt64Claim(claims, "purchaseDate")
	if !ok || purchaseMillis <= 0 {
		return nil, errors.New("compute apple: no purchase date")
	}
	transaction.purchaseTime = time.UnixMilli(purchaseMillis).UTC()
	if product.Monthly {
		expiresMillis, ok := appleControllerInt64Claim(claims, "expiresDate")
		if !ok || expiresMillis <= purchaseMillis {
			return nil, errors.New("compute apple: a subscription with no period")
		}
		transaction.expiresTime = time.UnixMilli(expiresMillis).UTC()
	}
	if revocationMillis, ok := appleControllerInt64Claim(claims, "revocationDate"); ok && 0 < revocationMillis {
		transaction.revoked = true
	}
	transaction.environment, _ = appleControllerStringClaim(claims, "environment")
	transaction.sandbox = transaction.environment == appleEnvironmentSandbox
	ownership, _ := appleControllerStringClaim(claims, "inAppOwnershipType")
	transaction.shared = ownership == computeAppleOwnershipShared
	transaction.transactionReason, _ = appleControllerStringClaim(claims, "transactionReason")
	return transaction, nil
}

// What transaction credits: its product's credits, or an upgrade's difference (see the file header).
func (self *ComputeAppleStore) grant(ctx context.Context, transaction *computeAppleTransaction) (int64, error) {
	full := transaction.product.Credits * sidecarmodel.ComputeCreditNanoUsd
	if !transaction.product.Monthly || transaction.transactionReason == computeAppleReasonRenewal || transaction.originalTransactionId == transaction.transactionId {
		// a consumable, a renewal, or the subscription's first purchase: no plan it replaces
		return full, nil
	}
	history, err := self.transactions.History(ctx, transaction.environment, transaction.transactionId, self.settings.HistoryPageMaxCount)
	if err != nil {
		return 0, fmt.Errorf("compute apple: the subscription's history: %w", err)
	}
	var previous *computeAppleTransaction
	for _, claims := range history {
		other, err := self.transactionOf(claims)
		if err != nil || other.transactionId == transaction.transactionId || !other.product.Monthly || !other.purchaseTime.Before(transaction.purchaseTime) {
			continue
		}
		if previous == nil || previous.purchaseTime.Before(other.purchaseTime) {
			previous = other
		}
	}
	if previous == nil {
		return full, nil
	}
	return computeStorePlanChange(transaction.product.Credits, previous.product.Credits, previous.purchaseTime, previous.expiresTime, transaction.purchaseTime), nil
}

// The payer's keyed hash of transaction's appTransactionId; nil where it names none or the host holds no key, the
// purchase then counting to its network alone.
func (self *ComputeAppleStore) payer(transaction *computeAppleTransaction) ([]byte, error) {
	if transaction.appTransactionId == "" {
		return nil, nil
	}
	hash, err := sidecarcontroller.ComputePayerHash(self.deps.Secrets(), sidecarcontroller.ComputePayerAppStoreAccount, transaction.appTransactionId)
	if errors.Is(err, sidecarcontroller.ErrComputeLedgerSecretsUnset) {
		return nil, nil
	}
	return hash, err
}

// Credits transaction to networkId.
func (self *ComputeAppleStore) credit(ctx context.Context, transaction *computeAppleTransaction, networkId server.Id) (*ComputeStoreCreditAnswer, error) {
	amount, err := self.grant(ctx, transaction)
	if err != nil {
		return nil, err
	}
	payerHash, err := self.payer(transaction)
	if err != nil {
		return nil, err
	}
	return ComputeStoreCredit(self.deps.Env(), ctx, &self.deps, &ComputeStorePurchase{
		Market:        sidecarmodel.ComputeMarketAppStore,
		NetworkId:     networkId,
		MarketRef:     transaction.transactionId,
		Product:       transaction.product,
		AmountNanoUsd: amount,
		PayerHash:     payerHash,
	})
}

// The network transaction's original transaction id is bound to, read alone.
func computeAppleBoundNetworkId(ctx context.Context, originalTransactionId string) (*server.Id, error) {
	var bound *server.Id
	var returnErr error
	server.HandleError(func() {
		if networkId, ok := model.GetAppleOfferCodeBindingNetworkId(ctx, originalTransactionId); ok {
			bound = &networkId
		}
	}, func(err error) {
		returnErr = err
	})
	return bound, returnErr
}

// Binds transaction's subscription or consumable to networkId where nothing bound it, and answers the network it is
// bound to: the first report's.
func computeAppleBind(ctx context.Context, transaction *computeAppleTransaction, networkId server.Id) (*server.Id, error) {
	var bound *server.Id
	var returnErr error
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			model.BindAppleOfferCodeTransactionInTx(tx, ctx, transaction.originalTransactionId, transaction.transactionId, networkId, computeAppleBindingOffer)
			if boundNetworkId, ok := model.GetAppleOfferCodeBindingNetworkIdInTx(tx, ctx, transaction.originalTransactionId); ok {
				bound = &boundNetworkId
			}
		})
	}, func(err error) {
		returnErr = err
	})
	return bound, returnErr
}

// The app's report of a compute transaction (see the file header), from sessionNetworkId.
func (self *ComputeAppleStore) VerifyTransaction(ctx context.Context, claims map[string]any, sessionNetworkId server.Id) (*VerifyStorePurchaseResult, error) {
	transaction, err := self.transactionOf(claims)
	if err != nil {
		glog.Infof("[compute]verify apple transaction: %s\n", err)
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if transaction.shared || (transaction.sandbox && !self.settings.SandboxCredit) {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if transaction.revoked {
		// refunded or revoked: its lot, where one stands, is reversed, and the report never credits
		if _, err := ComputeStoreReverse(self.deps.Env(), ctx, &self.deps, sidecarmodel.ComputeMarketAppStore, transaction.transactionId); err != nil {
			return nil, err
		}
		return NewVerifyStorePurchaseInvalid(), nil
	}
	networkId := transaction.networkId
	if networkId == nil {
		if networkId, err = computeAppleBoundNetworkId(ctx, transaction.originalTransactionId); err != nil {
			return nil, err
		}
		if networkId == nil {
			if networkId, err = computeAppleBind(ctx, transaction, sessionNetworkId); err != nil {
				return nil, err
			}
		}
	}
	if networkId == nil || *networkId != sessionNetworkId {
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), nil
	}
	answer, err := self.credit(ctx, transaction, *networkId)
	if err != nil {
		return nil, err
	}
	switch {
	case answer.Gone:
		computeStoreRecordUnowned(ctx, model.SubscriptionMarketApple, transaction.transactionId, "destination_deleted", networkId)
		return NewVerifyStorePurchaseInvalid(), nil
	case answer.Credited:
		return verifyStatusResult(VerifyPurchaseStatusCredited), nil
	}
	return verifyStatusResult(VerifyPurchaseStatusAlreadyCredited), nil
}

// A notification of a compute product (see the file header). processed is true only where this call credited.
func (self *ComputeAppleStore) Notification(ctx context.Context, notification AppleNotificationDecodedPayload) (bool, error) {
	credits := false
	// a change of the plan or of its renewal alone, reported to auto-renew and crediting nothing
	plan := false
	switch notification.NotificationType {
	case "ONE_TIME_CHARGE", "SUBSCRIBED", "DID_RENEW":
		credits = true
	case "DID_CHANGE_RENEWAL_PREF":
		// an upgrade takes effect at once; a downgrade at the next renewal, whose DID_RENEW grants it, and is reported
		// now as the plan that renews next
		if notification.Subtype == computeAppleSubtypeUpgrade {
			credits = true
		} else {
			plan = true
		}
	case "DID_CHANGE_RENEWAL_STATUS", "DID_FAIL_TO_RENEW", "GRACE_PERIOD_EXPIRED", "EXPIRED":
		plan = true
	case "REFUND", "REVOKE":
	default:
		return false, nil
	}
	if notification.TransactionInfo == nil {
		return false, errors.New("compute apple: a notification with no verified transaction")
	}
	transaction, err := self.transactionOf(notification.TransactionInfo)
	if err != nil {
		return false, err
	}
	if !credits && !plan {
		_, err := ComputeStoreReverse(self.deps.Env(), ctx, &self.deps, sidecarmodel.ComputeMarketAppStore, transaction.transactionId)
		return false, err
	}
	if transaction.shared || transaction.revoked || (transaction.sandbox && !self.settings.SandboxCredit) {
		return false, nil
	}
	if plan && !transaction.product.Monthly {
		return false, nil
	}
	networkId := transaction.networkId
	if networkId == nil {
		if networkId, err = computeAppleBoundNetworkId(ctx, transaction.originalTransactionId); err != nil {
			return false, err
		}
		if networkId == nil {
			if plan {
				// a plan no report has bound yet: its first grant reports it
				return false, nil
			}
			// nothing names its network until the app's report binds it: Apple sends it again
			return false, errAppleAccountTokenInvalid
		}
	}
	if plan {
		return false, self.reportPlan(ctx, transaction, *networkId, notification)
	}
	answer, err := self.credit(ctx, transaction, *networkId)
	if err != nil {
		return false, err
	}
	if answer.Gone {
		computeStoreRecordUnowned(ctx, model.SubscriptionMarketApple, transaction.transactionId, "destination_deleted", networkId)
		return false, nil
	}
	if transaction.product.Monthly {
		// after the grant, so an error here has Apple send it again, the grant then credited already
		if err := self.reportPlan(ctx, transaction, *networkId, notification); err != nil {
			return false, err
		}
	}
	return answer.Credited, nil
}

// Reports transaction's subscription to auto-renew as networkId's monthly plan (sidecar/controller's
// ComputeMonthlyStoreReport): by its original transaction id, the plan that renews next, the renewal info's
// autoRenewProductId where that names a compute subscription, and whether it renews, which the renewal info's
// autoRenewStatus says, an expiry renewing nothing. Renewal info absent or silent leaves it renewing, a grant's plan
// being one that just billed.
func (self *ComputeAppleStore) reportPlan(ctx context.Context, transaction *computeAppleTransaction, networkId server.Id, notification AppleNotificationDecodedPayload) error {
	credits := transaction.product.Credits
	renews := true
	if notification.RenewalInfo != nil {
		if productId, _ := appleControllerStringClaim(notification.RenewalInfo, "autoRenewProductId"); productId != "" {
			if product, ok := sidecar.ComputeStoreProductOf(self.deps.Products, productId); ok && product.Monthly {
				credits = product.Credits
			}
		}
		if status, ok := appleControllerInt64Claim(notification.RenewalInfo, "autoRenewStatus"); ok && status == 0 {
			renews = false
		}
	}
	if notification.NotificationType == "EXPIRED" {
		renews = false
	}
	answer, err := sidecarcontroller.ComputeMonthlyStoreReport(self.deps.Env(), ctx, self.deps.Ledger, &sidecarcontroller.ComputeStoreSubscriptionReport{
		NetworkId: networkId, Market: sidecarmodel.ComputeMarketAppStore, StoreRef: transaction.originalTransactionId, Credits: credits, Renews: renews,
	})
	if err != nil {
		return fmt.Errorf("compute apple: the plan's report: %w", err)
	}
	if answer.Concurrent {
		glog.Infof("[compute]apple: an App Store compute subscription of %s renews beside its other monthly plan\n", networkId)
	}
	return nil
}

// Whether a verified notification is of a compute product: its transaction's product, else its renewal's.
func computeAppleNotificationOf(notification AppleNotificationDecodedPayload) bool {
	productId, _ := appleControllerStringClaim(notification.TransactionInfo, "productId")
	if productId == "" {
		productId, _ = appleControllerStringClaim(notification.RenewalInfo, "autoRenewProductId")
	}
	return sidecar.ComputeStoreProductFamilyOf(productId)
}

// The App Store Server API's history (v2), an authenticated pull with the reconciler's credentials: the
// subscription's auto-renewable transactions, newest first, at most pageMaxCount pages.
type computeAppleServerTransactions struct{}

// The history API's answer.
type computeAppleHistoryPage struct {
	SignedTransactions []string `json:"signedTransactions"`
	Revision           string   `json:"revision"`
	HasMore            bool     `json:"hasMore"`
}

func (computeAppleServerTransactions) History(ctx context.Context, environment string, transactionId string, pageMaxCount int) ([]map[string]any, error) {
	baseUrl := appleAppStoreServerApiBaseUrl
	if environment == appleEnvironmentSandbox {
		baseUrl = appleAppStoreServerApiSandboxBaseUrl
	}
	claimsList := []map[string]any{}
	revision := ""
	for page := 0; page < pageMaxCount; page += 1 {
		query := url.Values{}
		query.Set("productType", "AUTO_RENEWABLE")
		query.Set("sort", "DESCENDING")
		if revision != "" {
			query.Set("revision", revision)
		}
		historyUrl := fmt.Sprintf("%s/inApps/v2/history/%s?%s", baseUrl, url.PathEscape(transactionId), query.Encode())
		answer, err := server.HttpGetRequireStatusOk[*computeAppleHistoryPage](
			ctx,
			historyUrl,
			func(header http.Header) {
				appleServerApiAuthHeaderFunc(ctx, header)
			},
			server.ResponseJsonObject[*computeAppleHistoryPage],
		)
		if err != nil {
			return nil, err
		}
		for _, signed := range answer.SignedTransactions {
			claims, err := appleDecodeJwsPayload(signed)
			if err != nil {
				return nil, err
			}
			claimsList = append(claimsList, claims)
		}
		if !answer.HasMore || answer.Revision == "" {
			break
		}
		revision = answer.Revision
	}
	return claimsList, nil
}
