package proxy

// A raise in a watch's change subscription ends its wakes only. The watch and
// the notification keep running, and the timed poll still finds new proxy
// clients. The subscription opens against a source that raises, the change
// reader is a fixture, and all timing runs on the synctest clock.

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// HandleError runs a cancel handler on every raise, so the subscription
// goroutine must not hand it the notification's cancel: nothing restarts a
// canceled notification, and the proxy would stop adding new clients' peers.
func TestProxyClientNotificationSubscriptionRaiseKeepsPolling(t *testing.T) {
	t.Setenv("WARP_HOST", "synthetic-proxy")
	t.Setenv("WARP_BLOCK", "g7")
	t.Setenv("WARP_DOMAIN", "example")
	synctest.Test(t, func(t *testing.T) {
		host := "synthetic-proxy"

		settings := DefaultProxySettings()
		settings.NotificationTimeout = time.Minute
		settings.ReconcileTimeout = 0

		subscriptionSource := &server.RedisShardSubscriptionSource{
			Open: func(ctx context.Context, channel string) (server.RedisShardSubscription, error) {
				panic(errors.New("synthetic change subscription fault"))
			},
		}
		polls := &testProxyChangePolls{hostCounts: map[string]int{}}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		notification := newProxyClientNotification(ctx, settings, subscriptionSource, polls.getProxyClientsSince)
		synctest.Wait()
		if notification.ctx.Err() != nil {
			t.Fatal("a subscription raise canceled the notification")
		}
		select {
		case <-notification.InitialSyncDone():
		default:
			t.Fatal("the initial poll did not complete the initial sync")
		}

		pollCount := polls.count(host)
		time.Sleep(settings.NotificationTimeout)
		synctest.Wait()
		if polls.count(host) <= pollCount {
			t.Fatal("the watch stopped polling after its subscription raised")
		}
	})
}
