package model

// Virtual socket deadlines distinguish incoming evidence from a successful
// write. All timing advances inside synctest, without Redis or wall-clock races.

import (
	"context"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

type testTimedContractOriginSubscription struct {
	*testContractOriginSubscription
	pingResponse func() any
	pings        atomic.Int32
}

func (self *testTimedContractOriginSubscription) ReceiveTimeout(ctx context.Context, timeout time.Duration) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-self.closed:
		return nil, errors.New("synthetic subscription closed")
	case event := <-self.events:
		return event, nil
	case <-time.After(timeout):
		return nil, context.DeadlineExceeded
	}
}

func (self *testTimedContractOriginSubscription) Ping(context.Context, ...string) error {
	self.pings.Add(1)
	if self.pingResponse != nil {
		self.events <- self.pingResponse()
	}
	return nil
}

type testTimedContractOriginOpen struct {
	channel      string
	subscription *testTimedContractOriginSubscription
}

func testTimedContractOriginOwner(ctx context.Context, opened chan<- testTimedContractOriginOpen, pingResponse func(string) any) *ContractOriginNotifications {
	return newContractOriginNotifications(ctx, DefaultContractOriginNotificationSettings(),
		func(_ context.Context, channel string) (contractOriginSubscription, error) {
			subscription := &testTimedContractOriginSubscription{testContractOriginSubscription: newTestContractOriginSubscription()}
			if pingResponse != nil {
				subscription.pingResponse = func() any { return pingResponse(channel) }
			}
			opened <- testTimedContractOriginOpen{channel: channel, subscription: subscription}
			return subscription, nil
		}, func(context.Context, string, string) error { return nil })
}

func TestContractOriginNotificationsRejoinAfterShardUnsubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, 16)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		first := <-opened
		first.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: first.channel}
		synctest.Wait()
		update := watch.Update()
		first.subscription.events <- &redis.Subscription{Kind: "sunsubscribe", Channel: first.channel}
		synctest.Wait()
		select {
		case <-first.subscription.closed:
		default:
			t.Fatal("slot migration left the unsubscribed socket open")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		var second testContractOriginOpen
		select {
		case second = <-opened:
		default:
			t.Fatal("slot migration did not establish a replacement subscription")
		}
		second.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: second.channel}
		synctest.Wait()
		select {
		case <-update:
		default:
			t.Fatal("replacement acknowledgement did not recheck possibly missed commits")
		}
	})
}

func TestContractOriginNotificationsIgnoreUnrelatedUnsubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, 16)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		connection := <-opened
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()
		update := watch.Update()
		connection.subscription.events <- &redis.Subscription{Kind: "sunsubscribe", Channel: "synthetic-unowned-channel"}
		synctest.Wait()
		select {
		case <-connection.subscription.closed:
			t.Fatal("unrelated unsubscribe closed the owned socket")
		case <-update:
			t.Fatal("unrelated unsubscribe woke the owned pair")
		default:
		}
	})
}

func TestContractOriginNotificationsRequireInitialAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testTimedContractOriginOpen, 16)
		owner := testTimedContractOriginOwner(t.Context(), opened, func(string) any { return &redis.Pong{} })
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		first := <-opened
		time.Sleep(3 * time.Second)
		synctest.Wait()
		select {
		case <-first.subscription.closed:
		default:
			t.Fatal("Pong traffic hid a missing subscription acknowledgement")
		}
		select {
		case <-opened:
		default:
			t.Fatal("missing acknowledgement did not reconnect")
		}
	})
}

func TestContractOriginNotificationsReconnectOneWayReadBlackhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testTimedContractOriginOpen, 16)
		owner := testTimedContractOriginOwner(t.Context(), opened, nil)
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		first := <-opened
		first.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: first.channel}
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if first.subscription.pings.Load() == 0 {
			t.Fatal("fixture never exercised a successful Ping write")
		}
		select {
		case <-first.subscription.closed:
		default:
			t.Fatal("successful Ping writes hid a blackholed read direction")
		}
		select {
		case <-opened:
		default:
			t.Fatal("missing incoming heartbeat did not reconnect")
		}
	})
}

func TestContractOriginNotificationsKeepHealthyIdleConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testTimedContractOriginOpen, 16)
		owner := testTimedContractOriginOwner(t.Context(), opened, func(string) any { return &redis.Pong{} })
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		connection := <-opened
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()
		update := watch.Update()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if connection.subscription.pings.Load() < 5 {
			t.Fatal("fixture did not exercise repeated idle heartbeats")
		}
		select {
		case <-connection.subscription.closed:
			t.Fatal("healthy idle connection was closed despite received Pongs")
		case <-opened:
			t.Fatal("healthy idle connection was needlessly replaced")
		case <-update:
			t.Fatal("idle heartbeat caused unnecessary PostgreSQL rechecks")
		default:
		}
	})
}

func TestContractOriginNotificationsIncomingMessagesProveLiveness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sourceId, destinationId := server.NewId(), server.NewId()
		key := contractOriginPairHash(sourceId, destinationId)
		opened := make(chan testTimedContractOriginOpen, 16)
		owner := testTimedContractOriginOwner(t.Context(), opened, func(channel string) any {
			return &redis.Message{Channel: channel, Payload: hex.EncodeToString(key[:])}
		})
		defer owner.Close()
		watch := owner.Watch(sourceId, destinationId)
		defer watch.Close()
		connection := <-opened
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()
		update := watch.Update()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		select {
		case <-connection.subscription.closed:
			t.Fatal("valid incoming messages did not prove read-direction liveness")
		case <-opened:
			t.Fatal("live message delivery caused an unnecessary reconnect")
		default:
		}
		select {
		case <-update:
		default:
			t.Fatal("valid heartbeat-window messages failed to notify their pair")
		}
	})
}

func TestContractOriginNotificationsRejectInvalidIncomingHealth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testTimedContractOriginOpen, 16)
		owner := testTimedContractOriginOwner(t.Context(), opened, func(channel string) any {
			return &redis.Message{Channel: channel, Payload: "synthetic-invalid-payload"}
		})
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		connection := <-opened
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		select {
		case <-connection.subscription.closed:
		default:
			t.Fatal("malformed payloads substituted for a live subscription")
		}
	})
}

func TestContractOriginNotificationsPaceFailedSubscriptions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts atomic.Int32
		owner := newContractOriginNotifications(t.Context(), DefaultContractOriginNotificationSettings(),
			func(context.Context, string) (contractOriginSubscription, error) {
				attempts.Add(1)
				return nil, errors.New("synthetic subscription unavailable")
			}, func(context.Context, string, string) error { return nil })
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		synctest.Wait()
		if attempts.Load() != 1 {
			t.Fatalf("initial attempts=%d, want 1", attempts.Load())
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if attempts.Load() != 1 {
			t.Fatalf("retry escaped the reconnect floor: attempts=%d", attempts.Load())
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if attempts.Load() != 2 {
			t.Fatalf("retry did not resume after the reconnect floor: attempts=%d", attempts.Load())
		}
	})
}
