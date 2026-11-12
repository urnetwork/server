package sidecart3

// T3's store clock (TEST-M2-07; QD-264, plan-TEST-clock-seam #2): the one reviewed file where T3 translates time. A
// schedule moves its fake clock, which engines and FileClusters read, and T3's stores keep their own real time, so
// without this an object written after an Advance reads as old the moment it lands and a ttl a schedule outlives
// never passes. A T3 Net translates three things:
//
//   - Deadlines, on every T3 Net: each request's x-ur-deadline-ms is the store's time the same span ahead of the
//     store's now as the deadline is of the schedule's (StoreTime), so the fork's clusters, which refuse a deadline
//     already past by their own clock, see the budget the client gave. The client's cancel at a deadline keeps the
//     schedule's clock (server.WithFileClusterStoreTime).
//   - LastModified, with the store clock on: the Blobs wrapper records each write on the schedule's clock by the
//     ETag the store answers, and every Head, Get, GetRange and List returns that time as LastModified, so a 25 h
//     age reads right for an object written before an Advance and for one written after it. A write whose answer
//     was lost is recorded with no ETag, and the first read of its key showing an ETag no write recorded takes it.
//     A DELETE naming a LastModified (sidecar.WithBlobUnmodified) is judged by the schedule's times and sent so the
//     store judges alike: a time no store comparison can pass when a recorded write is a second or more newer than
//     the one named, and one any passes otherwise.
//   - Redis ttls, with the store clock on: when the schedule's clock has moved, the next Redis call, or Advance at
//     once, shortens every key ttl in the families the row names by PEXPIRE, and a hash's field ttls by HPEXPIRE,
//     and deletes what the move passed. Every other key keeps the store's time: server's exchange and controller,
//     which keep real time, write their own.
//
// What it cannot move: Postgres now() and a script's Redis TIME, so a clause reading either after an Advance runs on
// T2 alone; and MinIO's own age behaviour, the settle rule's deadlines and held keys, drive timeouts and the trash
// sweep, which a wrapper would hide, so FS-114 and §22.9's file-cluster suite never run with it and run on real
// time. The catalog's store_clock flag marks every case the store clock translates: RunRow runs a flagged case with
// it and an unflagged one without it, refuses FS-114 with it, and fails a run whose read the store clock could not
// translate, so a gate report says what its pass proved. Real time still runs on during a test, so a ttl shorter
// than the test's own real-time length ages past what the schedule's clock says, which this does not undo.
//
// Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
)

// Rows the store clock refuses (QD-264): FS-114's settle cases, whose deadlines, held keys and drive timeouts are
// MinIO's own age behaviour, on the store's real time.
var storeClockRefusedRowIds = []string{"FS-114"}

// What the store clock translates, and on what.
type StoreClockSettings struct {
	// The Redis families whose ttls an Advance shortens, as SCAN MATCH patterns such as "{evsl_*}*": the families a
	// row's clauses age. Empty shortens none.
	RedisFamilies []string
	// How many keys one SCAN asks for: 1,000. Positive.
	RedisScanCount int64
	// How many fields one HPTTL or HPEXPIRE names: 1,000. Positive.
	RedisFieldCount int
}

func DefaultStoreClockSettings() *StoreClockSettings {
	return &StoreClockSettings{
		RedisScanCount:  1000,
		RedisFieldCount: 1000,
	}
}

// One key of one cluster.
type storeClockKey struct {
	clusterId string
	key       string
}

// One write the wrapper saw.
type storeClockWrite struct {
	// the schedule's time when it was sent
	scheduleTime time.Time
	// what the store answered; empty for a write whose answer was lost, until a read shows its ETag
	etag string
}

// One T3 Net's store clock: LastModified and Redis ttls on the schedule's clock.
type StoreClock struct {
	net      *sidecartest.Net
	settings *StoreClockSettings
	// the store's own clock, which LastModified and ttls run on
	store sidecar.Clock

	stateLock sync.Mutex
	// each write the wrapper saw, by cluster and key, in the order sent
	keyWrites map[storeClockKey][]*storeClockWrite
	// the schedule's time the Redis families were last aged to
	agedTime time.Time
	// reads of an object no recorded write made, each translated by its age on the store's clock alone
	untranslated []string
	// what aging the Redis families failed on
	ageErrs []error

	// held while the Redis families age, so a move is applied once
	ageLock sync.Mutex
}

func newStoreClock(net *sidecartest.Net, settings *StoreClockSettings, store sidecar.Clock) *StoreClock {
	return &StoreClock{
		net:       net,
		settings:  settings,
		store:     store,
		keyWrites: map[storeClockKey][]*storeClockWrite{},
		agedTime:  net.Clock().Now(),
	}
}

// What a T3 Net's clocks are: the store's own, and the store clock when its Settings turn it on.
type netClocks struct {
	store      sidecar.Clock
	storeClock *StoreClock
}

// Each open T3 Net's clocks, so StoreTime and Advance find them from the Net. A Net leaves at its close.
var netClockNets sync.Map

func netClocksOf(net *sidecartest.Net) *netClocks {
	if clocks, ok := netClockNets.Load(net); ok {
		return clocks.(*netClocks)
	}
	return nil
}

// The store's time for a time on net's clock: the store's now plus how far scheduleTime is from the schedule's
// now. What every T3 request's x-ur-deadline-ms carries. A Net that is no open T3 Net's keeps the time as it is.
func StoreTime(net *sidecartest.Net, scheduleTime time.Time) time.Time {
	clocks := netClocksOf(net)
	if clocks == nil {
		return scheduleTime
	}
	return clocks.store.Now().Add(scheduleTime.Sub(net.Clock().Now()))
}

// Moves net's clock forward, as a schedule's Advance does, and ages its store clock's Redis families at once rather
// than at the next Redis call. A Net with no store clock only moves its clock.
func Advance(ctx context.Context, net *sidecartest.Net, timeout time.Duration) error {
	net.Clock().Advance(timeout)
	if storeClock := StoreClockOf(net); storeClock != nil {
		return storeClock.age(ctx)
	}
	return nil
}

// net's store clock, or nil for a Net without one.
func StoreClockOf(net *sidecartest.Net) *StoreClock {
	if clocks := netClocksOf(net); clocks != nil {
		return clocks.storeClock
	}
	return nil
}

// The schedule's time of the newest write of key the store clock recorded, as a read of its object reports it, and
// whether one was recorded.
func (self *StoreClock) WriteTime(clusterId string, key string) (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	writes := self.keyWrites[storeClockKey{clusterId: clusterId, key: key}]
	if len(writes) == 0 {
		return time.Time{}, false
	}
	return writes[len(writes)-1].scheduleTime, true
}

// Each read of an object no recorded write made, so no translation covers it: what fails a store-clock case.
func (self *StoreClock) Untranslated() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.untranslated)
}

// What aging the Redis families failed on, so a run that could not shorten a ttl fails rather than passing on real
// time.
func (self *StoreClock) Errors() []error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.ageErrs)
}

// Whether err is a write's definite refusal, which wrote nothing (host_blobs.go).
func storeClockRefused(err error) bool {
	return errors.Is(err, sidecar.ErrBlobPrecondition) || errors.Is(err, sidecar.ErrBlobHeld) ||
		errors.Is(err, sidecar.ErrBlobChecksum) || errors.Is(err, sidecar.ErrBlobPutOptions)
}

// Records a PUT sent at scheduleTime: answered with etag, or with err, a lost answer recorded with no ETag and a
// refusal not at all.
func (self *StoreClock) put(clusterId string, key string, scheduleTime time.Time, etag string, err error) {
	if err != nil && storeClockRefused(err) {
		return
	}
	write := &storeClockWrite{scheduleTime: scheduleTime}
	if err == nil {
		write.etag = etag
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	objectKey := storeClockKey{clusterId: clusterId, key: key}
	self.keyWrites[objectKey] = append(self.keyWrites[objectKey], write)
}

// Forgets a key a DELETE removed: its writes name no object any more.
func (self *StoreClock) deleted(clusterId string, key string, err error) {
	if err != nil {
		// a refusal deleted nothing, and an unknown outcome may have
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.keyWrites, storeClockKey{clusterId: clusterId, key: key})
}

// Gives info, as the store answered it, LastModified on the schedule's clock: the time of the write whose ETag it
// holds, or the newest write still awaiting one, which then takes it. One no write recorded keeps its age on the
// store's clock and is listed as untranslated.
func (self *StoreClock) translate(clusterId string, info *sidecar.BlobInfo) {
	if info == nil {
		return
	}
	scheduleTime, ok := func() (time.Time, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		writes := self.keyWrites[storeClockKey{clusterId: clusterId, key: info.Key}]
		for index := len(writes) - 1; 0 <= index; index -= 1 {
			if writes[index].etag == info.Etag {
				return writes[index].scheduleTime, true
			}
		}
		for index := len(writes) - 1; 0 <= index; index -= 1 {
			if writes[index].etag == "" {
				writes[index].etag = info.Etag
				return writes[index].scheduleTime, true
			}
		}
		self.untranslated = append(self.untranslated, fmt.Sprintf("cluster %s key %s at etag %s", clusterId, info.Key, info.Etag))
		return time.Time{}, false
	}()
	if !ok {
		scheduleTime = self.net.Clock().Now().Add(-self.store.Now().Sub(info.LastModified))
	}
	info.LastModified = scheduleTime
}

// ctx for a DELETE of key on the store: one that names a LastModified (sidecar.WithBlobUnmodified) is judged on the
// schedule's clock by what the wrapper recorded, and names a time the store judges alike, whatever its own clock
// stamped. One that names nothing is ctx.
func (self *StoreClock) unmodified(ctx context.Context, clusterId string, key string) context.Context {
	named, ok := sidecar.BlobUnmodified(ctx)
	if !ok {
		return ctx
	}
	refuses, recorded := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		writes := self.keyWrites[storeClockKey{clusterId: clusterId, key: key}]
		refuses := false
		for _, write := range writes {
			// a write whose answer was lost may stand at the store, so it counts too
			refuses = refuses || sidecar.BlobUnmodifiedRefuses(named, write.scheduleTime)
		}
		return refuses, 0 < len(writes)
	}()
	switch {
	case !recorded:
		// nothing written through the wrapper: the time named, as its age on the store's clock
		return sidecar.WithBlobUnmodified(ctx, self.store.Now().Add(-self.net.Clock().Now().Sub(named)))
	case refuses:
		// no stored object passes a time this old
		return sidecar.WithBlobUnmodified(ctx, time.UnixMilli(0))
	}
	// every stored object passes a time this far ahead
	return sidecar.WithBlobUnmodified(ctx, self.store.Now().Add(24*time.Hour))
}

// Shortens the Redis families' ttls by how far the schedule's clock has moved since they were last aged, deleting a
// key or field the move passed. A failure is kept for Errors and returned.
func (self *StoreClock) age(ctx context.Context) error {
	if len(self.settings.RedisFamilies) == 0 {
		return nil
	}
	moved := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.agedTime.Before(self.net.Clock().Now())
	}()
	if !moved {
		return nil
	}
	self.ageLock.Lock()
	defer self.ageLock.Unlock()
	now := self.net.Clock().Now()
	timeout := func() time.Duration {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		timeout := now.Sub(self.agedTime)
		if 0 < timeout {
			self.agedTime = now
		}
		return timeout
	}()
	if timeout <= 0 {
		return nil
	}
	err := self.ageRedis(context.WithoutCancel(ctx), timeout)
	if err != nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ageErrs = append(self.ageErrs, err)
	}
	return err
}

// One pass of aging over every master: each family's keys found by SCAN, their ttls and their fields' shortened by
// timeout.
func (self *StoreClock) ageRedis(ctx context.Context, timeout time.Duration) (returnErr error) {
	defer func() {
		// server.Redis raises a pool's failure as a panic
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("the store clock's Redis: %v", recovered)
		}
	}()
	var client server.RedisClient
	server.Redis(ctx, func(redisClient server.RedisClient) {
		client = redisClient
	})
	scanNode := func(ctx context.Context, node redis.Cmdable) error {
		for _, pattern := range self.settings.RedisFamilies {
			cursor := uint64(0)
			for {
				keys, next, err := node.Scan(ctx, cursor, pattern, self.settings.RedisScanCount).Result()
				if err != nil {
					return fmt.Errorf("SCAN %s: %w", pattern, err)
				}
				for _, key := range keys {
					// a key's own commands go through client, which routes it to its slot's master
					if err := self.ageKey(ctx, client, key, timeout); err != nil {
						return err
					}
				}
				if next == 0 {
					break
				}
				cursor = next
			}
		}
		return nil
	}
	if clusterClient, ok := client.(*redis.ClusterClient); ok {
		// one goroutine a master: each master's keys are its own
		return clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			return scanNode(ctx, node)
		})
	}
	return scanNode(ctx, client)
}

// Shortens one key's ttl, and a hash's field ttls, by timeout.
func (self *StoreClock) ageKey(ctx context.Context, client server.RedisClient, key string, timeout time.Duration) error {
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("PTTL %s: %w", key, err)
	}
	// -1 for a key with no ttl and -2 for one gone, which go-redis reads as those nanoseconds
	if 0 < ttl {
		if remaining := ttl - timeout; remaining <= 0 {
			if err := client.Del(ctx, key).Err(); err != nil {
				return fmt.Errorf("DEL %s: %w", key, err)
			}
			return nil
		} else if err := client.PExpire(ctx, key, remaining).Err(); err != nil {
			return fmt.Errorf("PEXPIRE %s: %w", key, err)
		}
	}
	keyType, err := client.Type(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("TYPE %s: %w", key, err)
	}
	if keyType != "hash" {
		return nil
	}
	fields, err := client.HKeys(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("HKEYS %s: %w", key, err)
	}
	for start := 0; start < len(fields); start += self.settings.RedisFieldCount {
		chunk := fields[start:min(len(fields), start+self.settings.RedisFieldCount)]
		ttls, err := client.HPTTL(ctx, key, chunk...).Result()
		if err != nil {
			return fmt.Errorf("HPTTL %s: %w", key, err)
		}
		for index, fieldTtl := range ttls {
			if fieldTtl <= 0 || len(chunk) <= index {
				continue
			}
			remaining := time.Duration(fieldTtl)*time.Millisecond - timeout
			if remaining <= 0 {
				err = client.HDel(ctx, key, chunk[index]).Err()
			} else {
				err = client.HPExpire(ctx, key, remaining, chunk[index]).Err()
			}
			if err != nil {
				return fmt.Errorf("aging field %s of %s: %w", chunk[index], key, err)
			}
		}
	}
	return nil
}

// Whether a case may run as asked: a case the catalog flags store_clock runs with the store clock, one it does not
// runs without it, and FS-114's never does (QD-264).
func storeClockCaseErr(testCase *fault.Case, storeClockOn bool) error {
	switch {
	case storeClockOn && slices.Contains(storeClockRefusedRowIds, testCase.Row.Id):
		return fmt.Errorf("%s runs on the store's own clock, never through the store clock, which would hide MinIO's own age behaviour (QD-264)", testCase)
	case testCase.StoreClock && !storeClockOn:
		return fmt.Errorf("%s ages stored state across a clock move, which catalog.yml flags store_clock, so it runs with Settings.StoreClock on (QD-264)", testCase)
	case !testCase.StoreClock && storeClockOn:
		return fmt.Errorf("%s runs with the store clock, which catalog.yml does not flag it store_clock for, so its gate report would not say store clock translated: flag the case (QD-264)", testCase)
	}
	return nil
}

// What the store clock found on net that the run cannot pass with: a read it could not translate, or a ttl it could
// not shorten.
func storeClockProblems(net *sidecartest.Net) []string {
	storeClock := StoreClockOf(net)
	if storeClock == nil {
		return nil
	}
	problems := []string{}
	for _, read := range storeClock.Untranslated() {
		problems = append(problems, "the store clock read an object no write it recorded made, so its age is the store's own: "+read)
	}
	for _, err := range storeClock.Errors() {
		problems = append(problems, fmt.Sprintf("the store clock did not age a ttl: %v", err))
	}
	return problems
}

// The store clock's Blobs: every cluster's reads translated and writes recorded.
type storeClockBlobs struct {
	blobs sidecar.BlobStore
	clock *StoreClock
}

var _ sidecar.BlobStore = (*storeClockBlobs)(nil)

func (self *storeClockBlobs) wrap(cluster sidecar.BlobCluster, err error) (sidecar.BlobCluster, error) {
	if err != nil {
		return nil, err
	}
	return &storeClockCluster{cluster: cluster, clock: self.clock}, nil
}

func (self *storeClockBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	return self.wrap(self.blobs.Cluster(clusterId))
}

func (self *storeClockBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.blobs.FirstCluster())
}

func (self *storeClockBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.blobs.SegmentCluster())
}

func (self *storeClockBlobs) MetadataClusterIds() []string {
	return self.blobs.MetadataClusterIds()
}

func (self *storeClockBlobs) NewRepoClusterId() string {
	return self.blobs.NewRepoClusterId()
}

type storeClockCluster struct {
	cluster sidecar.BlobCluster
	clock   *StoreClock
}

var _ sidecar.BlobCluster = (*storeClockCluster)(nil)

func (self *storeClockCluster) ClusterId() string {
	return self.cluster.ClusterId()
}

func (self *storeClockCluster) RequestTimeout() time.Duration {
	return self.cluster.RequestTimeout()
}

func (self *storeClockCluster) SettlePassed() bool {
	return self.cluster.SettlePassed()
}

func (self *storeClockCluster) now() time.Time {
	return self.clock.net.Clock().Now()
}

func (self *storeClockCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	sent := self.now()
	newEtag, err := self.cluster.PutIfNoneMatch(ctx, key, body, options)
	self.clock.put(self.ClusterId(), key, sent, newEtag, err)
	return newEtag, err
}

func (self *storeClockCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	sent := self.now()
	newEtag, err := self.cluster.PutIfMatch(ctx, key, etag, body, options)
	self.clock.put(self.ClusterId(), key, sent, newEtag, err)
	return newEtag, err
}

func (self *storeClockCluster) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	sent := self.now()
	newEtag, err := self.cluster.PutStream(ctx, key, body, byteCount, checksumSha256)
	self.clock.put(self.ClusterId(), key, sent, newEtag, err)
	return newEtag, err
}

func (self *storeClockCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	object, err := self.cluster.Get(ctx, key)
	if err == nil {
		self.clock.translate(self.ClusterId(), &object.BlobInfo)
	}
	return object, err
}

func (self *storeClockCluster) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (*sidecar.BlobObject, error) {
	object, err := self.cluster.GetRange(ctx, key, offset, byteCount)
	if err == nil {
		self.clock.translate(self.ClusterId(), &object.BlobInfo)
	}
	return object, err
}

func (self *storeClockCluster) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	info, err := self.cluster.Head(ctx, key)
	if err == nil {
		self.clock.translate(self.ClusterId(), info)
	}
	return info, err
}

func (self *storeClockCluster) List(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	objects, more, err := self.cluster.List(ctx, keyPrefix, startAfter, limit)
	if err == nil {
		for _, object := range objects {
			self.clock.translate(self.ClusterId(), object)
		}
	}
	return objects, more, err
}

func (self *storeClockCluster) Delete(ctx context.Context, key string) error {
	err := self.cluster.Delete(self.clock.unmodified(ctx, self.ClusterId(), key), key)
	self.clock.deleted(self.ClusterId(), key, err)
	return err
}
