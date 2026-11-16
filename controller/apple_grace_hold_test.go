package controller

// The App Store's billing grace period: a renewal whose charge failed keeps Pro
// until Apple's gracePeriodExpiresDate while Apple retries the charge, from the
// DID_FAIL_TO_RENEW notification (subtype GRACE_PERIOD) or from the hourly
// reconcile's status 4. The hold mints no bytes and no revenue, ends when Apple
// stops retrying or refunds the purchase, and sends no subscription-ended notice.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const appleGraceTestProductId = "supporter_monthly_26"

// A network whose monthly App Store window was credited and has lapsed: the
// renewal's charge failed three days ago, and Apple's billing grace runs 13 more
// days.
type appleGraceFixture struct {
	ctx           context.Context
	networkId     server.Id
	transactionId string
	purchaseTime  time.Time
	expiresTime   time.Time
	graceExpires  time.Time
}

func newAppleGraceFixture(t testing.TB, name string) *appleGraceFixture {
	ctx := context.Background()
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "synthetic-"+name, server.NewId())
	now := server.NowUtc().Truncate(time.Millisecond)
	fixture := &appleGraceFixture{
		ctx:           ctx,
		networkId:     networkId,
		transactionId: "synthetic-grace-" + server.NewId().String(),
		purchaseTime:  now.Add(-33 * 24 * time.Hour),
		expiresTime:   now.Add(-3 * 24 * time.Hour),
		graceExpires:  now.Add(13 * 24 * time.Hour),
	}
	appleTestCredit(t, ctx, networkId, fixture.transactionId, appleGraceTestProductId, fixture.purchaseTime, fixture.expiresTime)
	return fixture
}

// JSON-decoded claims, as the verifier and the reconcile hand them over.
func appleGraceClaims(t testing.TB, claims map[string]any) map[string]any {
	claimsJson, err := json.Marshal(claims)
	connect.AssertEqual(t, err, nil)
	var normalized map[string]any
	connect.AssertEqual(t, json.Unmarshal(claimsJson, &normalized), nil)
	return normalized
}

func (self *appleGraceFixture) transactionClaims(t testing.TB) map[string]any {
	return appleTestTransactionClaims(t, self.networkId, self.transactionId, appleGraceTestProductId, self.purchaseTime, self.expiresTime)
}

func (self *appleGraceFixture) renewalClaims(t testing.TB, graceExpires time.Time) map[string]any {
	return appleGraceClaims(t, map[string]any{
		"appAccountToken":        self.networkId.String(),
		"originalTransactionId":  self.transactionId,
		"autoRenewStatus":        1,
		"gracePeriodExpiresDate": graceExpires.UnixMilli(),
	})
}

// Apple's DID_FAIL_TO_RENEW for the lapsed renewal, in its billing grace period.
func (self *appleGraceFixture) graceNotification(t testing.TB, graceExpires time.Time) AppleNotificationDecodedPayload {
	return AppleNotificationDecodedPayload{
		NotificationType: "DID_FAIL_TO_RENEW",
		Subtype:          "GRACE_PERIOD",
		NotificationUUID: server.NewId().String(),
		SignedDate:       server.NowUtc().UnixMilli(),
		TransactionInfo:  self.transactionClaims(t),
		RenewalInfo:      self.renewalClaims(t, graceExpires),
	}
}

// The App Store Server API's answer for the subscription, in the given status.
func (self *appleGraceFixture) statuses(t testing.TB, status int) *appleSubscriptionStatusesResponse {
	return &appleSubscriptionStatusesResponse{
		Data: []*appleSubscriptionGroup{{
			LastTransactions: []*appleLastTransaction{{
				Status:                status,
				OriginalTransactionId: self.transactionId,
				SignedTransactionInfo: appleTestJws(t, self.transactionClaims(t)),
				SignedRenewalInfo:     appleTestJws(t, self.renewalClaims(t, self.graceExpires)),
			}},
		}},
	}
}

func (self *appleGraceFixture) process(t testing.TB, notification AppleNotificationDecodedPayload) bool {
	processed, err := ProcessAppleNotification(self.ctx, notification, []string{appleGraceTestProductId})
	if err != nil {
		t.Fatalf("%s/%s: %v", notification.NotificationType, notification.Subtype, err)
	}
	return processed
}

func (self *appleGraceFixture) reconcile(t testing.TB) *PaymentReconcileRunResult {
	result, err := RunPaymentReconciliationWithOptions(
		reconcileTestSession(t, self.ctx),
		&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketApple}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// One in-window Pro balance of the network, which a zero-byte hold is among,
// unlike model.GetActiveTransferBalances.
type appleGraceProRow struct {
	startByteCount    int64
	netRevenue        int64
	subsidyNetRevenue int64
	endTime           time.Time
}

func (self *appleGraceFixture) proRows() []appleGraceProRow {
	rows := []appleGraceProRow{}
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
				var row appleGraceProRow
				server.Raise(result.Scan(&row.startByteCount, &row.netRevenue, &row.subsidyNetRevenue, &row.endTime))
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// The network's open App Store renewals and the revenue they record.
func (self *appleGraceFixture) openRenewals() (count int, netRevenue int64) {
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`
			SELECT count(*), COALESCE(sum(net_revenue_nano_cents), 0)
			FROM subscription_renewal
			WHERE network_id = $1 AND market = $2 AND $3 < end_time
			`,
			self.networkId,
			model.SubscriptionMarketApple,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count, &netRevenue))
			}
		})
	})
	return
}

func (self *appleGraceFixture) assertHeld(t testing.TB) {
	t.Helper()
	if !model.IsProNetwork(self.ctx, self.networkId) {
		t.Fatal("Pro did not hold through the App Store's billing grace period")
	}
	rows := self.proRows()
	if len(rows) != 1 || rows[0].startByteCount != 0 || rows[0].netRevenue != 0 || rows[0].subsidyNetRevenue != 0 {
		t.Fatalf("the hold must mint nothing: %+v", rows)
	}
	if !rows[0].endTime.Equal(self.graceExpires.Add(SubscriptionGracePeriod)) {
		t.Fatalf("the hold ends %v, want the grace period's end with the store's day %v", rows[0].endTime, self.graceExpires.Add(SubscriptionGracePeriod))
	}
	if count, netRevenue := self.openRenewals(); count != 1 || netRevenue != 0 {
		t.Fatalf("open renewals %d with revenue %d, want the hold's one with none", count, netRevenue)
	}
}

// Apple keeps the customer entitled in its billing grace period, so Pro stays.
// The hold is no credit: no bytes, no revenue, no notice. A redelivery, and the
// same grace under a new notification id, add nothing.
func TestAppleGraceNotificationHoldsProWithoutACredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newAppleGraceFixture(t, "applegracenotify")
		if model.IsProNetwork(fixture.ctx, fixture.networkId) {
			t.Fatal("the lapsed window must not read as Pro before the grace notification")
		}

		notification := fixture.graceNotification(t, fixture.graceExpires)
		if fixture.process(t, notification) {
			t.Fatal("a grace period is no new paid entitlement")
		}
		fixture.assertHeld(t)

		fixture.process(t, notification)
		fixture.process(t, fixture.graceNotification(t, fixture.graceExpires))
		fixture.assertHeld(t)
		if count := outboxMessageCount(t, fixture.ctx, fixture.networkId); count != 0 {
			t.Fatalf("a subscription in grace was sent %d notices", count)
		}
	})
}

// The hourly reconcile holds Pro for a grace period its notification missed,
// once, with no credit.
func TestPaymentReconcileAppleHoldsProThroughBillingGrace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer disableAllReconcileStores(t)()
		env := newAppleReconcileTestEnv(t, []string{appleGraceTestProductId})
		fixture := newAppleGraceFixture(t, "applegracereconcile")
		env.statuses[fixture.transactionId] = fixture.statuses(t, 4)

		for _, want := range []int{1, 0} {
			result := fixture.reconcile(t)
			if result.Credited != 0 || result.EntitlementsRepaired != want || result.Errors != 0 {
				t.Fatalf("reconcile: credited %d, repaired %d, errors %d, want 0, %d and 0", result.Credited, result.EntitlementsRepaired, result.Errors, want)
			}
			fixture.assertHeld(t)
		}
	})
}

// Apple stops retrying (billing retry, status 3, or expired, status 2): the
// reconcile ends the hold.
func TestPaymentReconcileAppleEndsTheGraceHoldWhenAppleStopsRetrying(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer disableAllReconcileStores(t)()
		env := newAppleReconcileTestEnv(t, []string{appleGraceTestProductId})
		for _, status := range []int{3, 2} {
			fixture := newAppleGraceFixture(t, fmt.Sprintf("applegraceends%d", status))
			env.statuses[fixture.transactionId] = fixture.statuses(t, 4)
			fixture.reconcile(t)
			fixture.assertHeld(t)

			env.statuses[fixture.transactionId] = fixture.statuses(t, status)
			if result := fixture.reconcile(t); result.Ended != 1 {
				t.Fatalf("status %d: ended %d, want the hold", status, result.Ended)
			}
			if model.IsProNetwork(fixture.ctx, fixture.networkId) {
				t.Fatalf("status %d: the hold outlived the grace period", status)
			}
		}
	})
}

// A refund of the purchase ends the hold with the rest of it.
func TestAppleGraceHoldEndsOnRefund(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newAppleGraceFixture(t, "applegracerefund")
		fixture.process(t, fixture.graceNotification(t, fixture.graceExpires))
		fixture.assertHeld(t)

		fixture.process(t, appleRevocationNotification("REFUND", server.NewId().String(), fixture.networkId, fixture.transactionId))
		if model.IsProNetwork(fixture.ctx, fixture.networkId) {
			t.Fatal("the hold outlived the refund")
		}
	})
}

// The charge goes through in the grace period: the recovered period is credited
// as usual, with its revenue, and starts a cycle with its full Pro grant.
func TestAppleGraceRecoveryCreditsTheNextPeriod(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		setProCycleTestData(t, 10*model.Tib)
		fixture := newAppleGraceFixture(t, "applegracerecovers")
		fixture.process(t, fixture.graceNotification(t, fixture.graceExpires))

		now := server.NowUtc().Truncate(time.Millisecond)
		recoveredTransactionId := "synthetic-grace-recovered-" + server.NewId().String()
		recovered := AppleNotificationDecodedPayload{
			NotificationType: "DID_RENEW",
			Subtype:          "BILLING_RECOVERY",
			NotificationUUID: server.NewId().String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo: appleTestTransactionClaims(
				t, fixture.networkId, recoveredTransactionId, appleGraceTestProductId,
				now.Add(-time.Hour), fixture.expiresTime.Add(30*24*time.Hour),
			),
		}
		if !fixture.process(t, recovered) {
			t.Fatal("the recovered charge was not credited")
		}
		credited := int64(0)
		for _, row := range fixture.proRows() {
			credited += row.startByteCount
		}
		if credited != int64(10*model.Tib) {
			t.Fatalf("credited %d bytes, want one cycle's full grant %d", credited, 10*model.Tib)
		}
		if !model.IsProNetwork(fixture.ctx, fixture.networkId) {
			t.Fatal("recovered, but not Pro")
		}
	})
}

// A grace period that has already run out holds nothing.
func TestAppleGraceNotificationAfterTheGraceEndedHoldsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newAppleGraceFixture(t, "applegraceover")
		fixture.process(t, fixture.graceNotification(t, server.NowUtc().Add(-2*24*time.Hour)))
		if model.IsProNetwork(fixture.ctx, fixture.networkId) {
			t.Fatal("a grace period that ended held Pro")
		}
		if rows := fixture.proRows(); len(rows) != 0 {
			t.Fatalf("a grace period that ended wrote %+v", rows)
		}
	})
}

// Manage Subscription reads the grace period's end, not the lapsed expiry, while
// Apple keeps the subscription in billing grace.
func TestAppleSubscriptionStateInBillingGraceEndsAtTheGraceEnd(t *testing.T) {
	expires := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	graceExpires := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	statuses := &appleSubscriptionStatusesResponse{
		Data: []*appleSubscriptionGroup{{
			LastTransactions: []*appleLastTransaction{{
				Status:                4,
				OriginalTransactionId: "2000000456",
				SignedTransactionInfo: appleTestJws(t, map[string]any{
					"productId":   "supporter_monthly_26",
					"expiresDate": float64(expires.UnixMilli()),
				}),
				SignedRenewalInfo: appleTestJws(t, map[string]any{
					"autoRenewStatus":        float64(1),
					"gracePeriodExpiresDate": float64(graceExpires.UnixMilli()),
				}),
			}},
		}},
	}
	state := appleSubscriptionState(statuses)
	if state == nil || !state.Active {
		t.Fatal("billing grace is entitled")
	}
	if state.EndTime == nil || !state.EndTime.Equal(graceExpires) {
		t.Fatalf("in billing grace the subscription runs to %v, got %v", graceExpires, state.EndTime)
	}

	// out of grace, the expiry stands
	statuses.Data[0].LastTransactions[0].Status = 1
	state = appleSubscriptionState(statuses)
	if state.EndTime == nil || !state.EndTime.Equal(expires) {
		t.Fatalf("active: expiry %v, got %v", expires, state.EndTime)
	}
}
