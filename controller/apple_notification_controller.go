package controller

// Transactional App Store entitlement processing. Inputs are the already
// verified nested JWS claims produced by api/handlers; notification and
// transaction ledgers make retries idempotent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const appleMaximumSubscriptionDuration = 2 * 366 * 24 * time.Hour
const appleMaximumPriceMilliunits int64 = 10_000_000

type validatedAppleTransaction struct {
	networkId     server.Id
	transactionId string
	productId     string
	purchaseTime  time.Time
	expiresTime   time.Time
	netRevenue    model.NanoCents
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
		transaction, err = validateAppleTransaction(notification, allowedProductIds, actionable)
		if err != nil {
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
				returnErr = errors.New("App Store account token does not name an existing network")
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
	})

	if processed {
		model.UpdateProNetwork(ctx, transaction.networkId)
	}
	for _, networkId := range revokedNetworkIds {
		// the entitlement ended under the network -- make the downgrade visible
		// immediately rather than after ProCacheTtl
		model.UpdateProNetwork(ctx, networkId)
	}
	return processed, returnErr
}

func appleNetworkExistsInTx(tx server.PgTx, ctx context.Context, networkId server.Id) bool {
	var networkExists bool
	result, err := tx.Query(ctx, `SELECT EXISTS (SELECT 1 FROM network WHERE network_id = $1)`, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&networkExists))
		}
	})
	return networkExists
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
// Returns true only when this call committed a new entitlement. The caller
// must refresh the pro cache (model.UpdateProNetwork) after commit when true.
func appleCreditSubscriptionTransactionInTx(
	tx server.PgTx,
	ctx context.Context,
	notificationId server.Id,
	transaction *validatedAppleTransaction,
) bool {
	transactionTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO apple_subscription_transaction (
				transaction_id,
				notification_uuid,
				network_id,
				product_id
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING
		`,
		transaction.transactionId,
		notificationId,
		transaction.networkId,
		transaction.productId,
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
		NetRevenue:         transaction.netRevenue,
		SubscriptionMarket: model.SubscriptionMarketApple,
		TransactionId:      transaction.transactionId,
	}
	server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, renewal))

	model.AddTransferBalanceInTx(ctx, tx, &model.TransferBalance{
		NetworkId:             transaction.networkId,
		StartTime:             transaction.purchaseTime,
		EndTime:               endTime,
		StartBalanceByteCount: RefreshSupporterTransferBalance,
		SubsidyNetRevenue:     transaction.netRevenue,
		BalanceByteCount:      RefreshSupporterTransferBalance,
		Pro:                   true,
	})
	return true
}

func validateAppleTransaction(
	notification AppleNotificationDecodedPayload,
	allowedProductIds []string,
	requireEntitlementFields bool,
) (*validatedAppleTransaction, error) {
	transactionClaims := notification.TransactionInfo
	renewalClaims := notification.RenewalInfo

	accountToken, _ := appleControllerStringClaim(transactionClaims, "appAccountToken")
	if accountToken == "" {
		accountToken, _ = appleControllerStringClaim(renewalClaims, "appAccountToken")
	}
	networkId, err := server.ParseId(accountToken)
	if err != nil || networkId == (server.Id{}) {
		return nil, errors.New("invalid App Store account token")
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
	if !slices.Contains(allowedProductIds, productId) {
		return nil, fmt.Errorf("App Store product %q is not allowed", productId)
	}

	validated := &validatedAppleTransaction{
		networkId:     networkId,
		transactionId: transactionId,
		productId:     productId,
	}
	if !requireEntitlementFields {
		return validated, nil
	}

	purchaseMillis, purchaseExists := appleControllerInt64Claim(transactionClaims, "purchaseDate")
	expiresMillis, expiresExists := appleControllerInt64Claim(transactionClaims, "expiresDate")
	priceMilliunits, priceExists := appleControllerInt64Claim(transactionClaims, "price")
	if !purchaseExists || !expiresExists || !priceExists || priceMilliunits < 0 ||
		priceMilliunits > appleMaximumPriceMilliunits {
		return nil, errors.New("invalid App Store purchase dates or price")
	}
	validated.purchaseTime = time.UnixMilli(purchaseMillis)
	validated.expiresTime = time.UnixMilli(expiresMillis)
	if !validated.expiresTime.After(validated.purchaseTime) ||
		validated.expiresTime.Sub(validated.purchaseTime) > appleMaximumSubscriptionDuration ||
		validated.purchaseTime.After(time.UnixMilli(notification.SignedDate).Add(5*time.Minute)) {
		return nil, errors.New("implausible App Store subscription period")
	}

	if priceMilliunits == 0 {
		// Preserve trial and promotional entitlements while retaining a paid
		// balance marker, matching the prior App Store behavior.
		validated.netRevenue = model.UsdToNanoCents(0.01)
	} else {
		priceUsd := float64(priceMilliunits) / 1000
		priceNanoCents := model.UsdToNanoCents(priceUsd)
		validated.netRevenue = model.NanoCents(float64(priceNanoCents) * 0.8)
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
