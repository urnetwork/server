// The load publisher and the inventory cache (move_load.go): the /proc/stat
// parse and the CPU intervals, the record a publish writes, the loop that
// publishes from the start and removes the record when it stops, the disabled
// settings, and the inventory reload that keeps the last good inventory.
package connect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The aggregate line of a current kernel and of an old one, and lines a parse
// must refuse.
func TestParseProcStatCpu(t *testing.T) {
	current := `cpu  4705 356 584 3699176 23060 0 277 1000 600 0
cpu0 1393280 32966 572056 13343292 6130 0 17875 0 23933 0
intr 114930548 113199788 3 0 5 263 0 4 [... lots more numbers ...]
ctxt 1990473
`
	cpu, err := parseProcStatCpu(strings.NewReader(current))
	if err != nil {
		t.Fatal(err)
	}
	// user..steal; guest and guest_nice are already in user and nice
	total := uint64(4705 + 356 + 584 + 3699176 + 23060 + 0 + 277 + 1000)
	if cpu.total != total || cpu.busy != total-3699176-23060 {
		t.Errorf("current kernel = %+v", cpu)
	}

	old, err := parseProcStatCpu(strings.NewReader("cpu 10 20 30 40\n"))
	if err != nil || old.total != 100 || old.busy != 60 {
		t.Errorf("old kernel = %+v (%v)", old, err)
	}

	for _, refused := range []string{
		"",
		"cpu0 1 2 3 4\n",
		"cpu 1 2 3\n",
		"cpu 1 2 three 4\n",
		"cpu -1 2 3 4\n",
	} {
		if _, err := parseProcStatCpu(strings.NewReader(refused)); err == nil {
			t.Errorf("parsed %q", refused)
		}
	}
}

// The busy fraction between two readings, and unknown when no time passed or
// a counter went backwards.
func TestProcStatCpuBusyFraction(t *testing.T) {
	previous := procStatCpu{busy: 1000, total: 4000}
	if busy := (procStatCpu{busy: 1750, total: 5000}).busyFractionSince(previous); busy != 0.75 {
		t.Errorf("busy = %v", busy)
	}
	if busy := (procStatCpu{busy: 1000, total: 4000}).busyFractionSince(previous); busy != model.ConnectMoveLoadCpuUnknown {
		t.Errorf("no interval = %v", busy)
	}
	if busy := (procStatCpu{busy: 900, total: 5000}).busyFractionSince(previous); busy != model.ConnectMoveLoadCpuUnknown {
		t.Errorf("a backwards counter = %v", busy)
	}
}

// The sampler measures each interval since its previous sample: host CPU from
// the stat file, process CPU in cores. The first sample has no interval. A
// missing stat file is an unknown host CPU, not an error.
func TestProcMoveCpuSamplerMeasuresIntervals(t *testing.T) {
	statPath := filepath.Join(t.TempDir(), "stat")
	writeStat := func(user int, idle int) {
		t.Helper()
		content := fmt.Sprintf("cpu %d 0 0 %d 0 0 0 0 0 0\n", user, idle)
		if err := os.WriteFile(statPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := moveTestNodeTime
	processCpu := time.Duration(0)
	sampler := &procMoveCpuSampler{
		procStatPath:   statPath,
		processCpuTime: func() (time.Duration, bool) { return processCpu, true },
		now:            func() time.Time { return now },
		hostCores:      72,
	}

	writeStat(1000, 9000)
	first := sampler.NextCpuSample()
	if first.hostCpuBusy != model.ConnectMoveLoadCpuUnknown || first.processCpu != 0 || first.hostCores != 72 {
		t.Errorf("first sample = %+v", first)
	}

	// 15 s: the host busy 400 of 1000 ticks, the process 30 s of CPU
	now = now.Add(15 * time.Second)
	processCpu = 30 * time.Second
	writeStat(1400, 9600)
	second := sampler.NextCpuSample()
	if second.hostCpuBusy != 0.4 || second.processCpu != 2 {
		t.Errorf("second sample = %+v", second)
	}

	// the stat file goes away: host CPU unknown, process CPU still measured
	if err := os.Remove(statPath); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	processCpu = 45 * time.Second
	third := sampler.NextCpuSample()
	if third.hostCpuBusy != model.ConnectMoveLoadCpuUnknown || third.processCpu != 1 {
		t.Errorf("third sample = %+v", third)
	}

	// back again: one sample to prime the interval
	writeStat(2000, 10000)
	now = now.Add(15 * time.Second)
	if fourth := sampler.NextCpuSample(); fourth.hostCpuBusy != model.ConnectMoveLoadCpuUnknown {
		t.Errorf("fourth sample = %+v", fourth)
	}

	// the production sampler reads this host's counters
	production := newProcMoveCpuSampler()
	production.NextCpuSample()
	sample := production.NextCpuSample()
	if sample.hostCores <= 0 || sample.processCpu < 0 || math.IsNaN(sample.processCpu) {
		t.Errorf("production sample = %+v", sample)
	}
}

// A fixed CPU sample.
type moveTestCpuSampler struct {
	sample moveCpuSample
}

func (self *moveTestCpuSampler) NextCpuSample() moveCpuSample {
	return self.sample
}

// A fake of the load table for the publisher.
type moveTestLoadStore struct {
	stateLock  sync.Mutex
	loads      []*model.ConnectMoveLoad
	deadlines  []bool
	publishErr error
	removes    []MoveProcessIdentity
	published  chan struct{}
	removed    chan struct{}
}

func (self *moveTestLoadStore) publishLoad(ctx context.Context, load *model.ConnectMoveLoad) (time.Time, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	_, hasDeadline := ctx.Deadline()
	self.deadlines = append(self.deadlines, hasDeadline)
	if self.publishErr != nil {
		return time.Time{}, self.publishErr
	}
	self.loads = append(self.loads, load)
	if self.published != nil {
		select {
		case self.published <- struct{}{}:
		default:
		}
	}
	return moveTestNodeTime, nil
}

func (self *moveTestLoadStore) removeLoad(ctx context.Context, identity MoveProcessIdentity) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removes = append(self.removes, identity)
	if self.removed != nil {
		close(self.removed)
		self.removed = nil
	}
	return nil
}

// A publish writes the process's identity, counts and CPU, bounded by a
// deadline, and exports the host CPU it measured; a failed write is counted.
func TestMoveLoadPublisherPublishesTheProcessLoad(t *testing.T) {
	settings := DefaultExchangeSettings()
	identity := MoveProcessIdentity{Host: "edge-a.example", Block: "g2", ExchangeId: server.NewId()}
	source := &moveTestLoadSource{counts: MoveLoadCounts{Residents: 1200, ProviderResidents: 800, Transports: 1500, Draining: true}}
	sampler := &moveTestCpuSampler{sample: moveCpuSample{processCpu: 3.5, hostCpuBusy: 0.82, hostCores: 72}}
	store := &moveTestLoadStore{}
	publisher := newMoveLoadPublisherWithStore(context.Background(), identity, settings, source, sampler, store)
	defer publisher.Close()

	publishedBefore := testutil.ToFloat64(moveLoadPublishesCounter.WithLabelValues(moveLoadPublished))
	if err := publisher.publish(); err != nil {
		t.Fatal(err)
	}
	if len(store.loads) != 1 || !store.deadlines[0] {
		t.Fatalf("published %d records, deadline %v", len(store.loads), store.deadlines)
	}
	load := store.loads[0]
	if load.Host != "edge-a.example" || load.Block != "g2" || load.ExchangeId != identity.ExchangeId ||
		load.Residents != 1200 || load.ProviderResidents != 800 || load.Transports != 1500 || !load.Draining ||
		load.ProcessCpu != 3.5 || load.HostCpuBusy != 0.82 || load.HostCores != 72 {
		t.Errorf("published %+v", load)
	}
	if delta := testutil.ToFloat64(moveLoadPublishesCounter.WithLabelValues(moveLoadPublished)) - publishedBefore; delta != 1 {
		t.Errorf("published counter rose %v", delta)
	}
	if busy := testutil.ToFloat64(moveHostCpuBusyGauge.WithLabelValues()); busy != 0.82 {
		t.Errorf("host CPU gauge = %v", busy)
	}

	// an unknown CPU has no series
	sampler.sample.hostCpuBusy = model.ConnectMoveLoadCpuUnknown
	if err := publisher.publish(); err != nil {
		t.Fatal(err)
	}
	if count := testutil.CollectAndCount(moveHostCpuBusyGauge); count != 0 {
		t.Errorf("host CPU series = %d while unknown", count)
	}

	failedBefore := testutil.ToFloat64(moveLoadPublishesCounter.WithLabelValues(moveLoadPublishFailed))
	store.publishErr = errors.New("redis: i/o timeout")
	if err := publisher.publish(); err == nil {
		t.Fatal("a failed publish answered nil")
	}
	if delta := testutil.ToFloat64(moveLoadPublishesCounter.WithLabelValues(moveLoadPublishFailed)) - failedBefore; delta != 1 {
		t.Errorf("failed counter rose %v", delta)
	}
}

// The loop publishes at once, then every interval, and removes the record
// when it stops.
func TestMoveLoadPublisherLoopPublishesUntilClosed(t *testing.T) {
	settings := DefaultExchangeSettings()
	settings.MoveLoadPublishInterval = 5 * time.Millisecond
	identity := MoveProcessIdentity{Host: "edge-a.example", Block: "g1", ExchangeId: server.NewId()}
	store := &moveTestLoadStore{
		published: make(chan struct{}, 1),
		removed:   make(chan struct{}),
	}
	removed := store.removed
	publisher := startMoveLoadPublisher(newMoveLoadPublisherWithStore(
		context.Background(),
		identity,
		settings,
		&moveTestLoadSource{},
		&moveTestCpuSampler{sample: moveCpuSample{hostCpuBusy: 0.5, hostCores: 8}},
		store,
	))
	for i := 0; i < 2; i += 1 {
		select {
		case <-store.published:
		case <-time.After(30 * time.Second):
			t.Fatal("the loop did not publish")
		}
	}
	publisher.Close()
	select {
	case <-removed:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop did not remove the record when it stopped")
	}
	store.stateLock.Lock()
	defer store.stateLock.Unlock()
	if len(store.removes) != 1 || store.removes[0] != identity {
		t.Errorf("removes = %+v", store.removes)
	}
}

// With EnableMove off, or a non-positive interval, the publisher publishes
// nothing and removes nothing.
func TestMoveLoadPublisherDisabled(t *testing.T) {
	disabled := DefaultExchangeSettings()
	disabled.EnableMove = false
	noInterval := DefaultExchangeSettings()
	noInterval.MoveLoadPublishInterval = 0
	for _, settings := range []*ExchangeSettings{disabled, noInterval} {
		store := &moveTestLoadStore{}
		publisher := newMoveLoadPublisherWithStore(
			context.Background(),
			MoveProcessIdentity{Host: "edge-a.example", Block: "g1", ExchangeId: server.NewId()},
			settings,
			&moveTestLoadSource{},
			&moveTestCpuSampler{},
			store,
		)
		done := make(chan struct{})
		go func() {
			defer close(done)
			publisher.run()
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("a disabled publisher kept running")
		}
		if len(store.loads) != 0 || len(store.removes) != 0 {
			t.Errorf("a disabled publisher wrote: %d publishes, %d removes", len(store.loads), len(store.removes))
		}
	}
}

// The cache loads the inventory at its first use, reloads it every refresh
// interval, keeps the last good one while a load fails and tries again at the
// next use, and with a non-positive interval loads once.
func TestMoveInventoryCacheReloadsAndKeepsTheLastGood(t *testing.T) {
	ctx := context.Background()
	now := moveTestNodeTime
	first := map[string]MoveInventoryHost{"edge-a.example": moveTestInventoryHost("192.0.2.1", "")}
	second := map[string]MoveInventoryHost{
		"edge-a.example":   moveTestInventoryHost("192.0.2.1", ""),
		"edge-new.example": moveTestInventoryHost("192.0.2.7", "2001:db8::7"),
	}
	loader := &moveTestInventory{err: errors.New("vault: services.yml not found")}
	cache := &moveInventoryCache{loader: loader, settings: &ExchangeMoverSettings{MoveInventoryRefreshInterval: 5 * time.Minute}}

	if hosts := cache.hosts(ctx, now); len(hosts) != 0 {
		t.Fatalf("hosts before any load = %v", hosts)
	}
	loader.err = nil
	loader.inventory = first
	if hosts := cache.hosts(ctx, now.Add(10*time.Second)); len(hosts) != 1 {
		t.Fatalf("the load after a failure did not run at the next use: %v", hosts)
	}
	loader.inventory = second
	if hosts := cache.hosts(ctx, now.Add(4*time.Minute)); len(hosts) != 1 || loader.loadCount != 2 {
		t.Fatalf("reloaded before the refresh interval: %d loads", loader.loadCount)
	}
	if hosts := cache.hosts(ctx, now.Add(10*time.Second+5*time.Minute)); len(hosts) != 2 {
		t.Fatalf("a new edge host was not picked up at the refresh: %v", hosts)
	}

	loader.err = errors.New("vault: parse error")
	if hosts := cache.hosts(ctx, now.Add(11*time.Minute)); len(hosts) != 2 {
		t.Fatalf("a failed reload dropped the last good inventory: %v", hosts)
	}

	once := &moveTestInventory{inventory: first}
	onceCache := &moveInventoryCache{loader: once, settings: &ExchangeMoverSettings{MoveInventoryRefreshInterval: 0}}
	onceCache.hosts(ctx, now)
	onceCache.hosts(ctx, now.Add(time.Hour))
	if once.loadCount != 1 {
		t.Errorf("a non-positive refresh loaded %d times", once.loadCount)
	}
}

// The cleanup timeout MOVE.md §5.4 names, and the mover cadences it works
// with.
func TestDefaultMoverLoadSettings(t *testing.T) {
	settings := DefaultExchangeSettings()
	if settings.MoveLoadCleanupTimeout != 10*time.Minute {
		t.Errorf("cleanup timeout = %s", settings.MoveLoadCleanupTimeout)
	}
	// a record refreshes several times within the stale timeout, and goes
	// stale long before the cleanup deletes it
	if !(settings.MoveLoadPublishInterval*3 <= settings.MoveLoadStaleTimeout && settings.MoveLoadStaleTimeout < settings.MoveLoadCleanupTimeout) {
		t.Errorf("publish %s, stale %s, cleanup %s", settings.MoveLoadPublishInterval, settings.MoveLoadStaleTimeout, settings.MoveLoadCleanupTimeout)
	}
	if settings.MoveTickInterval != 10*time.Second || settings.MoveMaxPerTick != 10 || settings.MoveInventoryRefreshInterval != 5*time.Minute ||
		settings.MoveDbWaitPauseThreshold != 50*time.Millisecond || settings.MoveEmergencyCpu != 0.95 || settings.MoveEmergencyTicks != 3 {
		t.Errorf("mover settings = %+v", settings.ExchangeMoverSettings)
	}
}
