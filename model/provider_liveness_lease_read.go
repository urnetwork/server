// The reader of the provider liveness lease (provider_liveness_lease.go): which
// candidates of a FindProviders2 request have no current lease.
//
// A read judges nothing unless every live connect handler, a heartbeat within
// two heartbeat timeouts, has a current stamp in the writer set; a connect
// process without the writer, mid-rollout, rolled back or built without it,
// would otherwise leave every provider it serves without a lease. The live
// handlers come from the database at most every providerLivenessHandlersMaxAge
// per process. While coverage is short, or a read fails, every candidate is
// unknown and admitted, as a candidate in a stale shard is. Nothing here ever
// refuses a request.
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

var findProviders2LivenessCandidates = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_candidates_total",
	Help: "FindProviders2 candidates judged by their provider liveness lease, by the request's rank mode and verdict: live; offline, no lease within the window, left out of every draw of the request; unknown, admitted because a live connect handler has no current writer stamp, the shard is stale or the read failed",
}, []string{"rank_mode", "verdict"})

var findProviders2LivenessReadErrors = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_liveness_read_errors_total",
	Help: "Provider liveness lease reads of FindProviders2 that failed, in whole or for some shard; their candidates were admitted as unknown",
})

var findProviders2LivenessWriterCoverage = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_liveness_writer_coverage",
	Help: "The share of live connect handlers with a current provider liveness writer stamp at this process's latest lease read; leases are enforced only at 1",
})

func init() {
	prometheus.MustRegister(findProviders2LivenessCandidates, findProviders2LivenessReadErrors, findProviders2LivenessWriterCoverage)
}

// How long a process reuses its read of the live connect handlers.
const providerLivenessHandlersMaxAge = 30 * time.Second

// A read of the live handlers older than this is no read: until one succeeds,
// every lease read fails open.
const providerLivenessHandlersMaxStaleness = 2 * providerLivenessHandlersMaxAge

// The live connect handlers, read from the database at most every
// providerLivenessHandlersMaxAge, by one caller at a time while the others use
// the previous read.
type providerLivenessHandlers struct {
	load func(ctx context.Context, minHeartbeatTime time.Time) ([]server.Id, error)

	stateLock  sync.Mutex
	handlerIds []server.Id
	// zero before the first read
	readTime time.Time
	loading  bool
}

var defaultProviderLivenessHandlers = &providerLivenessHandlers{
	load: loadLiveNetworkClientHandlerIds,
}

// The live handlers at now, and whether they are known. A failed read keeps the
// previous one until it is providerLivenessHandlersMaxStaleness old.
func (self *providerLivenessHandlers) get(ctx context.Context, now time.Time) ([]server.Id, bool) {
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
			loadedHandlerIds, err = self.load(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))
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
// holds the rest that are not live: every one while a live connect handler has
// no current writer stamp, and those in a stale shard or one whose read failed.
// Both are judged once per distinct id, in one pipelined read. The error is the
// context's alone: every other failure fails open, and is counted.
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
	failOpen := func(clientIds []server.Id) {
		for _, clientId := range clientIds {
			unknown[clientId] = true
		}
	}
	failAllOpen := func() {
		for _, clientIds := range shardClientIds {
			failOpen(clientIds)
		}
	}

	handlerIds, handlersKnown := defaultProviderLivenessHandlers.get(ctx, now)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !handlersKnown || len(handlerIds) == 0 {
		findProviders2LivenessWriterCoverage.Set(0)
		failAllOpen()
		return
	}
	handlerMembers := make([]string, len(handlerIds))
	for i, handlerId := range handlerIds {
		handlerMembers[i] = handlerId.String()
	}

	shards := slices.Sorted(maps.Keys(shardClientIds))
	var writerNewestCmd *redis.ZSliceCmd
	var writerStampCmd *redis.FloatSliceCmd
	shardNewestCmds := make([]*redis.ZSliceCmd, len(shards))
	shardStampCmds := make([]*redis.FloatSliceCmd, len(shards))
	readErr := providerLivenessLeaseRedis(ctx, func(r server.RedisClient) error {
		// a failed command is judged with its own shard below
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			writerNewestCmd = pipe.ZRevRangeWithScores(ctx, providerLivenessWritersKey, 0, 0)
			writerStampCmd = pipe.ZMScore(ctx, providerLivenessWritersKey, handlerMembers...)
			for i, shard := range shards {
				key := providerLivenessLeaseKey(shard)
				members := make([]string, len(shardClientIds[shard]))
				for j, clientId := range shardClientIds[shard] {
					members[j] = clientId.String()
				}
				shardNewestCmds[i] = pipe.ZRevRangeWithScores(ctx, key, 0, 0)
				shardStampCmds[i] = pipe.ZMScore(ctx, key, members...)
			}
			return nil
		})
		return nil
	})
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	failed := func(cmds ...redis.Cmder) bool {
		for _, cmd := range cmds {
			if cmd == nil || cmd.Err() != nil {
				return true
			}
		}
		return false
	}
	if readErr != nil || failed(writerNewestCmd, writerStampCmd) {
		findProviders2LivenessReadErrors.Inc()
		failAllOpen()
		return
	}

	nowMillis := now.UnixMilli()
	writersCurrent, writersOk := judgeProviderLivenessStamps(
		nowMillis,
		providerLivenessNewestMillis(writerNewestCmd),
		providerLivenessStampMillis(writerStampCmd),
		providerLivenessWriterWindow,
	)
	coveredCount := 0
	for _, current := range writersCurrent {
		if current {
			coveredCount += 1
		}
	}
	findProviders2LivenessWriterCoverage.Set(float64(coveredCount) / float64(len(handlerIds)))
	if !writersOk || coveredCount < len(handlerIds) {
		failAllOpen()
		return
	}

	shardFailed := false
	for i, shard := range shards {
		clientIds := shardClientIds[shard]
		if failed(shardNewestCmds[i], shardStampCmds[i]) {
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
			failOpen(clientIds)
			continue
		}
		for j, clientId := range clientIds {
			if !live[j] {
				offline[clientId] = true
			}
		}
	}
	if shardFailed {
		findProviders2LivenessReadErrors.Inc()
	}
	return
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
