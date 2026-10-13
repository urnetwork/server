package model

// Synthetic subscriptions force acknowledgement, delivery, reconnect and close
// orderings. They exercise the production registry and worker lifecycle without
// Redis, PostgreSQL, sleeps or endpoint identities from a live environment.

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

type testContractOriginSubscription struct {
	events       chan any
	closed       chan struct{}
	closeOnce    sync.Once
	closeBarrier <-chan struct{}
}

func newTestContractOriginSubscription() *testContractOriginSubscription {
	return &testContractOriginSubscription{events: make(chan any, 64), closed: make(chan struct{})}
}

func (self *testContractOriginSubscription) ReceiveTimeout(ctx context.Context, _ time.Duration) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-self.closed:
		return nil, errors.New("synthetic subscription closed")
	case event := <-self.events:
		if err, ok := event.(error); ok {
			return nil, err
		}
		return event, nil
	}
}

func (self *testContractOriginSubscription) Ping(context.Context, ...string) error { return nil }

func (self *testContractOriginSubscription) Close() error {
	self.closeOnce.Do(func() {
		if self.closeBarrier != nil {
			<-self.closeBarrier
		}
		close(self.closed)
	})
	return nil
}

type testContractOriginOpen struct {
	channel      string
	subscription *testContractOriginSubscription
}

func testContractOriginOwner(ctx context.Context, settings ContractOriginNotificationSettings, opened chan<- testContractOriginOpen) *ContractOriginNotifications {
	return newContractOriginNotifications(ctx, settings, func(_ context.Context, channel string) (contractOriginSubscription, error) {
		subscription := newTestContractOriginSubscription()
		opened <- testContractOriginOpen{channel: channel, subscription: subscription}
		return subscription, nil
	}, func(context.Context, string, string) error { return nil })
}

func TestContractOriginNotificationsFanOutAndRefcount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		first, second := owner.Watch(sourceId, destinationId), owner.Watch(sourceId, destinationId)
		defer second.Close()
		firstUpdate, secondUpdate := first.Update(), second.Update()
		synctest.Wait()
		connection := <-opened
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()
		for _, update := range []<-chan struct{}{firstUpdate, secondUpdate} {
			select {
			case <-update:
			default:
				t.Fatal("subscription acknowledgement did not force authoritative recheck")
			}
		}
		first.Close()
		secondUpdate = second.Update()
		key := contractOriginPairHash(sourceId, destinationId)
		connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: hex.EncodeToString(key[:])}
		synctest.Wait()
		select {
		case <-secondUpdate:
		default:
			t.Fatal("closing one waiter removed another")
		}
		second.Close()
		owner.stateLock.Lock()
		remaining := owner.waiters
		pairs := len(owner.buckets[int(key[0])%contractOriginBucketCount].pairs)
		owner.stateLock.Unlock()
		if remaining != 0 || pairs != 0 {
			t.Fatalf("waiter registry retained registrations=%d pairs=%d", remaining, pairs)
		}
	})
}

func TestContractOriginNotificationsBoundReadersAndRegistrations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount*2)
		settings := DefaultContractOriginNotificationSettings()
		settings.MaxWaiters = 256
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		var watches []*ContractOriginWatch
		for range settings.MaxWaiters {
			watches = append(watches, owner.Watch(server.NewId(), server.NewId()))
		}
		if extra := owner.Watch(server.NewId(), server.NewId()); extra != nil {
			t.Fatal("registration budget exceeded")
		}
		synctest.Wait()
		connections := 0
		for {
			select {
			case <-opened:
				connections++
			default:
				goto counted
			}
		}
	counted:
		if connections == 0 || connections > contractOriginBucketCount {
			t.Fatalf("opened %d readers for 256 waiters", connections)
		}
		for _, watch := range watches {
			watch.Close()
		}
		owner.Close()
		if owner.Watch(server.NewId(), server.NewId()) != nil {
			t.Fatal("closed owner admitted a waiter")
		}
	})
}

func TestContractOriginNotificationsRecoverLostEpochOnReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		synctest.Wait()
		first := <-opened
		first.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: first.channel}
		synctest.Wait()
		update := watch.Update()
		first.subscription.events <- errors.New("synthetic connection lost")
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		second := <-opened
		second.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: second.channel}
		synctest.Wait()
		select {
		case <-update:
		default:
			t.Fatal("reconnect lost the resync for a possibly missed commit")
		}
	})
}

func TestContractOriginNotificationsCloseJoinsOwnedReaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		synctest.Wait()
		connection := <-opened
		closeBarrier := make(chan struct{})
		connection.subscription.closeBarrier = closeBarrier
		returned := make(chan struct{})
		go func() { owner.Close(); close(returned) }()
		synctest.Wait()
		select {
		case <-returned:
			t.Fatal("owner Close returned before reader socket close joined")
		default:
		}
		close(closeBarrier)
		synctest.Wait()
		<-returned
		select {
		case <-connection.subscription.closed:
		default:
			t.Fatal("subscription remained open after Close")
		}
	})
}

func TestContractOriginNotificationsPublishQueueIsNonblockingAndOwned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := ContractOriginNotificationSettings{MaxWaiters: 1, PublishQueueSize: 1, PublishWorkers: 1}
		entered := make(chan struct{})
		var publishes atomic.Int32
		owner := newContractOriginNotifications(t.Context(), settings,
			func(context.Context, string) (contractOriginSubscription, error) { return nil, errors.New("unused") },
			func(ctx context.Context, channel string, payload string) error {
				publishes.Add(1)
				if len(payload) != 64 {
					t.Errorf("unbounded/nonhashed payload length %d", len(payload))
				}
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
		ctx := WithContractOriginNotifications(context.Background(), owner)
		sourceId, destinationId := server.NewId(), server.NewId()
		notifyCommittedContractOrigin(ctx, sourceId, destinationId)
		<-entered
		// One queued item fits, all later notifications are optional drops.
		for range 100 {
			notifyCommittedContractOrigin(ctx, sourceId, destinationId)
		}
		owner.Close()
		if publishes.Load() != 1 {
			t.Fatalf("closed owner started additional publications: %d", publishes.Load())
		}
	})
}

func TestContractOriginNotificationsSeparateOwnersAndDirections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		openedA, openedB := make(chan testContractOriginOpen, 16), make(chan testContractOriginOpen, 16)
		first := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), openedA)
		second := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), openedB)
		defer second.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watchA, watchB := first.Watch(sourceId, destinationId), second.Watch(sourceId, destinationId)
		defer watchA.Close()
		defer watchB.Close()
		forward, reverse := contractOriginPairHash(sourceId, destinationId), contractOriginPairHash(destinationId, sourceId)
		if forward == reverse {
			t.Fatal("origin directions collapsed")
		}
		synctest.Wait()
		connectionB := <-openedB
		update := watchB.Update()
		first.Close()
		connectionB.subscription.events <- &redis.Message{Channel: connectionB.channel, Payload: hex.EncodeToString(forward[:])}
		synctest.Wait()
		select {
		case <-update:
		default:
			t.Fatal("closing independent owner stopped the remaining owner")
		}
	})
}

func TestContractOriginNotificationsRejectUnrelatedAndMalformedPayloads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, 16)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		watch := owner.Watch(server.NewId(), server.NewId())
		defer watch.Close()
		synctest.Wait()
		connection := <-opened
		update := watch.Update()
		connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: "synthetic-invalid-payload"}
		key := contractOriginPairHash(server.NewId(), server.NewId())
		connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: hex.EncodeToString(key[:])}
		synctest.Wait()
		select {
		case <-update:
			t.Fatal("invalid or unrelated pair woke this waiter")
		default:
		}
	})
}

func TestContractOriginNotificationsCommitOutlivesRequestCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if GetContractOriginNotifications(context.Background()) != nil {
			t.Fatal("bare context unexpectedly acquired a singleton")
		}
		published := make(chan error, 1)
		owner := newContractOriginNotifications(t.Context(), DefaultContractOriginNotificationSettings(),
			func(context.Context, string) (contractOriginSubscription, error) {
				return nil, errors.New("unused reader")
			},
			func(ctx context.Context, _ string, _ string) error { published <- ctx.Err(); return nil })
		defer owner.Close()
		ctx, cancel := context.WithCancel(WithContractOriginNotifications(context.Background(), owner))
		cancel()
		if GetContractOriginNotifications(ctx) != owner {
			t.Fatal("request lost its explicitly supplied owner")
		}
		notifyCommittedContractOrigin(ctx, server.NewId(), server.NewId())
		if err := <-published; err != nil {
			t.Fatalf("committed origin inherited caller cancellation: %v", err)
		}
	})
}
