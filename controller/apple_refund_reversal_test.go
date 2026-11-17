package controller

// The App Store REFUND_REVERSED rules (apple_refund_reversal_controller.go)
// against a fake of the notification's transaction: the windows a refund cut,
// the refunds that ended something here, and the audit events. No database,
// no network, a fixed clock. apple_refund_reversal_db_test.go runs the same
// paths through ProcessAppleNotification against the test database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const reversalTestProductId = "supporter_monthly_26"

var reversalTestNow = time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC)

// One supporter renewal row of the fake: the window a credit wrote.
type reversalFakeWindow struct {
	networkId     server.Id
	transactionId string
	startTime     time.Time
	endTime       time.Time
	// nil for a row from before paid_end_time
	paidEndTime *time.Time
}

type reversalFakeTx struct {
	windows []*reversalFakeWindow
	// transactions a refund ended a window of here
	refundEnded map[string]bool
	events      []map[string]any
	restores    int
}

func newReversalFakeTx() *reversalFakeTx {
	return &reversalFakeTx{
		refundEnded: map[string]bool{},
	}
}

// credit writes the window the App Store credit path writes for the
// transaction: purchase to expiry plus the store grace, with the paid end.
func (self *reversalFakeTx) credit(networkId server.Id, transaction *validatedAppleTransaction) *reversalFakeWindow {
	paidEndTime := transaction.expiresTime
	window := &reversalFakeWindow{
		networkId:     networkId,
		transactionId: transaction.transactionId,
		startTime:     transaction.purchaseTime,
		endTime:       transaction.expiresTime.Add(SubscriptionGracePeriod),
		paidEndTime:   &paidEndTime,
	}
	self.windows = append(self.windows, window)
	return window
}

// refund is the REFUND notification: the transaction's open windows end now,
// and its revoked event says it ended something
func (self *reversalFakeTx) refund(transactionId string, now time.Time) {
	for _, window := range self.windows {
		if window.transactionId == transactionId && now.Before(window.endTime) {
			window.endTime = now
			self.refundEnded[transactionId] = true
		}
	}
}

// revoke is a family-sharing REVOKE: the window ends, and no refund ended it
func (self *reversalFakeTx) revoke(transactionId string, now time.Time) {
	for _, window := range self.windows {
		if window.transactionId == transactionId && now.Before(window.endTime) {
			window.endTime = now
		}
	}
}

func (self *reversalFakeTx) RefundEnded(transactionId string) bool {
	return self.refundEnded[transactionId]
}

// Restore matches model.RestoreRefundedAppleEntitlementInTx: the
// transaction's paid window on its network, ended before its end
func (self *reversalFakeTx) Restore(transaction *validatedAppleTransaction, now time.Time) (*model.ReconcileSubscriptionRenewal, error) {
	self.restores += 1
	endTime := transaction.expiresTime.Add(SubscriptionGracePeriod)
	for _, window := range self.windows {
		if window.networkId != transaction.networkId || window.transactionId != transaction.transactionId {
			continue
		}
		paid := (window.paidEndTime != nil && window.paidEndTime.Equal(transaction.expiresTime)) ||
			(window.paidEndTime == nil && window.startTime.Equal(transaction.purchaseTime))
		if !paid || now.Before(window.endTime) || !window.endTime.Before(endTime) {
			continue
		}
		window.endTime = endTime
		return &model.ReconcileSubscriptionRenewal{
			NetworkId:     window.networkId,
			StartTime:     window.startTime,
			EndTime:       endTime,
			TransactionId: window.transactionId,
		}, nil
	}
	return nil, nil
}

func (self *reversalFakeTx) RecordEvent(transaction *validatedAppleTransaction, details map[string]any) {
	self.events = append(self.events, details)
}

// pro reports whether the network holds an open window at the time
func (self *reversalFakeTx) pro(networkId server.Id, now time.Time) bool {
	for _, window := range self.windows {
		if window.networkId == networkId && !now.Before(window.startTime) && now.Before(window.endTime) {
			return true
		}
	}
	return false
}

// reversalTestTransaction is a monthly subscription's transaction, bought
// purchasedAgo before reversalTestNow, validated as the webhook validates it.
func reversalTestTransaction(t *testing.T, networkId server.Id, transactionId string, purchasedAgo time.Duration) *validatedAppleTransaction {
	t.Helper()
	purchase := reversalTestNow.Add(-purchasedAgo)
	transaction, err := validateAppleTransaction(AppleNotificationDecodedPayload{
		NotificationType: "REFUND_REVERSED",
		SignedDate:       reversalTestNow.UnixMilli(),
		TransactionInfo: map[string]any{
			"appAccountToken":       networkId.String(),
			"transactionId":         transactionId,
			"originalTransactionId": "orig-" + transactionId,
			"productId":             reversalTestProductId,
			"purchaseDate":          float64(purchase.UnixMilli()),
			"expiresDate":           float64(purchase.Add(30 * 24 * time.Hour).UnixMilli()),
			"price":                 float64(4990),
			"currency":              "USD",
		},
	}, []string{reversalTestProductId}, true)
	connect.AssertEqual(t, err, nil)
	return transaction
}

func reversalTestNotification() AppleNotificationDecodedPayload {
	return AppleNotificationDecodedPayload{
		NotificationType: "REFUND_REVERSED",
		NotificationUUID: server.NewId().String(),
		SignedDate:       reversalTestNow.UnixMilli(),
	}
}

// The reversal gives back what the refund took: the paid window comes back to
// its end, so the network is Pro again, with one refund_reversed event.
func TestAppleRefundReversalRestoresTheRefundedWindow(t *testing.T) {
	store := newReversalFakeTx()
	networkId := server.NewId()
	transaction := reversalTestTransaction(t, networkId, "tx-1", 5*24*time.Hour)
	window := store.credit(networkId, transaction)
	store.refund("tx-1", reversalTestNow.Add(-2*24*time.Hour))
	connect.AssertEqual(t, store.pro(networkId, reversalTestNow), false)

	restored := appleReverseRefund(store, transaction, reversalTestNotification(), reversalTestNow)

	connect.AssertEqual(t, restored != nil, true)
	connect.AssertEqual(t, restored.NetworkId, networkId)
	connect.AssertEqual(t, restored.EndTime, transaction.expiresTime.Add(SubscriptionGracePeriod))
	connect.AssertEqual(t, window.endTime, transaction.expiresTime.Add(SubscriptionGracePeriod))
	connect.AssertEqual(t, store.pro(networkId, reversalTestNow), true)
	connect.AssertEqual(t, len(store.events), 1)
	connect.AssertEqual(t, store.events[0]["restored"], true)
	connect.AssertEqual(t, store.events[0]["notification_type"], "REFUND_REVERSED")
	_, hasReason := store.events[0]["reason"]
	connect.AssertEqual(t, hasReason, false)
}

// A row from before paid_end_time (a purchase credited before the Pro grant
// cycles) is the transaction's paid window when it starts with the purchase.
func TestAppleRefundReversalRestoresAWindowFromBeforePaidEnds(t *testing.T) {
	store := newReversalFakeTx()
	networkId := server.NewId()
	transaction := reversalTestTransaction(t, networkId, "tx-1", 5*24*time.Hour)
	window := store.credit(networkId, transaction)
	window.paidEndTime = nil
	store.refund("tx-1", reversalTestNow.Add(-time.Hour))

	connect.AssertEqual(t, appleReverseRefund(store, transaction, reversalTestNotification(), reversalTestNow) != nil, true)
	connect.AssertEqual(t, store.pro(networkId, reversalTestNow), true)
}

// A second reversal of the same refund (Apple sends a new notification, or a
// retry after the first committed) finds the window restored and changes
// nothing. A redelivery of the same notification never gets here: the
// notification ledger absorbs it (apple_refund_reversal_db_test.go).
func TestAppleRefundReversalTwiceRestoresOnce(t *testing.T) {
	store := newReversalFakeTx()
	networkId := server.NewId()
	transaction := reversalTestTransaction(t, networkId, "tx-1", 5*24*time.Hour)
	window := store.credit(networkId, transaction)
	store.refund("tx-1", reversalTestNow.Add(-time.Hour))

	connect.AssertEqual(t, appleReverseRefund(store, transaction, reversalTestNotification(), reversalTestNow) != nil, true)
	restoredEnd := window.endTime

	restored := appleReverseRefund(store, transaction, reversalTestNotification(), reversalTestNow.Add(time.Minute))
	connect.AssertEqual(t, restored == nil, true)
	connect.AssertEqual(t, window.endTime, restoredEnd)
	connect.AssertEqual(t, len(store.windows), 1)
	connect.AssertEqual(t, len(store.events), 2)
	connect.AssertEqual(t, store.events[1]["restored"], false)
	connect.AssertEqual(t, store.events[1]["reason"], "window_not_found")
}

// Only a refund this server processed is given back. A transaction it never
// refunded (or never knew), and a window a family-sharing REVOKE ended, stay as
// they are: no restore is tried, and the event says why.
func TestAppleRefundReversalOfAnUnrefundedTransactionChangesNothing(t *testing.T) {
	store := newReversalFakeTx()

	// unknown here: no window at all
	unknown := reversalTestTransaction(t, server.NewId(), "tx-unknown", 5*24*time.Hour)
	connect.AssertEqual(t, appleReverseRefund(store, unknown, reversalTestNotification(), reversalTestNow) == nil, true)

	// credited and never refunded: its open window is untouched
	liveNetworkId := server.NewId()
	live := reversalTestTransaction(t, liveNetworkId, "tx-live", 5*24*time.Hour)
	liveWindow := store.credit(liveNetworkId, live)
	liveEnd := liveWindow.endTime
	connect.AssertEqual(t, appleReverseRefund(store, live, reversalTestNotification(), reversalTestNow) == nil, true)
	connect.AssertEqual(t, liveWindow.endTime, liveEnd)

	// ended by a family-sharing REVOKE, not by a refund: it stays ended
	revokedNetworkId := server.NewId()
	revoked := reversalTestTransaction(t, revokedNetworkId, "tx-revoked", 5*24*time.Hour)
	store.credit(revokedNetworkId, revoked)
	store.revoke("tx-revoked", reversalTestNow.Add(-time.Hour))
	connect.AssertEqual(t, appleReverseRefund(store, revoked, reversalTestNotification(), reversalTestNow) == nil, true)
	connect.AssertEqual(t, store.pro(revokedNetworkId, reversalTestNow), false)

	connect.AssertEqual(t, store.restores, 0)
	connect.AssertEqual(t, len(store.events), 3)
	for _, event := range store.events {
		connect.AssertEqual(t, event["restored"], false)
		connect.AssertEqual(t, event["reason"], "refund_ended_nothing")
	}
}

// A reversal that comes after the refunded period is over gives nothing back,
// and a renewal since then keeps its own window: the reversal never reaches
// past the refunded transaction.
func TestAppleRefundReversalAfterTheRenewalChangesNothing(t *testing.T) {
	store := newReversalFakeTx()
	networkId := server.NewId()
	// bought 40 days ago for 30 days, refunded 20 days ago
	refunded := reversalTestTransaction(t, networkId, "tx-1", 40*24*time.Hour)
	store.credit(networkId, refunded)
	store.refund("tx-1", reversalTestNow.Add(-20*24*time.Hour))
	// the next period, renewed when the first one ran out
	renewed := reversalTestTransaction(t, networkId, "tx-2", 10*24*time.Hour)
	renewal := store.credit(networkId, renewed)
	renewalEnd := renewal.endTime

	restored := appleReverseRefund(store, refunded, reversalTestNotification(), reversalTestNow)

	connect.AssertEqual(t, restored == nil, true)
	connect.AssertEqual(t, store.restores, 0)
	connect.AssertEqual(t, renewal.endTime, renewalEnd)
	connect.AssertEqual(t, store.pro(networkId, reversalTestNow), true)
	connect.AssertEqual(t, store.events[0]["reason"], "paid_period_over")

	// a reversal inside the paid period, with the renewal to come, restores
	// only the refunded window; the renewal credits its own window after it
	store = newReversalFakeTx()
	current := reversalTestTransaction(t, networkId, "tx-3", 5*24*time.Hour)
	currentWindow := store.credit(networkId, current)
	store.refund("tx-3", reversalTestNow.Add(-time.Hour))
	connect.AssertEqual(t, appleReverseRefund(store, current, reversalTestNotification(), reversalTestNow) != nil, true)
	// the renewal Apple charges when the restored period runs out
	next := &validatedAppleTransaction{
		networkId:     networkId,
		transactionId: "tx-4",
		purchaseTime:  current.expiresTime,
		expiresTime:   current.expiresTime.Add(30 * 24 * time.Hour),
	}
	nextWindow := store.credit(networkId, next)
	connect.AssertEqual(t, currentWindow.endTime, current.expiresTime.Add(SubscriptionGracePeriod))
	connect.AssertEqual(t, nextWindow.startTime, next.purchaseTime)
	connect.AssertEqual(t, store.pro(networkId, next.purchaseTime.Add(time.Hour)), true)
}

// The reversal follows the subscription to the account it moved to when its
// own account was deleted (owner, 2026-11-17: "Restore moves it."): the
// transaction resolves to the new account, whose window comes back, and the
// deleted account's window stays ended.
func TestAppleRefundReversalFollowsAMovedSubscription(t *testing.T) {
	env := installMoveFakes(t)
	_, deletedNetworkId := env.addNetwork()
	env.deleteNetwork(deletedNetworkId)
	_, networkId := env.addNetwork()
	env.state.moves["orig-tx-1"] = networkId

	purchase := reversalTestNow.Add(-5 * 24 * time.Hour)
	reversalPayload := reversalTestNotification()
	reversalPayload.TransactionInfo = map[string]any{
		"appAccountToken":       deletedNetworkId.String(),
		"transactionId":         "tx-1",
		"originalTransactionId": "orig-tx-1",
		"productId":             reversalTestProductId,
		"purchaseDate":          float64(purchase.UnixMilli()),
		"expiresDate":           float64(purchase.Add(30 * 24 * time.Hour).UnixMilli()),
		"price":                 float64(4990),
		"currency":              "USD",
	}
	transaction, err := validateAppleTransactionBound(context.Background(), reversalPayload, []string{reversalTestProductId}, true)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.networkId, networkId)

	store := newReversalFakeTx()
	// the deleted account's window, ended by the move, and the moved window on
	// the new account, ended by the refund
	deletedWindow := store.credit(deletedNetworkId, transaction)
	deletedWindow.endTime = reversalTestNow.Add(-3 * 24 * time.Hour)
	movedWindow := store.credit(networkId, transaction)
	movedWindow.startTime = reversalTestNow.Add(-3 * 24 * time.Hour)
	store.refund("tx-1", reversalTestNow.Add(-time.Hour))

	connect.AssertEqual(t, appleReverseRefund(store, transaction, reversalPayload, reversalTestNow) != nil, true)
	connect.AssertEqual(t, store.pro(networkId, reversalTestNow), true)
	connect.AssertEqual(t, deletedWindow.endTime, reversalTestNow.Add(-3*24*time.Hour))
}
