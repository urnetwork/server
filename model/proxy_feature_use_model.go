package model

// Pro feature use: which hosted proxies actually sign in over SOCKS or connect
// over WireGuard, the two proxy features pro.yml puts in Pro.
//
// The apps' "Pro features in use" notice (GetNetworkProxies, the sdk's
// pro_features_status.go) warns a network without Pro that enforce_features
// will take SOCKS and WireGuard from its proxies. Every proxy issued while
// enforcement is dark holds a SOCKS url, so credentials alone would warn every
// network with a proxy, including the ones that only use HTTP or HTTPS and
// lose nothing. Owner, 2026-11-11: warn only the networks whose proxies
// actually used SOCKS or WireGuard recently.
//
// Nothing recorded that per proxy (network_client.contract_time and auth_time
// say a proxy moved data or connected, not over which protocol), so the proxy
// records it here:
//   - a SOCKS sign-in with a proxy's signed credentials (proxy/server.go
//     validUser), whatever the plan answers, so a network still trying SOCKS
//     after enforcement keeps being told why it stopped;
//   - a completed WireGuard handshake of a proxy's peer (the session sampler,
//     proxy/wg_sessions.go).
//
// The sign-in path never waits on this. A use is noted in process
// (ProxyFeatureUses.Note), and a background flush writes one mark per proxy and
// feature at most once per ProxyFeatureUseMarkInterval on each proxy process:
// one redis hash per proxy, {pfu_<proxy_id>}, field = feature, value = the
// use's unix milliseconds, renewed for proxyFeatureUseTtl on every write, so a
// proxy that stops leaves nothing behind. No database write at all. A lost
// mark (a crash, a redis restart before its snapshot) is renewed by the
// proxy's next use, at most one interval later.
//
// The proxies list reads the marks back (GetProxyFeatureUses): a proxy lists a
// Pro-only feature it used within ProxyFeatureUseWindow.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// How far back a proxy's use counts for the notice. 14 days: the last use of
// free networks' proxies has no knee between one and two weeks (Main
// 2026-11-11: about 100 networks a day at every age from 1 to 29 days), so the
// window decides how long a lapsed user keeps the notice. Two weeks keeps it
// for a weekly user who skips a week and for a network that comes back in
// bursts, and it is how long a WireGuard user keeps the notice after
// enforcement, when a dropped peer can no longer handshake to renew it.
const ProxyFeatureUseWindow = 14 * 24 * time.Hour

// A proxy process writes one mark per proxy and feature at most this often;
// the uses in between are counted in process only.
const ProxyFeatureUseMarkInterval = time.Hour

// A proxy's hash expires this long after its last write: past the window, so
// every mark that still counts is kept.
const proxyFeatureUseTtl = ProxyFeatureUseWindow + 24*time.Hour

// The proxy features a mark records, the hash fields of the proxy's key.
var proxyFeatureUseFeatures = []string{FeatureSocksProxy, FeatureWireguardProxy}

// One proxy's marks. The hash tag is the proxy id, so a proxy is one cluster
// slot and the family spreads with the fleet.
func proxyFeatureUseKey(proxyId server.Id) string {
	return fmt.Sprintf("{pfu_%s}", proxyId)
}

// The plans the marks counter reports a proxy's network on.
const (
	proxyFeatureUsePlanFree    = "free"
	proxyFeatureUsePlanPro     = "pro"
	proxyFeatureUsePlanUnknown = "unknown"
)

var proxyFeatureUseMarks = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "proxy",
	Name:      "feature_use_marks_total",
	Help:      "SOCKS sign-ins and WireGuard handshakes recorded for the Pro features notice, at most one per proxy, feature and hour on each proxy process, by the plan of the proxy's network (free, pro, or unknown when it could not be read)",
}, []string{"feature", "plan"})

func init() {
	prometheus.MustRegister(proxyFeatureUseMarks)
}

// One recorded use: the newest use of a feature by a proxy.
type ProxyFeatureUseMark struct {
	ProxyId server.Id
	Feature string
	UseTime time.Time
}

// How a proxy process collects and writes its marks.
type ProxyFeatureUseSettings struct {
	// how often the noted uses are written
	FlushInterval time.Duration
	// the longest one flush may take, its plan reads for the counter included
	FlushTimeout time.Duration
	// a proxy's feature is written at most once per interval
	MarkInterval time.Duration
	// the most uses held between flushes; a use of a further proxy is dropped
	// and noted again at its next use
	MaxPendingUses int
	// the most written marks remembered for the interval; past it the memory
	// is cleared, which costs one more write per proxy and feature
	MaxMarkedUses int
	// how long the counter keeps a network's plan, and the most networks and
	// proxies it keeps
	PlanCacheTtl        time.Duration
	MaxPlanCacheEntries int
	// the least time between two flush failure log lines
	FailureLogInterval time.Duration
}

func DefaultProxyFeatureUseSettings() *ProxyFeatureUseSettings {
	return &ProxyFeatureUseSettings{
		FlushInterval: 30 * time.Second,
		// short, so a final write during a redis outage holds a stopping
		// proxy process at most this long
		FlushTimeout:        5 * time.Second,
		MarkInterval:        ProxyFeatureUseMarkInterval,
		MaxPendingUses:      64 * 1024,
		MaxMarkedUses:       256 * 1024,
		PlanCacheTtl:        time.Hour,
		MaxPlanCacheEntries: 64 * 1024,
		FailureLogInterval:  time.Minute,
	}
}

// A proxy's feature, as the process keys its uses.
type proxyFeatureUseKeyFeature struct {
	proxyId server.Id
	feature string
}

// The redis boundary of the marks, so the throttle and the flush can be tested
// without redis.
type proxyFeatureUseStore interface {
	markAll(ctx context.Context, marks []*ProxyFeatureUseMark) error
	readAll(ctx context.Context, proxyIds []server.Id) (map[server.Id]map[string]time.Time, error)
}

// The plans of the proxies' networks for the marks counter: free or pro per
// proxy, absent when unknown.
type proxyFeatureUsePlanReader interface {
	plans(ctx context.Context, proxyIds []server.Id) map[server.Id]string
}

// Notes the proxy's SOCKS sign-ins and WireGuard handshakes and writes them in
// the background. A proxy process owns one and closes it after its listeners
// stop; a nil owner notes nothing.
type ProxyFeatureUses struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *ProxyFeatureUseSettings
	store    proxyFeatureUseStore
	planner  proxyFeatureUsePlanReader
	nowFunc  func() time.Time
	logFunc  func(format string, args ...any)

	stateLock sync.Mutex
	// the newest use noted since the last flush, in unix milliseconds
	pending map[proxyFeatureUseKeyFeature]int64
	// the use each mark written or in flight carries, in unix milliseconds
	marked map[proxyFeatureUseKeyFeature]int64
	// flush failures since the last failure line
	failedCount    int
	lastFailure    error
	lastFailureLog time.Time

	// nil without the flush loop
	loopDone  chan struct{}
	closeOnce sync.Once
}

// A running owner: its loop writes the noted uses every FlushInterval until
// Close.
func NewProxyFeatureUses(ctx context.Context, settings *ProxyFeatureUseSettings) *ProxyFeatureUses {
	uses := newProxyFeatureUsesWithoutRun(
		ctx,
		settings,
		redisProxyFeatureUseStore{},
		newDbProxyFeatureUsePlans(settings),
		time.Now,
	)
	uses.loopDone = make(chan struct{})
	go server.HandleError(uses.run)
	return uses
}

// The owner without its flush loop; a test calls flush itself.
func newProxyFeatureUsesWithoutRun(
	ctx context.Context,
	settings *ProxyFeatureUseSettings,
	store proxyFeatureUseStore,
	planner proxyFeatureUsePlanReader,
	nowFunc func() time.Time,
) *ProxyFeatureUses {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &ProxyFeatureUses{
		ctx:      cancelCtx,
		cancel:   cancel,
		settings: settings,
		store:    store,
		planner:  planner,
		nowFunc:  nowFunc,
		logFunc:  glog.Infof,
		pending:  map[proxyFeatureUseKeyFeature]int64{},
		marked:   map[proxyFeatureUseKeyFeature]int64{},
	}
}

// Notes one use of a feature by a proxy at useTime, in process only: no I/O,
// one map lookup under a lock, so it is safe on the sign-in path. A use within
// MarkInterval of the proxy's last written mark changes nothing.
func (self *ProxyFeatureUses) Note(proxyId server.Id, feature string, useTime time.Time) {
	if self == nil {
		return
	}
	key := proxyFeatureUseKeyFeature{proxyId: proxyId, feature: feature}
	useMillis := useTime.UnixMilli()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if markedMillis, ok := self.marked[key]; ok && useMillis-markedMillis < self.settings.MarkInterval.Milliseconds() {
		return
	}
	if pendingMillis, ok := self.pending[key]; ok {
		self.pending[key] = max(pendingMillis, useMillis)
		return
	}
	if self.settings.MaxPendingUses <= len(self.pending) {
		// the proxy's next use notes it again
		return
	}
	self.pending[key] = useMillis
}

// Takes the pending uses as marks and claims them as written, so a use noted
// while they are in flight does not write them again. The marks an interval
// old no longer hold anything back and are forgotten, so the memory follows
// the last interval's uses; past MaxMarkedUses it is cleared.
func (self *ProxyFeatureUses) take() []*ProxyFeatureUseMark {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.pending) == 0 {
		return nil
	}
	expiredMillis := self.nowFunc().UnixMilli() - self.settings.MarkInterval.Milliseconds()
	for key, markedMillis := range self.marked {
		if markedMillis < expiredMillis {
			delete(self.marked, key)
		}
	}
	if self.settings.MaxMarkedUses <= len(self.marked)+len(self.pending) {
		self.marked = map[proxyFeatureUseKeyFeature]int64{}
	}
	marks := make([]*ProxyFeatureUseMark, 0, len(self.pending))
	for key, useMillis := range self.pending {
		marks = append(marks, &ProxyFeatureUseMark{
			ProxyId: key.proxyId,
			Feature: key.feature,
			UseTime: time.UnixMilli(useMillis).UTC(),
		})
		self.marked[key] = useMillis
	}
	self.pending = map[proxyFeatureUseKeyFeature]int64{}
	return marks
}

// Returns marks whose write failed to pending, so the next flush writes them,
// and releases their claim.
func (self *ProxyFeatureUses) restore(marks []*ProxyFeatureUseMark) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, mark := range marks {
		key := proxyFeatureUseKeyFeature{proxyId: mark.ProxyId, feature: mark.Feature}
		useMillis := mark.UseTime.UnixMilli()
		if self.marked[key] == useMillis {
			delete(self.marked, key)
		}
		if pendingMillis, ok := self.pending[key]; ok {
			self.pending[key] = max(pendingMillis, useMillis)
		} else if len(self.pending) < self.settings.MaxPendingUses {
			self.pending[key] = useMillis
		}
	}
}

// The flush loop: one flush every FlushInterval until the owner closes.
func (self *ProxyFeatureUses) run() {
	defer close(self.loopDone)
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.FlushInterval):
		}
		self.flush(self.ctx)
	}
}

// Flush writes the uses noted so far now, as the loop does every
// FlushInterval. The proxy calls it at the end of its drain, so the uses of
// its last minutes are written before it exits. Safe beside the loop: each
// flush takes its own uses. A nil owner writes nothing.
func (self *ProxyFeatureUses) Flush(ctx context.Context) {
	if self == nil {
		return
	}
	self.flush(ctx)
}

// Writes every pending use, bounded by FlushTimeout, then counts the written
// marks by plan. A failed write keeps its uses for the next flush.
func (self *ProxyFeatureUses) flush(ctx context.Context) (writtenCount int) {
	marks := self.take()
	if len(marks) == 0 {
		return 0
	}
	flushCtx, cancel := context.WithTimeout(ctx, self.settings.FlushTimeout)
	defer cancel()
	if err := self.store.markAll(flushCtx, marks); err != nil {
		self.restore(marks)
		self.logFailure(err, len(marks))
		return 0
	}
	self.countMarks(flushCtx, marks)
	return len(marks)
}

// Counts the written marks by the plan of each proxy's network.
func (self *ProxyFeatureUses) countMarks(ctx context.Context, marks []*ProxyFeatureUseMark) {
	proxyIds := make([]server.Id, 0, len(marks))
	seen := map[server.Id]bool{}
	for _, mark := range marks {
		if !seen[mark.ProxyId] {
			seen[mark.ProxyId] = true
			proxyIds = append(proxyIds, mark.ProxyId)
		}
	}
	var plans map[server.Id]string
	if self.planner != nil {
		server.HandleError(func() {
			plans = self.planner.plans(ctx, proxyIds)
		})
	}
	for _, mark := range marks {
		plan, ok := plans[mark.ProxyId]
		if !ok {
			plan = proxyFeatureUsePlanUnknown
		}
		proxyFeatureUseMarks.WithLabelValues(mark.Feature, plan).Inc()
	}
}

// At most one line per FailureLogInterval, summarizing every failed write since
// the previous line.
func (self *ProxyFeatureUses) logFailure(err error, markCount int) {
	now := self.nowFunc()
	failedCount, lastFailure, report := func() (int, error, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.failedCount += markCount
		self.lastFailure = err
		if now.Sub(self.lastFailureLog) < self.settings.FailureLogInterval {
			return 0, nil, false
		}
		failedCount, lastFailure := self.failedCount, self.lastFailure
		self.failedCount = 0
		self.lastFailure = nil
		self.lastFailureLog = now
		return failedCount, lastFailure, true
	}()
	if report {
		self.logFunc("[pfu]%d feature use marks not written since the last report, kept for the next flush (%s)\n", failedCount, lastFailure)
	}
}

// Stops the loop and writes what is left once, bounded by FlushTimeout. Close
// after the listeners that note into this owner have stopped.
func (self *ProxyFeatureUses) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		self.cancel()
		if self.loopDone != nil {
			select {
			case <-self.loopDone:
			case <-time.After(self.settings.FlushTimeout + self.settings.FlushInterval):
			}
		}
		self.flush(context.WithoutCancel(self.ctx))
	})
}

// The newest use of each Pro proxy feature per proxy, as the proxies wrote
// them: only the proxies and features with a mark that has not expired.
func GetProxyFeatureUses(ctx context.Context, proxyIds []server.Id) (map[server.Id]map[string]time.Time, error) {
	if len(proxyIds) == 0 {
		return map[server.Id]map[string]time.Time{}, nil
	}
	return redisProxyFeatureUseStore{}.readAll(ctx, proxyIds)
}

// Whether the feature has a use at or after since.
func proxyFeatureUsedSince(uses map[string]time.Time, feature string, since time.Time) bool {
	useTime, ok := uses[feature]
	return ok && !useTime.Before(since)
}

// The store a flush and a read use outside tests.
type redisProxyFeatureUseStore struct{}

// Runs one redis callback, returning a connection failure instead of raising
// it: a flush during an outage costs one bounded log line, and a list read
// that cannot reach the marks still lists the proxies.
func proxyFeatureUseRedis(ctx context.Context, callback func(server.RedisClient)) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", recovered)
			}
		}
	}()
	server.Redis(ctx, callback, server.OptNoRetry())
	return
}

// Marks per pipeline, so one flush never builds an unbounded batch.
const proxyFeatureUseMarkBatchSize = 1024

// One HSET and one PEXPIRE per mark, on the proxy's own key. The keys are
// different slots, so they go in plain pipelines, which route each command to
// its slot's node.
func (self redisProxyFeatureUseStore) markAll(ctx context.Context, marks []*ProxyFeatureUseMark) (returnErr error) {
	err := proxyFeatureUseRedis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(marks); start += proxyFeatureUseMarkBatchSize {
			batch := marks[start:min(start+proxyFeatureUseMarkBatchSize, len(marks))]
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, mark := range batch {
					key := proxyFeatureUseKey(mark.ProxyId)
					pipe.HSet(ctx, key, mark.Feature, strconv.FormatInt(mark.UseTime.UnixMilli(), 10))
					pipe.PExpire(ctx, key, proxyFeatureUseTtl)
				}
				return nil
			})
			if err != nil {
				returnErr = err
				return
			}
		}
	})
	if returnErr == nil {
		returnErr = err
	}
	return
}

// One HMGET per proxy key, pipelined; a proxy with no marks reads as absent.
func (self redisProxyFeatureUseStore) readAll(ctx context.Context, proxyIds []server.Id) (uses map[server.Id]map[string]time.Time, returnErr error) {
	uses = map[server.Id]map[string]time.Time{}
	err := proxyFeatureUseRedis(ctx, func(r server.RedisClient) {
		cmds := make([]*redis.SliceCmd, len(proxyIds))
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i, proxyId := range proxyIds {
				cmds[i] = pipe.HMGet(ctx, proxyFeatureUseKey(proxyId), proxyFeatureUseFeatures...)
			}
			return nil
		})
		if err != nil && !errors.Is(err, redis.Nil) {
			returnErr = err
			return
		}
		for i, proxyId := range proxyIds {
			values, err := cmds[i].Result()
			if err != nil {
				if !errors.Is(err, redis.Nil) {
					returnErr = err
					return
				}
				continue
			}
			for j, value := range values {
				if j < len(proxyFeatureUseFeatures) {
					if useTime, ok := parseProxyFeatureUseValue(value); ok {
						if uses[proxyId] == nil {
							uses[proxyId] = map[string]time.Time{}
						}
						uses[proxyId][proxyFeatureUseFeatures[j]] = useTime
					}
				}
			}
		}
	})
	if returnErr == nil {
		returnErr = err
	}
	if returnErr != nil {
		return nil, returnErr
	}
	return
}

// A mark's value: the use's unix milliseconds. Anything else is no mark.
func parseProxyFeatureUseValue(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	useMillis, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(useMillis).UTC(), true
}

// The plans the marks counter reads outside tests: each proxy's network from
// the database, in one query per flush for the proxies not cached (a proxy
// belongs to one network for its whole life), then IsProNetwork per network,
// kept for PlanCacheTtl. The counter is a rate of proxies, so a plan an hour
// old is fresh enough, and a flush costs at most one query plus one plan read
// per network not seen within the hour.
type dbProxyFeatureUsePlans struct {
	settings *ProxyFeatureUseSettings

	stateLock  sync.Mutex
	networkIds map[server.Id]server.Id
	networkPro map[server.Id]proxyFeatureUseNetworkPlan
}

type proxyFeatureUseNetworkPlan struct {
	pro      bool
	readTime time.Time
}

func newDbProxyFeatureUsePlans(settings *ProxyFeatureUseSettings) *dbProxyFeatureUsePlans {
	return &dbProxyFeatureUsePlans{
		settings:   settings,
		networkIds: map[server.Id]server.Id{},
		networkPro: map[server.Id]proxyFeatureUseNetworkPlan{},
	}
}

func (self *dbProxyFeatureUsePlans) plans(ctx context.Context, proxyIds []server.Id) map[server.Id]string {
	networkIds := map[server.Id]server.Id{}
	missingProxyIds := []server.Id{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, proxyId := range proxyIds {
			if networkId, ok := self.networkIds[proxyId]; ok {
				networkIds[proxyId] = networkId
			} else {
				missingProxyIds = append(missingProxyIds, proxyId)
			}
		}
	}()
	if 0 < len(missingProxyIds) {
		loadedNetworkIds := getProxyNetworkIds(ctx, missingProxyIds)
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.settings.MaxPlanCacheEntries <= len(self.networkIds)+len(loadedNetworkIds) {
				self.networkIds = map[server.Id]server.Id{}
			}
			for proxyId, networkId := range loadedNetworkIds {
				self.networkIds[proxyId] = networkId
				networkIds[proxyId] = networkId
			}
		}()
	}

	now := time.Now()
	plans := map[server.Id]string{}
	for proxyId, networkId := range networkIds {
		pro, ok := func() (bool, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			plan, ok := self.networkPro[networkId]
			return plan.pro, ok && now.Sub(plan.readTime) < self.settings.PlanCacheTtl
		}()
		if !ok {
			pro = IsProNetwork(ctx, networkId)
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.settings.MaxPlanCacheEntries <= len(self.networkPro) {
					self.networkPro = map[server.Id]proxyFeatureUseNetworkPlan{}
				}
				self.networkPro[networkId] = proxyFeatureUseNetworkPlan{pro: pro, readTime: now}
			}()
		}
		if pro {
			plans[proxyId] = proxyFeatureUsePlanPro
		} else {
			plans[proxyId] = proxyFeatureUsePlanFree
		}
	}
	return plans
}

// The network of each proxy that still has a proxy client, in one query.
func getProxyNetworkIds(ctx context.Context, proxyIds []server.Id) map[server.Id]server.Id {
	networkIds := map[server.Id]server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					proxy_client.proxy_id,
					network_client.network_id
				FROM proxy_client
				INNER JOIN network_client ON
					network_client.client_id = proxy_client.client_id
				WHERE proxy_client.proxy_id = ANY($1)
			`,
			proxyIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var proxyId server.Id
				var networkId server.Id
				server.Raise(result.Scan(&proxyId, &networkId))
				networkIds[proxyId] = networkId
			}
		})
	})
	return networkIds
}
