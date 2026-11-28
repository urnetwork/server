// The provider liveness lease's reader (provider_liveness_lease_read.go): the
// window and staleness boundaries to the millisecond, clock skew, the cached
// live handlers, writer coverage of a mixed connect fleet, ids across shards,
// duplicates, and failures that fail open and count, or take the last verdict,
// within their request. The Redis tests run on the local test env; the
// cluster test runs on a disposable local cluster. The lease cache's own
// mechanics run on fakes in provider_liveness_lease_cache_test.go.
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
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
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
	livenessTestForgetCoverage()
	t.Cleanup(livenessTestForgetCoverage)
}

// Forgets the process's read of the live handlers, its coverage verdict and
// its lease verdicts, as the next read past their ages would, once the
// background work begun before has ended.
func livenessTestForgetCoverage() {
	defaultProviderLivenessHandlers.reset()
	defaultProviderLivenessCoverage.reset()
	defaultProviderLivenessLeases.reset()
}

// Runs the process's lease cache on a clock the test sets, from a cache with
// no verdict, until the test ends. Returns the clock's setter.
func livenessTestLeaseClock(t testing.TB, start time.Time) func(time.Time) {
	t.Helper()
	var clockLock sync.Mutex
	clock := start
	previousNow := defaultProviderLivenessLeases.now
	defaultProviderLivenessLeases.now = func() time.Time {
		clockLock.Lock()
		defer clockLock.Unlock()
		return clock
	}
	defaultProviderLivenessLeases.reset()
	t.Cleanup(func() {
		defaultProviderLivenessLeases.now = previousNow
		defaultProviderLivenessLeases.reset()
	})
	return func(now time.Time) {
		clockLock.Lock()
		defer clockLock.Unlock()
		clock = now
	}
}

// The writer coverage gauge, and whether the process has exported it.
func livenessTestCoverage(t testing.TB) (float64, bool) {
	t.Helper()
	if testutil.CollectAndCount(findProviders2LivenessWriterCoverage) == 0 {
		return 0, false
	}
	return testutil.ToFloat64(findProviders2LivenessWriterCoverage.WithLabelValues()), true
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
	start := time.UnixMilli(1_800_000_000_000)
	clock := start
	handlers := &providerLivenessHandlers{
		load: func(ctx context.Context, minHeartbeatTime time.Time) ([]server.Id, error) {
			loadCount += 1
			minHeartbeatTimes = append(minHeartbeatTimes, minHeartbeatTime)
			if during != nil {
				during()
			}
			return handlerIds, loadErr
		},
		now: func() time.Time {
			return clock
		},
	}
	check := func(label string, now time.Time, wantLoadCount int, wantKnown bool) {
		t.Helper()
		clock = now
		got, known := handlers.get(ctx)
		if loadCount != wantLoadCount || known != wantKnown || (known && !slices.Equal(got, handlerIds)) {
			t.Fatalf("%s: %d reads, known %t, want %d reads, known %t", label, loadCount, known, wantLoadCount, wantKnown)
		}
	}
	wallBefore := server.NowUtc()
	check("first", start, 1, true)
	// the heartbeats are wall time, whatever the cache's clock
	if since := wallBefore.Sub(minHeartbeatTimes[0]); since < 2*NetworkClientHandlerHeartbeatTimeout-time.Second || 2*NetworkClientHandlerHeartbeatTimeout+time.Minute < since {
		t.Fatalf("read handlers with a heartbeat since %s ago, want two heartbeat timeouts", since)
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
		if _, known := handlers.get(ctx); !known || loadCount != 6 {
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
		offlineIds, unknownIds, err := getProviderLivenessExclusions(ctx, nil, clientIds, time.UnixMilli(nodeMillis))
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
		if _, err := readFindProviders2Liveness(ctx, nil, clientIds, time.UnixMilli(nodeMillis), RankModeSpeed); err != nil {
			t.Fatal(err)
		}
		if livenessTestVerdictCount(t, RankModeSpeed, providerLivenessLive)-liveBefore != 1 || livenessTestVerdictCount(t, RankModeSpeed, providerLivenessOffline)-offlineBefore != 2 {
			t.Fatal("the verdicts of repeated ids were counted more than once")
		}

		offlineIds, unknownIds, err = getProviderLivenessExclusions(ctx, nil, nil, time.UnixMilli(nodeMillis))
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
		offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, time.UnixMilli(nodeMillis+1000))
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
			// a verdict is reused for its cache age; judge afresh
			defaultProviderLivenessCoverage.reset()
			offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
			if err != nil {
				t.Fatal(err)
			}
			if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
			if coverage, ok := livenessTestCoverage(t); !ok || coverage != wantCoverage {
				t.Fatalf("%s: writer coverage %v (%t), want %v", label, coverage, ok, wantCoverage)
			}
		}
		failedOpen := map[server.Id]string{leased: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		enforced := map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline}

		read("one of two live handlers stamped", failedOpen, 0.5)

		if err := PublishProviderLivenessWriter(ctx, unstamped); err != nil {
			t.Fatal(err)
		}
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
		offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
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
		defaultProviderLivenessCoverage.reset()
		offline, unknown, err = getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
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
		offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
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

		if coverage, ok := livenessTestCoverage(t); !ok || coverage != 1 {
			t.Fatalf("writer coverage %v (%t) before the writer set fails, want 1", coverage, ok)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Del(ctx, providerLivenessWritersKey).Err(); err != nil {
				t.Fatal(err)
			}
			if err := r.Set(ctx, providerLivenessWritersKey, "test: not a sorted set", time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		defaultProviderLivenessCoverage.reset()
		offline, unknown, err = getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
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
		// failing open on an unreadable writer set does not leave the last
		// coverage on the gauge
		if coverage, ok := livenessTestCoverage(t); !ok || coverage != 0 {
			t.Fatalf("writer coverage %v (%t) after the writer set failed, want 0", coverage, ok)
		}

		// a canceled request is the request's own error
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := getProviderLivenessExclusions(canceledCtx, nil, clientIds, server.NowUtc()); !errors.Is(err, context.Canceled) {
			t.Fatalf("a canceled read returned %v, want the cancellation", err)
		}
	})
}

// Observes the deadline-bound pool's pipelines that touch the lease's keys:
// counts the shard commands, and while blocking, holds a pipeline that
// touches a key with the prefix until its context ends, as a socket read on a
// wedged node does.
type livenessTestPoolHook struct {
	shardCommands atomic.Int64
	blockPrefix   atomic.Pointer[string]
	failPrefix    atomic.Pointer[string]
}

func (self *livenessTestPoolHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *livenessTestPoolHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (self *livenessTestPoolHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		blocked := false
		for _, cmd := range cmds {
			args := cmd.Args()
			if len(args) < 2 {
				continue
			}
			key, _ := args[1].(string)
			if strings.HasPrefix(key, "{provider_live:") {
				self.shardCommands.Add(1)
			}
			if prefix := self.blockPrefix.Load(); prefix != nil && strings.HasPrefix(key, *prefix) {
				blocked = true
			}
		}
		if blocked {
			<-ctx.Done()
			for _, cmd := range cmds {
				cmd.SetErr(ctx.Err())
			}
			return ctx.Err()
		}
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			args := cmd.Args()
			if len(args) < 2 {
				continue
			}
			key, _ := args[1].(string)
			if prefix := self.failPrefix.Load(); prefix != nil && strings.HasPrefix(key, *prefix) {
				err = errors.New("test: shard immediately refused the command")
				cmd.SetErr(err)
			}
		}
		return err
	}
}

// Adds the hook to the deadline-bound pool, for the rest of the test env.
func livenessTestHookPool(ctx context.Context, t testing.TB) *livenessTestPoolHook {
	t.Helper()
	hook := &livenessTestPoolHook{}
	hookCtx, hookCancel := context.WithTimeout(ctx, 10*time.Second)
	defer hookCancel()
	if err := server.RedisWithDeadline(hookCtx, func(r server.RedisClient) error {
		r.AddHook(hook)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return hook
}

// Without full writer coverage a read judges nothing and reads no shard: a
// fleet with a live handler that does not write costs one writer set read per
// coverage verdict, not one command per shard per page.
func TestProviderLivenessReadSkipsShardsWithoutCoverage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		hook := livenessTestHookPool(ctx, t)
		clientIds := []server.Id{}
		for shard := range providerLivenessLeaseShardCount {
			clientIds = append(clientIds, livenessTestIdInShard(shard, 0))
		}
		coverageBefore := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenCoverage))
		for range 3 {
			offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
			if err != nil {
				t.Fatal(err)
			}
			if len(offline) != 0 || len(unknown) != len(clientIds) {
				t.Fatalf("an unstamped fleet judged %d offline and %d unknown of %d, want every one unknown", len(offline), len(unknown), len(clientIds))
			}
		}
		if count := hook.shardCommands.Load(); count != 0 {
			t.Fatalf("an unstamped fleet read the shards with %d commands, want none", count)
		}
		if count := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenCoverage)) - coverageBefore; count != 3 {
			t.Fatalf("counted %v coverage fail-opens, want 3", count)
		}
	})
}

// A slow or wedged node costs a read its budget and no more: a shard read held
// past it takes the last verdicts while they are young enough, and otherwise
// fails open; a writer set read held past it with no young verdict fails
// open. Each returns within a fraction of a second, counts a read error, and
// leaves the request alive.
func TestProviderLivenessReadFailsOpenWithinItsBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		hook := livenessTestHookPool(ctx, t)
		start := time.Now()
		setLeaseClock := livenessTestLeaseClock(t, start)
		leased := livenessTestIdInShard(7, 0)
		leaseless := livenessTestIdInShard(7, 1)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		clientIds := []server.Id{leased, leaseless}
		enforced := map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline}
		failedOpen := map[server.Id]string{leased: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		read := func(label string, want map[server.Id]string, wantReadErrors float64) {
			t.Helper()
			errorsBefore := livenessTestMetric(t, findProviders2LivenessReadErrors)
			started := time.Now()
			offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("%s: the request failed: %v", label, err)
			}
			if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
			if time.Second < elapsed {
				t.Fatalf("%s: the read took %s, want about the %s budget at most", label, elapsed, ProviderLivenessReadTimeout)
			}
			if count := livenessTestMetric(t, findProviders2LivenessReadErrors) - errorsBefore; count != wantReadErrors {
				t.Fatalf("%s: counted %v read errors, want %v", label, count, wantReadErrors)
			}
		}

		read("unhindered", enforced, 0)
		shardPrefix := "{provider_live:"
		hook.blockPrefix.Store(&shardPrefix)
		// past their fresh age the request reads the verdicts again, and when
		// that read runs past its budget takes them as they were
		staleBefore := livenessTestMetric(t, findProviders2LivenessCacheStales)
		setLeaseClock(start.Add(providerLivenessCacheFreshAge + time.Millisecond))
		read("a wedged shard node, the last verdicts young enough", enforced, 1)
		if count := livenessTestMetric(t, findProviders2LivenessCacheStales) - staleBefore; count != 2 {
			t.Fatalf("counted %v stale verdicts, want 2", count)
		}
		defaultProviderLivenessLeases.reset()
		read("a wedged shard node, no verdict", failedOpen, 1)
		hook.blockPrefix.Store(nil)

		writersPrefix := providerLivenessWritersKey
		hook.blockPrefix.Store(&writersPrefix)
		defaultProviderLivenessCoverage.reset()
		read("a wedged writer set node, no coverage verdict", failedOpen, 1)
		if coverage, ok := livenessTestCoverage(t); !ok || coverage != 0 {
			t.Fatalf("writer coverage %v (%t) after the writer set read ran past its budget, want 0", coverage, ok)
		}
		hook.blockPrefix.Store(nil)
	})
}

// Churn leaves a fraction of a shard's candidates offline; a lost key leaves
// nearly all of them offline until their connections refresh. More than half,
// from the minimum sample, trips the breaker; at most half, or a smaller
// sample, is judged.
func TestProviderLivenessBreakerTrips(t *testing.T) {
	minimum := providerLivenessBreakerMinCandidates
	for _, c := range []struct {
		candidates int
		offline    int
		trips      bool
	}{
		{candidates: minimum - 1, offline: minimum - 1, trips: false},
		{candidates: minimum, offline: minimum / 2, trips: false},
		{candidates: minimum, offline: minimum/2 + 1, trips: true},
		{candidates: 31, offline: 15, trips: false},
		{candidates: 31, offline: 16, trips: true},
		{candidates: 31, offline: 3, trips: false},
	} {
		if trips := providerLivenessBreakerTrips(c.candidates, c.offline); trips != c.trips {
			t.Fatalf("%d of %d offline: trips %t, want %t", c.offline, c.candidates, trips, c.trips)
		}
	}
}

// A shard whose key was lost (a node restart, an empty failover, an eviction)
// has a current newest lease again as soon as one connection refreshes, while
// every other lease it held is gone until its connection refreshes. Its
// candidates read offline together, and the breaker admits them as unknown; a
// shard with one leaseless candidate of ten is judged.
func TestProviderLivenessLostShardKeyTripsTheBreaker(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		clientIds := []server.Id{}
		for i := range 10 {
			clientId := livenessTestIdInShard(5, i)
			clientIds = append(clientIds, clientId)
			if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
				t.Fatal(err)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Del(ctx, providerLivenessLeaseKey(5)).Err(); err != nil {
				t.Fatal(err)
			}
		})
		// the first connection to refresh after the loss
		if err := RefreshProviderLivenessLease(ctx, livenessTestIdInShard(5, 10)); err != nil {
			t.Fatal(err)
		}
		breakerBefore := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenOfflineFraction))
		offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatal(err)
		}
		if len(offline) != 0 || len(unknown) != len(clientIds) {
			t.Fatalf("a lost shard key judged %d offline and %d unknown of %d, want every one unknown", len(offline), len(unknown), len(clientIds))
		}
		if livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenOfflineFraction))-breakerBefore != 1 {
			t.Fatal("the breaker's fail-open was not counted")
		}

		// the connections refresh, all but one, and the verdicts read after the
		// loss age out of the cache
		for _, clientId := range clientIds[1:] {
			if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
				t.Fatal(err)
			}
		}
		defaultProviderLivenessLeases.reset()
		offline, unknown, err = getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatal(err)
		}
		if len(unknown) != 0 || len(offline) != 1 || !offline[clientIds[0]] {
			t.Fatalf("one leaseless candidate of ten judged %d offline and %d unknown, want it alone offline", len(offline), len(unknown))
		}
	})
}

// A process exports no writer coverage until its first verdict, then the
// verdict's share; handlers it cannot read are a share of 0.
func TestProviderLivenessCoverageGaugeWaitsForTheFirstVerdict(t *testing.T) {
	findProviders2LivenessWriterCoverage.Reset()
	t.Cleanup(findProviders2LivenessWriterCoverage.Reset)
	if _, ok := livenessTestCoverage(t); ok {
		t.Fatal("a process with no verdict exported writer coverage")
	}
	coverage := &providerLivenessCoverage{
		handlers: &providerLivenessHandlers{
			load: func(context.Context, time.Time) ([]server.Id, error) {
				return nil, errors.New("test: the database refused this read")
			},
			now: time.Now,
		},
		now: time.Now,
	}
	covered, reason := coverage.get(context.Background())
	if covered || reason != providerLivenessFailOpenHandlers {
		t.Fatalf("unreadable handlers: covered %t, reason %q; want not covered for the handlers", covered, reason)
	}
	if value, ok := livenessTestCoverage(t); !ok || value != 0 {
		t.Fatalf("writer coverage %v (%t) with unreadable handlers, want 0", value, ok)
	}
}

// After a shard read fails, the request does not read that shard again rather
// than wait out its budget against the same slow node: its first read pays the
// budget, and its later reads issue no command to the shard, even once the
// node answers again, and count read_backoff. Another request reads the shard
// at once.
func TestProviderLivenessReadBacksOffWithinItsRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		hook := livenessTestHookPool(ctx, t)
		leased := livenessTestIdInShard(7, 0)
		leaseless := livenessTestIdInShard(7, 1)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		clientIds := []server.Id{leased, leaseless}
		request := newProviderLivenessRequest()
		backoffBefore := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenReadBackoff))
		shardPrefix := "{provider_live:"
		hook.blockPrefix.Store(&shardPrefix)
		started := time.Now()
		for range 4 {
			offline, unknown, err := getProviderLivenessExclusions(ctx, request, clientIds, livenessTestNodeTime(ctx, t))
			if err != nil {
				t.Fatal(err)
			}
			if len(offline) != 0 || len(unknown) != 2 {
				t.Fatalf("a request's read against a wedged node judged %d offline and %d unknown, want both unknown", len(offline), len(unknown))
			}
		}
		elapsed := time.Since(started)
		commands := hook.shardCommands.Load()
		if 2*ProviderLivenessReadTimeout+500*time.Millisecond < elapsed || 2 < commands {
			t.Fatalf("four reads of one request against a wedged node took %s and %d shard commands, want one read's budget and its 2 commands", elapsed, commands)
		}
		if count := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenReadBackoff)) - backoffBefore; count != 3 {
			t.Fatalf("counted %v backed-off reads, want the request's 3 later reads", count)
		}
		hook.blockPrefix.Store(nil)
		offline, unknown, err := getProviderLivenessExclusions(ctx, request, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil || len(offline) != 0 || len(unknown) != 2 || hook.shardCommands.Load() != 2 {
			t.Fatalf("the request's read with the node answering again judged %d offline and %d unknown (%v) after %d shard commands, want both unknown and no command", len(offline), len(unknown), err, hook.shardCommands.Load())
		}
		offline, unknown, err = getProviderLivenessExclusions(ctx, newProviderLivenessRequest(), clientIds, livenessTestNodeTime(ctx, t))
		if err != nil || !offline[leaseless] || len(unknown) != 0 || hook.shardCommands.Load() != 4 {
			t.Fatalf("another request judged %d offline and %d unknown (%v) after %d shard commands, want the leaseless id offline from its own read", len(offline), len(unknown), err, hook.shardCommands.Load())
		}
	})
}

// Immediate per-command failures keep the healthy shards' verdicts of the same
// read. The request's later reads issue no command to the failed shard but
// still read the others; another request reads the failed shard at once and
// takes the healthy shards' verdicts from memory.
func TestProviderLivenessReadBacksOffAfterImmediateShardErrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		hook := livenessTestHookPool(ctx, t)
		failed := livenessTestIdInShard(6, 0)
		leased := livenessTestIdInShard(7, 0)
		leaseless := livenessTestIdInShard(7, 1)
		laterLeaseless := livenessTestIdInShard(8, 0)
		// shard 8 holds a current lease, so it is judged
		for _, clientId := range []server.Id{failed, leased, livenessTestIdInShard(8, 1)} {
			if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
				t.Fatal(err)
			}
		}
		now := livenessTestNodeTime(ctx, t)
		request := newProviderLivenessRequest()
		clientIds := []server.Id{failed, leased, leaseless}
		failedPrefix := providerLivenessLeaseKey(6)
		hook.failPrefix.Store(&failedPrefix)
		offline, unknown, err := getProviderLivenessExclusions(ctx, request, clientIds, now)
		if err != nil || len(offline) != 1 || !offline[leaseless] || len(unknown) != 1 || !unknown[failed] {
			t.Fatalf("immediate shard failure lost healthy verdicts: offline %v, unknown %v, error %v", offline, unknown, err)
		}
		if commands := hook.shardCommands.Load(); commands != 4 {
			t.Fatalf("first read ran %d shard commands, want four", commands)
		}
		hook.failPrefix.Store(nil)
		laterClientIds := []server.Id{failed, laterLeaseless}
		offline, unknown, err = getProviderLivenessExclusions(ctx, request, laterClientIds, now)
		if err != nil || !unknown[failed] || !offline[laterLeaseless] || hook.shardCommands.Load() != 6 {
			t.Fatalf("the request's later read returned %v/%v (%v) after %d commands, want the failed shard unread and unknown, and shard 8 read", offline, unknown, err, hook.shardCommands.Load())
		}
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := getProviderLivenessExclusions(canceledCtx, request, clientIds, now); !errors.Is(err, context.Canceled) {
			t.Fatalf("backoff hid caller cancellation: %v", err)
		}
		offline, unknown, err = getProviderLivenessExclusions(ctx, newProviderLivenessRequest(), clientIds, now)
		if err != nil || len(offline) != 1 || !offline[leaseless] || len(unknown) != 0 || hook.shardCommands.Load() != 8 {
			t.Fatalf("another request returned %v/%v (%v) after %d commands, want shard 6 read alone and every id judged", offline, unknown, err, hook.shardCommands.Load())
		}
	})
}

// The root cause of fix #4 (P90SPEED.md §4.4): one request's failed shard read
// failed every read of the process open for 5 s. Now the failure is the
// request's alone, and another request right after it is judged.
func TestProviderLivenessShardFailureStaysWithItsRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		hook := livenessTestHookPool(ctx, t)
		leased := livenessTestIdInShard(7, 0)
		leaseless := livenessTestIdInShard(7, 1)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		clientIds := []server.Id{leased, leaseless}
		failedPrefix := providerLivenessLeaseKey(7)
		hook.failPrefix.Store(&failedPrefix)
		offline, unknown, err := getProviderLivenessExclusions(ctx, newProviderLivenessRequest(), clientIds, livenessTestNodeTime(ctx, t))
		if err != nil || len(offline) != 0 || len(unknown) != 2 {
			t.Fatalf("the failing request: offline %v, unknown %v, err %v; want both unknown", offline, unknown, err)
		}
		hook.failPrefix.Store(nil)
		offline, unknown, err = getProviderLivenessExclusions(ctx, newProviderLivenessRequest(), clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatal(err)
		}
		want := map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline}
		if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
			t.Fatalf("another request after one request's failed shard read: verdicts %v, want %v", got, want)
		}
	})
}

// The root cause of fix #4: a failed shard read admitted its candidates though
// they had been judged a moment before. Now a candidate takes its last verdict
// while it is at most providerLivenessCacheMaxAge old, to the nanosecond, and
// fails open past it.
func TestProviderLivenessShardErrorServesTheLastVerdict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		hook := livenessTestHookPool(ctx, t)
		start := time.Now()
		setLeaseClock := livenessTestLeaseClock(t, start)
		leased := livenessTestIdInShard(7, 0)
		leaseless := livenessTestIdInShard(7, 1)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		clientIds := []server.Id{leased, leaseless}
		enforced := map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline}
		failedOpen := map[server.Id]string{leased: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		read := func(label string, at time.Duration, want map[server.Id]string) {
			t.Helper()
			setLeaseClock(start.Add(at))
			offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
		}

		read("the first read", 0, enforced)
		failedPrefix := providerLivenessLeaseKey(7)
		hook.failPrefix.Store(&failedPrefix)
		staleBefore := livenessTestMetric(t, findProviders2LivenessCacheStales)
		failOpenBefore := livenessTestMetric(t, findProviders2LivenessCacheFailOpens)
		shardErrorBefore := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenShard))
		read("a shard read error past the fresh age", providerLivenessCacheFreshAge+time.Millisecond, enforced)
		read("a shard read error at the max age", providerLivenessCacheMaxAge, enforced)
		if count := livenessTestMetric(t, findProviders2LivenessCacheStales) - staleBefore; count != 4 {
			t.Fatalf("counted %v stale verdicts, want 4", count)
		}
		if count := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenShard)) - shardErrorBefore; count != 0 {
			t.Fatalf("counted %v shard_error fail-opens while the last verdicts were young enough, want none", count)
		}
		read("a shard read error past the max age", providerLivenessCacheMaxAge+time.Nanosecond, failedOpen)
		if count := livenessTestMetric(t, findProviders2LivenessCacheFailOpens) - failOpenBefore; count != 2 {
			t.Fatalf("counted %v candidates failed open, want 2", count)
		}
		if count := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenShard)) - shardErrorBefore; count != 1 {
			t.Fatalf("counted %v shard_error fail-opens, want 1", count)
		}
		hook.failPrefix.Store(nil)
		read("the shard readable again", providerLivenessCacheMaxAge+time.Second, enforced)
	})
}

// The root cause of fix #4 on the writer set: a writer set read that ran past
// its budget was stored as the coverage verdict, which failed every read of
// the process open for the next 5 s. Now it fails open only the read that
// judged it, and a young verdict stays in use while a background judging
// fails.
func TestProviderLivenessFailedWriterReadFailsOpenOnlyItsRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		hook := livenessTestHookPool(ctx, t)
		leased := livenessTestIdInShard(9, 0)
		leaseless := livenessTestIdInShard(9, 1)
		if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
			t.Fatal(err)
		}
		var clockLock sync.Mutex
		clock := time.Now()
		previousClock := defaultProviderLivenessCoverage.now
		defaultProviderLivenessCoverage.now = func() time.Time {
			clockLock.Lock()
			defer clockLock.Unlock()
			return clock
		}
		t.Cleanup(func() {
			defaultProviderLivenessCoverage.reset()
			defaultProviderLivenessCoverage.now = previousClock
		})
		advance := func(d time.Duration) {
			clockLock.Lock()
			defer clockLock.Unlock()
			clock = clock.Add(d)
		}
		clientIds := []server.Id{leased, leaseless}
		enforced := map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline}
		failedOpen := map[server.Id]string{leased: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		read := func(label string, want map[server.Id]string) {
			t.Helper()
			offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
			if got := livenessTestVerdicts(clientIds, offline, unknown); err != nil || !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v (%v), want %v", label, got, err, want)
			}
		}

		read("covered", enforced)
		writersPrefix := providerLivenessWritersKey
		hook.blockPrefix.Store(&writersPrefix)
		advance(providerLivenessCoverageRefreshAge)
		read("the writer set wedged, the verdict young", enforced)
		// the background judging that read started runs past its budget
		defaultProviderLivenessCoverage.background.Wait()
		advance(ProviderLivenessCoverageCacheAge - providerLivenessCoverageRefreshAge)
		read("the writer set wedged when the verdict is too old", failedOpen)
		hook.blockPrefix.Store(nil)
		advance(time.Second)
		read("a second later, the writer set readable", enforced)
	})
}

// A lapse spread across shards, too thin in each for the shard breaker, would
// read most of a small location offline: a read in which more than 30% of
// the judged candidates, and at least 3, read offline admits them all, and one
// offline of ten is judged.
func TestProviderLivenessReadTripsOnAnOfflineShare(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		livenessTestFreshHandlers(t)
		CreateNetworkClientHandler(ctx)
		livenessTestStampLiveHandlers(ctx, t)
		for shard := range providerLivenessLeaseShardCount {
			if err := RefreshProviderLivenessLease(ctx, livenessTestIdInShard(shard, 9)); err != nil {
				t.Fatal(err)
			}
		}
		// six candidates in six shards, four of them lapsed
		clientIds := []server.Id{}
		for shard := range 6 {
			clientId := livenessTestIdInShard(shard, 0)
			clientIds = append(clientIds, clientId)
			if shard < 2 {
				if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
					t.Fatal(err)
				}
			}
		}
		breakerBefore := livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenReadOfflineFraction))
		offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatal(err)
		}
		if len(offline) != 0 || len(unknown) != 4 {
			t.Fatalf("four of six offline across shards judged %d offline and %d unknown, want the four unknown", len(offline), len(unknown))
		}
		if livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenReadOfflineFraction))-breakerBefore != 1 {
			t.Fatal("the read breaker's fail-open was not counted")
		}

		// one offline of ten
		clientIds = []server.Id{}
		for shard := range 10 {
			clientId := livenessTestIdInShard(shard, 1)
			clientIds = append(clientIds, clientId)
			if 0 < shard {
				if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
					t.Fatal(err)
				}
			}
		}
		offline, unknown, err = getProviderLivenessExclusions(ctx, nil, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil || len(unknown) != 0 || len(offline) != 1 || !offline[clientIds[0]] {
			t.Fatalf("one offline of ten judged %d offline and %d unknown (%v), want it alone offline", len(offline), len(unknown), err)
		}
	})
}

// The read breaker's thresholds: more than 30% of the judged candidates, and
// at least 3 of them.
func TestProviderLivenessReadBreakerTrips(t *testing.T) {
	for _, c := range []struct {
		judged  int
		offline int
		trips   bool
	}{
		{judged: 2, offline: 2, trips: false},
		{judged: 3, offline: 3, trips: true},
		{judged: 10, offline: 3, trips: false},
		{judged: 10, offline: 4, trips: true},
		{judged: 1000, offline: 100, trips: false},
		{judged: 1000, offline: 301, trips: true},
	} {
		if trips := providerLivenessReadBreakerTrips(c.judged, c.offline); trips != c.trips {
			t.Fatalf("%d of %d offline: trips %t, want %t", c.offline, c.judged, trips, c.trips)
		}
	}
}

// The read of the live handlers is bounded and detached from the request that
// finds it due: a request already canceled still has it read, and a read that
// hangs ends at its bound, the handlers unknown.
func TestProviderLivenessHandlersLoadIsBoundedAndDetached(t *testing.T) {
	requestCtx, requestCancel := context.WithCancel(context.Background())
	requestCancel()
	loadCanceled := make(chan bool, 1)
	handlers := &providerLivenessHandlers{
		load: func(ctx context.Context, minHeartbeatTime time.Time) ([]server.Id, error) {
			loadCanceled <- ctx.Err() != nil
			<-ctx.Done()
			return nil, ctx.Err()
		},
		now: time.Now,
	}
	started := time.Now()
	_, known := handlers.get(requestCtx)
	elapsed := time.Since(started)
	if <-loadCanceled {
		t.Fatal("the read of the live handlers took the request's cancellation")
	}
	if known {
		t.Fatal("a read that hung made the handlers known")
	}
	if elapsed < ProviderLivenessHandlersReadTimeout || 2*ProviderLivenessHandlersReadTimeout < elapsed {
		t.Fatalf("a hung read returned after %s, want its %s bound", elapsed, ProviderLivenessHandlersReadTimeout)
	}
}

// One caller judges coverage at a time, detached from its request. The first
// verdict is judged by the caller that needs it, a canceled request's judging
// stored as well. From the refresh age a verdict is judged again in the
// background by the first caller to use it, while every caller keeps using it
// without waiting; at the cache age, with that judging still running, callers
// fail open for coverage, and the judging's verdict, as of its start, serves
// once it ends.
func TestProviderLivenessCoverageJudgesOneAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handlerId := server.NewId()
		var stampReads atomic.Int64
		var holdNextRead atomic.Bool
		release := make(chan struct{})
		coverage := &providerLivenessCoverage{
			handlers: &providerLivenessHandlers{
				load: func(context.Context, time.Time) ([]server.Id, error) {
					return []server.Id{handlerId}, nil
				},
				now: time.Now,
			},
			readStamps: func(ctx context.Context, members []string) (int64, []int64, error) {
				stampReads.Add(1)
				if holdNextRead.Swap(false) {
					<-release
				}
				nowMillis := server.NowUtc().UnixMilli()
				return nowMillis, []int64{nowMillis}, nil
			},
			now: time.Now,
		}
		ctx := context.Background()
		check := func(label string, wantCovered bool, wantReason string, wantReads int64) {
			t.Helper()
			covered, reason := coverage.get(ctx)
			synctest.Wait()
			if covered != wantCovered || reason != wantReason || stampReads.Load() != wantReads {
				t.Fatalf("%s: covered %t, reason %q after %d writer set reads; want %t, %q after %d", label, covered, reason, stampReads.Load(), wantCovered, wantReason, wantReads)
			}
		}

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if covered, reason := coverage.get(canceledCtx); !covered || reason != "" {
			t.Fatalf("a canceled request's judging: covered %t, reason %q; want its verdict", covered, reason)
		}
		time.Sleep(providerLivenessCoverageRefreshAge - time.Nanosecond)
		check("within the refresh age", true, "", 1)

		// the first caller past the refresh age starts a judging and goes on
		time.Sleep(time.Nanosecond)
		holdNextRead.Store(true)
		check("at the refresh age", true, "", 2)
		check("while that judging runs", true, "", 2)
		time.Sleep(ProviderLivenessCoverageCacheAge - providerLivenessCoverageRefreshAge - time.Nanosecond)
		check("just under the cache age, the judging still running", true, "", 2)
		time.Sleep(time.Nanosecond)
		check("at the cache age, the judging still running", false, providerLivenessFailOpenCoverage, 2)

		// the judging's verdict dates from its start, at the refresh age: it is
		// used, and judged again in the background at once
		close(release)
		synctest.Wait()
		check("once the judging ends", true, "", 3)
	})
}

// A judging that cannot read the writer set stores nothing. The young verdict
// stays in use, and each read past the refresh age tries the judging again in
// the background, until the cache age. A read that then finds no young verdict
// judges itself and fails open, for writers_error, with the gauge at 0, and
// stores nothing: the next read judges again.
func TestProviderLivenessCoverageKeepsItsVerdictThroughFailedReads(t *testing.T) {
	findProviders2LivenessWriterCoverage.Reset()
	t.Cleanup(findProviders2LivenessWriterCoverage.Reset)
	synctest.Test(t, func(t *testing.T) {
		handlerId := server.NewId()
		var stampReads atomic.Int64
		var failReads atomic.Bool
		coverage := &providerLivenessCoverage{
			handlers: &providerLivenessHandlers{
				load: func(context.Context, time.Time) ([]server.Id, error) {
					return []server.Id{handlerId}, nil
				},
				now: time.Now,
			},
			readStamps: func(ctx context.Context, members []string) (int64, []int64, error) {
				stampReads.Add(1)
				if failReads.Load() {
					return 0, nil, errors.New("test: the writer set read ran past its budget")
				}
				nowMillis := server.NowUtc().UnixMilli()
				return nowMillis, []int64{nowMillis}, nil
			},
			now: time.Now,
		}
		ctx := context.Background()
		start := time.Now()
		check := func(label string, at time.Duration, wantCovered bool, wantReason string, wantReads int64) {
			t.Helper()
			time.Sleep(start.Add(at).Sub(time.Now()))
			covered, reason := coverage.get(ctx)
			synctest.Wait()
			if covered != wantCovered || reason != wantReason || stampReads.Load() != wantReads {
				t.Fatalf("%s: covered %t, reason %q after %d writer set reads; want %t, %q after %d", label, covered, reason, stampReads.Load(), wantCovered, wantReason, wantReads)
			}
		}

		check("judged", 0, true, "", 1)
		errorsBefore := livenessTestMetric(t, findProviders2LivenessReadErrors)
		failReads.Store(true)
		check("a failed judging in the background", providerLivenessCoverageRefreshAge, true, "", 2)
		check("tried again", 2*providerLivenessCoverageRefreshAge, true, "", 3)
		check("just under the cache age", ProviderLivenessCoverageCacheAge-time.Nanosecond, true, "", 4)
		if value, ok := livenessTestCoverage(t); !ok || value != 1 {
			t.Fatalf("writer coverage %v (%t) while the young verdict serves, want its share, 1", value, ok)
		}
		check("at the cache age", ProviderLivenessCoverageCacheAge, false, providerLivenessFailOpenWriters, 5)
		if value, ok := livenessTestCoverage(t); !ok || value != 0 {
			t.Fatalf("writer coverage %v (%t) after a read failed open, want 0", value, ok)
		}
		if count := livenessTestMetric(t, findProviders2LivenessReadErrors) - errorsBefore; count != 4 {
			t.Fatalf("counted %v read errors, want 4", count)
		}
		check("the next read judges again", ProviderLivenessCoverageCacheAge, false, providerLivenessFailOpenWriters, 6)
		failReads.Store(false)
		check("the writer set readable", ProviderLivenessCoverageCacheAge+time.Nanosecond, true, "", 7)
	})
}

// Whether address can be a disposable local cluster's node: a loopback host on
// a port no production data tunnel forwards (server.Testing_LoopbackTunnelPort).
func livenessTestDisposableClusterNode(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("refusing cluster node %q: %w", address, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing cluster node %s: not a loopback host", address)
	}
	if port, err := strconv.ParseUint(portText, 10, 16); err != nil || server.Testing_LoopbackTunnelPort(uint16(port)) {
		return fmt.Errorf("refusing cluster node %s: a production data tunnel port", address)
	}
	return nil
}

// The cluster diagnostic dials and keeps only nodes on a loopback host and off
// the production data tunnel's ports.
func TestLivenessTestDisposableClusterNodeRefusesTunnelPortsAndOtherHosts(t *testing.T) {
	for address, allowed := range map[string]bool{
		"127.0.0.1:30001":   true,
		"localhost:30002":   true,
		"[::1]:30003":       true,
		"127.0.0.1:6379":    false,
		"127.0.0.1:6427":    false,
		"127.0.0.1:5432":    false,
		"127.0.0.1:6432":    false,
		"192.0.2.10:30001":  false,
		"198.51.100.7:6415": false,
		":30001":            false,
		"127.0.0.1":         false,
	} {
		if err := livenessTestDisposableClusterNode(address); (err == nil) != allowed {
			t.Errorf("cluster node %q: err %v, want allowed %t", address, err, allowed)
		}
	}
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
	// The seed and every node the cluster names dial through the process
	// guard, as server's TestRedisShardSubscriptionFollowsSlotMigration does,
	// and only on a loopback host off the production data tunnel's ports: only
	// the seed comes from the environment, and a production cluster names its
	// own hosts.
	clusterClient := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs: []string{authority},
		Dialer: func(ctx context.Context, network string, address string) (net.Conn, error) {
			if err := livenessTestDisposableClusterNode(address); err != nil {
				return nil, err
			}
			return server.Testing_DialTestEnv(ctx, "redis", network, address)
		},
	})
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
	slots, err := clusterClient.ClusterSlots(ctx).Result()
	if err != nil {
		t.Fatalf("cluster slots: %v", err)
	}
	// every node the slots name, replicas too, before any write or the process
	// pool, whose own dials take the cluster's names as they are
	for _, slot := range slots {
		for _, node := range slot.Nodes {
			if err := livenessTestDisposableClusterNode(node.Addr); err != nil {
				t.Fatal(err)
			}
		}
	}
	expectedMasterAddrs := map[string]bool{}
	for _, slot := range slots {
		if len(slot.Nodes) == 0 {
			t.Fatal("cluster slot has no master")
		}
		expectedMasterAddrs[slot.Nodes[0].Addr] = true
	}
	census := &livenessTestMasterCensus{}
	err = clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		keyCount, err := node.DBSize(ctx).Result()
		if err != nil {
			return err
		}
		if keyCount != 0 {
			return fmt.Errorf("master %s stores %d keys", node.Options().Addr, keyCount)
		}
		census.add(node.Options().Addr)
		return nil
	})
	if err != nil {
		t.Fatalf("refusing a cluster that is not empty: %v", err)
	}
	masterAddrs := census.addresses()
	masterAddrSet := map[string]bool{}
	for _, addr := range masterAddrs {
		masterAddrSet[addr] = true
	}
	if len(masterAddrs) != len(expectedMasterAddrs) || !maps.Equal(masterAddrSet, expectedMasterAddrs) {
		t.Fatalf("master census %v, want exactly %v", masterAddrs, expectedMasterAddrs)
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
	livenessTestForgetCoverage()
	t.Cleanup(livenessTestForgetCoverage)
	handlerId := server.NewId()
	previousLoad := defaultProviderLivenessHandlers.load
	defaultProviderLivenessHandlers.load = func(context.Context, time.Time) ([]server.Id, error) {
		return []server.Id{handlerId}, nil
	}
	t.Cleanup(func() {
		// a background judging may still read the handlers
		livenessTestForgetCoverage()
		defaultProviderLivenessHandlers.load = previousLoad
	})
	if err := PublishProviderLivenessWriter(ctx, handlerId); err != nil {
		t.Fatal(err)
	}

	// four leased ids and an unleased one in every shard, and a lapsed one in
	// every eighth: a fifth of the read offline, under the read breaker
	nodeMillis := int64(0)
	clientIds := []server.Id{}
	want := map[server.Id]string{}
	for shard := range providerLivenessLeaseShardCount {
		for i := range 4 {
			leased := livenessTestIdInShard(shard, 2+i)
			if err := RefreshProviderLivenessLease(ctx, leased); err != nil {
				t.Fatal(err)
			}
			refreshTime, ok := Testing_GetProviderLivenessLease(ctx, leased)
			if !ok {
				t.Fatalf("shard %d: the refresh wrote no lease", shard)
			}
			nodeMillis = max(nodeMillis, refreshTime.UnixMilli())
			clientIds = append(clientIds, leased)
			want[leased] = providerLivenessLive
		}
		unleased := livenessTestIdInShard(shard, 1)
		clientIds = append(clientIds, unleased)
		want[unleased] = providerLivenessOffline
		if shard%8 == 0 {
			lapsed := livenessTestIdInShard(shard, 0)
			if err := clusterClient.ZAdd(ctx, providerLivenessLeaseKey(shard), redis.Z{
				Score:  float64(nodeMillis - providerLivenessLeaseWindow.Milliseconds() - 1000),
				Member: lapsed.String(),
			}).Err(); err != nil {
				t.Fatal(err)
			}
			clientIds = append(clientIds, lapsed)
			want[lapsed] = providerLivenessOffline
		}
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

	offline, unknown, err := getProviderLivenessExclusions(ctx, nil, clientIds, time.UnixMilli(nodeMillis))
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
