// The provider liveness lease cache (provider_liveness_lease_read.go) on a
// fake of the lease's Redis and the fake clock of testing/synctest: a read
// error serves the last verdict up to its max age and fails open past it, a
// failed shard is the failing request's alone, verdicts in use are read again
// in the background one refresh at a time, and the cache stays bounded. No
// database or Redis.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// The lease's Redis for the cache's tests: every shard's newest stamp, each
// client's stamp, the shards that fail, and each read it was asked.
type livenessTestFakeLeases struct {
	stateLock    sync.Mutex
	newestMillis int64
	stampMillis  map[server.Id]int64
	failShards   map[int]bool
	failReads    bool
	// while set, a read waits for it to close
	hold  chan struct{}
	reads []livenessTestFakeRead
}

// One read asked of the fake: its budget and its clients, in byte order.
type livenessTestFakeRead struct {
	timeout   time.Duration
	clientIds []server.Id
}

// A fake whose shards are all current now.
func newLivenessTestFakeLeases() *livenessTestFakeLeases {
	return &livenessTestFakeLeases{
		newestMillis: time.Now().UnixMilli(),
		stampMillis:  map[server.Id]int64{},
		failShards:   map[int]bool{},
	}
}

// Leases the clients a second before the shards' newest stamp.
func (self *livenessTestFakeLeases) lease(clientIds ...server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, clientId := range clientIds {
		self.stampMillis[clientId] = self.newestMillis - 1000
	}
}

// Removes the clients' leases.
func (self *livenessTestFakeLeases) release(clientIds ...server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, clientId := range clientIds {
		delete(self.stampMillis, clientId)
	}
}

func (self *livenessTestFakeLeases) setFailShard(shard int, fail bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failShards[shard] = fail
}

func (self *livenessTestFakeLeases) setFailReads(fail bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failReads = fail
}

// Holds the reads from now until the returned release.
func (self *livenessTestFakeLeases) holdReads() func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hold := make(chan struct{})
	self.hold = hold
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.hold = nil
		close(hold)
	}
}

// Every read asked so far.
func (self *livenessTestFakeLeases) asked() []livenessTestFakeRead {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.reads)
}

// The read of the lease cache (readProviderLivenessShards), over the fake.
func (self *livenessTestFakeLeases) read(ctx context.Context, timeout time.Duration, shardClientIds map[int][]server.Id) (map[int]providerLivenessShardRead, error) {
	clientIds := []server.Id{}
	for _, shardIds := range shardClientIds {
		clientIds = append(clientIds, shardIds...)
	}
	slices.SortFunc(clientIds, func(a server.Id, b server.Id) int {
		return bytes.Compare(a[:], b[:])
	})
	self.stateLock.Lock()
	self.reads = append(self.reads, livenessTestFakeRead{timeout: timeout, clientIds: clientIds})
	hold := self.hold
	self.stateLock.Unlock()
	if hold != nil {
		<-hold
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failReads {
		return nil, errors.New("test: the read ran past its budget")
	}
	reads := map[int]providerLivenessShardRead{}
	for shard, shardIds := range shardClientIds {
		if self.failShards[shard] {
			reads[shard] = providerLivenessShardRead{failed: true}
			continue
		}
		stampMillis := make([]int64, len(shardIds))
		for i, clientId := range shardIds {
			stampMillis[i] = self.stampMillis[clientId]
		}
		reads[shard] = providerLivenessShardRead{newestMillis: self.newestMillis, stampMillis: stampMillis}
	}
	return reads, nil
}

// A reader of the fake whose writer coverage is always full.
func newLivenessTestFakeReader(leases *livenessTestFakeLeases) *providerLivenessReader {
	handlerId := server.NewId()
	return &providerLivenessReader{
		coverage: &providerLivenessCoverage{
			handlers: &providerLivenessHandlers{
				load: func(context.Context, time.Time) ([]server.Id, error) {
					return []server.Id{handlerId}, nil
				},
				now: time.Now,
			},
			readStamps: func(context.Context, []string) (int64, []int64, error) {
				nowMillis := server.NowUtc().UnixMilli()
				return nowMillis, []int64{nowMillis}, nil
			},
			now: time.Now,
		},
		leases: newProviderLivenessLeaseCache(leases.read, time.Now, nil),
	}
}

// Each client's verdict from one read of the reader at the clock's present,
// once the background work it started has ended.
func livenessTestFakeJudge(t *testing.T, reader *providerLivenessReader, request *providerLivenessRequest, clientIds ...server.Id) map[server.Id]string {
	t.Helper()
	offline, unknown, err := reader.exclusions(context.Background(), request, clientIds, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	return livenessTestVerdicts(clientIds, offline, unknown)
}

// Sleeps the fake clock to start plus at.
func livenessTestSleepUntil(start time.Time, at time.Duration) {
	time.Sleep(start.Add(at).Sub(time.Now()))
}

// The counts of the cache's verdict sources and of two fail-open reasons.
type livenessTestCacheCounts struct {
	hit, stale, read, failOpen, shardError, readBackoff float64
}

func livenessTestCacheCount(t *testing.T) livenessTestCacheCounts {
	t.Helper()
	return livenessTestCacheCounts{
		hit:         livenessTestMetric(t, findProviders2LivenessCacheHits),
		stale:       livenessTestMetric(t, findProviders2LivenessCacheStales),
		read:        livenessTestMetric(t, findProviders2LivenessCacheReads),
		failOpen:    livenessTestMetric(t, findProviders2LivenessCacheFailOpens),
		shardError:  livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenShard)),
		readBackoff: livenessTestMetric(t, findProviders2LivenessFailOpens.WithLabelValues(providerLivenessFailOpenReadBackoff)),
	}
}

// Fails unless the counts grew from before by want.
func assertLivenessTestCacheCounts(t *testing.T, label string, before livenessTestCacheCounts, want livenessTestCacheCounts) {
	t.Helper()
	after := livenessTestCacheCount(t)
	got := livenessTestCacheCounts{
		hit:         after.hit - before.hit,
		stale:       after.stale - before.stale,
		read:        after.read - before.read,
		failOpen:    after.failOpen - before.failOpen,
		shardError:  after.shardError - before.shardError,
		readBackoff: after.readBackoff - before.readBackoff,
	}
	if got != want {
		t.Fatalf("%s: counted %+v, want %+v", label, got, want)
	}
}

// Fails unless the read asked is a request's or a refresh's, by its budget, of
// exactly the clients.
func assertLivenessTestFakeRead(t *testing.T, label string, read livenessTestFakeRead, timeout time.Duration, clientIds ...server.Id) {
	t.Helper()
	slices.SortFunc(clientIds, func(a server.Id, b server.Id) int {
		return bytes.Compare(a[:], b[:])
	})
	if read.timeout != timeout || !slices.Equal(read.clientIds, clientIds) {
		t.Fatalf("%s: read %d clients within %s, want %d within %s", label, len(read.clientIds), read.timeout, len(clientIds), timeout)
	}
}

// A shard read error serves each candidate's last verdict while it is at most
// providerLivenessCacheMaxAge old, to the nanosecond, counted stale, and fails
// the candidate open past it, counted failopen and shard_error once. A verdict
// in memory whose background refresh failed is served stale as well, and
// neither changes a verdict.
func TestProviderLivenessCacheServesTheLastVerdictUpToItsMaxAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leases := newLivenessTestFakeLeases()
		reader := newLivenessTestFakeReader(leases)
		live := livenessTestIdInShard(3, 0)
		leaseless := livenessTestIdInShard(3, 1)
		leases.lease(live)
		enforced := map[server.Id]string{live: providerLivenessLive, leaseless: providerLivenessOffline}
		failedOpen := map[server.Id]string{live: providerLivenessUnknown, leaseless: providerLivenessUnknown}
		start := time.Now()
		judgeAt := func(label string, at time.Duration, want map[server.Id]string) {
			t.Helper()
			livenessTestSleepUntil(start, at)
			if got := livenessTestFakeJudge(t, reader, nil, live, leaseless); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
		}

		judgeAt("read", 0, enforced)
		leases.setFailReads(true)
		before := livenessTestCacheCount(t)
		judgeAt("in memory, refreshed in the background", 1500*time.Millisecond, enforced)
		assertLivenessTestCacheCounts(t, "in memory", before, livenessTestCacheCounts{hit: 2})
		asked := leases.asked()
		if len(asked) != 2 {
			t.Fatalf("asked %d reads, want the request's and one refresh", len(asked))
		}
		assertLivenessTestFakeRead(t, "the refresh", asked[1], providerLivenessRefreshReadTimeout, live, leaseless)

		before = livenessTestCacheCount(t)
		judgeAt("in memory, its refresh failed", 1600*time.Millisecond, enforced)
		assertLivenessTestCacheCounts(t, "in memory, refresh failed", before, livenessTestCacheCounts{stale: 2})

		before = livenessTestCacheCount(t)
		judgeAt("past the fresh age, the request's read failing", providerLivenessCacheFreshAge+time.Nanosecond, enforced)
		judgeAt("at the max age", providerLivenessCacheMaxAge, enforced)
		assertLivenessTestCacheCounts(t, "the last verdicts", before, livenessTestCacheCounts{stale: 4})

		before = livenessTestCacheCount(t)
		judgeAt("past the max age", providerLivenessCacheMaxAge+time.Nanosecond, failedOpen)
		assertLivenessTestCacheCounts(t, "past the max age", before, livenessTestCacheCounts{failOpen: 2, shardError: 1})

		leases.setFailReads(false)
		before = livenessTestCacheCount(t)
		judgeAt("readable again", providerLivenessCacheMaxAge+time.Second, enforced)
		assertLivenessTestCacheCounts(t, "readable again", before, livenessTestCacheCounts{read: 2})
		for _, read := range leases.asked() {
			if read.timeout != ProviderLivenessReadTimeout && read.timeout != providerLivenessRefreshReadTimeout {
				t.Fatalf("a read within %s, want a request's %s or a refresh's %s", read.timeout, ProviderLivenessReadTimeout, providerLivenessRefreshReadTimeout)
			}
		}
	})
}

// A failed shard is the failing request's alone. That request's later reads
// ask nothing of the shard, admitting its candidates without a verdict, counted
// read_backoff, while they still read other shards. Every other request reads
// the shard, failing or not, and once one has, the backed-off request takes
// the shard's verdicts from memory.
func TestProviderLivenessCacheBackoffStaysWithItsRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leases := newLivenessTestFakeLeases()
		reader := newLivenessTestFakeReader(leases)
		live := livenessTestIdInShard(3, 0)
		leaseless := livenessTestIdInShard(3, 1)
		laterLeaseless := livenessTestIdInShard(3, 2)
		other := livenessTestIdInShard(4, 0)
		laterOther := livenessTestIdInShard(4, 1)
		leases.lease(live, other)
		failing := newProviderLivenessRequest()
		check := func(label string, request *providerLivenessRequest, want map[server.Id]string, wantReads int) {
			t.Helper()
			clientIds := slices.Collect(maps.Keys(want))
			if got := livenessTestFakeJudge(t, reader, request, clientIds...); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
			if asked := leases.asked(); len(asked) != wantReads {
				t.Fatalf("%s: asked %d reads, want %d", label, len(asked), wantReads)
			}
		}

		leases.setFailShard(3, true)
		before := livenessTestCacheCount(t)
		check("the failing read", failing, map[server.Id]string{
			live:      providerLivenessUnknown,
			leaseless: providerLivenessUnknown,
			other:     providerLivenessLive,
		}, 1)
		assertLivenessTestCacheCounts(t, "the failing read", before, livenessTestCacheCounts{read: 1, failOpen: 2, shardError: 1})

		before = livenessTestCacheCount(t)
		check("the failing request's later read", failing, map[server.Id]string{
			laterLeaseless: providerLivenessUnknown,
			laterOther:     providerLivenessOffline,
		}, 2)
		assertLivenessTestFakeRead(t, "the later read", leases.asked()[1], ProviderLivenessReadTimeout, laterOther)
		assertLivenessTestCacheCounts(t, "the later read", before, livenessTestCacheCounts{read: 1, failOpen: 1, readBackoff: 1})

		check("another request, the shard still failing", newProviderLivenessRequest(), map[server.Id]string{
			live:      providerLivenessUnknown,
			leaseless: providerLivenessUnknown,
		}, 3)
		leases.setFailShard(3, false)
		check("another request, the shard healthy", newProviderLivenessRequest(), map[server.Id]string{
			live:      providerLivenessLive,
			leaseless: providerLivenessOffline,
		}, 4)
		check("the failing request, from memory", failing, map[server.Id]string{
			live:      providerLivenessLive,
			leaseless: providerLivenessOffline,
		}, 4)
	})
}

// A verdict a request uses past the refresh age is read again in the
// background, within the refresh's budget, while the request goes on with the
// verdict in memory, and the refresh's verdict serves the next request. A
// verdict past the fresh age is read by the request itself.
func TestProviderLivenessCacheRefreshesInTheBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leases := newLivenessTestFakeLeases()
		reader := newLivenessTestFakeReader(leases)
		leaving := livenessTestIdInShard(3, 0)
		atFreshAge := livenessTestIdInShard(5, 0)
		pastFreshAge := livenessTestIdInShard(6, 0)
		leases.lease(leaving, atFreshAge, pastFreshAge)
		start := time.Now()
		check := func(label string, at time.Duration, want map[server.Id]string, wantReads int) {
			t.Helper()
			livenessTestSleepUntil(start, at)
			clientIds := slices.Collect(maps.Keys(want))
			if got := livenessTestFakeJudge(t, reader, nil, clientIds...); !maps.Equal(got, want) {
				t.Fatalf("%s: verdicts %v, want %v", label, got, want)
			}
			if asked := leases.asked(); len(asked) != wantReads {
				t.Fatalf("%s: asked %d reads, want %d", label, len(asked), wantReads)
			}
		}

		check("read", 0, map[server.Id]string{
			leaving:      providerLivenessLive,
			atFreshAge:   providerLivenessLive,
			pastFreshAge: providerLivenessLive,
		}, 1)
		assertLivenessTestFakeRead(t, "the request's read", leases.asked()[0], ProviderLivenessReadTimeout, leaving, atFreshAge, pastFreshAge)
		check("at the refresh age", providerLivenessCacheRefreshAge, map[server.Id]string{leaving: providerLivenessLive}, 1)

		// the provider goes away: the verdict in memory serves the request
		// that starts the refresh, and the refresh's the next
		leases.release(leaving)
		before := livenessTestCacheCount(t)
		check("past the refresh age", providerLivenessCacheRefreshAge+time.Nanosecond, map[server.Id]string{leaving: providerLivenessLive}, 2)
		assertLivenessTestFakeRead(t, "the refresh", leases.asked()[1], providerLivenessRefreshReadTimeout, leaving)
		check("after the refresh", providerLivenessCacheRefreshAge+2*time.Nanosecond, map[server.Id]string{leaving: providerLivenessOffline}, 2)
		assertLivenessTestCacheCounts(t, "from memory", before, livenessTestCacheCounts{hit: 2})

		// at the fresh age a verdict still serves from memory, and is
		// refreshed; past it the request reads the candidate itself
		before = livenessTestCacheCount(t)
		check("at the fresh age", providerLivenessCacheFreshAge, map[server.Id]string{atFreshAge: providerLivenessLive}, 3)
		assertLivenessTestFakeRead(t, "the refresh at the fresh age", leases.asked()[2], providerLivenessRefreshReadTimeout, atFreshAge)
		assertLivenessTestCacheCounts(t, "at the fresh age", before, livenessTestCacheCounts{hit: 1})
		before = livenessTestCacheCount(t)
		check("past the fresh age", providerLivenessCacheFreshAge+time.Nanosecond, map[server.Id]string{pastFreshAge: providerLivenessLive}, 4)
		assertLivenessTestFakeRead(t, "the request's read past the fresh age", leases.asked()[3], ProviderLivenessReadTimeout, pastFreshAge)
		assertLivenessTestCacheCounts(t, "past the fresh age", before, livenessTestCacheCounts{read: 1})
	})
}

// One refresh runs at a time, and at most one starts per refresh age: verdicts
// queued while a refresh runs wait for it, then go in the next one, which
// starts a refresh age after the last began.
func TestProviderLivenessCacheRefreshesOneAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leases := newLivenessTestFakeLeases()
		reader := newLivenessTestFakeReader(leases)
		a := livenessTestIdInShard(3, 0)
		b := livenessTestIdInShard(4, 0)
		c := livenessTestIdInShard(5, 0)
		leases.lease(a, b, c)
		start := time.Now()
		judgeAt := func(label string, at time.Duration, wantReads int, clientIds ...server.Id) {
			t.Helper()
			livenessTestSleepUntil(start, at)
			got := livenessTestFakeJudge(t, reader, nil, clientIds...)
			for _, clientId := range clientIds {
				if got[clientId] != providerLivenessLive {
					t.Fatalf("%s: verdicts %v, want every one live", label, got)
				}
			}
			if asked := leases.asked(); len(asked) != wantReads {
				t.Fatalf("%s: asked %d reads, want %d", label, len(asked), wantReads)
			}
		}

		judgeAt("read", 0, 1, a, b, c)
		release := leases.holdReads()
		judgeAt("a refresh starts, and is held", 1100*time.Millisecond, 2, a)
		assertLivenessTestFakeRead(t, "the held refresh", leases.asked()[1], providerLivenessRefreshReadTimeout, a)
		judgeAt("queued while it runs", 1200*time.Millisecond, 2, b)
		judgeAt("queued a refresh age later, while it runs", 2200*time.Millisecond, 2, c)
		release()
		synctest.Wait()
		judgeAt("the next refresh", 2300*time.Millisecond, 3, b)
		assertLivenessTestFakeRead(t, "the next refresh", leases.asked()[2], providerLivenessRefreshReadTimeout, b, c)
		judgeAt("queued within the refresh age of the last", 2400*time.Millisecond, 3, a)
		judgeAt("a refresh age after the last began", 3300*time.Millisecond, 4, a)
		assertLivenessTestFakeRead(t, "the last refresh", leases.asked()[3], providerLivenessRefreshReadTimeout, a)
	})
}

// The cache holds at most providerLivenessCacheMaxClients verdicts: full, it
// evicts arbitrary ones for new reads (which may be new reads too), and once
// per max age it sweeps every verdict past the max age, which no read uses.
func TestProviderLivenessCacheIsBoundedAndSwept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leases := newLivenessTestFakeLeases()
		cache := newProviderLivenessLeaseCache(leases.read, time.Now, nil)
		size := func() int {
			cache.stateLock.Lock()
			defer cache.stateLock.Unlock()
			return len(cache.entries)
		}
		lookup := func(clientIds []server.Id) {
			t.Helper()
			shardClientIds := map[int][]server.Id{}
			for _, clientId := range clientIds {
				shard := providerLivenessLeaseShard(clientId)
				shardClientIds[shard] = append(shardClientIds[shard], clientId)
			}
			if _, err := cache.lookup(context.Background(), newProviderLivenessRequest(), shardClientIds); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		clientIds := make([]server.Id, providerLivenessCacheMaxClients+100)
		for i := range clientIds {
			clientIds[i][0] = 0x03
			binary.BigEndian.PutUint64(clientIds[i][8:], uint64(i))
		}

		lookup(clientIds[:providerLivenessCacheMaxClients])
		if n := size(); n != providerLivenessCacheMaxClients {
			t.Fatalf("holds %d verdicts after reading as many as the bound, want %d", n, providerLivenessCacheMaxClients)
		}
		lookup(clientIds[providerLivenessCacheMaxClients:])
		if n := size(); n != providerLivenessCacheMaxClients {
			t.Fatalf("holds %d verdicts past the bound, want %d", n, providerLivenessCacheMaxClients)
		}

		time.Sleep(providerLivenessCacheMaxAge + time.Nanosecond)
		lookup([]server.Id{livenessTestIdInShard(3, 0)})
		if n := size(); n != 1 {
			t.Fatalf("holds %d verdicts after the sweep, want the one read since", n)
		}
	})
}
