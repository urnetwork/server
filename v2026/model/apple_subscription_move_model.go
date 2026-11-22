package model

// App Store subscriptions moved from a deleted account (owner 2026-11-17,
// "Restore moves it."; support inbox 2489). An App Store purchase names the
// network it was bought for in its appAccountToken, for the life of the
// subscription. When that account is deleted, Apple keeps billing a customer
// who renews or re-enables the subscription, its renewals name a network that
// is gone, and a restore from the account the customer signs in to now was
// refused as another account's purchase. A verified restore of an active
// subscription whose account is deleted (deleted_network) now moves it to the
// restoring account (controller/apple_subscription_move_controller.go): this
// table records the move, keyed by the subscription's original transaction
// id, and the notification webhook and the reconciler resolve a renewal that
// names the deleted network to the account it moved to. A move is never made
// away from a network that exists. The row is the move's audit record, with a
// subscription_moved event beside it, and it is kept when either account is
// deleted.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// The network an App Store subscription was moved to, if it was moved.
func GetAppleSubscriptionMoveInTx(
	tx server.PgTx,
	ctx context.Context,
	originalTransactionId string,
) (toNetworkId server.Id, ok bool) {
	err := tx.QueryRow(
		ctx,
		`
			SELECT to_network_id
			FROM apple_subscription_move
			WHERE original_transaction_id = $1
		`,
		originalTransactionId,
	).Scan(&toNetworkId)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	server.Raise(err)
	ok = true
	return
}

// The network a renewal of the subscription is credited to when its
// appAccountToken names tokenNetworkId: the network the subscription moved
// to, while tokenNetworkId no longer exists. ok is false when the
// subscription never moved, or its token's network exists (a token that
// names a live network is that network's purchase, whatever moved before).
func GetAppleSubscriptionMoveTargetInConn(
	connOwner server.PgConn,
	ctx context.Context,
	originalTransactionId string,
	tokenNetworkId server.Id,
) (toNetworkId server.Id, ok bool) {
	if originalTransactionId == "" {
		return
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT move.to_network_id
				FROM apple_subscription_move move
				WHERE move.original_transaction_id = $1
				  AND NOT EXISTS (
					SELECT 1
					FROM network
					WHERE network.network_id = $2
				  )
			`,
			originalTransactionId,
			tokenNetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&toNetworkId))
				ok = true
			}
		})
	})
	return
}

// Records the subscription's move from fromNetworkId to toNetworkId, made by
// the restore of transactionId. With replaceToNetworkId nil the subscription
// must not have moved before; otherwise it must still be held by
// replaceToNetworkId (a network the subscription moved to that was deleted
// too). moved is false when another move got there first: the caller reads
// the move again.
func MoveAppleSubscriptionInTx(
	tx server.PgTx,
	ctx context.Context,
	originalTransactionId string,
	transactionId string,
	fromNetworkId server.Id,
	toNetworkId server.Id,
	replaceToNetworkId *server.Id,
	moveTime time.Time,
) (moved bool) {
	if replaceToNetworkId == nil {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO apple_subscription_move (
					original_transaction_id,
					from_network_id,
					to_network_id,
					transaction_id,
					move_time
				)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (original_transaction_id) DO NOTHING
			`,
			originalTransactionId,
			fromNetworkId,
			toNetworkId,
			transactionId,
			moveTime.UTC(),
		))
		return tag.RowsAffected() == 1
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE apple_subscription_move
			SET
				from_network_id = $2,
				to_network_id = $3,
				transaction_id = $4,
				move_time = $5
			WHERE original_transaction_id = $1
			  AND to_network_id = $6
		`,
		originalTransactionId,
		fromNetworkId,
		toNetworkId,
		transactionId,
		moveTime.UTC(),
		*replaceToNetworkId,
	))
	return tag.RowsAffected() == 1
}

// The network the apple_subscription_transaction ledger credited the
// transaction to, read in the caller's transaction.
func GetAppleTransactionNetworkIdInTx(
	tx server.PgTx,
	ctx context.Context,
	transactionId string,
) (networkId server.Id, ok bool) {
	err := tx.QueryRow(
		ctx,
		`
			SELECT network_id
			FROM apple_subscription_transaction
			WHERE transaction_id = $1
		`,
		transactionId,
	).Scan(&networkId)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	server.Raise(err)
	ok = true
	return
}

// Gives toNetworkId the rest of a paid App Store window that the ledger
// credited to a network that is deleted: the transaction's windows still open
// on any network end now (endReconciledEntitlementInTx, with the Pro cycle
// they carried), and toNetworkId gets the transaction's renewal from now to
// endTime (paidEndTime and its grace), a zero-byte Pro marker over the same
// window and the Pro grant of its cycle. Nothing is paid twice: the renewal
// and the marker record no revenue, which the original credit recorded, and
// the ledger row stays as the history of that credit. The caller has checked
// that toNetworkId exists and holds no window of the transaction, and
// refreshes the Pro cache after the commit. The tx must be ReadCommitted (the
// cycle grant) and use OptNoRetry (an ownership refusal aborts it).
// rehomed is false when the window is already over.
func RehomeAppleTransactionEntitlementInTx(
	tx server.PgTx,
	ctx context.Context,
	toNetworkId server.Id,
	transactionId string,
	paidEndTime time.Time,
	endTime time.Time,
	now time.Time,
) (rehomed bool) {
	if !now.Before(endTime) {
		return false
	}
	endReconciledEntitlementInTx(tx, ctx, nil, SubscriptionMarketApple, []string{transactionId}, "", now)
	paidEnd := paidEndTime.UTC()
	server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
		NetworkId:          toNetworkId,
		SubscriptionType:   SubscriptionTypeSupporter,
		StartTime:          now.UTC(),
		EndTime:            endTime.UTC(),
		NetRevenue:         0,
		SubscriptionMarket: SubscriptionMarketApple,
		TransactionId:      transactionId,
		PaidEndTime:        &paidEnd,
	}))
	AddTransferBalanceInTx(ctx, tx, &TransferBalance{
		NetworkId:             toNetworkId,
		StartTime:             now.UTC(),
		EndTime:               endTime.UTC(),
		StartBalanceByteCount: 0,
		NetRevenue:            0,
		SubsidyNetRevenue:     0,
		BalanceByteCount:      0,
		Pro:                   true,
	})
	GrantProCycleInTx(tx, ctx, toNetworkId)
	return true
}
