package controller

// ComputePlayStore (DESIGN-COMPUTE §9.5, §9.11, §9.12, §14.3, D-BAL-8; BAL-M12-10): Google Play's compute.* products,
// whose purchases reach the server two ways, each read back from the Android Publisher API before anything is
// credited, so neither the app's report nor a push is trusted for what was bought:
//
//	the app's report   POST api/subscription/verify-play-purchase with a compute.* product (VerifyPlayPurchase routes
//	                   it here after its rate limit): a consumable read through purchases.products, credited once by
//	                   its order id, then consumed (purchases.products.consume) at once, well inside the three days
//	                   after which Play refunds a purchase never acknowledged, and never before the credit commits; a
//	                   subscription read through subscriptionsv2, each period credited once by its own order id and
//	                   then acknowledged; a pending purchase answers pending, which the app reports again
//	the RTDNs          PlayWebhook routes compute's here: oneTimeProductNotification (a consumable purchased),
//	                   subscriptionNotification (SUBSCRIPTION_PURCHASED, SUBSCRIPTION_RENEWED, SUBSCRIPTION_RECOVERED
//	                   and SUBSCRIPTION_RESTARTED credit the period whose order they name; SUBSCRIPTION_REVOKED reverses
//	                   it), and voidedPurchaseNotification, which reverses the lot of a compute order
//
// A subscription is credited by each period's order id, never its purchase token, which renewals share: a key on the
// token would credit a subscription once ever. A replacement (its linkedPurchaseToken) grants the new plan's credits
// less the old plan's for the part of its period Play gives back (computeStorePlanChange), the old subscription read
// from subscriptionsv2 by the linked token.
//
// The network is the one the obfuscated account id names, which the app sets at purchase, as a subscription payment id
// or a plain network id, as Pro's purchases resolve it. A purchase naming none binds as Pro's do (play_purchase_binding,
// by its token, a replacement inheriting its linked token's binding): to the network whose app reports it first, a
// notification of one no report has bound waiting for that report. A network that is gone credits nothing and its order
// is refunded through Play's orders API. Play names no account, so its purchases count to the network alone for the
// limits (§9.11). A license tester's purchase credits only where the settings say so, since no money moves.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The Play compute paths' settings.
type ComputePlayStoreSettings struct {
	// Credit a license tester's purchase (purchaseType test, or a subscription's testPurchase): false, since no money
	// moves; a staging env whose test app adds credit in the sandbox sets it (§19's M12a exit).
	TestCredit bool
}

// The defaults, the only place one is set.
func DefaultComputePlayStoreSettings() *ComputePlayStoreSettings {
	return &ComputePlayStoreSettings{
		TestCredit: false,
	}
}

// Where the paths reach the Android Publisher API. A field left nil takes the process's own, which Pro's hermetic
// tests replace: playPublisherApiBaseUrl, playPackageNameFunc and playAuthHeaderFunc.
type ComputePlayApi struct {
	BaseUrl     func() string
	PackageName func() string
	AuthHeader  func(ctx context.Context, header http.Header)
}

// Google Play's compute paths (see the file header). Safe for concurrent use.
type ComputePlayStore struct {
	settings *ComputePlayStoreSettings
	deps     ComputeStoreDeps
	api      ComputePlayApi
}

// The paths over settings, deps and api, nil taking the defaults and the process's own.
func NewComputePlayStore(settings *ComputePlayStoreSettings, deps *ComputeStoreDeps, api *ComputePlayApi) *ComputePlayStore {
	if settings == nil {
		settings = DefaultComputePlayStoreSettings()
	}
	self := &ComputePlayStore{settings: settings, deps: computeStoreDepsOf(deps)}
	if api != nil {
		self.api = *api
	}
	if self.api.BaseUrl == nil {
		self.api.BaseUrl = func() string { return playPublisherApiBaseUrl }
	}
	if self.api.PackageName == nil {
		self.api.PackageName = func() string { return playPackageNameFunc() }
	}
	if self.api.AuthHeader == nil {
		self.api.AuthHeader = func(ctx context.Context, header http.Header) { playAuthHeaderFunc(ctx, header) }
	}
	return self
}

// The process's paths, which the verify route and the RTDN webhook share. Replaceable only by the hermetic tests;
// production never mutates it.
var computePlayStoreProcess = sync.OnceValue(func() *ComputePlayStore {
	return NewComputePlayStore(nil, nil, nil)
})

// Play's words the paths read: identifiers of its API.
const (
	// purchases.products' purchaseState and consumptionState, and purchaseType's license test.
	computePlayPurchased      = 0
	computePlayPending        = 2
	computePlayConsumed       = 1
	computePlayPurchaseTest   = 0
	computePlayAcknowledged   = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
	computePlayStateActive    = "SUBSCRIPTION_STATE_ACTIVE"
	computePlayStatePending   = "SUBSCRIPTION_STATE_PENDING"
	computePlayStateGrace     = "SUBSCRIPTION_STATE_IN_GRACE_PERIOD"
	computePlayStateCancelled = "SUBSCRIPTION_STATE_CANCELED"
	// RTDN's types: a one-time product purchased; a subscription recovered, renewed, purchased, restarted or revoked.
	computePlayOneTimePurchased       = 1
	computePlaySubscriptionRecovered  = 1
	computePlaySubscriptionRenewed    = 2
	computePlaySubscriptionPurchased  = 4
	computePlaySubscriptionRestarted  = 7
	computePlaySubscriptionRevoked    = 12
	computePlayVoidedSubscriptionType = 1
	// What a binding made by a compute purchase records as its offer: an identifier.
	computePlayBindingOffer = "compute"
)

// purchases.products' answer, the fields the paths read.
type computePlayProductPurchase struct {
	PurchaseTimeMillis          string `json:"purchaseTimeMillis"`
	PurchaseState               *int   `json:"purchaseState"`
	ConsumptionState            *int   `json:"consumptionState"`
	OrderId                     string `json:"orderId"`
	ObfuscatedExternalAccountId string `json:"obfuscatedExternalAccountId"`
	PurchaseType                *int   `json:"purchaseType"`
}

// subscriptionsv2's answer, the fields the paths read beside Pro's PlaySubscription.
type computePlaySubscription struct {
	PlaySubscription
	TestPurchase *struct{} `json:"testPurchase,omitempty"`
}

// The RTDN messages beside a subscription's (PlayRtdnMessage).
type PlayOneTimeProductNotification struct {
	Version          string `json:"version"`
	NotificationType int    `json:"notificationType"`
	PurchaseToken    string `json:"purchaseToken"`
	Sku              string `json:"sku"`
}

type PlayVoidedPurchaseNotification struct {
	PurchaseToken string `json:"purchaseToken"`
	OrderId       string `json:"orderId"`
	ProductType   int    `json:"productType"`
	RefundType    int    `json:"refundType"`
}

// The publisher url of path under the app's package.
func (self *ComputePlayStore) url(path string) string {
	return fmt.Sprintf("%s/androidpublisher/v3/applications/%s/%s", self.api.BaseUrl(), url.PathEscape(self.api.PackageName()), path)
}

// A store answer that names no such purchase: Play's 400, 404 and 410.
func computePlayGone(err error) bool {
	var statusErr *server.HttpStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

// A consumable's purchase as purchases.products holds it.
func (self *ComputePlayStore) product(ctx context.Context, productId string, purchaseToken string) (*computePlayProductPurchase, error) {
	return server.HttpGetRequireStatusOk[*computePlayProductPurchase](
		ctx,
		self.url(fmt.Sprintf("purchases/products/%s/tokens/%s", url.PathEscape(productId), url.PathEscape(purchaseToken))),
		func(header http.Header) { self.api.AuthHeader(ctx, header) },
		server.ResponseJsonObject[*computePlayProductPurchase],
	)
}

// A subscription as subscriptionsv2 holds it.
func (self *ComputePlayStore) subscription(ctx context.Context, purchaseToken string) (*computePlaySubscription, error) {
	return server.HttpGetRequireStatusOk[*computePlaySubscription](
		ctx,
		self.url(fmt.Sprintf("purchases/subscriptionsv2/tokens/%s", url.PathEscape(purchaseToken))),
		func(header http.Header) { self.api.AuthHeader(ctx, header) },
		server.ResponseJsonObject[*computePlaySubscription],
	)
}

// A POST to path with no body, answered 2xx.
func (self *ComputePlayStore) post(ctx context.Context, path string) error {
	_, err := server.HttpPostRawRequireStatusOk(ctx, self.url(path), []byte{}, func(header http.Header) { self.api.AuthHeader(ctx, header) })
	return err
}

// Consumes a consumable's purchase (purchases.products.consume): what tells Play the credit is given, and only after
// its commit. ComputePlayConsume.
func (self *ComputePlayStore) ComputePlayConsume(ctx context.Context, productId string, purchaseToken string) error {
	if err := self.post(ctx, fmt.Sprintf("purchases/products/%s/tokens/%s:consume", url.PathEscape(productId), url.PathEscape(purchaseToken))); err != nil {
		return fmt.Errorf("compute play: the consume of %s: %w", productId, err)
	}
	return nil
}

// Acknowledges a subscription's purchase, unless Play holds it acknowledged.
func (self *ComputePlayStore) acknowledge(ctx context.Context, productId string, purchaseToken string, sub *computePlaySubscription) error {
	if sub.AcknowledgementState == computePlayAcknowledged {
		return nil
	}
	if err := self.post(ctx, fmt.Sprintf("purchases/subscriptions/%s/tokens/%s:acknowledge", url.PathEscape(productId), url.PathEscape(purchaseToken))); err != nil {
		return fmt.Errorf("compute play: the acknowledge of %s: %w", productId, err)
	}
	return nil
}

// Refunds orderId through Play's orders API, revoking what it bought: a gone network's purchase.
func (self *ComputePlayStore) refund(ctx context.Context, orderId string) error {
	if err := self.post(ctx, fmt.Sprintf("orders/%s:refund?revoke=true", url.PathEscape(orderId))); err != nil {
		return fmt.Errorf("compute play: the refund of %s: %w", orderId, err)
	}
	return nil
}

// The network an obfuscated account id names, a subscription payment id or a plain network id; nil where it names
// none; an error where it names one that is not an id.
func computePlayNamedNetworkId(ctx context.Context, obfuscatedAccountId string) (*server.Id, error) {
	if obfuscatedAccountId == "" {
		return nil, nil
	}
	named, err := server.ParseId(obfuscatedAccountId)
	if err != nil {
		return nil, errors.New("compute play: an obfuscated account id that names no network")
	}
	var networkId server.Id
	var lookupErr error
	server.HandleError(func() {
		networkId, lookupErr = model.SubscriptionGetNetworkIdForPaymentIdInConn(nil, ctx, named)
	}, func(err error) {
		lookupErr = err
	})
	if lookupErr != nil {
		// the obfuscated account id is a plain network id
		networkId = named
	}
	return &networkId, nil
}

// The network purchaseToken is bound to, its linked token's binding inherited; nil where none is.
func computePlayBoundNetworkId(ctx context.Context, purchaseToken string, linkedPurchaseToken string) (*server.Id, error) {
	var bound *server.Id
	var returnErr error
	server.HandleError(func() {
		if networkId, ok := model.ResolvePlayPurchaseBinding(ctx, purchaseToken, linkedPurchaseToken, true); ok {
			bound = &networkId
		}
	}, func(err error) {
		returnErr = err
	})
	return bound, returnErr
}

// Binds purchaseToken to networkId where nothing bound it, and answers the network it is bound to: the first report's.
func computePlayBind(ctx context.Context, purchaseToken string, networkId server.Id) (*server.Id, error) {
	var bound *server.Id
	var returnErr error
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			model.BindPlayPurchaseInTx(tx, ctx, purchaseToken, purchaseToken, networkId, computePlayBindingOffer)
			if boundNetworkId, _, ok := model.GetPlayPurchaseBindingInTx(tx, ctx, purchaseToken); ok {
				bound = &boundNetworkId
			}
		})
	}, func(err error) {
		returnErr = err
	})
	return bound, returnErr
}

// The network a purchase is for: the obfuscated account id's, else the binding's, else, for a report, the reporting
// network, which the report binds. nil for a notification nothing names yet.
func (self *ComputePlayStore) networkOf(ctx context.Context, obfuscatedAccountId string, purchaseToken string, linkedPurchaseToken string, reporter *server.Id) (*server.Id, error) {
	networkId, err := computePlayNamedNetworkId(ctx, obfuscatedAccountId)
	if err != nil || networkId != nil {
		return networkId, err
	}
	if networkId, err = computePlayBoundNetworkId(ctx, purchaseToken, linkedPurchaseToken); err != nil || networkId != nil {
		return networkId, err
	}
	if reporter == nil {
		return nil, nil
	}
	return computePlayBind(ctx, purchaseToken, *reporter)
}

// What a credit of a Play purchase came to.
type computePlayOutcome struct {
	status string
	// processed: this call credited
	credited bool
}

// Credits a consumable's purchase, then consumes it (see the file header). reporter is the reporting network, nil for
// a notification.
func (self *ComputePlayStore) consumable(ctx context.Context, product *sidecar.ComputeStoreProduct, purchaseToken string, reporter *server.Id) (*computePlayOutcome, error) {
	purchase, err := self.product(ctx, product.ProductId, purchaseToken)
	if computePlayGone(err) {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	if err != nil {
		return nil, err
	}
	if purchase.PurchaseState == nil || purchase.OrderId == "" {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	switch *purchase.PurchaseState {
	case computePlayPurchased:
	case computePlayPending:
		return &computePlayOutcome{status: VerifyPurchaseStatusPending}, nil
	default:
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	if purchase.PurchaseType != nil && *purchase.PurchaseType == computePlayPurchaseTest && !self.settings.TestCredit {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	networkId, err := self.networkOf(ctx, purchase.ObfuscatedExternalAccountId, purchaseToken, "", reporter)
	if err != nil {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	if networkId == nil {
		// nothing names its network until the app's report binds it: Pub/Sub delivers it again
		return nil, errors.New("compute play: a purchase no account id or binding names")
	}
	if reporter != nil && *networkId != *reporter {
		return &computePlayOutcome{status: VerifyPurchaseStatusWrongNetwork}, nil
	}
	answer, err := ComputeStoreCredit(self.deps.Env(), ctx, &self.deps, &ComputeStorePurchase{
		Market:        sidecarmodel.ComputeMarketGooglePlay,
		NetworkId:     *networkId,
		MarketRef:     purchase.OrderId,
		Product:       product,
		AmountNanoUsd: product.Credits * sidecarmodel.ComputeCreditNanoUsd,
	})
	if err != nil {
		return nil, err
	}
	if answer.Gone {
		return self.gone(ctx, purchase.OrderId, networkId)
	}
	// the credit committed: only now is the purchase consumed
	if purchase.ConsumptionState == nil || *purchase.ConsumptionState != computePlayConsumed {
		if err := self.ComputePlayConsume(ctx, product.ProductId, purchaseToken); err != nil {
			return nil, err
		}
	}
	if answer.Credited {
		return &computePlayOutcome{status: VerifyPurchaseStatusCredited, credited: true}, nil
	}
	return &computePlayOutcome{status: VerifyPurchaseStatusAlreadyCredited}, nil
}

// A gone network's purchase: refunded through Play, and recorded for support where the refund fails.
func (self *ComputePlayStore) gone(ctx context.Context, orderId string, networkId *server.Id) (*computePlayOutcome, error) {
	if err := self.refund(ctx, orderId); err != nil {
		computeStoreRecordUnowned(ctx, model.SubscriptionMarketGoogle, orderId, "destination_deleted_refund_failed", networkId)
		return nil, err
	}
	return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
}

// The order a subscription's current period was paid by: its line item's latest successful order, else the latest
// order.
func computePlayPeriodOrderId(sub *computePlaySubscription) string {
	if 0 < len(sub.LineItems) && sub.LineItems[0] != nil && sub.LineItems[0].LatestSuccessfulOrderId != "" {
		return sub.LineItems[0].LatestSuccessfulOrderId
	}
	return sub.LatestOrderId
}

// What a subscription's current period credits: its plan's credits, or, for a replacement's first period, the
// difference from the plan its linked token held (see the file header).
func (self *ComputePlayStore) periodGrant(ctx context.Context, product *sidecar.ComputeStoreProduct, sub *computePlaySubscription, orderId string) (int64, error) {
	full := product.Credits * sidecarmodel.ComputeCreditNanoUsd
	if sub.LinkedPurchaseToken == "" || computePlayRenewalOrder(orderId) {
		return full, nil
	}
	start, err := time.Parse(time.RFC3339, sub.StartTime)
	if err != nil {
		return 0, fmt.Errorf("compute play: the replacement's start: %w", err)
	}
	old, err := self.subscription(ctx, sub.LinkedPurchaseToken)
	if computePlayGone(err) {
		return full, nil
	}
	if err != nil {
		return 0, err
	}
	if len(old.LineItems) == 0 || old.LineItems[0] == nil {
		return full, nil
	}
	oldProduct, ok := sidecar.ComputeStoreProductOf(self.deps.Products, old.LineItems[0].ProductId)
	if !ok || !oldProduct.Monthly {
		return full, nil
	}
	oldEnd, err := old.LineItems[0].ParseExpiryTime()
	if err != nil {
		return 0, fmt.Errorf("compute play: the replaced plan's expiry: %w", err)
	}
	return computeStorePlanChange(product.Credits, oldProduct.Credits, oldEnd.AddDate(0, -1, 0), oldEnd, start), nil
}

// Whether orderId is a renewal's: Play suffixes a subscription's renewal orders with ..<n>.
func computePlayRenewalOrder(orderId string) bool {
	for i := 0; i+1 < len(orderId); i += 1 {
		if orderId[i] == '.' && orderId[i+1] == '.' {
			return true
		}
	}
	return false
}

// Credits a subscription's current period, then acknowledges the purchase (see the file header). reporter is the
// reporting network, nil for a notification.
func (self *ComputePlayStore) period(ctx context.Context, purchaseToken string, reporter *server.Id) (*computePlayOutcome, error) {
	sub, err := self.subscription(ctx, purchaseToken)
	if computePlayGone(err) {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(sub.LineItems) == 0 || sub.LineItems[0] == nil {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	product, ok := sidecar.ComputeStoreProductOf(self.deps.Products, sub.LineItems[0].ProductId)
	if !ok || !product.Monthly {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	switch sub.SubscriptionState {
	case computePlayStateActive, computePlayStateCancelled:
		// a cancelled subscription keeps the period it paid for
	case computePlayStatePending, computePlayStateGrace:
		return &computePlayOutcome{status: VerifyPurchaseStatusPending}, nil
	default:
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	if sub.TestPurchase != nil && !self.settings.TestCredit {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	orderId := computePlayPeriodOrderId(sub)
	if orderId == "" {
		return &computePlayOutcome{status: VerifyPurchaseStatusPending}, nil
	}
	obfuscated := ""
	if sub.ExternalAccountIdentifiers != nil {
		obfuscated = sub.ExternalAccountIdentifiers.ObfuscatedExternalAccountId
		if sub.ExternalAccountIdentifiers.ExternalAccountId != "" {
			obfuscated = sub.ExternalAccountIdentifiers.ExternalAccountId
		}
	}
	networkId, err := self.networkOf(ctx, obfuscated, purchaseToken, sub.LinkedPurchaseToken, reporter)
	if err != nil {
		return &computePlayOutcome{status: VerifyPurchaseStatusInvalid}, nil
	}
	if networkId == nil {
		return nil, errors.New("compute play: a subscription no account id or binding names")
	}
	if reporter != nil && *networkId != *reporter {
		return &computePlayOutcome{status: VerifyPurchaseStatusWrongNetwork}, nil
	}
	amount, err := self.periodGrant(ctx, product, sub, orderId)
	if err != nil {
		return nil, err
	}
	answer, err := ComputeStoreCredit(self.deps.Env(), ctx, &self.deps, &ComputeStorePurchase{
		Market:        sidecarmodel.ComputeMarketGooglePlay,
		NetworkId:     *networkId,
		MarketRef:     orderId,
		Product:       product,
		AmountNanoUsd: amount,
	})
	if err != nil {
		return nil, err
	}
	if answer.Gone {
		return self.gone(ctx, orderId, networkId)
	}
	if err := self.acknowledge(ctx, product.ProductId, purchaseToken, sub); err != nil {
		return nil, err
	}
	if answer.Credited {
		return &computePlayOutcome{status: VerifyPurchaseStatusCredited, credited: true}, nil
	}
	return &computePlayOutcome{status: VerifyPurchaseStatusAlreadyCredited}, nil
}

// The app's report of a compute purchase (see the file header), from clientSession's network.
func (self *ComputePlayStore) VerifyPurchase(args *VerifyPlayPurchaseArgs, clientSession *session.ClientSession) (*VerifyStorePurchaseResult, error) {
	product, ok := sidecar.ComputeStoreProductOf(self.deps.Products, args.ProductId)
	if !ok {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	reporter := clientSession.ByJwt.NetworkId
	var outcome *computePlayOutcome
	var err error
	if product.Monthly {
		outcome, err = self.period(clientSession.Ctx, args.PurchaseToken, &reporter)
	} else {
		outcome, err = self.consumable(clientSession.Ctx, product, args.PurchaseToken, &reporter)
	}
	if err != nil {
		return nil, err
	}
	return verifyStatusResult(outcome.status), nil
}

// A compute RTDN, PlayWebhook's: handled is false for a message of none of compute's products, which goes on to
// Pro's paths.
func (self *ComputePlayStore) Rtdn(ctx context.Context, message *PlayRtdnMessage) (handled bool, returnErr error) {
	switch {
	case message.OneTimeProductNotification != nil:
		notification := message.OneTimeProductNotification
		product, ok := sidecar.ComputeStoreProductOf(self.deps.Products, notification.Sku)
		if !ok {
			return sidecar.ComputeStoreProductFamilyOf(notification.Sku), nil
		}
		if notification.NotificationType != computePlayOneTimePurchased || product.Monthly {
			return true, nil
		}
		_, err := self.consumable(ctx, product, notification.PurchaseToken, nil)
		return true, err
	case message.SubscriptionNotification != nil:
		notification := message.SubscriptionNotification
		if !sidecar.ComputeStoreProductFamilyOf(notification.SubscriptionId) {
			return false, nil
		}
		switch notification.NotificationType {
		case computePlaySubscriptionPurchased, computePlaySubscriptionRenewed, computePlaySubscriptionRecovered, computePlaySubscriptionRestarted:
			_, err := self.period(ctx, notification.PurchaseToken, nil)
			return true, err
		case computePlaySubscriptionRevoked:
			return true, self.revoked(ctx, notification.PurchaseToken)
		}
		return true, nil
	case message.VoidedPurchaseNotification != nil:
		notification := message.VoidedPurchaseNotification
		if notification.OrderId == "" || !self.deps.Active() {
			return false, nil
		}
		lot, err := self.lotOf(ctx, notification.OrderId)
		if err != nil {
			return true, err
		}
		if lot == nil {
			// no compute lot: Pro's, or a compute order never credited
			return false, nil
		}
		_, err = ComputeStoreReverse(self.deps.Env(), ctx, &self.deps, sidecarmodel.ComputeMarketGooglePlay, notification.OrderId)
		return true, err
	}
	return false, nil
}

// A revoked subscription: the lot of the order of its current period reversed.
func (self *ComputePlayStore) revoked(ctx context.Context, purchaseToken string) error {
	sub, err := self.subscription(ctx, purchaseToken)
	if computePlayGone(err) {
		glog.Infof("[compute]play revoked subscription is gone from the store; its voided purchase reverses it\n")
		return nil
	}
	if err != nil {
		return err
	}
	orderId := computePlayPeriodOrderId(sub)
	if orderId == "" {
		return nil
	}
	_, err = ComputeStoreReverse(self.deps.Env(), ctx, &self.deps, sidecarmodel.ComputeMarketGooglePlay, orderId)
	return err
}

// The ledger's Play lot of orderId, nil where none was credited.
func (self *ComputePlayStore) lotOf(ctx context.Context, orderId string) (*sidecarmodel.ComputeLot, error) {
	env := self.deps.Env()
	db, err := env.Db()
	if err != nil {
		return nil, err
	}
	var lot *sidecarmodel.ComputeLot
	err = db.Tx(ctx, func(tx operator.DbTx) error {
		var err error
		lot, err = sidecarmodel.ComputeLotByMarketRefInTx(env, ctx, tx, sidecarmodel.ComputeMarketGooglePlay, orderId, false)
		return err
	}, operator.DbIsolationReadCommitted, operator.DbReadOnly)
	return lot, err
}
