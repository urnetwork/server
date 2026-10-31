package controller

// Transactional App Store entitlement processing. Inputs are the already
// verified nested JWS claims produced by api/handlers; notification and
// transaction ledgers make retries idempotent.
//
// A transaction's price is in milliunits of its own ISO 4217 currency, while
// net revenue (subscription_renewal, transfer_balance) is USD. Only a USD price
// converts. Any other currency records the paid marker, and the transaction
// ledger keeps its price and currency. The signed transaction carries no
// proceeds, and Apple directs revenue accounting to App Store Connect reports.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const appleMaximumSubscriptionDuration = 2 * 366 * 24 * time.Hour

// How far a transaction's purchaseDate may lie after the moment its claims are
// judged (the notification's signedDate, or now for the verify endpoint and
// the reconciler). A purchase the customer made is dated when it was made, so
// only clock skew separates the two. A renewal is not: the App Store charges
// it ahead of the expiry (up to a day), and its purchaseDate is the start of
// the period it pays for, the previous transaction's expiresDate. On Main the
// renewals of 2026-10-17 and 2026-10-26 were charged about 8 hours before
// their period started, and every check in that span refused them as
// implausible: the DID_RENEW webhook, and the reconciler until the period
// began. Since the transaction ledger started (2026-08-11) the webhook never
// credited an ordinary production renewal; only a billing recovery, whose
// purchaseDate is its charge.
const appleMaximumPurchaseLead = 5 * time.Minute
const appleMaximumRenewalLead = 48 * time.Hour

// appleTransactionPurchaseLead is the lead the transaction's purchaseDate may
// have: a renewal's, when the claims name the transaction a RENEWAL or, when
// they name no reason, when it is not the subscription's first transaction
// (as appleSalesKind reads it).
func appleTransactionPurchaseLead(transaction *validatedAppleTransaction) time.Duration {
	switch transaction.transactionReason {
	case "RENEWAL":
		return appleMaximumRenewalLead
	case "":
		if transaction.originalTransactionId != "" && transaction.originalTransactionId != transaction.transactionId {
			return appleMaximumRenewalLead
		}
	}
	return appleMaximumPurchaseLead
}

// Apple's highest USD price, $10,000. It bounds only a USD price: a price in
// another currency is not dollars (a year of Pro in KRW is about 66,000 units)
// and records no USD revenue to bound.
const appleMaximumUsdPriceMilliunits int64 = 10_000_000

// The only currency whose price is recorded as net revenue.
const appleUsdCurrency = "USD"

// The App Store subscriptions the apps sold before the 2026 price tier
// (supporter_monthly_26 and supporter_yearly_26, from 2026-02): "supporter",
// the monthly plan from 2025-02, and "supporter_yearly", the yearly plan from
// 2025-10. Their subscribers keep renewing on them, so every path that
// credits a verified transaction (the notification webhook, the verify
// endpoint and the payment reconciler) accepts them next to the configured
// product_ids. Without them each such renewal was refused, and the subscriber
// dropped to Free while the App Store kept charging.
var appleLegacySupporterProductIds = []string{"supporter", "supporter_yearly"}

// appleProductAllowed reports whether a verified transaction's product is a
// Pro subscription this server credits.
func appleProductAllowed(allowedProductIds []string, productId string) bool {
	return slices.Contains(allowedProductIds, productId) ||
		slices.Contains(appleLegacySupporterProductIds, productId)
}

type validatedAppleTransaction struct {
	networkId     server.Id
	transactionId string
	productId     string
	purchaseTime  time.Time
	expiresTime   time.Time
	// USD. Only a USD price converts; a price in any other currency, or with no
	// currency, records the paid marker so that the balance stays paid.
	netRevenue model.NanoCents
	// the transaction price in milliunits of currency, the three-letter ISO
	// 4217 code from the claims ("" when they name none or a malformed one)
	priceMilliunits int64
	currency        string
	// storefront is the App Store storefront (ISO alpha-3), which prices the
	// regional tier; the offer fields say whether the purchase used an offer
	// (offerType 3 = offer code) and which one
	storefront        string
	offerType         int64
	offerIdentifier   string
	offerDiscountType string
	// originalTransactionId names the subscription across renewals; an
	// offer-code binding (apple_offer_code_binding) is keyed on it
	originalTransactionId string
	// transactionReason is PURCHASE (the customer bought) or RENEWAL (the
	// store renewed), "" when the claims do not say; read for the sales notice
	transactionReason string
	// environment is the App Store environment the signed transaction names
	// ("Production", "Sandbox"), "" when the claims do not say. Every path that
	// reads the claims verified them first: the webhook and the verify
	// endpoint against the pinned Apple roots and apple.yml's accepted
	// environments, the reconciler as an authenticated pull from Apple.
	environment string
	// unboundAccountToken is set when the claims carry no appAccountToken at
	// all (an offer-code redemption through the App Store redeem sheet or
	// link). networkId is then zero until the binding resolves it.
	unboundAccountToken bool
}

// sandbox reports whether the transaction is a sandbox purchase: TestFlight,
// App Review or a sandbox tester. No money moves for it.
func (self *validatedAppleTransaction) sandbox() bool {
	return self.environment == appleEnvironmentSandbox
}

// creditNetRevenue is the net revenue a credit records for the transaction:
// zero for a sandbox purchase, which keeps its Pro entitlement but earned
// nothing, and the validated net revenue otherwise.
func (self *validatedAppleTransaction) creditNetRevenue() model.NanoCents {
	if self.sandbox() {
		return 0
	}
	return self.netRevenue
}

// Returns true only when this call committed a new entitlement. Valid retries
// return false and are acknowledged without adding balances a second time.
func ProcessAppleNotification(
	ctx context.Context,
	notification AppleNotificationDecodedPayload,
	allowedProductIds []string,
) (processed bool, returnErr error) {
	notificationId, err := server.ParseId(notification.NotificationUUID)
	if err != nil {
		return false, errors.New("invalid notification UUID")
	}

	actionable := notification.NotificationType == "SUBSCRIBED" || notification.NotificationType == "DID_RENEW"
	// REFUND / REVOKE: Apple returned the money (or revoked family-shared
	// access) -- the transaction's entitlement must END now (UPGRADE.md §2 S7).
	// Verified through the SAME pinned-root path as SUBSCRIBED/DID_RENEW (this
	// function only ever sees already-verified claims), and the transaction is
	// validated the same way minus the entitlement fields a revocation payload
	// need not carry.
	revocation := notification.NotificationType == "REFUND" || notification.NotificationType == "REVOKE"
	var transaction *validatedAppleTransaction
	if notification.TransactionInfo != nil {
		transaction, err = validateAppleTransactionBound(ctx, notification, allowedProductIds, actionable)
		if err != nil {
			if actionable && errors.Is(err, errAppleAccountTokenInvalid) {
				appleRecordUnownedTransaction(ctx, notification, "destination_unresolved", nil)
			}
			return false, err
		}
	} else if actionable || revocation || notification.NotificationType == "EXPIRED" {
		return false, errors.New("missing verified transaction claims")
	}

	var revokedNetworkIds []server.Id
	server.Tx(ctx, func(tx server.PgTx) {
		processed = false
		returnErr = nil
		revokedNetworkIds = nil

		if transaction != nil {
			if !appleNetworkExistsInTx(tx, ctx, transaction.networkId) {
				returnErr = errAppleAccountNetworkMissing
				return
			}
		}

		notificationTag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO apple_notification (
					notification_uuid,
					notification_type,
					subtype,
					signed_date
				)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING
			`,
			notificationId,
			notification.NotificationType,
			notification.Subtype,
			time.UnixMilli(notification.SignedDate),
		))
		if notificationTag.RowsAffected() == 0 || !(actionable || revocation) {
			return
		}

		if revocation {
			// End the transaction's renewal and the pro balance it granted; the
			// apple_subscription_transaction ledger row STAYS as history (it is
			// the idempotency gate for the credit, and the credit did happen).
			// Idempotent two ways: the notification ledger above absorbs a
			// redelivery, and the end itself only touches windows still in the
			// future -- so a REFUND after a REVOKE (or after natural expiry)
			// ends nothing twice.
			revokedNetworkIds = model.EndReconciledEntitlementForTransactionsInTx(
				tx,
				ctx,
				model.SubscriptionMarketApple,
				[]string{transaction.transactionId},
				server.NowUtc(),
			)
			if eventErr := model.AddPaymentReconciliationEventInTx(tx, ctx, &model.PaymentReconciliationEvent{
				RunId:     server.NewId(),
				Store:     model.SubscriptionMarketApple,
				NetworkId: &transaction.networkId,
				Action:    model.PaymentReconcileActionRevoked,
				Evidence:  transaction.transactionId,
				Details: map[string]any{
					"notification_type": notification.NotificationType,
					"subtype":           notification.Subtype,
					"ended":             0 < len(revokedNetworkIds),
				},
			}); eventErr != nil {
				// the event commits with the clawback or neither does; Apple
				// retries the notification
				server.Raise(eventErr)
			}
			return
		}

		processed = appleCreditSubscriptionTransactionInTx(tx, ctx, notificationId, transaction)
	}, server.TxReadCommitted, server.OptNoRetry())

	if processed {
		model.UpdateProNetwork(ctx, transaction.networkId)
	}
	for _, networkId := range revokedNetworkIds {
		// the entitlement ended under the network -- make the downgrade visible
		// immediately rather than after ProCacheTtl
		model.UpdateProNetwork(ctx, networkId)
	}
	if actionable && errors.Is(returnErr, errAppleAccountNetworkMissing) {
		appleRecordUnownedTransaction(ctx, notification, "destination_deleted", &transaction.networkId)
	}
	if returnErr == nil && transaction != nil {
		appleRecordOnboardingOutcome(ctx, notification.NotificationType, transaction, processed, revokedNetworkIds)
	}
	return processed, returnErr
}

// The verified transaction names no network this server can credit: no
// appAccountToken and no offer-code binding, or a token that is not a network
// id. The message is the one these refusals always logged.
var errAppleAccountTokenInvalid = errors.New("invalid App Store account token")

// The verified transaction's appAccountToken names a network that does not
// exist (deleted).
var errAppleAccountNetworkMissing = errors.New("App Store account token does not name an existing network")

// appleRecordUnownedTransactionFunc is the durable-record seam for paid App
// Store transactions no live network can be credited for. Replaceable only by
// hermetic tests; production never mutates it.
var appleRecordUnownedTransactionFunc = model.AddPaymentReconciliationEventOnce

// appleRecordUnownedTransaction records, once per transaction, a verified paid
// App Store transaction (SUBSCRIBED / DID_RENEW) that no live network can be
// credited for: no appAccountToken and no offer-code binding
// (destination_unresolved), or a token naming a deleted network
// (destination_deleted). The App Store keeps charging that subscriber, so the
// payment must stay visible to the owner even though the notification is
// refused; nothing here picks an owner. Apple redelivers a refused
// notification, and each delivery finds the one row. An offer-code purchase
// the app binds later is credited through the ledger, which resolves the row
// for the monitor. A family-shared transaction is not the customer's own
// purchase and is not recorded. Never fails the caller.
func appleRecordUnownedTransaction(
	ctx context.Context,
	notification AppleNotificationDecodedPayload,
	reason string,
	networkId *server.Id,
) {
	claims := notification.TransactionInfo
	if ownership, _ := appleControllerStringClaim(claims, "inAppOwnershipType"); ownership == "FAMILY_SHARED" {
		return
	}
	transactionId, _ := appleControllerStringClaim(claims, "transactionId")
	if transactionId == "" || len(transactionId) > 128 {
		return
	}
	details := map[string]any{
		"reason":            reason,
		"leg":               "webhook",
		"notification_type": notification.NotificationType,
		"subtype":           notification.Subtype,
	}
	for key, claim := range map[string]string{
		"original_transaction": "originalTransactionId",
		"product":              "productId",
		"storefront":           "storefront",
		"currency":             "currency",
	} {
		if value, ok := appleControllerStringClaim(claims, claim); ok && value != "" && len(value) <= 128 {
			details[key] = value
		}
	}
	for key, claim := range map[string]string{
		"purchase_date": "purchaseDate",
		"expires_date":  "expiresDate",
	} {
		if millis, ok := appleControllerInt64Claim(claims, claim); ok {
			details[key] = time.UnixMilli(millis).UTC().Format(time.RFC3339)
		}
	}
	if price, ok := appleControllerInt64Claim(claims, "price"); ok {
		details["price_milliunits"] = price
	}
	added, err := appleRecordUnownedTransactionFunc(ctx, &model.PaymentReconciliationEvent{
		RunId:     server.NewId(),
		Store:     model.SubscriptionMarketApple,
		NetworkId: networkId,
		Action:    model.PaymentReconcileActionCreditUnfulfillable,
		Evidence:  transactionId,
		Details:   details,
	})
	if err != nil {
		glog.Errorf("[apple]transaction %s: could not record the unowned payment: %s\n", transactionId, err)
		return
	}
	if added {
		glog.Errorf(
			"[apple]transaction %s (original %v) is paid but no live network can be credited (%s); recorded for support as credit_unfulfillable\n",
			transactionId,
			details["original_transaction"],
			reason,
		)
	}
}

// appleRecordOnboardingOutcome writes the onboarding trial and refund outcomes
// a notification implies (mmm/onboarding/PLAN.md "MEASUREMENT", S3): a
// DID_RENEW credited after a recorded trial purchase is trial.converted, an
// EXPIRED within the trial window is trial.cancelled, and REFUND / REVOKE is a
// refund for every network whose entitlement ended. Never fails the caller.
func appleRecordOnboardingOutcome(ctx context.Context, notificationType string, transaction *validatedAppleTransaction, processed bool, revokedNetworkIds []server.Id) {
	defer func() {
		if r := recover(); r != nil {
			glog.Warningf("[onboarding]apple outcome %s: %v\n", notificationType, r)
		}
	}()
	now := server.NowUtc()
	plan := planForProductId(transaction.productId)
	switch notificationType {
	case "DID_RENEW":
		if processed {
			storeTrialConverted(ctx, transaction.networkId, model.OnboardingStoreApple, plan, now)
		}
	case "EXPIRED":
		storeTrialCancelled(ctx, transaction.networkId, model.OnboardingStoreApple, plan, now)
	case "REFUND", "REVOKE":
		networkIds := revokedNetworkIds
		if len(networkIds) == 0 {
			networkIds = []server.Id{transaction.networkId}
		}
		for _, networkId := range networkIds {
			RecordRefund(ctx, networkId, model.OnboardingStoreApple, 0)
		}
	}
}

func appleNetworkExistsInTx(tx server.PgTx, ctx context.Context, networkId server.Id) bool {
	err := model.LockPaymentNetworkInTx(tx, ctx, networkId)
	if errors.Is(err, model.ErrPaymentNetworkNotFound) {
		return false
	}
	server.Raise(err)
	return true
}

// appleCreditSubscriptionTransactionInTx is the ONE place a verified App Store
// transaction turns into an entitlement, gated by the
// apple_subscription_transaction ledger (ON CONFLICT DO NOTHING,
// rows-affected checked) in the SAME tx as the credit. Shared by the
// notification webhook (ProcessAppleNotification) and the payment reconciler,
// so a reconcile credit racing a late notification for the same transaction
// produces exactly one credit -- whichever inserts the ledger row second sees
// zero rows affected and adds nothing.
//
// notificationId is the notification UUID for webhook credits; the reconciler
// passes a fresh id (the column is only a provenance pointer, unique per row).
// The ledger row keeps the transaction's price and currency, the only record
// of what a credit in another currency was paid. Returns true only when this
// call committed a new entitlement. The caller must refresh the pro cache
// (model.UpdateProNetwork) after commit when true.
//
// A sandbox purchase (TestFlight, App Review) is credited like any other, so
// the tester keeps Pro, through the same ledger gate. It records no revenue
// on the renewal or the balance, and posts no sales notice. The balance's
// paid column is generated from its revenue, so the sandbox balance is
// unpaid: its contracts carry unpaid priority and payout planning counts its
// traffic as unpaid.
func appleCreditSubscriptionTransactionInTx(
	tx server.PgTx,
	ctx context.Context,
	notificationId server.Id,
	transaction *validatedAppleTransaction,
) bool {
	if appleTransactionCreditedBeforeLedgerInTx(tx, ctx, transaction) {
		// the pre-ledger handler already credited this paid window; a second
		// credit would add another Pro balance for it
		return false
	}
	transactionTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO apple_subscription_transaction (
				transaction_id,
				notification_uuid,
				network_id,
				product_id,
				price_milliunits,
				currency
			)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
			ON CONFLICT DO NOTHING
		`,
		transaction.transactionId,
		notificationId,
		transaction.networkId,
		transaction.productId,
		transaction.priceMilliunits,
		transaction.currency,
	))
	if transactionTag.RowsAffected() == 0 {
		return false
	}

	endTime := transaction.expiresTime.Add(SubscriptionGracePeriod)
	renewal := &model.SubscriptionRenewal{
		NetworkId:          transaction.networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          transaction.purchaseTime,
		EndTime:            endTime,
		NetRevenue:         transaction.creditNetRevenue(),
		SubscriptionMarket: model.SubscriptionMarketApple,
		TransactionId:      transaction.transactionId,
	}
	server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, renewal))
	// the regional price tier (from the storefront) and the welcome offer (an
	// offer-code purchase)
	appleRecordOnboardingInTx(tx, ctx, transaction)

	model.AddTransferBalanceInTx(ctx, tx, &model.TransferBalance{
		NetworkId:             transaction.networkId,
		StartTime:             transaction.purchaseTime,
		EndTime:               endTime,
		StartBalanceByteCount: RefreshSupporterTransferBalance,
		SubsidyNetRevenue:     transaction.creditNetRevenue(),
		BalanceByteCount:      RefreshSupporterTransferBalance,
		Pro:                   true,
	})
	if transaction.sandbox() {
		// no payment was made, so there is no sale to post
		return true
	}
	// the ledger row is new: this transaction is a new payment, posted once
	// the caller's transaction commits
	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		return appleSalesNotice(transaction)
	})
	return true
}

// appleTransactionCreditedBeforeLedgerInTx reports whether the notification
// handler from before the apple_subscription_transaction ledger already
// credited this transaction's paid window (model.HasPreLedgerAppleRenewalInTx).
// Such a credit has no ledger row, so the ledger gate alone would credit the
// window again.
func appleTransactionCreditedBeforeLedgerInTx(
	tx server.PgTx,
	ctx context.Context,
	transaction *validatedAppleTransaction,
) bool {
	return model.HasPreLedgerAppleRenewalInTx(
		tx,
		ctx,
		transaction.networkId,
		transaction.expiresTime.Add(SubscriptionGracePeriod),
	)
}

func validateAppleTransaction(
	notification AppleNotificationDecodedPayload,
	allowedProductIds []string,
	requireEntitlementFields bool,
) (*validatedAppleTransaction, error) {
	return validateAppleTransactionAccount(notification, allowedProductIds, requireEntitlementFields, false)
}

// validateAppleTransactionAccount is validateAppleTransaction that, with
// allowUnboundAccountToken, accepts claims carrying no appAccountToken at all
// (an offer-code redemption) as long as they name an originalTransactionId.
// The result is then marked unboundAccountToken with a zero networkId; the
// caller must resolve the network through the offer-code binding
// (resolveAppleTransactionNetwork) or the binding rules
// (appleBindOfferCodeTransaction) before crediting anything. A token that is
// present but malformed is still invalid.
func validateAppleTransactionAccount(
	notification AppleNotificationDecodedPayload,
	allowedProductIds []string,
	requireEntitlementFields bool,
	allowUnboundAccountToken bool,
) (*validatedAppleTransaction, error) {
	transactionClaims := notification.TransactionInfo
	renewalClaims := notification.RenewalInfo

	accountToken, _ := appleControllerStringClaim(transactionClaims, "appAccountToken")
	if accountToken == "" {
		accountToken, _ = appleControllerStringClaim(renewalClaims, "appAccountToken")
	}
	originalTransactionId, _ := appleControllerStringClaim(transactionClaims, "originalTransactionId")
	if originalTransactionId == "" {
		originalTransactionId, _ = appleControllerStringClaim(renewalClaims, "originalTransactionId")
	}
	if len(originalTransactionId) > 128 {
		return nil, errors.New("invalid App Store original transaction ID")
	}
	var networkId server.Id
	unboundAccountToken := false
	if accountToken == "" && allowUnboundAccountToken && originalTransactionId != "" {
		// no token at all: only a binding can name the network
		unboundAccountToken = true
	} else {
		var err error
		networkId, err = server.ParseId(accountToken)
		if err != nil || networkId == (server.Id{}) {
			return nil, errAppleAccountTokenInvalid
		}
	}
	if renewalAccountToken, exists := appleControllerStringClaim(renewalClaims, "appAccountToken"); exists && renewalAccountToken != accountToken {
		return nil, errors.New("transaction and renewal account tokens do not match")
	}

	transactionId, _ := appleControllerStringClaim(transactionClaims, "transactionId")
	if transactionId == "" {
		transactionId, _ = appleControllerStringClaim(transactionClaims, "appTransactionId")
	}
	if transactionId == "" || len(transactionId) > 128 {
		return nil, errors.New("invalid App Store transaction ID")
	}

	productId, _ := appleControllerStringClaim(transactionClaims, "productId")
	if productId == "" {
		productId, _ = appleControllerStringClaim(renewalClaims, "autoRenewProductId")
	}
	if !appleProductAllowed(allowedProductIds, productId) {
		return nil, fmt.Errorf("App Store product %q is not allowed", productId)
	}

	validated := &validatedAppleTransaction{
		networkId:             networkId,
		transactionId:         transactionId,
		productId:             productId,
		originalTransactionId: originalTransactionId,
		unboundAccountToken:   unboundAccountToken,
	}
	validated.storefront, _ = appleControllerStringClaim(transactionClaims, "storefront")
	validated.offerType, _ = appleControllerInt64Claim(transactionClaims, "offerType")
	validated.offerIdentifier, _ = appleControllerStringClaim(transactionClaims, "offerIdentifier")
	validated.offerDiscountType, _ = appleControllerStringClaim(transactionClaims, "offerDiscountType")
	validated.transactionReason, _ = appleControllerStringClaim(transactionClaims, "transactionReason")
	validated.environment, _ = appleControllerStringClaim(transactionClaims, "environment")
	if !requireEntitlementFields {
		return validated, nil
	}

	purchaseMillis, purchaseExists := appleControllerInt64Claim(transactionClaims, "purchaseDate")
	expiresMillis, expiresExists := appleControllerInt64Claim(transactionClaims, "expiresDate")
	priceMilliunits, priceExists := appleControllerInt64Claim(transactionClaims, "price")
	// Apple sends the code in upper case; anything that is not three letters
	// names no currency
	currency, _ := appleControllerStringClaim(transactionClaims, "currency")
	currency = strings.ToUpper(currency)
	if len(currency) != 3 || strings.Trim(currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		currency = ""
	}
	if !purchaseExists || !expiresExists || !priceExists || priceMilliunits < 0 ||
		(currency == appleUsdCurrency && appleMaximumUsdPriceMilliunits < priceMilliunits) {
		return nil, errors.New("invalid App Store purchase dates or price")
	}
	validated.purchaseTime = time.UnixMilli(purchaseMillis)
	validated.expiresTime = time.UnixMilli(expiresMillis)
	if !validated.expiresTime.After(validated.purchaseTime) ||
		validated.expiresTime.Sub(validated.purchaseTime) > appleMaximumSubscriptionDuration ||
		validated.purchaseTime.After(time.UnixMilli(notification.SignedDate).Add(appleTransactionPurchaseLead(validated))) {
		return nil, errors.New("implausible App Store subscription period")
	}
	validated.priceMilliunits = priceMilliunits
	validated.currency = currency

	switch {
	case priceMilliunits == 0:
		// Preserve trial and promotional entitlements while retaining a paid
		// balance marker, matching the prior App Store behavior.
		validated.netRevenue = model.UsdToNanoCents(0.01)
	case validated.currency == appleUsdCurrency:
		priceUsd := float64(priceMilliunits) / 1000
		priceNanoCents := model.UsdToNanoCents(priceUsd)
		validated.netRevenue = model.NanoCents(float64(priceNanoCents) * 0.8)
	default:
		// A price in another currency, or with no currency, is not USD, and the
		// server has no reliable rate to convert it. Record the same paid marker
		// as a trial: the balance stays paid with its grant and entitlement
		// unchanged, and revenue accounting counts nothing rather than the
		// foreign figure as USD. The ledger keeps the price and currency.
		validated.netRevenue = model.UsdToNanoCents(0.01)
	}
	if validated.netRevenue <= 0 {
		return nil, errors.New("invalid App Store net revenue")
	}
	return validated, nil
}

func appleControllerStringClaim(claims map[string]any, key string) (string, bool) {
	if claims == nil {
		return "", false
	}
	value, exists := claims[key]
	if !exists {
		return "", false
	}
	stringValue, ok := value.(string)
	return stringValue, ok
}

func appleControllerInt64Claim(claims map[string]any, key string) (int64, bool) {
	if claims == nil {
		return 0, false
	}
	value, exists := claims[key]
	if !exists {
		return 0, false
	}
	switch value := value.(type) {
	case float64:
		if value != float64(int64(value)) {
			return 0, false
		}
		return int64(value), true
	case json.Number:
		intValue, err := value.Int64()
		return intValue, err == nil
	default:
		return 0, false
	}
}
