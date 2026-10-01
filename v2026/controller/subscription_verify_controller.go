package controller

// Client purchase reporting -- the wave-2 half of the lost-webhook fix
// (UPGRADE.md §4 item 2). The app reports proof of purchase (the Play purchase
// token, the App Store transaction JWS) and the server verifies it with the
// store and credits through the SAME idempotency gates the webhooks and the
// payment reconciler use:
//
//   - play: PlaySubscriptionRenewal -- purchase-token advisory xact lock with
//     the overlap re-checked inside the credit tx, so a client report racing an
//     RTDN delivery or the scheduled renewal task credits exactly once.
//   - apple: appleCreditSubscriptionTransactionInTx -- the
//     apple_subscription_transaction ledger (ON CONFLICT DO NOTHING,
//     rows-affected checked) in the same tx as the credit, so a client report
//     racing a notification or a reconcile pass credits exactly once.
//
// The client contract (sdk PurchaseReportBackoffMillis doc): persist the proof,
// retry with backoff until a TERMINAL status (credited, already_credited,
// wrong_network, invalid), and only then acknowledge (android) / finish()
// (apple). pending and transport/store errors are retryable. Because
// acknowledge/finish happens only after a terminal answer, a lost webhook is no
// longer lost money -- the store keeps redelivering the proof to the client
// until the server has seen it.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Purchase verify statuses. credited, already_credited, wrong_network and
// invalid are TERMINAL: the client stops retrying and finalizes with the
// store. pending is retryable. Transport/store failures are non-2xx (no
// status), also retryable.
const (
	VerifyPurchaseStatusCredited        = "credited"
	VerifyPurchaseStatusAlreadyCredited = "already_credited"
	VerifyPurchaseStatusPending         = "pending"
	VerifyPurchaseStatusInvalid         = "invalid"
	VerifyPurchaseStatusWrongNetwork    = "wrong_network"
)

type VerifyStorePurchaseResult struct {
	Status     string     `json:"status"`
	ExpiryTime *time.Time `json:"expiry_time,omitempty"`
}

func verifyStatusResult(status string) *VerifyStorePurchaseResult {
	return &VerifyStorePurchaseResult{Status: status}
}

// NewVerifyStorePurchaseInvalid is the terminal "this proof will never
// credit" answer, exported for the api/handlers apple JWS verification step.
func NewVerifyStorePurchaseInvalid() *VerifyStorePurchaseResult {
	return verifyStatusResult(VerifyPurchaseStatusInvalid)
}

// CheckVerifyPurchaseRateLimit applies the shared per-account budget for the
// purchase-verify endpoints (they trigger verify-with-store work an abuser
// could spin). Every attempt counts -- checked and recorded atomically -- so
// failed verifies burn budget too. Exported because the apple endpoint checks
// it in api/handlers BEFORE the JWS cryptographic verification.
func CheckVerifyPurchaseRateLimit(clientSession *session.ClientSession) error {
	if err := model.CheckAndRecordAccountActionRateLimit(
		clientSession.Ctx,
		clientSession.ByJwt.UserId,
		model.AccountActionVerifyStorePurchase,
		model.AccountActionVerifyStorePurchaseWindowLimit,
		model.AccountActionVerifyStorePurchaseWindow,
	); err != nil {
		return fmt.Errorf("429 %s", err.Error())
	}
	return nil
}

// ----- play -----

const maxPlayPurchaseTokenLength = 4 * 1024

type VerifyPlayPurchaseArgs struct {
	// PackageName defaults to (and must match) this app's package.
	PackageName string `json:"package_name,omitempty"`
	// ProductId is the sku the client believes it bought. When set it must
	// name one of the token's line items; the credited sku is always the
	// store's, never the client's.
	ProductId     string `json:"product_id"`
	PurchaseToken string `json:"purchase_token"`
}

// VerifyPlayPurchase verifies a client-reported Play purchase token with the
// Android Publisher API (subscriptionsv2) and credits it through
// PlaySubscriptionRenewal -- the same advisory-lock-gated path the RTDN
// webhook and the payment reconciler use, so this is idempotent against both.
//
// decision table (store state -> status):
//
//	token unknown / malformed (400, 404, 410)                    -> invalid
//	linked account (obfuscated/external id) != session network   -> wrong_network
//	SUBSCRIPTION_STATE_PENDING / PAUSED / ON_HOLD / GRACE        -> pending (retry)
//	SUBSCRIPTION_STATE_ACTIVE, credit landed                     -> credited
//	SUBSCRIPTION_STATE_ACTIVE, balance already overlaps expiry   -> already_credited
//	CANCELED / EXPIRED / PENDING_PURCHASE_CANCELED / other       -> invalid
//	Google API failure                                           -> error (non-2xx, retry)
func VerifyPlayPurchase(
	verifyPlayPurchase *VerifyPlayPurchaseArgs,
	clientSession *session.ClientSession,
) (*VerifyStorePurchaseResult, error) {
	// cheap validation first, so malformed requests burn no rate-limit budget
	// and no store API calls
	purchaseToken := verifyPlayPurchase.PurchaseToken
	if purchaseToken == "" || maxPlayPurchaseTokenLength < len(purchaseToken) {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	packageName := playPackageNameFunc()
	if verifyPlayPurchase.PackageName != "" && verifyPlayPurchase.PackageName != packageName {
		return NewVerifyStorePurchaseInvalid(), nil
	}

	if err := CheckVerifyPurchaseRateLimit(clientSession); err != nil {
		return nil, err
	}

	subUrl := fmt.Sprintf(
		"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
		playPublisherApiBaseUrl,
		packageName,
		purchaseToken,
	)
	sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
		clientSession.Ctx,
		subUrl,
		func(header http.Header) {
			playAuthHeaderFunc(clientSession.Ctx, header)
		},
		server.ResponseJsonObject[*PlaySubscription],
	)
	if err != nil {
		if v, ok := err.(*server.HttpStatusError); ok {
			switch v.StatusCode {
			// malformed token, unknown token, purchase gone: this proof will
			// never verify -- terminal
			case 400, 404, 410:
				return NewVerifyStorePurchaseInvalid(), nil
			}
		}
		// credentials/backend trouble on our side: retryable, non-2xx
		return nil, err
	}

	// the wrong-account case: the token's linked network (set by the app via
	// setObfuscatedAccountId at purchase-flow launch) must match the session
	// network where present. The linked network still gets its credit through
	// the webhook/reconciler paths; this session just isn't it.
	linkedNetworkId, ok := playLinkedNetworkId(clientSession, sub)
	if !ok {
		// linkage present but unparseable: not a purchase launched by this app
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if linkedNetworkId != nil && *linkedNetworkId != clientSession.ByJwt.NetworkId {
		glog.Infof(
			"[sub]verify play purchase: token is linked to network %s, session is %s\n",
			*linkedNetworkId,
			clientSession.ByJwt.NetworkId,
		)
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), nil
	}

	switch sub.SubscriptionState {
	case "SUBSCRIPTION_STATE_PENDING",
		"SUBSCRIPTION_STATE_PAUSED",
		"SUBSCRIPTION_STATE_ON_HOLD",
		"SUBSCRIPTION_STATE_IN_GRACE_PERIOD":
		// not entitled yet (or the credit path will not credit it yet), but the
		// state can still resolve to ACTIVE -- keep retrying, do NOT finalize
		return verifyStatusResult(VerifyPurchaseStatusPending), nil
	case "SUBSCRIPTION_STATE_ACTIVE":
		// fall through to credit
	default:
		// CANCELED / EXPIRED / PENDING_PURCHASE_CANCELED / unknown: this report
		// will never credit through this endpoint -- terminal
		return NewVerifyStorePurchaseInvalid(), nil
	}

	if len(sub.LineItems) == 0 {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	// the credited sku is the store's line item; a client-claimed product that
	// matches no line item marks the report inconsistent
	if verifyPlayPurchase.ProductId != "" {
		found := false
		for _, item := range sub.LineItems {
			if item.ProductId == verifyPlayPurchase.ProductId {
				found = true
				break
			}
		}
		if !found {
			return NewVerifyStorePurchaseInvalid(), nil
		}
	}

	// the credit path: re-fetches the subscription, takes the purchase-token
	// advisory lock, and re-checks the overlap inside the credit tx -- the
	// same gate the RTDN webhook and the reconciler go through
	renewalResult, err := PlaySubscriptionRenewal(
		&PlaySubscriptionRenewalArgs{
			NetworkId:      clientSession.ByJwt.NetworkId,
			PackageName:    packageName,
			SubscriptionId: sub.LineItems[0].ProductId,
			PurchaseToken:  purchaseToken,
		},
		clientSession,
	)
	if err != nil {
		return nil, err
	}
	if renewalResult.Canceled {
		// the state flipped between the two fetches
		return NewVerifyStorePurchaseInvalid(), nil
	}
	expiryTime := renewalResult.ExpiryTime
	result := &VerifyStorePurchaseResult{ExpiryTime: &expiryTime}
	if renewalResult.Renewed {
		result.Status = VerifyPurchaseStatusCredited
	} else {
		result.Status = VerifyPurchaseStatusAlreadyCredited
	}
	return result, nil
}

// playLinkedNetworkId resolves the network the purchase token was launched
// for, using the same resolution as PlayWebhook: the external account id, or
// the obfuscated external account id as a subscription payment id falling back
// to a plain network id. nil (with ok) when the token carries no linkage;
// ok=false when linkage is present but unparseable.
func playLinkedNetworkId(
	clientSession *session.ClientSession,
	sub *PlaySubscription,
) (*server.Id, bool) {
	identifiers := sub.ExternalAccountIdentifiers
	if identifiers == nil {
		return nil, true
	}
	if identifiers.ExternalAccountId != "" {
		networkId, err := server.ParseId(identifiers.ExternalAccountId)
		if err != nil {
			return nil, false
		}
		return &networkId, true
	}
	if identifiers.ObfuscatedExternalAccountId != "" {
		networkIdOrSubscriptionPaymentId, err := server.ParseId(identifiers.ObfuscatedExternalAccountId)
		if err != nil {
			return nil, false
		}
		networkId, err := model.SubscriptionGetNetworkIdForPaymentId(clientSession.Ctx, networkIdOrSubscriptionPaymentId)
		if err != nil {
			// the obfuscated account id is just a plain network id
			networkId = networkIdOrSubscriptionPaymentId
		}
		return &networkId, true
	}
	return nil, true
}

// ----- apple -----

type VerifyAppleTransactionArgs struct {
	// SignedTransaction is the StoreKit transaction JWS
	// (Transaction.jwsRepresentation), verified in api/handlers with the full
	// pinned-root webhook verifier before this controller sees claims.
	SignedTransaction string `json:"signed_transaction"`
}

// VerifyAppleTransactionClaims takes ALREADY VERIFIED transaction claims (the
// api/handlers pinned-root verifier ran first -- a client-reported JWS is an
// unauthenticated-content push, so it gets webhook-grade verification, unlike
// the reconciler's authenticated TLS pulls from Apple), validates them with
// the same validator the notification webhook uses, requires appAccountToken
// == the session network, and credits through
// appleCreditSubscriptionTransactionInTx -- the apple_subscription_transaction
// ledger gate shared with ProcessAppleNotification and the reconciler, so this
// is idempotent against both.
//
// decision table (claims -> status):
//
//	validation fails (account token, product, dates, price) -> invalid
//	appAccountToken != session network                      -> wrong_network
//	ledger insert landed                                    -> credited
//	transaction already in the ledger                       -> already_credited
//
// There is no apple pending: a client only holds a transaction JWS once the
// purchase completed (an Ask to Buy in progress has no transaction to report).
func VerifyAppleTransactionClaims(
	transactionClaims map[string]any,
	allowedProductIds []string,
	clientSession *session.ClientSession,
) (*VerifyStorePurchaseResult, error) {
	// SignedDate here is only the "claims are being judged now" anchor for the
	// purchase-date plausibility window; the JWS's own signedDate was already
	// used by the handler for certificate-chain validity
	notification := AppleNotificationDecodedPayload{
		SignedDate:      server.NowUtc().UnixMilli(),
		TransactionInfo: transactionClaims,
	}
	transaction, err := validateAppleTransaction(notification, allowedProductIds, true)
	if err != nil {
		glog.Infof("[sub]verify apple transaction: %s\n", err)
		return NewVerifyStorePurchaseInvalid(), nil
	}

	if transaction.networkId != clientSession.ByJwt.NetworkId {
		glog.Infof(
			"[sub]verify apple transaction: appAccountToken names network %s, session is %s\n",
			transaction.networkId,
			clientSession.ByJwt.NetworkId,
		)
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), nil
	}

	credited := false
	var creditErr error
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		credited = false
		creditErr = nil
		if !appleNetworkExistsInTx(tx, clientSession.Ctx, transaction.networkId) {
			creditErr = fmt.Errorf("session network does not exist")
			return
		}
		// the ledger row's notification_uuid column is only a provenance
		// pointer; a client report has no notification, so it gets a fresh id
		// (the reconciler does the same)
		credited = appleCreditSubscriptionTransactionInTx(tx, clientSession.Ctx, server.NewId(), transaction)
	})
	if creditErr != nil {
		return nil, creditErr
	}

	if credited {
		model.UpdateProNetwork(clientSession.Ctx, transaction.networkId)
	}

	expiryTime := transaction.expiresTime
	result := &VerifyStorePurchaseResult{ExpiryTime: &expiryTime}
	if credited {
		result.Status = VerifyPurchaseStatusCredited
	} else {
		result.Status = VerifyPurchaseStatusAlreadyCredited
	}
	return result, nil
}
