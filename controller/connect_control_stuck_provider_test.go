package controller

// Stuck provider reports (connect_control_stuck_provider.go): the run
// thresholds, the bound, the report results, the switch, and which replays
// count. The fake store and clock keep these off Redis; the last test runs
// the real control read and flag write.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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
	results   []stuckProviderTestResult
	hold      chan struct{}
	calls     chan server.Id
}

type stuckProviderTestResult struct {
	set bool
	err error
}

func newStuckProviderTestStore() *stuckProviderTestStore {
	return &stuckProviderTestStore{calls: make(chan server.Id, 64)}
}

func (self *stuckProviderTestStore) SetRecycle(ctx context.Context, clientId server.Id, cooldown time.Duration) (bool, error) {
	hold, result := func() (chan struct{}, stuckProviderTestResult) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.clientIds = append(self.clientIds, clientId)
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
			return false, ctx.Err()
		}
	}
	if _, ok := ctx.Deadline(); !ok || cooldown <= 0 {
		return false, errors.New("a report without a deadline or cooldown")
	}
	return result.set, result.err
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
		if signals.noteRetry(clientId, clock.Set(offset)) {
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

// A run is reported once it has lasted MinRetryDuration with at least
// MinRetryCount answers and no silence longer than MaxRetryGap, and then
// starts over.
func TestStuckProviderRunCrossesItsThresholds(t *testing.T) {
	signals, _, _, clock := newStuckProviderTestSignals(t, stuckProviderTestSettings(), "g1", map[string]string{"stuckProviderSignal": "g1"})

	cases := []struct {
		name    string
		offsets []time.Duration
		want    []time.Duration
	}{
		// a stuck route as one process sees it: at 10 min, then a new run
		{name: "a steady run", offsets: minutes(0, 3, 6, 9, 10, 11, 14, 17, 20, 21), want: minutes(10, 21)},
		// one contract wait's answers span 30 s, however many there are
		{name: "one burst", offsets: minutes(0, 0.1, 0.2, 0.3, 0.4, 0.45, 0.5), want: minutes()},
		// long enough but too few answers, until the fourth
		{name: "too few answers", offsets: minutes(0, 5, 10, 11), want: minutes(11)},
		// a silence longer than the gap starts the run over at 14
		{name: "a gap", offsets: minutes(0, 4, 8, 14, 18, 22, 24), want: minutes(24)},
		// a silence of exactly the gap continues the run
		{name: "exactly the gap", offsets: minutes(0, 5, 10, 10.5), want: minutes(10.5)},
		// bursts more than the gap apart never make a run
		{name: "sparse bursts", offsets: minutes(0, 0.2, 6, 6.2, 12, 12.2, 18, 18.2), want: minutes()},
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

	// kept's run was dropped too by now, as dropped's was: both start over
	if crossings := noteStuckProviderRetries(signals, clock, kept, minutes(4.5, 5, 6, 10)...); len(crossings) != 0 {
		t.Fatalf("a dropped run crossed at %v", crossings)
	}
	if crossings := noteStuckProviderRetries(signals, clock, dropped, minutes(4.5, 5, 6, 10)...); len(crossings) != 0 {
		t.Fatalf("a dropped run crossed at %v", crossings)
	}

	// a run answered within the turnovers keeps its start
	survivor := server.NewId()
	noteStuckProviderRetries(signals, clock, survivor, minutes(20, 22)...)
	for range 2 {
		noteStuckProviderRetries(signals, clock, server.NewId(), minutes(23)...)
	}
	noteStuckProviderRetries(signals, clock, server.NewId(), minutes(23)...)
	if crossings := noteStuckProviderRetries(signals, clock, survivor, minutes(25, 30)...); !sameOffsets(crossings, minutes(30)) {
		t.Fatalf("a surviving run crossed at %v, want 30m", crossings)
	}
}

// A crossing reports the client off the request. The results count by the
// flag write's outcome, and a report past MaxConcurrentSignals is dropped.
func TestStuckProviderReportsCountTheirResults(t *testing.T) {
	settings := stuckProviderTestSettings()
	settings.MaxConcurrentSignals = 1
	signals, _, store, clock := newStuckProviderTestSignals(t, settings, "g1", map[string]string{"stuckProviderSignal": "g1"})
	store.results = []stuckProviderTestResult{{set: true}, {set: false}, {err: errors.New("unavailable")}}

	cross := func(clientId server.Id, start float64) {
		for _, offset := range minutes(start, start+3, start+6, start+9, start+10) {
			clock.Set(offset)
			signals.observeTerminalRetry(clientId)
		}
	}
	waitFor := func(result string, want float64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for stuckProviderTestDecisions(t, result) < want {
			if time.Now().After(deadline) {
				t.Fatalf("%s decisions %v, want %v", result, stuckProviderTestDecisions(t, result), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	setBefore := stuckProviderTestDecisions(t, stuckProviderRecycleSet)
	cooldownBefore := stuckProviderTestDecisions(t, stuckProviderRecycleCooldown)
	errorBefore := stuckProviderTestDecisions(t, stuckProviderRecycleError)
	droppedBefore := stuckProviderTestDecisions(t, stuckProviderRecycleDropped)

	clientId := server.NewId()
	cross(clientId, 0)
	store.RequireCall(t, clientId)
	waitFor(stuckProviderRecycleSet, setBefore+1)
	// the next run of the same client finds the flag in its cooldown
	cross(clientId, 11)
	store.RequireCall(t, clientId)
	waitFor(stuckProviderRecycleCooldown, cooldownBefore+1)
	otherClientId := server.NewId()
	cross(otherClientId, 0)
	store.RequireCall(t, otherClientId)
	waitFor(stuckProviderRecycleError, errorBefore+1)

	// one report in flight: the next is dropped without a write
	hold := make(chan struct{})
	func() {
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		store.hold = hold
	}()
	heldClientId := server.NewId()
	cross(heldClientId, 30)
	store.RequireCall(t, heldClientId)
	droppedClientId := server.NewId()
	cross(droppedClientId, 30)
	waitFor(stuckProviderRecycleDropped, droppedBefore+1)
	close(hold)
	signals.Close()
	for _, called := range store.Calls() {
		if called == droppedClientId {
			t.Fatal("a dropped report wrote the flag")
		}
	}
	if len(store.Calls()) != 4 {
		t.Fatalf("%d writes, want 4", len(store.Calls()))
	}
}

// Off by default, off past the ceiling, off for another block, and turned off
// at run time: nothing is tracked or written. Turning off forgets every run,
// and a failed read keeps the last switch.
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
	signals, control, store, clock := newStuckProviderTestSignals(t, stuckProviderTestSettings(), "g1", map[string]string{"stuckProviderSignal": "g1"})
	if !signals.allows() || stuckProviderTestGauge(t, "urnetwork_connect_stuck_provider_signal_enabled") != 1 {
		t.Fatal("the named block is off")
	}
	reportedClientId := server.NewId()
	answerFor(signals, clock, reportedClientId)
	store.RequireCall(t, reportedClientId)

	// a failed read keeps the switch on
	control.Set(nil, errors.New("unavailable"))
	signals.refresh()
	if !signals.allows() {
		t.Fatal("a failed read turned the switch off")
	}

	// turned off mid-run: the run is forgotten, and once back on a client
	// starts from zero
	clientId := server.NewId()
	for _, offset := range minutes(30, 33, 36, 39) {
		clock.Set(offset)
		signals.observeTerminalRetry(clientId)
	}
	control.Set(map[string]string{"stuckProviderSignal": "off"}, nil)
	signals.refresh()
	if signals.allows() || stuckProviderTestGauge(t, "urnetwork_connect_stuck_provider_signal_enabled") != 0 {
		t.Fatal("the kill switch left the block on")
	}
	clock.Set(40 * time.Minute)
	signals.observeTerminalRetry(clientId)
	control.Set(map[string]string{"stuckProviderSignal": "g1"}, nil)
	signals.refresh()
	for _, offset := range minutes(41, 42, 44) {
		clock.Set(offset)
		signals.observeTerminalRetry(clientId)
	}
	if !waitForNoWrite(store) {
		t.Fatal("a run from before the kill switch was reported")
	}
	for _, offset := range minutes(47, 50, 51) {
		clock.Set(offset)
		signals.observeTerminalRetry(clientId)
	}
	store.RequireCall(t, clientId)
}

// Only a replay whose every refusal is terminal, for a pack none of whose
// frames carries a sender role, counts toward a run. A sender role on any
// frame, or any frame refused for a missing origin or a missing row, keeps
// the client out of the tracker; the answer is the same either way.
func TestReplayCountsOnlyTerminalRetriesWithoutSenderRole(t *testing.T) {
	ctx := newRefusalReplayOwnerContext(t)
	owner := model.GetContractOriginNotifications(ctx)
	signals, _, store, clock := newStuckProviderTestSignals(t, stuckProviderTestSettings(), "g1", map[string]string{"stuckProviderSignal": "g1"})
	ctx = WithStuckProviderSignals(ctx, signals)

	missingRow := refusalReplayInactive()
	missingRow.Relationship.DestinationLifecycle = model.NetworkClientLifecycleMissing
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
	addCase("one terminal frame", true, func(clientId server.Id) *ConnectControlArgs {
		retiredId := server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, false))
	})
	addCase("two terminal frames", true, func(clientId server.Id) *ConnectControlArgs {
		retiredId, otherRetiredId := server.NewId(), server.NewId()
		owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
		owner.RecordRouteRejection(clientId, otherRetiredId, true, refusalReplayInactive(), nil)
		return refusalReplayArgs(t, refusalReplayCreate(retiredId, false), refusalReplayCreate(otherRetiredId, true))
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

	for _, c := range cases {
		refusals, ok := PrepareConnectControlRefusals(ctx, c.clientId, c.args)
		if !ok {
			t.Fatalf("%s: not prepared", c.name)
		}
		if refusals.terminalWithoutSenderRole != c.eligible {
			t.Fatalf("%s: terminal without a sender role = %t, want %t", c.name, refusals.terminalWithoutSenderRole, c.eligible)
		}
	}

	// every client keeps retrying for 12 minutes, one replay a minute
	for minute := 0; minute <= 12; minute += 1 {
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
	for range 2 {
		select {
		case clientId := <-store.calls:
			reported[clientId] += 1
		case <-time.After(10 * time.Second):
			t.Fatalf("%d reports, want 2", len(reported))
		}
	}
	signals.Close()
	if len(store.Calls()) != 2 {
		t.Fatalf("%d reports, want 2", len(store.Calls()))
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
// client's flag once, with the cooldown as its ttl; a process of another
// block, and any process with the switch off, sets none.
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
			for _, result := range []string{stuckProviderRecycleSet, stuckProviderRecycleCooldown, stuckProviderRecycleError, stuckProviderRecycleDropped} {
				total += testutil.ToFloat64(stuckProviderRecycleDecisionsCounter.WithLabelValues(result))
			}
			return total
		}
		// a report that the owner decides on runs off the request: wait for
		// its result before the close, which cancels reports in flight
		answer := func(signals *StuckProviderSignals, clock *stuckProviderTestClock, clientId server.Id) {
			decisionsBefore := decisions()
			for _, offset := range minutes(0, 3, 6, 9, 10) {
				clock.Set(offset)
				signals.observeTerminalRetry(clientId)
			}
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
		answer(onSignals, onClock, clientId)
		if ttl := flagTtl(clientId); ttl <= 0 || settings.Cooldown < ttl {
			t.Fatalf("the named block's flag ttl %s, want (0, %s]", ttl, settings.Cooldown)
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
