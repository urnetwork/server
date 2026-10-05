package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// waitForPaymentLockWaiter observes an actual PostgreSQL lock wait bearing the
// synthetic query marker.
func waitForPaymentLockWaiter(ctx context.Context, marker string) (returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("observe payment lock waiter: %v", recovered)
		}
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		waiting := false
		server.Db(ctx, func(conn server.PgConn) {
			returnErr = conn.QueryRow(
				ctx,
				`
					SELECT EXISTS (
						SELECT 1
						FROM pg_stat_activity
						WHERE datname = current_database()
							AND pid <> pg_backend_pid()
							AND state = 'active'
							AND wait_event_type = 'Lock'
							AND query LIKE '%' || $1 || '%'
					)
				`,
				marker,
			).Scan(&waiting)
		})
		if returnErr != nil {
			return fmt.Errorf("observe payment lock waiter: %w", returnErr)
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("query with marker %q did not enter a database lock wait: %w", marker, ctx.Err())
		case <-ticker.C:
		}
	}
}

// runPaymentModelTest starts a database interleaving and reports panics as
// ordinary test errors through a buffered completion channel.
func runPaymentModelTest(run func() error) <-chan error {
	done := make(chan error, 1)
	go func() {
		var returnErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				returnErr = fmt.Errorf("database interleaving panic: %v", recovered)
			}
			done <- returnErr
		}()
		returnErr = run()
	}()
	return done
}

// awaitPaymentModelTest bounds cleanup of one synthetic database interleaving.
func awaitPaymentModelTest(done <-chan error) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errors.New("database interleaving did not finish")
	}
}

func TestRemoveNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)

		/**
		 * Add SSO Auth
		 */
		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeGoogle,
			UserAuth: email,
			UserName: "",
		}

		err := addSsoAuth(&AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        userId,
		}, ctx)
		connect.AssertEqual(t, err, nil)

		/**
		 * Add Wallet Auth
		 */

		walletAuth := signedAcceptanceWalletChallenge(t, ctx, newSolanaAcceptanceWalletSigner(t))
		err = addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: walletAuth,
				UserId:     userId,
			},
			ctx,
		)
		connect.AssertEqual(t, err, nil)

		networkUser := GetNetworkUser(ctx, userId)
		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, len(networkUser.UserAuths), 1)
		connect.AssertEqual(t, len(networkUser.SsoAuths), 1)
		connect.AssertEqual(t, len(networkUser.WalletAuths), 1)

		RemoveNetwork(ctx, networkId, &userId)

		networkUser = GetNetworkUser(ctx, userId)
		connect.AssertEqual(t, networkUser, nil)

		userAuths, err := getUserAuths(userId, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(userAuths), 0)

		ssoAuths, err := getSsoAuths(ctx, userId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(ssoAuths), 0)

		walletAuths, err := getWalletAuths(ctx, userId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletAuths), 0)

	})
}

// TestRemoveNetworkRefusesFutureStripeRenewal pins the model-level guard used
// by direct CLI callers: queued Stripe time is cancellation work even before
// its local start time arrives.
func TestRemoveNetworkRefusesFutureStripeRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-future-stripe", userId)
		now := server.NowUtc()
		if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   SubscriptionTypeSupporter,
			StartTime:          now.Add(time.Hour),
			EndTime:            now.Add(2 * time.Hour),
			SubscriptionMarket: SubscriptionMarketStripe,
			TransactionId:      "in_synthetic_future",
		}); err != nil {
			t.Fatalf("add future renewal: %v", err)
		}

		if success, _ := RemoveNetwork(ctx, networkId, &userId); success {
			t.Fatal("direct removal bypassed a future Stripe renewal")
		}

		networkExists := false
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT EXISTS (SELECT 1 FROM network WHERE network_id = $1)`, networkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&networkExists))
				}
			})
		})
		if !networkExists {
			t.Fatal("guarded direct removal deleted the network")
		}
	})
}

// TestRemoveNetworkRechecksStripeAfterWaitingForCredit proves the exact
// credit-first ordering: deletion waits on the credit's shared row lock, then
// ReadCommitted sees the newly committed renewal and refuses deletion.
func TestRemoveNetworkRechecksStripeAfterWaitingForCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-credit-first", userId)
		now := server.NowUtc()
		renewal := &SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   SubscriptionTypeSupporter,
			StartTime:          now.Add(-time.Minute),
			EndTime:            now.Add(time.Hour),
			SubscriptionMarket: SubscriptionMarketStripe,
			TransactionId:      "in_synthetic_credit_first",
		}

		creditLocked := make(chan struct{})
		releaseCredit := make(chan struct{})
		creditDone := runPaymentModelTest(func() error {
			var returnErr error
			server.Tx(ctx, func(tx server.PgTx) {
				returnErr = LockPaymentNetworkInTx(tx, ctx, networkId)
				if returnErr != nil {
					return
				}
				close(creditLocked)
				select {
				case <-releaseCredit:
				case <-ctx.Done():
					returnErr = ctx.Err()
					return
				}
				returnErr = AddSubscriptionRenewalInTx(tx, ctx, renewal)
			}, server.TxReadCommitted, server.OptNoRetry())
			return returnErr
		})
		select {
		case <-creditLocked:
		case <-ctx.Done():
			if err := awaitPaymentModelTest(creditDone); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("credit setup cleanup: %v", err)
			}
			t.Fatal("credit transaction did not acquire its row lock")
		}

		removed := false
		removeDone := runPaymentModelTest(func() error {
			removed, _ = RemoveNetwork(ctx, networkId, &userId)
			return nil
		})

		waitErr := waitForPaymentLockWaiter(ctx, "payment-network-delete-lock")
		if waitErr == nil {
			close(releaseCredit)
		} else {
			cancel()
		}
		creditErr := awaitPaymentModelTest(creditDone)
		removeErr := awaitPaymentModelTest(removeDone)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if creditErr != nil {
			t.Fatalf("credit transaction: %v", creditErr)
		}
		if removeErr != nil {
			t.Fatalf("remove transaction: %v", removeErr)
		}
		if removed {
			t.Fatal("delete missed the renewal committed by the lock winner")
		}

		networkExists, renewalExists := false, false
		server.Db(ctx, func(conn server.PgConn) {
			returnErr := conn.QueryRow(
				ctx,
				`
					SELECT
						EXISTS (SELECT 1 FROM network WHERE network_id = $1),
						EXISTS (SELECT 1 FROM subscription_renewal WHERE transaction_id = $2)
				`,
				networkId,
				renewal.TransactionId,
			).Scan(&networkExists, &renewalExists)
			server.Raise(returnErr)
		})
		if !networkExists || !renewalExists {
			t.Fatalf("credit-first state = network %v renewal %v, want both true", networkExists, renewalExists)
		}
	})
}

// removeNetworkTestStoreRenewal is an active App Store or Google Play renewal
// whose store id is identity. Distinct end times keep renewals of one market
// from colliding on the renewal key.
func removeNetworkTestStoreRenewal(
	networkId server.Id,
	market SubscriptionMarket,
	identity string,
	startTime time.Time,
	endTime time.Time,
) *SubscriptionRenewal {
	renewal := &SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		SubscriptionMarket: market,
	}
	switch market {
	case SubscriptionMarketApple:
		renewal.TransactionId = identity
	case SubscriptionMarketGoogle:
		renewal.PurchaseToken = identity
	}
	return renewal
}

func removeNetworkTestNetworkExists(ctx context.Context, networkId server.Id) bool {
	networkExists := false
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network WHERE network_id = $1)`,
			networkId,
		).Scan(&networkExists))
	})
	return networkExists
}

// The deletion refuses a store renewal its snapshot does not name. A nil
// snapshot (no store steps ran) names none, and a renewal without a store id
// is never named, even when the snapshot holds an empty id.
func TestRemoveNetworkStoreSnapshotCovers(t *testing.T) {
	storeSnapshot := &RemoveNetworkStoreSnapshot{
		AppleTransactionIds: []string{"synthetic-apple-checked", ""},
		PlayPurchaseTokens:  []string{"synthetic-play-checked", ""},
	}
	var noSnapshot *RemoveNetworkStoreSnapshot
	apple := func(transactionId string) *ActiveSubscriptionRenewal {
		return &ActiveSubscriptionRenewal{Market: SubscriptionMarketApple, TransactionId: transactionId}
	}
	play := func(purchaseToken string) *ActiveSubscriptionRenewal {
		return &ActiveSubscriptionRenewal{Market: SubscriptionMarketGoogle, PurchaseToken: purchaseToken}
	}
	for _, c := range []struct {
		name     string
		snapshot *RemoveNetworkStoreSnapshot
		renewal  *ActiveSubscriptionRenewal
		covered  bool
	}{
		{"checked apple", storeSnapshot, apple("synthetic-apple-checked"), true},
		{"apple credited later", storeSnapshot, apple("synthetic-apple-new"), false},
		{"checked play", storeSnapshot, play("synthetic-play-checked"), true},
		{"play credited later", storeSnapshot, play("synthetic-play-new"), false},
		{"apple with a play id", storeSnapshot, apple("synthetic-play-checked"), false},
		{"play with an apple id", storeSnapshot, play("synthetic-apple-checked"), false},
		{"apple without an id", storeSnapshot, apple(""), false},
		{"play without an id", storeSnapshot, play(""), false},
		{"apple without store steps", noSnapshot, apple("synthetic-apple-checked"), false},
		{"play without store steps", noSnapshot, play("synthetic-play-checked"), false},
		{"other market", storeSnapshot, &ActiveSubscriptionRenewal{Market: SubscriptionMarketStripe, TransactionId: "synthetic-apple-checked"}, false},
	} {
		if covered := c.snapshot.Covers(c.renewal); covered != c.covered {
			t.Errorf("%s: covered = %v, want %v", c.name, covered, c.covered)
		}
	}
}

// TestRemoveNetworkRechecksStoreRenewalsAfterWaitingForCredit is the App
// Store and Google Play form of the Stripe test above. The deletion's store
// steps ran against a snapshot; a credit holding the shared row lock then
// commits a renewal the snapshot does not name; the deletion that waited on
// the lock refuses instead of leaving that renewal billing a deleted account.
func TestRemoveNetworkRechecksStoreRenewalsAfterWaitingForCredit(t *testing.T) {
	for _, market := range []SubscriptionMarket{SubscriptionMarketApple, SubscriptionMarketGoogle} {
		t.Run(market, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				networkId := server.NewId()
				userId := server.NewId()
				Testing_CreateNetwork(ctx, networkId, "synthetic-store-credit-first", userId)

				// the store steps ran before the credit, with nothing to check
				storeSnapshot := GetRemoveNetworkStoreSnapshot(ctx, networkId)
				now := server.NowUtc()
				renewal := removeNetworkTestStoreRenewal(
					networkId,
					market,
					"synthetic-store-credit-first",
					now.Add(-time.Minute),
					now.Add(time.Hour),
				)

				creditLocked := make(chan struct{})
				releaseCredit := make(chan struct{})
				creditDone := runPaymentModelTest(func() error {
					var returnErr error
					server.Tx(ctx, func(tx server.PgTx) {
						returnErr = LockPaymentNetworkInTx(tx, ctx, networkId)
						if returnErr != nil {
							return
						}
						close(creditLocked)
						select {
						case <-releaseCredit:
						case <-ctx.Done():
							returnErr = ctx.Err()
							return
						}
						returnErr = AddSubscriptionRenewalInTx(tx, ctx, renewal)
					}, server.TxReadCommitted, server.OptNoRetry())
					return returnErr
				})
				select {
				case <-creditLocked:
				case <-ctx.Done():
					if err := awaitPaymentModelTest(creditDone); err != nil && !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("credit setup cleanup: %v", err)
					}
					t.Fatal("credit transaction did not acquire its row lock")
				}

				var outcome RemoveNetworkOutcome
				removeDone := runPaymentModelTest(func() error {
					outcome, _ = RemoveNetworkWithStoreSnapshot(ctx, networkId, &userId, storeSnapshot)
					return nil
				})

				waitErr := waitForPaymentLockWaiter(ctx, "payment-network-delete-lock")
				if waitErr == nil {
					close(releaseCredit)
				} else {
					cancel()
				}
				creditErr := awaitPaymentModelTest(creditDone)
				removeErr := awaitPaymentModelTest(removeDone)
				if waitErr != nil {
					t.Fatal(waitErr)
				}
				if creditErr != nil {
					t.Fatalf("credit transaction: %v", creditErr)
				}
				if removeErr != nil {
					t.Fatalf("remove transaction: %v", removeErr)
				}
				if outcome != RemoveNetworkStoreRenewalUnchecked {
					t.Fatalf("delete outcome = %q, want %q for the renewal committed by the lock winner", outcome, RemoveNetworkStoreRenewalUnchecked)
				}
				if !removeNetworkTestNetworkExists(ctx, networkId) {
					t.Fatal("delete removed the network while the new store renewal bills it")
				}
			})
		})
	}
}

// The removal goes ahead for store renewals the snapshot names and refuses
// others: one credited after the snapshot, or any at all when no store steps
// ran (bringyourctl and other direct callers). The retry's fresh snapshot
// names the new renewal, including a queued one that has not started yet.
func TestRemoveNetworkStoreSnapshotGatesRemoval(t *testing.T) {
	for _, market := range []SubscriptionMarket{SubscriptionMarketApple, SubscriptionMarketGoogle} {
		t.Run(market, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				networkId := server.NewId()
				userId := server.NewId()
				Testing_CreateNetwork(ctx, networkId, "synthetic-store-snapshot", userId)
				now := server.NowUtc()
				addRenewal := func(identity string, startTime time.Time, endTime time.Time) {
					if err := AddSubscriptionRenewal(ctx, removeNetworkTestStoreRenewal(networkId, market, identity, startTime, endTime)); err != nil {
						t.Fatalf("add %s renewal %s: %v", market, identity, err)
					}
				}

				addRenewal("synthetic-store-checked", now.Add(-time.Minute), now.Add(time.Hour))

				// no store steps ran: any active store renewal refuses
				if outcome, _ := RemoveNetworkWithStoreSnapshot(ctx, networkId, &userId, nil); outcome != RemoveNetworkStoreRenewalUnchecked {
					t.Fatalf("removal without a snapshot: outcome %q, want %q", outcome, RemoveNetworkStoreRenewalUnchecked)
				}
				if success, _ := RemoveNetwork(ctx, networkId, &userId); success {
					t.Fatal("direct removal bypassed an active store renewal")
				}

				storeSnapshot := GetRemoveNetworkStoreSnapshot(ctx, networkId)

				// credited after the store steps
				addRenewal("synthetic-store-credited-later", now.Add(-time.Minute), now.Add(2*time.Hour))
				if outcome, _ := RemoveNetworkWithStoreSnapshot(ctx, networkId, &userId, storeSnapshot); outcome != RemoveNetworkStoreRenewalUnchecked {
					t.Fatalf("removal after a later credit: outcome %q, want %q", outcome, RemoveNetworkStoreRenewalUnchecked)
				}
				if !removeNetworkTestNetworkExists(ctx, networkId) {
					t.Fatal("refused removal deleted the network")
				}

				// queued to start later; still part of what the store steps check
				addRenewal("synthetic-store-queued", now.Add(time.Hour), now.Add(3*time.Hour))
				storeSnapshot = GetRemoveNetworkStoreSnapshot(ctx, networkId)
				identities := storeSnapshot.AppleTransactionIds
				if market == SubscriptionMarketGoogle {
					identities = storeSnapshot.PlayPurchaseTokens
				}
				for _, identity := range []string{"synthetic-store-checked", "synthetic-store-credited-later", "synthetic-store-queued"} {
					if !slices.Contains(identities, identity) {
						t.Fatalf("retry snapshot %+v is missing %s", storeSnapshot, identity)
					}
				}

				if outcome, _ := RemoveNetworkWithStoreSnapshot(ctx, networkId, &userId, storeSnapshot); outcome != RemoveNetworkRemoved {
					t.Fatalf("removal with a current snapshot: outcome %q, want %q", outcome, RemoveNetworkRemoved)
				}
				if removeNetworkTestNetworkExists(ctx, networkId) {
					t.Fatal("removed network still exists")
				}
			})
		})
	}
}

// TestPaymentCreditWaitsForDeleteAndObservesAbsence proves the delete-first
// ordering: a credit is visibly blocked on FOR KEY SHARE and, after the
// deletion commits, returns the missing-network result without a renewal.
func TestPaymentCreditWaitsForDeleteAndObservesAbsence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-delete-first", userId)

		deleteLocked := make(chan struct{})
		releaseDelete := make(chan struct{})
		deleteDone := runPaymentModelTest(func() error {
			server.Tx(ctx, func(tx server.PgTx) {
				_, found := lockPaymentNetworkForRemoveInTx(tx, ctx, networkId)
				if !found {
					server.Raise(fmt.Errorf("synthetic network missing before delete"))
				}
				close(deleteLocked)
				select {
				case <-releaseDelete:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network WHERE network_id = $1`, networkId))
			}, server.TxReadCommitted, server.OptNoRetry())
			return nil
		})
		select {
		case <-deleteLocked:
		case <-ctx.Done():
			if err := awaitPaymentModelTest(deleteDone); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("delete setup cleanup: %v", err)
			}
			t.Fatal("delete transaction did not acquire its row lock")
		}

		now := server.NowUtc()
		creditDone := runPaymentModelTest(func() error {
			return AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				StartTime:          now,
				EndTime:            now.Add(time.Hour),
				SubscriptionMarket: SubscriptionMarketStripe,
				TransactionId:      "in_synthetic_delete_first",
			})
		})

		waitErr := waitForPaymentLockWaiter(ctx, "payment-network-credit-lock")
		if waitErr == nil {
			close(releaseDelete)
		} else {
			cancel()
		}
		deleteErr := awaitPaymentModelTest(deleteDone)
		creditErr := awaitPaymentModelTest(creditDone)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if deleteErr != nil {
			t.Fatalf("delete transaction: %v", deleteErr)
		}
		if !errors.Is(creditErr, ErrPaymentNetworkNotFound) {
			t.Fatalf("credit after delete = %v", creditErr)
		}

		renewalExists := false
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT EXISTS (SELECT 1 FROM subscription_renewal WHERE transaction_id = $1)`,
				"in_synthetic_delete_first",
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&renewalExists))
				}
			})
		})
		if renewalExists {
			t.Fatal("blocked credit persisted after deletion")
		}
	})
}
