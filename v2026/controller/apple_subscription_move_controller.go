package controller

// An App Store subscription whose account is deleted moves to the account its
// owner restores it from (owner 2026-11-17: "Restore moves it."; support inbox
// 2489). A purchase names the network it was bought for in its
// appAccountToken for the life of the subscription. A customer who deletes the
// account and signs up again kept paying Apple for a subscription nothing
// could credit: its renewals named a network that is gone (destination_deleted)
// and a restore from the new account answered wrong_network. Now the verify
// endpoint's restore, with Apple's verified transaction, moves it:
//
//   - only when the account the token names is deleted: its network row is
//     gone and the account has a deletion record (deleted_network). A network
//     that is merely not found was never ours to give away, and a live
//     account's purchase stays its own (wrong_network, as before);
//   - only an active purchase of the customer's own: its paid period is
//     running, Apple has not refunded or revoked it, and it is not a family
//     member's shared access;
//   - once: the first restore records the move (apple_subscription_move, keyed
//     by the original transaction id), and a concurrent restore from another
//     account finds it and stays refused. A subscription moved to an account
//     that is deleted in turn moves again the same way;
//   - with what was paid: a transaction the ledger never credited is credited
//     to the restoring account through the ledger gate; a paid window the
//     deleted account was credited moves with its rest (the deleted account's
//     window ends, the restoring account's starts, with no second revenue).
//
// The move, the credit and its subscription_moved audit event commit together
// or not at all. From the move on, a renewal or a revocation whose token names
// the deleted account resolves to the account it moved to, in the
// notification webhook and the reconciler (validateAppleTransactionBound), and
// the Pro cycle grants follow that account's renewals. Nothing of the deleted
// account comes back: its rows stay as history.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The transactional state the move rules read and write. The production
// implementation is one ReadCommitted db transaction; tests use a fake.
type appleSubscriptionMoveTx interface {
	// NetworkExists locks the network row for the rest of the tx
	NetworkExists(networkId server.Id) bool
	// NetworkDeleted reports whether the network has a deletion record
	NetworkDeleted(networkId server.Id) bool
	// MovedTo is the network the subscription moved to, if it moved
	MovedTo(originalTransactionId string) (server.Id, bool)
	// Move records the subscription's move from fromNetworkId to toNetworkId.
	// replaceToNetworkId is the deleted network a second move takes it from,
	// nil for a first move. false when another move got there first.
	Move(transaction *validatedAppleTransaction, fromNetworkId server.Id, toNetworkId server.Id, replaceToNetworkId *server.Id) bool
	// CreditedNetworkId is the network the ledger credited the transaction to
	CreditedNetworkId(transactionId string) (server.Id, bool)
	// Credit is appleCreditSubscriptionTransactionInTx for the transaction's
	// networkId
	Credit(transaction *validatedAppleTransaction) bool
	// Rehome gives toNetworkId the rest of the transaction's paid window,
	// which the ledger credited to a deleted network; false when it is over
	Rehome(transaction *validatedAppleTransaction, toNetworkId server.Id, now time.Time) bool
	// RecordMove writes the move's subscription_moved event
	RecordMove(transaction *validatedAppleTransaction, fromNetworkId server.Id, toNetworkId server.Id, rehomed bool)
}

type appleSubscriptionMoveResult struct {
	// a VerifyPurchaseStatus
	status string
	// why a report was refused, or what the move did (logged)
	reason string
	// this report moved the subscription
	moved bool
	// commit is false when the tx must roll back (nothing may persist)
	commit bool
	err    error
}

func appleSubscriptionMoveRefused(reason string) appleSubscriptionMoveResult {
	return appleSubscriptionMoveResult{status: VerifyPurchaseStatusWrongNetwork, reason: reason}
}

// appleMoveDeletedOwnerSubscription applies the move rules to a verified
// transaction whose appAccountToken names another network than
// sessionNetworkId, the network that reported it. On a move or a credit the
// transaction's networkId is the session network.
func appleMoveDeletedOwnerSubscription(
	store appleSubscriptionMoveTx,
	transaction *validatedAppleTransaction,
	sessionNetworkId server.Id,
	now time.Time,
) appleSubscriptionMoveResult {
	if transaction.unboundAccountToken || transaction.originalTransactionId == "" {
		return appleSubscriptionMoveRefused("no subscription to move")
	}
	if !store.NetworkExists(sessionNetworkId) {
		return appleSubscriptionMoveResult{err: errors.New("session network does not exist")}
	}
	tokenNetworkId := transaction.networkId
	if store.NetworkExists(tokenNetworkId) {
		// a live account's purchase stays its own
		return appleSubscriptionMoveRefused("the purchase's account exists")
	}

	// the subscription is the session network's: the ledger decides credited
	// or already credited
	creditHere := func(reason string) appleSubscriptionMoveResult {
		transaction.networkId = sessionNetworkId
		if _, credited := store.CreditedNetworkId(transaction.transactionId); credited {
			return appleSubscriptionMoveResult{status: VerifyPurchaseStatusAlreadyCredited, reason: reason, commit: true}
		}
		if store.Credit(transaction) {
			return appleSubscriptionMoveResult{status: VerifyPurchaseStatusCredited, reason: reason, commit: true}
		}
		return appleSubscriptionMoveResult{status: VerifyPurchaseStatusAlreadyCredited, reason: reason, commit: true}
	}

	fromNetworkId := tokenNetworkId
	var replaceToNetworkId *server.Id
	if movedTo, ok := store.MovedTo(transaction.originalTransactionId); ok {
		if movedTo == sessionNetworkId {
			return creditHere("the subscription moved to this account before")
		}
		// it moved to another account, and moves again only when that
		// account is deleted too
		if store.NetworkExists(movedTo) || !store.NetworkDeleted(movedTo) {
			return appleSubscriptionMoveRefused("the subscription moved to another account")
		}
		fromNetworkId = movedTo
		replaceToNetworkId = &movedTo
	} else if !store.NetworkDeleted(tokenNetworkId) {
		// not found is not deleted: never move a purchase on a guess
		return appleSubscriptionMoveRefused("the purchase's account has no deletion record")
	}

	if transaction.revoked {
		return appleSubscriptionMoveRefused("Apple refunded or revoked the purchase")
	}
	if transaction.familyShared {
		return appleSubscriptionMoveRefused("a family member's shared access is not the account's purchase")
	}
	if !now.Before(transaction.expiresTime) {
		return appleSubscriptionMoveRefused("the purchase's paid period is over")
	}

	if !store.Move(transaction, fromNetworkId, sessionNetworkId, replaceToNetworkId) {
		// another restore moved it first: read its move again
		if movedTo, ok := store.MovedTo(transaction.originalTransactionId); ok && movedTo == sessionNetworkId {
			return creditHere("a concurrent restore from this account moved the subscription")
		}
		return appleSubscriptionMoveRefused("another account restored the subscription first")
	}

	transaction.networkId = sessionNetworkId
	status := VerifyPurchaseStatusCredited
	rehomed := false
	creditedNetworkId, credited := store.CreditedNetworkId(transaction.transactionId)
	switch {
	case !credited:
		if !store.Credit(transaction) {
			return appleSubscriptionMoveRefused("the transaction was credited while it moved")
		}
	case creditedNetworkId == sessionNetworkId:
		status = VerifyPurchaseStatusAlreadyCredited
	case store.NetworkExists(creditedNetworkId):
		// credited to a live account: never take it from there
		return appleSubscriptionMoveRefused("the transaction is credited to another account")
	default:
		// the deleted account was credited this paid window: its rest moves
		rehomed = store.Rehome(transaction, sessionNetworkId, now)
		if !rehomed {
			status = VerifyPurchaseStatusAlreadyCredited
		}
	}
	store.RecordMove(transaction, fromNetworkId, sessionNetworkId, rehomed)
	return appleSubscriptionMoveResult{
		status: status,
		reason: "moved from a deleted account",
		moved:  true,
		commit: true,
	}
}

// ----- db implementation -----

type appleSubscriptionMoveDbTx struct {
	tx  server.PgTx
	ctx context.Context
}

func (self *appleSubscriptionMoveDbTx) NetworkExists(networkId server.Id) bool {
	return appleNetworkExistsInTx(self.tx, self.ctx, networkId)
}

func (self *appleSubscriptionMoveDbTx) NetworkDeleted(networkId server.Id) bool {
	return model.IsNetworkDeletedInTx(self.tx, self.ctx, networkId)
}

func (self *appleSubscriptionMoveDbTx) MovedTo(originalTransactionId string) (server.Id, bool) {
	return model.GetAppleSubscriptionMoveInTx(self.tx, self.ctx, originalTransactionId)
}

func (self *appleSubscriptionMoveDbTx) Move(
	transaction *validatedAppleTransaction,
	fromNetworkId server.Id,
	toNetworkId server.Id,
	replaceToNetworkId *server.Id,
) bool {
	return model.MoveAppleSubscriptionInTx(
		self.tx,
		self.ctx,
		transaction.originalTransactionId,
		transaction.transactionId,
		fromNetworkId,
		toNetworkId,
		replaceToNetworkId,
		server.NowUtc(),
	)
}

func (self *appleSubscriptionMoveDbTx) CreditedNetworkId(transactionId string) (server.Id, bool) {
	return model.GetAppleTransactionNetworkIdInTx(self.tx, self.ctx, transactionId)
}

func (self *appleSubscriptionMoveDbTx) Credit(transaction *validatedAppleTransaction) bool {
	// the ledger row's notification_uuid is only a provenance pointer; a
	// client report has no notification
	return appleCreditSubscriptionTransactionInTx(self.tx, self.ctx, server.NewId(), transaction)
}

func (self *appleSubscriptionMoveDbTx) Rehome(transaction *validatedAppleTransaction, toNetworkId server.Id, now time.Time) bool {
	return model.RehomeAppleTransactionEntitlementInTx(
		self.tx,
		self.ctx,
		toNetworkId,
		transaction.transactionId,
		transaction.expiresTime,
		transaction.expiresTime.Add(SubscriptionGracePeriod),
		now,
	)
}

func (self *appleSubscriptionMoveDbTx) RecordMove(
	transaction *validatedAppleTransaction,
	fromNetworkId server.Id,
	toNetworkId server.Id,
	rehomed bool,
) {
	server.Raise(model.AddPaymentReconciliationEventInTx(self.tx, self.ctx, &model.PaymentReconciliationEvent{
		RunId:     server.NewId(),
		Store:     model.SubscriptionMarketApple,
		NetworkId: &toNetworkId,
		Action:    model.PaymentReconcileActionSubscriptionMoved,
		Evidence:  transaction.originalTransactionId,
		Details: map[string]any{
			"from_network": fromNetworkId.String(),
			"transaction":  transaction.transactionId,
			"rehomed":      rehomed,
			"leg":          "restore",
		},
	}))
}

type appleSubscriptionMoveRollback struct{}

// appleSubscriptionMoveRunFunc runs the rules in one ReadCommitted
// transaction (the cycle grant reads the chain after its lock), rolled back
// unless the result says commit. OptNoRetry: a balance ownership refusal in
// the window's move aborts the transaction, and the client reports again.
// Tests replace it with a fake store.
var appleSubscriptionMoveRunFunc = func(
	ctx context.Context,
	callback func(store appleSubscriptionMoveTx) appleSubscriptionMoveResult,
) (result appleSubscriptionMoveResult) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(appleSubscriptionMoveRollback); ok {
				return
			}
			panic(r)
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		result = callback(&appleSubscriptionMoveDbTx{tx: tx, ctx: ctx})
		if !result.commit {
			// a panic is the tx helper's rollback path
			panic(appleSubscriptionMoveRollback{})
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

var appleSubscriptionMoveProRefreshFunc = func(ctx context.Context, networkId server.Id) {
	model.UpdateProNetwork(ctx, networkId)
}

// verifyAppleTransactionForAnotherNetwork is the verify endpoint's answer to a
// transaction whose appAccountToken names another network than the session's:
// the move rules, wrong_network when they refuse.
func verifyAppleTransactionForAnotherNetwork(
	ctx context.Context,
	transaction *validatedAppleTransaction,
	sessionNetworkId server.Id,
) (*VerifyStorePurchaseResult, error) {
	tokenNetworkId := transaction.networkId
	now := appleVerifyNowFunc()
	result := appleSubscriptionMoveRunFunc(ctx, func(store appleSubscriptionMoveTx) appleSubscriptionMoveResult {
		return appleMoveDeletedOwnerSubscription(store, transaction, sessionNetworkId, now)
	})
	if result.err != nil {
		return nil, result.err
	}
	if !result.commit || result.status == VerifyPurchaseStatusWrongNetwork || result.status == "" {
		glog.Infof(
			"[sub]verify apple transaction %s (original %s): appAccountToken names network %s, session is %s: %s\n",
			transaction.transactionId,
			transaction.originalTransactionId,
			tokenNetworkId,
			sessionNetworkId,
			result.reason,
		)
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), nil
	}
	glog.Infof(
		"[sub]verify apple transaction %s (original %s) of deleted network %s for network %s: %s (%s)\n",
		transaction.transactionId,
		transaction.originalTransactionId,
		tokenNetworkId,
		sessionNetworkId,
		result.status,
		result.reason,
	)
	if result.status == VerifyPurchaseStatusCredited || result.moved {
		appleSubscriptionMoveProRefreshFunc(ctx, sessionNetworkId)
	}
	expiryTime := transaction.expiresTime
	return &VerifyStorePurchaseResult{Status: result.status, ExpiryTime: &expiryTime}, nil
}

// appleSubscriptionMoveLookupFunc resolves a subscription whose
// appAccountToken names a deleted network that it moved from. Tests replace it.
var appleSubscriptionMoveLookupFunc = func(
	connOwner server.PgConn,
	ctx context.Context,
	originalTransactionId string,
	tokenNetworkId server.Id,
) (server.Id, bool) {
	return model.GetAppleSubscriptionMoveTargetInConn(connOwner, ctx, originalTransactionId, tokenNetworkId)
}

// resolveAppleSubscriptionMove points a transaction whose appAccountToken
// names a deleted network at the network its subscription moved to, for the
// paths that credit and end windows without a session (the notification
// webhook and the reconciler, through validateAppleTransactionBound).
func resolveAppleSubscriptionMove(
	connOwner server.PgConn,
	ctx context.Context,
	transaction *validatedAppleTransaction,
) {
	if transaction.unboundAccountToken || transaction.originalTransactionId == "" {
		return
	}
	toNetworkId, ok := appleSubscriptionMoveLookupFunc(connOwner, ctx, transaction.originalTransactionId, transaction.networkId)
	if !ok || toNetworkId == transaction.networkId {
		return
	}
	fromNetworkId := transaction.networkId
	transaction.movedFromNetworkId = &fromNetworkId
	transaction.networkId = toNetworkId
}
