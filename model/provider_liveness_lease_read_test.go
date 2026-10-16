// The provider liveness lease's reader (provider_liveness_lease_read.go): the
// window and staleness boundaries to the millisecond, clock skew, the cached
// live handlers, writer coverage of a mixed connect fleet, ids across shards,
// duplicates, and failures that fail open and count. The Redis tests run on
// the local test env; the cluster test runs on a disposable local cluster.
package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The current value of one counter or gauge.
func livenessTestMetric(t testing.TB, metric prometheus.Collector) float64 {
	t.Helper()
	return testutil.ToFloat64(metric)
}

// The candidates counted for a rank mode and verdict.
func livenessTestVerdictCount(t testing.TB, rankMode RankMode, verdict string) float64 {
	t.Helper()
	return livenessTestMetric(t, findProviders2LivenessCandidates.WithLabelValues(rankMode, verdict))
}

// Forgets the process's read of the live handlers, now and when the test ends:
// the read is the process's, and each test env has its own database.
func livenessTestFreshHandlers(t testing.TB) {
	defaultProviderLivenessHandlers.reset()
	t.Cleanup(defaultProviderLivenessHandlers.reset)
}

// Stamps every live connect handler in the writer set with the node's clock.
func livenessTestStampLiveHandlers(ctx context.Context, t testing.TB) []server.Id {
	t.Helper()
	handlerIds, err := loadLiveNetworkClientHandlerIds(ctx, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout))
	if err != nil {
		t.Fatal(err)
	}
	for _, handlerId := range handlerIds {
		if err := PublishProviderLivenessWriter(ctx, handlerId); err != nil {
			t.Fatal(err)
		}
	}
	return handlerIds
}

// Moves a handler's heartbeat to heartbeatTime.
func livenessTestHeartbeat(ctx context.Context, handlerId server.Id, heartbeatTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_handler SET heartbeat_time = $2 WHERE handler_id = $1`, handlerId, heartbeatTime.UTC()))
	})
}

// The verdict a read gave each id: live, offline or unknown.
func livenessTestVerdicts(clientIds []server.Id, offline map[server.Id]bool, unknown map[server.Id]bool) map[server.Id]string {
	verdicts := map[server.Id]string{}
	for _, clientId := range clientIds {
		switch {
		case offline[clientId] && unknown[clientId]:
			verdicts[clientId] = "offline and unknown"
		case offline[clientId]:
			verdicts[clientId] = providerLivenessOffline
		case unknown[clientId]:
			verdicts[clientId] = providerLivenessUnknown
		default:
			verdicts[clientId] = providerLivenessLive
		}
	}
	return verdicts
}

// Live exactly to the window: a stamp the window before the reference is
// current, one millisecond older is not. A set is judged while its newest stamp
// is within the window of now, to the millisecond.
func TestProviderLivenessStampsWindowBoundary(t *testing.T) {
	window := providerLivenessLeaseWindow.Milliseconds()
	reference := int64(1_800_000_000_000)
	current, ok := judgeProviderLivenessStamps(reference, reference, []int64{
		reference - window + 1,
		reference - window,
		reference - window - 1,
		0,
	}, providerLivenessLeaseWindow)
	if !ok || !slices.Equal(current, []bool{true, true, false, false}) {
		t.Fatalf("judged %v (%t), want live to exactly the window, offline one millisecond past it and without a stamp", current, ok)
	}
	for _, c := range []struct {
		sinceNewest int64
		ok          bool
	}{
		{sinceNewest: window - 1, ok: true},
		{sinceNewest: window, ok: true},
		{sinceNewest: window + 1, ok: false},
	} {
		if _, ok := judgeProviderLivenessStamps(reference+c.sinceNewest, reference, []int64{reference}, providerLivenessLeaseWindow); ok != c.ok {
			t.Fatalf("a set whose newest stamp is %d ms old judged %t, want %t", c.sinceNewest, ok, c.ok)
		}
	}
}

// An empty or stale set judges no one, so every id in it is admitted; a
// current set enforces, and an id without a stamp in it is not live.
func TestProviderLivenessStampsStaleSetAdmitsAndCurrentSetEnforces(t *testing.T) {
	now := int64(1_800_000_000_000)
	window := providerLivenessLeaseWindow.Milliseconds()
	if _, ok := judgeProviderLivenessStamps(now, 0, []int64{0}, providerLivenessLeaseWindow); ok {
		t.Fatal("an empty shard judged its ids")
	}
	if _, ok := judgeProviderLivenessStamps(now, now-window-1, []int64{now - window - 1, 0}, providerLivenessLeaseWindow); ok {
		t.Fatal("a stale shard judged its ids")
	}
	current, ok := judgeProviderLivenessStamps(now, now-1000, []int64{now - 1000, 0}, providerLivenessLeaseWindow)
	if !ok || !slices.Equal(current, []bool{true, false}) {
		t.Fatalf("a current shard judged %v (%t), want the stamped id live and the other not", current, ok)
	}
}

// Stamps are the Redis node's, so a writer's clock never enters them. A
// reader whose clock runs ahead of the node's judges each stamp against the
// set's newest, not its own clock, until it is a window ahead, and then admits
// everyone; one whose clock runs behind only admits more. No skew refuses a
// stamp that is current by the node's clock.
func TestProviderLivenessStampsClockSkew(t *testing.T) {
	window := providerLivenessLeaseWindow.Milliseconds()
	node := int64(1_800_000_000_000)
	stamps := []int64{node, node - 100_000, node - window - 1}
	for _, c := range []struct {
		skew    int64
		ok      bool
		current []bool
	}{
		// in step
		{skew: 0, ok: true, current: []bool{true, true, false}},
		// ahead by almost the window: by its own clock the second stamp would
		// be 249 s old
		{skew: window - 1000, ok: true, current: []bool{true, true, false}},
		// ahead by more than the window: every set looks stale, and admits
		{skew: window + 1000, ok: false},
		// behind by ten minutes: the reference is its own clock, and admits
		{skew: -600_000, ok: true, current: []bool{true, true, true}},
	} {
		current, ok := judgeProviderLivenessStamps(node+c.skew, node, stamps, providerLivenessLeaseWindow)
		if ok != c.ok || !slices.Equal(current, c.current) {
			t.Fatalf("reader skew %d ms: judged %v (%t), want %v (%t)", c.skew, current, ok, c.current, c.ok)
		}
	}
}

// A process reads the live handlers once per max age; a failed read keeps the
// previous one until it is too stale, and then the handlers are unknown. While
// one caller reads, the others use the previous read.
func TestProviderLivenessHandlersReuseTheirRead(t *testing.T) {
	ctx := context.Background()
	handlerIds := []server.Id{server.NewId()}
	var loadErr error
	loadCount := 0
	var minHeartbeatTimes []time.Time
	var during func()
	handlers := &providerLivenessHandlers{
		load: func(ctx context.Context, minHeartbeatTime time.Time) ([]server.Id, error) {
			loadCount += 1
			minHeartbeatTimes = append(minHeartbeatTimes, minHeartbeatTime)
			if during != nil {
				during()
			}
			return handlerIds, loadErr
		},
	}
	start := time.UnixMilli(1_800_000_000_000)
	check := func(label string, now time.Time, wantLoadCount int, wantKnown bool) {
		t.Helper()
		got, known := handlers.get(ctx, now)
		if loadCount != wantLoadCount || known != wantKnown || (known && !slices.Equal(got, handlerIds)) {
			t.Fatalf("%s: %d reads, known %t, want %d reads, known %t", label, loadCount, known, wantLoadCount, wantKnown)
		}
	}
	check("first", start, 1, true)
	if !minHeartbeatTimes[0].Equal(start.Add(-2 * NetworkClientHandlerHeartbeatTimeout)) {
		t.Fatalf("read handlers with a heartbeat since %s, want two heartbeat timeouts", start.Sub(minHeartbeatTimes[0]))
	}
	check("reused", start.Add(providerLivenessHandlersMaxAge-time.Millisecond), 1, true)
	check("expired", start.Add(providerLivenessHandlersMaxAge), 2, true)

	loadErr = errors.New("test: the database refused this read")
	readTime := start.Add(providerLivenessHandlersMaxAge)
	check("failed, previous kept", readTime.Add(providerLivenessHandlersMaxAge), 3, true)
	check("failed, previous too stale", readTime.Add(providerLivenessHandlersMaxStaleness+time.Millisecond), 4, false)

	loadErr = nil
	check("recovered", readTime.Add(providerLivenessHandlersMaxStaleness+time.Second), 5, true)
	// a caller that finds another reading uses the previous read
	during = func() {
		if _, known := handlers.get(ctx, readTime.Add(providerLivenessHandlersMaxStaleness+2*providerLivenessHandlersMaxAge)); !known || loadCount != 6 {
			t.Fatalf("a caller during another's read read again (%d) or found nothing (%t)", loadCount, known)
		}
	}
	check("concurrent", readTime.Add(providerLivenessHandlersMaxStaleness+2*providerLivenessHandlersMaxAge), 6, true)
}

// Shard by shard in one read: an id leased within the window is live, one
// without a lease in a current shard is offline, one in a stale shard is
// unknown; duplicates are judged once, and no ids read nothing.
func TestProviderLivenessExclusionsAcrossShards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)

		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		window := providerLivenessLeaseWindow.Milliseconds()
		live := livenessTestIdInShard(3, 0)
		offline := livenessTestIdInShard(3, 1)
		lapsed := livenessTestIdInShard(11, 0)
		staleShard := livenessTestIdInShard(19, 0)
		livenessTestScore(ctx, t, providerLivenessLeaseKey(3), map[string]int64{
			live.String(): nodeMillis - 1000,
		})
		livenessTestScore(ctx, t, providerLivenessLeaseKey(11), map[string]int64{
			"test-anchor":   nodeMillis,
			lapsed.String(): nodeMillis - window - 1,
		})
		livenessTestScore(ctx, t, providerLivenessLeaseKey(19), map[string]int64{
			staleShard.String(): nodeMillis - window - 1,
		})

		clientIds := []server.Id{live, offline, lapsed, staleShard, live, lapsed}
		offlineIds, unknownIds, err := getProviderLivenessExclusions(ctx, clientIds, time.UnixMilli(nodeMillis))
		if err != nil {
			t.Fatal(err)
		}
		want := map[server.Id]string{
			live:       providerLivenessLive,
			offline:    providerLivenessOffline,
			lapsed:     providerLivenessOffline,
			staleShard: providerLivenessUnknown,
		}
		if got := livenessTestVerdicts(clientIds, offlineIds, unknownIds); !maps.Equal(got, want) {
			t.Fatalf("verdicts %v, want %v", got, want)
		}
		if len(offlineIds) != 2 || len(unknownIds) != 1 {
			t.Fatalf("judged %d offline and %d unknown of four distinct ids, want 2 and 1", len(offlineIds), len(unknownIds))
		}

		// each distinct id counts once, under the request's rank mode
		liveBefore := livenessTestVerdictCount(t, RankModeSpeed, providerLivenessLive)
		offlineBefore := livenessTestVerdictCount(t, RankModeSpeed, providerLivenessOffline)
		if _, err := readFindProviders2Liveness(ctx, clientIds, time.UnixMilli(nodeMillis), RankModeSpeed); err != nil {
			t.Fatal(err)
		}
		if livenessTestVerdictCount(t, RankModeSpeed, providerLivenessLive)-liveBefore != 1 || livenessTestVerdictCount(t, RankModeSpeed, providerLivenessOffline)-offlineBefore != 2 {
			t.Fatal("the verdicts of repeated ids were counted more than once")
		}

		offlineIds, unknownIds, err = getProviderLivenessExclusions(ctx, nil, time.UnixMilli(nodeMillis))
		if err != nil || len(offlineIds) != 0 || len(unknownIds) != 0 {
			t.Fatalf("no ids judged %v and %v (%v), want nothing", offlineIds, unknownIds, err)
		}
	})
}

// The window boundary in Redis: the shard's newest stamp is the reference, a
// lease exactly the window older is live, and one a millisecond older is not.
func TestProviderLivenessExclusionsWindowBoundaryInRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)

		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		window := providerLivenessLeaseWindow.Milliseconds()
		atWindow := livenessTestIdInShard(5, 0)
		pastWindow := livenessTestIdInShard(5, 1)
		withinWindow := livenessTestIdInShard(5, 2)
		livenessTestScore(ctx, t, providerLivenessLeaseKey(5), map[string]int64{
			"test-anchor":         nodeMillis,
			atWindow.String():     nodeMillis - window,
			pastWindow.String():   nodeMillis - window - 1,
			withinWindow.String(): nodeMillis - window + 1,
		})
		clientIds := []server.Id{atWindow, pastWindow, withinWindow}
		// the reader's clock a second after the node's: the reference is the
		// shard's newest stamp
		offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, time.UnixMilli(nodeMillis+1000))
		if err != nil {
			t.Fatal(err)
		}
		want := map[server.Id]string{
			atWindow:     providerLivenessLive,
			pastWindow:   providerLivenessOffline,
			withinWindow: providerLivenessLive,
		}
		if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
			t.Fatalf("verdicts %v, want %v", got, want)
		}
	})
}

// A mixed connect fleet: while any live handler has no current stamp, a
// connect process may be serving providers without the writer, so every id is
// unknown and admitted, and the coverage gauge says how much is stamped. Every
// live handler stamped enforces; a handler whose heartbeat stopped needs no
// stamp; a stamp past its window fails open again.
func TestProviderLivenessExclusionsNeedEveryLiveHandlerStamped(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		stamped := CreateNetworkClientHandler(ctx)
		unstamped := CreateNetworkClientHandler(ctx)
		retired := CreateNetworkClientHandler(ctx)
		livenessTestHeartbeat(ctx, retired, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout-time.Second))
		if err := PublishProviderLivenessWriter(ctx, stamped); err != nil {
			t.Fatal(err)
		}

		leased := livenessTestIdInShard(9, 0)
		leaseless := livenessTestIdInShard(9, 1)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		clientIds := []server.Id{leased, leaseless}
		read := func(label string, want map[server.Id]string, wantCoverage float64) {
			t.Helper()
			offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
			if err != nil {
				t.Fatal(err)
			}
			if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
			if coverage := livenessTestMetric(t, findProviders2LivenessWriterCoverage); coverage != wantCoverage {
				t.Fatalf("%s: writer coverage %v, want %v", label, coverage, wantCoverage)
			}
		}
		failedOpen := map[server.Id]string{leased: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		enforced := map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline}

		read("one of two live handlers stamped", failedOpen, 0.5)

		if err := PublishProviderLivenessWriter(ctx, unstamped); err != nil {
			t.Fatal(err)
		}
		// the stamps are read with every lease read
		read("every live handler stamped", enforced, 1)

		// one stamp past its window, the other current
		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		livenessTestScore(ctx, t, providerLivenessWritersKey, map[string]int64{
			unstamped.String(): nodeMillis - providerLivenessWriterWindow.Milliseconds() - 1,
		})
		if err := PublishProviderLivenessWriter(ctx, stamped); err != nil {
			t.Fatal(err)
		}
		read("a stale stamp", failedOpen, 0.5)
	})
}

// A stamp older than its window is a writer that stopped: with every live
// handler stamped but one stamp stale, every id is unknown and admitted, and
// the same handler stamped again enforces.
func TestProviderLivenessExclusionsFailOpenOnAStaleWriterStamp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		current := CreateNetworkClientHandler(ctx)
		stale := CreateNetworkClientHandler(ctx)
		if err := PublishProviderLivenessWriter(ctx, current); err != nil {
			t.Fatal(err)
		}
		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		livenessTestScore(ctx, t, providerLivenessWritersKey, map[string]int64{
			stale.String(): nodeMillis - providerLivenessWriterWindow.Milliseconds() - 1,
		})
		if err := PublishProviderLivenessWriter(ctx, current); err != nil {
			t.Fatal(err)
		}

		leased := livenessTestIdInShard(9, 0)
		leaseless := livenessTestIdInShard(9, 1)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		clientIds := []server.Id{leased, leaseless}
		offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatal(err)
		}
		want := map[server.Id]string{leased: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
			t.Fatalf("a stale writer stamp: verdicts %v, want %v", got, want)
		}

		if err := PublishProviderLivenessWriter(ctx, stale); err != nil {
			t.Fatal(err)
		}
		offline, unknown, err = getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatal(err)
		}
		want = map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline}
		if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
			t.Fatalf("the writer stamped again: verdicts %v, want %v", got, want)
		}
	})
}

// A failed read fails open and counts: a shard whose commands fail admits its
// ids while a current shard still enforces, and a writer set that cannot be
// read admits every id. Neither is an error of the request.
func TestProviderLivenessExclusionsFailOpenOnRedisErrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)

		broken := livenessTestIdInShard(13, 0)
		leaseless := livenessTestIdInShard(14, 0)
		if err := RefreshProviderLivenessLease(ctx, livenessTestIdInShard(14, 1)); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Set(ctx, providerLivenessLeaseKey(13), "test: not a sorted set", time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		clientIds := []server.Id{broken, leaseless}

		errorsBefore := livenessTestMetric(t, findProviders2LivenessReadErrors)
		offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatalf("a failed shard read failed the request: %v", err)
		}
		want := map[server.Id]string{broken: providerLivenessUnknown, leaseless: providerLivenessOffline}
		if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
			t.Fatalf("verdicts %v, want %v", got, want)
		}
		if count := livenessTestMetric(t, findProviders2LivenessReadErrors) - errorsBefore; count != 1 {
			t.Fatalf("counted %v read errors, want 1", count)
		}

		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Del(ctx, providerLivenessWritersKey).Err(); err != nil {
				t.Fatal(err)
			}
			if err := r.Set(ctx, providerLivenessWritersKey, "test: not a sorted set", time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		offline, unknown, err = getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatalf("a failed writer set read failed the request: %v", err)
		}
		want = map[server.Id]string{broken: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
			t.Fatalf("verdicts %v, want %v", got, want)
		}
		if count := livenessTestMetric(t, findProviders2LivenessReadErrors) - errorsBefore; count != 2 {
			t.Fatalf("counted %v read errors, want 2", count)
		}

		// a canceled request is the request's own error
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := getProviderLivenessExclusions(canceledCtx, clientIds, server.NowUtc()); !errors.Is(err, context.Canceled) {
			t.Fatalf("a canceled read returned %v, want the cancellation", err)
		}
	})
}

// One read pipelined across every shard of a real Redis cluster: each shard is
// one slot on whichever node holds it, and a missing, current or lapsed lease
// is judged as on one node. It runs only against a disposable local cluster
// that WARP_TEST_REDIS_CLUSTER names by one master's loopback host:port, as
// TestRedisShardSubscriptionFollowsSlotMigration does, and refuses a cluster
// with more than six nodes or any stored key.
func TestProviderLivenessExclusionsPipelineAcrossClusterShards(t *testing.T) {
	authority := os.Getenv("WARP_TEST_REDIS_CLUSTER")
	if authority == "" {
		t.Skip("WARP_TEST_REDIS_CLUSTER names no disposable local cluster")
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		t.Fatalf("WARP_TEST_REDIS_CLUSTER=%s is not a loopback host:port", authority)
	}
	// the production data tunnel forwards 5432, 6432 and 6379-6427 to loopback
	if portNumber, err := strconv.Atoi(port); err != nil || portNumber == 5432 || portNumber == 6432 || (6379 <= portNumber && portNumber <= 6427) {
		t.Fatalf("WARP_TEST_REDIS_CLUSTER=%s is a production data tunnel port; use a disposable cluster on other ports", authority)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	clusterClient := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{authority}})
	defer clusterClient.Close()

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
	masterAddrs := []string{}
	err = clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		keyCount, err := node.DBSize(ctx).Result()
		if err != nil {
			return err
		}
		if keyCount != 0 {
			return fmt.Errorf("master %s stores %d keys", node.Options().Addr, keyCount)
		}
		masterAddrs = append(masterAddrs, node.Options().Addr)
		return nil
	})
	if err != nil {
		t.Fatalf("refusing a cluster that is not empty: %v", err)
	}
	if len(masterAddrs) < 2 {
		t.Fatalf("the cluster has %d masters, want more than one to spread the shards", len(masterAddrs))
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		clusterClient.ForEachMaster(cleanupCtx, func(ctx context.Context, node *redis.Client) error {
			return node.FlushDB(ctx).Err()
		})
	}()

	// the process's pool opens on the cluster, as in production
	popVaultRedis := server.Vault.PushSimpleResource("redis.yml", []byte(fmt.Sprintf("authority: %q\npassword: \"\"\ncluster: true\n", authority)))
	popConfigRedis := server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 8\n"))
	server.RedisReset()
	t.Cleanup(func() {
		popConfigRedis()
		popVaultRedis()
		server.RedisReset()
	})
	defaultProviderLivenessHandlers.reset()
	t.Cleanup(defaultProviderLivenessHandlers.reset)
	handlerId := server.NewId()
	previousLoad := defaultProviderLivenessHandlers.load
	defaultProviderLivenessHandlers.load = func(context.Context, time.Time) ([]server.Id, error) {
		return []server.Id{handlerId}, nil
	}
	t.Cleanup(func() {
		defaultProviderLivenessHandlers.load = previousLoad
	})
	if err := PublishProviderLivenessWriter(ctx, handlerId); err != nil {
		t.Fatal(err)
	}

	// a leased, an unleased and a lapsed id in every shard
	nodeMillis := int64(0)
	clientIds := []server.Id{}
	want := map[server.Id]string{}
	for shard := range providerLivenessLeaseShardCount {
		leased := livenessTestIdInShard(shard, 0)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		refreshTime, ok := Testing_GetProviderLivenessLease(ctx, leased)
		if !ok {
			t.Fatalf("shard %d: the refresh wrote no lease", shard)
		}
		nodeMillis = max(nodeMillis, refreshTime.UnixMilli())
		lapsed := livenessTestIdInShard(shard, 2)
		if err := clusterClient.ZAdd(ctx, providerLivenessLeaseKey(shard), redis.Z{
			Score:  float64(refreshTime.UnixMilli() - providerLivenessLeaseWindow.Milliseconds() - 1),
			Member: lapsed.String(),
		}).Err(); err != nil {
			t.Fatal(err)
		}
		unleased := livenessTestIdInShard(shard, 1)
		clientIds = append(clientIds, leased, unleased, lapsed)
		want[leased] = providerLivenessLive
		want[unleased] = providerLivenessOffline
		want[lapsed] = providerLivenessOffline
	}
	shardMasterAddrs := map[string]bool{}
	for shard := range providerLivenessLeaseShardCount {
		master, err := clusterClient.MasterForKey(ctx, providerLivenessLeaseKey(shard))
		if err != nil {
			t.Fatal(err)
		}
		shardMasterAddrs[master.Options().Addr] = true
	}
	if len(shardMasterAddrs) < 2 {
		t.Fatal("every shard is on one master; the read would not cross nodes")
	}

	offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, time.UnixMilli(nodeMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
		mismatches := 0
		for clientId, verdict := range want {
			if got[clientId] != verdict {
				mismatches += 1
			}
		}
		t.Fatalf("%d of %d verdicts across the cluster's shards differ", mismatches, len(want))
	}
}
