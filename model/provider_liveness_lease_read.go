// The reader of the provider liveness lease (provider_liveness_lease.go): which
// candidates of a FindProviders2 request have no current lease.
//
// A read judges nothing unless every live connect handler, a heartbeat within
// two heartbeat timeouts, has a current stamp in the writer set; a connect
// process without the writer, mid-rollout, rolled back, built without it or
// with its lease writes stopped, would otherwise leave every provider it
// serves without a lease. The live handlers come from the database at most
// every providerLivenessHandlersMaxAge per process, and the verdict on their
// stamps is reused for ProviderLivenessCoverageCacheAge; while coverage is
// short no shard is read at all. Every Redis read runs on the deadline-bound
// pool within ProviderLivenessReadTimeout, retries nothing and fails open, so a
// slow or wedged node costs a request at most that budget. A shard whose read
// fails, whose newest lease is stale, or more than half of whose candidates
// read offline (a lost or stale-restored key, not churn) is unknown and
// admitted. Nothing here ever refuses a request.
//
// Safe for concurrent use.
package model

import (
	"context"
	"errors"
	"maps"
	"runtime"
	"slices"
	"sync"
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
	// the writer set could not be read within the budget
	providerLivenessFailOpenWriters = "writers_error"
	// a shard could not be read within the budget
	providerLivenessFailOpenShard = "shard_error"
	// a shard has no lease within the window
	providerLivenessFailOpenStale = "stale"
	// more than half of a shard's candidates read offline
	providerLivenessFailOpenOfflineFraction = "offline_fraction"
	// more than providerLivenessReadOfflineShare of a read's judged
	// candidates read offline
	providerLivenessFailOpenReadOfflineFraction = "read_offline_fraction"
	// a shard read failed within the last providerLivenessReadBackoff
	providerLivenessFailOpenReadBackoff = "read_backoff"
)

var findProviders2LivenessCandidates = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_candidates_total",
	Help: "FindProviders2 candidates judged by their provider liveness lease, by the request's rank mode and verdict: live; offline, no lease within the window, left out of every draw of the request; unknown, admitted (urnetwork_findproviders2_liveness_failopens_total says why)",
}, []string{"rank_mode", "verdict"})

var findProviders2LivenessReadErrors = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_read_errors_total",
	Help: "Provider liveness lease reads of FindProviders2 that failed or ran past their budget, of the writer set or of some shard; their candidates were admitted as unknown",
})

var findProviders2LivenessFailOpens = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_failopens_total",
	Help: "Provider liveness lease reads that admitted candidates they could not judge, by reason: handlers, coverage, writers_error, shard_error, read_backoff and read_offline_fraction per read, stale and offline_fraction per shard",
}, []string{"reason"})

// No labels: the child exists from a process's first verdict on coverage, so a
// process that has read nothing exports nothing.
var findProviders2LivenessWriterCoverage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_liveness_writer_coverage",
	Help: "The share of live connect handlers with a current provider liveness writer stamp at this process's latest verdict, 0 when the handlers or the stamps could not be read; leases are enforced only at 1",
}, []string{})

func init() {
	prometheus.MustRegister(
		findProviders2LivenessCandidates,
		findProviders2LivenessReadErrors,
		findProviders2LivenessFailOpens,
		findProviders2LivenessWriterCoverage,
	)
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

// After a shard read fails, every read of the process fails open for this
// long rather than wait out the budget against the same slow node again.
const providerLivenessReadBackoff = 5 * time.Second

// How long a process reuses its read of the live connect handlers.
const providerLivenessHandlersMaxAge = 30 * time.Second

// A read of the live handlers older than this is no read: until one succeeds,
// every lease read fails open.
const providerLivenessHandlersMaxStaleness = 2 * providerLivenessHandlersMaxAge

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

// The verdict on writer coverage, reused for ProviderLivenessCoverageCacheAge
// of this process's monotonic clock: whether every live handler has a current
// stamp, and when not, why. One caller judges at a time, detached from its
// request and within model's ProviderLivenessCoverageJudgeTimeout, so every
// judging stores its verdict; the others take the previous verdict while it is
// younger than twice the cache age, or else not covered.
type providerLivenessCoverage struct {
	handlers *providerLivenessHandlers
	now      func() time.Time

	stateLock sync.Mutex
	covered   bool
	reason    string
	// zero before the first verdict
	checkTime time.Time
	judging   bool
}

var defaultProviderLivenessCoverage = &providerLivenessCoverage{
	handlers: defaultProviderLivenessHandlers,
	now:      time.Now,
}

// Whether every live handler has a current stamp, and the fail-open reason
// when not, from a verdict at most ProviderLivenessCoverageCacheAge old. The
// error is the context's alone.
func (self *providerLivenessCoverage) get(ctx context.Context) (covered bool, reason string, returnErr error) {
	now := self.now()
	judge := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.checkTime.IsZero() && now.Sub(self.checkTime) < ProviderLivenessCoverageCacheAge {
			covered, reason = self.covered, self.reason
			return
		}
		if self.judging {
			// another caller is judging: the previous verdict, while young
			if !self.checkTime.IsZero() && now.Sub(self.checkTime) < 2*ProviderLivenessCoverageCacheAge {
				covered, reason = self.covered, self.reason
			} else {
				covered, reason = false, providerLivenessFailOpenCoverage
			}
			return
		}
		self.judging = true
		judge = true
	}()
	if !judge {
		return
	}
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.judging = false
		if returnErr == nil {
			self.covered, self.reason, self.checkTime = covered, reason, now
		}
	}()
	// detached from the request, which may end before the judging does
	covered, reason, returnErr = self.read(context.WithoutCancel(ctx))
	return
}

// Forgets the verdict, so the next lease read judges coverage again.
func (self *providerLivenessCoverage) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.covered, self.reason, self.checkTime = false, "", time.Time{}
}

// When the process's shard reads last failed, by its monotonic clock.
type providerLivenessReadHealth struct {
	now func() time.Time

	stateLock sync.Mutex
	// zero while no read has failed
	failTime time.Time
}

var defaultProviderLivenessReadHealth = &providerLivenessReadHealth{
	now: time.Now,
}

// Whether a shard read failed within the last providerLivenessReadBackoff.
func (self *providerLivenessReadHealth) backingOff() bool {
	now := self.now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.failTime.IsZero() && now.Sub(self.failTime) < providerLivenessReadBackoff
}

func (self *providerLivenessReadHealth) failed() {
	now := self.now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failTime = now
}

// Forgets the failure.
func (self *providerLivenessReadHealth) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failTime = time.Time{}
}

// Reads the live handlers and their stamps, and sets the coverage gauge.
func (self *providerLivenessCoverage) read(ctx context.Context) (bool, string, error) {
	coverage := findProviders2LivenessWriterCoverage.WithLabelValues()
	handlerIds, handlersKnown := self.handlers.get(ctx)
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	if !handlersKnown || len(handlerIds) == 0 {
		coverage.Set(0)
		return false, providerLivenessFailOpenHandlers, nil
	}
	handlerMembers := make([]string, len(handlerIds))
	for i, handlerId := range handlerIds {
		handlerMembers[i] = handlerId.String()
	}
	var newestCmd *redis.ZSliceCmd
	var stampCmd *redis.FloatSliceCmd
	readErr := readProviderLiveness(ctx, func(readCtx context.Context, pipe redis.Pipeliner) {
		newestCmd = pipe.ZRevRangeWithScores(readCtx, providerLivenessWritersKey, 0, 0)
		stampCmd = pipe.ZMScore(readCtx, providerLivenessWritersKey, handlerMembers...)
	})
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	if readErr != nil || providerLivenessZSliceFailed(newestCmd) || providerLivenessFloatSliceFailed(stampCmd) {
		findProviders2LivenessReadErrors.Inc()
		coverage.Set(0)
		return false, providerLivenessFailOpenWriters, nil
	}
	current, ok := judgeProviderLivenessStamps(
		server.NowUtc().UnixMilli(),
		providerLivenessNewestMillis(newestCmd),
		providerLivenessStampMillis(stampCmd),
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
		return false, providerLivenessFailOpenCoverage, nil
	}
	return true, "", nil
}

// Runs one pipelined read within ProviderLivenessReadTimeout on the
// deadline-bound pool, which applies the deadline to its socket I/O and
// retries no command or redirect, without its preflight ping, which would
// reach an arbitrary node. The read is single-key commands only, which that
// pool's routing serves shard by shard. A command's own failure is on the
// command; the error is the read's.
func readProviderLiveness(ctx context.Context, queue func(context.Context, redis.Pipeliner)) error {
	readCtx, readCancel := context.WithTimeout(ctx, ProviderLivenessReadTimeout)
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

// Judges members of one sorted set at now from their stamps and the set's
// newest stamp, all in unix milliseconds, zero for none. A set without a stamp
// within the window of now is stale and judges no one (ok false). Otherwise a
// member is current while its stamp is at most the window before the
// reference, the earlier of now and the newest stamp: the newest stamp of a set
// that is written every few milliseconds is the node's present, so a reader
// whose clock runs ahead of the node's judges no stamp older than it is, and
// one whose clock runs behind only admits more.
func judgeProviderLivenessStamps(nowMillis int64, newestMillis int64, stampMillis []int64, window time.Duration) (current []bool, ok bool) {
	windowMillis := window.Milliseconds()
	if newestMillis <= 0 || windowMillis < nowMillis-newestMillis {
		return nil, false
	}
	reference := min(nowMillis, newestMillis)
	current = make([]bool, len(stampMillis))
	for i, stamp := range stampMillis {
		current[i] = 0 < stamp && reference-stamp <= windowMillis
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

// The lease's verdicts on clientIds at now. offline holds the providers with
// no lease, or one older than the window, in a shard that is current; unknown
// holds the rest that are not live: every one while writer coverage is short
// or the shard read fails, and those in a stale shard or one that trips the
// offline-fraction breaker. Both are judged once per distinct id. The error is
// the context's alone: every other failure fails open, and is counted.
func getProviderLivenessExclusions(ctx context.Context, clientIds []server.Id, now time.Time) (offline map[server.Id]bool, unknown map[server.Id]bool, returnErr error) {
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
	failAllOpen := func(reason string) {
		findProviders2LivenessFailOpens.WithLabelValues(reason).Inc()
		for _, clientIds := range shardClientIds {
			failOpen(clientIds)
		}
	}

	covered, reason, err := defaultProviderLivenessCoverage.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !covered {
		failAllOpen(reason)
		return
	}
	if defaultProviderLivenessReadHealth.backingOff() {
		failAllOpen(providerLivenessFailOpenReadBackoff)
		return
	}

	shards := slices.Sorted(maps.Keys(shardClientIds))
	shardNewestCmds := make([]*redis.ZSliceCmd, len(shards))
	shardStampCmds := make([]*redis.FloatSliceCmd, len(shards))
	readErr := readProviderLiveness(ctx, func(readCtx context.Context, pipe redis.Pipeliner) {
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
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if readErr != nil {
		defaultProviderLivenessReadHealth.failed()
		findProviders2LivenessReadErrors.Inc()
		failAllOpen(providerLivenessFailOpenShard)
		return
	}

	nowMillis := now.UnixMilli()
	shardFailed := false
	for i, shard := range shards {
		clientIds := shardClientIds[shard]
		if providerLivenessZSliceFailed(shardNewestCmds[i]) || providerLivenessFloatSliceFailed(shardStampCmds[i]) {
			shardFailed = true
			failOpen(clientIds)
			continue
		}
		live, ok := judgeProviderLivenessStamps(
			nowMillis,
			providerLivenessNewestMillis(shardNewestCmds[i]),
			providerLivenessStampMillis(shardStampCmds[i]),
			providerLivenessLeaseWindow,
		)
		if !ok || len(live) != len(clientIds) {
			findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenStale).Inc()
			failOpen(clientIds)
			continue
		}
		offlineClientIds := []server.Id{}
		for j, clientId := range clientIds {
			if !live[j] {
				offlineClientIds = append(offlineClientIds, clientId)
			}
		}
		if providerLivenessBreakerTrips(len(clientIds), len(offlineClientIds)) {
			findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenOfflineFraction).Inc()
			failOpen(clientIds)
			continue
		}
		for _, clientId := range offlineClientIds {
			offline[clientId] = true
		}
	}
	if shardFailed {
		defaultProviderLivenessReadHealth.failed()
		findProviders2LivenessReadErrors.Inc()
		findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenShard).Inc()
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
func readFindProviders2Liveness(ctx context.Context, clientIds []server.Id, now time.Time, rankMode RankMode) (map[server.Id]bool, error) {
	offline, unknown, err := getProviderLivenessExclusions(ctx, clientIds, now)
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
