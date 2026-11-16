package controller

// Stuck provider reports (connect_control_stuck_provider.go): the run
// thresholds, the bound, the report results and pacing, the switch, and which
// replays count. The fake store and clock keep these off Redis; the last test
// runs the real control read and flag write.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type stuckProviderTestClock struct {
	stateLock sync.Mutex
	start     time.Time
	now       time.Time
}

func newStuckProviderTestClock() *stuckProviderTestClock {
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	return &stuckProviderTestClock{start: start, now: start}
}

func (self *stuckProviderTestClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// Sets the clock to offset past the start.
func (self *stuckProviderTestClock) Set(offset time.Duration) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.start.Add(offset)
	return self.now
}

// The recycle flag writes a test owner made. A write waits on hold while it
// is set, and returns the next of results, or set once they run out.
type stuckProviderTestStore struct {
	stateLock sync.Mutex
	clientIds []server.Id
	ttls      []time.Duration
	results   []stuckProviderTestResult
	hold      chan struct{}
	calls     chan server.Id
}

// One write's answer: set, or the flag found with its set time, or err.
type stuckProviderTestResult struct {
	set      bool
	flagTime time.Time
	err      error
}

func newStuckProviderTestStore() *stuckProviderTestStore {
	return &stuckProviderTestStore{calls: make(chan server.Id, 64)}
}

func (self *stuckProviderTestStore) SetRecycle(ctx context.Context, clientId server.Id, ttl time.Duration) (bool, time.Time, error) {
	hold, result := func() (chan struct{}, stuckProviderTestResult) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.clientIds = append(self.clientIds, clientId)
		self.ttls = append(self.ttls, ttl)
		result := stuckProviderTestResult{set: true}
		if 0 < len(self.results) {
			result = self.results[0]
			self.results = self.results[1:]
		}
		return self.hold, result
	}()
	self.calls <- clientId
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return false, time.Time{}, ctx.Err()
		}
	}
	if _, ok := ctx.Deadline(); !ok || ttl <= 0 {
		return false, time.Time{}, errors.New("a report without a deadline or ttl")
	}
	return result.set, result.flagTime, result.err
}

func (self *stuckProviderTestStore) Ttls() []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Duration{}, self.ttls...)
}

func (self *stuckProviderTestStore) Calls() []server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]server.Id{}, self.clientIds...)
}

// Waits for the next write, which must be clientId's.
func (self *stuckProviderTestStore) RequireCall(t *testing.T, clientId server.Id) {
	t.Helper()
	select {
	case called := <-self.calls:
		if called != clientId {
			t.Fatalf("reported %s, want %s", called, clientId)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no report for %s", clientId)
	}
}

// The control fields a test reader answers with. A set err fails each read.
type stuckProviderTestControl struct {
	stateLock sync.Mutex
	fields    map[string]string
	err       error
}

func (self *stuckProviderTestControl) Set(fields map[string]string, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fields = fields
	self.err = err
}

func (self *stuckProviderTestControl) Read(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return nil, self.err
	}
	return model.NarrowConnectMoveControl(self.fields, ceiling), nil
}

func stuckProviderTestSettings() *StuckProviderSignalSettings {
	settings := DefaultStuckProviderSignalSettings()
	settings.EnableStuckProviderSignal = true
	return settings
}

// An owner for block whose control holds fields, read once.
func newStuckProviderTestSignals(
	t *testing.T,
	settings *StuckProviderSignalSettings,
	block string,
	fields map[string]string,
) (*StuckProviderSignals, *stuckProviderTestControl, *stuckProviderTestStore, *stuckProviderTestClock) {
	t.Helper()
	control := &stuckProviderTestControl{fields: fields}
	store := newStuckProviderTestStore()
	clock := newStuckProviderTestClock()
	signals := newStuckProviderSignalsWithStore(t.Context(), settings, block, control.Read, store.SetRecycle, clock.Now)
	t.Cleanup(signals.Close)
	signals.refresh()
	return signals, control, store, clock
}

func stuckProviderTestGauge(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			for _, metric := range family.GetMetric() {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func stuckProviderTestDecisions(t *testing.T, result string) float64 {
	t.Helper()
	return refusalReplayCounter(t, "urnetwork_connect_stuck_provider_recycle_decisions_total", map[string]string{"result": result})
}

// Answers clientId at each offset, in order, and returns the offsets at which
// its run crossed.
func noteStuckProviderRetries(signals *StuckProviderSignals, clock *stuckProviderTestClock, clientId server.Id, offsets ...time.Duration) []time.Duration {
	crossings := []time.Duration{}
	for _, offset := range offsets {
		if signals.noteRetry(clientId, clock.Set(offset).UnixNano()) {
			crossings = append(crossings, offset)
		}
	}
	return crossings
}

func minutes(values ...float64) []time.Duration {
	offsets := []time.Duration{}
	for _, value := range values {
		offsets = append(offsets, time.Duration(value*float64(time.Minute)))
	}
	return offsets
}

func sameOffsets(a []time.Duration, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The defaults the thresholds follow from (the file header): a run of 20 to
// 30 minutes, which no route an sdk stops asking for by itself spans, and two
// generations of at least 64Ki clients.
func TestStuckProviderDefaultsSpanTheRetryCadence(t *testing.T) {
	settings := DefaultStuckProviderSignalSettings()
	if settings.MinRetryDuration < 20*time.Minute || 30*time.Minute < settings.MinRetryDuration {
		t.Errorf("MinRetryDuration %s, want 20 to 30 min", settings.MinRetryDuration)
	}
	if settings.MinRetryCount < 6 {
		t.Errorf("MinRetryCount %d, want at least 6", settings.MinRetryCount)
	}
	if settings.TrackedClientCount < 65536 {
		t.Errorf("TrackedClientCount %d, want at least 65536", settings.TrackedClientCount)
	}
	if settings.ControlMaxFailedReads <= 0 {
		t.Errorf("ControlMaxFailedReads %d keeps an unreadable control on forever", settings.ControlMaxFailedReads)
	}
	if settings.SignalsPerSecond <= 0 || settings.SignalBurst <= 0 {
		t.Errorf("reports unpaced or closed: %v a second, burst %d", settings.SignalsPerSecond, settings.SignalBurst)
	}
	if settings.StillStuckAge < settings.MaxRetryGap || settings.MinRetryDuration < settings.StillStuckAge {
		t.Errorf("StillStuckAge %s, want between the gap %s and the run %s", settings.StillStuckAge, settings.MaxRetryGap, settings.MinRetryDuration)
	}
}

// A run is reported once it has lasted MinRetryDuration with at least
// MinRetryCount answers and no silence longer than MaxRetryGap, and then
// starts over. The default thresholds.
func TestStuckProviderRunCrossesItsThresholds(t *testing.T) {
	signals, _, _, clock := newStuckProviderTestSignals(t, stuckProviderTestSettings(), "g1", map[string]string{"stuckProviderSignal": "g1"})

	everyMinute := []float64{}
	for minute := 0; minute <= 15; minute += 1 {
		everyMinute = append(everyMinute, float64(minute))
	}
	cases := []struct {
		name    string
		offsets []time.Duration
		want    []time.Duration
	}{
		// a stuck route as one process sees it: at 20 min, then a new run
		{name: "a steady run", offsets: minutes(0, 3, 6, 9, 12, 15, 18, 20, 21, 24, 27, 30, 33, 36, 39, 41), want: minutes(20, 41)},
		// a quarter hour of answers is not a run
		{name: "fifteen minutes", offsets: minutes(everyMinute...), want: minutes()},
		// one contract wait's answers span 30 s, however many there are
		{name: "one burst", offsets: minutes(0, 0.1, 0.2, 0.3, 0.4, 0.45, 0.5), want: minutes()},
		// long enough but too few answers, until the sixth
		{name: "too few answers", offsets: minutes(0, 5, 10, 15, 20, 21), want: minutes(21)},
		// a silence longer than the gap starts the run over at 14
		{name: "a gap", offsets: minutes(0, 4, 8, 14, 18, 22, 26, 30, 33, 34), want: minutes(34)},
		// a silence of exactly the gap continues the run
		{name: "exactly the gap", offsets: minutes(0, 5, 10, 15, 20, 20.5), want: minutes(20.5)},
		// bursts more than the gap apart never make a run
		{name: "sparse bursts", offsets: minutes(0, 0.2, 6, 6.2, 12, 12.2, 18, 18.2, 24, 24.2), want: minutes()},
	}
	for _, c := range cases {
		crossings := noteStuckProviderRetries(signals, clock, server.NewId(), c.offsets...)
		if !sameOffsets(crossings, c.want) {
			t.Errorf("%s: crossed at %v, want %v", c.name, crossings, c.want)
		}
	}
}

// At most two generations of TrackedClientCount runs are held. A run answered
// again moves into the current generation and survives a turnover; a run
// left in a dropped generation starts over.
func TestStuckProviderTrackerIsBounded(t *testing.T) {
	settings := stuckProviderTestSettings()
	settings.TrackedClientCount = 3
	signals, _, _, clock := newStuckProviderTestSignals(t, settings, "g1", map[string]string{"stuckProviderSignal": "all"})

	kept, dropped := server.NewId(), server.NewId()
	noteStuckProviderRetries(signals, clock, kept, minutes(0)...)
	noteStuckProviderRetries(signals, clock, dropped, minutes(0)...)
	for range 2 {
		noteStuckProviderRetries(signals, clock, server.NewId(), minutes(0)...)
	}
	// the turnover: kept and dropped are now in the previous generation
	noteStuckProviderRetries(signals, clock, kept, minutes(3)...)
	for range 4 {
		noteStuckProviderRetries(signals, clock, server.NewId(), minutes(3)...)
	}
	for range 100 {
		noteStuckProviderRetries(signals, clock, server.NewId(), minutes(4)...)
		held := func() int {
			signals.stateLock.Lock()
			defer signals.stateLock.Unlock()
			return len(signals.currentClientRetries) + len(signals.previousClientRetries)
		}()
		if 2*settings.TrackedClientCount < held {
			t.Fatalf("held %d runs, want at most %d", held, 2*settings.TrackedClientCount)
		}
		if gauge := stuckProviderTestGauge(t, "urnetwork_connect_stuck_provider_tracked_clients"); gauge != float64(held) {
			t.Fatalf("tracked gauge %v, held %d", gauge, held)
		}
	}

	// kept's run was dropped too by now, as dropped's was: both start over,
	// where a kept start would cross at 20
	if crossings := noteStuckProviderRetries(signals, clock, kept, minutes(4.5, 5, 6, 10, 15, 20)...); len(crossings) != 0 {
		t.Fatalf("a dropped run crossed at %v", crossings)
	}
	if crossings := noteStuckProviderRetries(signals, clock, dropped, minutes(4.5, 5, 6, 10, 15, 20)...); len(crossings) != 0 {
		t.Fatalf("a dropped run crossed at %v", crossings)
	}

	// a run answered within the turnovers keeps its start: three clients
	// after it turn the generations over at most once
	survivor := server.NewId()
	noteStuckProviderRetries(signals, clock, survivor, minutes(21, 23)...)
	for range 3 {
		noteStuckProviderRetries(signals, clock, server.NewId(), minutes(24)...)
	}
	if crossings := noteStuckProviderRetries(signals, clock, survivor, minutes(26, 30, 35, 40, 41)...); !sameOffsets(crossings, minutes(41)) {
		t.Fatalf("a surviving run crossed at %v, want 41m", crossings)
	}
}

// Answers clientId through a run that crosses at start+20 min.
func crossStuckProviderRun(signals *StuckProviderSignals, clock *stuckProviderTestClock, clientId server.Id, start float64) {
	for _, offset := range minutes(start, start+4, start+8, start+12, start+16, start+20) {
		clock.Set(offset)
		signals.observeTerminalRetry(clientId)
	}
}

func waitForStuckProviderDecisions(t *testing.T, result string, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for stuckProviderTestDecisions(t, result) < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s decisions %v, want %v", result, stuckProviderTestDecisions(t, result), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A crossing reports the client off the request. The results count by the
// flag write's outcome: a flag found long after it was set is a client still
// stuck after its recycle, one found soon after or unreadable a cooldown. A
// report past MaxConcurrentSignals is dropped. Every flag's ttl is the
// cooldown and up to a quarter more.
func TestStuckProviderReportsCountTheirResults(t *testing.T) {
	settings := stuckProviderTestSettings()
	settings.MaxConcurrentSignals = 1
	signals, _, store, clock := newStuckProviderTestSignals(t, settings, "g1", map[string]string{"stuckProviderSignal": "g1"})
	// the flag found 1 min, 30 min, and an unreadable time after it was set,
	// as of each report's crossing
	store.results = []stuckProviderTestResult{
		{set: true},
		{flagTime: clock.start.Add(41*time.Minute - 1*time.Minute)},
		{flagTime: clock.start.Add(62*time.Minute - 30*time.Minute)},
		{},
		{err: errors.New("unavailable")},
	}

	before := map[string]float64{}
	for _, result := range []string{stuckProviderRecycleSet, stuckProviderRecycleCooldown, stuckProviderRecycleStillStuck, stuckProviderRecycleError, stuckProviderRecycleDropped} {
		before[result] = stuckProviderTestDecisions(t, result)
	}

	clientId := server.NewId()
	crossStuckProviderRun(signals, clock, clientId, 0)
	store.RequireCall(t, clientId)
	waitForStuckProviderDecisions(t, stuckProviderRecycleSet, before[stuckProviderRecycleSet]+1)
	// the next runs of the same client find the flag: a minute old, a
	// cooldown; half an hour old, still stuck; unreadable, a cooldown
	crossStuckProviderRun(signals, clock, clientId, 21)
	store.RequireCall(t, clientId)
	waitForStuckProviderDecisions(t, stuckProviderRecycleCooldown, before[stuckProviderRecycleCooldown]+1)
	crossStuckProviderRun(signals, clock, clientId, 42)
	store.RequireCall(t, clientId)
	waitForStuckProviderDecisions(t, stuckProviderRecycleStillStuck, before[stuckProviderRecycleStillStuck]+1)
	crossStuckProviderRun(signals, clock, clientId, 63)
	store.RequireCall(t, clientId)
	waitForStuckProviderDecisions(t, stuckProviderRecycleCooldown, before[stuckProviderRecycleCooldown]+2)
	otherClientId := server.NewId()
	crossStuckProviderRun(signals, clock, otherClientId, 0)
	store.RequireCall(t, otherClientId)
	waitForStuckProviderDecisions(t, stuckProviderRecycleError, before[stuckProviderRecycleError]+1)

	// one report in flight: the next is dropped without a write
	hold := make(chan struct{})
	func() {
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		store.hold = hold
	}()
	heldClientId := server.NewId()
	crossStuckProviderRun(signals, clock, heldClientId, 100)
	store.RequireCall(t, heldClientId)
	droppedClientId := server.NewId()
	crossStuckProviderRun(signals, clock, droppedClientId, 100)
	waitForStuckProviderDecisions(t, stuckProviderRecycleDropped, before[stuckProviderRecycleDropped]+1)
	close(hold)
	signals.Close()
	for _, called := range store.Calls() {
		if called == droppedClientId {
			t.Fatal("a dropped report wrote the flag")
		}
	}
	if len(store.Calls()) != 6 {
		t.Fatalf("%d writes, want 6", len(store.Calls()))
	}
	for _, ttl := range store.Ttls() {
		if ttl < settings.Cooldown || settings.Cooldown+settings.Cooldown/4 <= ttl {
			t.Fatalf("a flag ttl %s, want [%s, %s)", ttl, settings.Cooldown, settings.Cooldown+settings.Cooldown/4)
		}
	}
}

// A flag's ttl is the cooldown and a random part of up to a quarter of it,
// so the flags of clients reported together expire apart.
func TestStuckProviderFlagTtlSpreadsOverAQuarterOfTheCooldown(t *testing.T) {
	settings := stuckProviderTestSettings()
	signals, _, _, _ := newStuckProviderTestSignals(t, settings, "g1", map[string]string{"stuckProviderSignal": "g1"})
	cooldown := settings.Cooldown
	signals.randInt64N = func(n int64) int64 {
		if n != int64(cooldown/4) {
			t.Fatalf("drew from [0, %d), want [0, %d)", n, int64(cooldown/4))
		}
		return 0
	}
	if ttl := signals.flagTtl(); ttl != cooldown {
		t.Fatalf("the least ttl %s, want %s", ttl, cooldown)
	}
	signals.randInt64N = func(n int64) int64 { return n - 1 }
	if ttl := signals.flagTtl(); ttl != cooldown+cooldown/4-1 {
		t.Fatalf("the most ttl %s, want %s", ttl, cooldown+cooldown/4-1)
	}
	signals.randInt64N = rand.Int64N
	distinct := map[time.Duration]bool{}
	for range 100 {
		ttl := signals.flagTtl()
		if ttl < cooldown || cooldown+cooldown/4 <= ttl {
			t.Fatalf("a ttl %s outside [%s, %s)", ttl, cooldown, cooldown+cooldown/4)
		}
		distinct[ttl] = true
	}
	if len(distinct) < 90 {
		t.Fatalf("%d distinct ttls of 100", len(distinct))
	}
}

// Reports are paced by the token bucket: past its burst a crossing counts as
// rate_limited without a write, its client starts a new run, and once a token
// has refilled the client's next crossing is reported.
func TestStuckProviderReportsArePaced(t *testing.T) {
	settings := stuckProviderTestSettings()
	// one token an hour, two at once
	settings.SignalsPerSecond = 1.0 / 3600
	settings.SignalBurst = 2
	signals, _, store, clock := newStuckProviderTestSignals(t, settings, "g1", map[string]string{"stuckProviderSignal": "g1"})
	rateLimitedBefore := stuckProviderTestDecisions(t, stuckProviderRecycleRateLimited)
	setBefore := stuckProviderTestDecisions(t, stuckProviderRecycleSet)

	first, second, paced := server.NewId(), server.NewId(), server.NewId()
	crossStuckProviderRun(signals, clock, first, 0)
	store.RequireCall(t, first)
	crossStuckProviderRun(signals, clock, second, 0)
	store.RequireCall(t, second)
	// the third at the same minute is past the burst
	crossStuckProviderRun(signals, clock, paced, 0)
	waitForStuckProviderDecisions(t, stuckProviderRecycleRateLimited, rateLimitedBefore+1)
	// its run started over: 15 more minutes do not cross
	for _, offset := range minutes(21, 25, 29, 33, 36) {
		clock.Set(offset)
		signals.observeTerminalRetry(paced)
	}
	if rateLimited := stuckProviderTestDecisions(t, stuckProviderRecycleRateLimited); rateLimited != rateLimitedBefore+1 {
		t.Fatalf("a run that started over crossed: rate limited +%v", rateLimited-rateLimitedBefore)
	}
	// an hour after the burst a token has refilled
	crossStuckProviderRun(signals, clock, paced, 60)
	store.RequireCall(t, paced)
	waitForStuckProviderDecisions(t, stuckProviderRecycleSet, setBefore+3)
	signals.Close()
	if len(store.Calls()) != 3 {
		t.Fatalf("%d writes, want 3", len(store.Calls()))
	}
}

// Off by default, off past the ceiling, off for another block, and turned off
// at run time: nothing is tracked or written. Turning off forgets every run.
// A failed read keeps the last switch until ControlMaxFailedReads fail in a
// row, which turn it off.
func TestStuckProviderSwitchGatesReports(t *testing.T) {
	answerFor := func(signals *StuckProviderSignals, clock *stuckProviderTestClock, clientId server.Id) {
		for _, offset := range minutes(0, 3, 6, 9, 10, 12, 15, 18, 20) {
			clock.Set(offset)
			signals.observeTerminalRetry(clientId)
		}
	}
	waitForNoWrite := func(store *stuckProviderTestStore) bool {
		select {
		case <-store.calls:
			return false
		case <-time.After(200 * time.Millisecond):
			return true
		}
	}

	// the ceiling off: no owner, and a nil owner does nothing
	if signals := NewStuckProviderSignals(t.Context(), DefaultStuckProviderSignalSettings(), "g1"); signals != nil {
		t.Fatal("an owner was made with the ceiling off")
	}
	var none *StuckProviderSignals
	none.observeTerminalRetry(server.NewId())
	none.Close()

	for _, c := range []struct {
		name   string
		block  string
		fields map[string]string
	}{
		{name: "the default", block: "g1", fields: nil},
		{name: "the kill switch", block: "g1", fields: map[string]string{"stuckProviderSignal": "off"}},
		{name: "another block", block: "g2", fields: map[string]string{"stuckProviderSignal": "g1"}},
		{name: "the recycle field", block: "g1", fields: map[string]string{"stuckProviderRecycle": "all"}},
	} {
		signals, _, store, clock := newStuckProviderTestSignals(t, stuckProviderTestSettings(), c.block, c.fields)
		answerFor(signals, clock, server.NewId())
		if signals.allows() || !waitForNoWrite(store) {
			t.Fatalf("%s: reported with the switch off", c.name)
		}
		held := func() int {
			signals.stateLock.Lock()
			defer signals.stateLock.Unlock()
			return len(signals.currentClientRetries) + len(signals.previousClientRetries)
		}()
		if held != 0 {
			t.Fatalf("%s: tracked %d runs with the switch off", c.name, held)
		}
	}

	// one block on: it reports
	settings := stuckProviderTestSettings()
	signals, control, store, clock := newStuckProviderTestSignals(t, settings, "g1", map[string]string{"stuckProviderSignal": "g1"})
	if !signals.allows() || stuckProviderTestGauge(t, "urnetwork_connect_stuck_provider_signal_enabled") != 1 {
		t.Fatal("the named block is off")
	}
	reportedClientId := server.NewId()
	answerFor(signals, clock, reportedClientId)
	store.RequireCall(t, reportedClientId)

	// failed reads keep the switch on until the limit fail in a row, and a
	// good read starts the count over
	unavailable := errors.New("unavailable")
	failReads := func(count int) {
		control.Set(nil, unavailable)
		for range count {
			signals.refresh()
		}
	}
	failReads(settings.ControlMaxFailedReads - 1)
	if !signals.allows() {
		t.Fatal("fewer failed reads than the limit turned the switch off")
	}
	control.Set(map[string]string{"stuckProviderSignal": "g1"}, nil)
	signals.refresh()
	failReads(settings.ControlMaxFailedReads - 1)
	if !signals.allows() {
		t.Fatal("failed reads on either side of a good one turned the switch off")
	}
	// a run under way when the control becomes unreadable is forgotten
	unreadableClientId := server.NewId()
	for _, offset := range minutes(21, 25, 29, 33) {
		clock.Set(offset)
		signals.observeTerminalRetry(unreadableClientId)
	}
	failReads(1)
	if signals.allows() || stuckProviderTestGauge(t, "urnetwork_connect_stuck_provider_signal_enabled") != 0 {
		t.Fatal("an unreadable control kept the switch on")
	}
	for _, offset := range minutes(34, 35) {
		clock.Set(offset)
		signals.observeTerminalRetry(unreadableClientId)
	}
	control.Set(map[string]string{"stuckProviderSignal": "g1"}, nil)
	signals.refresh()
	if !signals.allows() {
		t.Fatal("a good read did not turn the switch back on")
	}
	// a run kept from 21 would cross at 41
	for _, offset := range minutes(36, 38, 41) {
		clock.Set(offset)
		signals.observeTerminalRetry(unreadableClientId)
	}
	if !waitForNoWrite(store) {
		t.Fatal("a run from before the unreadable control was reported")
	}

	// turned off mid-run: the run is forgotten, and once back on a client
	// starts from zero
	clientId := server.NewId()
	for _, offset := range minutes(60, 63, 66, 69) {
		clock.Set(offset)
		signals.observeTerminalRetry(clientId)
	}
	control.Set(map[string]string{"stuckProviderSignal": "off"}, nil)
	signals.refresh()
	if signals.allows() || stuckProviderTestGauge(t, "urnetwork_connect_stuck_provider_signal_enabled") != 0 {
		t.Fatal("the kill switch left the block on")
	}
	clock.Set(70 * time.Minute)
	signals.observeTerminalRetry(clientId)
	control.Set(map[string]string{"stuckProviderSignal": "g1"}, nil)
	signals.refresh()
	// a run kept from 60 would cross at 80
	for _, offset := range minutes(71, 72, 74, 77, 80) {
		clock.Set(offset)
		signals.observeTerminalRetry(clientId)
	}
	if !waitForNoWrite(store) {
		t.Fatal("a run from before the kill switch was reported")
	}
	for _, offset := range minutes(83, 86, 89, 91) {
		clock.Set(offset)
		signals.observeTerminalRetry(clientId)
	}
	store.RequireCall(t, clientId)
}

// Only a replay whose every refusal is terminal and was refused to a
// top-level source, for a pack none of whose frames carries a sender role,
// counts toward a run. The production cohort is the non-companion request of
// an old provider to a removed window client, so a companion flag is not
// required. A sender role on any frame, a source that is a derived client
// (the window client of an old consumer retrying a removed provider) or
// inactive or missing, or any frame refused for a missing origin or a missing
// row, keeps the client out of the tracker; the answer is the same either way.
func TestReplayCountsOnlyTerminalRetriesOfTopLevelSourcesWithoutSenderRole(t *testing.T) {
	ctx := newRefusalReplayOwnerContext(t)
	owner := model.GetContractOriginNotifications(ctx)
	signals, _, store, clock := newStuckProviderTestSignals(t, stuckProviderTestSettings(), "g1", map[string]string{"stuckProviderSignal": "g1"})
	ctx = WithStuckProviderSignals(ctx, signals)

	missingRow := refusalReplayInactive()
	missingRow.Relationship.DestinationLifecycle = model.NetworkClientLifecycleMissing
	inactiveFrom := func(sourceLifecycle model.NetworkClientLifecycle) model.ContractRouteRejection {
		rejection := refusalReplayInactive()
		rejection.Relationship.SourceLifecycle = sourceLifecycle
		return rejection
	}
	withRole := func(destinationId server.Id, role protocol.SequenceRole) *protocol.CreateContract {
		createContract := refusalReplayCreate(destinationId, false)
		createContract.SenderRole = &role
		return createContract
	}

	type replayCase struct {
		name     string
		clientId server.Id
		args     *ConnectControlArgs
		eligible bool
	}
	cases := []replayCase{}
	addCase := func(name string, eligible bool, build func(clientId server.Id) *ConnectControlArgs) {
		clientId := server.NewId()
		cases = append(cases, replayCase{name: name, clientId: clientId, args: build(clientId), eligible: eligible})
	}
	addCase("one terminal frame of a top-level source", true, func(clientId server.Id) *ConnectControlArgs {
		retiredId := server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, false))
	})
	addCase("a companion terminal frame of a top-level source", true, func(clientId server.Id) *ConnectControlArgs {
		retiredId := server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, true, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, true))
	})
	addCase("two terminal frames", true, func(clientId server.Id) *ConnectControlArgs {
		retiredId, otherRetiredId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		owner.RecordRouteRejection(clientId, otherRetiredId, true, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, false), refusalReplayCreate(otherRetiredId, true))
	})
	for _, sourceLifecycle := range []model.NetworkClientLifecycle{
		model.NetworkClientLifecycleActiveDerived,
		model.NetworkClientLifecycleInactiveTop,
		model.NetworkClientLifecycleInactiveDerived,
		model.NetworkClientLifecycleMissing,
	} {
		addCase(fmt.Sprintf("a terminal frame of a source %s", sourceLifecycle), false, func(clientId server.Id) *ConnectControlArgs {
			retiredId := server.NewId()
			owner.RecordRouteRejection(clientId, retiredId, false, inactiveFrom(sourceLifecycle), nil)
			return refusalReplayArgs(t, refusalReplayCreate(retiredId, false))
		})
	}
	addCase("a derived source on one frame of two", false, func(clientId server.Id) *ConnectControlArgs {
		retiredId, otherRetiredId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		owner.RecordRouteRejection(clientId, otherRetiredId, false, inactiveFrom(model.NetworkClientLifecycleActiveDerived), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, false), refusalReplayCreate(otherRetiredId, false))
	})
	addCase("a server sender role", false, func(clientId server.Id) *ConnectControlArgs {
		retiredId := server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, withRole(retiredId, protocol.SequenceRole_SequenceRoleServer))
	})
	addCase("a client sender role", false, func(clientId server.Id) *ConnectControlArgs {
		retiredId := server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, withRole(retiredId, protocol.SequenceRole_SequenceRoleClient))
	})
	addCase("a sender role on one frame of two", false, func(clientId server.Id) *ConnectControlArgs {
		retiredId, otherRetiredId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		owner.RecordRouteRejection(clientId, otherRetiredId, false, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, false), withRole(otherRetiredId, protocol.SequenceRole_SequenceRoleServer))
	})
	addCase("a missing origin", false, func(clientId server.Id) *ConnectControlArgs {
		windowId := server.NewId()
		owner.RecordRouteRejection(clientId, windowId, false, refusalReplayMissingOrigin(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(windowId, false))
	})
	addCase("a terminal frame with a missing origin", false, func(clientId server.Id) *ConnectControlArgs {
		retiredId, windowId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		owner.RecordRouteRejection(clientId, windowId, false, refusalReplayMissingOrigin(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, false), refusalReplayCreate(windowId, false))
	})
	addCase("a missing destination row", false, func(clientId server.Id) *ConnectControlArgs {
		deletedId := server.NewId()
		owner.RecordRouteRejection(clientId, deletedId, false, missingRow, nil)
		return refusalReplayArgs(t, refusalReplayCreate(deletedId, false))
	})

	eligibleCount := 0
	for _, c := range cases {
		refusals, ok := PrepareConnectControlRefusals(ctx, c.clientId, c.args)
		if !ok {
			t.Fatalf("%s: not prepared", c.name)
		}
		if refusals.stuckProviderRetry != c.eligible {
			t.Fatalf("%s: a stuck provider retry = %t, want %t", c.name, refusals.stuckProviderRetry, c.eligible)
		}
		if c.eligible {
			eligibleCount += 1
		}
	}

	// every client keeps retrying for 22 minutes, one replay every 2
	for minute := 0; minute <= 22; minute += 2 {
		clock.Set(time.Duration(minute) * time.Minute)
		for _, c := range cases {
			refusals, ok := PrepareConnectControlRefusals(ctx, c.clientId, c.args)
			if !ok {
				t.Fatalf("%s: not prepared at %d min", c.name, minute)
			}
			result, err := ReplayConnectControlRefusals(ctx, refusals)
			if err != nil || result.Error != nil {
				t.Fatalf("%s: replay failed at %d min: err=%v result=%+v", c.name, minute, err, result)
			}
		}
	}
	reported := map[server.Id]int{}
	for range eligibleCount {
		select {
		case clientId := <-store.calls:
			reported[clientId] += 1
		case <-time.After(10 * time.Second):
			t.Fatalf("%d reports, want %d", len(reported), eligibleCount)
		}
	}
	signals.Close()
	if len(store.Calls()) != eligibleCount {
		t.Fatalf("%d reports, want %d", len(store.Calls()), eligibleCount)
	}
	for _, c := range cases {
		if c.eligible && reported[c.clientId] != 1 {
			t.Fatalf("%s: reported %d times, want once", c.name, reported[c.clientId])
		}
		if !c.eligible && reported[c.clientId] != 0 {
			t.Fatalf("%s: reported %d times, want never", c.name, reported[c.clientId])
		}
	}
}

// With the real control and flag: an api process of the named block sets the
// client's flag once, with the cooldown and up to a quarter more as its ttl,
// and a second process's report finds it; a process of another block, and any
// process with the switch off, sets none.
func TestStuckProviderSignalSetsTheFlagOnlyOnItsBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		settings := stuckProviderTestSettings()
		settings.ControlRefreshInterval = 0
		newSignals := func(block string) (*StuckProviderSignals, *stuckProviderTestClock) {
			clock := newStuckProviderTestClock()
			signals := newStuckProviderSignalsWithStore(ctx, settings, block, model.GetConnectMoveControl, model.SetClientRecycle, clock.Now)
			signals.refresh()
			return signals, clock
		}
		decisions := func() float64 {
			total := float64(0)
			for _, result := range []string{stuckProviderRecycleSet, stuckProviderRecycleCooldown, stuckProviderRecycleStillStuck, stuckProviderRecycleError, stuckProviderRecycleRateLimited, stuckProviderRecycleDropped} {
				total += testutil.ToFloat64(stuckProviderRecycleDecisionsCounter.WithLabelValues(result))
			}
			return total
		}
		// a report that the owner decides on runs off the request: wait for
		// its result before the close, which cancels reports in flight
		answer := func(signals *StuckProviderSignals, clock *stuckProviderTestClock, clientId server.Id) {
			decisionsBefore := decisions()
			crossStuckProviderRun(signals, clock, clientId, 0)
			deadline := time.Now().Add(2 * time.Second)
			for decisions() == decisionsBefore && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			signals.Close()
		}
		flagTtl := func(clientId server.Id) time.Duration {
			var ttl time.Duration
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				ttl, err = r.PTTL(ctx, "{"+clientId.String()+"}c_recycle").Result()
				server.Raise(err)
			})
			return ttl
		}

		// absent control: dark
		darkClientId := server.NewId()
		darkSignals, darkClock := newSignals("g1")
		answer(darkSignals, darkClock, darkClientId)
		if 0 < flagTtl(darkClientId) {
			t.Fatal("the default set a flag")
		}

		if err := model.SetConnectMoveControl(ctx, map[string]string{model.ConnectMoveControlFieldStuckProviderSignal: "g1"}); err != nil {
			t.Fatal(err)
		}
		clientId := server.NewId()
		onSignals, onClock := newSignals("g1")
		setBefore := testutil.ToFloat64(stuckProviderRecycleDecisionsCounter.WithLabelValues(stuckProviderRecycleSet))
		answer(onSignals, onClock, clientId)
		if set := testutil.ToFloat64(stuckProviderRecycleDecisionsCounter.WithLabelValues(stuckProviderRecycleSet)); set != setBefore+1 {
			t.Fatalf("the named block's report set +%v flags, want +1", set-setBefore)
		}
		if ttl := flagTtl(clientId); ttl <= 0 || settings.Cooldown+settings.Cooldown/4 < ttl {
			t.Fatalf("the named block's flag ttl %s, want (0, %s]", ttl, settings.Cooldown+settings.Cooldown/4)
		}
		// another process of the block reports the client: it finds the
		// flag, and reads back its set time (the age is the fake store's test)
		found := func() float64 {
			return testutil.ToFloat64(stuckProviderRecycleDecisionsCounter.WithLabelValues(stuckProviderRecycleCooldown)) +
				testutil.ToFloat64(stuckProviderRecycleDecisionsCounter.WithLabelValues(stuckProviderRecycleStillStuck))
		}
		foundBefore := found()
		secondSignals, secondClock := newSignals("g1")
		answer(secondSignals, secondClock, clientId)
		if foundAfter := found(); foundAfter != foundBefore+1 {
			t.Fatalf("a second report found +%v flags, want +1", foundAfter-foundBefore)
		}
		otherBlockClientId := server.NewId()
		otherSignals, otherClock := newSignals("g2")
		answer(otherSignals, otherClock, otherBlockClientId)
		if 0 < flagTtl(otherBlockClientId) {
			t.Fatal("another block set a flag")
		}

		if err := model.SetConnectMoveControl(ctx, map[string]string{model.ConnectMoveControlFieldStuckProviderSignal: "off"}); err != nil {
			t.Fatal(err)
		}
		offClientId := server.NewId()
		offSignals, offClock := newSignals("g1")
		answer(offSignals, offClock, offClientId)
		if 0 < flagTtl(offClientId) {
			t.Fatal("the kill switch set a flag")
		}
	})
}
