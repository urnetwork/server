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
// (checkPlan), and QuorumLoss and DriveMiss, which need T3's 4-host MinIO layouts (TEST-M2-07), stay
// refused by the base. Wrappers are safe for concurrent use.

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
// it, and T3's clock and cluster faults (TEST-M2-07) are where a real store's would come from.
func checkPlan(plan *sidecartest.FaultPlan) error {
	if plan.Result == sidecartest.Delay {
		return fmt.Errorf("%w: a real store parks no request on a schedule's word (TEST-M2-07)", errPlanNeedsAFake)
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

// server.FileClusters with the Net's plans. Each cluster it hands out is wrapped, and before a request it
// reads the clusters' admin info again whenever the Net's clock has moved since it last did, so a
// schedule's Advance never leaves a write refused on admin info the fake clock made stale.
type faultBlobs struct {
	clusters *server.FileClusters
	net      *sidecartest.Net

	stateLock sync.Mutex
	// the Net's time at the last check
	checkedTime time.Time
}

var _ sidecar.BlobStore = (*faultBlobs)(nil)

func newFaultBlobs(clusters *server.FileClusters, net *sidecartest.Net) *faultBlobs {
	return &faultBlobs{clusters: clusters, net: net}
}

// Rereads admin info once the Net's clock has moved since the last read.
func (self *faultBlobs) fresh(ctx context.Context) {
	now := self.net.Clock().Now()
	stale := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.checkedTime.Equal(now) {
			return false
		}
		self.checkedTime = now
		return true
	}()
	if stale {
		self.clusters.Check(context.WithoutCancel(ctx))
	}
}

func (self *faultBlobs) wrap(cluster sidecar.BlobCluster, err error) (sidecar.BlobCluster, error) {
	if err != nil {
		return nil, err
	}
	return &faultBlobCluster{cluster: cluster, blobs: self}, nil
}

func (self *faultBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	return self.wrap(self.clusters.Cluster(clusterId))
}

func (self *faultBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.clusters.FirstCluster())
}

func (self *faultBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return self.wrap(self.clusters.SegmentCluster())
}

func (self *faultBlobs) MetadataClusterIds() []string {
	return self.clusters.MetadataClusterIds()
}

func (self *faultBlobs) NewRepoClusterId() string {
	return self.clusters.NewRepoClusterId()
}

type faultBlobCluster struct {
	cluster sidecar.BlobCluster
	blobs   *faultBlobs
}

var _ sidecar.BlobCluster = (*faultBlobCluster)(nil)

// One request: its plan, then the real store, then what the plan does with the answer. FakeBlobs'
// request, on a store that keeps its own lock and deadlines.
func (self *faultBlobCluster) request(ctx context.Context, op string, key string, write bool, serve func() error) error {
	sidecartest.StopIfDead(ctx)
	self.blobs.fresh(ctx)
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
	err := serve()
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
	err := self.request(ctx, sidecartest.FaultOpPut, key, true, func() error {
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
	err := self.request(ctx, sidecartest.FaultOpPut, key, true, func() error {
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
	err := self.request(ctx, sidecartest.FaultOpPut, key, true, func() error {
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
func (self *faultBlobCluster) open(ctx context.Context, key string, get func() (*sidecar.BlobObject, error)) (*sidecar.BlobObject, error) {
	var object *sidecar.BlobObject
	err := self.request(ctx, sidecartest.FaultOpGet, key, false, func() error {
		var err error
		object, err = get()
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
	return self.open(ctx, key, func() (*sidecar.BlobObject, error) {
		return self.cluster.Get(ctx, key)
	})
}

func (self *faultBlobCluster) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (*sidecar.BlobObject, error) {
	return self.open(ctx, key, func() (*sidecar.BlobObject, error) {
		return self.cluster.GetRange(ctx, key, offset, byteCount)
	})
}

func (self *faultBlobCluster) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	var info *sidecar.BlobInfo
	err := self.request(ctx, sidecartest.FaultOpGet, key, false, func() error {
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
	err := self.request(ctx, sidecartest.FaultOpList, keyPrefix, false, func() error {
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
	return self.request(ctx, sidecartest.FaultOpDelete, key, true, func() error {
		return self.cluster.Delete(ctx, key)
	})
}

// The calls that only read, by CountingRedis's names: every other call writes, a script included, as on
// FakeRedis.
var redisReadOps = []string{"get", "mget", "hget", "hgetall", "xrange"}

// server's sidecar.Redis with the Net's plans: FakeRedis's call, on the real store. Each script and each
// call naming two or more keys goes to calls too, for RunCrossslot.
type faultRedis struct {
	redis sidecar.Redis
	net   *sidecartest.Net
	calls *CrossslotCalls
}

var _ sidecar.Redis = (*faultRedis)(nil)

func newFaultRedis(redis sidecar.Redis, net *sidecartest.Net, calls *CrossslotCalls) *faultRedis {
	return &faultRedis{redis: redis, net: net, calls: calls}
}

// One call: its plan, then Redis, then what the plan does with the answer.
func (self *faultRedis) call(ctx context.Context, op string, keys []string, serve func() error) error {
	sidecartest.StopIfDead(ctx)
	if err := ctx.Err(); err != nil {
		return err
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
