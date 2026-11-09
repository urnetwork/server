//go:build sidecar_pending

package model

// The token scripts and taskworker's pass on a real Redis Cluster of 3 masters (SDK-M8-09, SDK-M8-11): t0
// is the TIME of the master serving the client's slot, read through a script on its tokrev, which the
// cluster routes there, while go-redis's TIME goes to any node; and the pass's topology reads each
// master's run_id and slots and lists a slot's keys on its master's own connection.
//
// These run only against a cluster that WARP_TEST_REDIS_CLUSTER names by one master's loopback host:port,
// and dial only through the test environment's guard (server.Testing_DialTestEnv), so a cluster runs here
// once test-env.sh attests its masters (plan-TEST-t3-redis-attestation, QD-028). Until then they skip, and
// T3's rows on the cluster wait, as the owner's interim says. Before any write they refuse a cluster of
// more than six nodes or one that stores a key.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
)

// A client of the disposable cluster WARP_TEST_REDIS_CLUSTER names, through the test environment's dial
// guard, or a skip when it names none.
func testPushTokenClusterClient(t *testing.T, ctx context.Context) *redis.ClusterClient {
	t.Helper()
	authority := os.Getenv("WARP_TEST_REDIS_CLUSTER")
	if authority == "" {
		t.Skip("WARP_TEST_REDIS_CLUSTER names no disposable local cluster; T3's cluster rows wait on QD-028")
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
		t.Fatalf("WARP_TEST_REDIS_CLUSTER=%s is not a loopback host:port", authority)
	}
	if port, err := strconv.ParseUint(portText, 10, 16); err != nil || server.Testing_LoopbackTunnelPort(uint16(port)) || port <= 50000 {
		t.Fatalf("WARP_TEST_REDIS_CLUSTER=%s is a production data tunnel port or not above 50000", authority)
	}
	clusterClient := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs: []string{authority},
		Dialer: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return server.Testing_DialTestEnv(ctx, "redis", network, address)
		},
	})
	t.Cleanup(func() { clusterClient.Close() })
	clusterInfo, err := clusterClient.ClusterInfo(ctx).Result()
	if err != nil {
		t.Fatalf("cluster info: %v", err)
	}
	for _, line := range strings.Split(clusterInfo, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "cluster_known_nodes:"); ok {
			if knownNodes, err := strconv.Atoi(value); err != nil || 6 < knownNodes {
				t.Fatalf("refusing a cluster of %s nodes", value)
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
	return clusterClient
}

// t0 comes from the master serving the client's slot: the time script runs there and nowhere else, its
// answer that master's TIME, and the apply that follows lands on the same master, with no CROSSSLOT.
func TestT0ReadFromTheSlotsMaster(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	clusterClient := testPushTokenClusterClient(t, ctx)
	store := NewPushTokenRedisStore(clusterClient)
	sum := sha1.Sum([]byte(pushservice.WakeTokenTimeScript.Source))
	sha := hex.EncodeToString(sum[:])

	clientId := connect.NewId()
	tokrevKey := pushservice.WakeTokenRevisionKey(clientId)
	slotMaster, err := clusterClient.MasterForKey(ctx, tokrevKey)
	if err != nil {
		t.Fatal(err)
	}
	if slot, err := clusterClient.ClusterKeySlot(ctx, tokrevKey).Result(); err != nil || int(slot) != server.RedisHashSlot(tokrevKey) {
		t.Fatalf("CLUSTER KEYSLOT %d, ours %d: %v", slot, server.RedisHashSlot(tokrevKey), err)
	}
	t0, err := pushservice.ReadWakeTokenTime(ctx, store, clientId)
	if err != nil {
		t.Fatal(err)
	}
	masterTime, err := slotMaster.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	if delta := masterTime.UnixMilli() - t0; delta < 0 || 1000 < delta {
		t.Fatalf("t0 %d is not the slot's master's TIME %d", t0, masterTime.UnixMilli())
	}
	err = clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		exists, err := node.ScriptExists(ctx, sha).Result()
		if err != nil {
			return err
		}
		if want := node.Options().Addr == slotMaster.Options().Addr; exists[0] != want {
			return fmt.Errorf("master %s holds the time script %t, want %t", node.Options().Addr, exists[0], want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	row := &pushservice.WakeTokenRow{ClientId: clientId, Revision: 1, Platform: sidecar.PushTokenPlatformApns, Token: "cluster-token"}
	if applied, err := pushservice.ApplyWakeToken(ctx, store, row, t0, time.Minute); err != nil || applied != pushservice.WakeTokenApplied {
		t.Fatalf("%s %v", applied, err)
	}
	if tokrev, err := slotMaster.Get(ctx, tokrevKey).Result(); err != nil || tokrev != "1" {
		t.Fatalf("tokrev on the slot's master: %q %v", tokrev, err)
	}
	clusterClient.Del(ctx, pushservice.HintTokenKey(clientId), tokrevKey)
}

// The pass's topology on the cluster: every slot served by one of the masters, each with its INFO
// run_id; a slot's keys listed on its own master; a found slot reconciled on the pass ReconcileWait on,
// its stale key gone and its sentinel the master's run_id.
func TestPushTokenPassOnACluster(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clusterClient := testPushTokenClusterClient(t, ctx)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		store := NewPushTokenRedisStore(clusterClient)
		topology := NewPushTokenRedisTopology(clusterClient)
		masters, err := topology.Masters(ctx)
		if err != nil {
			t.Fatal(err)
		}
		served := map[int]string{}
		for _, master := range masters {
			if master.RunId == "" {
				t.Fatalf("%s has no run_id", master.Address)
			}
			for _, slot := range master.Slots {
				served[slot] = master.Address
			}
		}
		if len(masters) != 3 || len(served) != server.RedisSlotCount {
			t.Fatalf("%d masters serving %d slots", len(masters), len(served))
		}

		clientId := server.NewId()
		for 64 <= PushTokenSlot(clientId) {
			clientId = server.NewId()
		}
		orphan := &pushservice.WakeTokenRow{ClientId: connect.Id(clientId), Revision: 2, Platform: sidecar.PushTokenPlatformFcm, Token: "cluster-orphan"}
		t0, err := pushservice.ReadWakeTokenTime(ctx, store, connect.Id(clientId))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pushservice.ApplyWakeToken(ctx, store, orphan, t0, time.Minute); err != nil {
			t.Fatal(err)
		}
		slot := PushTokenSlot(clientId)
		var slotMaster *PushTokenMaster
		for _, master := range masters {
			if master.Address == served[slot] {
				slotMaster = master
			}
		}
		_, keys, more, err := topology.SlotTimeAndKeys(ctx, slotMaster, slot, 1000)
		if err != nil || more || len(keys) != 2 {
			t.Fatalf("slot %d's keys %v %t: %v", slot, keys, more, err)
		}

		settings := DefaultSidecarPushTokenPassSettings()
		settings.ReconcileSlotCount = 64
		start := time.Now()
		state, report, err := RunPushTokenPass(ctx, &PushTokenPassState{LastCompletedStartTime: start}, start, true, store, topology, settings, nil)
		if err != nil || report.FoundSlotCount != server.RedisSlotCount {
			t.Fatalf("finding pass: %+v %v", report, err)
		}
		_, report, err = RunPushTokenPass(ctx, state, start.Add(settings.ReconcileWait), true, store, topology, settings, nil)
		if err != nil || report.ReconciledSlotCount != 64 || report.DeletedClientCount != 1 {
			t.Fatalf("reconciling pass: %+v %v", report, err)
		}
		sentinel, err := clusterClient.Get(ctx, PushTokenSentinelKey(slot)).Result()
		if err != nil || sentinel != slotMaster.RunId {
			t.Fatalf("sentinel %q: %v", sentinel, err)
		}
		// leave the cluster as empty as it came
		clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			return node.FlushDB(ctx).Err()
		})
	})
}
