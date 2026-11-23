package server

// A real cluster moves a subscribed channel's slot the way a rebalance does:
// SETSLOT IMPORTING and MIGRATING, MIGRATE of the slot's keys, then SETSLOT NODE
// on every master. The subscription runs through the production source, the
// process client and its slot map, pointed at the test cluster.
//
// It runs only against a disposable local cluster that WARP_TEST_REDIS_CLUSTER
// names by one master's loopback host:port, for example:
//
//	docker run -d --name shard-test-cluster -p 127.0.0.1:7301-7303:7301-7303 redis:8-alpine sh -c '
//	  for p in 7301 7302 7303; do redis-server --port $p --cluster-enabled yes \
//	    --cluster-config-file /tmp/nodes-$p.conf --save "" --appendonly no \
//	    --daemonize yes --cluster-announce-ip 127.0.0.1; done; sleep 1
//	  redis-cli --cluster create 127.0.0.1:7301 127.0.0.1:7302 127.0.0.1:7303 --cluster-yes
//	  sleep infinity'
//	WARP_TEST_REDIS_CLUSTER=127.0.0.1:7301 go test -run TestRedisShardSubscriptionFollowsSlotMigration .
//
// A loopback authority can also be a tunnel to a live cluster, so before any
// write the test refuses a cluster with more than six nodes or any stored key.

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The disposable cluster WARP_TEST_REDIS_CLUSTER names, after its read-only
// checks, installed as the process client until the test ends. Skips when no
// cluster is named.
func testRedisShardCluster(t *testing.T) (context.Context, *redis.ClusterClient) {
	authority := os.Getenv("WARP_TEST_REDIS_CLUSTER")
	if authority == "" {
		t.Skip("WARP_TEST_REDIS_CLUSTER names no disposable local cluster")
	}
	authorityHost, authorityPort, err := net.SplitHostPort(authority)
	if err != nil || !redisLoopbackHost(authorityHost) {
		t.Fatalf("WARP_TEST_REDIS_CLUSTER=%s is not a loopback host:port", authority)
	}
	// A production data tunnel forwards its cluster to these loopback ports.
	if port, err := strconv.ParseUint(authorityPort, 10, 16); err != nil || testEnvLoopbackTunnelPort(uint16(port)) {
		t.Fatalf("WARP_TEST_REDIS_CLUSTER=%s is a production data tunnel port; use a disposable cluster on other ports", authority)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	clusterClient := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:  []string{authority},
		Dialer: currentTestEnvDialGuard().wrapDial("redis", NewDialer(5*time.Second).DialContext),
	})
	t.Cleanup(func() { clusterClient.Close() })

	// read-only checks that this is a disposable cluster
	clusterInfo, err := clusterClient.ClusterInfo(ctx).Result()
	if err != nil {
		t.Fatalf("cluster info: %v", err)
	}
	for _, line := range strings.Split(clusterInfo, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "cluster_known_nodes:"); ok {
			if knownNodes, err := strconv.Atoi(value); err != nil || 6 < knownNodes {
				t.Fatalf("refusing a cluster of %s nodes; use a disposable local cluster", value)
			}
		}
	}
	err = clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		keyCount, err := node.DBSize(ctx).Result()
		if err != nil {
			return err
		}
		if keyCount != 0 {
			return fmt.Errorf("master %s stores %d keys", node.Options().Addr, keyCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("refusing a cluster that is not empty: %v", err)
	}

	// the production source opens through the process client
	func() {
		safeClient.mutex.Lock()
		defer safeClient.mutex.Unlock()
		previousClient := safeClient.client
		safeClient.client = clusterClient
		t.Cleanup(func() {
			safeClient.mutex.Lock()
			defer safeClient.mutex.Unlock()
			safeClient.client = previousClient
		})
	}()
	return ctx, clusterClient
}

func TestRedisShardSubscriptionFollowsSlotMigration(t *testing.T) {
	ctx, clusterClient := testRedisShardCluster(t)

	channel := fmt.Sprintf("test:shard-subscription:{%s}", NewId())
	// a key in the channel's slot, so the move migrates data like a rebalance
	key := fmt.Sprintf("%s:key", channel)
	if err := clusterClient.Set(ctx, key, "synthetic", time.Minute).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	defer clusterClient.Del(context.Background(), key)

	subscribeds := make(chan bool, 16)
	payloads := make(chan string, 16)
	subscriptionCtx, subscriptionCancel := context.WithCancel(ctx)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		RunRedisShardSubscription(subscriptionCtx, channel, DefaultRedisShardSubscriptionSettings(), DefaultRedisShardSubscriptionSource(), &RedisShardSubscriptionHandler{
			Subscribed: func(resubscribed bool) {
				subscribeds <- resubscribed
			},
			Message: func(message RedisMessage) bool {
				if message.Channel != channel {
					return false
				}
				payloads <- message.Payload
				return true
			},
		})
	}()
	defer func() {
		subscriptionCancel()
		<-returned
	}()

	requireSubscribed := func(want bool, message string) {
		select {
		case resubscribed := <-subscribeds:
			if resubscribed != want {
				t.Fatalf("%s: resubscribed=%t, want %t", message, resubscribed, want)
			}
		case <-time.After(20 * time.Second):
			t.Fatal(message)
		}
	}
	publish := func(payload string) {
		receiverCount, err := clusterClient.SPublish(ctx, channel, payload).Result()
		if err != nil {
			t.Fatalf("spublish %s: %v", payload, err)
		}
		if receiverCount != 1 {
			t.Fatalf("spublish %s reached %d subscribers, want 1", payload, receiverCount)
		}
		select {
		case received := <-payloads:
			if received != payload {
				t.Fatalf("received %s, want %s", received, payload)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no message %s", payload)
		}
	}

	requireSubscribed(false, "no subscription acknowledged")
	publish("before-move")

	slot, err := clusterClient.ClusterKeySlot(ctx, channel).Result()
	if err != nil {
		t.Fatalf("keyslot: %v", err)
	}
	sourceAddr, masterAddrs := testRedisSlotOwner(t, ctx, clusterClient, slot)
	targetAddr := ""
	for _, masterAddr := range masterAddrs {
		if masterAddr != sourceAddr {
			targetAddr = masterAddr
			break
		}
	}
	if targetAddr == "" {
		t.Fatal("the cluster has a single master")
	}
	testRedisMoveSlot(t, ctx, slot, sourceAddr, targetAddr, masterAddrs)
	defer testRedisMoveSlot(t, context.Background(), slot, targetAddr, sourceAddr, masterAddrs)

	requireSubscribed(true, "the moved slot was never subscribed again")
	publish("after-move")

	target := redis.NewClient(&redis.Options{
		Addr:   targetAddr,
		Dialer: currentTestEnvDialGuard().wrapDial("redis", NewDialer(5*time.Second).DialContext),
	})
	defer target.Close()
	subscriberCounts, err := target.PubSubShardNumSub(ctx, channel).Result()
	if err != nil {
		t.Fatalf("shardnumsub: %v", err)
	}
	if subscriberCounts[channel] != 1 {
		t.Fatalf("new owner subscribers=%d, want 1", subscriberCounts[channel])
	}
	if value, err := clusterClient.Get(ctx, key).Result(); err != nil || value != "synthetic" {
		t.Fatalf("migrated key=%q err=%v", value, err)
	}
}

// CPUOPT7-ORIGIN-DELIVERY.md §4.3 test 14. A consumer also carries channels of
// the owned channel's slot on its socket, as the contract-origin owner carries
// its held pairs' channels: it subscribes them on each socket the handler's
// Socket hands it, in one SSUBSCRIBE, and judges their notices by the shard
// channel count. The slot move ends them with the owned channel; the socket is
// replaced, the channels resubscribe on the new owner, and an edge published
// after the move reaches the consumer exactly once.
func TestRedisShardSubscriptionFollowsSlotMigrationWithSlotChannels(t *testing.T) {
	ctx, clusterClient := testRedisShardCluster(t)

	id := NewId()
	channel := fmt.Sprintf("test:shard-subscription:{%s}", id)
	slotChannels := []string{
		fmt.Sprintf("test:shard-subscription:{%s}:pair-a", id),
		fmt.Sprintf("test:shard-subscription:{%s}:pair-b", id),
	}
	key := fmt.Sprintf("%s:key", channel)
	if err := clusterClient.Set(ctx, key, "synthetic", time.Minute).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	defer clusterClient.Del(context.Background(), key)

	type notice struct {
		kind    string
		channel string
		count   int
	}
	subscribeds := make(chan bool, 16)
	notices := make(chan notice, 64)
	payloads := make(chan string, 16)
	subscriptionCtx, subscriptionCancel := context.WithCancel(ctx)
	returned := make(chan struct{})
	acknowledgedChannels := map[string]bool{}
	go func() {
		defer close(returned)
		RunRedisShardSubscription(subscriptionCtx, channel, DefaultRedisShardSubscriptionSettings(), DefaultRedisShardSubscriptionSource(), &RedisShardSubscriptionHandler{
			Socket: func(subscription RedisShardSubscription) {
				clear(acknowledgedChannels)
				if err := subscription.SSubscribe(subscriptionCtx, slotChannels...); err != nil {
					t.Errorf("slot channels: %v", err)
				}
			},
			Subscribed: func(resubscribed bool) {
				subscribeds <- resubscribed
			},
			Message: func(message RedisMessage) bool {
				payloads <- message.Channel + "=" + message.Payload
				return true
			},
			Notice: func(subscriptionNotice RedisSubscriptionNotice) error {
				switch subscriptionNotice.Kind {
				case "ssubscribe":
					acknowledgedChannels[subscriptionNotice.Channel] = true
				case "sunsubscribe":
					// nothing here asks for one: the slot moved
					delete(acknowledgedChannels, subscriptionNotice.Channel)
					notices <- notice{kind: subscriptionNotice.Kind, channel: subscriptionNotice.Channel, count: subscriptionNotice.Count}
					return fmt.Errorf("unrequested sunsubscribe of %s", subscriptionNotice.Channel)
				}
				notices <- notice{kind: subscriptionNotice.Kind, channel: subscriptionNotice.Channel, count: subscriptionNotice.Count}
				if subscriptionNotice.Count != 1+len(acknowledgedChannels) {
					return fmt.Errorf("shard channel count %d, want %d", subscriptionNotice.Count, 1+len(acknowledgedChannels))
				}
				return nil
			},
		})
	}()
	defer func() {
		subscriptionCancel()
		<-returned
	}()

	requireSubscribed := func(want bool, message string) {
		select {
		case resubscribed := <-subscribeds:
			if resubscribed != want {
				t.Fatalf("%s: resubscribed=%t, want %t", message, resubscribed, want)
			}
		case <-time.After(20 * time.Second):
			t.Fatal(message)
		}
	}
	requireSlotChannels := func(message string) {
		for i := range slotChannels {
			select {
			case received := <-notices:
				// one SSUBSCRIBE, acknowledged in order with the growing count
				if received.kind != "ssubscribe" || received.channel != slotChannels[i] || received.count != 2+i {
					t.Fatalf("%s: notice %+v, want ssubscribe %s with count %d", message, received, slotChannels[i], 2+i)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("%s: slot channel %s was not acknowledged", message, slotChannels[i])
			}
		}
	}
	publish := func(publishChannel string, payload string) {
		receiverCount, err := clusterClient.SPublish(ctx, publishChannel, payload).Result()
		if err != nil {
			t.Fatalf("spublish %s: %v", payload, err)
		}
		if receiverCount != 1 {
			t.Fatalf("spublish %s reached %d subscribers, want 1", payload, receiverCount)
		}
		select {
		case received := <-payloads:
			if received != publishChannel+"="+payload {
				t.Fatalf("received %s, want %s=%s", received, publishChannel, payload)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no message %s", payload)
		}
	}

	requireSubscribed(false, "no subscription acknowledged")
	requireSlotChannels("first socket")
	publish(slotChannels[1], "before-move")

	slot, err := clusterClient.ClusterKeySlot(ctx, channel).Result()
	if err != nil {
		t.Fatalf("keyslot: %v", err)
	}
	sourceAddr, masterAddrs := testRedisSlotOwner(t, ctx, clusterClient, slot)
	targetAddr := ""
	for _, masterAddr := range masterAddrs {
		if masterAddr != sourceAddr {
			targetAddr = masterAddr
			break
		}
	}
	if targetAddr == "" {
		t.Fatal("the cluster has a single master")
	}
	testRedisMoveSlot(t, ctx, slot, sourceAddr, targetAddr, masterAddrs)
	defer testRedisMoveSlot(t, context.Background(), slot, targetAddr, sourceAddr, masterAddrs)

	requireSubscribed(true, "the moved slot was never subscribed again")
	// the old owner's sunsubscribes, if read before the socket closed, then
	// the new owner's acknowledgements
	deadline := time.After(20 * time.Second)
	acknowledged := 0
	for acknowledged < len(slotChannels) {
		select {
		case received := <-notices:
			switch {
			case received.kind == "sunsubscribe":
			case received.kind == "ssubscribe" && received.channel == slotChannels[acknowledged] && received.count == 2+acknowledged:
				acknowledged += 1
			default:
				t.Fatalf("after the move: notice %+v", received)
			}
		case <-deadline:
			t.Fatalf("after the move: %d of %d slot channels acknowledged", acknowledged, len(slotChannels))
		}
	}
	publish(slotChannels[0], "after-move")

	target := redis.NewClient(&redis.Options{
		Addr:   targetAddr,
		Dialer: currentTestEnvDialGuard().wrapDial("redis", NewDialer(5*time.Second).DialContext),
	})
	defer target.Close()
	subscriberCounts, err := target.PubSubShardNumSub(ctx, append([]string{channel}, slotChannels...)...).Result()
	if err != nil {
		t.Fatalf("shardnumsub: %v", err)
	}
	for _, subscribedChannel := range append([]string{channel}, slotChannels...) {
		if subscriberCounts[subscribedChannel] != 1 {
			t.Fatalf("new owner subscribers of %s=%d, want 1", subscribedChannel, subscriberCounts[subscribedChannel])
		}
	}
}

// The master that owns the slot, and every master.
func testRedisSlotOwner(t *testing.T, ctx context.Context, clusterClient *redis.ClusterClient, slot int64) (string, []string) {
	slotRanges, err := clusterClient.ClusterSlots(ctx).Result()
	if err != nil {
		t.Fatalf("cluster slots: %v", err)
	}
	ownerAddr := ""
	masterAddrSet := map[string]bool{}
	var masterAddrs []string
	for _, slotRange := range slotRanges {
		masterAddr := slotRange.Nodes[0].Addr
		if !masterAddrSet[masterAddr] {
			masterAddrSet[masterAddr] = true
			masterAddrs = append(masterAddrs, masterAddr)
		}
		if int64(slotRange.Start) <= slot && slot <= int64(slotRange.End) {
			ownerAddr = masterAddr
		}
	}
	if ownerAddr == "" {
		t.Fatalf("slot %d has no owner", slot)
	}
	return ownerAddr, masterAddrs
}

// Moves the slot as redis-cli --cluster rebalance does.
func testRedisMoveSlot(t *testing.T, ctx context.Context, slot int64, sourceAddr string, targetAddr string, masterAddrs []string) {
	nodeClients := map[string]*redis.Client{}
	for _, masterAddr := range masterAddrs {
		nodeClients[masterAddr] = redis.NewClient(&redis.Options{
			Addr:   masterAddr,
			Dialer: currentTestEnvDialGuard().wrapDial("redis", NewDialer(5*time.Second).DialContext),
		})
	}
	defer func() {
		for _, nodeClient := range nodeClients {
			nodeClient.Close()
		}
	}()
	source, target := nodeClients[sourceAddr], nodeClients[targetAddr]
	sourceId, err := source.Do(ctx, "CLUSTER", "MYID").Text()
	if err != nil {
		t.Fatalf("source id: %v", err)
	}
	targetId, err := target.Do(ctx, "CLUSTER", "MYID").Text()
	if err != nil {
		t.Fatalf("target id: %v", err)
	}
	targetHost, targetPort, err := net.SplitHostPort(targetAddr)
	if err != nil {
		t.Fatalf("target address: %v", err)
	}

	if err := target.Do(ctx, "CLUSTER", "SETSLOT", slot, "IMPORTING", sourceId).Err(); err != nil {
		t.Fatalf("importing: %v", err)
	}
	if err := source.Do(ctx, "CLUSTER", "SETSLOT", slot, "MIGRATING", targetId).Err(); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	for {
		keys, err := source.ClusterGetKeysInSlot(ctx, int(slot), 100).Result()
		if err != nil {
			t.Fatalf("keys in slot: %v", err)
		}
		if len(keys) == 0 {
			break
		}
		args := []any{"MIGRATE", targetHost, targetPort, "", 0, 5000, "KEYS"}
		for _, key := range keys {
			args = append(args, key)
		}
		if err := source.Do(ctx, args...).Err(); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	// the new owner first, then the old one, which drops the slot's shard
	// subscriptions and sends sunsubscribe
	if err := target.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", targetId).Err(); err != nil {
		t.Fatalf("target node: %v", err)
	}
	if err := source.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", targetId).Err(); err != nil {
		t.Fatalf("source node: %v", err)
	}
	for masterAddr, nodeClient := range nodeClients {
		if masterAddr != sourceAddr && masterAddr != targetAddr {
			if err := nodeClient.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", targetId).Err(); err != nil {
				t.Fatalf("node %s: %v", masterAddr, err)
			}
		}
	}
}
