package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// deletePaymentTestNetwork removes one synthetic network while retaining its
// deliberately unconsumed payment evidence.
func deletePaymentTestNetwork(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id) {
	t.Helper()
	if success, _ := model.RemoveNetwork(ctx, networkId, &userId); !success {
		t.Fatal("remove synthetic payment network")
	}
}

// paymentGuardWriteCounts returns payment rows keyed by one synthetic provider
// transaction after a refused credit.
func paymentGuardWriteCounts(ctx context.Context, transactionId string) (renewals int, balances int) {
	server.Db(ctx, func(conn server.PgConn) {
		returnErr := conn.QueryRow(
			ctx,
			`
				SELECT
					(SELECT count(*) FROM subscription_renewal WHERE transaction_id = $1),
					(SELECT count(*) FROM transfer_balance WHERE purchase_token = $1)
			`,
			transactionId,
		).Scan(&renewals, &balances)
		server.Raise(returnErr)
	})
	return
}

// TestSolanaCreditsRefuseDeletedNetworkBeforeIntent proves both the supporter
// and data-pack paths leave an open intent unconsumed after owner deletion.
func TestSolanaCreditsRefuseDeletedNetworkBeforeIntent(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, testCase := range []struct {
			name string
			plan string
		}{
			{name: "supporter", plan: model.SolanaPlanMonthly},
			{name: "data", plan: StripeItemData1Tib},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-solana-"+testCase.name, userId)
			reference := "synthetic-solana-reference-" + testCase.name
			if testCase.plan == model.SolanaPlanMonthly {
				clientId := server.NewId()
				userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
					NetworkId: networkId,
					ClientId:  &clientId,
					UserId:    userId,
				})
				if err := model.CreateSolanaPaymentIntent(reference, 1, testCase.plan, userSession); err != nil {
					t.Fatalf("create %s intent: %v", testCase.name, err)
				}
			} else if err := model.CreateSolanaPaymentIntentForNetwork(
				ctx,
				reference,
				networkId,
				1,
				testCase.plan,
				server.NowUtc().Add(time.Hour),
			); err != nil {
				t.Fatalf("create %s intent: %v", testCase.name, err)
			}
			deletePaymentTestNetwork(t, ctx, networkId, userId)

			credited, err := solanaCreditPaymentIntent(
				session.Testing_CreateClientSession(ctx, nil),
				&model.PaymentIntentSearchResult{
					NetworkId:         &networkId,
					PaymentReference:  reference,
					ExpectedAmountUsd: 1,
					SubscriptionPlan:  testCase.plan,
				},
				"synthetic-solana-signature-"+testCase.name,
				1,
			)
			if credited || !errors.Is(err, model.ErrPaymentNetworkNotFound) {
				t.Fatalf("%s credit after delete = %v, %v", testCase.name, credited, err)
			}
			intent := model.GetSolanaPaymentIntent(ctx, reference)
			if intent == nil || intent.TxSignature != nil {
				t.Fatalf("%s intent after refused credit = %#v", testCase.name, intent)
			}
			if balances := model.GetActiveTransferBalances(ctx, networkId); len(balances) != 0 {
				t.Fatalf("refused %s credit added %d balances", testCase.name, len(balances))
			}
		}
	})
}

// TestX402GrantsRefuseDeletedNetwork proves both x402 product branches reject
// after settlement without persisting a grant for a deleted owner.
func TestX402GrantsRefuseDeletedNetwork(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, testCase := range []struct {
			name string
			pro  bool
		}{
			{name: "pro", pro: true},
			{name: "data", pro: false},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-x402-"+testCase.name, userId)
			deletePaymentTestNetwork(t, ctx, networkId, userId)

			transactionId := "synthetic-x402-transaction-" + testCase.name
			sku := &X402Sku{
				SkuId:     X402SkuData1Tib,
				PriceUsd:  1,
				Pro:       testCase.pro,
				ByteCount: model.Tib,
			}
			settle := &X402SettleResponse{
				Success:     true,
				Transaction: transactionId,
				Network:     "synthetic-chain",
			}
			var err error
			if testCase.pro {
				sku.SkuId = X402SkuProMonth
				err = x402GrantProMonth(ctx, networkId, sku, model.UsdToNanoCents(1), settle)
			} else {
				err = x402GrantData(ctx, networkId, sku, model.UsdToNanoCents(1), settle)
			}
			if !errors.Is(err, model.ErrPaymentNetworkNotFound) {
				t.Fatalf("%s grant after delete = %v", testCase.name, err)
			}
			if renewals, balances := paymentGuardWriteCounts(ctx, transactionId); renewals != 0 || balances != 0 {
				t.Fatalf("refused %s grant persisted renewals=%d balances=%d", testCase.name, renewals, balances)
			}
		}
	})
}

// TestPlayCreditRefusesDeletedNetworkBeforeGrant proves the Google writer owns
// the shared guard before either its renewal or transfer-balance write.
func TestPlayCreditRefusesDeletedNetworkBeforeGrant(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			"synthetic-supporter": {
				FeeFraction:    0.3,
				PriceAmountUsd: 1,
				Supporter:      true,
			},
		})
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-play-late", userId)
		deletePaymentTestNetwork(t, ctx, networkId, userId)

		purchaseToken := "synthetic-play-token"
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"synthetic-supporter",
			server.NowUtc().Add(-time.Hour),
			server.NowUtc().Add(time.Hour),
		)
		result, err := PlaySubscriptionRenewal(
			&PlaySubscriptionRenewalArgs{
				NetworkId:      networkId,
				PackageName:    env.packageName,
				SubscriptionId: "synthetic-supporter",
				PurchaseToken:  purchaseToken,
			},
			session.Testing_CreateClientSession(ctx, nil),
		)
		if result != nil || !errors.Is(err, model.ErrPaymentNetworkNotFound) {
			t.Fatalf("Play credit after delete = %#v, %v", result, err)
		}
		if renewals, balances := paymentGuardWriteCounts(ctx, purchaseToken); renewals != 0 || balances != 0 {
			t.Fatalf("refused Play credit persisted renewals=%d balances=%d", renewals, balances)
		}
	})
}

// TestAppleCreditRefusesDeletedNetworkBeforeLedgers is a healthy-path control:
// both the old existence check and the new locking check reject a delayed
// verified notification before either Apple idempotency ledger is consumed.
func TestAppleCreditRefusesDeletedNetworkBeforeLedgers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-apple-late", userId)
		deletePaymentTestNetwork(t, ctx, networkId, userId)

		now := server.NowUtc().Truncate(time.Millisecond)
		notificationId := server.NewId()
		transactionId := "synthetic-apple-transaction"
		processed, err := ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
			NotificationType: "SUBSCRIBED",
			Subtype:          "INITIAL_BUY",
			NotificationUUID: notificationId.String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo: map[string]any{
				"appAccountToken": networkId.String(),
				"transactionId":   transactionId,
				"productId":       "synthetic-apple-product",
				"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
				"expiresDate":     float64(now.Add(time.Hour).UnixMilli()),
				"price":           float64(1000),
			},
		}, []string{"synthetic-apple-product"})
		if processed || err == nil {
			t.Fatalf("Apple credit after delete = %v, %v", processed, err)
		}

		notificationCount, transactionCount := 0, 0
		server.Db(ctx, func(conn server.PgConn) {
			returnErr := conn.QueryRow(
				ctx,
				`
					SELECT
						(SELECT count(*) FROM apple_notification WHERE notification_uuid = $1),
						(SELECT count(*) FROM apple_subscription_transaction WHERE transaction_id = $2)
				`,
				notificationId,
				transactionId,
			).Scan(&notificationCount, &transactionCount)
			server.Raise(returnErr)
		})
		if notificationCount != 0 || transactionCount != 0 {
			t.Fatalf("refused Apple credit persisted notification=%d transaction=%d", notificationCount, transactionCount)
		}
	})
}

// TestAutomaticBalanceCodeDeliveryRequiresNetworkOrEmail proves a deleted
// destination fails a no-email delivery while an emailed code remains usable.
func TestAutomaticBalanceCodeDeliveryRequiresNetworkOrEmail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		noEmailNetworkId := server.NewId()
		noEmailUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, noEmailNetworkId, "synthetic-code-no-email", noEmailUserId)
		deletePaymentTestNetwork(t, ctx, noEmailNetworkId, noEmailUserId)

		noEmailPurchase := "synthetic-code-purchase-no-email"
		err := createBalanceCode(
			ctx,
			1024,
			time.Hour,
			model.UsdToNanoCents(1),
			noEmailPurchase,
			"synthetic-code-record-no-email",
			"",
			&noEmailNetworkId,
			"synthetic-code-no-email",
		)
		if !errors.Is(err, model.ErrPaymentNetworkNotFound) {
			t.Fatalf("no-email automatic delivery = %v", err)
		}
		noEmailCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, noEmailPurchase)
		if err != nil {
			t.Fatalf("get retained no-email code id: %v", err)
		}
		noEmailCode, err := model.GetBalanceCode(ctx, noEmailCodeId)
		if err != nil || noEmailCode.RedeemNetworkId != nil || !noEmailCode.RedeemTime.IsZero() {
			t.Fatalf("retained no-email code = %#v, %v", noEmailCode, err)
		}

		emailNetworkId := server.NewId()
		emailUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, emailNetworkId, "synthetic-code-email", emailUserId)
		deletePaymentTestNetwork(t, ctx, emailNetworkId, emailUserId)
		sent := 0
		previousSender := GetAWSMessageSender()
		SetMessageSender(&mockAWSMessageSender{
			SendMessageFunc: func(userAuth string, template Template, sendOpts ...any) error {
				if userAuth != "synthetic@example.invalid" {
					t.Errorf("emailed recovery destination = %q", userAuth)
				}
				sent++
				return nil
			},
		})
		defer SetMessageSender(previousSender)

		emailPurchase := "synthetic-code-purchase-email"
		err = createBalanceCode(
			ctx,
			1024,
			time.Hour,
			model.UsdToNanoCents(1),
			emailPurchase,
			"synthetic-code-record-email",
			"synthetic@example.invalid",
			&emailNetworkId,
			"synthetic-code-email",
		)
		if err != nil {
			t.Fatalf("emailed recovery = %v", err)
		}
		if sent != 1 {
			t.Fatalf("emailed recovery count = %d, want 1", sent)
		}
		emailCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, emailPurchase)
		if err != nil {
			t.Fatalf("get retained emailed code id: %v", err)
		}
		emailCode, err := model.GetBalanceCode(ctx, emailCodeId)
		if err != nil || emailCode.RedeemNetworkId != nil || !emailCode.RedeemTime.IsZero() {
			t.Fatalf("retained emailed code = %#v, %v", emailCode, err)
		}
	})
}
