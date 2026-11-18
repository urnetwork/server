// The reader of the provider liveness lease (provider_liveness_lease.go): which
// candidates of a FindProviders2 request have no current lease.
//
// A read judges nothing unless every live connect handler, a heartbeat within
// two heartbeat timeouts, has a current stamp in the writer set; a connect
// process without the writer, mid-rollout, rolled back, built without it or
// with its lease writes stopped, would otherwise leave every provider it
// serves without a lease. The live handlers come from the database at most
// every providerLivenessHandlersMaxAge per process. The verdict on their
// stamps is used until it is ProviderLivenessCoverageCacheAge old, and judged
// again in the background from providerLivenessCoverageRefreshAge, so a
// request waits on that judging only when no verdict is young enough; a
// judging whose read fails leaves the verdict it would have replaced. While
// coverage is short no shard is read at all.
//
// The leases are read through one cache per process
// (providerLivenessLeaseCache). A request takes a candidate's verdict from
// memory while the read that saw it is at most providerLivenessCacheFreshAge
// old, and the cache reads again, in the background, the verdicts that
// requests use past providerLivenessCacheRefreshAge. A request reads only the
// candidates without such a verdict, in one pipeline on the deadline-bound
// pool within ProviderLivenessReadTimeout that retries nothing, so a slow or
// wedged node costs a request at most that budget. When that read fails, a
// candidate takes its last verdict while it is at most
// providerLivenessCacheMaxAge old, and is otherwise unknown and admitted; the
// failed shard is not read again for the rest of that request, and every
// other request reads it as usual. A shard whose newest lease is stale, or
// more than half of whose candidates read offline (a lost or stale-restored
// key, not churn), is unknown and admitted. Nothing here ever refuses a
// request.
//
// Safe for concurrent use.
package model

import (
	"context"
	"errors"
	"maps"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The verdicts of the lease, as the candidates metric labels them.
const (
	providerLivenessLive    = "live"
	providerLivenessOffline = "offline"
	providerLivenessUnknown = "unknown"
)

// Why a read admitted candidates it could not judge, as the fail-open metric
// labels them.
const (
	// the live handlers are unknown, or there are none
	providerLivenessFailOpenHandlers = "handlers"
	// a live handler has no current stamp
	providerLivenessFailOpenCoverage = "coverage"
	// no coverage verdict was young enough, and the writer set could not be
	// read within the budget
	providerLivenessFailOpenWriters = "writers_error"
	// a shard could not be read within the budget, and some of its candidates
	// had no verdict within providerLivenessCacheMaxAge
	providerLivenessFailOpenShard = "shard_error"
	// a shard has no lease within the window
	providerLivenessFailOpenStale = "stale"
	// more than half of a shard's candidates read offline
	providerLivenessFailOpenOfflineFraction = "offline_fraction"
	// more than providerLivenessReadOfflineShare of a read's judged
	// candidates read offline
	providerLivenessFailOpenReadOfflineFraction = "read_offline_fraction"
	// a shard whose read failed earlier in the same request, which does not
	// read it again, and some of its candidates had no verdict within
	// providerLivenessCacheMaxAge
	providerLivenessFailOpenReadBackoff = "read_backoff"
)

// Where the verdict of a candidate came from, as the cache metric labels them.
const (
	// memory, from a read at most providerLivenessCacheFreshAge old that
	// succeeded
	providerLivenessCacheHit = "hit"
	// memory, the last verdict of a candidate whose latest read failed, at
	// most providerLivenessCacheMaxAge old
	providerLivenessCacheStale = "stale"
	// the request's own read
	providerLivenessCacheRead = "read"
	// none: the request's read failed or was backed off, and no verdict was
	// at most providerLivenessCacheMaxAge old, so the candidate is unknown
	providerLivenessCacheFailOpen = "failopen"
)

// The outcomes of a background refresh of the lease cache.
const (
	providerLivenessRefreshOk    = "ok"
	providerLivenessRefreshError = "error"
)

var findProviders2LivenessCandidates = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_candidates_total",
	Help: "FindProviders2 candidates judged by their provider liveness lease, by the request's rank mode and verdict: live; offline, no lease within the window, left out of every draw of the request; unknown, admitted (urnetwork_findproviders2_liveness_failopens_total says why)",
}, []string{"rank_mode", "verdict"})

var findProviders2LivenessReadErrors = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_read_errors_total",
	Help: "Provider liveness lease reads of FindProviders2 that failed or ran past their budget: a request's read of its shards, a background refresh of the lease cache, or a read of the writer set; a candidate of a failed read takes its last verdict while it is at most 30 s old, and is otherwise admitted as unknown",
})

var findProviders2LivenessFailOpens = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_failopens_total",
	Help: "Provider liveness lease reads that admitted candidates they could not judge, by reason: handlers, coverage, writers_error, shard_error, read_backoff (a shard whose read failed earlier in the same request) and read_offline_fraction per read, stale and offline_fraction per shard",
}, []string{"reason"})

// No labels: the child exists from a process's first verdict on coverage, so a
// process that has read nothing exports nothing.
var findProviders2LivenessWriterCoverage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_liveness_writer_coverage",
	Help: "The share of live connect handlers with a current provider liveness writer stamp at this process's latest verdict, 0 when the handlers or the stamps could not be read and no earlier verdict was young enough to keep; leases are enforced only at 1",
}, []string{})

var findProviders2LivenessCache = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_cache_total",
	Help: "Distinct FindProviders2 candidates of reads with full writer coverage, by where their provider liveness verdict came from: hit, this process's lease cache, from a read at most 3 s old; stale, the cache's last verdict, at most 30 s old, of a candidate whose latest read failed; read, the request's own read; failopen, no verdict, the request's read failed or was backed off and the cache had none within 30 s, so the candidate is admitted as unknown",
}, []string{"result"})

var findProviders2LivenessRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_refreshes_total",
	Help: "Background refreshes of this process's provider liveness lease cache, by result (ok, error): one pipelined read, at most one at a time and one a second, of the verdicts requests used past 1 s; no request waits on them",
}, []string{"result"})

var findProviders2LivenessCacheClients = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_liveness_cache_clients",
	Help: "Providers whose provider liveness lease verdict this process's lease cache holds, at most 65,536; verdicts older than 30 s are swept",
})

var (
	findProviders2LivenessCacheHits      = findProviders2LivenessCache.WithLabelValues(providerLivenessCacheHit)
	findProviders2LivenessCacheStales    = findProviders2LivenessCache.WithLabelValues(providerLivenessCacheStale)
	findProviders2LivenessCacheReads     = findProviders2LivenessCache.WithLabelValues(providerLivenessCacheRead)
	findProviders2LivenessCacheFailOpens = findProviders2LivenessCache.WithLabelValues(providerLivenessCacheFailOpen)
)

func init() {
	prometheus.MustRegister(
		findProviders2LivenessCandidates,
		findProviders2LivenessReadErrors,
		findProviders2LivenessFailOpens,
		findProviders2LivenessWriterCoverage,
		findProviders2LivenessCache,
		findProviders2LivenessRefreshes,
		findProviders2LivenessCacheClients,
	)
	findProviders2LivenessRefreshes.WithLabelValues(providerLivenessRefreshOk)
	findProviders2LivenessRefreshes.WithLabelValues(providerLivenessRefreshError)
}

// A shard is judged by the fraction of its candidates that read offline only
// from this many candidates in one read; below it, one offline candidate is
// too much of the sample to say anything.
const providerLivenessBreakerMinCandidates = 8

// A read in which more than this share of its judged candidates, and at
// least providerLivenessReadBreakerMinOffline of them, read offline admits
// them all: churn leaves 5-10% offline, and a lapse spread across shards, too
// thin in each for the shard breaker, would otherwise empty a small location
// or a refill page.
const providerLivenessReadOfflineShare = 0.3

// Below this many offline candidates a read is judged whatever their share,
// so a small location keeps losing its one or two offline providers.
const providerLivenessReadBreakerMinOffline = 3

// How long a process reuses its read of the live connect handlers.
const providerLivenessHandlersMaxAge = 30 * time.Second

// A read of the live handlers older than this is no read: until one succeeds,
// every lease read fails open.
const providerLivenessHandlersMaxStaleness = 2 * providerLivenessHandlersMaxAge

// A coverage verdict this old is judged again, in the background, by the
// first read that uses it; it is used until ProviderLivenessCoverageCacheAge.
// Unlike a lease verdict, a covered verdict is never kept past that age when
// the writer set cannot be read: TestProviderLivenessBudgetStaysUnderTheLeaseWindow
// bounds how long a reader may enforce after a writer's stamp lapses.
const providerLivenessCoverageRefreshAge = time.Second

// A lease verdict a request uses past this age is read again by the cache in
// the background; background reads start at most this often.
const providerLivenessCacheRefreshAge = time.Second

// A request takes a lease verdict from memory while the read that saw it is
// at most this old, and reads the candidate itself otherwise. Verdicts that
// requests keep using are read again within about two refresh ages.
const providerLivenessCacheFreshAge = 3 * time.Second

// When a read fails, a candidate takes its last verdict while it is at most
// this old, and is admitted as unknown past it. A verdict older than this is
// swept from the cache.
const providerLivenessCacheMaxAge = 30 * time.Second

// The most candidates the cache holds; past it, arbitrary verdicts make room.
// No entry holds a pointer, so the collector never scans the map.
const providerLivenessCacheMaxClients = 1 << 16

// The most candidates one background refresh reads; the rest wait for the
// next.
const providerLivenessRefreshMaxClients = 1 << 13

// The budget of a background refresh, which no request waits on.
const providerLivenessRefreshReadTimeout = time.Second

// The live connect handlers, read from the database at most every
// providerLivenessHandlersMaxAge of this process's monotonic clock, by one
// caller at a time while the others use the previous read.
type providerLivenessHandlers struct {
	load func(ctx context.Context, minHeartbeatTime time.Time) ([]server.Id, error)
	now  func() time.Time

	stateLock  sync.Mutex
	handlerIds []server.Id
	// zero before the first read
	readTime time.Time
	loading  bool
}

var defaultProviderLivenessHandlers = &providerLivenessHandlers{
	load: loadLiveNetworkClientHandlerIds,
	now:  time.Now,
}

// The live handlers, and whether they are known. A failed read keeps the
// previous one until it is providerLivenessHandlersMaxStaleness old.
func (self *providerLivenessHandlers) get(ctx context.Context) ([]server.Id, bool) {
	now := self.now()
	load := false
	var handlerIds []server.Id
	var readTime time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		handlerIds, readTime = self.handlerIds, self.readTime
		if !self.loading && (readTime.IsZero() || providerLivenessHandlersMaxAge <= now.Sub(readTime)) {
			self.loading = true
			load = true
		}
	}()
	if load {
		var loadedHandlerIds []server.Id
		err := errors.New("live handlers were not read")
		func() {
			// a raise ends the read too, and the next caller reads again
			defer func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.loading = false
				if err == nil {
					self.handlerIds, self.readTime = loadedHandlerIds, now
				}
			}()
			// bounded, and detached from the request that found the read due,
			// which may end before the read does
			loadCtx, loadCancel := context.WithTimeout(context.WithoutCancel(ctx), ProviderLivenessHandlersReadTimeout)
			defer loadCancel()
			// the heartbeats are the connect hosts' wall clocks
			loadedHandlerIds, err = self.load(loadCtx, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout))
		}()
		if err == nil {
			return loadedHandlerIds, true
		}
	}
	if readTime.IsZero() || providerLivenessHandlersMaxStaleness < now.Sub(readTime) {
		return nil, false
	}
	return handlerIds, true
}

// Forgets the read, so the next lease read reads the live handlers again.
func (self *providerLivenessHandlers) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.handlerIds, self.readTime = nil, time.Time{}
}

// The connect handlers whose heartbeat is at or after minHeartbeatTime, on the
// heartbeat index. A raised database failure is returned; a programming fault
// still raises.
func loadLiveNetworkClientHandlerIds(ctx context.Context, minHeartbeatTime time.Time) (handlerIds []server.Id, returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err, ok := r.(error)
		if _, fault := r.(runtime.Error); fault || !ok {
			panic(r)
		}
		handlerIds, returnErr = nil, err
	}()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT handler_id FROM network_client_handler WHERE heartbeat_time >= $1`,
			minHeartbeatTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var handlerId server.Id
				server.Raise(result.Scan(&handlerId))
				handlerIds = append(handlerIds, handlerId)
			}
		})
	}, server.OptReadOnly(), server.OptNoRetry())
	return
}

// The verdict on writer coverage: whether every live handler has a current
// stamp, and when not, why. A verdict is used until it is
// ProviderLivenessCoverageCacheAge old by this process's monotonic clock, from
// the start of the judging that made it. From
// providerLivenessCoverageRefreshAge the first caller to use it has it judged
// again in the background, one judging at a time, while every caller keeps
// using it. Only a caller that finds no verdict young enough judges, detached
// from its request and within model's ProviderLivenessCoverageJudgeTimeout,
// while the others meanwhile fail open. A judging that cannot read the writer
// set stores nothing, so the previous verdict stays in use until its age runs
// out.
type providerLivenessCoverage struct {
	handlers *providerLivenessHandlers
	// the writer set's newest stamp and the members' stamps, in unix
	// milliseconds of the node's clock, zero for none; nil reads Redis
	readStamps func(ctx context.Context, members []string) (newestMillis int64, stampMillis []int64, err error)
	now        func() time.Time

	stateLock sync.Mutex
	covered   bool
	reason    string
	// zero before the first verdict
	checkTime time.Time
	judging   bool
	// counts resets: a judging begun before one stores nothing
	generation uint64
	background sync.WaitGroup
}

var defaultProviderLivenessCoverage = &providerLivenessCoverage{
	handlers:   defaultProviderLivenessHandlers,
	readStamps: readProviderLivenessWriterStamps,
	now:        time.Now,
}

// Whether every live handler has a current stamp, and the fail-open reason
// when not, from a verdict younger than ProviderLivenessCoverageCacheAge.
func (self *providerLivenessCoverage) get(ctx context.Context) (covered bool, reason string) {
	now := self.now()
	judgeInBackground := false
	judgeHere := false
	var generation uint64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.checkTime.IsZero() && now.Sub(self.checkTime) < ProviderLivenessCoverageCacheAge {
			covered, reason = self.covered, self.reason
			if providerLivenessCoverageRefreshAge <= now.Sub(self.checkTime) && !self.judging {
				self.judging = true
				judgeInBackground = true
				generation = self.generation
				self.background.Add(1)
			}
			return
		}
		if self.judging {
			// another caller is judging, and no verdict is young enough
			covered, reason = false, providerLivenessFailOpenCoverage
			return
		}
		self.judging = true
		judgeHere = true
		generation = self.generation
	}()
	if judgeInBackground {
		judgeCtx := context.WithoutCancel(ctx)
		go func() {
			defer self.background.Done()
			server.HandleError(func() {
				self.judge(judgeCtx, now, generation, false)
			})
		}()
	}
	if judgeHere {
		covered, reason = self.judge(ctx, now, generation, true)
	}
	return
}

// Judges coverage, detached from the request, as of now, the start of the
// judging. Stores the verdict, unless the writer set could not be read, which
// leaves the previous verdict in place; a judging a request waits on then
// sets the coverage gauge to 0 and answers writers_error.
func (self *providerLivenessCoverage) judge(ctx context.Context, now time.Time, generation uint64, waited bool) (covered bool, reason string) {
	covered, reason = false, providerLivenessFailOpenWriters
	judged := false
	defer func() {
		// a raise ends the judging too, and the next caller judges again
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.generation != generation {
			return
		}
		self.judging = false
		if judged {
			self.covered, self.reason, self.checkTime = covered, reason, now
		}
	}()
	covered, reason, judged = self.read(context.WithoutCancel(ctx))
	if !judged && waited {
		findProviders2LivenessWriterCoverage.WithLabelValues().Set(0)
	}
	return
}

// Forgets the verdict, so the next lease read judges coverage again, and
// waits out a judging begun before, which stores nothing.
func (self *providerLivenessCoverage) reset() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.generation += 1
		self.covered, self.reason, self.checkTime, self.judging = false, "", time.Time{}, false
	}()
	self.background.Wait()
}

// Reads the live handlers and their stamps, and sets the coverage gauge.
// judged is false when the writer set could not be read: there is no verdict
// then, and the gauge keeps the last one's share.
func (self *providerLivenessCoverage) read(ctx context.Context) (covered bool, reason string, judged bool) {
	coverage := findProviders2LivenessWriterCoverage.WithLabelValues()
	handlerIds, handlersKnown := self.handlers.get(ctx)
	if !handlersKnown || len(handlerIds) == 0 {
		coverage.Set(0)
		return false, providerLivenessFailOpenHandlers, true
	}
	handlerMembers := make([]string, len(handlerIds))
	for i, handlerId := range handlerIds {
		handlerMembers[i] = handlerId.String()
	}
	readStamps := self.readStamps
	if readStamps == nil {
		readStamps = readProviderLivenessWriterStamps
	}
	newestMillis, stampMillis, err := readStamps(ctx, handlerMembers)
	if err != nil || len(stampMillis) != len(handlerIds) {
		findProviders2LivenessReadErrors.Inc()
		return false, providerLivenessFailOpenWriters, false
	}
	current, ok := judgeProviderLivenessStamps(
		server.NowUtc().UnixMilli(),
		newestMillis,
		stampMillis,
		providerLivenessWriterWindow,
	)
	coveredCount := 0
	for _, isCurrent := range current {
		if isCurrent {
			coveredCount += 1
		}
	}
	coverage.Set(float64(coveredCount) / float64(len(handlerIds)))
	if !ok || coveredCount < len(handlerIds) {
		return false, providerLivenessFailOpenCoverage, true
	}
	return true, "", true
}

// The writer set's newest stamp and the stamps of members, in one read within
// ProviderLivenessReadTimeout. Any failure of the read or of its commands is
// the error.
func readProviderLivenessWriterStamps(ctx context.Context, members []string) (int64, []int64, error) {
	var newestCmd *redis.ZSliceCmd
	var stampCmd *redis.FloatSliceCmd
	readErr := readProviderLiveness(ctx, func(readCtx context.Context, pipe redis.Pipeliner) {
		newestCmd = pipe.ZRevRangeWithScores(readCtx, providerLivenessWritersKey, 0, 0)
		stampCmd = pipe.ZMScore(readCtx, providerLivenessWritersKey, members...)
	})
	if readErr != nil {
		return 0, nil, readErr
	}
	if providerLivenessZSliceFailed(newestCmd) || providerLivenessFloatSliceFailed(stampCmd) {
		return 0, nil, errors.New("the writer set could not be read")
	}
	return providerLivenessNewestMillis(newestCmd), providerLivenessStampMillis(stampCmd), nil
}

// Runs one pipelined read within ProviderLivenessReadTimeout
// (readProviderLivenessWithin).
func readProviderLiveness(ctx context.Context, queue func(context.Context, redis.Pipeliner)) error {
	return readProviderLivenessWithin(ctx, ProviderLivenessReadTimeout, queue)
}

// Runs one pipelined read within timeout on the deadline-bound pool, which
// applies the deadline to its socket I/O and retries no command or redirect,
// without its preflight ping, which would reach an arbitrary node. The read is
// single-key commands only, which that pool's routing serves shard by shard. A
// command's own failure is on the command; the error is the read's.
func readProviderLivenessWithin(ctx context.Context, timeout time.Duration, queue func(context.Context, redis.Pipeliner)) error {
	readCtx, readCancel := context.WithTimeout(ctx, timeout)
	defer readCancel()
	return server.RedisWithDeadline(readCtx, func(r server.RedisClient) error {
		_, err := r.Pipelined(readCtx, func(pipe redis.Pipeliner) error {
			queue(readCtx, pipe)
			return nil
		})
		if err != nil && readCtx.Err() != nil {
			return readCtx.Err()
		}
		return nil
	}, server.OptRedisNoPreflight())
}

func providerLivenessZSliceFailed(cmd *redis.ZSliceCmd) bool {
	return cmd == nil || cmd.Err() != nil
}

func providerLivenessFloatSliceFailed(cmd *redis.FloatSliceCmd) bool {
	return cmd == nil || cmd.Err() != nil
}

// Judges one member of a sorted set at now from its stamp and the set's newest
// stamp, all in unix milliseconds, zero for none. A set without a stamp within
// the window of now is stale and judges no one (current false). Otherwise the
// member is live while its stamp is at most the window before the reference,
// the earlier of now and the newest stamp: the newest stamp of a set that is
// written every few milliseconds is the node's present, so a reader whose
// clock runs ahead of the node's judges no stamp older than it is, and one
// whose clock runs behind only admits more.
func judgeProviderLivenessStamp(nowMillis int64, newestMillis int64, stampMillis int64, window time.Duration) (live bool, current bool) {
	windowMillis := window.Milliseconds()
	if newestMillis <= 0 || windowMillis < nowMillis-newestMillis {
		return false, false
	}
	reference := min(nowMillis, newestMillis)
	return 0 < stampMillis && reference-stampMillis <= windowMillis, true
}

// Judges members of one sorted set at now from their stamps and the set's
// newest stamp (judgeProviderLivenessStamp): which are current, or nothing
// (ok false) for a stale set.
func judgeProviderLivenessStamps(nowMillis int64, newestMillis int64, stampMillis []int64, window time.Duration) (current []bool, ok bool) {
	if _, ok := judgeProviderLivenessStamp(nowMillis, newestMillis, 0, window); !ok {
		return nil, false
	}
	current = make([]bool, len(stampMillis))
	for i, stamp := range stampMillis {
		current[i], _ = judgeProviderLivenessStamp(nowMillis, newestMillis, stamp, window)
	}
	return current, true
}

// Whether a shard's read is the loss of its key rather than churn: more than
// half of its candidates offline, from providerLivenessBreakerMinCandidates
// candidates. Churn leaves about a tenth of a page's candidates offline (the
// pool is up to 65 minutes old, and 5-10% of what it lent was offline); a key
// lost with a node restart, an empty failover or an eviction, or restored
// stale, leaves nearly all of them offline until their connections refresh,
// up to 30 s later. Admitting such a shard costs what a stale one does.
func providerLivenessBreakerTrips(candidateCount int, offlineCount int) bool {
	return providerLivenessBreakerMinCandidates <= candidateCount && candidateCount < 2*offlineCount
}

// The newest stamp of a ZREVRANGE 0 0 WITHSCORES, zero for an empty set.
func providerLivenessNewestMillis(cmd *redis.ZSliceCmd) int64 {
	if newest := cmd.Val(); 0 < len(newest) {
		return int64(newest[0].Score)
	}
	return 0
}

// The stamps of a ZMSCORE, zero for a member without one.
func providerLivenessStampMillis(cmd *redis.FloatSliceCmd) []int64 {
	scores := cmd.Val()
	stampMillis := make([]int64, len(scores))
	for i, score := range scores {
		stampMillis[i] = int64(score)
	}
	return stampMillis
}

// A shard's read: its newest stamp and its candidates' stamps, in the order
// they were asked, in unix milliseconds of the node's clock, zero for none;
// or failed, when its commands failed.
type providerLivenessShardRead struct {
	failed       bool
	newestMillis int64
	stampMillis  []int64
}

// Reads, in one pipeline within timeout (readProviderLivenessWithin), the
// newest stamp of each shard and the stamps of its candidates. A shard whose
// commands failed reads as failed; the error is the read's, and fails every
// shard.
func readProviderLivenessShards(ctx context.Context, timeout time.Duration, shardClientIds map[int][]server.Id) (map[int]providerLivenessShardRead, error) {
	shards := slices.Sorted(maps.Keys(shardClientIds))
	shardNewestCmds := make([]*redis.ZSliceCmd, len(shards))
	shardStampCmds := make([]*redis.FloatSliceCmd, len(shards))
	readErr := readProviderLivenessWithin(ctx, timeout, func(readCtx context.Context, pipe redis.Pipeliner) {
		for i, shard := range shards {
			key := providerLivenessLeaseKey(shard)
			members := make([]string, len(shardClientIds[shard]))
			for j, clientId := range shardClientIds[shard] {
				members[j] = clientId.String()
			}
			shardNewestCmds[i] = pipe.ZRevRangeWithScores(readCtx, key, 0, 0)
			shardStampCmds[i] = pipe.ZMScore(readCtx, key, members...)
		}
	})
	if readErr != nil {
		return nil, readErr
	}
	reads := make(map[int]providerLivenessShardRead, len(shards))
	for i, shard := range shards {
		if providerLivenessZSliceFailed(shardNewestCmds[i]) || providerLivenessFloatSliceFailed(shardStampCmds[i]) {
			reads[shard] = providerLivenessShardRead{failed: true}
			continue
		}
		stampMillis := providerLivenessStampMillis(shardStampCmds[i])
		if len(stampMillis) != len(shardClientIds[shard]) {
			reads[shard] = providerLivenessShardRead{failed: true}
			continue
		}
		reads[shard] = providerLivenessShardRead{
			newestMillis: providerLivenessNewestMillis(shardNewestCmds[i]),
			stampMillis:  stampMillis,
		}
	}
	return reads, nil
}

// The shards hold the leases in a 64-bit mask per request.
var _ [64 - providerLivenessLeaseShardCount]struct{}

// One FindProviders2 request's reads of the lease. A shard whose read failed
// during the request is not read again by it, rather than wait out the budget
// against the same slow node again: the rest of the request takes the shard's
// last verdicts, at most providerLivenessCacheMaxAge old, or admits its
// candidates. Every other request reads the shard as usual.
type providerLivenessRequest struct {
	failedShards atomic.Uint64
}

func newProviderLivenessRequest() *providerLivenessRequest {
	return &providerLivenessRequest{}
}

func (self *providerLivenessRequest) shardFailed(shard int) bool {
	return self.failedShards.Load()&(uint64(1)<<shard) != 0
}

func (self *providerLivenessRequest) failShard(shard int) {
	self.failedShards.Or(uint64(1) << shard)
}

// Before any time this process can read: a refresh or a sweep that never ran.
const providerLivenessCacheNever = math.MinInt64 / 2

// The lease verdicts this process read for FindProviders2 candidates, kept for
// the next requests (see the file comment). One per process; reads from
// memory, and from Redis only for candidates without a fresh verdict. The
// background refresh is a goroutine a request starts, one at a time, which
// ends with its read.
type providerLivenessLeaseCache struct {
	// reads shards within a budget (readProviderLivenessShards)
	read func(ctx context.Context, timeout time.Duration, shardClientIds map[int][]server.Id) (map[int]providerLivenessShardRead, error)
	// the clock of the ages, read only by requests, never in the background
	now func() time.Time
	// the size gauge, nil for none
	clients prometheus.Gauge

	stateLock sync.Mutex
	// times below are nanoseconds of the clock since this origin, so no entry
	// holds a pointer and the collector never scans the map
	epoch   time.Time
	entries map[server.Id]providerLivenessLeaseEntry
	// candidates whose verdict a request used past the refresh age, each once,
	// for the next background refresh
	due        []server.Id
	refreshing bool
	// the start of the last background refresh and of the last sweep
	refreshNanos int64
	sweepNanos   int64
	// counts resets: a read begun before one stores nothing
	generation uint64
	background sync.WaitGroup
}

// A candidate's verdict as the read that last saw it.
type providerLivenessLeaseEntry struct {
	// the candidate's stamp and its shard's newest stamp, in unix milliseconds
	// of the node's clock, zero for none
	stampMillis  int64
	newestMillis int64
	// when the read that saw them began
	readNanos int64
	// a read of the candidate failed since, so its verdict is stale
	failed bool
	// the candidate waits in due
	queued bool
}

func newProviderLivenessLeaseCache(
	read func(ctx context.Context, timeout time.Duration, shardClientIds map[int][]server.Id) (map[int]providerLivenessShardRead, error),
	now func() time.Time,
	clients prometheus.Gauge,
) *providerLivenessLeaseCache {
	return &providerLivenessLeaseCache{
		read:         read,
		now:          now,
		clients:      clients,
		epoch:        now(),
		entries:      map[server.Id]providerLivenessLeaseEntry{},
		refreshNanos: providerLivenessCacheNever,
		sweepNanos:   providerLivenessCacheNever,
	}
}

var defaultProviderLivenessLeases = newProviderLivenessLeaseCache(readProviderLivenessShards, time.Now, findProviders2LivenessCacheClients)

// Forgets every verdict, and waits out a background refresh begun before,
// which stores nothing. The ages restart from the clock's present.
func (self *providerLivenessLeaseCache) reset() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.generation += 1
		self.epoch = self.now()
		self.entries = map[server.Id]providerLivenessLeaseEntry{}
		self.due = nil
		self.refreshing = false
		self.refreshNanos, self.sweepNanos = providerLivenessCacheNever, providerLivenessCacheNever
		self.setClientsLocked()
	}()
	self.background.Wait()
}

func (self *providerLivenessLeaseCache) setClientsLocked() {
	if self.clients != nil {
		self.clients.Set(float64(len(self.entries)))
	}
}

// A candidate's lease as a request judges it: its stamp and its shard's newest
// stamp, from memory or the request's read; ok false for no verdict.
type providerLivenessStamp struct {
	stampMillis  int64
	newestMillis int64
	ok           bool
}

// The verdicts of a request's candidates, shard by shard in the order of the
// candidates asked, and why some have none.
type providerLivenessLookup struct {
	stamps map[int][]providerLivenessStamp
	// a shard's read failed in this lookup, or had failed earlier in the
	// request, and left candidates without a verdict
	shardError  bool
	readBackoff bool
}

// One candidate the request reads, and its last verdict within the max age.
type providerLivenessPendingRead struct {
	index        int
	fallback     providerLivenessLeaseEntry
	haveFallback bool
}

// The verdicts of shardClientIds, distinct candidates grouped by shard: from
// memory while fresh, and otherwise from one read of the request's within
// ProviderLivenessReadTimeout, which counts every failure. The error is the
// context's alone.
func (self *providerLivenessLeaseCache) lookup(ctx context.Context, request *providerLivenessRequest, shardClientIds map[int][]server.Id) (providerLivenessLookup, error) {
	lookup := providerLivenessLookup{
		stamps: make(map[int][]providerLivenessStamp, len(shardClientIds)),
	}
	pending := map[int][]providerLivenessPendingRead{}
	hitCount, staleCount, readCount, failOpenCount := 0, 0, 0, 0
	countSources := func() {
		addProviderLivenessCount(findProviders2LivenessCacheHits, hitCount)
		addProviderLivenessCount(findProviders2LivenessCacheStales, staleCount)
		addProviderLivenessCount(findProviders2LivenessCacheReads, readCount)
		addProviderLivenessCount(findProviders2LivenessCacheFailOpens, failOpenCount)
	}

	var lookupNanos int64
	var generation uint64
	var refreshClientIds []server.Id
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		lookupNanos = self.now().Sub(self.epoch).Nanoseconds()
		generation = self.generation
		for shard, clientIds := range shardClientIds {
			stamps := make([]providerLivenessStamp, len(clientIds))
			for i, clientId := range clientIds {
				entry, ok := self.entries[clientId]
				age := lookupNanos - entry.readNanos
				if ok && age <= providerLivenessCacheFreshAge.Nanoseconds() {
					stamps[i] = providerLivenessStamp{stampMillis: entry.stampMillis, newestMillis: entry.newestMillis, ok: true}
					if entry.failed {
						staleCount += 1
					} else {
						hitCount += 1
					}
					if providerLivenessCacheRefreshAge.Nanoseconds() < age && !entry.queued && len(self.due) < providerLivenessCacheMaxClients {
						entry.queued = true
						self.entries[clientId] = entry
						self.due = append(self.due, clientId)
					}
					continue
				}
				pending[shard] = append(pending[shard], providerLivenessPendingRead{
					index:        i,
					fallback:     entry,
					haveFallback: ok && age <= providerLivenessCacheMaxAge.Nanoseconds(),
				})
			}
			lookup.stamps[shard] = stamps
		}
		self.sweepLocked(lookupNanos, false)
		refreshClientIds = self.takeRefreshLocked(lookupNanos)
	}()
	if refreshClientIds != nil {
		refreshCtx := context.WithoutCancel(ctx)
		go func() {
			defer self.background.Done()
			server.HandleError(func() {
				self.refresh(refreshCtx, generation, lookupNanos, refreshClientIds)
			})
		}()
	}
	if len(pending) == 0 {
		countSources()
		return lookup, nil
	}

	// the request's last verdict, or none, for a candidate without a read
	fallBack := func(shard int, pendingRead providerLivenessPendingRead) bool {
		if pendingRead.haveFallback {
			lookup.stamps[shard][pendingRead.index] = providerLivenessStamp{
				stampMillis:  pendingRead.fallback.stampMillis,
				newestMillis: pendingRead.fallback.newestMillis,
				ok:           true,
			}
			staleCount += 1
			return true
		}
		failOpenCount += 1
		return false
	}
	readClientIds := map[int][]server.Id{}
	for shard, pendingReads := range pending {
		if request.shardFailed(shard) {
			for _, pendingRead := range pendingReads {
				if !fallBack(shard, pendingRead) {
					lookup.readBackoff = true
				}
			}
			continue
		}
		clientIds := make([]server.Id, len(pendingReads))
		for i, pendingRead := range pendingReads {
			clientIds[i] = shardClientIds[shard][pendingRead.index]
		}
		readClientIds[shard] = clientIds
	}
	if len(readClientIds) == 0 {
		countSources()
		return lookup, nil
	}

	reads, readErr := self.read(ctx, ProviderLivenessReadTimeout, readClientIds)
	if err := ctx.Err(); err != nil {
		// a canceled request is the request's own error
		return providerLivenessLookup{}, err
	}
	readFailed := false
	for shard, clientIds := range readClientIds {
		shardRead, ok := providerLivenessShardReadOf(reads, readErr, shard, len(clientIds))
		if !ok {
			readFailed = true
			request.failShard(shard)
			for _, pendingRead := range pending[shard] {
				if !fallBack(shard, pendingRead) {
					lookup.shardError = true
				}
			}
			continue
		}
		for i := range clientIds {
			lookup.stamps[shard][pending[shard][i].index] = providerLivenessStamp{
				stampMillis:  shardRead.stampMillis[i],
				newestMillis: shardRead.newestMillis,
				ok:           true,
			}
		}
		readCount += len(clientIds)
	}
	if readFailed {
		findProviders2LivenessReadErrors.Inc()
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.generation == generation {
			self.storeLocked(lookupNanos, readClientIds, reads, readErr)
		}
	}()
	countSources()
	return lookup, nil
}

func addProviderLivenessCount(counter prometheus.Counter, count int) {
	if 0 < count {
		counter.Add(float64(count))
	}
}

// A shard's read of count candidates, or false when the read, or the shard's
// own commands, failed.
func providerLivenessShardReadOf(reads map[int]providerLivenessShardRead, readErr error, shard int, count int) (providerLivenessShardRead, bool) {
	shardRead, ok := reads[shard]
	if readErr != nil || !ok || shardRead.failed || len(shardRead.stampMillis) != count {
		return providerLivenessShardRead{}, false
	}
	return shardRead, true
}

// Stores a read begun at readNanos of the clientIds of each shard: the
// verdicts of the shards read, unless a later read has stored its own, and a
// failed read on the verdicts it leaves. The caller checks that no reset came
// after the read began.
func (self *providerLivenessLeaseCache) storeLocked(readNanos int64, shardClientIds map[int][]server.Id, reads map[int]providerLivenessShardRead, readErr error) {
	for shard, clientIds := range shardClientIds {
		shardRead, ok := providerLivenessShardReadOf(reads, readErr, shard, len(clientIds))
		if !ok {
			for _, clientId := range clientIds {
				if entry, ok := self.entries[clientId]; ok && entry.readNanos <= readNanos {
					entry.failed = true
					self.entries[clientId] = entry
				}
			}
			continue
		}
		for i, clientId := range clientIds {
			entry, ok := self.entries[clientId]
			if ok && readNanos < entry.readNanos {
				continue
			}
			if !ok && !self.makeRoomLocked(readNanos) {
				continue
			}
			entry.stampMillis = shardRead.stampMillis[i]
			entry.newestMillis = shardRead.newestMillis
			entry.readNanos = readNanos
			entry.failed = false
			self.entries[clientId] = entry
		}
	}
	self.setClientsLocked()
}

// Whether the map has room for one more candidate, sweeping it, or evicting
// an arbitrary candidate, when it is full.
func (self *providerLivenessLeaseCache) makeRoomLocked(nowNanos int64) bool {
	if len(self.entries) < providerLivenessCacheMaxClients {
		return true
	}
	self.sweepLocked(nowNanos, true)
	for clientId := range self.entries {
		if len(self.entries) < providerLivenessCacheMaxClients {
			break
		}
		// the map's order is unspecified: an arbitrary candidate
		delete(self.entries, clientId)
	}
	return len(self.entries) < providerLivenessCacheMaxClients
}

// Removes the verdicts older than the max age, which no request uses, at most
// once per max age, or once per refresh age when full.
func (self *providerLivenessLeaseCache) sweepLocked(nowNanos int64, full bool) {
	interval := providerLivenessCacheMaxAge
	if full {
		interval = providerLivenessCacheRefreshAge
	}
	if nowNanos-self.sweepNanos < interval.Nanoseconds() {
		return
	}
	self.sweepNanos = nowNanos
	for clientId, entry := range self.entries {
		if providerLivenessCacheMaxAge.Nanoseconds() < nowNanos-entry.readNanos {
			delete(self.entries, clientId)
		}
	}
	self.setClientsLocked()
}

// The candidates of the next background refresh, or nil when none is due:
// one is running, one began within the refresh age, or no verdict waits. The
// caller starts it.
func (self *providerLivenessLeaseCache) takeRefreshLocked(nowNanos int64) []server.Id {
	if self.refreshing || len(self.due) == 0 || nowNanos-self.refreshNanos < providerLivenessCacheRefreshAge.Nanoseconds() {
		return nil
	}
	n := min(len(self.due), providerLivenessRefreshMaxClients)
	clientIds := self.due[:n:n]
	if n == len(self.due) {
		self.due = nil
	} else {
		self.due = slices.Clone(self.due[n:])
	}
	self.refreshing = true
	self.refreshNanos = nowNanos
	self.background.Add(1)
	return clientIds
}

// One background refresh, begun at readNanos: reads the candidates' leases
// within providerLivenessRefreshReadTimeout and stores them as a request's
// read would, unless a reset came between.
func (self *providerLivenessLeaseCache) refresh(ctx context.Context, generation uint64, readNanos int64, clientIds []server.Id) {
	shardClientIds := map[int][]server.Id{}
	for _, clientId := range clientIds {
		shard := providerLivenessLeaseShard(clientId)
		shardClientIds[shard] = append(shardClientIds[shard], clientId)
	}
	var reads map[int]providerLivenessShardRead
	readErr := errors.New("the refresh did not read")
	defer func() {
		// a raise ends the refresh too, and its candidates may wait again
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.generation != generation {
				return
			}
			self.storeLocked(readNanos, shardClientIds, reads, readErr)
			self.refreshing = false
			for _, clientId := range clientIds {
				if entry, ok := self.entries[clientId]; ok && entry.queued {
					entry.queued = false
					self.entries[clientId] = entry
				}
			}
		}()
		failed := false
		for shard, shardClientIds := range shardClientIds {
			if _, ok := providerLivenessShardReadOf(reads, readErr, shard, len(shardClientIds)); !ok {
				failed = true
			}
		}
		if failed {
			findProviders2LivenessReadErrors.Inc()
			findProviders2LivenessRefreshes.WithLabelValues(providerLivenessRefreshError).Inc()
		} else {
			findProviders2LivenessRefreshes.WithLabelValues(providerLivenessRefreshOk).Inc()
		}
	}()
	reads, readErr = self.read(ctx, providerLivenessRefreshReadTimeout, shardClientIds)
}

// A process's reader of the lease: its coverage verdict and its lease cache.
type providerLivenessReader struct {
	coverage *providerLivenessCoverage
	leases   *providerLivenessLeaseCache
}

var defaultProviderLivenessReader = &providerLivenessReader{
	coverage: defaultProviderLivenessCoverage,
	leases:   defaultProviderLivenessLeases,
}

// The lease's verdicts on clientIds at now, by this process's reader
// (providerLivenessReader.exclusions).
func getProviderLivenessExclusions(ctx context.Context, request *providerLivenessRequest, clientIds []server.Id, now time.Time) (offline map[server.Id]bool, unknown map[server.Id]bool, returnErr error) {
	return defaultProviderLivenessReader.exclusions(ctx, request, clientIds, now)
}

// The lease's verdicts on clientIds at now. offline holds the providers with
// no lease, or one older than the window, in a shard that is current; unknown
// holds the rest that are not live: every one while writer coverage is short,
// those without a verdict when a read fails, and those in a stale shard or one
// that trips the offline-fraction breaker. Both are judged once per distinct
// id. request carries the shards whose read already failed in the same
// request; nil is a request of this read alone. The error is the context's
// alone: every other failure fails open, and is counted.
func (self *providerLivenessReader) exclusions(ctx context.Context, request *providerLivenessRequest, clientIds []server.Id, now time.Time) (offline map[server.Id]bool, unknown map[server.Id]bool, returnErr error) {
	offline = map[server.Id]bool{}
	unknown = map[server.Id]bool{}
	shardClientIds := map[int][]server.Id{}
	seenClientIds := map[server.Id]bool{}
	for _, clientId := range clientIds {
		if !seenClientIds[clientId] {
			seenClientIds[clientId] = true
			shard := providerLivenessLeaseShard(clientId)
			shardClientIds[shard] = append(shardClientIds[shard], clientId)
		}
	}
	if len(shardClientIds) == 0 {
		return
	}
	// a canceled request is the request's own error, cached verdicts or not
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	failOpen := func(clientIds []server.Id) {
		for _, clientId := range clientIds {
			unknown[clientId] = true
		}
	}

	covered, reason := self.coverage.get(ctx)
	if !covered {
		findProviders2LivenessFailOpens.WithLabelValues(reason).Inc()
		for _, clientIds := range shardClientIds {
			failOpen(clientIds)
		}
		return
	}

	if request == nil {
		request = newProviderLivenessRequest()
	}
	lookup, err := self.leases.lookup(ctx, request, shardClientIds)
	if err != nil {
		return nil, nil, err
	}
	if lookup.shardError {
		findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenShard).Inc()
	}
	if lookup.readBackoff {
		findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenReadBackoff).Inc()
	}

	nowMillis := now.UnixMilli()
	for shard, clientIds := range shardClientIds {
		stamps := lookup.stamps[shard]
		stale := false
		judgedCount := 0
		offlineClientIds := []server.Id{}
		for i, clientId := range clientIds {
			if !stamps[i].ok {
				unknown[clientId] = true
				continue
			}
			live, current := judgeProviderLivenessStamp(nowMillis, stamps[i].newestMillis, stamps[i].stampMillis, providerLivenessLeaseWindow)
			if !current {
				stale = true
				break
			}
			judgedCount += 1
			if !live {
				offlineClientIds = append(offlineClientIds, clientId)
			}
		}
		if stale {
			// the shard as a whole
			findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenStale).Inc()
			failOpen(clientIds)
			continue
		}
		if providerLivenessBreakerTrips(judgedCount, len(offlineClientIds)) {
			findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenOfflineFraction).Inc()
			for i, clientId := range clientIds {
				if stamps[i].ok {
					unknown[clientId] = true
				}
			}
			continue
		}
		for _, clientId := range offlineClientIds {
			offline[clientId] = true
		}
	}
	judgedCount := len(seenClientIds) - len(unknown)
	if providerLivenessReadBreakerTrips(judgedCount, len(offline)) {
		findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenReadOfflineFraction).Inc()
		for clientId := range offline {
			unknown[clientId] = true
		}
		offline = map[server.Id]bool{}
	}
	return
}

// Whether a read is a lapse rather than churn: more than
// providerLivenessReadOfflineShare of its judged candidates offline, and at
// least providerLivenessReadBreakerMinOffline of them.
func providerLivenessReadBreakerTrips(judgedCount int, offlineCount int) bool {
	return providerLivenessReadBreakerMinOffline <= offlineCount && providerLivenessReadOfflineShare*float64(judgedCount) < float64(offlineCount)
}

// The rank mode label of a request: its mode, or unknown for a mode no
// bucket serves, which keeps the label set bounded.
func findProviders2LivenessRankModeLabel(rankMode RankMode) string {
	switch rankMode {
	case RankModeQuality, RankModeSpeed, RankModeOnline:
		return rankMode
	default:
		return providerLivenessUnknown
	}
}

// Reads the lease of a request's candidates (getProviderLivenessExclusions)
// and counts each distinct candidate's verdict under the request's rank mode.
// Returns the offline candidates.
func readFindProviders2Liveness(ctx context.Context, request *providerLivenessRequest, clientIds []server.Id, now time.Time, rankMode RankMode) (map[server.Id]bool, error) {
	offline, unknown, err := getProviderLivenessExclusions(ctx, request, clientIds, now)
	if err != nil {
		return nil, err
	}
	distinctClientIds := map[server.Id]bool{}
	for _, clientId := range clientIds {
		distinctClientIds[clientId] = true
	}
	label := findProviders2LivenessRankModeLabel(rankMode)
	for verdict, count := range map[string]int{
		providerLivenessLive:    len(distinctClientIds) - len(offline) - len(unknown),
		providerLivenessOffline: len(offline),
		providerLivenessUnknown: len(unknown),
	} {
		if 0 < count {
			findProviders2LivenessCandidates.WithLabelValues(label, verdict).Add(float64(count))
		}
	}
	return offline, nil
}
