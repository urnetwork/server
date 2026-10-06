package model

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"maps"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type FindNetworkResult struct {
	NetworkId   server.Id
	NetworkName string
	UserAuths   []string
}

func FindNetworksByName(ctx context.Context, networkName string) ([]*FindNetworkResult, error) {
	findNetworkResults := []*FindNetworkResult{}

	searchResults := networkNameSearch().Around(ctx, networkName, 3)

	if len(searchResults) == 0 {
		return []*FindNetworkResult{}, nil
	}

	server.Db(ctx, func(conn server.PgConn) {
		args := []any{}
		networkIdPlaceholders := []string{}
		for i, searchResult := range searchResults {
			args = append(args, searchResult.ValueId)
			networkIdPlaceholders = append(networkIdPlaceholders, fmt.Sprintf("$%d", i+1))
		}

		result, err := conn.Query(
			ctx,
			`
				SELECT
					network.network_id,
					network.network_name,
					network_user.user_auth
				FROM network
				INNER JOIN network_user ON network_user.user_id = network.admin_user_id
				WHERE network.network_id IN (`+strings.Join(networkIdPlaceholders, ",")+`)
			`,
			args...,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				findNetworkResult := &FindNetworkResult{}
				var adminUserAuth string
				server.Raise(result.Scan(
					&findNetworkResult.NetworkId,
					&findNetworkResult.NetworkName,
					&adminUserAuth,
				))
				findNetworkResult.UserAuths = append(findNetworkResult.UserAuths, adminUserAuth)
				findNetworkResults = append(findNetworkResults, findNetworkResult)
			}
		})
	})

	return findNetworkResults, nil
}

func FindNetworksByUserAuth(ctx context.Context, userAuth string) ([]*FindNetworkResult, error) {
	findNetworkResults := []*FindNetworkResult{}

	normalUserAuth, _ := NormalUserAuthV1(&userAuth)

	if normalUserAuth == nil {
		return nil, fmt.Errorf("Bad user auth: %s", userAuth)
	}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network.network_id,
					network.network_name
				FROM network_user
				INNER JOIN network ON network.admin_user_id = network_user.user_id
				WHERE network_user.user_auth = $1
			`,
			*normalUserAuth,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				findNetworkResult := &FindNetworkResult{}
				server.Raise(result.Scan(
					&findNetworkResult.NetworkId,
					&findNetworkResult.NetworkName,
				))
				findNetworkResult.UserAuths = append(findNetworkResult.UserAuths, *normalUserAuth)
				findNetworkResults = append(findNetworkResults, findNetworkResult)
			}
		})
	})

	return findNetworkResults, nil
}

// What RemoveNetworkWithStoreSnapshot did.
type RemoveNetworkOutcome string

const (
	// the network and its users are removed
	RemoveNetworkRemoved RemoveNetworkOutcome = "removed"
	// no such network, or adminUserId is not its admin
	RemoveNetworkNotFound RemoveNetworkOutcome = "not_found"
	// a Stripe renewal is active; it must be cancelled first
	RemoveNetworkStripeRenewalActive RemoveNetworkOutcome = "stripe_renewal_active"
	// an App Store or Google Play renewal that the store snapshot does not
	// name is active: one credited after the deletion's store steps ran, or,
	// with no snapshot, any active one. The store steps must run again.
	RemoveNetworkStoreRenewalUnchecked RemoveNetworkOutcome = "store_renewal_unchecked"
)

// Names the App Store and Google Play renewals an account deletion checked with
// the stores before it removes the network: the App Store transactions it
// looked up and the Google Play purchases it cancelled.
// RemoveNetworkWithStoreSnapshot refuses any other active renewal from those
// stores that it finds under the deletion lock.
type RemoveNetworkStoreSnapshot struct {
	AppleTransactionIds []string
	PlayPurchaseTokens  []string
}

// Reports whether the deletion's store steps checked renewal. A nil
// snapshot (a caller that ran no store steps) covers nothing, and a renewal
// without a store identity is never covered: the store steps refuse it too.
func (self *RemoveNetworkStoreSnapshot) Covers(renewal *ActiveSubscriptionRenewal) bool {
	if self == nil {
		return false
	}
	switch renewal.Market {
	case SubscriptionMarketApple:
		return renewal.TransactionId != "" && slices.Contains(self.AppleTransactionIds, renewal.TransactionId)
	case SubscriptionMarketGoogle:
		return renewal.PurchaseToken != "" && slices.Contains(self.PlayPurchaseTokens, renewal.PurchaseToken)
	default:
		return false
	}
}

// Reads the App Store and Google Play renewals that may still bill the
// network, for an account deletion to check with the stores and then hand to
// RemoveNetworkWithStoreSnapshot.
func GetRemoveNetworkStoreSnapshot(ctx context.Context, networkId server.Id) *RemoveNetworkStoreSnapshot {
	storeSnapshot := &RemoveNetworkStoreSnapshot{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, renewal := range removeNetworkStoreRenewals(ctx, conn, networkId) {
			switch renewal.Market {
			case SubscriptionMarketApple:
				storeSnapshot.AppleTransactionIds = append(storeSnapshot.AppleTransactionIds, renewal.TransactionId)
			case SubscriptionMarketGoogle:
				storeSnapshot.PlayPurchaseTokens = append(storeSnapshot.PlayPurchaseTokens, renewal.PurchaseToken)
			}
		}
	})
	return storeSnapshot
}

// Reads every App Store and Google Play supporter renewal whose window has not
// ended, including one queued to start later (as the Stripe check reads Stripe
// renewals). The snapshot and the check under the deletion lock read the same
// rows, so only a renewal credited in between tells them apart.
func removeNetworkStoreRenewals(
	ctx context.Context,
	query server.PgCanQuery,
	networkId server.Id,
) []*ActiveSubscriptionRenewal {
	renewals := []*ActiveSubscriptionRenewal{}
	result, err := query.Query(
		ctx,
		`
			SELECT
				market,
				COALESCE(transaction_id, ''),
				COALESCE(purchase_token, '')
			FROM subscription_renewal
			WHERE
				network_id = $1
				AND subscription_type = $2
				AND market IN ($3, $4)
				AND now() < end_time
			ORDER BY market, end_time DESC, start_time DESC
		`,
		networkId,
		SubscriptionTypeSupporter,
		SubscriptionMarketApple,
		SubscriptionMarketGoogle,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			renewal := &ActiveSubscriptionRenewal{}
			server.Raise(result.Scan(
				&renewal.Market,
				&renewal.TransactionId,
				&renewal.PurchaseToken,
			))
			renewals = append(renewals, renewal)
		}
	})
	return renewals
}

// RemoveNetworkWithStoreSnapshot with no store snapshot: it refuses while any
// Stripe, App Store or Google Play renewal is active.
func RemoveNetwork(
	ctx context.Context,
	networkId server.Id,
	adminUserId *server.Id,
) (success bool, userAuths map[string]bool) {
	outcome, userAuths := RemoveNetworkWithStoreSnapshot(ctx, networkId, adminUserId, nil)
	return outcome == RemoveNetworkRemoved, userAuths
}

// Removes the network and its users. storeSnapshot names the App Store and
// Google Play renewals the caller already checked with the stores; any other
// active renewal from those stores refuses the removal. Pass nil when no store
// steps ran.
func RemoveNetworkWithStoreSnapshot(
	ctx context.Context,
	networkId server.Id,
	adminUserId *server.Id,
	storeSnapshot *RemoveNetworkStoreSnapshot,
) (outcome RemoveNetworkOutcome, userAuths map[string]bool) {
	var posts []server.PostFunction
	server.Tx(ctx, func(tx server.PgTx) {
		// Tx may rerun the callback after a transient database failure. Never let
		// a value produced by an aborted attempt escape a later refusal.
		outcome = ""
		userAuths = nil
		posts = nil

		// Serialize deletion against every renewal credit. The matching credit
		// path takes FOR KEY SHARE before it consumes a provider idempotency
		// ledger or payment intent. ReadCommitted below is required: if this
		// waits behind a credit, the active-renewal query must see that credit's
		// commit rather than retain a pre-wait RepeatableRead snapshot.
		networkAdminUserId, networkFound := lockPaymentNetworkForRemoveInTx(tx, ctx, networkId)
		if !networkFound || (adminUserId != nil && *adminUserId != networkAdminUserId) {
			outcome = RemoveNetworkNotFound
			return
		}

		// Controller deletion cancels and closes Stripe renewals first. Keep
		// this invariant in the model too, so direct CLI/model callers fail
		// closed and a Stripe credit that won the row-lock race forces a retry.
		activeStripeRenewal := false
		result, err := tx.Query(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM subscription_renewal
					WHERE network_id = $1
						AND market = $2
						AND now() < end_time
				)
			`,
			networkId,
			SubscriptionMarketStripe,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&activeStripeRenewal))
			}
		})
		if activeStripeRenewal {
			outcome = RemoveNetworkStripeRenewalActive
			return
		}

		// The App Store and Google Play steps run before this lock, against the
		// snapshot. A renewal credited since then committed before the lock was
		// granted and is visible here; deleting now would leave it billing a
		// deleted account. The retry runs the store steps on it.
		for _, renewal := range removeNetworkStoreRenewals(ctx, tx, networkId) {
			if !storeSnapshot.Covers(renewal) {
				outcome = RemoveNetworkStoreRenewalUnchecked
				return
			}
		}

		userIds := map[server.Id]bool{networkAdminUserId: true}
		userAuths = map[string]bool{}

		server.CreateTempTableInTx(ctx, tx, "temp_user_id(user_id uuid)", slices.Collect(maps.Keys(userIds))...)

		result, err = tx.Query(
			ctx,
			`
			SELECT
				user_auth
			FROM network_user_auth_password
			INNER JOIN temp_user_id ON temp_user_id.user_id = network_user_auth_password.user_id

			UNION ALL

			SELECT
				user_auth
			FROM network_user_auth_sso
			INNER JOIN temp_user_id ON temp_user_id.user_id = network_user_auth_sso.user_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userAuth string
				server.Raise(result.Scan(&userAuth))
				userAuths[userAuth] = true
			}
		})

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for userId, _ := range userIds {
				batch.Queue(
					`
						DELETE FROM network_user
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_wallet
				batch.Queue(
					`
						DELETE FROM network_user_auth_wallet
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_password
				batch.Queue(
					`
						DELETE FROM network_user_auth_password
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_sso
				batch.Queue(
					`
						DELETE FROM network_user_auth_sso
						WHERE user_id = $1
					`,
					userId,
				)

				// (cascade) delete network_user_auth_seedphrase
				batch.Queue(
					`
						DELETE FROM network_user_auth_seedphrase
						WHERE user_id = $1
					`,
					userId,
				)
			}
		})

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network
				WHERE network_id = $1
			`,
			networkId,
		))

		// the network leaves this process's in-memory name index only once the
		// removal has committed
		posts = append(posts, networkNameSearch().RemoveInTxPost(ctx, networkId, tx))

		outcome = RemoveNetworkRemoved
	}, server.TxReadCommitted)

	if outcome == RemoveNetworkRemoved {
		server.RunPosts(ctx, posts...)
		// the networks stats series (ComputeStats) replays created/deleted
		// events; creation is recorded in network_model.go, and without this
		// the active-network count only ever grew
		auditNetworkEvent := NewAuditNetworkEvent(AuditEventTypeNetworkDeleted)
		auditNetworkEvent.NetworkId = networkId
		AddAuditEvent(ctx, auditNetworkEvent)
	}
	return
}

// lockPaymentNetworkForRemoveInTx serializes owner deletion against paid
// entitlement and data writers and returns the administrator under that lock.
func lockPaymentNetworkForRemoveInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
) (networkAdminUserId server.Id, found bool) {
	result, err := tx.Query(
		ctx,
		`
			/* payment-network-delete-lock */
			SELECT admin_user_id
			FROM network
			WHERE network_id = $1
			FOR UPDATE
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&networkAdminUserId))
			found = true
		}
	})
	return
}
