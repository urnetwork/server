package controller

// Root-cause tests for the reconciler's end scope. A network can hold more
// than one purchase in a store (two Stripe checkouts, two Play tokens, two
// App Store subscriptions, two USDC payments). When the store reports one of
// them over, the reconciler used to end every renewal the network held in
// that market -- and the Pro balances they granted -- so a customer whose
// duplicate subscription was cancelled lost the subscription they kept and
// paid for (production, 2026-10-26). The end must touch only the renewals the
// over purchase paid for.
//
// Store fixtures are anchored to the run time with margins of days; every
// store API is an httptest fake. Balances are counted at an instant taken
// after the run, because an end moves a window's end to the run's own clock.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// the latest end_time of the network's renewal in the market that the
// transaction id or purchase token names
func renewalEndTimeFor(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	market string,
	transactionId string,
	purchaseToken string,
) time.Time {
	var endTime time.Time
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT end_time FROM subscription_renewal
			WHERE network_id = $1 AND market = $2
			  AND ($3::varchar = '' OR transaction_id = $3::varchar)
			  AND ($4::varchar = '' OR purchase_token = $4::varchar)
			ORDER BY end_time DESC
			LIMIT 1
			`,
			networkId,
			market,
			transactionId,
			purchaseToken,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&endTime))
				found = true
			}
		})
	})
	if !found {
		t.Fatalf("no %s renewal for %q%q", market, transactionId, purchaseToken)
	}
	return endTime
}

// the end_time of every renewal the network holds for the purchase token,
// earliest first
func renewalEndTimesForToken(
	ctx context.Context,
	networkId server.Id,
	market string,
	purchaseToken string,
) []time.Time {
	endTimes := []time.Time{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT end_time FROM subscription_renewal
			WHERE network_id = $1 AND market = $2 AND purchase_token = $3
			ORDER BY end_time
			`,
			networkId,
			market,
			purchaseToken,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var endTime time.Time
				server.Raise(result.Scan(&endTime))
				endTimes = append(endTimes, endTime)
			}
		})
	})
	return endTimes
}

// Production 2026-10-26: duplicate yearly Stripe subscriptions on one
// network, the later one cancelled. The run that saw the cancelled
// subscription ended the kept one's renewal and Pro balance too.
func TestPaymentReconcileStripeEndsOnlyTheOverSubscription(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcilestripescope", server.NewId())
		now := server.NowUtc()

		// the kept yearly subscription
		keptInvoiceId := "in_reconcile_scope_kept"
		keptStart := now.Add(-30 * 24 * time.Hour).Truncate(time.Second)
		keptPeriodEnd := keptStart.Add(365 * 24 * time.Hour)
		credited, err := stripeCreditInvoicePaid(
			ctx, networkId, keptInvoiceId,
			model.UsdToNanoCents(28.0), keptStart, keptPeriodEnd.Add(SubscriptionGracePeriod),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		stripeEnv.fullInvoices[keptInvoiceId] = stripeTestFullInvoice(
			keptInvoiceId, "sub_reconcile_scope_kept", networkId,
			keptStart, keptPeriodEnd,
			"active", false,
		)

		// the duplicate, bought minutes later and cancelled: its window ends
		// last, so it is the network's newest renewal
		cancelledInvoiceId := "in_reconcile_scope_cancelled"
		cancelledStart := keptStart.Add(5 * time.Minute)
		cancelledPeriodEnd := cancelledStart.Add(365 * 24 * time.Hour)
		credited, err = stripeCreditInvoicePaid(
			ctx, networkId, cancelledInvoiceId,
			model.UsdToNanoCents(28.0), cancelledStart, cancelledPeriodEnd.Add(SubscriptionGracePeriod),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		stripeEnv.fullInvoices[cancelledInvoiceId] = stripeTestFullInvoice(
			cancelledInvoiceId, "sub_reconcile_scope_cancelled", networkId,
			cancelledStart, cancelledPeriodEnd,
			"canceled", false,
		)

		keptEnd := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketStripe, keptInvoiceId, "")
		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		after := server.NowUtc()
		connect.AssertEqual(t, result.Errors, 0)

		keptEndAfter := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketStripe, keptInvoiceId, "")
		proBalances := countProBalancesCovering(ctx, networkId, after)
		if !keptEndAfter.Equal(keptEnd) || proBalances != 1 {
			t.Fatalf(
				"cancelling one of two Stripe subscriptions moved the kept renewal's end from %s to %s and left %d Pro balance(s); want it unchanged and 1",
				keptEnd, keptEndAfter, proBalances,
			)
		}
		if cancelledEnd := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketStripe, cancelledInvoiceId, ""); cancelledEnd.After(after) {
			t.Fatalf("the cancelled subscription's renewal is still open until %s", cancelledEnd)
		}
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the next run leaves the kept subscription alone
		result2, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Errors, 0)
		connect.AssertEqual(t, result2.Ended, 0)
		connect.AssertEqual(t, renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketStripe, keptInvoiceId, "").Equal(keptEnd), true)
	})
}

// One invoice can sit on two networks (support moved a subscription to the
// customer's other network), and the network it moved to can hold a
// subscription of its own. When the moved subscription is over, each
// network's renewal for it ends, and the other subscription keeps its window.
func TestPaymentReconcileStripeEndsAMovedInvoiceOnEachNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)

		creditNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, creditNetworkId, "reconcilestripecredit", server.NewId())
		movedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, movedNetworkId, "reconcilestripemoved", server.NewId())
		now := server.NowUtc()

		invoiceId := "in_reconcile_scope_moved"
		start := now.Add(-30 * 24 * time.Hour).Truncate(time.Second)
		periodEnd := start.Add(365 * 24 * time.Hour)
		credited, err := stripeCreditInvoicePaid(
			ctx, creditNetworkId, invoiceId,
			model.UsdToNanoCents(28.0), start, periodEnd.Add(SubscriptionGracePeriod),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		stripeEnv.fullInvoices[invoiceId] = stripeTestFullInvoice(
			invoiceId, "sub_reconcile_scope_moved", creditNetworkId,
			start, periodEnd,
			"canceled", false,
		)

		// the copy support wrote on the customer's other network
		movedStart := start.Add(9 * time.Hour)
		movedEnd := periodEnd.Add(12 * time.Hour)
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          movedNetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          movedStart,
			EndTime:            movedEnd,
			NetRevenue:         model.UsdToNanoCents(3.5),
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      invoiceId,
		}), nil)
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             movedNetworkId,
			StartTime:             movedStart,
			EndTime:               movedEnd,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     model.UsdToNanoCents(3.5),
			BalanceByteCount:      RefreshSupporterTransferBalance,
			Pro:                   true,
		})
		// a different, paid subscription the moved-to network also holds
		otherInvoiceId := "in_reconcile_scope_moved_other"
		otherStart := now.Add(-3 * 24 * time.Hour).Truncate(time.Second)
		otherPeriodEnd := otherStart.Add(30 * 24 * time.Hour)
		credited, err = stripeCreditInvoicePaid(
			ctx, movedNetworkId, otherInvoiceId,
			model.UsdToNanoCents(3.5), otherStart, otherPeriodEnd.Add(SubscriptionGracePeriod),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		stripeEnv.fullInvoices[otherInvoiceId] = stripeTestFullInvoice(
			otherInvoiceId, "sub_reconcile_scope_moved_other", movedNetworkId,
			otherStart, otherPeriodEnd,
			"active", false,
		)
		otherEnd := renewalEndTimeFor(t, ctx, movedNetworkId, model.SubscriptionMarketStripe, otherInvoiceId, "")

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		after := server.NowUtc()
		connect.AssertEqual(t, result.Errors, 0)

		otherEndAfter := renewalEndTimeFor(t, ctx, movedNetworkId, model.SubscriptionMarketStripe, otherInvoiceId, "")
		if !otherEndAfter.Equal(otherEnd) {
			t.Fatalf(
				"the moved subscription being cancelled moved the other subscription's renewal end from %s to %s; want it unchanged",
				otherEnd, otherEndAfter,
			)
		}
		for _, networkId := range []server.Id{creditNetworkId, movedNetworkId} {
			if end := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketStripe, invoiceId, ""); end.After(after) {
				t.Fatalf("network %s still holds the cancelled invoice's renewal until %s", networkId, end)
			}
		}
		connect.AssertEqual(t, countProBalancesCovering(ctx, creditNetworkId, after), 0)
		connect.AssertEqual(t, countProBalancesCovering(ctx, movedNetworkId, after), 1)
		connect.AssertEqual(t, result.Ended, 2)
	})
}

// The same defect in the Play leg: one token expired, the network's other
// token is still paid.
func TestPaymentReconcileGoogleEndsOnlyTheExpiredPurchase(t *testing.T) {
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
		model.Testing_CreateNetwork(ctx, networkId, "reconcileplayscope", server.NewId())
		now := server.NowUtc()
		webhookSession := session.Testing_CreateClientSession(ctx, nil)
		// two purchases made at different times, as two real purchases are
		credit := func(purchaseToken string, start time.Time, expiry time.Time) {
			playEnv.subscriptions[purchaseToken] = playTestSubscription(
				networkId, "supporter_monthly", start, expiry,
			)
			result, err := PlaySubscriptionRenewal(&PlaySubscriptionRenewalArgs{
				NetworkId:      networkId,
				PackageName:    playEnv.packageName,
				SubscriptionId: "supporter_monthly",
				PurchaseToken:  purchaseToken,
			}, webhookSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Renewed, true)
		}
		paidToken := "play-reconcile-scope-paid"
		credit(paidToken, now.Add(-3*24*time.Hour), now.Add(20*24*time.Hour))
		expiredToken := "play-reconcile-scope-expired"
		credit(expiredToken, now.Add(-24*time.Hour), now.Add(29*24*time.Hour))
		// the store's truth: the second purchase expired early
		playEnv.subscriptions[expiredToken].SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"

		paidEnd := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketGoogle, "", paidToken)
		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		after := server.NowUtc()

		paidEndsAfter := renewalEndTimesForToken(ctx, networkId, model.SubscriptionMarketGoogle, paidToken)
		if len(paidEndsAfter) != 1 || !paidEndsAfter[0].Equal(paidEnd) || result.Credited != 0 {
			t.Fatalf(
				"one of two Play purchases expiring left the paid purchase with renewals ending %v and credited it %d time(s) again (%d store errors); want its one renewal still ending %s and no credit",
				paidEndsAfter, result.Credited, result.Errors, paidEnd,
			)
		}
		connect.AssertEqual(t, result.Errors, 0)
		if expiredEnd := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketGoogle, "", expiredToken); expiredEnd.After(after) {
			t.Fatalf("the expired purchase's renewal is still open until %s", expiredEnd)
		}
		connect.AssertEqual(t, countProBalancesCovering(ctx, networkId, after), 1)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}

// The same defect in the App Store leg: two subscriptions on one network (two
// Apple IDs), the newest revoked at Apple. The revoked window was also
// credited by the pre-ledger notification handler, the shape 16 production
// yearly windows have: its second renewal, keyed on the app transaction id,
// and its second Pro balance end with the window too.
func TestPaymentReconcileAppleEndsOnlyTheRevokedSubscription(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		appleEnv := newAppleReconcileTestEnv(t, []string{"supporter_monthly_26"})

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileapplescope", server.NewId())
		now := server.NowUtc()

		paidTransactionId := "1000000000000301"
		appleTestCredit(
			t, ctx, networkId, paidTransactionId, "supporter_monthly_26",
			now.Add(-5*24*time.Hour), now.Add(20*24*time.Hour),
		)

		revokedTransactionId := "1000000000000302"
		revokedPurchase := now.Add(-2 * 24 * time.Hour).Truncate(time.Second)
		// Apple's expiry carries milliseconds; the pre-ledger handler kept
		// whole seconds
		revokedExpires := revokedPurchase.Add(28 * 24 * time.Hour).Add(123 * time.Millisecond)
		revokedClaims := appleTestTransactionClaims(
			t, networkId, revokedTransactionId, "supporter_monthly_26",
			revokedPurchase, revokedExpires,
		)
		appleTestCredit(
			t, ctx, networkId, revokedTransactionId, "supporter_monthly_26",
			revokedPurchase, revokedExpires,
		)
		// the pre-ledger handler's credit of the same window: renewal keyed
		// on the app transaction id, started at processing time, ended at the
		// whole-second expiry plus the grace period, no ledger row
		twinTransactionId := "705000000000000302"
		twinStart := time.Unix(revokedPurchase.Unix()+2, 0).UTC()
		twinEnd := time.Unix(revokedExpires.UnixMilli()/1000, 0).UTC().Add(SubscriptionGracePeriod)
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          twinStart,
			EndTime:            twinEnd,
			NetRevenue:         model.UsdToNanoCents(4.0),
			SubscriptionMarket: model.SubscriptionMarketApple,
			TransactionId:      twinTransactionId,
		}), nil)
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             twinStart,
			EndTime:               twinEnd,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     model.UsdToNanoCents(4.0),
			BalanceByteCount:      RefreshSupporterTransferBalance,
			Pro:                   true,
		})
		model.UpdateProNetwork(ctx, networkId)
		// the store's truth for the newest subscription: revoked
		appleEnv.statuses[revokedTransactionId] = appleTestStatuses(t, 5, revokedTransactionId, revokedClaims)

		paidEnd := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketApple, paidTransactionId, "")
		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		after := server.NowUtc()
		connect.AssertEqual(t, result.Errors, 0)

		paidEndAfter := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketApple, paidTransactionId, "")
		if !paidEndAfter.Equal(paidEnd) {
			t.Fatalf(
				"one of two App Store subscriptions being revoked moved the other's renewal end from %s to %s; want it unchanged",
				paidEnd, paidEndAfter,
			)
		}
		for _, transactionId := range []string{revokedTransactionId, twinTransactionId} {
			if end := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketApple, transactionId, ""); end.After(after) {
				t.Fatalf("the revoked window's renewal %s is still open until %s", transactionId, end)
			}
		}
		if proBalances := countProBalancesCovering(ctx, networkId, after); proBalances != 1 {
			t.Fatalf("%d Pro balances remain after one of two App Store subscriptions was revoked; want the other one's 1", proBalances)
		}
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}

// The same defect in the Solana leg: two USDC payments on one network, one of
// which vanished from the chain.
func TestPaymentReconcileSolanaEndsOnlyTheVanishedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		solanaEnv := newSolanaReconcileTestEnv(t)

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcilesolanascope", userId)
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		webhookSession := session.Testing_CreateClientSession(ctx, nil)
		pay := func(reference string, signature string) {
			connect.AssertEqual(t, model.CreateSolanaPaymentIntent(reference, 5.0, model.SolanaPlanMonthly, userSession), nil)
			result, err := HeliusWebhook(
				[]*SolanaTransaction{solanaTestPayment(reference, signature, 5.0)},
				webhookSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Message, "Processed 1 matching payments")
		}
		pay("reconcile-scope-ref-kept", "sig-reconcile-scope-kept")
		pay("reconcile-scope-ref-vanished", "sig-reconcile-scope-vanished")
		solanaEnv.onChain["sig-reconcile-scope-kept"] = true
		// push both credits past the finality grace so the reconciler
		// re-verifies them; each Pro balance keeps its renewal's window, as a
		// credit made two hours ago has
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE subscription_renewal SET start_time = start_time - interval '2 hours' WHERE network_id = $1 AND market = $2`,
				networkId,
				model.SubscriptionMarketSolana,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_balance SET start_time = start_time - interval '2 hours' WHERE network_id = $1 AND pro`,
				networkId,
			))
		})

		keptEnd := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketSolana, "reconcile-scope-ref-kept", "")
		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		after := server.NowUtc()
		connect.AssertEqual(t, result.Errors, 0)

		keptEndAfter := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketSolana, "reconcile-scope-ref-kept", "")
		if !keptEndAfter.Equal(keptEnd) {
			t.Fatalf("one vanished USDC payment moved the other payment's renewal end from %s to %s; want it unchanged", keptEnd, keptEndAfter)
		}
		if vanishedEnd := renewalEndTimeFor(t, ctx, networkId, model.SubscriptionMarketSolana, "reconcile-scope-ref-vanished", ""); vanishedEnd.After(after) {
			t.Fatalf("the vanished payment's renewal is still open until %s", vanishedEnd)
		}
		connect.AssertEqual(t, countProBalancesCovering(ctx, networkId, after), 1)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}
