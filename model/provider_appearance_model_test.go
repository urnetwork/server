package model

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The appearance histogram without redis: bucketing, the in-process counter,
// the flush and its bounds, the key layout, trimming, expiry and the read.

// A redis stand-in with the hash, expiry and failure behavior the flush and
// the read rely on. Writes apply in full or not at all, like the MULTI/EXEC
// of each write in redisProviderAppearanceStore.writeAll.
type testingProviderAppearanceStore struct {
	stateLock      sync.Mutex
	nowFunc        func() time.Time
	keyHashes      map[string]map[string]int64
	keyExpireTimes map[string]time.Time
	// every write fails with this error
	writeErr error
	// every write waits for its context to end
	block  bool
	writes []*providerAppearanceWrite
}

// An empty store whose expiry reads the time from nowFunc.
func newTestingProviderAppearanceStore(nowFunc func() time.Time) *testingProviderAppearanceStore {
	return &testingProviderAppearanceStore{
		nowFunc:        nowFunc,
		keyHashes:      map[string]map[string]int64{},
		keyExpireTimes: map[string]time.Time{},
	}
}

// Applies each write in order, or fails it as the store is set to.
func (self *testingProviderAppearanceStore) writeAll(ctx context.Context, writes []*providerAppearanceWrite, parallel int) []error {
	errs := make([]error, len(writes))
	for i, write := range writes {
		switch {
		case self.block:
			<-ctx.Done()
			errs[i] = ctx.Err()
		case self.writeErr != nil:
			errs[i] = self.writeErr
		default:
			self.apply(write)
		}
	}
	return errs
}

// Drops the key's hash once its expiry has passed.
func (self *testingProviderAppearanceStore) expireWithLock(key string) {
	if expiry, ok := self.keyExpireTimes[key]; ok && !self.nowFunc().Before(expiry) {
		delete(self.keyHashes, key)
		delete(self.keyExpireTimes, key)
	}
}

// Applies one write: its increments, its stale field removals and its expiry.
func (self *testingProviderAppearanceStore) apply(write *providerAppearanceWrite) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writes = append(self.writes, write)
	self.expireWithLock(write.key)
	hash, ok := self.keyHashes[write.key]
	if !ok {
		hash = map[string]int64{}
		self.keyHashes[write.key] = hash
	}
	for field, count := range write.fieldIncrements {
		hash[field] += count
	}
	for _, field := range write.staleFields {
		delete(hash, field)
	}
	self.keyExpireTimes[write.key] = self.nowFunc().Add(write.ttl)
}

// The key's live hash, nil once it has expired or was never written.
func (self *testingProviderAppearanceStore) hash(key string) map[string]int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.expireWithLock(key)
	return self.keyHashes[key]
}

// Reads each client's live hash as redis returns it, as strings.
func (self *testingProviderAppearanceStore) readAll(ctx context.Context, clientIds []server.Id) ([]map[string]string, error) {
	fieldsList := []map[string]string{}
	for _, clientId := range clientIds {
		fields := map[string]string{}
		for field, count := range self.hash(providerAppearanceKey(clientId)) {
			fields[field] = strconv.FormatInt(count, 10)
		}
		fieldsList = append(fieldsList, fields)
	}
	return fieldsList, nil
}

// The default settings with a short flush timeout.
func testingProviderAppearanceSettings() *ProviderAppearanceSettings {
	settings := DefaultProviderAppearanceSettings()
	settings.FlushTimeout = 200 * time.Millisecond
	return settings
}

// One provider is one key whose hash tag is exactly its client id, so its
// minutes sit in one cluster slot and every command of a write (one MULTI on
// one key) is cluster-safe; providers spread over distinct tags.
func TestProviderAppearanceKeyLayout(t *testing.T) {
	a := server.NewId()
	b := server.NewId()
	keyA := providerAppearanceKey(a)
	if keyA != fmt.Sprintf("{pa_%s}m", a) {
		t.Fatalf("key = %s", keyA)
	}
	tag := func(key string) string {
		return key[strings.Index(key, "{")+1 : strings.Index(key, "}")]
	}
	if tag(keyA) != "pa_"+a.String() {
		t.Fatalf("hash tag = %s, want the client id", tag(keyA))
	}
	if tag(keyA) == tag(providerAppearanceKey(b)) {
		t.Fatal("two providers share a hash tag")
	}
	if providerAppearanceField(29_000_000) != "29000000" {
		t.Fatal("a field is the unix minute in decimal")
	}

	write, _ := newProviderAppearanceWrite(a, map[int64]int64{29_000_000: 3}, 29_000_000)
	if write.key != keyA || write.clientId != a {
		t.Fatal("a write targets only its provider's key")
	}
	if write.ttl != ProviderAppearanceTtl || ProviderAppearanceTtl != 65*time.Minute {
		t.Fatalf("ttl = %s, want 65 minutes after the last write", write.ttl)
	}
}

// Appearances count in the unix minute of their time.
func TestProviderAppearanceMinuteBucketing(t *testing.T) {
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	minute := providerAppearanceMinute(base)
	if minute != base.Unix()/60 {
		t.Fatalf("minute = %d", minute)
	}
	if providerAppearanceMinute(base.Add(59*time.Second+999*time.Millisecond)) != minute {
		t.Fatal("the last instant of a minute left its bucket")
	}
	if providerAppearanceMinute(base.Add(time.Minute)) != minute+1 {
		t.Fatal("the next minute did not start a new bucket")
	}

	now := base.Add(59 * time.Second)
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	appearances := newProviderAppearancesWithoutRun(context.Background(), testingProviderAppearanceSettings(), store, func() time.Time { return now })
	clientId := server.NewId()
	appearances.Record([]server.Id{clientId}, base.Add(59*time.Second))
	appearances.Record([]server.Id{clientId}, base.Add(60*time.Second))
	appearances.Record([]server.Id{clientId}, base.Add(61*time.Second))
	now = base.Add(61 * time.Second)
	appearances.flush(context.Background())

	hash := store.hash(providerAppearanceKey(clientId))
	if hash[providerAppearanceField(minute)] != 1 || hash[providerAppearanceField(minute+1)] != 2 || len(hash) != 2 {
		t.Fatalf("buckets = %v", hash)
	}
}

// The counter sums repeats per (client, minute), drains to empty, and past its
// capacity drops only appearances that would add a new count.
func TestProviderAppearanceCounterDrainAndCapacity(t *testing.T) {
	counter := newProviderAppearanceCounter(1024)
	a := server.NewId()
	b := server.NewId()
	if dropped := counter.add([]server.Id{a, b, a}, 10); dropped != 0 {
		t.Fatalf("dropped %d", dropped)
	}
	counter.add([]server.Id{a}, 11)
	counts := counter.drain()
	if counts[a][10] != 2 || counts[a][11] != 1 || counts[b][10] != 1 || len(counts) != 2 {
		t.Fatalf("counts = %v", counts)
	}
	if len(counter.drain()) != 0 {
		t.Fatal("a drain left counts behind")
	}

	// one count per shard: a client whose shard holds a count drops new keys
	full := newProviderAppearanceCounter(providerAppearanceShardCount)
	full.add([]server.Id{a}, 10)
	// another client in a's shard: the shard is picked by the id's last byte
	sameShardClientId := a
	sameShardClientId[0] ^= 0xff
	if full.shard(sameShardClientId) != full.shard(a) {
		t.Fatal("the ids are not in one shard")
	}
	if dropped := full.add([]server.Id{a, sameShardClientId}, 10); dropped != 1 {
		t.Fatalf("dropped %d, want only the new count", dropped)
	}
	if counts := full.drain(); counts[a][10] != 2 || counts[sameShardClientId] != nil {
		t.Fatalf("counts = %v", counts)
	}
}

// A flush writes each provider's minutes in one write and leaves nothing
// pending; counts for minutes the window no longer holds are dropped.
func TestProviderAppearanceFlushWritesCounts(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 30, 15, 0, time.UTC)
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	appearances := newProviderAppearancesWithoutRun(context.Background(), testingProviderAppearanceSettings(), store, func() time.Time { return now })

	a := server.NewId()
	b := server.NewId()
	c := server.NewId()
	appearances.Record([]server.Id{a, b}, now)
	appearances.Record([]server.Id{a}, now)
	appearances.Record([]server.Id{a}, now.Add(-time.Minute))
	// recorded an hour and more ago: outside the window when written
	appearances.Record([]server.Id{c}, now.Add(-time.Duration(providerAppearanceRetainedBuckets)*time.Minute))

	result := appearances.flush(context.Background())
	if result.writtenCount != 4 || result.staleCount != 1 || result.failedCount != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(store.writes) != 2 {
		t.Fatalf("%d writes, want one per provider with counts", len(store.writes))
	}
	nowMinute := providerAppearanceMinute(now)
	if hash := store.hash(providerAppearanceKey(a)); hash[providerAppearanceField(nowMinute)] != 2 || hash[providerAppearanceField(nowMinute-1)] != 1 {
		t.Fatalf("a = %v", hash)
	}
	if hash := store.hash(providerAppearanceKey(b)); hash[providerAppearanceField(nowMinute)] != 1 || len(hash) != 1 {
		t.Fatalf("b = %v", hash)
	}
	if store.hash(providerAppearanceKey(c)) != nil {
		t.Fatal("a stale count was written")
	}
	if result := appearances.flush(context.Background()); result != (providerAppearanceFlushResult{}) || len(store.writes) != 2 {
		t.Fatal("an idle flush wrote")
	}
}

// A failing write drops its counts (never retried, never held), and the
// failures cost at most one log line per FailureLogInterval, which then
// sums every drop since the previous line.
func TestProviderAppearanceFlushFailureDropsAndLogsBounded(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	store.writeErr = errors.New("redis: connection refused")
	settings := testingProviderAppearanceSettings()
	appearances := newProviderAppearancesWithoutRun(context.Background(), settings, store, func() time.Time { return now })
	logLines := []string{}
	appearances.logFunc = func(format string, args ...any) {
		logLines = append(logLines, fmt.Sprintf(format, args...))
	}

	clientIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
	for i := 0; i < 100; i += 1 {
		appearances.Record(clientIds, now)
		result := appearances.flush(context.Background())
		if result.failedCount != 3 || result.failedProviderCount != 3 || result.writtenCount != 0 {
			t.Fatalf("result = %+v", result)
		}
		now = now.Add(settings.FlushInterval)
	}
	// 100 flushes over 500 seconds: the first line, then one per minute
	if len(logLines) != 9 {
		t.Fatalf("%d log lines for 100 failing flushes, want 9", len(logLines))
	}
	if !strings.Contains(logLines[1], "dropped 36 provider appearances of 36 provider writes") {
		t.Fatalf("line = %s", logLines[1])
	}
	if len(appearances.counter.drain()) != 0 {
		t.Fatal("failed counts were kept for a retry")
	}

	// recovery: the next flush past the interval reports the drops since the
	// last line, then no line while writes succeed
	store.writeErr = nil
	for i := 0; i < 3; i += 1 {
		now = now.Add(time.Hour)
		appearances.Record(clientIds, now)
		if result := appearances.flush(context.Background()); result.writtenCount != 3 || len(logLines) != 10 {
			t.Fatalf("result = %+v, %d lines", result, len(logLines))
		}
	}
	if !strings.Contains(logLines[9], "dropped 9 provider appearances of 9 provider writes") {
		t.Fatalf("line = %s", logLines[9])
	}
}

// A flush against a redis that never answers returns after FlushTimeout with
// every count dropped.
func TestProviderAppearanceFlushIsBoundedByTimeout(t *testing.T) {
	now := time.Now()
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	store.block = true
	settings := testingProviderAppearanceSettings()
	appearances := newProviderAppearancesWithoutRun(context.Background(), settings, store, func() time.Time { return now })
	appearances.logFunc = func(string, ...any) {}
	appearances.Record([]server.Id{server.NewId(), server.NewId()}, now)

	startTime := time.Now()
	result := appearances.flush(context.Background())
	if elapsed := time.Since(startTime); elapsed < settings.FlushTimeout || 10*settings.FlushTimeout < elapsed {
		t.Fatalf("flush took %s, want about %s", elapsed, settings.FlushTimeout)
	}
	if result.failedCount != 2 || !errors.Is(result.firstErr, context.DeadlineExceeded) {
		t.Fatalf("result = %+v", result)
	}
}

// Each write removes the stale minutes and renews the expiry, so a provider
// offered around the clock, with gaps of every length up to past the expiry,
// keeps at most the window plus one minute: nothing older survives a write.
func TestProviderAppearanceHashStaysBounded(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	clientId := server.NewId()
	key := providerAppearanceKey(clientId)
	random := mathrand.New(mathrand.NewSource(8))

	maxFields := 0
	writeCount := 0
	for session := 0; session < 80; session += 1 {
		// offered every few seconds for up to three hours, a flush at a time
		activeEnd := now.Add(time.Duration(random.Intn(3*60*60)) * time.Second)
		for now.Before(activeEnd) {
			now = now.Add(time.Duration(1+random.Intn(10)) * time.Second)
			nowMinute := providerAppearanceMinute(now)
			minuteCounts := map[int64]int64{nowMinute: 1}
			if random.Intn(4) == 0 {
				// a count from the previous minute, flushed late
				minuteCounts[nowMinute-1] = 1
			}
			write, _ := newProviderAppearanceWrite(clientId, minuteCounts, nowMinute)
			store.apply(write)
			writeCount += 1

			hash := store.hash(key)
			maxFields = max(maxFields, len(hash))
			if providerAppearanceRetainedBuckets < len(hash) {
				t.Fatalf("write %d: %d fields, want at most %d", writeCount, len(hash), providerAppearanceRetainedBuckets)
			}
			for field := range hash {
				minute, err := strconv.ParseInt(field, 10, 64)
				if err != nil || minute <= nowMinute-int64(providerAppearanceRetainedBuckets) || nowMinute < minute {
					t.Fatalf("write %d: field %s outside the window ending at %d", writeCount, field, nowMinute)
				}
			}
		}
		// then idle, from a moment to past the expiry
		now = now.Add(time.Duration(random.Intn(75*60)) * time.Second)
	}
	if maxFields < ProviderAppearanceWindowBuckets {
		t.Fatalf("the simulation never filled the window (%d fields)", maxFields)
	}

	// idle past the expiry: nothing is left
	now = now.Add(ProviderAppearanceTtl)
	if store.hash(key) != nil {
		t.Fatal("an idle provider's hash outlived its expiry")
	}
}

// A read is the window ending at the current minute, oldest first, zero
// filled; fields outside it or malformed are ignored.
func TestProviderAppearanceHistogramZeroFill(t *testing.T) {
	endMinute := int64(29_000_100)
	histogram := newProviderAppearanceHistogram(map[string]string{
		providerAppearanceField(endMinute):      "7",
		providerAppearanceField(endMinute - 1):  "2",
		providerAppearanceField(endMinute - 59): "5",
		// outside the window, both sides
		providerAppearanceField(endMinute - 60): "100",
		providerAppearanceField(endMinute + 1):  "100",
		"not-a-minute":                          "9",
		providerAppearanceField(endMinute - 30): "-4",
	}, endMinute)

	if histogram.StartMinute != endMinute-59 || histogram.BucketSeconds != 60 {
		t.Fatalf("start = %d, bucket = %d", histogram.StartMinute, histogram.BucketSeconds)
	}
	if len(histogram.AppearancesPerMinute) != ProviderAppearanceWindowBuckets || ProviderAppearanceWindowBuckets != 60 {
		t.Fatalf("%d buckets, want 60", len(histogram.AppearancesPerMinute))
	}
	want := make([]int64, 60)
	want[0] = 5
	want[58] = 2
	want[59] = 7
	for i, count := range histogram.AppearancesPerMinute {
		if count != want[i] {
			t.Fatalf("bucket %d = %d, want %d (%v)", i, count, want[i], histogram.AppearancesPerMinute)
		}
	}

	empty := newProviderAppearanceHistogram(nil, endMinute)
	for _, count := range empty.AppearancesPerMinute {
		if count != 0 {
			t.Fatal("a provider with no hash is not all zeros")
		}
	}
}

// Recording, flushing and reading back give each provider exactly its own
// appearances; a provider never offered reads as zeros.
func TestProviderAppearanceRecordFlushRead(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	appearances := newProviderAppearancesWithoutRun(context.Background(), testingProviderAppearanceSettings(), store, func() time.Time { return now })
	offeredClientIds := []server.Id{server.NewId(), server.NewId()}
	idleClientId := server.NewId()
	appearances.Record(offeredClientIds, now)
	appearances.Record(offeredClientIds[:1], now)
	appearances.flush(context.Background())

	histograms, err := getProviderAppearanceHistograms(context.Background(), store, append(offeredClientIds, idleClientId), now)
	if err != nil {
		t.Fatal(err)
	}
	last := ProviderAppearanceWindowBuckets - 1
	if histograms[offeredClientIds[0]].AppearancesPerMinute[last] != 2 || histograms[offeredClientIds[1]].AppearancesPerMinute[last] != 1 {
		t.Fatal("the offered providers' current minute is wrong")
	}
	for _, count := range histograms[idleClientId].AppearancesPerMinute {
		if count != 0 {
			t.Fatal("a provider never offered read a count")
		}
	}
}

// Only an owner in the context counts; a bare context counts nothing and a
// nil owner is safe.
func TestRecordProviderAppearancesRequiresOwner(t *testing.T) {
	now := time.Now()
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	appearances := newProviderAppearancesWithoutRun(context.Background(), testingProviderAppearanceSettings(), store, func() time.Time { return now })
	clientId := server.NewId()

	recordProviderAppearances(context.Background(), []server.Id{clientId}, now)
	if len(appearances.counter.drain()) != 0 {
		t.Fatal("a bare context counted")
	}
	var nilAppearances *ProviderAppearances
	nilAppearances.Record([]server.Id{clientId}, now)

	recordProviderAppearances(WithProviderAppearances(context.Background(), appearances), []server.Id{clientId}, now)
	if counts := appearances.counter.drain(); counts[clientId][providerAppearanceMinute(now)] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

// Close stops the loop and writes what is pending once.
func TestProviderAppearancesCloseFlushes(t *testing.T) {
	now := time.Now()
	store := newTestingProviderAppearanceStore(func() time.Time { return now })
	settings := testingProviderAppearanceSettings()
	settings.FlushInterval = time.Hour
	appearances := newProviderAppearancesWithoutRun(context.Background(), settings, store, func() time.Time { return now })
	appearances.loopDone = make(chan struct{})
	go appearances.run()

	clientId := server.NewId()
	appearances.Record([]server.Id{clientId}, now)
	appearances.Close()
	appearances.Close()
	select {
	case <-appearances.loopDone:
	default:
		t.Fatal("close did not stop the loop")
	}
	if hash := store.hash(providerAppearanceKey(clientId)); hash[providerAppearanceField(providerAppearanceMinute(now))] != 1 {
		t.Fatalf("hash = %v", hash)
	}
	if len(store.writes) != 1 {
		t.Fatalf("%d writes, want one", len(store.writes))
	}
}
