package sidecart3

// What makes the base's fault plans happen on T3's real stores (§22.8's FaultPlan): each wrapper takes
// its plan from the Net's queue as the fake it stands beside takes one, and gives the result the same
// meaning, so a row's plans read alike on both tiers.
//
//   - put, get, list and delete: faultBlobs, around server.FileClusters. ApplyThenDrop sends the
//     request and then shows the caller a transport error, so a PUT's object is there and its writer
//     holds sidecar.BlobUnknownError, as §11.8's unknown row expects.
//   - redis: faultRedis, around server's sidecar.Redis, which also records each script and multi-key
//     call for RunCrossslot.
//   - pg-commit: armPgCommitFaults, through server.Tx's test hook. Only a schedule actor's commits reach
//     the queue, since server's own background work commits too and would make a commit's place in a
//     count a matter of timing.
//   - http: FaultTransport, a client's http.RoundTripper, so the plan goes to the actor that sent the
//     request.
//
// A real store parks no request on a schedule's word, so a Delay is refused when a schedule arms it
// (checkPlan). QuorumLoss and DriveMiss run on T3's 4-host file-cluster layouts alone (cluster_faults.go),
// and a Net on the local stack's one MinIO process refuses both. Every blob request carries its deadline in
// the store's time, and the store clock, when on, sits beneath these wrappers, so what a plan lets through
// is recorded on the schedule's clock (clock.go). Once the Net's clock has moved past
// Settings.AdminInfoRereadAfter since the last reread, the clusters' admin info is read again once, before the
// first request, and every request arriving meanwhile waits for that reread (clockCatchUp). Wrappers are safe for
// concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

var (
	// What a caller sees when a plan drops a request or its answer on T3: a transport error, as a lost
	// connection gives.
	errTransport = errors.New("connection reset by T3's fault plan")
	// A plan T3's stores cannot carry out.
	errPlanNeedsAFake = errors.New("T3's real stores cannot carry this plan out")
)

// Refuses at Add what a real store cannot do on a schedule's word: park a request (Delay). T2's fakes do
// it, and on a real store a request waits only on the store's own state: a host down, a quorum lost, a key
// held (cluster_faults.go).
func checkPlan(plan *sidecartest.FaultPlan) error {
	if plan.Result == sidecartest.Delay {
		return fmt.Errorf("%w: a real store parks no request on a schedule's word", errPlanNeedsAFake)
	}
	return nil
}

// The result a plan gives, or zero for none.
func planResult(plan *sidecartest.FaultPlan) sidecartest.FaultResult {
	if plan == nil {
		return 0
	}
	return plan.Result
}

// How a transport error reaches a blob client: a write's outcome is unknown, a read just failed. The
// fakes' blobClientError, for the error a plan makes.
func blobTransportError(write bool) error {
	if write {
		return &sidecar.BlobUnknownError{Err: errTransport}
	}
	return fmt.Errorf("blob request failed: %w", errTransport)
}

// What this package's tests see of a Net's catch-ups with its clock (clockCatchUp), from Settings: set before the
// Net opens and never changed after, so nothing guards it.
type catchUpHooks struct {
	// Runs each catch-up's work, by calling work: a test holds one in flight here, or counts them. what names the
	// catch-up: "admin info reread" or "store clock aging". nil runs work alone.
	work func(ctx context.Context, what string, work func())
	// Told when a request finds a catch-up in flight, before it waits for it. nil tells no one.
	waiting func(ctx context.Context, what string)
}

// The work a move of the Net's clock calls for before any request after it goes on to the stores (QD-1111):
// faultBlobs' reread of the clusters' admin info, without which a cluster refuses every write and delete once its
// admin info is CheckStaleAfter old on the Net's clock, and the store clock's aging of the Redis families' ttls
// (clock.go).
//
// The first request to find the clock moved past the last catch-up, by more than moveThreshold, runs the work up to
// that time, and every request that arrives while it is in flight waits for it to finish and then looks at the clock
// again, so no request after a move goes on before the work that move called for has finished, whatever their order.
// One that went on during a reread would be refused on admin info still stale (ErrFileClusterPools, which evs
// answers 503 UNAVAILABLE), and its client's retry would wait on a fake clock no one moves. The work runs once a
// move, with no lock held, and the time is recorded only once it has finished. This is QD-1111's lock held across the
// check, built so that a wait still ends with its request's ctx and a killed actor parks holding nothing.
//
// A move within moveThreshold calls for no work. faultBlobs' rereads take Settings.AdminInfoRereadAfter (QD-1395), so
// a clock following real time in small steps rereads about once a threshold, not before every request after a step;
// the store clock's aging takes none, every move aging the families.
//
// A request waits with the baton given up, as a fake that parks a caller does, and stops waiting when its ctx ends.
// Requests queue behind the work for its real duration, which on a layout under a cluster fault may be a check's
// timeout. Safe for concurrent use.
type clockCatchUp struct {
	net *sidecartest.Net
	// what the hooks and a waiting request's error call this catch-up
	what  string
	hooks *catchUpHooks
	// how far the clock moves past caughtUpTime before the work runs, set before the first catch-up; non-positive:
	// any move
	moveThreshold time.Duration

	stateLock sync.Mutex
	// the Net's time the last finished catch-up ran to
	caughtUpTime time.Time
	// closed when the catch-up in flight ends, nil while none is
	inFlight chan struct{}
}

// A catch-up with nothing to do until the Net's clock moves past caughtUpTime, whose work runs after any move. hooks
// nil tells no test anything.
func newClockCatchUp(net *sidecartest.Net, what string, caughtUpTime time.Time, hooks *catchUpHooks) *clockCatchUp {
	return &clockCatchUp{net: net, what: what, hooks: hooks, caughtUpTime: caughtUpTime}
}

// Runs work from the time the last catch-up ran to up to the Net's now, once the clock has moved past it by more than
// moveThreshold, after waiting for any catch-up in flight. An error is ctx's, which ended while the request waited:
// the request goes no further, and a dead actor's goroutine parks here as it would at a fake.
func (self *clockCatchUp) catchUp(ctx context.Context, work func(fromTime time.Time, toTime time.Time)) error {
	for {
		// read outside the lock, which calls no one: a move after this read is the next request's to catch up, as a
		// move after the lock is released would be
		now := self.net.Clock().Now()
		var fromTime time.Time
		var inFlight chan struct{}
		run := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.inFlight != nil {
				inFlight = self.inFlight
				return false
			}
			if !self.caughtUpTime.Add(max(self.moveThreshold, 0)).Before(now) {
				return false
			}
			fromTime = self.caughtUpTime
			self.inFlight = make(chan struct{})
			return true
		}()
		switch {
		case run:
			self.runWork(ctx, func() {
				work(fromTime, now)
			}, now)
			return nil
		case inFlight == nil:
			return nil
		}
		if self.hooks != nil && self.hooks.waiting != nil {
			self.hooks.waiting(ctx, self.what)
		}
		var err error
		self.net.Barriers().Blocking(func() {
			select {
			case <-inFlight:
			case <-ctx.Done():
				err = ctx.Err()
			}
		})
		if err != nil {
			sidecartest.StopIfDead(ctx)
			return fmt.Errorf("waiting for the %s a move of the Net's clock called for: %w", self.what, err)
		}
		// the clock may have moved again while the work ran
	}
}

// Runs the work of the catch-up this request started, through the hooks, then records toTime and ends the
// catch-up. Work that never returns, as at a test's Fatal, still ends it, recording nothing, so the next request
// runs it again.
func (self *clockCatchUp) runWork(ctx context.Context, work func(), toTime time.Time) {
	finished := false
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if finished {
			self.caughtUpTime = toTime
		}
		close(self.inFlight)
		self.inFlight = nil
	}()
	if self.hooks != nil && self.hooks.work != nil {
		self.hooks.work(ctx, self.what, work)
	} else {
		work()
	}
	finished = true
}

// server.FileClusters with the Net's plans, over blobs, the clusters themselves or the store clock on them.
// Each cluster it hands out is wrapped, and before a request it reads the clusters' admin info again
// whenever the Net's clock has moved past rereadAfter since it last did, every request after such a move waiting
// for that reread (clockCatchUp), so a schedule's Advance never leaves a write refused on admin info the fake clock
// made stale. A move within rereadAfter, which stays below the clusters' CheckStaleAfter, leaves admin info fresh.
type faultBlobs struct {
	blobs sidecar.BlobStore
	// reads every cluster's admin info and sentinel again: server.FileClusters.Check
	check func(ctx context.Context)
	net   *sidecartest.Net
	// QuorumLoss and DriveMiss on a layout, nil on the local stack
	clusterFaults *clusterFaults
	// the reread after each move of the Net's clock past rereadAfter
	rereads *clockCatchUp
}

var _ sidecar.BlobStore = (*faultBlobs)(nil)

// check is the clusters' Check. rereadAfter is Settings.AdminInfoRereadAfter: non-positive rereads after every move.
// hooks nil tells no test anything.
func newFaultBlobs(blobs sidecar.BlobStore, check func(ctx context.Context), net *sidecartest.Net, clusterFaults *clusterFaults, rereadAfter time.Duration, hooks *catchUpHooks) *faultBlobs {
	// the first request reads admin info again whatever the clock says, as the zero time is before every time, past
	// any threshold
	rereads := newClockCatchUp(net, "admin info reread", time.Time{}, hooks)
	rereads.moveThreshold = rereadAfter
	return &faultBlobs{
		blobs:         blobs,
		check:         check,
		net:           net,
		clusterFaults: clusterFaults,
		rereads:       rereads,
	}
}

// Rereads admin info once the Net's clock has moved past rereadAfter since the last reread, or waits for the reread
// in flight. An error is ctx's, which ended while the request waited.
func (self *faultBlobs) fresh(ctx context.Context) error {
	return self.rereads.catchUp(ctx, func(fromTime time.Time, toTime time.Time) {
		self.check(context.WithoutCancel(ctx))
	})
}

func (self *faultBlobs) wrap(cluster sidecar.BlobCluster, err error) (sidecar.BlobCluster, error) {
	if err != nil {
		return nil, err
	}
	return &faultBlobCluster{cluster: cluster, blobs: self}, nil
}

func (self *faultBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	return self.wrap(self.blobs.Cluster(clusterId))
}

func (self *faultBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.blobs.FirstCluster())
}

func (self *faultBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.blobs.SegmentCluster())
}

func (self *faultBlobs) MetadataClusterIds() []string {
	return self.blobs.MetadataClusterIds()
}

func (self *faultBlobs) NewRepoClusterId() string {
	return self.blobs.NewRepoClusterId()
}

type faultBlobCluster struct {
	cluster sidecar.BlobCluster
	blobs   *faultBlobs
}

var _ sidecar.BlobCluster = (*faultBlobCluster)(nil)

// One request: its plan, then the real store, then what the plan does with the answer. FakeBlobs'
// request, on a store that keeps its own lock and deadlines. serve takes the ctx the store gets: the
// caller's, with its deadline header in the store's time.
func (self *faultBlobCluster) request(ctx context.Context, op string, key string, write bool, serve func(ctx context.Context) error) error {
	sidecartest.StopIfDead(ctx)
	if err := self.blobs.fresh(ctx); err != nil {
		// sent nothing and took no plan
		return err
	}
	plan := self.blobs.net.Faults().Take(ctx, op, self.cluster.ClusterId(), key, write)
	result := planResult(plan)
	switch result {
	case sidecartest.FailBeforeApply:
		return blobTransportError(write)
	case sidecartest.Precondition:
		return sidecar.ErrBlobPrecondition
	case sidecartest.Held:
		return sidecar.ErrBlobHeld
	case sidecartest.CrashBeforeApply:
		sidecartest.CrashCaller(ctx)
	}
	net := self.blobs.net
	storeCtx := server.WithFileClusterStoreTime(ctx, func(deadline time.Time) time.Time {
		return StoreTime(net, deadline)
	})
	var err error
	switch result {
	case sidecartest.QuorumLoss, sidecartest.DriveMiss:
		// admitted only on a layout (admitClusterFault), so clusterFaults is there
		err = self.blobs.clusterFaults.carryOut(ctx, plan, self.cluster, key, write, func() error {
			return serve(storeCtx)
		})
	default:
		err = serve(storeCtx)
	}
	switch result {
	case sidecartest.ApplyThenDrop:
		return blobTransportError(write)
	case sidecartest.CrashAfterApply:
		sidecartest.CrashCaller(ctx)
	}
	return err
}

func (self *faultBlobCluster) ClusterId() string {
	return self.cluster.ClusterId()
}

func (self *faultBlobCluster) RequestTimeout() time.Duration {
	return self.cluster.RequestTimeout()
}

func (self *faultBlobCluster) SettlePassed() bool {
	return self.cluster.SettlePassed()
}

func (self *faultBlobCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	var newEtag string
	err := self.request(ctx, sidecartest.FaultOpPut, key, true, func(ctx context.Context) error {
		var err error
		newEtag, err = self.cluster.PutIfNoneMatch(ctx, key, body, options)
		return err
	})
	if err != nil {
		return "", err
	}
	return newEtag, nil
}

func (self *faultBlobCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	var newEtag string
	err := self.request(ctx, sidecartest.FaultOpPut, key, true, func(ctx context.Context) error {
		var err error
		newEtag, err = self.cluster.PutIfMatch(ctx, key, etag, body, options)
		return err
	})
	if err != nil {
		return "", err
	}
	return newEtag, nil
}

func (self *faultBlobCluster) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	var newEtag string
	err := self.request(ctx, sidecartest.FaultOpPut, key, true, func(ctx context.Context) error {
		var err error
		newEtag, err = self.cluster.PutStream(ctx, key, body, byteCount, checksumSha256)
		return err
	})
	if err != nil {
		return "", err
	}
	return newEtag, nil
}

// An object whose answer a plan dropped is closed here, since its caller never sees it.
func (self *faultBlobCluster) open(ctx context.Context, key string, get func(ctx context.Context) (*sidecar.BlobObject, error)) (*sidecar.BlobObject, error) {
	var object *sidecar.BlobObject
	err := self.request(ctx, sidecartest.FaultOpGet, key, false, func(ctx context.Context) error {
		var err error
		object, err = get(ctx)
		return err
	})
	if err != nil {
		if object != nil && object.Body != nil {
			object.Body.Close()
		}
		return nil, err
	}
	return object, nil
}

func (self *faultBlobCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	return self.open(ctx, key, func(ctx context.Context) (*sidecar.BlobObject, error) {
		return self.cluster.Get(ctx, key)
	})
}

func (self *faultBlobCluster) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (*sidecar.BlobObject, error) {
	return self.open(ctx, key, func(ctx context.Context) (*sidecar.BlobObject, error) {
		return self.cluster.GetRange(ctx, key, offset, byteCount)
	})
}

func (self *faultBlobCluster) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	var info *sidecar.BlobInfo
	err := self.request(ctx, sidecartest.FaultOpGet, key, false, func(ctx context.Context) error {
		var err error
		info, err = self.cluster.Head(ctx, key)
		return err
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (self *faultBlobCluster) List(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	var objects []*sidecar.BlobInfo
	var more bool
	err := self.request(ctx, sidecartest.FaultOpList, keyPrefix, false, func(ctx context.Context) error {
		var err error
		objects, more, err = self.cluster.List(ctx, keyPrefix, startAfter, limit)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return objects, more, nil
}

func (self *faultBlobCluster) Delete(ctx context.Context, key string) error {
	return self.request(ctx, sidecartest.FaultOpDelete, key, true, func(ctx context.Context) error {
		return self.cluster.Delete(ctx, key)
	})
}

// The calls that only read, by CountingRedis's names: every other call writes, a script included, as on
// FakeRedis.
var redisReadOps = []string{"get", "mget", "hget", "hgetall", "xrange"}

// server's sidecar.Redis with the Net's plans: FakeRedis's call, on the real store. Each script and each
// call naming two or more keys goes to calls too, for RunCrossslot. With the store clock on, a call first
// ages its families by however far the Net's clock has moved (clock.go).
type faultRedis struct {
	redis sidecar.Redis
	net   *sidecartest.Net
	calls *CrossslotCalls
	// nil when off
	storeClock *StoreClock

	writtenLock sync.Mutex
	// every key a call that writes named, which is the sidecar's Redis on a database server's platform shares
	writtenKeys map[string]bool
}

// Every key a write through the Net's host named, sorted: the keys of the sidecar's families on T3, apart from
// those server's exchange and controller keep beside them in the test's database.
func (self *faultRedis) WrittenKeys() []string {
	self.writtenLock.Lock()
	defer self.writtenLock.Unlock()
	keys := []string{}
	for key := range self.writtenKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

var _ sidecar.Redis = (*faultRedis)(nil)

func newFaultRedis(redis sidecar.Redis, net *sidecartest.Net, calls *CrossslotCalls, storeClock *StoreClock) *faultRedis {
	return &faultRedis{redis: redis, net: net, calls: calls, storeClock: storeClock}
}

// One call: its plan, then Redis, then what the plan does with the answer.
func (self *faultRedis) call(ctx context.Context, op string, keys []string, serve func() error) error {
	sidecartest.StopIfDead(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if self.storeClock != nil {
		// a failure to age is the store clock's to report: the call itself goes on, unless its ctx ended, as a wait
		// for an aging in flight may end
		self.storeClock.age(ctx)
		if err := ctx.Err(); err != nil {
			sidecartest.StopIfDead(ctx)
			return err
		}
	}
	if !slices.Contains(redisReadOps, op) {
		func() {
			self.writtenLock.Lock()
			defer self.writtenLock.Unlock()
			if self.writtenKeys == nil {
				self.writtenKeys = map[string]bool{}
			}
			for _, key := range keys {
				self.writtenKeys[key] = true
			}
		}()
	}
	faultKey := ""
	if 0 < len(keys) {
		faultKey = keys[0]
	}
	plan := self.net.Faults().Take(ctx, sidecartest.FaultOpRedis, "", faultKey, !slices.Contains(redisReadOps, op))
	result := planResult(plan)
	switch result {
	case sidecartest.FailBeforeApply:
		return errTransport
	case sidecartest.CrashBeforeApply:
		sidecartest.CrashCaller(ctx)
	}
	err := serve()
	switch result {
	case sidecartest.ApplyThenDrop:
		return errTransport
	case sidecartest.CrashAfterApply:
		sidecartest.CrashCaller(ctx)
	}
	return err
}

func (self *faultRedis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var value []byte
	var found bool
	err := self.call(ctx, "get", []string{key}, func() error {
		var err error
		value, found, err = self.redis.Get(ctx, key)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return value, found, nil
}

func (self *faultRedis) GetMany(ctx context.Context, keys []string) ([][]byte, error) {
	self.calls.addKeys("mget", keys)
	var values [][]byte
	err := self.call(ctx, "mget", keys, func() error {
		var err error
		values, err = self.redis.GetMany(ctx, keys)
		return err
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

func (self *faultRedis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return self.call(ctx, "set", []string{key}, func() error {
		return self.redis.Set(ctx, key, value, ttl)
	})
}

func (self *faultRedis) SetIfAbsent(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	var set bool
	err := self.call(ctx, "setnx", []string{key}, func() error {
		var err error
		set, err = self.redis.SetIfAbsent(ctx, key, value, ttl)
		return err
	})
	if err != nil {
		return false, err
	}
	return set, nil
}

func (self *faultRedis) CompareAndSet(ctx context.Context, key string, expected []byte, value []byte, ttl time.Duration) (bool, error) {
	var set bool
	err := self.call(ctx, "cas", []string{key}, func() error {
		var err error
		set, err = self.redis.CompareAndSet(ctx, key, expected, value, ttl)
		return err
	})
	if err != nil {
		return false, err
	}
	return set, nil
}

func (self *faultRedis) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	var deleted bool
	err := self.call(ctx, "cad", []string{key}, func() error {
		var err error
		deleted, err = self.redis.CompareAndDelete(ctx, key, expected)
		return err
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

func (self *faultRedis) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	var value int64
	err := self.call(ctx, "incr", []string{key}, func() error {
		var err error
		value, err = self.redis.Increment(ctx, key, ttl)
		return err
	})
	if err != nil {
		return 0, err
	}
	return value, nil
}

func (self *faultRedis) Delete(ctx context.Context, keys []string) error {
	self.calls.addKeys("del", keys)
	return self.call(ctx, "del", keys, func() error {
		return self.redis.Delete(ctx, keys)
	})
}

func (self *faultRedis) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	var set bool
	err := self.call(ctx, "expire", []string{key}, func() error {
		var err error
		set, err = self.redis.Expire(ctx, key, ttl)
		return err
	})
	if err != nil {
		return false, err
	}
	return set, nil
}

func (self *faultRedis) HashGet(ctx context.Context, key string, field string) ([]byte, bool, error) {
	var value []byte
	var found bool
	err := self.call(ctx, "hget", []string{key}, func() error {
		var err error
		value, found, err = self.redis.HashGet(ctx, key, field)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return value, found, nil
}

func (self *faultRedis) HashGetAll(ctx context.Context, key string) (map[string][]byte, error) {
	var fieldValues map[string][]byte
	err := self.call(ctx, "hgetall", []string{key}, func() error {
		var err error
		fieldValues, err = self.redis.HashGetAll(ctx, key)
		return err
	})
	if err != nil {
		return nil, err
	}
	return fieldValues, nil
}

func (self *faultRedis) HashSet(ctx context.Context, key string, fieldValues map[string][]byte) error {
	return self.call(ctx, "hset", []string{key}, func() error {
		return self.redis.HashSet(ctx, key, fieldValues)
	})
}

func (self *faultRedis) HashDelete(ctx context.Context, key string, fields []string) error {
	return self.call(ctx, "hdel", []string{key}, func() error {
		return self.redis.HashDelete(ctx, key, fields)
	})
}

func (self *faultRedis) HashExpire(ctx context.Context, key string, fields []string, ttl time.Duration) error {
	return self.call(ctx, "hexpire", []string{key}, func() error {
		return self.redis.HashExpire(ctx, key, fields, ttl)
	})
}

func (self *faultRedis) StreamAdd(ctx context.Context, key string, maxLength int64, fieldValues map[string][]byte) (string, error) {
	var entryId string
	err := self.call(ctx, "xadd", []string{key}, func() error {
		var err error
		entryId, err = self.redis.StreamAdd(ctx, key, maxLength, fieldValues)
		return err
	})
	if err != nil {
		return "", err
	}
	return entryId, nil
}

func (self *faultRedis) StreamRange(ctx context.Context, key string, afterEntryId string, count int64) ([]*sidecar.RedisStreamEntry, error) {
	var entries []*sidecar.RedisStreamEntry
	err := self.call(ctx, "xrange", []string{key}, func() error {
		var err error
		entries, err = self.redis.StreamRange(ctx, key, afterEntryId, count)
		return err
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (self *faultRedis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	return self.call(ctx, "xgroup", []string{key}, func() error {
		return self.redis.StreamGroupCreate(ctx, key, group)
	})
}

func (self *faultRedis) StreamGroupRead(ctx context.Context, key string, group string, consumer string, count int64, block time.Duration) ([]*sidecar.RedisStreamEntry, error) {
	var entries []*sidecar.RedisStreamEntry
	err := self.call(ctx, "xreadgroup", []string{key}, func() error {
		var err error
		entries, err = self.redis.StreamGroupRead(ctx, key, group, consumer, count, block)
		return err
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (self *faultRedis) StreamGroupAck(ctx context.Context, key string, group string, entryIds []string) error {
	return self.call(ctx, "xack", []string{key}, func() error {
		return self.redis.StreamGroupAck(ctx, key, group, entryIds)
	})
}

func (self *faultRedis) StreamGroupClaim(ctx context.Context, key string, group string, consumer string, minIdle time.Duration, cursor string, count int64) ([]*sidecar.RedisStreamEntry, string, error) {
	var entries []*sidecar.RedisStreamEntry
	var nextCursor string
	err := self.call(ctx, "xautoclaim", []string{key}, func() error {
		var err error
		entries, nextCursor, err = self.redis.StreamGroupClaim(ctx, key, group, consumer, minIdle, cursor, count)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return entries, nextCursor, nil
}

func (self *faultRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	name := ""
	if script != nil {
		name = script.Name
	}
	self.calls.addScript(script, keys, args)
	var reply *sidecar.RedisReply
	err := self.call(ctx, "script:"+name, keys, func() error {
		var err error
		reply, err = self.redis.RunScript(ctx, script, keys, args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// Registering is no call to Redis, so no plan reaches it, as on FakeRedis.
func (self *faultRedis) AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (func(), error) {
	return self.redis.AddKeyEventCallback(keyPattern, callback)
}

// Arms §22.8's pg-commit plans for every commit a schedule actor's transaction makes until stop or the
// end of t. A commit outside any actor takes no plan and commits as it would, since server's own
// background work commits too and would make a count of commits a matter of timing. The key a plan
// matches is empty: T3 names commits by their actor and their order, not by a table.
func armPgCommitFaults(t testing.TB, net *sidecartest.Net) (stop func()) {
	t.Helper()
	return server.SetPgCommitFaultForTest(t, func(ctx context.Context, commit func() error, rollback func()) error {
		if sidecartest.ActorFromContext(ctx) == nil {
			return commit()
		}
		sidecartest.StopIfDead(ctx)
		plan := net.Faults().Take(ctx, sidecartest.FaultOpPgCommit, "", "", true)
		switch planResult(plan) {
		case sidecartest.FailBeforeApply:
			rollback()
			return errTransport
		case sidecartest.CrashBeforeApply:
			rollback()
			sidecartest.CrashCaller(ctx)
		case sidecartest.ApplyThenDrop:
			if err := commit(); err != nil {
				return err
			}
			return errTransport
		case sidecartest.CrashAfterApply:
			commit()
			sidecartest.CrashCaller(ctx)
		}
		return commit()
	})
}

// §22.8's http fault on T3: a client's transport to server's api, taking an `http` plan by the request's
// path from the Net's queue, on the actor that sent it. FailBeforeApply sends nothing; ApplyThenDrop
// sends the request, reads its answer whole and loses it; CrashBeforeApply and CrashAfterApply end the
// sending actor on either side. base nil is http.DefaultTransport.
func FaultTransport(net *sidecartest.Net, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &faultTransport{net: net, base: base}
}

type faultTransport struct {
	net  *sidecartest.Net
	base http.RoundTripper
}

func (self *faultTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	sidecartest.StopIfDead(ctx)
	write := request.Method != http.MethodGet && request.Method != http.MethodHead
	plan := self.net.Faults().Take(ctx, sidecartest.FaultOpHttp, "", request.URL.Path, write)
	result := planResult(plan)
	switch result {
	case sidecartest.FailBeforeApply:
		if request.Body != nil {
			request.Body.Close()
		}
		return nil, errTransport
	case sidecartest.CrashBeforeApply:
		if request.Body != nil {
			request.Body.Close()
		}
		sidecartest.CrashCaller(ctx)
	}
	response, err := self.base.RoundTrip(request)
	switch result {
	case sidecartest.ApplyThenDrop:
		if err == nil {
			// the handler has run whole once its answer has been read
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		return nil, errTransport
	case sidecartest.CrashAfterApply:
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		sidecartest.CrashCaller(ctx)
	}
	return response, err
}
