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
	"sync"
	"sync/atomic"
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
	livenessTestForgetCoverage()
	t.Cleanup(livenessTestForgetCoverage)
}

// Forgets the process's read of the live handlers and its coverage verdict,
// as the next read past their ages would.
func livenessTestForgetCoverage() {
	defaultProviderLivenessHandlers.reset()
	defaultProviderLivenessCoverage.reset()
	defaultProviderLivenessReadHealth.reset()
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
			// a verdict is reused for its cache age; judge afresh
			defaultProviderLivenessCoverage.reset()
			offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
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
		defaultProviderLivenessCoverage.reset()
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
		// failing open on an unreadable writer set does not leave the last
		// coverage on the gauge
		if coverage, ok := livenessTestCoverage(t); !ok || coverage != 0 {
			t.Fatalf("writer coverage %v (%t) after the writer set failed, want 0", coverage, ok)
		}

		// a canceled request is the request's own error
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := getProviderLivenessExclusions(canceledCtx, clientIds, server.NowUtc()); !errors.Is(err, context.Canceled) {
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
			offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
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
// past it, and then a writer set read held past it, each fail open within a
// fraction of a second, count a read error, and leave the request alive.
func TestProviderLivenessReadFailsOpenWithinItsBudget(t *testing.T) {
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
		failedOpen := map[server.Id]string{leased: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		read := func(label string, want map[server.Id]string) {
			t.Helper()
			errorsBefore := livenessTestMetric(t, findProviders2LivenessReadErrors)
			started := time.Now()
			offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("%s: the request failed: %v", label, err)
			}
			if got := livenessTestVerdicts(clientIds, offline, unknown); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
			if maps.Equal(want, failedOpen) {
				if time.Second < elapsed {
					t.Fatalf("%s: failing open took %s, want about the %s budget", label, elapsed, ProviderLivenessReadTimeout)
				}
				if livenessTestMetric(t, findProviders2LivenessReadErrors)-errorsBefore != 1 {
					t.Fatalf("%s: the read past its budget was not counted", label)
				}
			}
		}

		read("unhindered", map[server.Id]string{leased: providerLivenessLive, leaseless: providerLivenessOffline})
		shardPrefix := "{provider_live:"
		hook.blockPrefix.Store(&shardPrefix)
		read("a wedged shard node", failedOpen)
		hook.blockPrefix.Store(nil)

		writersPrefix := providerLivenessWritersKey
		hook.blockPrefix.Store(&writersPrefix)
		defaultProviderLivenessCoverage.reset()
		read("a wedged writer set node", failedOpen)
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
		offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil {
			t.Fatal(err)
		}
		if len(offline) != 0 || len(unknown) != len(clientIds) {
			t.Fatalf("a lost shard key judged %d offline and %d unknown of %d, want every one unknown", len(offline), len(unknown), len(clientIds))
		}
		if livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenOfflineFraction))-breakerBefore != 1 {
			t.Fatal("the breaker's fail-open was not counted")
		}

		// the connections refresh, all but one
		for _, clientId := range clientIds[1:] {
			if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
				t.Fatal(err)
			}
		}
		offline, unknown, err = getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
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
	covered, reason, err := coverage.get(context.Background())
	if err != nil || covered || reason != providerLivenessFailOpenHandlers {
		t.Fatalf("unreadable handlers: covered %t, reason %q, err %v; want not covered for the handlers", covered, reason, err)
	}
	if value, ok := livenessTestCoverage(t); !ok || value != 0 {
		t.Fatalf("writer coverage %v (%t) with unreadable handlers, want 0", value, ok)
	}
}

// After a shard read fails, the process fails every read open for the backoff
// instead of waiting out its budget against the same slow node again: one
// read pays the budget, and the reads after it issue no shard command until
// the backoff ends.
func TestProviderLivenessReadBacksOffAfterAFailedRead(t *testing.T) {
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
		clock := time.Now()
		defaultProviderLivenessReadHealth.now = func() time.Time {
			return clock
		}
		t.Cleanup(func() {
			defaultProviderLivenessReadHealth.now = time.Now
		})
		clientIds := []server.Id{leased, leaseless}
		shardPrefix := "{provider_live:"
		hook.blockPrefix.Store(&shardPrefix)
		started := time.Now()
		for range 4 {
			offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
			if err != nil {
				t.Fatal(err)
			}
			if len(offline) != 0 || len(unknown) != 2 {
				t.Fatalf("a read against a wedged node judged %d offline and %d unknown, want both unknown", len(offline), len(unknown))
			}
		}
		elapsed := time.Since(started)
		commands := hook.shardCommands.Load()
		if 2*ProviderLivenessReadTimeout+500*time.Millisecond < elapsed || 2 < commands {
			t.Fatalf("four reads against a wedged node took %s and %d shard commands, want one read's budget and its 2 commands", elapsed, commands)
		}
		hook.blockPrefix.Store(nil)
		clock = clock.Add(providerLivenessReadBackoff)
		offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
		if err != nil || !offline[leaseless] || len(unknown) != 0 {
			t.Fatalf("after the backoff the read judged %d offline and %d unknown (%v), want the leaseless id offline", len(offline), len(unknown), err)
		}
	})
}

// Immediate per-command failures retain healthy-shard answers for this read,
// but suppress further shard reads until the exact fake-clock backoff boundary.
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
		for _, clientId := range []server.Id{failed, leased} {
			if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
				t.Fatal(err)
			}
		}
		now := livenessTestNodeTime(ctx, t)
		clock := now
		previousHealthClock := defaultProviderLivenessReadHealth.now
		defaultProviderLivenessReadHealth.now = func() time.Time { return clock }
		t.Cleanup(func() { defaultProviderLivenessReadHealth.now = previousHealthClock })
		clientIds := []server.Id{failed, leased, leaseless}
		failedPrefix := providerLivenessLeaseKey(6)
		hook.failPrefix.Store(&failedPrefix)
		offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, now)
		if err != nil || len(offline) != 1 || !offline[leaseless] || len(unknown) != 1 || !unknown[failed] {
			t.Fatalf("immediate shard failure lost healthy verdicts: offline %v, unknown %v, error %v", offline, unknown, err)
		}
		if commands := hook.shardCommands.Load(); commands != 4 {
			t.Fatalf("first read ran %d shard commands, want four", commands)
		}
		for _, elapsed := range []time.Duration{0, time.Second, providerLivenessReadBackoff - time.Nanosecond} {
			clock = now.Add(elapsed)
			offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, now)
			if err != nil || len(offline) != 0 || len(unknown) != len(clientIds) || hook.shardCommands.Load() != 4 {
				t.Fatalf("at %s, backoff read returned %v/%v (%v) after %d commands", elapsed, offline, unknown, err, hook.shardCommands.Load())
			}
		}
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := getProviderLivenessExclusions(canceledCtx, clientIds, now); !errors.Is(err, context.Canceled) {
			t.Fatalf("backoff hid caller cancellation: %v", err)
		}
		hook.failPrefix.Store(nil)
		clock = now.Add(providerLivenessReadBackoff)
		offline, unknown, err = getProviderLivenessExclusions(ctx, clientIds, now)
		if err != nil || len(offline) != 1 || !offline[leaseless] || len(unknown) != 0 || hook.shardCommands.Load() != 8 {
			t.Fatalf("at backoff expiry, read returned %v/%v (%v) after %d commands", offline, unknown, err, hook.shardCommands.Load())
		}
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
		offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
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
		offline, unknown, err = getProviderLivenessExclusions(ctx, clientIds, livenessTestNodeTime(ctx, t))
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

// One caller judges coverage at a time, detached from its request: the others
// read nothing and take the previous verdict while it is younger than twice
// the cache age, or else not covered, until the judge stores its own verdict,
// which a canceled request's judging stores as well.
func TestProviderLivenessCoverageJudgesOneAtATime(t *testing.T) {
	loads := atomic.Int64{}
	entered := make(chan struct{})
	release := make(chan struct{})
	start := time.UnixMilli(1_800_000_000_000)
	clock := start
	clockLock := sync.Mutex{}
	now := func() time.Time {
		clockLock.Lock()
		defer clockLock.Unlock()
		return clock
	}
	coverage := &providerLivenessCoverage{
		handlers: &providerLivenessHandlers{
			load: func(ctx context.Context, minHeartbeatTime time.Time) ([]server.Id, error) {
				if loads.Add(1) == 2 {
					close(entered)
					<-release
				}
				return nil, errors.New("test: the database refused this read")
			},
			now: now,
		},
		now: now,
	}
	// a first verdict, judged for a request already canceled
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if covered, reason, err := coverage.get(canceledCtx); err != nil || covered || reason != providerLivenessFailOpenHandlers {
		t.Fatalf("a canceled request's judging: covered %t, reason %q, err %v; want its verdict, for the handlers", covered, reason, err)
	}

	// that verdict is young while the next judging runs, then too old
	clockLock.Lock()
	clock = start.Add(ProviderLivenessCoverageCacheAge)
	clockLock.Unlock()
	judged := make(chan struct{})
	go func() {
		defer close(judged)
		coverage.get(context.Background())
	}()
	<-entered
	if covered, reason, err := coverage.get(context.Background()); err != nil || covered || reason != providerLivenessFailOpenHandlers {
		t.Fatalf("during judging, a young previous verdict: covered %t, reason %q, err %v; want the previous verdict", covered, reason, err)
	}
	clockLock.Lock()
	clock = start.Add(2 * ProviderLivenessCoverageCacheAge)
	clockLock.Unlock()
	if covered, reason, err := coverage.get(context.Background()); err != nil || covered || reason != providerLivenessFailOpenCoverage {
		t.Fatalf("during judging, a previous verdict twice the cache age old: covered %t, reason %q, err %v; want not covered for coverage", covered, reason, err)
	}
	close(release)
	<-judged
	if count := loads.Load(); count != 2 {
		t.Fatalf("the callers read the live handlers %d times, want once per judging", count)
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
	slots, err := clusterClient.ClusterSlots(ctx).Result()
	if err != nil {
		t.Fatalf("cluster slots: %v", err)
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
