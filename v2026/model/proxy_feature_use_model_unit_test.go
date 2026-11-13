package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Pure tests of the feature use marks: no database or redis.

// Records each markAll call; err fails them.
type testProxyFeatureUseStore struct {
	stateLock sync.Mutex
	writes    [][]*ProxyFeatureUseMark
	err       error
}

func (self *testProxyFeatureUseStore) markAll(ctx context.Context, marks []*ProxyFeatureUseMark) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return self.err
	}
	self.writes = append(self.writes, slices.Clone(marks))
	return nil
}

func (self *testProxyFeatureUseStore) readAll(ctx context.Context, proxyIds []server.Id) (map[server.Id]map[string]time.Time, error) {
	return nil, errors.New("not read in these tests")
}

func (self *testProxyFeatureUseStore) setErr(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

// The marks of the last write, keyed for comparison.
func (self *testProxyFeatureUseStore) lastWrite() map[proxyFeatureUseKeyFeature]time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	marks := map[proxyFeatureUseKeyFeature]time.Time{}
	if len(self.writes) == 0 {
		return marks
	}
	for _, mark := range self.writes[len(self.writes)-1] {
		marks[proxyFeatureUseKeyFeature{proxyId: mark.ProxyId, feature: mark.Feature}] = mark.UseTime
	}
	return marks
}

// A fixed plan per proxy; a proxy it does not list is unknown.
type testProxyFeatureUsePlans map[server.Id]string

func (self testProxyFeatureUsePlans) plans(ctx context.Context, proxyIds []server.Id) map[server.Id]string {
	plans := map[server.Id]string{}
	for _, proxyId := range proxyIds {
		if plan, ok := self[proxyId]; ok {
			plans[proxyId] = plan
		}
	}
	return plans
}

// A planner whose read raises, as a database outage does.
type testRaisingProxyFeatureUsePlans struct{}

func (testRaisingProxyFeatureUsePlans) plans(ctx context.Context, proxyIds []server.Id) map[server.Id]string {
	panic(errors.New("synthetic plan read failure"))
}

type testProxyFeatureUseClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testProxyFeatureUseClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testProxyFeatureUseClock) Set(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = now
}

func newTestProxyFeatureUses(
	t *testing.T,
	settings *ProxyFeatureUseSettings,
	store proxyFeatureUseStore,
	planner proxyFeatureUsePlanReader,
	clock *testProxyFeatureUseClock,
) (*ProxyFeatureUses, *[]string) {
	t.Helper()
	uses := newProxyFeatureUsesWithoutRun(context.Background(), settings, store, planner, clock.Now)
	lines := &[]string{}
	var linesLock sync.Mutex
	uses.logFunc = func(format string, args ...any) {
		linesLock.Lock()
		defer linesLock.Unlock()
		*lines = append(*lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(uses.Close)
	return uses, lines
}

// One proxy's feature is written at most once per interval: the uses in
// between change nothing, the newest use since the last flush is the one
// written, and the features and proxies are independent.
func TestProxyFeatureUsesWriteOneMarkPerProxyFeatureAndInterval(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := &testProxyFeatureUseClock{now: t0}
	store := &testProxyFeatureUseStore{}
	uses, _ := newTestProxyFeatureUses(t, DefaultProxyFeatureUseSettings(), store, nil, clock)
	ctx := context.Background()
	p1, p2 := server.NewId(), server.NewId()
	key := func(proxyId server.Id, feature string) proxyFeatureUseKeyFeature {
		return proxyFeatureUseKeyFeature{proxyId: proxyId, feature: feature}
	}

	connect.AssertEqual(t, uses.flush(ctx), 0)

	uses.Note(p1, FeatureSocksProxy, t0)
	uses.Note(p1, FeatureSocksProxy, t0.Add(time.Minute))
	uses.Note(p1, FeatureWireguardProxy, t0)
	uses.Note(p2, FeatureSocksProxy, t0)
	connect.AssertEqual(t, uses.flush(ctx), 3)
	connect.AssertEqual(t, store.lastWrite(), map[proxyFeatureUseKeyFeature]time.Time{
		key(p1, FeatureSocksProxy):     t0.Add(time.Minute),
		key(p1, FeatureWireguardProxy): t0,
		key(p2, FeatureSocksProxy):     t0,
	})

	// inside the interval of the written mark: nothing to write
	uses.Note(p1, FeatureSocksProxy, t0.Add(30*time.Minute))
	uses.Note(p1, FeatureSocksProxy, t0.Add(time.Minute).Add(ProxyFeatureUseMarkInterval-time.Millisecond))
	connect.AssertEqual(t, uses.flush(ctx), 0)

	// a use an interval after the written mark is written
	uses.Note(p1, FeatureSocksProxy, t0.Add(time.Minute).Add(ProxyFeatureUseMarkInterval))
	connect.AssertEqual(t, uses.flush(ctx), 1)
	connect.AssertEqual(t, store.lastWrite(), map[proxyFeatureUseKeyFeature]time.Time{
		key(p1, FeatureSocksProxy): t0.Add(time.Minute).Add(ProxyFeatureUseMarkInterval),
	})

	// a handshake older than the written mark, as a sampler reads it, never
	// rewrites the mark
	uses.Note(p1, FeatureWireguardProxy, t0.Add(-time.Minute))
	connect.AssertEqual(t, uses.flush(ctx), 0)

	connect.AssertEqual(t, len(store.writes), 2)
	connect.AssertEqual(t, ProxyFeatureUseMarkInterval, time.Hour)
}

// A write that fails keeps its uses for the next flush, with a newer use
// noted meanwhile, and the failures are logged at most once per interval.
func TestProxyFeatureUsesKeepFailedMarksForTheNextFlush(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := &testProxyFeatureUseClock{now: t0}
	store := &testProxyFeatureUseStore{}
	uses, lines := newTestProxyFeatureUses(t, DefaultProxyFeatureUseSettings(), store, nil, clock)
	ctx := context.Background()
	p1 := server.NewId()

	store.setErr(errors.New("synthetic redis outage"))
	uses.Note(p1, FeatureSocksProxy, t0)
	connect.AssertEqual(t, uses.flush(ctx), 0)
	connect.AssertEqual(t, len(*lines), 1)
	// the claim is released, so the use waits as a pending use, not as a mark
	uses.Note(p1, FeatureSocksProxy, t0.Add(time.Minute))
	connect.AssertEqual(t, uses.flush(ctx), 0)
	// one line per FailureLogInterval
	connect.AssertEqual(t, len(*lines), 1)

	store.setErr(nil)
	connect.AssertEqual(t, uses.flush(ctx), 1)
	connect.AssertEqual(t, store.lastWrite(), map[proxyFeatureUseKeyFeature]time.Time{
		{proxyId: p1, feature: FeatureSocksProxy}: t0.Add(time.Minute),
	})
	uses.Note(p1, FeatureSocksProxy, t0.Add(2*time.Minute))
	connect.AssertEqual(t, uses.flush(ctx), 0)

	// the next failure after the interval is logged again
	clock.Set(t0.Add(DefaultProxyFeatureUseSettings().FailureLogInterval))
	store.setErr(errors.New("synthetic redis outage"))
	uses.Note(server.NewId(), FeatureWireguardProxy, t0)
	connect.AssertEqual(t, uses.flush(ctx), 0)
	connect.AssertEqual(t, len(*lines), 2)
}

// The uses held between flushes and the marks remembered are bounded: a use
// past the pending bound is dropped and noted again at the proxy's next use,
// and a full memory of marks is cleared, which costs one more write.
func TestProxyFeatureUsesStayBounded(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := &testProxyFeatureUseClock{now: t0}
	store := &testProxyFeatureUseStore{}
	settings := DefaultProxyFeatureUseSettings()
	settings.MaxPendingUses = 2
	settings.MaxMarkedUses = 2
	uses, _ := newTestProxyFeatureUses(t, settings, store, nil, clock)
	ctx := context.Background()
	p1, p2, p3 := server.NewId(), server.NewId(), server.NewId()

	uses.Note(p1, FeatureSocksProxy, t0)
	uses.Note(p2, FeatureSocksProxy, t0)
	uses.Note(p3, FeatureSocksProxy, t0)
	// an update of a pending use is no new entry
	uses.Note(p1, FeatureSocksProxy, t0.Add(time.Second))
	connect.AssertEqual(t, uses.flush(ctx), 2)
	_, p3Written := store.lastWrite()[proxyFeatureUseKeyFeature{proxyId: p3, feature: FeatureSocksProxy}]
	connect.AssertEqual(t, p3Written, false)

	// the dropped proxy's next use is written, and remembering it clears the
	// memory of the first two
	uses.Note(p3, FeatureSocksProxy, t0.Add(time.Minute))
	connect.AssertEqual(t, uses.flush(ctx), 1)
	connect.AssertEqual(t, len(uses.marked) <= settings.MaxMarkedUses, true)
	uses.Note(p1, FeatureSocksProxy, t0.Add(2*time.Minute))
	connect.AssertEqual(t, uses.flush(ctx), 1)
	connect.AssertEqual(t, len(uses.marked) <= settings.MaxMarkedUses, true)

	// a mark an interval old holds nothing back and is forgotten at the next
	// flush, so the memory follows the last interval's uses
	roomy := DefaultProxyFeatureUseSettings()
	aging, _ := newTestProxyFeatureUses(t, roomy, store, nil, clock)
	aging.Note(p1, FeatureSocksProxy, t0)
	aging.Note(p2, FeatureWireguardProxy, t0)
	connect.AssertEqual(t, aging.flush(ctx), 2)
	connect.AssertEqual(t, len(aging.marked), 2)
	clock.Set(t0.Add(ProxyFeatureUseMarkInterval + time.Minute))
	aging.Note(p3, FeatureSocksProxy, t0.Add(ProxyFeatureUseMarkInterval+time.Minute))
	connect.AssertEqual(t, aging.flush(ctx), 1)
	connect.AssertEqual(t, aging.marked, map[proxyFeatureUseKeyFeature]int64{
		{proxyId: p3, feature: FeatureSocksProxy}: t0.Add(ProxyFeatureUseMarkInterval + time.Minute).UnixMilli(),
	})
}

// Each written mark is counted by the plan of the proxy's network: free, pro,
// or unknown when the plan could not be read. A failed write counts nothing.
func TestProxyFeatureUseMarksAreCountedByPlan(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := &testProxyFeatureUseClock{now: t0}
	store := &testProxyFeatureUseStore{}
	pFree, pPro, pUnknown := server.NewId(), server.NewId(), server.NewId()
	planner := testProxyFeatureUsePlans{pFree: proxyFeatureUsePlanFree, pPro: proxyFeatureUsePlanPro}
	uses, _ := newTestProxyFeatureUses(t, DefaultProxyFeatureUseSettings(), store, planner, clock)
	ctx := context.Background()

	count := func(feature string, plan string) float64 {
		return testutil.ToFloat64(proxyFeatureUseMarks.WithLabelValues(feature, plan))
	}
	type series struct{ feature, plan string }
	all := []series{
		{FeatureSocksProxy, proxyFeatureUsePlanFree},
		{FeatureSocksProxy, proxyFeatureUsePlanPro},
		{FeatureSocksProxy, proxyFeatureUsePlanUnknown},
		{FeatureWireguardProxy, proxyFeatureUsePlanFree},
		{FeatureWireguardProxy, proxyFeatureUsePlanPro},
		{FeatureWireguardProxy, proxyFeatureUsePlanUnknown},
	}
	snapshot := func() map[series]float64 {
		counts := map[series]float64{}
		for _, s := range all {
			counts[s] = count(s.feature, s.plan)
		}
		return counts
	}
	delta := func(before map[series]float64) map[series]float64 {
		deltas := map[series]float64{}
		for s, value := range snapshot() {
			if d := value - before[s]; d != 0 {
				deltas[s] = d
			}
		}
		return deltas
	}

	before := snapshot()
	uses.Note(pFree, FeatureSocksProxy, t0)
	uses.Note(pPro, FeatureSocksProxy, t0)
	uses.Note(pPro, FeatureWireguardProxy, t0)
	uses.Note(pUnknown, FeatureWireguardProxy, t0)
	connect.AssertEqual(t, uses.flush(ctx), 4)
	connect.AssertEqual(t, delta(before), map[series]float64{
		{FeatureSocksProxy, proxyFeatureUsePlanFree}:        1,
		{FeatureSocksProxy, proxyFeatureUsePlanPro}:         1,
		{FeatureWireguardProxy, proxyFeatureUsePlanPro}:     1,
		{FeatureWireguardProxy, proxyFeatureUsePlanUnknown}: 1,
	})

	before = snapshot()
	store.setErr(errors.New("synthetic redis outage"))
	uses.Note(server.NewId(), FeatureSocksProxy, t0)
	connect.AssertEqual(t, uses.flush(ctx), 0)
	connect.AssertEqual(t, len(delta(before)), 0)

	// a plan read that raises counts the marks as unknown
	store.setErr(nil)
	raising, _ := newTestProxyFeatureUses(t, DefaultProxyFeatureUseSettings(), store, testRaisingProxyFeatureUsePlans{}, clock)
	before = snapshot()
	raising.Note(pFree, FeatureSocksProxy, t0)
	connect.AssertEqual(t, raising.flush(ctx), 1)
	connect.AssertEqual(t, delta(before), map[series]float64{
		{FeatureSocksProxy, proxyFeatureUsePlanUnknown}: 1,
	})
}

// Close writes what is left once; a nil owner notes and closes nothing.
func TestProxyFeatureUsesCloseWritesWhatIsLeft(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := &testProxyFeatureUseClock{now: t0}
	store := &testProxyFeatureUseStore{}
	uses, _ := newTestProxyFeatureUses(t, DefaultProxyFeatureUseSettings(), store, nil, clock)
	p1 := server.NewId()
	uses.Note(p1, FeatureWireguardProxy, t0)
	uses.Close()
	connect.AssertEqual(t, store.lastWrite(), map[proxyFeatureUseKeyFeature]time.Time{
		{proxyId: p1, feature: FeatureWireguardProxy}: t0,
	})
	uses.Close()
	connect.AssertEqual(t, len(store.writes), 1)

	var none *ProxyFeatureUses
	none.Note(p1, FeatureSocksProxy, t0)
	none.Close()
}

// A mark's value is the use's unix milliseconds; anything else is no mark.
func TestProxyFeatureUseValueIsUnixMilliseconds(t *testing.T) {
	useTime := time.Date(2026, 11, 10, 9, 30, 0, 123_000_000, time.UTC)
	parsed, ok := parseProxyFeatureUseValue("1794303000123")
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, parsed.Equal(useTime), true)
	for _, value := range []any{nil, "", "x", 12, []byte("1")} {
		_, ok := parseProxyFeatureUseValue(value)
		connect.AssertEqual(t, ok, false)
	}
	connect.AssertEqual(t, proxyFeatureUseKey(server.RequireParseId("019e84f5-a3f0-0dd2-6d4d-dddc755b1edc")), "{pfu_019e84f5-a3f0-0dd2-6d4d-dddc755b1edc}")
	// the key outlives the window, so a mark that still counts is kept
	connect.AssertEqual(t, ProxyFeatureUseWindow < proxyFeatureUseTtl, true)
}
