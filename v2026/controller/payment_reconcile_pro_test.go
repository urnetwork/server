package controller

// Root-cause tests for the paid-but-Free failure modes found in the support
// inbox (2026-10-26): every store payment the server can attribute to a
// network must end as a Pro grant, through the ordinary idempotent credit
// gates; a payment it cannot attribute is recorded for the owner and never
// credited to a guessed network.
//
// The App Store validator takes its clock from the notification's
// SignedDate, so the pure tests inject a fixed instant. The reconcile tests
// anchor every store fixture to the run time with margins of days against the
// 48-hour and 90-day windows, and every store API is an httptest fake.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// payment events the webhooks and the reconciler wrote for one store object
func paymentEventDetailsForEvidence(
	t testing.TB,
	ctx context.Context,
	store string,
	action string,
	evidence string,
) []map[string]any {
	detailsList := []map[string]any{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT COALESCE(details, '{}')
			FROM payment_reconciliation_event
			WHERE store = $1 AND action = $2 AND evidence = $3 AND NOT dry_run
			ORDER BY event_time
			`,
			store,
			action,
			evidence,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var detailsJson string
				server.Raise(result.Scan(&detailsJson))
				details := map[string]any{}
				connect.AssertEqual(t, json.Unmarshal([]byte(detailsJson), &details), nil)
				detailsList = append(detailsList, details)
			}
		})
	})
	return detailsList
}

func countMarketRenewals(ctx context.Context, networkId server.Id, market string) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1 AND market = $2`,
			networkId,
			market,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// pro balances whose window covers at
func countProBalancesCovering(ctx context.Context, networkId server.Id, at time.Time) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT COUNT(*) FROM transfer_balance
			WHERE network_id = $1 AND pro AND start_time <= $2 AND $2 < end_time
			`,
			networkId,
			at,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

func appleTestStatuses(t testing.TB, status int, originalTransactionId string, claims map[string]any) *appleSubscriptionStatusesResponse {
	return &appleSubscriptionStatusesResponse{
		Data: []*appleSubscriptionGroup{
			{
				LastTransactions: []*appleLastTransaction{
					{
						Status:                status,
						OriginalTransactionId: originalTransactionId,
						SignedTransactionInfo: appleTestJws(t, claims),
					},
				},
			},
		},
	}
}

// Failure mode: the App Store plans sold from 2025, "supporter" (monthly) and
// "supporter_yearly", are missing from the configured product list since the
// 2026 price tier, and every credit path validates against that list. Each of
// their renewals was refused while the App Store kept charging.
func TestAppleLegacySupporterProductsAreAllowed(t *testing.T) {
	signedAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	configured := []string{"supporter_monthly_26", "supporter_yearly_26"}
	networkId := server.NewId()
	claims := func(productId string) AppleNotificationDecodedPayload {
		return AppleNotificationDecodedPayload{
			NotificationType: "DID_RENEW",
			SignedDate:       signedAt.UnixMilli(),
			TransactionInfo: map[string]any{
				"appAccountToken": networkId.String(),
				"transactionId":   "legacy-" + productId,
				"productId":       productId,
				"purchaseDate":    float64(signedAt.Add(-time.Hour).UnixMilli()),
				"expiresDate":     float64(signedAt.Add(365 * 24 * time.Hour).UnixMilli()),
				"price":           float64(39990),
				"currency":        "USD",
			},
		}
	}

	for _, productId := range []string{"supporter", "supporter_yearly", "supporter_monthly_26", "supporter_yearly_26"} {
		transaction, err := validateAppleTransaction(claims(productId), configured, true)
		if err != nil {
			t.Fatalf("App Store product %q refused: %v", productId, err)
		}
		connect.AssertEqual(t, transaction.productId, productId)
		connect.AssertEqual(t, transaction.networkId, networkId)
	}
	// a product the apps never sold as Pro is still refused
	for _, productId := range []string{"", "supporter_plan", "attacker-controlled-product"} {
		if _, err := validateAppleTransaction(claims(productId), configured, true); err == nil {
			t.Fatalf("App Store product %q was accepted", productId)
		}
	}
}

// The same failure mode through the notification webhook and the verify
// endpoint: a legacy renewal credits Pro.
func TestAppleLegacySupporterRenewalCreditsPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		configured := []string{"supporter_monthly_26", "supporter_yearly_26"}
		now := server.NowUtc().Truncate(time.Millisecond)

		// the webhook: DID_RENEW of a legacy yearly subscription
		webhookNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, webhookNetworkId, "legacyyearlyrenew", server.NewId())
		renewTransactionId := "legacy-yearly-" + server.NewId().String()
		processed, err := ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
			NotificationType: "DID_RENEW",
			NotificationUUID: server.NewId().String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo: map[string]any{
				"appAccountToken": webhookNetworkId.String(),
				"transactionId":   renewTransactionId,
				"productId":       "supporter_yearly",
				"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
				"expiresDate":     float64(now.Add(365 * 24 * time.Hour).UnixMilli()),
				"price":           float64(39990),
				"currency":        "USD",
			},
		}, configured)
		if err != nil || !processed {
			t.Fatalf("DID_RENEW of the legacy supporter_yearly plan: processed = %t, err = %v; want it credited", processed, err)
		}
		connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, renewTransactionId), true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, webhookNetworkId), true)

		// the verify endpoint: the app reports a legacy monthly transaction
		verifyNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, verifyNetworkId, "legacymonthlyverify", server.NewId())
		clientId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: verifyNetworkId,
			ClientId:  &clientId,
			UserId:    server.NewId(),
		})
		verifyTransactionId := "legacy-monthly-" + server.NewId().String()
		result, err := VerifyAppleTransactionClaims(map[string]any{
			"appAccountToken": verifyNetworkId.String(),
			"transactionId":   verifyTransactionId,
			"productId":       "supporter",
			"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
			"expiresDate":     float64(now.Add(30 * 24 * time.Hour).UnixMilli()),
			"price":           float64(3990),
			"currency":        "USD",
		}, configured, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Status != VerifyPurchaseStatusCredited {
			t.Fatalf("verify of a legacy supporter transaction: status = %q; want %q", result.Status, VerifyPurchaseStatusCredited)
		}
		connect.AssertEqual(t, model.IsProNetwork(ctx, verifyNetworkId), true)
	})
}

// Failure mode: the notification handler before the
// apple_subscription_transaction ledger (2026-08) credited transactions
// without a ledger row, keyed on the app transaction id. The reconciler's
// first App Store runs (2026-10-09 and 10-13) credited 13 of those paid
// windows a second time. Accepting the legacy products would do the same to
// every legacy yearly subscriber.
func TestPaymentReconcileAppleNeverRecreditsPreLedgerWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		appleEnv := newAppleReconcileTestEnv(t, []string{"supporter_yearly_26"})

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcilepreledger", server.NewId())

		now := server.NowUtc()
		purchaseTime := now.Add(-30 * 24 * time.Hour).Truncate(time.Second)
		// Apple's expiry carries milliseconds; the pre-ledger handler kept
		// whole seconds
		expiresTime := purchaseTime.Add(365 * 24 * time.Hour).Add(123 * time.Millisecond)
		appTransactionId := "705000000000000001"
		transactionId := "300000000000000001"

		// the pre-ledger credit, built the way that handler built it: renewal
		// keyed on the app transaction id, started at processing time
		// (time.Now), ended at time.Unix(expiresDate/1000) plus the grace
		// period, no ledger row
		preLedgerStart := time.Unix(purchaseTime.Unix()+2, 0)
		preLedgerEnd := time.Unix(expiresTime.UnixMilli()/1000, 0).Add(SubscriptionGracePeriod)
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          preLedgerStart,
			EndTime:            preLedgerEnd,
			NetRevenue:         model.UsdToNanoCents(32.0),
			SubscriptionMarket: model.SubscriptionMarketApple,
			TransactionId:      appTransactionId,
		}), nil)
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             preLedgerStart,
			EndTime:               preLedgerEnd,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     model.UsdToNanoCents(32.0),
			BalanceByteCount:      RefreshSupporterTransferBalance,
			Pro:                   true,
		})
		model.UpdateProNetwork(ctx, networkId)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the store's truth: the same, already credited, paid year
		claims := appleTestTransactionClaims(t, networkId, transactionId, "supporter_yearly_26", purchaseTime, expiresTime)
		appleEnv.statuses[appTransactionId] = appleTestStatuses(t, 1, appTransactionId, claims)

		dryRun, err := RunPaymentReconciliationWithOptions(reconcileTestSession(t, ctx), &PaymentReconcileRunOptions{DryRun: true})
		connect.AssertEqual(t, err, nil)
		if dryRun.Credited != 0 {
			t.Errorf("dry run would credit %d already-paid App Store window(s), want 0", dryRun.Credited)
		}

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		renewals := countMarketRenewals(ctx, networkId, model.SubscriptionMarketApple)
		proBalances := countProBalancesCovering(ctx, networkId, now)
		if result.Credited != 0 || renewals != 1 || proBalances != 1 {
			t.Fatalf(
				"reconcile credited %d already-paid window(s), leaving %d App Store renewals and %d Pro balances for one paid year; want 0, 1 and 1",
				result.Credited, renewals, proBalances,
			)
		}
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// only that paid window is recognized: a transaction for another
		// window on the same network still credits
		otherTransactionId := "300000000000000002"
		otherClaims := appleTestTransactionClaims(t, networkId, otherTransactionId, "supporter_yearly_26", now.Add(-time.Hour), now.Add(400*24*time.Hour))
		processed, err := ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
			NotificationType: "SUBSCRIBED",
			NotificationUUID: server.NewId().String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo:  otherClaims,
		}, []string{"supporter_yearly_26"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)
	})
}

// Failure mode: the reconciler forgot a subscription 48 hours after its last
// credited window ended. A renewal whose notification was refused or lost (the
// App Store leg had no credentials until 2026-10-09) was never credited.
func TestPaymentReconcileAppleCreditsLapsedRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		appleEnv := newAppleReconcileTestEnv(t, []string{"supporter_monthly_26"})

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileapplelapsed", server.NewId())

		now := server.NowUtc()
		// the last credited month ended 20 days ago: past the 48-hour window,
		// inside the lapsed window
		oldTransactionId := "1000000000000101"
		lapsedEnd := now.Add(-20 * 24 * time.Hour)
		appleTestCredit(
			t, ctx, networkId, oldTransactionId, "supporter_monthly_26",
			lapsedEnd.Add(-31*24*time.Hour), lapsedEnd.Add(-SubscriptionGracePeriod),
		)
		model.UpdateProNetwork(ctx, networkId)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		// the store's truth: billing retry recovered and renewed it; the
		// notification never credited
		newTransactionId := "1000000000000102"
		renewalClaims := appleTestTransactionClaims(
			t, networkId, newTransactionId, "supporter_monthly_26",
			now.Add(-2*24*time.Hour), now.Add(28*24*time.Hour),
		)
		appleEnv.statuses[oldTransactionId] = appleTestStatuses(t, 1, oldTransactionId, renewalClaims)
		appleEnv.statuses[newTransactionId] = appleEnv.statuses[oldTransactionId]

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		if result.Credited != 1 || !model.IsProNetwork(ctx, networkId) {
			t.Fatalf(
				"App Store renewal paid after a 20-day lapse: reconcile credited %d, network Pro = %t; want 1 and true",
				result.Credited, model.IsProNetwork(ctx, networkId),
			)
		}
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, newTransactionId), true)

		// idempotent: the next run credits nothing
		result2, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Credited, 0)
		connect.AssertEqual(t, countProBalancesCovering(ctx, networkId, now), 1)
	})
}

// The same failure mode for Play: the RTDN and the scheduled poll (which stops
// a day after a missed expiry) both missed an account-hold recovery.
func TestPaymentReconcileGoogleCreditsLapsedRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		playEnv := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})
		previousHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = previousHasCredentials })

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileplaylapsed", server.NewId())

		now := server.NowUtc()
		purchaseToken := "play-reconcile-lapsed-1"
		startTime := now.Add(-50 * 24 * time.Hour)
		// the last credited window ended 20 days ago
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            now.Add(-20 * 24 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(3.5),
			PurchaseToken:      purchaseToken,
			SubscriptionMarket: model.SubscriptionMarketGoogle,
		}), nil)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		// the store's truth: renewed, paid through 10 days from now
		playEnv.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			startTime,
			now.Add(10*24*time.Hour),
		)

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		if result.Credited != 1 || !model.IsProNetwork(ctx, networkId) {
			t.Fatalf(
				"Play renewal paid after a 20-day lapse: reconcile credited %d, network Pro = %t; want 1 and true",
				result.Credited, model.IsProNetwork(ctx, networkId),
			)
		}
		connect.AssertEqual(t, result.Ended, 0)

		result2, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Credited, 0)
		connect.AssertEqual(t, countProBalancesCovering(ctx, networkId, now), 1)
	})
}

// Failure mode: cancelled ≠ expired was applied only to ending. A Play
// renewal charged before the customer turned auto-renew off is paid through
// its expiry, but a CANCELED state never credited it when its renewal
// notification was missed -- not in the reconciler, the scheduled poll or
// the verify endpoint.
func TestPaymentReconcileGoogleCreditsPaidThroughCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		playEnv := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})
		previousHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = previousHasCredentials })

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileplaycancel", server.NewId())

		now := server.NowUtc()
		purchaseToken := "play-reconcile-paid-through-1"
		startTime := now.Add(-31 * 24 * time.Hour)
		// the last credited window ended an hour ago, inside the 48-hour window
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            now.Add(-1 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(3.5),
			PurchaseToken:      purchaseToken,
			SubscriptionMarket: model.SubscriptionMarketGoogle,
		}), nil)

		// the store's truth: renewed for another month, then auto-renew off
		playEnv.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			startTime,
			now.Add(29*24*time.Hour),
		)
		playEnv.subscriptions[purchaseToken].SubscriptionState = "SUBSCRIPTION_STATE_CANCELED"

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		if result.Credited != 1 || !model.IsProNetwork(ctx, networkId) {
			t.Fatalf(
				"Play renewal paid before auto-renew was turned off (CANCELED, paid through 29 more days): reconcile credited %d, network Pro = %t; want 1 and true",
				result.Credited, model.IsProNetwork(ctx, networkId),
			)
		}
		connect.AssertEqual(t, result.Ended, 0)

		// the scheduled poll sees the same paid-through cancel: nothing twice,
		// and one poll stays at the paid-through expiry
		pollResult, err := PlaySubscriptionRenewal(&PlaySubscriptionRenewalArgs{
			NetworkId:      networkId,
			PackageName:    playEnv.packageName,
			SubscriptionId: "supporter_monthly",
			PurchaseToken:  purchaseToken,
		}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, pollResult.Canceled, true)
		connect.AssertEqual(t, pollResult.Renewed, false)
		connect.AssertEqual(t, pollResult.Terminal, false)
		connect.AssertEqual(t, countProBalancesCovering(ctx, networkId, now), 1)
	})
}

// Failure mode: a verified paid App Store transaction that names no live
// network (no appAccountToken and no offer-code binding, or a deleted
// network) was refused with only a log line. The App Store keeps charging the
// subscriber and nothing durable showed the payment.
func TestProcessAppleNotificationRecordsUnownedPaidTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		configured := []string{"supporter_monthly_26"}
		now := server.NowUtc().Truncate(time.Millisecond)
		notification := func(transactionId string, claims map[string]any) AppleNotificationDecodedPayload {
			transactionInfo := map[string]any{
				"transactionId":         transactionId,
				"originalTransactionId": "orig-" + transactionId,
				"productId":             "supporter_monthly_26",
				"purchaseDate":          float64(now.Add(-time.Hour).UnixMilli()),
				"expiresDate":           float64(now.Add(30 * 24 * time.Hour).UnixMilli()),
				"price":                 float64(4990),
				"currency":              "USD",
			}
			for key, value := range claims {
				transactionInfo[key] = value
			}
			return AppleNotificationDecodedPayload{
				NotificationType: "DID_RENEW",
				NotificationUUID: server.NewId().String(),
				SignedDate:       now.UnixMilli(),
				TransactionInfo:  transactionInfo,
			}
		}

		// no appAccountToken and no binding: refused as before, and recorded
		// once however often Apple redelivers
		unresolvedId := "unowned-" + server.NewId().String()
		for range 2 {
			processed, err := ProcessAppleNotification(ctx, notification(unresolvedId, nil), configured)
			connect.AssertEqual(t, processed, false)
			connect.AssertEqual(t, err != nil, true)
		}
		unresolved := paymentEventDetailsForEvidence(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionCreditUnfulfillable, unresolvedId)
		if len(unresolved) != 1 {
			t.Fatalf("paid App Store transaction with no account token and no binding: %d credit_unfulfillable record(s) after two deliveries; want exactly 1", len(unresolved))
		}
		connect.AssertEqual(t, unresolved[0]["reason"], "destination_unresolved")
		connect.AssertEqual(t, unresolved[0]["original_transaction"], "orig-"+unresolvedId)
		connect.AssertEqual(t, unresolved[0]["product"], "supporter_monthly_26")

		// a token naming a deleted network
		deletedId := "deleted-" + server.NewId().String()
		_, err := ProcessAppleNotification(ctx, notification(deletedId, map[string]any{
			"appAccountToken": server.NewId().String(),
		}), configured)
		connect.AssertEqual(t, err != nil, true)
		deleted := paymentEventDetailsForEvidence(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionCreditUnfulfillable, deletedId)
		connect.AssertEqual(t, len(deleted), 1)
		connect.AssertEqual(t, deleted[0]["reason"], "destination_deleted")

		// a family-shared transaction is not the customer's own purchase
		familyId := "family-" + server.NewId().String()
		_, err = ProcessAppleNotification(ctx, notification(familyId, map[string]any{
			"inAppOwnershipType": "FAMILY_SHARED",
		}), configured)
		connect.AssertEqual(t, err != nil, true)
		connect.AssertEqual(t, len(paymentEventDetailsForEvidence(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionCreditUnfulfillable, familyId)), 0)
	})
}

// Failure mode: a paid Play purchase with no account identifiers and no
// binding was answered 200 with nothing recorded, and a verify the binding
// rules refuse lets the app acknowledge it, so Google keeps the money with no
// trace (support item 8012 has no record at all).
func TestPlayWebhookRecordsUnownedPaidPurchase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		now := server.NowUtc()
		purchaseToken := "play-unowned-" + server.NewId().String()
		sub := playTestSubscription(server.Id{}, "supporter_monthly", now.Add(-time.Hour), now.Add(30*24*time.Hour))
		sub.ExternalAccountIdentifiers = nil
		env.subscriptions[purchaseToken] = sub
		env.extraFields = map[string]map[string]any{
			purchaseToken: {"latestOrderId": "GPA.0000-0000-0000-00001"},
		}

		for range 2 {
			result, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, result, nil)
		}
		connect.AssertEqual(t, env.acknowledgeCount(purchaseToken), int64(0))
		recorded := paymentEventDetailsForEvidence(t, ctx, model.SubscriptionMarketGoogle, model.PaymentReconcileActionCreditUnfulfillable, purchaseToken)
		if len(recorded) != 1 {
			t.Fatalf("paid Play purchase with no account identifiers and no binding: %d credit_unfulfillable record(s) after two deliveries; want exactly 1", len(recorded))
		}
		connect.AssertEqual(t, recorded[0]["reason"], "destination_unresolved")
		connect.AssertEqual(t, recorded[0]["order"], "GPA.0000-0000-0000-00001")
		connect.AssertEqual(t, recorded[0]["product"], "supporter_monthly")

		// a pending purchase is not paid yet and is not recorded
		pendingToken := "play-unowned-pending-" + server.NewId().String()
		pending := playTestSubscription(server.Id{}, "supporter_monthly", now.Add(-time.Hour), now.Add(30*24*time.Hour))
		pending.ExternalAccountIdentifiers = nil
		pending.SubscriptionState = "SUBSCRIPTION_STATE_PENDING"
		env.subscriptions[pendingToken] = pending
		_, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", pendingToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(paymentEventDetailsForEvidence(t, ctx, model.SubscriptionMarketGoogle, model.PaymentReconcileActionCreditUnfulfillable, pendingToken)), 0)
	})
}

// Failure mode: an active Play purchase bound to another network than the
// renewal row it renewed for was an hourly reconcile error -- never credited
// (correct) but never recorded as a payment the owner must place.
func TestPaymentReconcileGoogleBoundElsewhereIsRecordedOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		playEnv := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})
		previousHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = previousHasCredentials })

		rowNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, rowNetworkId, "reconcileplayrow", server.NewId())
		boundNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, boundNetworkId, "reconcileplaybound", server.NewId())

		now := server.NowUtc()
		purchaseToken := "play-reconcile-bound-elsewhere-1"
		startTime := now.Add(-10 * 24 * time.Hour)
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          rowNetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            now.Add(-1 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(3.5),
			PurchaseToken:      purchaseToken,
			SubscriptionMarket: model.SubscriptionMarketGoogle,
		}), nil)
		sub := playTestSubscription(rowNetworkId, "supporter_monthly", startTime, now.Add(20*24*time.Hour))
		sub.ExternalAccountIdentifiers = nil
		playEnv.subscriptions[purchaseToken] = sub

		previousLookup := playPurchaseBindingLookupFunc
		playPurchaseBindingLookupFunc = func(connOwner server.PgConn, ctx context.Context, token string, linkedToken string, inherit bool) (server.Id, bool) {
			if token == purchaseToken {
				return boundNetworkId, true
			}
			return previousLookup(connOwner, ctx, token, linkedToken, inherit)
		}
		t.Cleanup(func() { playPurchaseBindingLookupFunc = previousLookup })

		for run := range 2 {
			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			if result.Errors != 0 || result.Credited != 0 {
				t.Fatalf(
					"run %d on a Play purchase bound to another network: %d error(s), %d credit(s); want 0 and 0 (recorded once for the owner instead)",
					run+1, result.Errors, result.Credited,
				)
			}
		}
		recorded := paymentEventDetailsForEvidence(t, ctx, model.SubscriptionMarketGoogle, model.PaymentReconcileActionCreditUnfulfillable, purchaseToken)
		if len(recorded) != 1 {
			t.Fatalf("Play purchase bound to another network: %d credit_unfulfillable record(s) after two runs; want exactly 1", len(recorded))
		}
		connect.AssertEqual(t, recorded[0]["reason"], "bound_to_another_network")
		connect.AssertEqual(t, model.IsProNetwork(ctx, rowNetworkId), false)
		connect.AssertEqual(t, model.IsProNetwork(ctx, boundNetworkId), false)
	})
}
