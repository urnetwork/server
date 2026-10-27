package monitor

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// This models a legal live Err read followed by cancellation before the
// caller consumes that read. Only routerTargets' second direct Err call is
// armed; every other call delegates normally. It is not a held-lock barrier.
type routerPairReadContext struct {
	context.Context
	cancel             context.CancelFunc
	ownerLock          *sync.Mutex
	stateLock          sync.Mutex
	arm                bool
	checks             int
	consumed           int
	liveRead           bool
	closedAtBoundary   bool
	callbacksUnderLock int
}

func (self *routerPairReadContext) checkCallbackLock() {
	if self.ownerLock.TryLock() {
		self.ownerLock.Unlock()
		return
	}
	self.stateLock.Lock()
	self.callbacksUnderLock++
	self.stateLock.Unlock()
}

func (self *routerPairReadContext) Done() <-chan struct{} {
	self.checkCallbackLock()
	return self.Context.Done()
}

func (self *routerPairReadContext) Err() error {
	self.checkCallbackLock()
	snapshot := self.Context.Err()
	var pcs [1]uintptr
	runtime.Callers(2, pcs[:])
	frame, _ := runtime.CallersFrames(pcs[:]).Next()
	owner := runtime.FuncForPC(reflect.ValueOf(routerTargets).Pointer()).Name()
	if frame.Function != owner {
		return snapshot
	}
	self.stateLock.Lock()
	self.checks++
	cancelAtRead := self.arm && self.checks == 2 && self.consumed == 0 && snapshot == nil
	if cancelAtRead {
		self.consumed++
		self.liveRead = true
	}
	self.stateLock.Unlock()
	if cancelAtRead {
		self.cancel()
		closed := false
		select {
		case <-self.Context.Done():
			closed = self.Context.Err() == context.Canceled
		default:
		}
		self.stateLock.Lock()
		self.closedAtBoundary = closed
		self.stateLock.Unlock()
	}
	return snapshot
}

func (self *routerPairReadContext) requireBoundary(t *testing.T, armed bool) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	wantConsumed := 0
	if armed {
		wantConsumed = 1
	}
	if self.checks != 2 || self.consumed != wantConsumed || self.callbacksUnderLock != 0 {
		t.Fatalf("exact routerTargets read seam: checks=%d consumed=%d callbacks_under_lock=%d", self.checks, self.consumed, self.callbacksUnderLock)
	}
	if armed && (!self.liveRead || !self.closedAtBoundary || self.Context.Err() != context.Canceled) {
		t.Fatal("actual parent did not end after the captured live read at the owning callback boundary")
	}
	if !armed && self.Context.Err() != nil {
		t.Fatal("unarmed control ended its parent")
	}
}

func routerPairReadParent(signal Signal, armed bool) *routerPairReadContext {
	parent, cancel := context.WithCancel(context.Background())
	var ownerLock *sync.Mutex
	switch probe := signal.(*signalAdapter).probe.(type) {
	case *routerNeighborsProbe:
		ownerLock = &probe.stateLock
	case *routerConntrackProbe:
		ownerLock = &probe.stateLock
	default:
		panic("unexpected synthetic router pair owner")
	}
	return &routerPairReadContext{Context: parent, cancel: cancel, ownerLock: ownerLock, arm: armed}
}

func routerPairBody(kind string, active bool) string {
	if kind == "neighbors" {
		if active {
			return "192.0.2.1 dev eth0 used 1/300/2 probes 6 FAILED"
		}
		return "192.0.2.1 dev eth0 lladdr 02:00:00:00:00:01 used 1/1/1 probes 1 REACHABLE"
	}
	drop := "0"
	if active {
		drop = "a"
	}
	return "--count--\n100\n--max--\n1000\n--hash--\n256\n--stat--\nentries insert_failed drop early_drop\n64 0 " + drop + " 0"
}

func routerPairSignal(kind string) Signal {
	if kind == "neighbors" {
		return NewRouterNeighborsSignal()
	}
	return NewRouterConntrackSignal()
}

// Clone every mutable field, including nested desired neighbors, entries and
// per-CPU rows. An extra key proves a rejected frame leaves the whole map alone.
func routerPairCacheSnapshot(signal Signal, seedOther bool) any {
	cloneObservation := func(observation routerObservation) routerObservation {
		observation.summary.Topology.Neighbors = slices.Clone(observation.summary.Topology.Neighbors)
		return observation
	}
	switch probe := signal.(*signalAdapter).probe.(type) {
	case *routerNeighborsProbe:
		probe.stateLock.Lock()
		defer probe.stateLock.Unlock()
		if seedOther {
			probe.previous["synthetic-other-router"] = probe.previous[syntheticRouterName]
		}
		result := map[string]routerNeighborSample{}
		for key, sample := range probe.previous {
			sample.observation = cloneObservation(sample.observation)
			sample.entries = maps.Clone(sample.entries)
			result[key] = sample
		}
		return result
	case *routerConntrackProbe:
		probe.stateLock.Lock()
		defer probe.stateLock.Unlock()
		if seedOther {
			probe.previous["synthetic-other-router"] = probe.previous[syntheticRouterName]
		}
		result := map[string]routerConntrackSample{}
		for key, sample := range probe.previous {
			sample.observation = cloneObservation(sample.observation)
			sample.counters = slices.Clone(sample.counters)
			result[key] = sample
		}
		return result
	default:
		panic("unexpected synthetic router pair owner")
	}
}

func TestRouterNeighborsCanceledFrameCannotBecomePair(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = routerPairBody("neighbors", false)
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterNeighborsSignal()
	if alerts, err := signal.Run(context.Background(), settings); err != nil || len(alerts) != 0 {
		t.Fatalf("accepted healthy seed: alerts=%d error=%v", len(alerts), err)
	}
	before := routerPairCacheSnapshot(signal, true)
	now = now.Add(5 * time.Minute)
	source.body = routerPairBody("neighbors", true)
	parent := routerPairReadParent(signal, true)
	defer parent.cancel()
	alerts, err := signal.Run(parent, settings)
	parent.requireBoundary(t, true)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || source.hostCalls != 2 || source.localCalls != 4 {
		t.Fatalf("canceled complete frame: alerts=%d error=%v host_calls=%d local_calls=%d", len(alerts), err, source.hostCalls, source.localCalls)
	}
	if !reflect.DeepEqual(before, routerPairCacheSnapshot(signal, false)) {
		t.Fatal("canceled router neighbor frame changed accepted pair cache")
	}
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		if alert.Class == "router-neighbor-active-failure" {
			t.Fatal("canceled neighbor failure counted as an accepted predecessor")
		}
	}
	requireRouterPrivate(t, alerts)
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-neighbor-active-failure")
	requireRouterPrivate(t, alerts)
}

func TestRouterConntrackCanceledFrameCannotConsumeDelta(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = routerPairBody("conntrack", false)
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	before := routerPairCacheSnapshot(signal, true)
	now = now.Add(5 * time.Minute)
	source.body = routerPairBody("conntrack", true)
	parent := routerPairReadParent(signal, true)
	defer parent.cancel()
	alerts, err = signal.Run(parent, settings)
	parent.requireBoundary(t, true)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || source.hostCalls != 2 || source.localCalls != 4 {
		t.Fatalf("canceled complete frame: alerts=%d error=%v host_calls=%d local_calls=%d", len(alerts), err, source.hostCalls, source.localCalls)
	}
	if !reflect.DeepEqual(before, routerPairCacheSnapshot(signal, false)) {
		t.Fatal("canceled router conntrack frame changed accepted pair cache")
	}
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	drops := requireAlertClass(t, alerts, "router-conntrack-drops")
	if !strings.Contains(drops.Observed, "drop_delta=10 ") {
		t.Fatal("canceled counter frame consumed the accepted-only T0 to T2 delta")
	}
	requireRouterPrivate(t, alerts)
	// Independent public accepted-only T0/T2 control, with no canceled frame.
	controlNow := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	controlSource := newSyntheticRouterSource()
	controlSource.body = routerPairBody("conntrack", false)
	controlSettings := syntheticRouterSettings(controlSource, &controlNow)
	controlSignal := NewRouterConntrackSignal()
	if _, err := controlSignal.Run(context.Background(), controlSettings); err != nil {
		t.Fatal(err)
	}
	controlNow = now
	controlSource.body = routerPairBody("conntrack", true)
	controlAlerts, err := controlSignal.Run(context.Background(), controlSettings)
	if err != nil {
		t.Fatal(err)
	}
	controlDrops := requireAlertClass(t, controlAlerts, "router-conntrack-drops")
	if drops.Observed != controlDrops.Observed {
		t.Fatal("next accepted pair differs from the independent accepted-only delta control")
	}
	requireRouterPrivate(t, controlAlerts)
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("accepted stable zero-delta pair: alerts=%d error=%v", len(alerts), err)
	}
}

func TestRouterPairUnarmedReadControlAcceptsHealthyFrames(t *testing.T) {
	for _, kind := range []string{"neighbors", "conntrack"} {
		now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
		source := newSyntheticRouterSource()
		source.body = routerPairBody(kind, false)
		settings := syntheticRouterSettings(source, &now)
		signal := routerPairSignal(kind)
		if _, err := signal.Run(context.Background(), settings); err != nil {
			t.Fatalf("%s seed: %v", kind, err)
		}
		before := routerPairCacheSnapshot(signal, true)
		now = now.Add(5 * time.Minute)
		parent := routerPairReadParent(signal, false)
		alerts, err := signal.Run(parent, settings)
		parent.requireBoundary(t, false)
		parent.cancel()
		if err != nil || len(alerts) != 0 || reflect.DeepEqual(before, routerPairCacheSnapshot(signal, false)) {
			t.Fatalf("%s unarmed healthy frame not accepted: alerts=%d error=%v", kind, len(alerts), err)
		}
	}
}

func TestRouterPairCanceledErrorsDoNotEvictAcceptedCache(t *testing.T) {
	for _, kind := range []string{"neighbors", "conntrack"} {
		for _, fault := range []string{"capture", "parse"} {
			now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
			source := newSyntheticRouterSource()
			source.body = routerPairBody(kind, false)
			settings := syntheticRouterSettings(source, &now)
			signal := routerPairSignal(kind)
			if _, err := signal.Run(context.Background(), settings); err != nil {
				t.Fatalf("%s/%s seed: %v", kind, fault, err)
			}
			before := routerPairCacheSnapshot(signal, true)
			if fault == "capture" {
				source.err = context.DeadlineExceeded
			} else {
				source.body = "synthetic-secret invalid required scalar or neighbor row"
			}
			now = now.Add(5 * time.Minute)
			parent := routerPairReadParent(signal, true)
			alerts, err := signal.Run(parent, settings)
			parent.requireBoundary(t, true)
			parent.cancel()
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
				t.Fatalf("%s/%s ended parent published: alerts=%d error=%v", kind, fault, len(alerts), err)
			}
			if !reflect.DeepEqual(before, routerPairCacheSnapshot(signal, false)) {
				t.Fatalf("%s/%s canceled error evicted accepted pair cache", kind, fault)
			}
		}
	}
}

func TestRouterPairLiveChildAndParseFailuresStillResetPair(t *testing.T) {
	for _, kind := range []string{"neighbors", "conntrack"} {
		for _, fault := range []string{"capture", "parse"} {
			now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
			source := newSyntheticRouterSource()
			source.body = routerPairBody(kind, true)
			settings := syntheticRouterSettings(source, &now)
			signal := routerPairSignal(kind)
			if _, err := signal.Run(context.Background(), settings); err != nil {
				t.Fatalf("%s/%s seed: %v", kind, fault, err)
			}
			before := routerPairCacheSnapshot(signal, true)
			if fault == "capture" {
				source.err = context.DeadlineExceeded
			} else {
				source.body = "synthetic-secret invalid required scalar or neighbor row"
			}
			now = now.Add(5 * time.Minute)
			parent := routerPairReadParent(signal, false)
			alerts, err := signal.Run(parent, settings)
			parent.requireBoundary(t, false)
			parent.cancel()
			if err != nil {
				t.Fatalf("%s/%s live parent became lifecycle error: %v", kind, fault, err)
			}
			requireAlertClass(t, alerts, "cannot-observe")
			requireRouterPrivate(t, alerts)
			after := routerPairCacheSnapshot(signal, false)
			switch previous := after.(type) {
			case map[string]routerNeighborSample:
				if _, ok := previous[syntheticRouterName]; ok || !reflect.DeepEqual(previous["synthetic-other-router"], before.(map[string]routerNeighborSample)["synthetic-other-router"]) {
					t.Fatalf("%s/%s live failure did not reset only its own pair", kind, fault)
				}
			case map[string]routerConntrackSample:
				if _, ok := previous[syntheticRouterName]; ok || !reflect.DeepEqual(previous["synthetic-other-router"], before.(map[string]routerConntrackSample)["synthetic-other-router"]) {
					t.Fatalf("%s/%s live failure did not reset only its own pair", kind, fault)
				}
			}
			source.err = nil
			source.body = routerPairBody(kind, true)
			now = now.Add(5 * time.Minute)
			alerts, err = signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatalf("%s/%s next accepted frame: %v", kind, fault, err)
			}
			requireAlertClass(t, alerts, "cannot-observe")
			for _, alert := range alerts {
				if alert.Class == "router-neighbor-active-failure" || alert.Class == "router-conntrack-drops" {
					t.Fatalf("%s/%s failed live capture bridged a pair gap", kind, fault)
				}
			}
		}
	}
}

func TestRouterPairPreCanceledParentDoesNotContactOrChangeCache(t *testing.T) {
	for _, kind := range []string{"neighbors", "conntrack"} {
		now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
		source := newSyntheticRouterSource()
		source.body = routerPairBody(kind, false)
		settings := syntheticRouterSettings(source, &now)
		signal := routerPairSignal(kind)
		if _, err := signal.Run(context.Background(), settings); err != nil {
			t.Fatalf("%s seed: %v", kind, err)
		}
		before := routerPairCacheSnapshot(signal, true)
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		alerts, err := signal.Run(parent, settings)
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || source.hostCalls != 1 || source.localCalls != 2 || !reflect.DeepEqual(before, routerPairCacheSnapshot(signal, false)) {
			t.Fatalf("%s pre-canceled run contacted or changed cache: alerts=%d error=%v host_calls=%d local_calls=%d", kind, len(alerts), err, source.hostCalls, source.localCalls)
		}
	}
}

func TestRouterPairInFlightParentCancelStillRejectsBeforeEvaluation(t *testing.T) {
	for _, kind := range []string{"neighbors", "conntrack"} {
		now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
		source := newSyntheticRouterSource()
		source.body = routerPairBody(kind, false)
		settings := syntheticRouterSettings(source, &now)
		signal := routerPairSignal(kind)
		if _, err := signal.Run(context.Background(), settings); err != nil {
			t.Fatalf("%s seed: %v", kind, err)
		}
		before := routerPairCacheSnapshot(signal, true)
		parent, cancel := context.WithCancel(context.Background())
		generationCalls := 0
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
			generationCalls++
			if generationCalls == 2 {
				cancel()
			}
			return true, nil
		}
		now = now.Add(5 * time.Minute)
		alerts, err := signal.Run(parent, settings)
		cancel()
		if generationCalls != 2 || !errors.Is(err, context.Canceled) || len(alerts) != 0 || source.hostCalls != 2 || source.localCalls != 4 || !reflect.DeepEqual(before, routerPairCacheSnapshot(signal, false)) {
			t.Fatalf("%s source-final cancellation: generation_calls=%d alerts=%d error=%v host_calls=%d local_calls=%d", kind, generationCalls, len(alerts), err, source.hostCalls, source.localCalls)
		}
	}
}
