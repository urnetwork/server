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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

type testContractOriginSubscription struct {
	events       chan any
	closed       chan struct{}
	closeOnce    sync.Once
	closeBarrier <-chan struct{}
	// the bucket channel it was opened for, and the pair channels it carries
	stateLock    sync.Mutex
	channel      string
	pairChannels map[string]bool
}

func newTestContractOriginSubscription() *testContractOriginSubscription {
	return &testContractOriginSubscription{events: make(chan any, 64), closed: make(chan struct{}), pairChannels: map[string]bool{}}
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

// A re-assertion of the bucket channel is never acknowledged here, so an
// unexpected one would replace the socket. A pair channel is acknowledged at
// once with the socket's shard channel count, as a node does.
func (self *testContractOriginSubscription) SSubscribe(_ context.Context, channels ...string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, channel := range channels {
		if channel == self.channel {
			continue
		}
		self.pairChannels[channel] = true
		self.events <- &redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: 1 + len(self.pairChannels)}
	}
	return nil
}

func (self *testContractOriginSubscription) SUnsubscribe(_ context.Context, channels ...string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, channel := range channels {
		delete(self.pairChannels, channel)
		self.events <- &redis.Subscription{Kind: "sunsubscribe", Channel: channel, Count: 1 + len(self.pairChannels)}
	}
	return nil
}

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
		subscription.channel = channel
		// a test takes the sockets it acts on; later reopens are not waited on
		select {
		case opened <- testContractOriginOpen{channel: channel, subscription: subscription}:
		default:
		}
		return subscription, nil
	}, func(context.Context, []contractOriginPublication) error { return nil })
}

func TestContractOriginNotificationsFanOutAndRefcount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		first, second := owner.Watch(sourceId, destinationId), owner.Watch(sourceId, destinationId)
		defer second.Close()
		firstUpdate, _ := first.Update()
		secondUpdate, _ := second.Update()
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
		secondUpdate, _ = second.Update()
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
		update, _ := watch.Update()
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
		settings := DefaultContractOriginNotificationSettings()
		settings.MaxWaiters, settings.PublishQueueSize, settings.PublishWorkers = 1, 1, 1
		entered := make(chan struct{})
		var publishes atomic.Int32
		owner := newContractOriginNotifications(t.Context(), settings,
			func(context.Context, string) (contractOriginSubscription, error) { return nil, errors.New("unused") },
			func(ctx context.Context, publications []contractOriginPublication) error {
				publishes.Add(1)
				// the bucket channel's hashed digest, and the pair channel's empty edge
				if len(publications) != 2 || len(publications[0].payload) != 64 || publications[1].payload != "" {
					t.Errorf("unbounded/nonhashed publications %+v", publications)
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
		update, _ := watchB.Update()
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
		update, _ := watch.Update()
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
			func(ctx context.Context, _ []contractOriginPublication) error { published <- ctx.Err(); return nil })
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

// Returns a pair in the bucket.
func testContractOriginPairInBucket(bucket int) (sourceId server.Id, destinationId server.Id) {
	for {
		sourceId, destinationId = server.NewId(), server.NewId()
		key := contractOriginPairHash(sourceId, destinationId)
		if int(key[0])%contractOriginBucketCount == bucket {
			return
		}
	}
}

// The deltas of what received edges found, for one test.
type testContractOriginMatches struct {
	matchedWaiter  float64
	matchedRefusal float64
	unmatched      float64
}

func testContractOriginMatchCounts() testContractOriginMatches {
	return testContractOriginMatches{
		matchedWaiter:  testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("matched_waiter")),
		matchedRefusal: testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("matched_refusal")),
		unmatched:      testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("unmatched")),
	}
}

// Stage 0 of CPUOPT7-ORIGIN-DELIVERY: each received edge counts what it found
// here, exactly once per class. A watched pair is matched_waiter, a removed
// refusal matched_refusal, both count both, and an edge that finds neither,
// including one whose refusal is already gone or expired, is unmatched. A
// resync is not an edge and counts nothing.
func TestContractOriginNotificationsCountWhatEachEdgeFinds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		settings := DefaultContractOriginNotificationSettings()
		owner := testContractOriginOwner(t.Context(), settings, opened)
		defer owner.Close()
		watchedSourceId, watchedDestinationId := testContractOriginPairInBucket(3)
		watch := owner.Watch(watchedSourceId, watchedDestinationId)
		defer watch.Close()
		synctest.Wait()
		connection := <-opened
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel}
		synctest.Wait()

		// a route refused here, whose reverse pair is in the same bucket
		refusedOriginId, refusedRouteSourceId := testContractOriginPairInBucket(3)
		missing := ContractRouteRejection{Cause: ContractRouteMissingOrigin, ResolutionPath: "stream_fallback"}
		// a watched pair whose reverse route is refused too
		bothSourceId, bothDestinationId := testContractOriginPairInBucket(3)
		bothWatch := owner.Watch(bothSourceId, bothDestinationId)
		defer bothWatch.Close()
		unwatchedSourceId, unwatchedDestinationId := testContractOriginPairInBucket(3)
		for _, route := range [][2]server.Id{
			{refusedRouteSourceId, refusedOriginId},
			{bothDestinationId, bothSourceId},
		} {
			if !owner.RecordRouteRejection(route[0], route[1], true, missing, nil) {
				t.Fatal("refusal was not stored")
			}
		}
		edge := func(sourceId server.Id, destinationId server.Id) testContractOriginMatches {
			before := testContractOriginMatchCounts()
			key := contractOriginPairHash(sourceId, destinationId)
			connection.subscription.events <- &redis.Message{Channel: connection.channel, Payload: hex.EncodeToString(key[:])}
			synctest.Wait()
			after := testContractOriginMatchCounts()
			return testContractOriginMatches{
				matchedWaiter:  after.matchedWaiter - before.matchedWaiter,
				matchedRefusal: after.matchedRefusal - before.matchedRefusal,
				unmatched:      after.unmatched - before.unmatched,
			}
		}
		for _, check := range []struct {
			name          string
			sourceId      server.Id
			destinationId server.Id
			want          testContractOriginMatches
		}{
			{name: "watched pair", sourceId: watchedSourceId, destinationId: watchedDestinationId, want: testContractOriginMatches{matchedWaiter: 1}},
			{name: "refused route", sourceId: refusedOriginId, destinationId: refusedRouteSourceId, want: testContractOriginMatches{matchedRefusal: 1}},
			{name: "refused route again", sourceId: refusedOriginId, destinationId: refusedRouteSourceId, want: testContractOriginMatches{unmatched: 1}},
			{name: "watched and refused", sourceId: bothSourceId, destinationId: bothDestinationId, want: testContractOriginMatches{matchedWaiter: 1, matchedRefusal: 1}},
			{name: "unwatched pair", sourceId: unwatchedSourceId, destinationId: unwatchedDestinationId, want: testContractOriginMatches{unmatched: 1}},
		} {
			if got := edge(check.sourceId, check.destinationId); got != check.want {
				t.Fatalf("%s: counted %+v, want %+v", check.name, got, check.want)
			}
		}
		// a lost socket's replacement resyncs the bucket
		resubscribedBefore := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("reconnected"))
		before := testContractOriginMatchCounts()
		connection.subscription.events <- errors.New("synthetic connection lost")
		synctest.Wait()
		time.Sleep(settings.SubscriptionReconnectTimeout)
		synctest.Wait()
		connection = <-opened
		connection.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: connection.channel, Count: 1}
		synctest.Wait()
		if testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("reconnected")) != resubscribedBefore+1 {
			t.Fatal("the fixture's replacement did not resync the bucket")
		}
		if after := testContractOriginMatchCounts(); after != before {
			t.Fatalf("a resync counted %+v, then %+v", before, after)
		}

		// a refusal that expires before its edge arrives
		expiredOriginId, expiredRouteSourceId := testContractOriginPairInBucket(3)
		if !owner.RecordRouteRejection(expiredRouteSourceId, expiredOriginId, true, missing, nil) {
			t.Fatal("refusal was not stored")
		}
		time.Sleep(settings.RouteRejectionTimeout)
		if got := edge(expiredOriginId, expiredRouteSourceId); got != (testContractOriginMatches{unmatched: 1}) {
			t.Fatalf("expired refusal: counted %+v, want unmatched only", got)
		}
	})
}
