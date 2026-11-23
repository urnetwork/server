package model

// Build B of CPUOPT7-ORIGIN-DELIVERY.md through the in-memory shard broker
// (internal/redisshardtest): which owners an origin's edges reach, how held
// pairs follow their sockets, what replaces a socket, the fence of a stored
// refusal, the balance of holds and subscriptions, mixed publisher and owner
// versions, and idle buckets. Every test runs in a testing/synctest bubble:
// the broker's deliveries, the sockets' timeouts and the owners' timers all
// use its clock, and synctest.Wait is the barrier. No Redis, PostgreSQL or
// wall-clock wait is involved, except in the benchmark.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/redisshardtest"
)

// The sockets one owner opened on the broker, in order.
type testDeliverySockets struct {
	stateLock sync.Mutex
	sockets   []*redisshardtest.Socket
}

func (self *testDeliverySockets) add(socket *redisshardtest.Socket) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sockets = append(self.sockets, socket)
}

func (self *testDeliverySockets) all() []*redisshardtest.Socket {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*redisshardtest.Socket(nil), self.sockets...)
}

// The bucket's open socket, or nil.
func (self *testDeliverySockets) live(bucket int) *redisshardtest.Socket {
	var live *redisshardtest.Socket
	for _, socket := range self.all() {
		if socket.AnchorChannel() == contractOriginChannel(bucket) && !socket.Closed() {
			live = socket
		}
	}
	return live
}

// The messages delivered to all of the owner's sockets.
func (self *testDeliverySockets) deliveryCount() int {
	count := 0
	for _, socket := range self.all() {
		count += socket.DeliveryCount()
	}
	return count
}

func testDeliveryPublish(broker *redisshardtest.Broker, publications []contractOriginPublication) {
	for _, publication := range publications {
		broker.Publish(publication.channel, publication.payload)
	}
}

// An owner on the broker, as an api process's is on Redis: its sockets are
// the broker's, and its publications go out through the broker. wrap, when
// set, stands between the owner and each socket.
func testDeliveryOwner(
	ctx context.Context,
	broker *redisshardtest.Broker,
	settings ContractOriginNotificationSettings,
	wrap func(*redisshardtest.Socket) contractOriginSubscription,
) (*ContractOriginNotifications, *testDeliverySockets) {
	sockets := &testDeliverySockets{}
	owner := newContractOriginNotifications(ctx, settings,
		func(ctx context.Context, channel string) (contractOriginSubscription, error) {
			socket, err := broker.Open(ctx, channel)
			if err != nil {
				return nil, err
			}
			sockets.add(socket)
			if wrap != nil {
				return wrap(socket), nil
			}
			return socket, nil
		},
		func(_ context.Context, publications []contractOriginPublication) error {
			testDeliveryPublish(broker, publications)
			return nil
		})
	return owner, sockets
}

// The bucket channel of Build A and of the builds before it, which Build B
// neither publishes nor subscribes: every edge of the bucket, as the pair's
// digest in hex. Build B's bucket channel (`contractOriginChannel`) is its
// sockets' anchor, which nobody publishes to.
func testDeliveryBuildAChannel(bucket int) string {
	return fmt.Sprintf("contract-origin:v1:{%02x}", bucket)
}

// Publishes an origin committed source → destination as each version's
// publisher does: before Build A ("old") the Build A bucket channel's digest
// only, Build A that and the pair channel in one pipeline, and Build B the
// pair channel only (`contractOriginPublications`).
func testDeliveryPublishOrigin(broker *redisshardtest.Broker, version string, sourceId server.Id, destinationId server.Id) {
	key := contractOriginPairHash(sourceId, destinationId)
	bucketChannelPublication := contractOriginPublication{
		channel: testDeliveryBuildAChannel(contractOriginBucketIndex(key)),
		payload: hex.EncodeToString(key[:]),
	}
	var publications []contractOriginPublication
	switch version {
	case "old":
		publications = []contractOriginPublication{bucketChannelPublication}
	case "A":
		publications = append([]contractOriginPublication{bucketChannelPublication}, contractOriginPublications(key)...)
	default:
		publications = contractOriginPublications(key)
	}
	testDeliveryPublish(broker, publications)
}

// A wait whose first read missed: it holds the pair, and once the pair's
// subscription is acknowledged its reads are fenced.
func testDeliveryHoldByMiss(t *testing.T, owner *ContractOriginNotifications, sourceId server.Id, destinationId server.Id) *ContractOriginWatch {
	t.Helper()
	watch := owner.Watch(sourceId, destinationId)
	if watch == nil {
		t.Fatal("the owner declined the watch")
	}
	if _, fenced := watch.Update(); fenced {
		t.Fatal("a read of a pair nobody held was fenced")
	}
	registration, held := watch.Missed()
	if !held {
		t.Fatal("the miss was not held")
	}
	synctest.Wait()
	select {
	case <-registration:
	default:
		t.Fatal("the held pair's subscription was not acknowledged")
	}
	if _, fenced := watch.Update(); !fenced {
		t.Fatal("a read armed after the pair's acknowledgement was not fenced")
	}
	return watch
}

func testDeliveryClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func testDeliveryMissingOrigin() ContractRouteRejection {
	return ContractRouteRejection{Cause: ContractRouteMissingOrigin, ResolutionPath: "stream_fallback"}
}

// Root cause of the broadcast (§1.3): every owner that ever waited in a bucket
// received every origin of that bucket. Three owners share one broker; one
// waits on P past a miss and holds it, the other two waited once in P's bucket
// and in another and stopped. Once their sockets idle out, of 1,000 origins
// for P and for other pairs only P's reach the holder, on P's channel, and
// nothing else reaches anyone: Build B publishes on pair channels alone, and
// nobody publishes to a socket's anchor. Before Build A every started bucket
// stayed subscribed for the owner's life and received every one.
func TestContractOriginEdgeReachesOnlyHoldingOwners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		holder, holderSockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer holder.Close()
		first, firstSockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer first.Close()
		second, secondSockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer second.Close()

		const heldBucket, otherBucket = 5, 9
		heldSourceId, heldDestinationId := testContractOriginPairInBucket(heldBucket)
		heldKey := contractOriginPairHash(heldSourceId, heldDestinationId)
		holding := testDeliveryHoldByMiss(t, holder, heldSourceId, heldDestinationId)
		defer holding.Close()

		// the other owners' waits are decided by their first read and end
		for _, owner := range []*ContractOriginNotifications{first, second} {
			for _, bucket := range []int{heldBucket, otherBucket} {
				watch := owner.Watch(testContractOriginPairInBucket(bucket))
				_, _ = watch.Update()
				synctest.Wait()
				watch.Close()
			}
		}
		synctest.Wait()
		for _, sockets := range []*testDeliverySockets{firstSockets, secondSockets} {
			if sockets.live(heldBucket) == nil || sockets.live(otherBucket) == nil {
				t.Fatal("a wait did not open its bucket's socket")
			}
		}
		time.Sleep(settings.IdleBucketTimeout)
		synctest.Wait()
		for _, sockets := range []*testDeliverySockets{firstSockets, secondSockets} {
			for _, socket := range sockets.all() {
				if !socket.Closed() {
					t.Fatalf("an owner with no watch and no held pair kept its socket for %s past the idle timeout", socket.AnchorChannel())
				}
			}
		}
		holderSocket := holderSockets.live(heldBucket)
		if holderSocket == nil || !holderSocket.Subscribed(contractOriginPairChannel(heldKey)) {
			t.Fatal("the holder's socket does not carry the held pair's channel")
		}

		update, _ := holding.Update()
		idleDeliveries := firstSockets.deliveryCount() + secondSockets.deliveryCount()
		heldPairChannel := contractOriginPairChannel(heldKey)
		heldBefore := holderSocket.ChannelDeliveryCount(heldPairChannel)
		holderBefore := holderSockets.deliveryCount()
		heldPublishCount := 0
		var otherPairChannels []string
		for i := range 1000 {
			if i%10 == 0 {
				testDeliveryPublishOrigin(broker, "B", heldSourceId, heldDestinationId)
				heldPublishCount += 1
				continue
			}
			bucket := heldBucket
			if i%2 == 1 {
				bucket = i % contractOriginBucketCount
			}
			sourceId, destinationId := testContractOriginPairInBucket(bucket)
			testDeliveryPublishOrigin(broker, "B", sourceId, destinationId)
			otherPairChannels = append(otherPairChannels, contractOriginPairChannel(contractOriginPairHash(sourceId, destinationId)))
		}
		synctest.Wait()

		if delivered := holderSocket.ChannelDeliveryCount(heldPairChannel) - heldBefore; delivered != heldPublishCount {
			t.Fatalf("the held pair's channel delivered %d of %d edges to its holder", delivered, heldPublishCount)
		}
		otherPairDeliveries := 0
		for _, sockets := range []*testDeliverySockets{holderSockets, firstSockets, secondSockets} {
			for _, socket := range sockets.all() {
				for _, channel := range otherPairChannels {
					otherPairDeliveries += socket.ChannelDeliveryCount(channel)
				}
			}
		}
		if otherPairDeliveries != 0 {
			t.Fatalf("pairs nobody held delivered %d pair channel edges", otherPairDeliveries)
		}
		if after := firstSockets.deliveryCount() + secondSockets.deliveryCount(); after != idleDeliveries {
			t.Fatalf("owners with no watch and no held pair received %d edges", after-idleDeliveries)
		}
		// nothing reaches the holder beyond P's channel, its anchor included
		if delivered := holderSockets.deliveryCount() - holderBefore; delivered != heldPublishCount {
			t.Fatalf("the holder received %d edges, want only P's %d", delivered, heldPublishCount)
		}
		if delivered := holderSocket.ChannelDeliveryCount(contractOriginChannel(heldBucket)); delivered != 0 {
			t.Fatalf("the holder's anchor delivered %d edges; nobody publishes to it", delivered)
		}
		if !testDeliveryClosed(update) {
			t.Fatal("the held pair's edges did not wake its waiter")
		}
	})
}

// A lost socket's replacement subscribes every pair still held, in one
// command, and leaves alone a pair that is only watched. Build B has no resync
// of the whole bucket: nothing is published to the anchor, so the anchor's
// acknowledgement changes nothing, and each pair acknowledged on the lost
// socket resyncs at its own acknowledgement on the replacement, waking its
// waiters and removing its refusals. A pair held only by a refusal therefore
// resubscribes, resyncs, and, its refusal gone, is released.
func TestContractOriginSocketReplacementResubscribesHeldPairs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		const bucket = 6
		firstSourceId, firstDestinationId := testContractOriginPairInBucket(bucket)
		firstKey := contractOriginPairHash(firstSourceId, firstDestinationId)
		secondSourceId, secondDestinationId := testContractOriginPairInBucket(bucket)
		secondKey := contractOriginPairHash(secondSourceId, secondDestinationId)
		firstWatch := testDeliveryHoldByMiss(t, owner, firstSourceId, firstDestinationId)
		defer firstWatch.Close()
		secondWatch := testDeliveryHoldByMiss(t, owner, secondSourceId, secondDestinationId)
		defer secondWatch.Close()
		// a route refused here, whose reverse pair only the refusal holds
		refusedOriginId, refusedRouteSourceId := testContractOriginPairInBucket(bucket)
		refusedKey := contractOriginPairHash(refusedOriginId, refusedRouteSourceId)
		if !owner.RecordRouteRejection(refusedRouteSourceId, refusedOriginId, true, testDeliveryMissingOrigin(), nil) {
			t.Fatal("refusal was not stored")
		}
		watchedSourceId, watchedDestinationId := testContractOriginPairInBucket(bucket)
		watchedKey := contractOriginPairHash(watchedSourceId, watchedDestinationId)
		watched := owner.Watch(watchedSourceId, watchedDestinationId)
		defer watched.Close()
		synctest.Wait()

		lostSocket := sockets.live(bucket)
		for _, key := range [][32]byte{firstKey, secondKey, refusedKey} {
			if !lostSocket.Subscribed(contractOriginPairChannel(key)) {
				t.Fatal("a held pair's channel is not subscribed")
			}
		}
		if lostSocket.Subscribed(contractOriginPairChannel(watchedKey)) {
			t.Fatal("a pair that is only watched was subscribed")
		}
		firstUpdate, _ := firstWatch.Update()
		secondUpdate, _ := secondWatch.Update()
		subscribeCommandsBefore := broker.SubscribeCommandCount()
		unsubscribeCommandsBefore := broker.UnsubscribeCommandCount()
		subscribedPairsBefore := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("subscribed_pair"))
		releasedPairsBefore := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("pair_released"))
		broker.Inject(lostSocket, errors.New("synthetic connection lost"))
		synctest.Wait()
		if !lostSocket.Closed() {
			t.Fatal("a read error kept the socket")
		}
		if sockets.live(bucket) != nil {
			t.Fatal("the socket was replaced before the reconnect pause")
		}
		time.Sleep(settings.SubscriptionReconnectTimeout)
		synctest.Wait()

		replacement := sockets.live(bucket)
		if replacement == nil {
			t.Fatal("the lost socket was never replaced")
		}
		if !testDeliveryClosed(firstUpdate) || !testDeliveryClosed(secondUpdate) {
			t.Fatal("the held pairs' acknowledgements did not wake their waiters")
		}
		if _, ok := owner.RouteRejection(refusedRouteSourceId, refusedOriginId, true); ok {
			t.Fatal("a refusal that may have missed an edge survived its pair's resubscription")
		}
		// the open plus one SSUBSCRIBE of the three held pairs
		if commands := broker.SubscribeCommandCount() - subscribeCommandsBefore; commands != 2 {
			t.Fatalf("the replacement sent %d subscribe commands, want the open and one for the held pairs", commands)
		}
		if subscribed := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("subscribed_pair")) - subscribedPairsBefore; subscribed != 3 {
			t.Fatalf("the replacement subscribed %v pairs, want the three held", subscribed)
		}
		for _, key := range [][32]byte{firstKey, secondKey} {
			if !replacement.Subscribed(contractOriginPairChannel(key)) {
				t.Fatal("a held pair did not resubscribe on the replacement")
			}
		}
		// the refusal's pair, released once its resync removed the refusal
		if released := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("pair_released")) - releasedPairsBefore; released != 1 {
			t.Fatalf("%v pairs were released after the replacement, want the refusal's", released)
		}
		if commands := broker.UnsubscribeCommandCount() - unsubscribeCommandsBefore; commands != 1 {
			t.Fatalf("the replacement sent %d unsubscribe commands, want one for the refusal's pair", commands)
		}
		for _, key := range [][32]byte{refusedKey, watchedKey} {
			if replacement.Subscribed(contractOriginPairChannel(key)) {
				t.Fatal("a pair nobody holds was subscribed on the replacement")
			}
		}
		if _, fenced := firstWatch.Update(); !fenced {
			t.Fatal("the resubscribed pair's acknowledgement did not fence its reads again")
		}

		// The pair's own acknowledgement: withheld until a refusal is stored
		// for it on the replacement, then delivered. It is the only held
		// pair, so no other acknowledgement's count includes it first.
		secondWatch.Close()
		firstPairChannel := contractOriginPairChannel(firstKey)
		broker.SetWithholdAcknowledgement(func(channel string) bool { return channel == firstPairChannel })
		broker.Inject(replacement, errors.New("synthetic connection lost"))
		synctest.Wait()
		time.Sleep(settings.SubscriptionReconnectTimeout)
		synctest.Wait()
		third := sockets.live(bucket)
		if third == nil || !third.Subscribed(firstPairChannel) {
			t.Fatal("the held pair did not resubscribe on the third socket")
		}
		firstUpdate, fenced := firstWatch.Update()
		if fenced {
			t.Fatal("a read was fenced before its pair's acknowledgement")
		}
		if !owner.RecordRouteRejection(firstDestinationId, firstSourceId, false, testDeliveryMissingOrigin(), nil) {
			t.Fatal("refusal was not stored")
		}
		broker.Inject(third, &redis.Subscription{Kind: "ssubscribe", Channel: firstPairChannel, Count: len(third.Channels())})
		synctest.Wait()
		broker.SetWithholdAcknowledgement(nil)
		if !testDeliveryClosed(firstUpdate) {
			t.Fatal("the pair's resubscription did not wake its waiter")
		}
		if _, ok := owner.RouteRejection(firstDestinationId, firstSourceId, false); ok {
			t.Fatal("the pair's resubscription kept a refusal that may have missed an edge")
		}
		if _, fenced := firstWatch.Update(); !fenced {
			t.Fatal("the pair's acknowledgement did not fence its reads")
		}
	})
}

// A socket whose pair channels can no longer be trusted is replaced: the old
// one closes at once, a new one opens after the reconnect pause, and every
// held pair resubscribes and is acknowledged there.
func testDeliveryRequireReplaced(
	t *testing.T,
	settings ContractOriginNotificationSettings,
	sockets *testDeliverySockets,
	bucket int,
	replaced *redisshardtest.Socket,
	heldWatches map[[32]byte]*ContractOriginWatch,
) {
	t.Helper()
	synctest.Wait()
	if !replaced.Closed() {
		t.Fatal("the socket was kept")
	}
	if sockets.live(bucket) != nil {
		t.Fatal("the socket was replaced before the reconnect pause")
	}
	time.Sleep(settings.SubscriptionReconnectTimeout)
	synctest.Wait()
	replacement := sockets.live(bucket)
	if replacement == nil {
		t.Fatal("the socket was never replaced")
	}
	for key, watch := range heldWatches {
		if !replacement.Subscribed(contractOriginPairChannel(key)) {
			t.Fatal("a held pair did not resubscribe on the replacement")
		}
		if _, fenced := watch.Update(); !fenced {
			t.Fatal("a held pair was not acknowledged on the replacement")
		}
	}
}

// A slot move can drop a pair channel with a sunsubscribe notice that nothing
// asked for, while the socket's anchor stays: the socket is replaced.
func TestContractOriginUnrequestedSunsubscribeReplacesSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		const bucket = 2
		sourceId, destinationId := testContractOriginPairInBucket(bucket)
		key := contractOriginPairHash(sourceId, destinationId)
		watch := testDeliveryHoldByMiss(t, owner, sourceId, destinationId)
		defer watch.Close()
		socket := sockets.live(bucket)
		broker.Migrate(contractOriginPairChannel(key))
		testDeliveryRequireReplaced(t, settings, sockets, bucket, socket, map[[32]byte]*ContractOriginWatch{key: watch})
	})
}

// A node that drops a pair channel without notice is found by the next
// notice's shard channel count, which no longer matches the acknowledged
// channels: the socket is replaced.
func TestContractOriginChannelCountMismatchReplacesSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		const bucket = 3
		firstSourceId, firstDestinationId := testContractOriginPairInBucket(bucket)
		firstKey := contractOriginPairHash(firstSourceId, firstDestinationId)
		firstWatch := testDeliveryHoldByMiss(t, owner, firstSourceId, firstDestinationId)
		defer firstWatch.Close()
		socket := sockets.live(bucket)
		broker.DropSilently(socket, contractOriginPairChannel(firstKey))
		synctest.Wait()
		if socket.Closed() {
			t.Fatal("a silent drop was seen before any notice")
		}
		secondSourceId, secondDestinationId := testContractOriginPairInBucket(bucket)
		secondKey := contractOriginPairHash(secondSourceId, secondDestinationId)
		secondWatch := owner.Watch(secondSourceId, secondDestinationId)
		defer secondWatch.Close()
		_, _ = secondWatch.Update()
		if _, held := secondWatch.Missed(); !held {
			t.Fatal("the miss was not held")
		}
		testDeliveryRequireReplaced(t, settings, sockets, bucket, socket, map[[32]byte]*ContractOriginWatch{firstKey: firstWatch, secondKey: secondWatch})
	})
}

// An error reply, such as a MOVED for a slot this node gave up, replaces the
// socket.
func TestContractOriginErrorReplyReplacesSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		const bucket = 4
		sourceId, destinationId := testContractOriginPairInBucket(bucket)
		key := contractOriginPairHash(sourceId, destinationId)
		watch := testDeliveryHoldByMiss(t, owner, sourceId, destinationId)
		defer watch.Close()
		socket := sockets.live(bucket)
		broker.Inject(socket, errors.New("MOVED 1730 192.0.2.10:6379"))
		testDeliveryRequireReplaced(t, settings, sockets, bucket, socket, map[[32]byte]*ContractOriginWatch{key: watch})
	})
}

// A pair subscription whose acknowledgement does not come within the
// acknowledgement timeout replaces the socket, as the anchor's would;
// the held pair resubscribes on the replacement.
func TestContractOriginOverduePairAcknowledgementReplacesSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		const bucket = 7
		sourceId, destinationId := testContractOriginPairInBucket(bucket)
		key := contractOriginPairHash(sourceId, destinationId)
		pairChannel := contractOriginPairChannel(key)
		broker.SetWithholdAcknowledgement(func(channel string) bool { return channel == pairChannel })
		watch := owner.Watch(sourceId, destinationId)
		defer watch.Close()
		_, _ = watch.Update()
		registration, held := watch.Missed()
		if !held {
			t.Fatal("the miss was not held")
		}
		synctest.Wait()
		socket := sockets.live(bucket)
		if socket == nil || !socket.Subscribed(pairChannel) {
			t.Fatal("the held pair was not subscribed")
		}
		failedBefore := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("subscribe_failed"))
		time.Sleep(settings.SubscriptionAcknowledgementTimeout - time.Millisecond)
		synctest.Wait()
		if socket.Closed() {
			t.Fatal("the socket was replaced before the acknowledgement timeout")
		}
		if testDeliveryClosed(registration) {
			t.Fatal("a withheld acknowledgement registered the pair")
		}
		time.Sleep(time.Millisecond)
		broker.SetWithholdAcknowledgement(nil)
		testDeliveryRequireReplaced(t, settings, sockets, bucket, socket, map[[32]byte]*ContractOriginWatch{key: watch})
		if failed := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("subscribe_failed")) - failedBefore; failed != 1 {
			t.Fatalf("an overdue acknowledgement counted %v subscribe failures, want 1", failed)
		}
		if !testDeliveryClosed(registration) {
			t.Fatal("the replacement's acknowledgement did not register the waiting pair")
		}
	})
}

// The fence of a missing-origin verdict read while its pair was acknowledged:
// in every order of the read's arm, the origin's commit and publication, a
// lost socket and its replacement, and the verdict's put, no refusal stays
// stored once its origin committed after the read. The put refuses (the
// edge, or the pair's resync at its acknowledgement on the replacement,
// closed the fence) or the edge, or that resync, removes what it stored.
// Build A's and Build B's publishers each suffice; one from before Build A
// reaches no Build B owner, which is why Build B runs only once none is left.
// A read armed before the pair's acknowledgement is not fenced, and the
// controller stores no missing-origin verdict from it.
func TestContractRouteRejectionFenceArmedAfterAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		orders := [][]string{
			{"arm", "edge", "put"},
			{"arm", "put", "edge"},
			// the read would have seen this origin; storing is not stale
			{"edge", "arm", "put"},
			{"arm", "lose", "edge", "put", "resubscribe"},
			{"arm", "lose", "edge", "resubscribe", "put"},
		}
		caseIndex := 0
		for _, version := range []string{"A", "B"} {
			for _, order := range orders {
				bucket := caseIndex % contractOriginBucketCount
				caseIndex += 1
				originSourceId, originDestinationId := testContractOriginPairInBucket(bucket)
				// the route a companion would ride this origin back on
				routeSourceId, routeDestinationId := originDestinationId, originSourceId
				// holds the pair from its first miss, and fences its next read
				watch := testDeliveryHoldByMiss(t, owner, originSourceId, originDestinationId)
				var fence <-chan struct{}
				armed := false
				edgeAfterArm := false
				for _, step := range order {
					switch step {
					case "arm":
						var fenced bool
						fence, fenced = watch.Update()
						if !fenced {
							t.Fatalf("%s %v: a read armed after the acknowledgement was not fenced", version, order)
						}
						armed = true
					case "edge":
						edgeAfterArm = armed
						testDeliveryPublishOrigin(broker, version, originSourceId, originDestinationId)
						synctest.Wait()
					case "put":
						owner.RecordRouteRejection(routeSourceId, routeDestinationId, true, testDeliveryMissingOrigin(), fence)
					case "lose":
						broker.Inject(sockets.live(bucket), errors.New("synthetic connection lost"))
						synctest.Wait()
					case "resubscribe":
						time.Sleep(settings.SubscriptionReconnectTimeout)
						synctest.Wait()
					}
				}
				synctest.Wait()
				_, stored := owner.RouteRejection(routeSourceId, routeDestinationId, true)
				if edgeAfterArm && stored {
					t.Fatalf("%s %v: a refusal read before its origin committed stayed stored", version, order)
				}
				if !edgeAfterArm && !stored {
					t.Fatalf("%s %v: a refusal read after its origin's edge was not stored", version, order)
				}
				watch.Close()
			}
		}
	})
}

// A read armed while its pair was acknowledged holds the pair until its watch
// closes, so the pair stays subscribed through the read's verdict even when
// the holder that subscribed it lets go first: a Build B publisher's edge, on
// the pair channel alone, still closes the verdict's fence.
func TestContractOriginFencedReadHoldsItsPair(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		const bucket = 8
		originSourceId, originDestinationId := testContractOriginPairInBucket(bucket)
		key := contractOriginPairHash(originSourceId, originDestinationId)
		first := testDeliveryHoldByMiss(t, owner, originSourceId, originDestinationId)
		// another request's read of the same pair, armed while it is acknowledged
		second := owner.Watch(originSourceId, originDestinationId)
		defer second.Close()
		fence, fenced := second.Update()
		if !fenced {
			t.Fatal("a read armed while its pair was acknowledged was not fenced")
		}
		// the request that subscribed the pair ends first
		first.Close()
		synctest.Wait()
		if !sockets.live(bucket).Subscribed(contractOriginPairChannel(key)) {
			t.Fatal("the pair was unsubscribed while a fenced read's verdict was pending")
		}
		// an origin commits after the read, published on the pair channel alone
		testDeliveryPublishOrigin(broker, "B", originSourceId, originDestinationId)
		synctest.Wait()
		if owner.RecordRouteRejection(originDestinationId, originSourceId, true, testDeliveryMissingOrigin(), fence) {
			t.Fatal("a verdict read before its origin's edge was stored")
		}
	})
}

// Snapshot under the ring's lock, then the owner's: once quiet, every pair's
// holds are its live ring refusals and its holding watches, each bucket counts
// its held pairs, and the owner and the gauge count them all.
func testDeliveryCheckHolds(t *testing.T, owner *ContractOriginNotifications, watches []*ContractOriginWatch, gaugeBase float64) {
	t.Helper()
	ringHolds := map[[32]byte]int{}
	func() {
		owner.routeRejections.stateLock.Lock()
		defer owner.routeRejections.stateLock.Unlock()
		for key := range owner.routeRejections.entries {
			ringHolds[key.originPair] += 1
		}
	}()
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	watchHolds := map[[32]byte]int{}
	for _, watch := range watches {
		if watch.holding {
			watchHolds[watch.key] += 1
		}
	}
	total := 0
	for i := range owner.buckets {
		bucket := &owner.buckets[i]
		held := 0
		for key, pair := range bucket.pairs {
			if want := ringHolds[key] + watchHolds[key]; pair.holds != want {
				t.Fatalf("a pair holds %d, but has %d refusals and %d holding watches", pair.holds, ringHolds[key], watchHolds[key])
			}
			if 0 < pair.holds {
				held += 1
			}
		}
		if bucket.heldPairs != held {
			t.Fatalf("bucket %d counts %d held pairs of %d", i, bucket.heldPairs, held)
		}
		total += held
	}
	for _, holders := range []map[[32]byte]int{ringHolds, watchHolds} {
		for key := range holders {
			if owner.buckets[contractOriginBucketIndex(key)].pairs[key] == nil {
				t.Fatal("a held pair is not registered")
			}
		}
	}
	if owner.heldPairs != total || owner.settings.MaxHeldPairs < total {
		t.Fatalf("the owner counts %d held pairs of %d (bound %d)", owner.heldPairs, total, owner.settings.MaxHeldPairs)
	}
	if gauge := testutil.ToFloat64(contractOriginHeldPairsGauge) - gaugeBase; gauge != float64(total) {
		t.Fatalf("the held pairs gauge reads %v, want %d", gauge, total)
	}
}

// Once quiet, each live socket carries its anchor and the channels
// of the bucket's held pairs, and no other.
func testDeliveryCheckSubscriptions(t *testing.T, owner *ContractOriginNotifications) {
	t.Helper()
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	for i := range owner.buckets {
		bucket := &owner.buckets[i]
		socket, ok := bucket.socket.(*redisshardtest.Socket)
		if !ok || socket == nil {
			continue
		}
		wantChannels := map[string]bool{contractOriginChannel(i): true}
		for key, pair := range bucket.pairs {
			if 0 < pair.holds {
				wantChannels[contractOriginPairChannel(key)] = true
			}
		}
		gotChannels := socket.Channels()
		if len(gotChannels) != len(wantChannels) {
			t.Fatalf("bucket %d's socket carries %d channels, want %d", i, len(gotChannels), len(wantChannels))
		}
		for _, channel := range gotChannels {
			if !wantChannels[channel] {
				t.Fatalf("bucket %d's socket carries %s, which nothing holds", i, channel)
			}
		}
		if bucket.ackedPairs != len(wantChannels)-1 {
			t.Fatalf("bucket %d counts %d acknowledged pairs, want %d", i, bucket.ackedPairs, len(wantChannels)-1)
		}
	}
}

// Holds balance under a random mix of everything that takes or releases one:
// refusals of both causes and shapes stored, replayed, replaced, evicted by a
// small bound, expired, removed by an edge or a local commit, and removed by
// their pair's resync after a lost socket; watches opened, missing, reading
// fenced and closed; time passing the ring's timeout and the idle timeout. After every step the
// holds equal their holders, and the subscriptions their held pairs. At the
// end every hold is released, every pair channel unsubscribed, and every idle
// socket closed.
func TestContractRouteRejectionHoldsBalance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		settings.MaxRouteRejections = 8
		settings.MaxHeldPairs = 12
		gaugeBase := testutil.ToFloat64(contractOriginHeldPairsGauge)
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		ownerCtx := WithContractOriginNotifications(t.Context(), owner)

		type testRoute struct {
			// a refused route, from the origin's destination back to its source
			sourceId      server.Id
			destinationId server.Id
		}
		var routes []testRoute
		for i := range 24 {
			originSourceId, originDestinationId := testContractOriginPairInBucket(i % 4)
			routes = append(routes, testRoute{sourceId: originDestinationId, destinationId: originSourceId})
		}
		var watches []*ContractOriginWatch
		// a fixed seed makes the sequence, and any failure, reproducible
		random := rand.New(rand.NewPCG(20261123, 7))
		for step := range 10000 {
			route := routes[random.IntN(len(routes))]
			switch operation := random.IntN(100); {
			case operation < 20:
				rejection := testDeliveryMissingOrigin()
				if random.IntN(2) == 0 {
					rejection = testDestinationRejection()
				}
				owner.RecordRouteRejection(route.sourceId, route.destinationId, random.IntN(2) == 0, rejection, nil)
			case operation < 28:
				owner.RouteRejection(route.sourceId, route.destinationId, random.IntN(2) == 0)
			case operation < 38:
				if len(watches) < 40 {
					if watch := owner.Watch(route.destinationId, route.sourceId); watch != nil {
						watches = append(watches, watch)
					}
				}
			case operation < 48:
				if 0 < len(watches) {
					watch := watches[random.IntN(len(watches))]
					_, _ = watch.Update()
					watch.Missed()
				}
			case operation < 54:
				if 0 < len(watches) {
					// a fenced read's watch holds; its verdict may be refused
					watch := watches[random.IntN(len(watches))]
					fence, _ := watch.Update()
					owner.RecordRouteRejection(route.sourceId, route.destinationId, true, testDeliveryMissingOrigin(), fence)
				}
			case operation < 64:
				if 0 < len(watches) {
					i := random.IntN(len(watches))
					watches[i].Close()
					watches = append(watches[:i], watches[i+1:]...)
				}
			case operation < 72:
				testDeliveryPublishOrigin(broker, "B", route.destinationId, route.sourceId)
			case operation < 76:
				notifyCommittedContractOrigin(ownerCtx, route.destinationId, route.sourceId)
			case operation < 80:
				if socket := sockets.live(random.IntN(4)); socket != nil {
					broker.Inject(socket, errors.New("synthetic connection lost"))
				}
			case operation < 99:
				time.Sleep(time.Duration(random.IntN(2500)) * time.Millisecond)
			default:
				time.Sleep(settings.RouteRejectionTimeout + time.Second)
			}
			synctest.Wait()
			testDeliveryCheckHolds(t, owner, watches, gaugeBase)
			testDeliveryCheckSubscriptions(t, owner)
			if t.Failed() {
				t.Fatalf("after step %d", step)
			}
		}

		for _, watch := range watches {
			watch.Close()
		}
		watches = nil
		// the refusals expire and release their pairs
		time.Sleep(settings.RouteRejectionTimeout + time.Second)
		synctest.Wait()
		testDeliveryCheckHolds(t, owner, nil, gaugeBase)
		testDeliveryCheckSubscriptions(t, owner)
		owner.stateLock.Lock()
		heldPairs := owner.heldPairs
		owner.stateLock.Unlock()
		if heldPairs != 0 {
			t.Fatalf("%d pairs stay held with no holder", heldPairs)
		}
		// and with nothing held, the idle sockets close
		time.Sleep(settings.IdleBucketTimeout + time.Second)
		synctest.Wait()
		if count := broker.SubscriptionCount(); count != 0 {
			t.Fatalf("%d channels stay subscribed with nothing held or watched", count)
		}
		owner.Close()
		synctest.Wait()
		if count := broker.SubscriptionCount(); count != 0 {
			t.Fatalf("%d channels stay subscribed after the owner closed", count)
		}
		if gauge := testutil.ToFloat64(contractOriginHeldPairsGauge) - gaugeBase; gauge != 0 {
			t.Fatalf("the held pairs gauge reads %v after the owner closed", gauge)
		}
	})
}

// Every pair of publisher and owner a roll or a rollback can run together with
// Build B. An owner from before Build A is emulated by a bare socket on the
// Build A bucket channel, which is all it subscribes: it is woken exactly when
// that socket receives the edge. A Build A owner is emulated by a Build B
// owner, which subscribes a pair's channel after a miss as Build A does, plus
// Build A's reading of its bucket channel: the test reads a bare socket on
// that channel and hands each of its edges to the owner as the pair's edge,
// which is what Build A does with it. Build A's publisher is emulated by its
// pipeline of both channels; Build B's is real. Each allowed pair wakes the
// held pair's waiter and removes its refusal (or, for the old owner, delivers
// the edge). The two pairs the roll rule keeps apart show the regression: an
// old publisher reaches no Build B owner, and a Build B publisher reaches no
// owner from before Build A.
func TestContractOriginMixedVersions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		bOwner, bSockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer bOwner.Close()
		// the Build A emulation (see above)
		aOwner, aSockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer aOwner.Close()
		// a Build B process committing origins through its own owner
		publisher, _ := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer publisher.Close()
		publisherCtx := WithContractOriginNotifications(t.Context(), publisher)
		publish := func(version string, sourceId server.Id, destinationId server.Id) {
			if version == "B" {
				notifyCommittedContractOrigin(publisherCtx, sourceId, destinationId)
			} else {
				testDeliveryPublishOrigin(broker, version, sourceId, destinationId)
			}
			synctest.Wait()
		}
		caseIndex := 0
		for _, ownerVersion := range []string{"old", "A", "B"} {
			for _, publisherVersion := range []string{"old", "A", "B"} {
				allowed := !(ownerVersion == "old" && publisherVersion == "B") && !(ownerVersion == "B" && publisherVersion == "old")
				bucket := caseIndex % contractOriginBucketCount
				caseIndex += 1
				originSourceId, originDestinationId := testContractOriginPairInBucket(bucket)
				key := contractOriginPairHash(originSourceId, originDestinationId)
				bucketChannel := testDeliveryBuildAChannel(bucket)
				if ownerVersion == "old" {
					socket, err := broker.Open(t.Context(), bucketChannel)
					if err != nil {
						t.Fatal(err)
					}
					publish(publisherVersion, originSourceId, originDestinationId)
					delivered := 0 < socket.ChannelDeliveryCount(bucketChannel)
					if delivered != allowed {
						t.Fatalf("%s publisher, owner from before Build A: delivered=%t, want %t", publisherVersion, delivered, allowed)
					}
					_ = socket.Close()
					continue
				}
				owner, sockets := bOwner, bSockets
				var bucketChannelSocket *redisshardtest.Socket
				if ownerVersion == "A" {
					owner, sockets = aOwner, aSockets
					var err error
					bucketChannelSocket, err = broker.Open(t.Context(), bucketChannel)
					if err != nil {
						t.Fatal(err)
					}
				}
				watch := testDeliveryHoldByMiss(t, owner, originSourceId, originDestinationId)
				// the route a companion would ride this origin back on
				if !owner.RecordRouteRejection(originDestinationId, originSourceId, true, testDeliveryMissingOrigin(), nil) {
					t.Fatal("refusal was not stored")
				}
				update, _ := watch.Update()
				publish(publisherVersion, originSourceId, originDestinationId)
				if bucketChannelSocket != nil {
					// Build A acts on a bucket channel edge for its pair as on
					// the pair's own edge
					for range bucketChannelSocket.ChannelDeliveryCount(bucketChannel) {
						broker.Inject(sockets.live(bucket), &redis.Message{Channel: contractOriginPairChannel(key), Payload: ""})
					}
					synctest.Wait()
					_ = bucketChannelSocket.Close()
				}
				woken := testDeliveryClosed(update)
				_, kept := owner.RouteRejection(originDestinationId, originSourceId, true)
				if woken != allowed || kept == allowed {
					t.Fatalf("%s publisher, %s owner: woken=%t refusal kept=%t, want woken=%t", publisherVersion, ownerVersion, woken, kept, allowed)
				}
				watch.Close()
			}
		}
	})
}

// B1: a bucket with no watch and no held pair closes its socket after the
// idle timeout, and no sooner. The next hold or watch reopens it. In Build B
// the reopened anchor's acknowledgement resyncs nothing: nothing is published
// to the anchor, and the held pairs had no subscription on the closed socket
// to miss edges on. Each held pair subscribes on the new socket, its
// acknowledgement registers a waiter that missed before it, and its edges then
// remove its refusals. A waiter that has not missed is not woken.
func TestContractOriginIdleBucketClosesAndReopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		settings := DefaultContractOriginNotificationSettings()
		owner, sockets := testDeliveryOwner(t.Context(), broker, settings, nil)
		defer owner.Close()
		const bucket = 11
		watch := owner.Watch(testContractOriginPairInBucket(bucket))
		_, _ = watch.Update()
		synctest.Wait()
		socket := sockets.live(bucket)
		if socket == nil {
			t.Fatal("a watch did not open its bucket's socket")
		}
		watch.Close()
		closedBefore := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("bucket_closed"))
		synctest.Wait()
		time.Sleep(settings.IdleBucketTimeout - time.Millisecond)
		synctest.Wait()
		if socket.Closed() {
			t.Fatal("the socket closed before the idle timeout")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if !socket.Closed() {
			t.Fatal("an idle socket stayed open past the idle timeout")
		}
		if closed := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("bucket_closed")) - closedBefore; closed != 1 {
			t.Fatalf("bucket_closed counted %v, want 1", closed)
		}
		if subscribers := broker.Subscribers(contractOriginChannel(bucket)); subscribers != 0 {
			t.Fatalf("the closed bucket's channel keeps %d subscribers", subscribers)
		}

		// A refusal reopens it, with two waiters armed before the
		// acknowledgements, one past a miss: the broker's latency holds the
		// acknowledgements back until all three are in.
		broker.SetLatency(time.Millisecond)
		refusedOriginId, refusedRouteSourceId := testContractOriginPairInBucket(bucket)
		refusedKey := contractOriginPairHash(refusedOriginId, refusedRouteSourceId)
		if !owner.RecordRouteRejection(refusedRouteSourceId, refusedOriginId, true, testDeliveryMissingOrigin(), nil) {
			t.Fatal("refusal was not stored")
		}
		missedSourceId, missedDestinationId := testContractOriginPairInBucket(bucket)
		missedKey := contractOriginPairHash(missedSourceId, missedDestinationId)
		missing := owner.Watch(missedSourceId, missedDestinationId)
		defer missing.Close()
		missingUpdate, _ := missing.Update()
		registration, held := missing.Missed()
		if !held {
			t.Fatal("the miss was not held")
		}
		waiting := owner.Watch(testContractOriginPairInBucket(bucket))
		defer waiting.Close()
		update, _ := waiting.Update()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		broker.SetLatency(0)
		reopened := sockets.live(bucket)
		if reopened == nil || reopened == socket {
			t.Fatal("a held pair did not reopen its bucket's socket")
		}
		for _, key := range [][32]byte{refusedKey, missedKey} {
			if !reopened.Subscribed(contractOriginPairChannel(key)) {
				t.Fatal("a held pair did not subscribe on the reopened socket")
			}
		}
		if !testDeliveryClosed(registration) {
			t.Fatal("the pair's acknowledgement did not register the waiter that missed")
		}
		if testDeliveryClosed(update) || testDeliveryClosed(missingUpdate) {
			t.Fatal("the reopened anchor's acknowledgement woke a waiter")
		}
		if _, ok := owner.RouteRejection(refusedRouteSourceId, refusedOriginId, true); !ok {
			t.Fatal("a refusal was removed with no edge and no lost subscription")
		}
		// the refusal's pair is subscribed: its origin's edge removes it
		testDeliveryPublishOrigin(broker, "B", refusedOriginId, refusedRouteSourceId)
		synctest.Wait()
		if _, ok := owner.RouteRejection(refusedRouteSourceId, refusedOriginId, true); ok {
			t.Fatal("the refusal's origin edge did not remove it")
		}

		// with neither again it closes again, and a watch alone reopens it
		missing.Close()
		waiting.Close()
		synctest.Wait()
		time.Sleep(settings.IdleBucketTimeout)
		synctest.Wait()
		if !reopened.Closed() {
			t.Fatal("the reopened socket stayed open past the idle timeout")
		}
		again := owner.Watch(testContractOriginPairInBucket(bucket))
		defer again.Close()
		synctest.Wait()
		if live := sockets.live(bucket); live == nil || live == reopened {
			t.Fatal("a watch did not reopen its bucket's socket")
		}
	})
}

// The cost of a published origin, with 40 owners each with a socket in one
// bucket: when one holds the pair only that owner reads its edge, as Build B
// delivers; when all 40 hold it every owner reads every edge, which is what
// the bucket channel cost every owner with a socket in the bucket before
// Build B. The edges are published back to back and the time runs until every
// owner they reach has handled them all; the owners' readers run in parallel,
// so the time understates the CPU of 40 deliveries an edge.
func BenchmarkContractOriginDelivery(b *testing.B) {
	for _, delivery := range []struct {
		name        string
		holderCount int
	}{
		{name: "one_holder", holderCount: 1},
		{name: "all_holding", holderCount: 40},
	} {
		b.Run(delivery.name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			broker := redisshardtest.NewBroker()
			settings := DefaultContractOriginNotificationSettings()
			const ownerCount = 40
			const bucket = 1
			heldSourceId, heldDestinationId := testContractOriginPairInBucket(bucket)
			heldKey := contractOriginPairHash(heldSourceId, heldDestinationId)
			var owners []*ContractOriginNotifications
			var watches []*ContractOriginWatch
			for i := range ownerCount {
				owner, _ := testDeliveryOwner(ctx, broker, settings, nil)
				owners = append(owners, owner)
				var watch *ContractOriginWatch
				if i < delivery.holderCount {
					watch = owner.Watch(heldSourceId, heldDestinationId)
					_, _ = watch.Update()
					watch.Missed()
				} else {
					watch = owner.Watch(testContractOriginPairInBucket(bucket))
				}
				watches = append(watches, watch)
			}
			defer func() {
				for _, watch := range watches {
					watch.Close()
				}
				for _, owner := range owners {
					owner.Close()
				}
			}()
			waitFor := func(condition func() bool) {
				deadline := time.Now().Add(time.Minute)
				for !condition() {
					if deadline.Before(time.Now()) {
						b.Fatal("the owners did not settle")
					}
					time.Sleep(100 * time.Microsecond)
				}
			}
			channel := contractOriginPairChannel(heldKey)
			waitFor(func() bool {
				return broker.Subscribers(contractOriginChannel(bucket)) == ownerCount && broker.Subscribers(channel) == delivery.holderCount
			})
			received := func() float64 {
				return testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("received"))
			}
			b.ResetTimer()
			before := received()
			for range b.N {
				if broker.Publish(channel, "") != delivery.holderCount {
					b.Fatal("the edge reached the wrong number of owners")
				}
			}
			waitFor(func() bool { return before+float64(delivery.holderCount*b.N) <= received() })
			b.StopTimer()
			b.ReportMetric(float64(delivery.holderCount), "deliveries/op")
		})
	}
}
