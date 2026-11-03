package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const playTerminalTestSubscriptionId = "synthetic_supporter"

func seedPlayTerminalEntitlement(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	purchaseToken string,
	startTime time.Time,
	endTime time.Time,
	pro bool,
) {
	t.Helper()
	if pro {
		err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            endTime,
			PurchaseToken:      purchaseToken,
			SubscriptionMarket: model.SubscriptionMarketGoogle,
		})
		if err != nil {
			t.Fatalf("seed Play renewal: %v", err)
		}
	}
	model.AddTransferBalance(ctx, &model.TransferBalance{
		NetworkId:             networkId,
		StartTime:             startTime,
		EndTime:               endTime,
		StartBalanceByteCount: model.Gib,
		BalanceByteCount:      model.Gib,
		PurchaseToken:         purchaseToken,
		Pro:                   pro,
	})
}

func countActivePlayRenewals(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	purchaseToken string,
) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT count(*)
			FROM subscription_renewal
			WHERE network_id = $1
			  AND market = $2
			  AND purchase_token = $3
			  AND end_time > $4
			`,
			networkId,
			model.SubscriptionMarketGoogle,
			purchaseToken,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

func countActivePlayBalances(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	purchaseToken string,
	pro bool,
) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT count(*)
			FROM transfer_balance
			WHERE network_id = $1
			  AND purchase_token = $2
			  AND pro = $3
			  AND start_time <= $4
			  AND $4 < end_time
			`,
			networkId,
			purchaseToken,
			pro,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

func countScheduledPlayRenewals(
	t testing.TB,
	ctx context.Context,
	purchaseToken string,
) (int, time.Time) {
	t.Helper()
	count := 0
	runAt := time.Time{}
	runOnceKey := task.RunOnce("play_subscription_renewal", purchaseToken).String()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT count(*), max(run_at)
			FROM pending_task
			WHERE run_once_key = $1
			`,
			runOnceKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var maxRunAt *time.Time
				server.Raise(result.Scan(&count, &maxRunAt))
				if maxRunAt != nil {
					runAt = *maxRunAt
				}
			}
		})
	})
	return count, runAt
}

func playTerminalTestArgs(
	networkId server.Id,
	packageName string,
	purchaseToken string,
) *PlaySubscriptionRenewalArgs {
	return &PlaySubscriptionRenewalArgs{
		NetworkId:      networkId,
		PackageName:    packageName,
		SubscriptionId: playTerminalTestSubscriptionId,
		PurchaseToken:  purchaseToken,
	}
}

// TestPlaySubscriptionRenewalTerminalStatesEndMatchingEntitlement pins the
// ordinary scheduled-poll defect: terminal provider states used to return
// Canceled and stop the task without ending the still-active local entitlement.
func TestPlaySubscriptionRenewalTerminalStatesEndMatchingEntitlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		now := server.NowUtc().Truncate(time.Second)

		cases := []struct {
			name           string
			state          string
			providerExpiry time.Time
		}{
			{
				name:           "expired",
				state:          "SUBSCRIPTION_STATE_EXPIRED",
				providerExpiry: now.Add(-time.Minute),
			},
			{
				name:           "pending_purchase_canceled",
				state:          "SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED",
				providerExpiry: now.Add(time.Hour),
			},
			{
				name:           "canceled_after_paid_expiry",
				state:          "SUBSCRIPTION_STATE_CANCELED",
				providerExpiry: now.Add(-time.Minute),
			},
		}
		for _, testCase := range cases {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "playterminal"+testCase.name, server.NewId())
			purchaseToken := "synthetic-terminal-" + testCase.name
			seedPlayTerminalEntitlement(
				t,
				ctx,
				networkId,
				purchaseToken,
				now.Add(-24*time.Hour),
				now.Add(24*time.Hour),
				true,
			)
			env.subscriptions[purchaseToken] = playTestSubscription(
				networkId,
				playTerminalTestSubscriptionId,
				now.Add(-24*time.Hour),
				testCase.providerExpiry,
			)
			env.subscriptions[purchaseToken].SubscriptionState = testCase.state

			result, err := PlaySubscriptionRenewal(
				playTerminalTestArgs(networkId, env.packageName, purchaseToken),
				clientSession,
			)
			if err != nil {
				t.Fatalf("%s renewal: %v", testCase.name, err)
			}
			if !result.Canceled || !result.Terminal || !result.EntitlementEnded {
				t.Fatalf("%s result = %+v, want terminal entitlement end", testCase.name, result)
			}
			if count := countActivePlayRenewals(t, ctx, networkId, purchaseToken); count != 0 {
				t.Fatalf("%s active renewals = %d, want 0", testCase.name, count)
			}
			if count := countActivePlayBalances(t, ctx, networkId, purchaseToken, true); count != 0 {
				t.Fatalf("%s active Pro balances = %d, want 0", testCase.name, count)
			}
			if model.IsProNetwork(ctx, networkId) {
				t.Fatalf("%s network remains Pro after terminal end", testCase.name)
			}
		}
	})
}

// A non-renewed subscription past its grace period must finish its post hook
// even when a guest or wallet admin has no recipient for the optional notice.
func TestPlaySubscriptionRenewalPostWithoutEmailCompletes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		previousSender := GetAWSMessageSender()
		sent := 0
		SetMessageSender(&mockAWSMessageSender{
			SendMessageFunc: func(string, Template, ...any) error {
				sent++
				return nil
			},
		})
		defer SetMessageSender(previousSender)

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		result := &PlaySubscriptionRenewalResult{ExpiryTime: server.NowUtc().Add(-2 * SubscriptionGracePeriod)}

		for _, accountType := range []string{"guest", "wallet"} {
			networkId := server.NewId()
			if accountType == "guest" {
				model.Testing_CreateGuestNetwork(ctx, networkId, "synthetic-no-email-guest", server.NewId())
			} else {
				wallet := solana.NewWallet()
				message := "synthetic subscription notice fixture"
				signature, err := wallet.PrivateKey.Sign([]byte(message))
				if err != nil {
					t.Fatal(err)
				}
				model.Testing_CreateNetworkByWallet(ctx, networkId, "synthetic-no-email-wallet", server.NewId(),
					wallet.PublicKey().String(), base64.StdEncoding.EncodeToString(signature[:]), message)
			}
			args := playTerminalTestArgs(networkId, "synthetic.package", "synthetic-no-email-"+accountType)
			var postErr error
			server.Tx(ctx, func(tx server.PgTx) {
				postErr = PlaySubscriptionRenewalPost(args, result, clientSession, tx)
			})
			if postErr != nil {
				t.Fatalf("%s completed post without recipient: err=%v", accountType, postErr)
			}
			if count := outboxMessageCount(t, ctx, networkId); count != 0 {
				t.Fatalf("%s completed post without recipient owes %d notices, want none", accountType, count)
			}
			if count, _ := countScheduledPlayRenewals(t, ctx, args.PurchaseToken); count != 0 {
				t.Fatalf("%s completed post scheduled %d additional renewals, want none", accountType, count)
			}
		}
		if sent != 0 {
			t.Fatalf("sent %d notices without a recipient", sent)
		}
	})
}

// An existing recipient still receives exactly one notice with the intended
// template when the completed renewal stops scheduling itself. The post sends
// nothing in its transaction; it adds the notice to the account message
// outbox, and a delivery after the commit sends it.
func TestPlaySubscriptionRenewalPostWithEmailSendsNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateGuestNetwork(ctx, networkId, "synthetic-email-recipient", userId)
		const recipient = "recipient@synthetic.example"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`UPDATE network_user SET user_auth = $1, auth_type = $2 WHERE user_id = $3`,
				recipient, model.AuthTypePassword, userId))
		})
		previousSender := GetAWSMessageSender()
		sent := 0
		SetMessageSender(&mockAWSMessageSender{
			SendMessageFunc: func(userAuth string, template Template, _ ...any) error {
				if userAuth != recipient {
					t.Errorf("notice recipient = %q, want %q", userAuth, recipient)
				}
				if _, ok := template.(*SubscriptionEndedTemplate); !ok {
					t.Errorf("notice template = %T", template)
				}
				sent++
				return nil
			},
		})
		defer SetMessageSender(previousSender)

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		args := playTerminalTestArgs(networkId, "synthetic.package", "synthetic-email-token")
		result := &PlaySubscriptionRenewalResult{ExpiryTime: server.NowUtc().Add(-2 * SubscriptionGracePeriod)}
		var postErr error
		openSent := 0
		server.Tx(ctx, func(tx server.PgTx) {
			postErr = PlaySubscriptionRenewalPost(args, result, clientSession, tx)
			openSent = sent
		})
		if postErr != nil || openSent != 0 {
			t.Fatalf("email completed post error=%v notices in its transaction=%d, want none", postErr, openSent)
		}
		if count := outboxMessageCount(t, ctx, networkId); count != 1 {
			t.Fatalf("email completed post owes %d notices, want one", count)
		}
		// the delivery task sends it after the finish commits
		deliverAccountMessagesAt(ctx, GetAWSMessageSender(), server.NowUtc())
		if sent != 1 {
			t.Fatalf("email completed post notices=%d, want one", sent)
		}
		if count, _ := countScheduledPlayRenewals(t, ctx, args.PurchaseToken); count != 0 {
			t.Fatalf("email completed post scheduled %d additional renewals, want none", count)
		}
	})
}

// A canceled database lookup must remain a failed post even for an account
// whose successful lookup would have produced the skippable recipient error.
// The lookup reads on the post's transaction with the session's context, so the
// failure is that context's cancellation.
func TestPlaySubscriptionRenewalPostPreservesDatabaseCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateGuestNetwork(ctx, networkId, "synthetic-canceled-recipient", server.NewId())
		previousSender := GetAWSMessageSender()
		sent := 0
		SetMessageSender(&mockAWSMessageSender{
			SendMessageFunc: func(string, Template, ...any) error {
				sent++
				return nil
			},
		})
		defer SetMessageSender(previousSender)

		lookupCtx, cancel := context.WithCancel(ctx)
		clientSession := session.Testing_CreateClientSession(lookupCtx, nil)
		cancel()
		args := playTerminalTestArgs(networkId, "synthetic.package", "synthetic-canceled-recipient-token")
		result := &PlaySubscriptionRenewalResult{ExpiryTime: server.NowUtc().Add(-2 * SubscriptionGracePeriod)}
		var postErr error
		func() {
			defer func() {
				if value := recover(); value != nil {
					var ok bool
					if postErr, ok = value.(error); !ok {
						panic(value)
					}
				}
			}()
			// Keep the transaction live so cancellation occurs in GetUserAuthInTx.
			server.Tx(ctx, func(tx server.PgTx) {
				postErr = PlaySubscriptionRenewalPost(args, result, clientSession, tx)
			})
		}()
		if !errors.Is(postErr, context.Canceled) {
			t.Fatalf("canceled lookup error = %v, want the session's cancellation", postErr)
		}
		if sent != 0 {
			t.Fatalf("sent %d notices after failed recipient lookup", sent)
		}
		if count := outboxMessageCount(t, ctx, networkId); count != 0 {
			t.Fatalf("failed recipient lookup owes %d notices, want none", count)
		}
		if count, _ := countScheduledPlayRenewals(t, ctx, args.PurchaseToken); count != 0 {
			t.Fatalf("failed post scheduled %d additional renewals, want none", count)
		}
	})
}

// TestPlaySubscriptionRenewalFutureCancellationPreservesAndReschedules proves
// cancel-at-period-end is not terminal: paid access remains and the task keeps
// one poll at the maximum provider paid-through expiry.
func TestPlaySubscriptionRenewalFutureCancellationPreservesAndReschedules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		now := server.NowUtc().Truncate(time.Second)
		providerExpiry := now.Add(12 * time.Hour)
		localEndTime := providerExpiry.Add(SubscriptionGracePeriod)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playfuturecancel", server.NewId())
		purchaseToken := "synthetic-future-cancel"
		seedPlayTerminalEntitlement(
			t,
			ctx,
			networkId,
			purchaseToken,
			now.Add(-24*time.Hour),
			localEndTime,
			true,
		)
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			playTerminalTestSubscriptionId,
			now.Add(-24*time.Hour),
			now.Add(-time.Minute),
		)
		// Mixed line-item expiries pin the max-expiry boundary. Scheduling the
		// past minimum would immediately loop while paid access remains.
		env.subscriptions[purchaseToken].LineItems = append(
			env.subscriptions[purchaseToken].LineItems,
			&PlaySubscriptionPurchaseLineItem{
				ProductId:  playTerminalTestSubscriptionId,
				ExpiryTime: providerExpiry.UTC().Format(time.RFC3339),
			},
		)
		env.subscriptions[purchaseToken].SubscriptionState = "SUBSCRIPTION_STATE_CANCELED"
		args := playTerminalTestArgs(networkId, env.packageName, purchaseToken)

		result, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil {
			t.Fatalf("future cancellation renewal: %v", err)
		}
		if !result.Canceled || result.Terminal || result.EntitlementEnded {
			t.Fatalf("future cancellation result = %+v, want paid-through cancellation", result)
		}
		if !result.ExpiryTime.Equal(providerExpiry) {
			t.Fatalf("future cancellation expiry = %s, want maximum %s", result.ExpiryTime, providerExpiry)
		}
		if count := countActivePlayRenewals(t, ctx, networkId, purchaseToken); count != 1 {
			t.Fatalf("active renewals = %d, want 1", count)
		}
		if count := countActivePlayBalances(t, ctx, networkId, purchaseToken, true); count != 1 {
			t.Fatalf("active Pro balances = %d, want 1", count)
		}
		if !model.IsProNetwork(ctx, networkId) {
			t.Fatal("future cancellation removed paid-through Pro access")
		}

		server.Tx(ctx, func(tx server.PgTx) {
			if err := PlaySubscriptionRenewalPost(args, result, clientSession, tx); err != nil {
				t.Fatalf("future cancellation post: err=%v", err)
			}
		})
		if count := outboxMessageCount(t, ctx, networkId); count != 0 {
			t.Fatalf("future cancellation post owes %d notices, want none", count)
		}
		count, runAt := countScheduledPlayRenewals(t, ctx, purchaseToken)
		if count != 1 {
			t.Fatalf("scheduled future-cancellation polls = %d, want 1", count)
		}
		if !runAt.Equal(providerExpiry) {
			t.Fatalf("future-cancellation poll = %s, want %s", runAt, providerExpiry)
		}

		// Finished tasks written by the prior result schema deserialize with
		// Terminal=false and a zero expiry. They retain the historical stop
		// behavior instead of creating an immediate year-one retry loop.
		legacyToken := "synthetic-legacy-canceled"
		legacyArgs := playTerminalTestArgs(networkId, env.packageName, legacyToken)
		server.Tx(ctx, func(tx server.PgTx) {
			if err := PlaySubscriptionRenewalPost(
				legacyArgs,
				&PlaySubscriptionRenewalResult{Canceled: true},
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("legacy canceled post: err=%v", err)
			}
		})
		if count := outboxMessageCount(t, ctx, networkId); count != 0 {
			t.Fatalf("legacy canceled post owes %d notices, want none", count)
		}
		if count, _ := countScheduledPlayRenewals(t, ctx, legacyToken); count != 0 {
			t.Fatalf("scheduled legacy canceled polls = %d, want 0", count)
		}

		// If task post-processing crosses the provider expiry, the retained
		// poll becomes due now rather than disappearing or remaining in the
		// past. The next execution can then observe and apply terminal state.
		crossedToken := "synthetic-crossed-canceled"
		crossedArgs := playTerminalTestArgs(networkId, env.packageName, crossedToken)
		beforePost := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := PlaySubscriptionRenewalPost(
				crossedArgs,
				&PlaySubscriptionRenewalResult{
					Canceled:   true,
					ExpiryTime: beforePost.Add(-time.Minute),
				},
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("crossed-boundary canceled post: err=%v", err)
			}
		})
		if count := outboxMessageCount(t, ctx, networkId); count != 0 {
			t.Fatalf("crossed-boundary canceled post owes %d notices, want none", count)
		}
		afterPost := server.NowUtc()
		count, runAt = countScheduledPlayRenewals(t, ctx, crossedToken)
		if count != 1 {
			t.Fatalf("scheduled crossed-boundary polls = %d, want 1", count)
		}
		if runAt.Before(beforePost) || afterPost.Before(runAt) {
			t.Fatalf(
				"crossed-boundary poll = %s, want between %s and %s",
				runAt,
				beforePost,
				afterPost,
			)
		}
	})
}

// TestPlaySubscriptionRenewalEndsOnlyMatchingNetworkAndToken makes the scope
// adversarial: the same network has another Play purchase, another network has
// the same synthetic token, and the target token has a data-only balance. Only
// the matching network's matching-token Pro entitlement may end.
func TestPlaySubscriptionRenewalEndsOnlyMatchingNetworkAndToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		now := server.NowUtc().Truncate(time.Second)
		startTime := now.Add(-24 * time.Hour)
		targetEndTime := now.Add(24 * time.Hour)
		targetNetworkId := server.NewId()
		siblingNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, targetNetworkId, "playtargetscope", server.NewId())
		model.Testing_CreateNetwork(ctx, siblingNetworkId, "playsiblingscope", server.NewId())
		targetToken := "synthetic-target-token"
		unrelatedToken := "synthetic-unrelated-token"

		seedPlayTerminalEntitlement(t, ctx, targetNetworkId, targetToken, startTime, targetEndTime, true)
		seedPlayTerminalEntitlement(
			t,
			ctx,
			targetNetworkId,
			unrelatedToken,
			startTime.Add(time.Minute),
			targetEndTime.Add(time.Hour),
			true,
		)
		// This standalone Pro balance deliberately shares the target renewal's
		// exact end time. Window-only matching would incorrectly end it.
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             targetNetworkId,
			StartTime:             startTime,
			EndTime:               targetEndTime,
			StartBalanceByteCount: model.Gib,
			BalanceByteCount:      model.Gib,
			PurchaseToken:         unrelatedToken,
			Pro:                   true,
		})
		seedPlayTerminalEntitlement(t, ctx, targetNetworkId, targetToken, startTime, targetEndTime, false)
		seedPlayTerminalEntitlement(t, ctx, siblingNetworkId, targetToken, startTime, targetEndTime, true)

		env.subscriptions[targetToken] = playTestSubscription(
			targetNetworkId,
			playTerminalTestSubscriptionId,
			startTime,
			now.Add(-time.Minute),
		)
		env.subscriptions[targetToken].SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"

		result, err := PlaySubscriptionRenewal(
			playTerminalTestArgs(targetNetworkId, env.packageName, targetToken),
			clientSession,
		)
		if err != nil {
			t.Fatalf("scoped terminal renewal: %v", err)
		}
		if !result.EntitlementEnded {
			t.Fatalf("scoped terminal result = %+v, want end", result)
		}
		if count := countActivePlayRenewals(t, ctx, targetNetworkId, targetToken); count != 0 {
			t.Fatalf("target active renewals = %d, want 0", count)
		}
		if count := countActivePlayBalances(t, ctx, targetNetworkId, targetToken, true); count != 0 {
			t.Fatalf("target active Pro balances = %d, want 0", count)
		}
		if count := countActivePlayRenewals(t, ctx, targetNetworkId, unrelatedToken); count != 1 {
			t.Fatalf("unrelated-token active renewals = %d, want 1", count)
		}
		if count := countActivePlayBalances(t, ctx, targetNetworkId, unrelatedToken, true); count != 2 {
			t.Fatalf("unrelated-token active Pro balances = %d, want 2", count)
		}
		if count := countActivePlayBalances(t, ctx, targetNetworkId, targetToken, false); count != 1 {
			t.Fatalf("target-token data balances = %d, want 1", count)
		}
		if count := countActivePlayRenewals(t, ctx, siblingNetworkId, targetToken); count != 1 {
			t.Fatalf("sibling-network active renewals = %d, want 1", count)
		}
		if count := countActivePlayBalances(t, ctx, siblingNetworkId, targetToken, true); count != 1 {
			t.Fatalf("sibling-network active Pro balances = %d, want 1", count)
		}
		if !model.IsProNetwork(ctx, targetNetworkId) || !model.IsProNetwork(ctx, siblingNetworkId) {
			t.Fatal("terminal end removed an unrelated network or purchase entitlement")
		}
	})
}

// TestPlaySubscriptionRenewalGoneEndsIdempotentlyAndStops covers the provider
// 410 path and redelivery. A completed terminal task must not schedule another
// poll, and a second delivery must report a no-op.
func TestPlaySubscriptionRenewalGoneEndsIdempotentlyAndStops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		now := server.NowUtc().Truncate(time.Second)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playgone", server.NewId())
		purchaseToken := "synthetic-gone-token"
		seedPlayTerminalEntitlement(
			t,
			ctx,
			networkId,
			purchaseToken,
			now.Add(-24*time.Hour),
			now.Add(24*time.Hour),
			true,
		)
		env.statusFailures[purchaseToken] = 410
		args := playTerminalTestArgs(networkId, env.packageName, purchaseToken)

		first, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil {
			t.Fatalf("first gone renewal: %v", err)
		}
		if !first.Terminal || !first.EntitlementEnded {
			t.Fatalf("first gone result = %+v, want terminal end", first)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := PlaySubscriptionRenewalPost(args, first, clientSession, tx); err != nil {
				t.Fatalf("gone renewal post: err=%v", err)
			}
		})
		if count := outboxMessageCount(t, ctx, networkId); count != 0 {
			t.Fatalf("gone renewal post owes %d notices, want none", count)
		}
		if count, _ := countScheduledPlayRenewals(t, ctx, purchaseToken); count != 0 {
			t.Fatalf("scheduled gone polls = %d, want 0", count)
		}

		second, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil {
			t.Fatalf("redelivered gone renewal: %v", err)
		}
		if !second.Terminal || second.EntitlementEnded {
			t.Fatalf("redelivered gone result = %+v, want terminal no-op", second)
		}
		if count := countActivePlayRenewals(t, ctx, networkId, purchaseToken); count != 0 {
			t.Fatalf("active renewals after redelivery = %d, want 0", count)
		}
		if count := countActivePlayBalances(t, ctx, networkId, purchaseToken, true); count != 0 {
			t.Fatalf("active Pro balances after redelivery = %d, want 0", count)
		}
	})
}

// TestPlaySubscriptionRenewalTerminalEndFailureReturnsError proves the end is
// part of task success, not best-effort cleanup: a storage failure is returned
// and the scheduler can retry the same idempotent operation.
func TestPlaySubscriptionRenewalTerminalEndFailureReturnsError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		now := server.NowUtc().Truncate(time.Second)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playendfailure", server.NewId())
		purchaseToken := "synthetic-end-failure"
		seedPlayTerminalEntitlement(
			t,
			ctx,
			networkId,
			purchaseToken,
			now.Add(-24*time.Hour),
			now.Add(24*time.Hour),
			true,
		)
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			playTerminalTestSubscriptionId,
			now.Add(-24*time.Hour),
			now.Add(-time.Minute),
		)
		env.subscriptions[purchaseToken].SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"

		previousEnd := endPlaySubscriptionEntitlement
		endPlaySubscriptionEntitlement = func(
			server.PgConn,
			context.Context,
			server.Id,
			string,
			time.Time,
		) (bool, error) {
			return false, errors.New("synthetic storage failure")
		}
		t.Cleanup(func() { endPlaySubscriptionEntitlement = previousEnd })

		result, err := PlaySubscriptionRenewal(
			playTerminalTestArgs(networkId, env.packageName, purchaseToken),
			clientSession,
		)
		if result != nil || err == nil {
			t.Fatalf("terminal storage failure result = %+v, err = %v", result, err)
		}
		if !strings.Contains(err.Error(), "could not end terminal Play entitlement") {
			t.Fatalf("terminal storage error = %q, want stable task context", err)
		}
		if count := countActivePlayRenewals(t, ctx, networkId, purchaseToken); count != 1 {
			t.Fatalf("active renewals after failed end = %d, want 1", count)
		}
	})
}

// TestPlaySubscriptionRenewalTerminalEndSerializesAfterCredit uses the shared
// database locks as a channel-driven race barrier. The terminal end must wait
// behind an in-flight credit of the same network/token, then observe and end
// that credit after it commits.
func TestPlaySubscriptionRenewalTerminalEndSerializesAfterCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		now := server.NowUtc().Truncate(time.Second)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playterminalrace", server.NewId())
		purchaseToken := "synthetic-terminal-race"
		startTime := now.Add(-time.Hour)
		endTime := now.Add(24 * time.Hour)

		releaseCredit := make(chan struct{})
		releasedCredit := false
		defer func() {
			if !releasedCredit {
				close(releaseCredit)
			}
		}()
		writerReady := make(chan int, 1)
		writerDone := make(chan error, 1)
		go func() {
			var writerErr error
			server.Tx(ctx, func(tx server.PgTx) {
				writerErr = model.LockPlaySubscriptionPurchaseInTx(
					tx,
					ctx,
					networkId,
					purchaseToken,
				)
				if writerErr != nil {
					return
				}
				writerErr = model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
					NetworkId:          networkId,
					SubscriptionType:   model.SubscriptionTypeSupporter,
					StartTime:          startTime,
					EndTime:            endTime,
					PurchaseToken:      purchaseToken,
					SubscriptionMarket: model.SubscriptionMarketGoogle,
				})
				if writerErr != nil {
					return
				}
				model.AddTransferBalanceInTx(ctx, tx, &model.TransferBalance{
					NetworkId:             networkId,
					StartTime:             startTime,
					EndTime:               endTime,
					StartBalanceByteCount: model.Gib,
					BalanceByteCount:      model.Gib,
					PurchaseToken:         purchaseToken,
					Pro:                   true,
				})
				var backendPid int
				writerErr = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPid)
				if writerErr != nil {
					return
				}
				writerReady <- backendPid
				<-releaseCredit
			}, server.TxReadCommitted)
			writerDone <- writerErr
		}()

		var writerPid int
		select {
		case writerPid = <-writerReady:
		case <-ctx.Done():
			t.Fatalf("credit lock was not acquired: %v", ctx.Err())
		}

		type endResult struct {
			ended bool
			err   error
		}
		endStarted := make(chan struct{})
		endDone := make(chan endResult, 1)
		go func() {
			close(endStarted)
			ended, err := model.EndReconciledEntitlementForNetworkPurchaseToken(
				ctx,
				networkId,
				model.SubscriptionMarketGoogle,
				purchaseToken,
				server.NowUtc(),
			)
			endDone <- endResult{ended: ended, err: err}
		}()
		<-endStarted

		waiting := false
		for !waiting {
			select {
			case <-ctx.Done():
				close(releaseCredit)
				releasedCredit = true
				t.Fatalf("terminal end did not wait on the credit lock: %v", ctx.Err())
			default:
			}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT EXISTS (
						SELECT 1
						FROM pg_locks held
						INNER JOIN pg_locks waiter
						  ON waiter.locktype = held.locktype
						 AND waiter.database = held.database
						 AND waiter.classid = held.classid
						 AND waiter.objid = held.objid
						 AND waiter.objsubid = held.objsubid
						WHERE held.locktype = 'advisory'
						  AND held.pid = $1
						  AND held.granted
						  AND NOT waiter.granted
					)
					`,
					writerPid,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&waiting))
					}
				})
			})
		}

		close(releaseCredit)
		releasedCredit = true
		if err := <-writerDone; err != nil {
			t.Fatalf("credit transaction: %v", err)
		}
		end := <-endDone
		if end.err != nil || !end.ended {
			t.Fatalf("serialized terminal end = %+v, want committed end", end)
		}
		if count := countActivePlayRenewals(t, ctx, networkId, purchaseToken); count != 0 {
			t.Fatalf("active renewals after serialized race = %d, want 0", count)
		}
		if count := countActivePlayBalances(t, ctx, networkId, purchaseToken, true); count != 0 {
			t.Fatalf("active Pro balances after serialized race = %d, want 0", count)
		}
	})
}

// TestPlaySubscriptionRenewalStaleActiveCannotRestoreTerminalEnd covers the
// opposite lock order. Once a terminal owner ends the local entitlement, an
// earlier ACTIVE response with an already-ended provider window cannot acquire
// the lock later and recreate that entitlement.
func TestPlaySubscriptionRenewalStaleActiveCannotRestoreTerminalEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			playTerminalTestSubscriptionId: {
				FeeFraction:    0.3,
				PriceAmountUsd: 5,
				Supporter:      true,
			},
		})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		now := server.NowUtc().Truncate(time.Second)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playstaleactive", server.NewId())
		purchaseToken := "synthetic-stale-active"
		seedPlayTerminalEntitlement(
			t,
			ctx,
			networkId,
			purchaseToken,
			now.Add(-24*time.Hour),
			now.Add(24*time.Hour),
			true,
		)
		args := playTerminalTestArgs(networkId, env.packageName, purchaseToken)
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			playTerminalTestSubscriptionId,
			now.Add(-24*time.Hour),
			now.Add(-time.Minute),
		)
		env.subscriptions[purchaseToken].SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"
		terminal, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil || !terminal.EntitlementEnded {
			t.Fatalf("terminal end result = %+v, err = %v", terminal, err)
		}

		// This is the response an ACTIVE caller could have fetched before the
		// terminal transaction acquired the shared lock.
		env.subscriptions[purchaseToken].SubscriptionState = "SUBSCRIPTION_STATE_ACTIVE"
		stale, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil {
			t.Fatalf("stale active renewal: %v", err)
		}
		if stale.Renewed {
			t.Fatalf("stale active result = %+v, want no credit", stale)
		}
		if count := countActivePlayRenewals(t, ctx, networkId, purchaseToken); count != 0 {
			t.Fatalf("stale ACTIVE restored %d renewal(s), want 0", count)
		}
		if count := countActivePlayBalances(t, ctx, networkId, purchaseToken, true); count != 0 {
			t.Fatalf("stale ACTIVE restored %d Pro balance(s), want 0", count)
		}
	})
}

// playGraceFixture is a Play purchase whose paid window has lapsed and that Play
// has in its grace period, with a billing recipient on the network.
type playGraceFixture struct {
	ctx           context.Context
	env           *playWebhookTestEnv
	clientSession *session.ClientSession
	networkId     server.Id
	token         string
	startTime     time.Time
	graceEnd      time.Time
}

func newPlayGraceFixture(t testing.TB, name string) *playGraceFixture {
	ctx := context.Background()
	env := newPlayWebhookTestEnv(t, map[string]*Sku{
		playTerminalTestSubscriptionId: {
			FeeFraction:    0.3,
			PriceAmountUsd: 5.00,
			Supporter:      true,
		},
	})
	clientSession := session.Testing_CreateClientSession(ctx, nil)
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateGuestNetwork(ctx, networkId, "synthetic-"+name, userId)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`UPDATE network_user SET user_auth = $1, auth_type = $2 WHERE user_id = $3`,
			name+"@synthetic.example", model.AuthTypePassword, userId))
	})

	startTime := server.NowUtc().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	paidThrough := server.NowUtc().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	seedPlayTerminalEntitlement(t, ctx, networkId, "synthetic-grace-"+name, startTime, paidThrough.Add(SubscriptionGracePeriod), true)

	graceEnd := server.NowUtc().Add(4 * 24 * time.Hour).Truncate(time.Second)
	subscription := playTestSubscription(networkId, playTerminalTestSubscriptionId, startTime, graceEnd)
	subscription.SubscriptionState = "SUBSCRIPTION_STATE_IN_GRACE_PERIOD"
	subscription.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
	env.subscriptions["synthetic-grace-"+name] = subscription

	return &playGraceFixture{
		ctx:           ctx,
		env:           env,
		clientSession: clientSession,
		networkId:     networkId,
		token:         "synthetic-grace-" + name,
		startTime:     startTime,
		graceEnd:      graceEnd,
	}
}

func (self *playGraceFixture) args() *PlaySubscriptionRenewalArgs {
	return playTerminalTestArgs(self.networkId, self.env.packageName, self.token)
}

// playGraceProRow is one in-window Pro balance of the network, which a
// zero-byte hold is among, unlike model.GetActiveTransferBalances.
type playGraceProRow struct {
	startByteCount    int64
	netRevenue        int64
	subsidyNetRevenue int64
	endTime           time.Time
}

func (self *playGraceFixture) proRows() []playGraceProRow {
	rows := []playGraceProRow{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`
			SELECT start_balance_byte_count, net_revenue_nano_cents, subsidy_net_revenue_nano_cents, end_time
			FROM transfer_balance
			WHERE network_id = $1 AND pro AND $2 < end_time
			ORDER BY start_balance_byte_count
			`,
			self.networkId,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row playGraceProRow
				server.Raise(result.Scan(&row.startByteCount, &row.netRevenue, &row.subsidyNetRevenue, &row.endTime))
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// Play holds a customer in its grace period entitled, so Pro stays. The hold is
// no credit: no bytes, no revenue. A second poll adds nothing.
func TestPlaySubscriptionRenewalGraceHoldsProWithoutACredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newPlayGraceFixture(t, "graceholds")
		ctx := fixture.ctx
		if model.IsProNetwork(ctx, fixture.networkId) {
			t.Fatal("the lapsed window must not read as Pro before the poll")
		}

		result, err := PlaySubscriptionRenewal(fixture.args(), fixture.clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.Renewed || result.Canceled || result.Terminal {
			t.Fatalf("a grace period credits and ends nothing: %+v", result)
		}
		if !model.IsProNetwork(ctx, fixture.networkId) {
			t.Fatal("Pro did not hold through Play's grace period")
		}
		rows := fixture.proRows()
		if len(rows) != 1 || rows[0].startByteCount != 0 || rows[0].netRevenue != 0 || rows[0].subsidyNetRevenue != 0 {
			t.Fatalf("the hold must mint nothing: %+v", rows)
		}
		if !rows[0].endTime.Equal(fixture.graceEnd.Add(SubscriptionGracePeriod)) {
			t.Fatalf("the hold ends %v, want the grace period's end with the store's day %v", rows[0].endTime, fixture.graceEnd.Add(SubscriptionGracePeriod))
		}
		if count := countActivePlayRenewals(t, ctx, fixture.networkId, fixture.token); count != 1 {
			t.Fatalf("open renewals = %d, want the hold's one", count)
		}

		if _, err := PlaySubscriptionRenewal(fixture.args(), fixture.clientSession); err != nil {
			t.Fatal(err)
		}
		if rows := fixture.proRows(); len(rows) != 1 {
			t.Fatalf("a second poll added a balance: %+v", rows)
		}
	})
}

// A renewal in grace has not ended: no ended notice, and the poll stays scheduled
// for the grace period's end, when Play has decided.
func TestPlaySubscriptionRenewalPostInGraceKeepsPollingWithoutANotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newPlayGraceFixture(t, "gracepost")
		ctx := fixture.ctx
		result, err := PlaySubscriptionRenewal(fixture.args(), fixture.clientSession)
		if err != nil {
			t.Fatal(err)
		}
		args := fixture.args()
		var postErr error
		server.Tx(ctx, func(tx server.PgTx) {
			postErr = PlaySubscriptionRenewalPost(args, result, fixture.clientSession, tx)
		})
		if postErr != nil {
			t.Fatal(postErr)
		}
		if count := outboxMessageCount(t, ctx, fixture.networkId); count != 0 {
			t.Fatalf("a subscription in grace was told it ended: %d notices", count)
		}
		if count, _ := countScheduledPlayRenewals(t, ctx, fixture.token); count != 1 {
			t.Fatalf("scheduled polls = %d, want one at the grace period's end", count)
		}
	})
}

// The charge lands: the recovered period gets its credit and its revenue, also
// when its expiry falls inside the hold's window.
func TestPlaySubscriptionRenewalRecoveryAfterGraceCredits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newPlayGraceFixture(t, "gracerecovers")
		ctx := fixture.ctx
		if _, err := PlaySubscriptionRenewal(fixture.args(), fixture.clientSession); err != nil {
			t.Fatal(err)
		}

		// recovered with the hold's own expiry: the 30-day grace on a monthly plan
		sub := fixture.env.subscriptions[fixture.token]
		sub.SubscriptionState = "SUBSCRIPTION_STATE_ACTIVE"
		result, err := PlaySubscriptionRenewal(fixture.args(), fixture.clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Renewed {
			t.Fatalf("the recovered charge was not credited: %+v", result)
		}
		credited := int64(0)
		for _, row := range fixture.proRows() {
			credited += row.startByteCount
		}
		if credited != int64(RefreshSupporterTransferBalance) {
			t.Fatalf("credited %d bytes, want one period's %d", credited, RefreshSupporterTransferBalance)
		}

		// and a redelivery of the same state does not credit again
		result, err = PlaySubscriptionRenewal(fixture.args(), fixture.clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.Renewed {
			t.Fatal("the same expiry was credited twice")
		}
		if !model.IsProNetwork(ctx, fixture.networkId) {
			t.Fatal("recovered, but not Pro")
		}
	})
}

// Play gives up: the subscription goes on hold, nothing new is written, and the
// hold runs out with its window.
func TestPlaySubscriptionRenewalOnHoldAfterGraceAddsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newPlayGraceFixture(t, "graceonhold")
		sub := fixture.env.subscriptions[fixture.token]
		sub.SubscriptionState = "SUBSCRIPTION_STATE_ON_HOLD"
		result, err := PlaySubscriptionRenewal(fixture.args(), fixture.clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.InGrace || result.GraceHeld || result.Renewed {
			t.Fatalf("on hold: %+v", result)
		}
		if model.IsProNetwork(fixture.ctx, fixture.networkId) {
			t.Fatal("a subscription on hold holds no Pro")
		}
	})
}

// A revocation during grace ends the hold with the rest of the purchase.
func TestPlayGraceHoldEndsWithThePurchase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newPlayGraceFixture(t, "graceends")
		if _, err := PlaySubscriptionRenewal(fixture.args(), fixture.clientSession); err != nil {
			t.Fatal(err)
		}
		model.EndReconciledEntitlementForPurchaseToken(fixture.ctx, model.SubscriptionMarketGoogle, fixture.token, server.NowUtc())
		if model.IsProNetwork(fixture.ctx, fixture.networkId) {
			t.Fatal("the hold outlived the purchase")
		}
	})
}

// The hourly reconcile holds Pro for a grace period its notification and poll
// both missed, once, with no credit.
func TestPaymentReconcileGoogleHoldsProThroughGrace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer disableAllReconcileStores(t)()
		fixture := newPlayGraceFixture(t, "gracereconcile")
		previousHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = previousHasCredentials })

		for _, want := range []int{1, 0} {
			result, err := RunPaymentReconciliationWithOptions(
				reconcileTestSession(t, fixture.ctx),
				&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketGoogle}},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Credited != 0 || result.EntitlementsRepaired != want {
				t.Fatalf("reconcile: credited %d, repaired %d, want 0 and %d", result.Credited, result.EntitlementsRepaired, want)
			}
			if !model.IsProNetwork(fixture.ctx, fixture.networkId) {
				t.Fatal("Pro did not hold through Play's grace period")
			}
		}
		if rows := fixture.proRows(); len(rows) != 1 || rows[0].startByteCount != 0 {
			t.Fatalf("the hold must mint nothing: %+v", rows)
		}
	})
}
