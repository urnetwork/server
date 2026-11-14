package controller

// Database-backed test of the accepted mark (store_notification_flow.go): a
// verified Play notification that is processed moves the store's row of
// store_notification_flow, which the migration made with its clock at the
// migration, and counts as accepted. Needs the test database
// (server.DefaultTestEnv).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The migration's rows exist for both stores; a processed Play notification
// moves google's to now and counts as accepted; an earlier mark never moves
// it back.
func TestPlayNotificationAcceptedMovesTheStoreFlow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// the migration's row
		seeded, ok := model.GetStoreNotificationLastAcceptedInConn(nil, ctx, model.SubscriptionMarketGoogle)
		connect.AssertEqual(t, ok, true)
		_, ok = model.GetStoreNotificationLastAcceptedInConn(nil, ctx, model.SubscriptionMarketApple)
		connect.AssertEqual(t, ok, true)

		// the row is older than this notification
		cryptoDbExec(ctx, `UPDATE store_notification_flow SET last_accepted_at = $1 WHERE store = 'google'`, seeded.Add(-48*time.Hour))
		// a fresh recorder: no mark written by this process yet
		previousRecorder := storeNotifications
		storeNotifications = newStoreNotificationRecorder(
			DefaultStoreNotificationFlowSettings(),
			func(ctx context.Context, store string, acceptedAt time.Time) {
				model.MarkStoreNotificationAcceptedInConn(nil, ctx, store, acceptedAt)
			},
		)
		previousPackage := playPackageNameFunc
		playPackageNameFunc = func() string { return "network.ur.synthetic" }
		defer func() {
			storeNotifications = previousRecorder
			playPackageNameFunc = previousPackage
		}()

		// a Play test notification: verified, processed, nothing to credit
		message, err := json.Marshal(map[string]any{
			"version":          "1.0",
			"packageName":      "network.ur.synthetic",
			"eventTimeMillis":  "1762963200000",
			"testNotification": map[string]any{"version": "1.0"},
		})
		connect.AssertEqual(t, err, nil)
		args := &PlayWebhookArgs{Message: &PlayWebhookMessage{Data: base64.StdEncoding.EncodeToString(message)}}

		before := storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationAccepted)
		start := server.NowUtc()
		_, err = PlayWebhook(args, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationAccepted)-before, 1.0)
		accepted, ok := model.GetStoreNotificationLastAcceptedInConn(nil, ctx, model.SubscriptionMarketGoogle)
		connect.AssertEqual(t, ok, true)
		if accepted.Before(start.Add(-time.Second)) {
			t.Fatalf("google last accepted %s, want at or after %s", accepted, start)
		}

		// an earlier mark never moves the time back
		model.MarkStoreNotificationAcceptedInConn(nil, ctx, model.SubscriptionMarketGoogle, accepted.Add(-time.Hour))
		again, _ := model.GetStoreNotificationLastAcceptedInConn(nil, ctx, model.SubscriptionMarketGoogle)
		connect.AssertEqual(t, again.Equal(accepted), true)
	})
}
