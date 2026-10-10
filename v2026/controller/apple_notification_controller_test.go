package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProcessAppleNotificationIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-notification-test", userId)

		now := server.NowUtc().Truncate(time.Millisecond)
		notificationId := server.NewId()
		transactionId := "apple-" + server.NewId().String()
		notification := AppleNotificationDecodedPayload{
			NotificationType: "SUBSCRIBED",
			Subtype:          "INITIAL_BUY",
			NotificationUUID: notificationId.String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo: map[string]any{
				"appAccountToken": networkId.String(),
				"transactionId":   transactionId,
				"productId":       "supporter_monthly_26",
				"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
				"expiresDate":     float64(now.Add(30 * 24 * time.Hour).UnixMilli()),
				"price":           float64(4990),
			},
		}

		processed, err := ProcessAppleNotification(ctx, notification, []string{"supporter_monthly_26"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)

		processed, err = ProcessAppleNotification(ctx, notification, []string{"supporter_monthly_26"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)

		notificationCount := 0
		transactionCount := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT count(*) FROM apple_notification WHERE notification_uuid = $1`, notificationId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&notificationCount))
				}
			})

			result, err = conn.Query(ctx, `SELECT count(*) FROM apple_subscription_transaction WHERE transaction_id = $1`, transactionId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&transactionCount))
				}
			})
		})
		connect.AssertEqual(t, notificationCount, 1)
		connect.AssertEqual(t, transactionCount, 1)
	})
}

func TestProcessAppleNotificationRejectsUnapprovedProduct(t *testing.T) {
	now := server.NowUtc().Truncate(time.Millisecond)
	notification := AppleNotificationDecodedPayload{
		NotificationType: "SUBSCRIBED",
		NotificationUUID: server.NewId().String(),
		SignedDate:       now.UnixMilli(),
		TransactionInfo: map[string]any{
			"appAccountToken": server.NewId().String(),
			"transactionId":   "unapproved-product-transaction",
			"productId":       "attacker-controlled-product",
			"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
			"expiresDate":     float64(now.Add(30 * 24 * time.Hour).UnixMilli()),
			"price":           float64(4990),
		},
	}

	_, err := ProcessAppleNotification(context.Background(), notification, []string{"supporter_monthly_26"})
	connect.AssertEqual(t, err != nil, true)
}
