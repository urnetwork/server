package controller

// Failure mode (support inbox 7765): the App Store charges a renewal ahead of
// the expiry, and the renewal transaction's purchaseDate is the start of the
// period it pays for, the previous transaction's expiresDate. The validator
// shared by the notification webhook, the verify endpoint and the reconciler
// refused any purchaseDate more than 5 minutes after the signed date, so every
// early renewal was "implausible": the DID_RENEW webhook refused it, and the
// reconciler only credited it once the period began. On Main the monthly
// renewals of 2026-10-17 and 2026-10-26 were charged about 8 hours early and
// refused until then, and since the ledger started (2026-08-11) no ordinary
// production DID_RENEW was credited by the webhook. The legacy yearly
// subscribers (supporter_yearly, renewing from December 2026) renew the same
// way.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Claims of a renewal signed signedAt, whose period starts lead after it.
func appleEarlyRenewalClaims(
	networkId server.Id,
	productId string,
	transactionId string,
	signedAt time.Time,
	lead time.Duration,
	period time.Duration,
) AppleNotificationDecodedPayload {
	periodStart := signedAt.Add(lead)
	return AppleNotificationDecodedPayload{
		NotificationType: "DID_RENEW",
		NotificationUUID: server.NewId().String(),
		SignedDate:       signedAt.UnixMilli(),
		TransactionInfo: map[string]any{
			"appAccountToken":       networkId.String(),
			"transactionId":         transactionId,
			"originalTransactionId": "original-" + transactionId,
			"transactionReason":     "RENEWAL",
			"productId":             productId,
			"purchaseDate":          float64(periodStart.UnixMilli()),
			"expiresDate":           float64(periodStart.Add(period).UnixMilli()),
			"price":                 float64(39990),
			"currency":              "USD",
		},
	}
}

func TestAppleEarlyRenewalIsAccepted(t *testing.T) {
	signedAt := time.Date(2026, 10, 26, 6, 30, 0, 0, time.UTC)
	configured := []string{"supporter_monthly_26", "supporter_yearly_26"}
	networkId := server.NewId()
	year := 365 * 24 * time.Hour
	month := 31 * 24 * time.Hour

	for _, c := range []struct {
		productId string
		period    time.Duration
	}{
		{"supporter_yearly", year},
		{"supporter_yearly_26", year},
		{"supporter", month},
		{"supporter_monthly_26", month},
	} {
		claims := appleEarlyRenewalClaims(networkId, c.productId, "early-"+c.productId, signedAt, 8*time.Hour, c.period)
		transaction, err := validateAppleTransaction(claims, configured, true)
		if err != nil {
			t.Fatalf("%s renewal charged 8 hours before its period was refused: %v", c.productId, err)
		}
		// the period is the one the renewal pays for, not shifted to the charge
		connect.AssertEqual(t, transaction.purchaseTime.Equal(signedAt.Add(8*time.Hour)), true)
		connect.AssertEqual(t, transaction.expiresTime.Equal(signedAt.Add(8*time.Hour).Add(c.period)), true)
	}

	// claims that name no reason: a transaction after the subscription's first
	// is a renewal
	unnamed := appleEarlyRenewalClaims(networkId, "supporter_yearly", "early-unnamed", signedAt, 20*time.Hour, year)
	delete(unnamed.TransactionInfo, "transactionReason")
	if _, err := validateAppleTransaction(unnamed, configured, true); err != nil {
		t.Fatalf("renewal without a transactionReason charged 20 hours early was refused: %v", err)
	}

	// the restrictions that stay: a purchase is dated when the customer made it
	purchase := appleEarlyRenewalClaims(networkId, "supporter_yearly", "future-purchase", signedAt, time.Hour, year)
	purchase.TransactionInfo["transactionReason"] = "PURCHASE"
	if _, err := validateAppleTransaction(purchase, configured, true); err == nil {
		t.Fatal("a purchase dated an hour after it was signed was accepted")
	}
	first := appleEarlyRenewalClaims(networkId, "supporter_yearly", "future-first", signedAt, time.Hour, year)
	delete(first.TransactionInfo, "transactionReason")
	first.TransactionInfo["originalTransactionId"] = "future-first"
	if _, err := validateAppleTransaction(first, configured, true); err == nil {
		t.Fatal("a subscription's first transaction dated an hour after it was signed was accepted")
	}
	// no renewal is charged days ahead
	if _, err := validateAppleTransaction(appleEarlyRenewalClaims(networkId, "supporter_yearly", "days-early", signedAt, 72*time.Hour, year), configured, true); err == nil {
		t.Fatal("a renewal dated 3 days after it was signed was accepted")
	}
	// nor does it pay for more than two years
	if _, err := validateAppleTransaction(appleEarlyRenewalClaims(networkId, "supporter_yearly", "too-long", signedAt, 8*time.Hour, 3*year), configured, true); err == nil {
		t.Fatal("a renewal paying for three years was accepted")
	}
	// and a product the apps never sold is still refused
	if _, err := validateAppleTransaction(appleEarlyRenewalClaims(networkId, "attacker-controlled-product", "unsold", signedAt, 8*time.Hour, year), configured, true); err == nil {
		t.Fatal("an unsold product's early renewal was accepted")
	}
}

// The webhook credits a yearly renewal when Apple charges it: the next period
// is credited as its own window, the current one stays, and a redelivery is
// acknowledged without a second credit.
func TestAppleEarlyDidRenewCreditsTheNextPeriod(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		configured := []string{"supporter_monthly_26", "supporter_yearly_26"}
		year := 365 * 24 * time.Hour
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "appleearlyrenew", server.NewId())

		// the current yearly period, which ends in 8 hours
		currentTransactionId := "current-yearly-" + server.NewId().String()
		processed, err := ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
			NotificationType: "SUBSCRIBED",
			NotificationUUID: server.NewId().String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo: map[string]any{
				"appAccountToken":       networkId.String(),
				"transactionId":         currentTransactionId,
				"originalTransactionId": currentTransactionId,
				"transactionReason":     "PURCHASE",
				"productId":             "supporter_yearly",
				"purchaseDate":          float64(now.Add(8*time.Hour - year).UnixMilli()),
				"expiresDate":           float64(now.Add(8 * time.Hour).UnixMilli()),
				"price":                 float64(39990),
				"currency":              "USD",
			},
		}, configured)
		if err != nil || !processed {
			t.Fatalf("the current period was not credited: processed = %t, err = %v", processed, err)
		}

		// Apple renews it now, 8 hours ahead, for the period that starts then
		renewTransactionId := "early-yearly-" + server.NewId().String()
		renewal := appleEarlyRenewalClaims(networkId, "supporter_yearly", renewTransactionId, now, 8*time.Hour, year)
		renewal.TransactionInfo["originalTransactionId"] = currentTransactionId
		processed, err = ProcessAppleNotification(ctx, renewal, configured)
		if err != nil || !processed {
			t.Fatalf("DID_RENEW of a yearly renewal charged 8 hours early: processed = %t, err = %v; want it credited", processed, err)
		}
		connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, renewTransactionId), true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// The next period starts where the current one expires and lasts the
		// year it paid for. The windows are compared with each other, as the
		// timestamp columns store them.
		var renewals int
		var currentEnd, renewalStart, renewalEnd time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM subscription_renewal WHERE network_id = $1`, networkId).Scan(&renewals))
			server.Raise(conn.QueryRow(ctx, `SELECT end_time FROM subscription_renewal WHERE network_id = $1 AND transaction_id = $2`,
				networkId, currentTransactionId).Scan(&currentEnd))
			server.Raise(conn.QueryRow(ctx, `SELECT start_time, end_time FROM subscription_renewal WHERE network_id = $1 AND transaction_id = $2`,
				networkId, renewTransactionId).Scan(&renewalStart, &renewalEnd))
		})
		connect.AssertEqual(t, renewals, 2)
		if !renewalStart.Equal(currentEnd.Add(-SubscriptionGracePeriod)) || renewalEnd.Sub(renewalStart) != year+SubscriptionGracePeriod {
			t.Fatalf("the renewal was credited for [%s, %s), want the year from the current period's expiry %s",
				renewalStart, renewalEnd, currentEnd.Add(-SubscriptionGracePeriod))
		}

		// Apple redelivers the same notification: acknowledged, not credited again
		processed, err = ProcessAppleNotification(ctx, renewal, configured)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM subscription_renewal WHERE network_id = $1`, networkId).Scan(&renewals))
		})
		connect.AssertEqual(t, renewals, 2)
	})
}
