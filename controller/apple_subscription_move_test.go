package controller

// Root-cause tests for the App Store subscriptions of deleted accounts
// (support inbox 2489; owner 2026-11-17, "Restore moves it."). A customer who
// deleted the account a subscription was bought for kept paying Apple, its
// renewals named a network that is gone, and a restore from the account the
// customer signed in to now answered wrong_network. They drive
// VerifyAppleTransactionClaims, the exact input the api/handlers pinned-root
// verifier hands it, and validateAppleTransactionBound, which the
// notification webhook and the reconciler run, against a fake transactional
// store that reads at ReadCommitted (no db, fixed clock), so they run without
// the test database. apple_subscription_move_db_test.go runs the same paths
// against the database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const moveTestProductId = "supporter_yearly_26"

// moveTestNow is fixed; the claims' dates are relative to it.
var moveTestNow = time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC)

type moveFakeEvent struct {
	originalTransactionId string
	fromNetworkId         server.Id
	toNetworkId           server.Id
	rehomed               bool
}

// The committed state of the fake database.
type moveFakeState struct {
	networks map[server.Id]bool
	deleted  map[server.Id]bool
	moves    map[string]server.Id
	// the credit ledger: transaction id -> network
	ledger map[string]server.Id
	// open Pro windows: network -> transaction ids
	windows map[server.Id]map[string]bool
	events  []moveFakeEvent
}

type moveFakeEnv struct {
	state     *moveFakeState
	refreshed []server.Id
}

// One fake transaction. Its reads see its own writes, then whatever is
// committed when they run (ReadCommitted, as the move's tx), and it publishes
// its writes only on commit, like a db rollback otherwise.
type moveFakeStore struct {
	env     *moveFakeEnv
	moves   map[string]server.Id
	ledger  map[string]server.Id
	opened  map[server.Id]map[string]bool
	ended   map[string]bool
	events  []moveFakeEvent
	rehomes int
	// staleMovedTo answers the first MovedTo from before a concurrent
	// restore's move committed, as a read that ran before that commit
	staleMovedTo bool
}

func (self *moveFakeEnv) newStore() *moveFakeStore {
	return &moveFakeStore{
		env:    self,
		moves:  map[string]server.Id{},
		ledger: map[string]server.Id{},
		opened: map[server.Id]map[string]bool{},
		ended:  map[string]bool{},
	}
}

func (self *moveFakeEnv) commit(store *moveFakeStore) {
	for originalTransactionId, networkId := range store.moves {
		self.state.moves[originalTransactionId] = networkId
	}
	for transactionId, networkId := range store.ledger {
		self.state.ledger[transactionId] = networkId
	}
	for transactionId := range store.ended {
		for _, transactionIds := range self.state.windows {
			delete(transactionIds, transactionId)
		}
	}
	for networkId, transactionIds := range store.opened {
		if self.state.windows[networkId] == nil {
			self.state.windows[networkId] = map[string]bool{}
		}
		for transactionId := range transactionIds {
			self.state.windows[networkId][transactionId] = true
		}
	}
	self.state.events = append(self.state.events, store.events...)
}

func (self *moveFakeStore) NetworkExists(networkId server.Id) bool {
	return self.env.state.networks[networkId]
}

func (self *moveFakeStore) NetworkDeleted(networkId server.Id) bool {
	return self.env.state.deleted[networkId]
}

func (self *moveFakeStore) MovedTo(originalTransactionId string) (server.Id, bool) {
	if networkId, ok := self.moves[originalTransactionId]; ok {
		return networkId, true
	}
	if self.staleMovedTo {
		self.staleMovedTo = false
		return server.Id{}, false
	}
	networkId, ok := self.env.state.moves[originalTransactionId]
	return networkId, ok
}

// the primary key sees the committed row of a concurrent move
func (self *moveFakeStore) Move(transaction *validatedAppleTransaction, fromNetworkId server.Id, toNetworkId server.Id, replaceToNetworkId *server.Id) bool {
	current, exists := self.env.state.moves[transaction.originalTransactionId]
	if own, ok := self.moves[transaction.originalTransactionId]; ok {
		current, exists = own, true
	}
	if replaceToNetworkId == nil {
		if exists {
			return false
		}
	} else if !exists || current != *replaceToNetworkId {
		return false
	}
	self.moves[transaction.originalTransactionId] = toNetworkId
	return true
}

func (self *moveFakeStore) CreditedNetworkId(transactionId string) (server.Id, bool) {
	if networkId, ok := self.ledger[transactionId]; ok {
		return networkId, true
	}
	networkId, ok := self.env.state.ledger[transactionId]
	return networkId, ok
}

func (self *moveFakeStore) open(networkId server.Id, transactionId string) {
	if self.opened[networkId] == nil {
		self.opened[networkId] = map[string]bool{}
	}
	self.opened[networkId][transactionId] = true
}

func (self *moveFakeStore) Credit(transaction *validatedAppleTransaction) bool {
	if _, credited := self.CreditedNetworkId(transaction.transactionId); credited {
		return false
	}
	self.ledger[transaction.transactionId] = transaction.networkId
	self.open(transaction.networkId, transaction.transactionId)
	return true
}

func (self *moveFakeStore) Rehome(transaction *validatedAppleTransaction, toNetworkId server.Id, now time.Time) bool {
	if !now.Before(transaction.expiresTime.Add(SubscriptionGracePeriod)) {
		return false
	}
	self.ended[transaction.transactionId] = true
	self.open(toNetworkId, transaction.transactionId)
	self.rehomes += 1
	return true
}

func (self *moveFakeStore) RecordMove(transaction *validatedAppleTransaction, fromNetworkId server.Id, toNetworkId server.Id, rehomed bool) {
	self.events = append(self.events, moveFakeEvent{
		originalTransactionId: transaction.originalTransactionId,
		fromNetworkId:         fromNetworkId,
		toNetworkId:           toNetworkId,
		rehomed:               rehomed,
	})
}

// installMoveFakes swaps the move seams for the fake store and restores them
// when the test ends.
func installMoveFakes(t *testing.T) *moveFakeEnv {
	env := &moveFakeEnv{
		state: &moveFakeState{
			networks: map[server.Id]bool{},
			deleted:  map[server.Id]bool{},
			moves:    map[string]server.Id{},
			ledger:   map[string]server.Id{},
			windows:  map[server.Id]map[string]bool{},
		},
	}
	previousRun := appleSubscriptionMoveRunFunc
	previousRefresh := appleSubscriptionMoveProRefreshFunc
	previousLookup := appleSubscriptionMoveLookupFunc
	previousNow := appleVerifyNowFunc
	t.Cleanup(func() {
		appleSubscriptionMoveRunFunc = previousRun
		appleSubscriptionMoveProRefreshFunc = previousRefresh
		appleSubscriptionMoveLookupFunc = previousLookup
		appleVerifyNowFunc = previousNow
	})
	appleSubscriptionMoveRunFunc = func(
		ctx context.Context,
		callback func(store appleSubscriptionMoveTx) appleSubscriptionMoveResult,
	) appleSubscriptionMoveResult {
		store := env.newStore()
		result := callback(store)
		if result.commit {
			env.commit(store)
		}
		return result
	}
	appleSubscriptionMoveProRefreshFunc = func(ctx context.Context, networkId server.Id) {
		env.refreshed = append(env.refreshed, networkId)
	}
	// model.GetAppleSubscriptionMoveTargetInConn: the move, while the
	// token's network is gone
	appleSubscriptionMoveLookupFunc = func(connOwner server.PgConn, ctx context.Context, originalTransactionId string, tokenNetworkId server.Id) (server.Id, bool) {
		if env.state.networks[tokenNetworkId] {
			return server.Id{}, false
		}
		networkId, ok := env.state.moves[originalTransactionId]
		return networkId, ok
	}
	appleVerifyNowFunc = func() time.Time {
		return moveTestNow
	}
	return env
}

func (self *moveFakeEnv) addNetwork() (*session.ClientSession, server.Id) {
	networkId := server.NewId()
	self.state.networks[networkId] = true
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{
		NetworkId: networkId,
		UserId:    server.NewId(),
	})
	return clientSession, networkId
}

// deleteNetwork removes the network as an account deletion does: its row is
// gone and its deletion is recorded
func (self *moveFakeEnv) deleteNetwork(networkId server.Id) {
	delete(self.state.networks, networkId)
	self.state.deleted[networkId] = true
}

// creditToNetwork is a credit of the transaction's paid window made before
// the account was deleted
func (self *moveFakeEnv) creditToNetwork(networkId server.Id, transactionId string) {
	self.state.ledger[transactionId] = networkId
	if self.state.windows[networkId] == nil {
		self.state.windows[networkId] = map[string]bool{}
	}
	self.state.windows[networkId][transactionId] = true
}

// moveTestClaims is a yearly subscription's transaction as StoreKit signs it:
// bought for tokenNetworkId, purchasedAgo before now, paid for a year.
func moveTestClaims(tokenNetworkId server.Id, originalTransactionId string, transactionId string, purchasedAgo time.Duration) map[string]any {
	purchase := moveTestNow.Add(-purchasedAgo)
	return map[string]any{
		"appAccountToken":       tokenNetworkId.String(),
		"transactionId":         transactionId,
		"originalTransactionId": originalTransactionId,
		"productId":             moveTestProductId,
		"purchaseDate":          float64(purchase.UnixMilli()),
		"expiresDate":           float64(purchase.Add(365 * 24 * time.Hour).UnixMilli()),
		"price":                 float64(39990),
		"currency":              "USD",
	}
}

func verifyMove(t *testing.T, claims map[string]any, clientSession *session.ClientSession) string {
	t.Helper()
	result, err := VerifyAppleTransactionClaims(claims, []string{moveTestProductId}, clientSession)
	connect.AssertEqual(t, err, nil)
	return result.Status
}

func moveTestRenewal(claims map[string]any) AppleNotificationDecodedPayload {
	return AppleNotificationDecodedPayload{
		NotificationType: "DID_RENEW",
		SignedDate:       moveTestNow.UnixMilli(),
		TransactionInfo:  claims,
	}
}

// The 2489 defect: the account the subscription was bought for is deleted,
// Apple renewed it, and the owner restores it from their new account. The
// restore must credit the new account and move the subscription there, not
// answer wrong_network (which left them paying Apple for nothing).
func TestAppleRestoreMovesSubscriptionOfDeletedAccount(t *testing.T) {
	env := installMoveFakes(t)
	_, deletedNetworkId := env.addNetwork()
	env.deleteNetwork(deletedNetworkId)
	clientSession, networkId := env.addNetwork()

	// the renewal Apple charged after the deletion, which no network could
	// be credited for
	claims := moveTestClaims(deletedNetworkId, "orig-1", "tx-2", 24*time.Hour)
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusCredited)

	connect.AssertEqual(t, env.state.ledger["tx-2"], networkId)
	connect.AssertEqual(t, env.state.windows[networkId]["tx-2"], true)
	connect.AssertEqual(t, env.state.moves["orig-1"], networkId)
	connect.AssertEqual(t, env.state.events, []moveFakeEvent{{
		originalTransactionId: "orig-1",
		fromNetworkId:         deletedNetworkId,
		toNetworkId:           networkId,
		rehomed:               false,
	}})
	connect.AssertEqual(t, env.refreshed, []server.Id{networkId})
	// nothing of the deleted account comes back
	connect.AssertEqual(t, env.state.networks[deletedNetworkId], false)
	connect.AssertEqual(t, len(env.state.windows[deletedNetworkId]), 0)
}

// The account was deleted inside a paid period the ledger credited to it: the
// rest of that paid window moves to the restoring account, once, and the
// deleted account's window ends.
func TestAppleRestoreMovesDeletedAccountsPaidWindow(t *testing.T) {
	env := installMoveFakes(t)
	_, deletedNetworkId := env.addNetwork()
	env.creditToNetwork(deletedNetworkId, "tx-1")
	env.deleteNetwork(deletedNetworkId)
	clientSession, networkId := env.addNetwork()

	claims := moveTestClaims(deletedNetworkId, "orig-1", "tx-1", 30*24*time.Hour)
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusCredited)

	connect.AssertEqual(t, env.state.windows[networkId]["tx-1"], true)
	connect.AssertEqual(t, env.state.windows[deletedNetworkId]["tx-1"], false)
	// the ledger keeps the history of the original credit
	connect.AssertEqual(t, env.state.ledger["tx-1"], deletedNetworkId)
	connect.AssertEqual(t, env.state.moves["orig-1"], networkId)
	connect.AssertEqual(t, len(env.state.events), 1)
	connect.AssertEqual(t, env.state.events[0].rehomed, true)

	// the client reports the same proof again: nothing moves or opens twice
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusAlreadyCredited)
	connect.AssertEqual(t, len(env.state.events), 1)
	connect.AssertEqual(t, len(env.state.windows[networkId]), 1)
}

// A live account's purchase stays its own, and a network that is merely not
// found (no deletion record) is never treated as deleted: wrong_network, as
// before, with nothing written.
func TestAppleRestoreOfLiveOrUndeletedAccountStaysRefused(t *testing.T) {
	env := installMoveFakes(t)
	_, liveNetworkId := env.addNetwork()
	clientSession, networkId := env.addNetwork()

	claims := moveTestClaims(liveNetworkId, "orig-live", "tx-live", 24*time.Hour)
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusWrongNetwork)

	// a token naming a network that is gone without a deletion record
	unknownNetworkId := server.NewId()
	claims = moveTestClaims(unknownNetworkId, "orig-unknown", "tx-unknown", 24*time.Hour)
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusWrongNetwork)

	connect.AssertEqual(t, len(env.state.moves), 0)
	connect.AssertEqual(t, len(env.state.ledger), 0)
	connect.AssertEqual(t, len(env.state.events), 0)
	connect.AssertEqual(t, len(env.state.windows[networkId]), 0)
	connect.AssertEqual(t, len(env.refreshed), 0)
}

// Only an active purchase of the customer's own moves: not one whose paid
// period is over, one Apple refunded or revoked, or a family member's shared
// access.
func TestAppleRestoreMoveRefusesEndedRevokedOrSharedPurchases(t *testing.T) {
	env := installMoveFakes(t)
	_, deletedNetworkId := env.addNetwork()
	env.deleteNetwork(deletedNetworkId)
	clientSession, _ := env.addNetwork()

	ended := moveTestClaims(deletedNetworkId, "orig-ended", "tx-ended", 400*24*time.Hour)
	connect.AssertEqual(t, verifyMove(t, ended, clientSession), VerifyPurchaseStatusWrongNetwork)

	revoked := moveTestClaims(deletedNetworkId, "orig-revoked", "tx-revoked", 24*time.Hour)
	revoked["revocationDate"] = float64(moveTestNow.Add(-time.Hour).UnixMilli())
	connect.AssertEqual(t, verifyMove(t, revoked, clientSession), VerifyPurchaseStatusWrongNetwork)

	shared := moveTestClaims(deletedNetworkId, "orig-shared", "tx-shared", 24*time.Hour)
	shared["inAppOwnershipType"] = "FAMILY_SHARED"
	connect.AssertEqual(t, verifyMove(t, shared, clientSession), VerifyPurchaseStatusWrongNetwork)

	connect.AssertEqual(t, len(env.state.moves), 0)
	connect.AssertEqual(t, len(env.state.ledger), 0)
	connect.AssertEqual(t, len(env.state.events), 0)
}

// The first restore wins. A repeated report from the same account credits
// nothing twice; a report from another account, before or after, stays
// refused, including one whose read ran before the first move committed.
func TestAppleRestoreMoveIsIdempotentAndRaceSafe(t *testing.T) {
	env := installMoveFakes(t)
	_, deletedNetworkId := env.addNetwork()
	env.deleteNetwork(deletedNetworkId)
	clientSession, networkId := env.addNetwork()
	otherSession, otherNetworkId := env.addNetwork()

	claims := moveTestClaims(deletedNetworkId, "orig-1", "tx-2", 24*time.Hour)
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusAlreadyCredited)
	connect.AssertEqual(t, verifyMove(t, claims, otherSession), VerifyPurchaseStatusWrongNetwork)

	// two restores at once: the other account's read ran before the first
	// move committed, and its insert meets the committed move
	previousRun := appleSubscriptionMoveRunFunc
	appleSubscriptionMoveRunFunc = func(
		ctx context.Context,
		callback func(store appleSubscriptionMoveTx) appleSubscriptionMoveResult,
	) appleSubscriptionMoveResult {
		store := env.newStore()
		store.staleMovedTo = true
		result := callback(store)
		if result.commit {
			env.commit(store)
		}
		return result
	}
	connect.AssertEqual(t, verifyMove(t, claims, otherSession), VerifyPurchaseStatusWrongNetwork)
	// the same race from the account that won credits nothing twice
	connect.AssertEqual(t, verifyMove(t, claims, clientSession), VerifyPurchaseStatusAlreadyCredited)
	appleSubscriptionMoveRunFunc = previousRun

	connect.AssertEqual(t, env.state.moves["orig-1"], networkId)
	connect.AssertEqual(t, env.state.ledger["tx-2"], networkId)
	connect.AssertEqual(t, len(env.state.ledger), 1)
	connect.AssertEqual(t, len(env.state.events), 1)
	connect.AssertEqual(t, len(env.state.windows[otherNetworkId]), 0)
	connect.AssertEqual(t, env.refreshed, []server.Id{networkId})
}

// From the move on, the renewals and revocations the notification webhook
// and the reconciler validate resolve to the account the subscription moved
// to, while a new purchase whose token names a live account stays that
// account's.
func TestAppleRenewalAfterMoveResolvesToTheNewAccount(t *testing.T) {
	env := installMoveFakes(t)
	_, deletedNetworkId := env.addNetwork()
	env.deleteNetwork(deletedNetworkId)
	clientSession, networkId := env.addNetwork()

	renewal := moveTestRenewal(moveTestClaims(deletedNetworkId, "orig-1", "tx-3", time.Hour))
	// before the move, nothing names a live network
	transaction, err := validateAppleTransactionBound(context.Background(), renewal, []string{moveTestProductId}, true)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.networkId, deletedNetworkId)

	connect.AssertEqual(t, verifyMove(t, moveTestClaims(deletedNetworkId, "orig-1", "tx-2", 24*time.Hour), clientSession), VerifyPurchaseStatusCredited)

	transaction, err = validateAppleTransactionBound(context.Background(), renewal, []string{moveTestProductId}, true)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.networkId, networkId)
	connect.AssertEqual(t, *transaction.movedFromNetworkId, deletedNetworkId)

	// a revocation payload (no entitlement fields) resolves the same way
	revocation := AppleNotificationDecodedPayload{
		NotificationType: "REFUND",
		SignedDate:       moveTestNow.UnixMilli(),
		TransactionInfo: map[string]any{
			"appAccountToken":       deletedNetworkId.String(),
			"transactionId":         "tx-2",
			"originalTransactionId": "orig-1",
			"productId":             moveTestProductId,
		},
	}
	transaction, err = validateAppleTransactionBound(context.Background(), revocation, []string{moveTestProductId}, false)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.networkId, networkId)

	// a later purchase of the same subscription from a live account stays its own
	_, liveNetworkId := env.addNetwork()
	transaction, err = validateAppleTransactionBound(
		context.Background(),
		moveTestRenewal(moveTestClaims(liveNetworkId, "orig-1", "tx-9", time.Hour)),
		[]string{moveTestProductId},
		true,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.networkId, liveNetworkId)
	connect.AssertEqual(t, transaction.movedFromNetworkId == nil, true)
}

// A subscription moved to an account that is deleted in turn moves again to
// the account restoring it; it never moves away from a live account.
func TestAppleRestoreMovesAgainFromADeletedMoveTarget(t *testing.T) {
	env := installMoveFakes(t)
	_, firstNetworkId := env.addNetwork()
	env.deleteNetwork(firstNetworkId)
	secondSession, secondNetworkId := env.addNetwork()
	thirdSession, thirdNetworkId := env.addNetwork()

	claims := moveTestClaims(firstNetworkId, "orig-1", "tx-2", 24*time.Hour)
	connect.AssertEqual(t, verifyMove(t, claims, secondSession), VerifyPurchaseStatusCredited)
	// the second account is live: the third stays refused
	connect.AssertEqual(t, verifyMove(t, claims, thirdSession), VerifyPurchaseStatusWrongNetwork)

	env.deleteNetwork(secondNetworkId)
	connect.AssertEqual(t, verifyMove(t, claims, thirdSession), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.state.moves["orig-1"], thirdNetworkId)
	connect.AssertEqual(t, env.state.windows[thirdNetworkId]["tx-2"], true)
	connect.AssertEqual(t, env.state.windows[secondNetworkId]["tx-2"], false)
	connect.AssertEqual(t, len(env.state.events), 2)
	connect.AssertEqual(t, env.state.events[1].fromNetworkId, secondNetworkId)
	connect.AssertEqual(t, env.state.events[1].rehomed, true)
}
