package model

// How often each provider client was offered to clients, per minute, over the
// last hour (the provider status histogram, GET /network/provider-status).
//
// FindProviders2 is a hot path, so an answer never touches redis here: the
// lifecycle owner (ProviderAppearances, one per API router or proxy process)
// counts each returned provider in process per (client, minute), and a
// background loop writes the counts every few seconds, one same-key command
// per provider. Each provider's minutes live in one hash whose hash tag is its
// client id, so a read is one command. Lost counts (a crash, a redis blip) are
// acceptable: the histogram is a display, not accounting.

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

const ProviderAppearanceBucketDuration = time.Minute

// The minutes a histogram reads, the current one last.
const ProviderAppearanceWindowBuckets = 60

// The minutes a provider's hash keeps: the window, plus one minute of clock
// skew between the writing processes.
const providerAppearanceRetainedBuckets = ProviderAppearanceWindowBuckets + 1

// A hash expires this long after its last write, so an idle provider leaves
// nothing behind.
const ProviderAppearanceTtl = 65 * time.Minute

// The stale minutes each write removes, oldest first, ending at the newest
// minute the hash no longer keeps. A field ages out of the window, and the
// next write to the same hash comes at most one ttl after the previous one or
// finds the hash expired, so this range always covers it (see
// TestProviderAppearanceHashStaysBounded); five more minutes absorb clock skew
// between writers.
const providerAppearanceStaleBuckets = int64(ProviderAppearanceTtl/ProviderAppearanceBucketDuration) + 5

var providerAppearanceEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_appearances_total",
	Help: "FindProviders2 provider appearances by outcome: recorded in process, written to redis, or dropped (capacity, stale, write failure, no lifecycle owner)",
}, []string{"event"})

// resolved once: the recorded and unowned counters are on the FindProviders2
// path
var (
	providerAppearancesRecorded        = providerAppearanceEvents.WithLabelValues("recorded")
	providerAppearancesWritten         = providerAppearanceEvents.WithLabelValues("written")
	providerAppearancesDroppedCapacity = providerAppearanceEvents.WithLabelValues("dropped_capacity")
	providerAppearancesDroppedStale    = providerAppearanceEvents.WithLabelValues("dropped_stale")
	providerAppearancesDroppedWrite    = providerAppearanceEvents.WithLabelValues("dropped_write")
	providerAppearancesUnowned         = providerAppearanceEvents.WithLabelValues("unowned")
)

// Registers the appearance counter with the default registry.
func init() {
	prometheus.MustRegister(providerAppearanceEvents)
}

// How a lifecycle owner collects and writes the appearance counts.
type ProviderAppearanceSettings struct {
	// how often the counts collected in process are written
	FlushInterval time.Duration
	// the longest one flush may take; the counts it has not written by then
	// are dropped
	FlushTimeout time.Duration
	// concurrent per-provider writes in one flush
	FlushParallel int
	// the most (client, minute) counts held between flushes; appearances of
	// further providers are dropped until the next flush
	MaxPendingCounts int
	// the least time between two flush failure log lines
	FailureLogInterval time.Duration
}

// The settings of the API router and proxy process owners.
func DefaultProviderAppearanceSettings() *ProviderAppearanceSettings {
	return &ProviderAppearanceSettings{
		FlushInterval:      5 * time.Second,
		FlushTimeout:       4 * time.Second,
		FlushParallel:      8,
		MaxPendingCounts:   128 * 1024,
		FailureLogInterval: time.Minute,
	}
}

// The minute a time falls in, in unix minutes.
func providerAppearanceMinute(t time.Time) int64 {
	return t.Unix() / int64(ProviderAppearanceBucketDuration/time.Second)
}

// One provider's minutes, field = unix minute, value = appearances. The hash
// tag is the provider's client id: one provider is one cluster slot, and the
// family spreads with the fleet.
func providerAppearanceKey(clientId server.Id) string {
	return fmt.Sprintf("{pa_%s}m", clientId)
}

// The hash field of a unix minute.
func providerAppearanceField(minute int64) string {
	return strconv.FormatInt(minute, 10)
}

// One provider's count in one minute, as the counter keys it in process.
type providerAppearanceKeyMinute struct {
	clientId server.Id
	minute   int64
}

const providerAppearanceShardCount = 32

// Requests touching different providers rarely share a shard lock.
type providerAppearanceShard struct {
	stateLock       sync.Mutex
	keyMinuteCounts map[providerAppearanceKeyMinute]int64
}

// Counts one appearance unless the shard is full and the count is new.
func (self *providerAppearanceShard) add(key providerAppearanceKeyMinute, maxCounts int) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.keyMinuteCounts[key]; !ok && maxCounts <= len(self.keyMinuteCounts) {
		return false
	}
	self.keyMinuteCounts[key] += 1
	return true
}

// Takes every count of the shard, leaving it empty.
func (self *providerAppearanceShard) drain() map[providerAppearanceKeyMinute]int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	keyMinuteCounts := self.keyMinuteCounts
	self.keyMinuteCounts = map[providerAppearanceKeyMinute]int64{}
	return keyMinuteCounts
}

// The counts collected between flushes, bounded by maxPendingCounts.
type providerAppearanceCounter struct {
	maxShardCounts int
	shards         [providerAppearanceShardCount]providerAppearanceShard
}

// An empty counter whose shards together hold at most maxPendingCounts counts.
func newProviderAppearanceCounter(maxPendingCounts int) *providerAppearanceCounter {
	counter := &providerAppearanceCounter{
		maxShardCounts: max(1, maxPendingCounts/providerAppearanceShardCount),
	}
	for i := range counter.shards {
		counter.shards[i].keyMinuteCounts = map[providerAppearanceKeyMinute]int64{}
	}
	return counter
}

// The shard that holds the client's counts.
func (self *providerAppearanceCounter) shard(clientId server.Id) *providerAppearanceShard {
	return &self.shards[int(clientId[len(clientId)-1])%providerAppearanceShardCount]
}

// Counts one appearance per entry at minute and returns how many were
// dropped for capacity.
func (self *providerAppearanceCounter) add(clientIds []server.Id, minute int64) (droppedCount int) {
	for _, clientId := range clientIds {
		if !self.shard(clientId).add(providerAppearanceKeyMinute{clientId: clientId, minute: minute}, self.maxShardCounts) {
			droppedCount += 1
		}
	}
	return
}

// Takes every pending count, per client then minute.
func (self *providerAppearanceCounter) drain() map[server.Id]map[int64]int64 {
	clientMinuteCounts := map[server.Id]map[int64]int64{}
	for i := range self.shards {
		for key, count := range self.shards[i].drain() {
			minuteCounts, ok := clientMinuteCounts[key.clientId]
			if !ok {
				minuteCounts = map[int64]int64{}
				clientMinuteCounts[key.clientId] = minuteCounts
			}
			minuteCounts[key.minute] += count
		}
	}
	return clientMinuteCounts
}

// One provider's write: its new counts, the stale minutes it removes and the
// renewed expiry, all on one key.
type providerAppearanceWrite struct {
	clientId        server.Id
	key             string
	fieldIncrements map[string]int64
	staleFields     []string
	ttl             time.Duration
	// the appearances the increments carry
	count int64
}

// Builds a provider's write at nowMinute. Counts for minutes the window no
// longer holds are dropped, never written.
func newProviderAppearanceWrite(clientId server.Id, minuteCounts map[int64]int64, nowMinute int64) (write *providerAppearanceWrite, staleCount int64) {
	write = &providerAppearanceWrite{
		clientId:        clientId,
		key:             providerAppearanceKey(clientId),
		fieldIncrements: map[string]int64{},
		ttl:             ProviderAppearanceTtl,
	}
	oldestMinute := nowMinute - int64(providerAppearanceRetainedBuckets) + 1
	for minute, count := range minuteCounts {
		if minute < oldestMinute {
			staleCount += count
			continue
		}
		write.fieldIncrements[providerAppearanceField(minute)] += count
		write.count += count
	}
	for minute := oldestMinute - providerAppearanceStaleBuckets; minute < oldestMinute; minute += 1 {
		write.staleFields = append(write.staleFields, providerAppearanceField(minute))
	}
	return
}

// The redis boundary of a flush, so bucketing, bounds and failures can be
// tested without redis. writeAll applies each write with at most parallel in
// flight and reports each write's error by index.
type providerAppearanceStore interface {
	writeAll(ctx context.Context, writes []*providerAppearanceWrite, parallel int) []error
	readAll(ctx context.Context, clientIds []server.Id) ([]map[string]string, error)
}

// The store a flush and a read use outside tests.
type redisProviderAppearanceStore struct{}

// Runs one redis callback, returning a connection failure instead of raising
// it: a flush during an outage must cost one bounded log line, not a stack
// trace per attempt.
func providerAppearanceRedis(ctx context.Context, callback func(server.RedisClient)) (returnErr error) {
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

// Applies each write as one transaction on its provider key. Every command of
// a write targets that one key (one slot), so batching it in a transaction is
// cluster-safe.
func (self redisProviderAppearanceStore) writeAll(ctx context.Context, writes []*providerAppearanceWrite, parallel int) []error {
	errs := make([]error, len(writes))
	err := providerAppearanceRedis(ctx, func(r server.RedisClient) {
		writeProviderAppearance := func(write *providerAppearanceWrite) error {
			_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for field, count := range write.fieldIncrements {
					pipe.HIncrBy(ctx, write.key, field, count)
				}
				if 0 < len(write.staleFields) {
					pipe.HDel(ctx, write.key, write.staleFields...)
				}
				pipe.PExpire(ctx, write.key, write.ttl)
				return nil
			})
			return err
		}

		next := make(chan int)
		var wg sync.WaitGroup
		for range max(1, min(parallel, len(writes))) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					server.HandleError(func() {
						errs[i] = writeProviderAppearance(writes[i])
					}, func(err error) {
						errs[i] = err
					})
				}

			}()
		}
		func() {
			defer close(next)
			for i := range writes {
				select {
				case <-ctx.Done():
					for j := i; j < len(writes); j += 1 {
						errs[j] = ctx.Err()
					}
					return
				case next <- i:
				}
			}
		}()
		wg.Wait()
	})
	if err != nil {
		for i := range errs {
			if errs[i] == nil {
				errs[i] = err
			}
		}
	}
	return errs
}

// One command per provider key; the keys are different slots, so they are
// read one by one.
func (self redisProviderAppearanceStore) readAll(ctx context.Context, clientIds []server.Id) (fieldsList []map[string]string, returnErr error) {
	fieldsList = make([]map[string]string, len(clientIds))
	err := providerAppearanceRedis(ctx, func(r server.RedisClient) {
		for i, clientId := range clientIds {
			fields, err := r.HGetAll(ctx, providerAppearanceKey(clientId)).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				returnErr = err
				return
			}
			fieldsList[i] = fields
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

// Counts the providers FindProviders2 answers return and writes the counts in
// the background. A router or process owns one, installs it with
// WithProviderAppearances, and closes it after its requests drain; a context
// without an owner counts nothing (and no hidden singleton or lazy goroutine
// stands in for one).
type ProviderAppearances struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *ProviderAppearanceSettings
	counter  *providerAppearanceCounter
	store    providerAppearanceStore
	nowFunc  func() time.Time
	logFunc  func(format string, args ...any)

	// flush failures since the last failure line
	stateLock           sync.Mutex
	failedCount         int64
	failedProviderCount int
	lastFailure         error
	lastFailureLog      time.Time

	// nil without the flush loop
	loopDone  chan struct{}
	closeOnce sync.Once
}

// A running owner: its flush loop writes the counts every FlushInterval until
// Close.
func NewProviderAppearances(ctx context.Context, settings *ProviderAppearanceSettings) *ProviderAppearances {
	appearances := newProviderAppearancesWithoutRun(ctx, settings, redisProviderAppearanceStore{}, time.Now)
	appearances.loopDone = make(chan struct{})
	go server.HandleError(appearances.run)
	return appearances
}

// The owner without its flush loop; a test calls flush itself.
func newProviderAppearancesWithoutRun(
	ctx context.Context,
	settings *ProviderAppearanceSettings,
	store providerAppearanceStore,
	nowFunc func() time.Time,
) *ProviderAppearances {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &ProviderAppearances{
		ctx:      cancelCtx,
		cancel:   cancel,
		settings: settings,
		counter:  newProviderAppearanceCounter(settings.MaxPendingCounts),
		store:    store,
		nowFunc:  nowFunc,
		logFunc:  glog.Infof,
	}
}

// The context key of the lifecycle owner.
type providerAppearancesContextKey struct{}

// Only an explicit lifecycle owner installs this context value.
func WithProviderAppearances(ctx context.Context, appearances *ProviderAppearances) context.Context {
	return context.WithValue(ctx, providerAppearancesContextKey{}, appearances)
}

// The lifecycle owner the context carries, nil when it has none.
func GetProviderAppearances(ctx context.Context) *ProviderAppearances {
	appearances, _ := ctx.Value(providerAppearancesContextKey{}).(*ProviderAppearances)
	return appearances
}

// Counts one appearance for each provider of a FindProviders2 answer, in
// process only.
func recordProviderAppearances(ctx context.Context, clientIds []server.Id, now time.Time) {
	if len(clientIds) == 0 {
		return
	}
	appearances := GetProviderAppearances(ctx)
	if appearances == nil {
		providerAppearancesUnowned.Add(float64(len(clientIds)))
		return
	}
	appearances.Record(clientIds, now)
}

// Counts one appearance for each of clientIds at now, in process only. A count
// the counter has no room for is dropped. A nil owner counts nothing.
func (self *ProviderAppearances) Record(clientIds []server.Id, now time.Time) {
	if self == nil || len(clientIds) == 0 {
		return
	}
	droppedCount := self.counter.add(clientIds, providerAppearanceMinute(now))
	providerAppearancesRecorded.Add(float64(len(clientIds) - droppedCount))
	if 0 < droppedCount {
		providerAppearancesDroppedCapacity.Add(float64(droppedCount))
	}
}

// The flush loop: one flush every FlushInterval until the owner closes.
func (self *ProviderAppearances) run() {
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

// What one flush did with the counts it drained.
type providerAppearanceFlushResult struct {
	writtenCount        int64
	staleCount          int64
	failedCount         int64
	failedProviderCount int
	firstErr            error
}

// Writes every pending count, bounded by FlushTimeout. A failed write drops
// its counts.
func (self *ProviderAppearances) flush(ctx context.Context) (result providerAppearanceFlushResult) {
	clientMinuteCounts := self.counter.drain()
	if len(clientMinuteCounts) == 0 {
		return
	}
	now := self.nowFunc()
	nowMinute := providerAppearanceMinute(now)
	writes := make([]*providerAppearanceWrite, 0, len(clientMinuteCounts))
	for clientId, minuteCounts := range clientMinuteCounts {
		write, staleCount := newProviderAppearanceWrite(clientId, minuteCounts, nowMinute)
		result.staleCount += staleCount
		if 0 < len(write.fieldIncrements) {
			writes = append(writes, write)
		}
	}

	if 0 < len(writes) {
		flushCtx, cancel := context.WithTimeout(ctx, self.settings.FlushTimeout)
		defer cancel()
		for i, err := range self.store.writeAll(flushCtx, writes, self.settings.FlushParallel) {
			if err == nil {
				result.writtenCount += writes[i].count
				continue
			}
			result.failedCount += writes[i].count
			result.failedProviderCount += 1
			if result.firstErr == nil {
				result.firstErr = err
			}
		}
	}

	providerAppearancesWritten.Add(float64(result.writtenCount))
	providerAppearancesDroppedStale.Add(float64(result.staleCount))
	providerAppearancesDroppedWrite.Add(float64(result.failedCount))
	self.logFailures(now, result)
	return
}

// At most one line per FailureLogInterval, summarizing every drop since the
// previous line.
func (self *ProviderAppearances) logFailures(now time.Time, result providerAppearanceFlushResult) {
	// the line is taken under the lock and logged after it
	failedCount, failedProviderCount, lastFailure, report := func() (int64, int, error, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if result.firstErr != nil {
			self.failedCount += result.failedCount
			self.failedProviderCount += result.failedProviderCount
			self.lastFailure = result.firstErr
		}
		if self.lastFailure == nil || now.Sub(self.lastFailureLog) < self.settings.FailureLogInterval {
			return 0, 0, nil, false
		}
		failedCount, failedProviderCount, lastFailure := self.failedCount, self.failedProviderCount, self.lastFailure
		self.failedCount = 0
		self.failedProviderCount = 0
		self.lastFailure = nil
		self.lastFailureLog = now
		return failedCount, failedProviderCount, lastFailure, true
	}()
	if !report {
		return
	}
	self.logFunc(
		"[pa]dropped %d provider appearances of %d provider writes since the last report (%s)\n",
		failedCount,
		failedProviderCount,
		lastFailure,
	)
}

// Stops the loop and writes what is left once, bounded by FlushTimeout.
// Close after the requests that record into this owner have drained.
func (self *ProviderAppearances) Close() {
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

// One provider's appearances per minute over the last
// ProviderAppearanceWindowBuckets minutes.
type ProviderAppearanceHistogram struct {
	// the unix minute (unix seconds / 60) of the first, oldest bucket
	StartMinute   int64 `json:"start_minute"`
	BucketSeconds int   `json:"bucket_seconds"`
	// oldest first; the last is the current, partial minute
	AppearancesPerMinute []int64 `json:"appearances_per_minute"`
}

// The window ending at endMinute from a provider's hash fields, zero filled.
// Fields outside the window and malformed fields are ignored.
func newProviderAppearanceHistogram(fields map[string]string, endMinute int64) *ProviderAppearanceHistogram {
	startMinute := endMinute - ProviderAppearanceWindowBuckets + 1
	histogram := &ProviderAppearanceHistogram{
		StartMinute:          startMinute,
		BucketSeconds:        int(ProviderAppearanceBucketDuration / time.Second),
		AppearancesPerMinute: make([]int64, ProviderAppearanceWindowBuckets),
	}
	for field, value := range fields {
		minute, err := strconv.ParseInt(field, 10, 64)
		if err != nil || minute < startMinute || endMinute < minute {
			continue
		}
		count, err := strconv.ParseInt(value, 10, 64)
		if err != nil || count < 0 {
			continue
		}
		histogram.AppearancesPerMinute[minute-startMinute] = count
	}
	return histogram
}

// Reads the last hour of each client's appearances, zero filled, oldest
// first: one read per client.
func GetProviderAppearanceHistograms(ctx context.Context, clientIds []server.Id, now time.Time) (map[server.Id]*ProviderAppearanceHistogram, error) {
	return getProviderAppearanceHistograms(ctx, redisProviderAppearanceStore{}, clientIds, now)
}

// The histograms from store, so a test reads them without redis.
func getProviderAppearanceHistograms(
	ctx context.Context,
	store providerAppearanceStore,
	clientIds []server.Id,
	now time.Time,
) (map[server.Id]*ProviderAppearanceHistogram, error) {
	histograms := map[server.Id]*ProviderAppearanceHistogram{}
	if len(clientIds) == 0 {
		return histograms, nil
	}
	fieldsList, err := store.readAll(ctx, clientIds)
	if err != nil {
		return nil, err
	}
	endMinute := providerAppearanceMinute(now)
	for i, clientId := range clientIds {
		histograms[clientId] = newProviderAppearanceHistogram(fieldsList[i], endMinute)
	}
	return histograms, nil
}
