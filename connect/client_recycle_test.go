package connect

// Stuck provider recycles (client_recycle.go): a recycle flag the api sets
// reaches the exchange as a key event, and only an exchange whose block the
// switch names closes the client's transports with the moved close and
// retires its resident.

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	connectlib "github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func stuckProviderRecycleTestSettings() *ExchangeSettings {
	settings := DefaultExchangeSettings()
	settings.EnableStuckProviderRecycle = true
	settings.EnableMove = false
	return settings
}

// The Redis control in memory, narrowed as the model narrows the real one.
type stuckProviderRecycleTestControl struct {
	stateLock sync.Mutex
	fields    map[string]string
	err       error
}

// Sets the control's `stuckProviderRecycle`.
func (self *stuckProviderRecycleTestControl) set(value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fields = map[string]string{model.ConnectMoveControlFieldStuckProviderRecycle: value}
	self.err = nil
}

// Makes every read fail.
func (self *stuckProviderRecycleTestControl) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

func (self *stuckProviderRecycleTestControl) read(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return nil, self.err
	}
	return model.NarrowConnectMoveControl(self.fields, ceiling), nil
}

// The connect service allows recycles for the control to choose by block, and
// ships them off: with no control no block recycles. Every other exchange
// keeps the ceiling off.
func TestStuckProviderRecycleShipsDark(t *testing.T) {
	defaults := DefaultExchangeSettings()
	if defaults.EnableStuckProviderRecycle {
		t.Error("the default exchange settings allow stuck provider recycles")
	}
	runSettings := exchangeSettingsForRun(RunOptions{Port: 443})
	if !runSettings.EnableStuckProviderRecycle {
		t.Error("the connect service does not allow stuck provider recycles")
	}
	if runSettings.StuckProviderRecycleDefault != model.ConnectLocalTransportOff {
		t.Errorf("the connect service's default = %q, want off", runSettings.StuckProviderRecycleDefault)
	}
	control := model.NarrowConnectMoveControl(nil, model.ConnectMoveControlCeiling{
		EnableStuckProviderRecycle:  runSettings.EnableStuckProviderRecycle,
		StuckProviderRecycleDefault: runSettings.StuckProviderRecycleDefault,
	})
	for _, block := range []string{"beta", "g1", "g2", "g3", "g4"} {
		if control.StuckProviderRecycleAllows(block) {
			t.Errorf("block %s recycles with no control", block)
		}
	}
	if runSettings.StuckProviderRecycleControlRefreshInterval != 10*time.Second {
		t.Errorf("refresh %s, want 10s", runSettings.StuckProviderRecycleControlRefreshInterval)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if newExchangeStuckProviderRecycle(ctx, defaults, "g1") != nil {
		t.Error("the ceiling off still built a switch that reads the control")
	}
}

// Only an exchange with the recycle ceiling on subscribes the recycle flags'
// channel: every pattern is matched against every keyspace notification on
// every master, so a dark exchange, and alt's, must not add one.
func TestKeyEventSubscriberSubscribesRecycleFlagsOnlyUnderTheCeiling(t *testing.T) {
	recyclePattern := model.ClientRecycleKeyEventPattern(3)
	off := (&keyEventSubscriber{clientRecyclePattern: false}).patterns(3)
	on := (&keyEventSubscriber{clientRecyclePattern: true}).patterns(3)
	for _, pattern := range off {
		if pattern == recyclePattern {
			t.Fatal("a subscriber without the ceiling subscribes the recycle flags")
		}
	}
	if len(on) != len(off)+1 || on[len(on)-1] != recyclePattern {
		t.Fatalf("a subscriber with the ceiling has patterns %v, want %v and the recycle flags", on, off)
	}
	for i, pattern := range off {
		if on[i] != pattern {
			t.Fatalf("the ceiling changed pattern %d: %s, want %s", i, on[i], pattern)
		}
	}
}

// The switch follows the control for its own block, keeps its last value
// through a failed read, and sets the enabled gauge with it.
func TestStuckProviderRecycleControlReadsTheSwitch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := &stuckProviderRecycleTestControl{}
	recycle := newExchangeStuckProviderRecycleWithStore(ctx, stuckProviderRecycleTestSettings(), "g1", control.read)
	if recycle.allows() || testutil.ToFloat64(stuckProviderRecycleEnabledGauge) != 0 {
		t.Fatal("a dark default allowed recycles before the first read")
	}
	recycle.refresh()
	if recycle.allows() {
		t.Fatal("an absent control widened a dark default")
	}
	for _, c := range []struct {
		value string
		allow bool
	}{
		{value: "beta", allow: false},
		{value: "g1", allow: true},
		{value: "beta,g1", allow: true},
		{value: "off", allow: false},
		{value: "all", allow: true},
	} {
		control.set(c.value)
		recycle.refresh()
		if recycle.allows() != c.allow || (testutil.ToFloat64(stuckProviderRecycleEnabledGauge) == 1) != c.allow {
			t.Errorf("control %q: allows=%t, want %t", c.value, recycle.allows(), c.allow)
		}
	}
	control.fail(errors.New("synthetic control read failure"))
	recycle.refresh()
	if !recycle.allows() {
		t.Error("a failed read dropped the last good value")
	}
}

// A flag only counts: without a resident here as not_resident, whatever the
// switch, and with a resident here but the switch off as disabled. Neither
// starts a recycle.
func TestStuckProviderRecycleFlagWithoutWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := stuckProviderRecycleTestSettings()
	exchangeCtx, exchangeCancel := context.WithCancel(ctx)
	exchange := &Exchange{
		ctx:       exchangeCtx,
		cancel:    exchangeCancel,
		settings:  settings,
		block:     "g1",
		residents: map[server.Id]*Resident{},
	}
	control := &stuckProviderRecycleTestControl{}
	exchange.stuckProviderRecycle = newExchangeStuckProviderRecycleWithStore(exchangeCtx, settings, exchange.block, control.read)

	// a resident here that the switch must keep: never run, only looked up
	residentClientId := server.NewId()
	exchange.residents[residentClientId] = &Resident{clientId: residentClientId}
	counts := func() (disabled float64, notResident float64) {
		return testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleDisabled)),
			testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleNotResident))
	}

	disabledBefore, notResidentBefore := counts()
	exchange.handleClientRecycle(residentClientId)
	exchange.handleClientRecycle(server.NewId())
	if disabled, notResident := counts(); disabled != disabledBefore+1 || notResident != notResidentBefore+1 {
		t.Fatalf("with the switch off: disabled +%v, not_resident +%v; want +1 each", disabled-disabledBefore, notResident-notResidentBefore)
	}
	control.set("g1")
	exchange.stuckProviderRecycle.refresh()
	disabledBefore, notResidentBefore = counts()
	exchange.handleClientRecycle(server.NewId())
	if disabled, notResident := counts(); disabled != disabledBefore || notResident != notResidentBefore+1 {
		t.Fatalf("with the switch on: disabled +%v, not_resident +%v; want +0 and +1", disabled-disabledBefore, notResident-notResidentBefore)
	}
	exchange.Close()
	if !exchange.WaitForIdle(ctx) {
		t.Fatal("a flag without work left a worker")
	}
	// an exchange with the ceiling off has no switch and ignores flags
	(&Exchange{ctx: ctx, settings: DefaultExchangeSettings(), residents: map[server.Id]*Resident{}}).handleClientRecycle(server.NewId())
}

// WaitForIdle joins the control loop, even while it waits on a slow read.
func TestStuckProviderRecycleWaitForIdleJoinsTheControlLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := stuckProviderRecycleTestSettings()
	exchangeCtx, exchangeCancel := context.WithCancel(ctx)
	exchange := &Exchange{
		ctx:       exchangeCtx,
		cancel:    exchangeCancel,
		settings:  settings,
		block:     "test-block",
		residents: map[server.Id]*Resident{},
	}
	readEntered := make(chan struct{}, 16)
	readRelease := make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(readRelease) })
	defer releaseRead()
	var readCount atomic.Int64
	read := func(context.Context, model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
		readCount.Add(1)
		readEntered <- struct{}{}
		// a store slower than the cancellation
		<-readRelease
		return nil, errors.New("synthetic slow store")
	}
	exchange.stuckProviderRecycle = newExchangeStuckProviderRecycleWithStore(exchangeCtx, settings, exchange.block, read)
	exchange.stuckProviderRecycle.start()
	<-readEntered
	exchange.Close()
	heldCtx, heldCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	idle := exchange.WaitForIdle(heldCtx)
	heldCancel()
	if idle {
		t.Fatal("WaitForIdle returned while the control loop was in a read")
	}
	releaseRead()
	if !exchange.WaitForIdle(ctx) {
		t.Fatal("WaitForIdle did not return after the read")
	}
	if count := readCount.Load(); count != 1 {
		t.Fatalf("the control loop read %d times after the close, want 1", count)
	}
}

func newClientRecycleTestEnv(ctx context.Context, t testing.TB, mutate func(*ExchangeSettings)) *peerDiscoveryEnv {
	server.Testing_EnableKeyspaceNotifications(ctx)
	return testing_newPeerDiscoveryEnvWithSettings(ctx, t, func(settings *ExchangeSettings) {
		settings.KeyEventDelivery.Enabled = true
		// far outside the test window: delivery must come from key events
		settings.KeyEventDelivery.CorrectivePollInterval = 10 * time.Minute
		settings.KeyEventDelivery.ResyncSpreadTimeout = 1 * time.Second
		settings.EnableStuckProviderRecycle = true
		settings.StuckProviderRecycleControlRefreshInterval = 100 * time.Millisecond
		if mutate != nil {
			mutate(settings)
		}
	})
}

func setClientRecycleTestControl(ctx context.Context, t testing.TB, value string) {
	t.Helper()
	controlCtx, controlCancel := context.WithTimeout(ctx, 10*time.Second)
	defer controlCancel()
	if err := model.SetConnectMoveControl(controlCtx, map[string]string{model.ConnectMoveControlFieldStuckProviderRecycle: value}); err != nil {
		t.Fatal(err)
	}
}

// The api's report: sets the flag as the api would.
func setClientRecycleTestFlag(ctx context.Context, t testing.TB, clientId server.Id) {
	t.Helper()
	flagCtx, flagCancel := context.WithTimeout(ctx, 10*time.Second)
	defer flagCancel()
	set, err := model.SetClientRecycle(flagCtx, clientId, time.Hour)
	if err != nil || !set {
		t.Fatalf("set the recycle flag = %t, %v", set, err)
	}
}

// One connected client with a resident on the env's exchange, and a second
// socket of the same instance that reads the transport's close.
type clientRecycleTestClient struct {
	clientId server.Id
	resident *Resident
	ws       connectlib.H1MessageConn
	close    func()
}

func connectClientRecycleTestClient(ctx context.Context, t testing.TB, env *peerDiscoveryEnv, description string) *clientRecycleTestClient {
	t.Helper()
	clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{Description: description})
	instanceId := server.NewId()
	client := env.newClient(clientId)
	transport := env.newTransport(byClientJwt, instanceId, client.RouteManager())
	ws, closeWs, err := dialSessionAdmissionWithInstance(ctx, env, instanceId, "h1plus", byClientJwt, "")
	if err != nil {
		transport.Close()
		client.Close()
		t.Fatal(err)
	}
	if !waitFor(ctx, 10*time.Second, func() bool {
		connections, resident := env.exchange.testingHoldsClient(clientId)
		return 2 <= connections && resident
	}) {
		connections, resident := env.exchange.testingHoldsClient(clientId)
		t.Fatalf("%s is not connected: connections=%d resident=%t", description, connections, resident)
	}
	return &clientRecycleTestClient{
		clientId: clientId,
		resident: env.exchange.localResident(clientId),
		ws:       ws,
		close: func() {
			closeWs()
			transport.Close()
			client.Close()
		},
	}
}

// Reads the socket until its transport control close, and returns its reason.
func readClientRecycleTestClose(t testing.TB, ws connectlib.H1MessageConn, timeout time.Duration) (uint32, bool) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, message, err := ws.ReadMessage()
		if err != nil {
			return 0, false
		}
		if len(message) == 5 && message[0] == transportControlClose {
			return binary.BigEndian.Uint32(message[1:]), true
		}
	}
}

// With the switch naming this block, a flag closes the reported client's
// transports here with the moved close and retires its resident, so the
// client nominates a fresh one. Another client is left alone, and a flag for
// a client without a resident here only counts.
func TestExchangeRecyclesReportedProviderOnItsBlock(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// the trial: one block, named by the control
		setClientRecycleTestControl(ctx, t, "g1 test")
		env := newClientRecycleTestEnv(ctx, t, nil)
		defer env.Close()
		if !waitFor(ctx, 10*time.Second, env.exchange.stuckProviderRecycle.allows) {
			t.Fatal("the named block does not recycle")
		}
		if testutil.ToFloat64(stuckProviderRecycleEnabledGauge) != 1 {
			t.Fatal("the enabled gauge is off")
		}
		if !env.exchange.keyEventSubscriber.clientRecyclePattern {
			t.Fatal("an exchange with the ceiling on does not subscribe the recycle flags")
		}

		reported := connectClientRecycleTestClient(ctx, t, env, "reported provider")
		defer reported.close()
		bystander := connectClientRecycleTestClient(ctx, t, env, "bystander")
		defer bystander.close()

		recycledBefore := testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleRecycled))
		notResidentBefore := testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleNotResident))
		closesBefore := testutil.ToFloat64(stuckProviderRecycleTransportClosesCounter)
		connectionsBefore, _ := env.exchange.testingHoldsClient(reported.clientId)

		setClientRecycleTestFlag(ctx, t, reported.clientId)

		reason, closed := readClientRecycleTestClose(t, reported.ws, 10*time.Second)
		if !closed || reason != closeReasonMoved {
			t.Fatalf("the reported client's transport closed=%t with reason %d, want the moved close %d", closed, reason, closeReasonMoved)
		}
		if !waitFor(ctx, 10*time.Second, reported.resident.IsDone) {
			t.Fatal("the reported client's resident did not retire")
		}
		if !reported.resident.moved.Load() {
			t.Fatal("the resident retired without the moved teardown")
		}
		if !waitFor(ctx, 10*time.Second, func() bool {
			return recycledBefore+1 <= testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleRecycled))
		}) {
			t.Fatal("the recycle was not counted")
		}
		// every one of its transports here
		if closes := testutil.ToFloat64(stuckProviderRecycleTransportClosesCounter) - closesBefore; closes != float64(connectionsBefore) {
			t.Fatalf("closed %v transports, want %d", closes, connectionsBefore)
		}
		// the client comes back to a fresh resident
		if !waitFor(ctx, 10*time.Second, func() bool {
			resident := env.exchange.localResident(reported.clientId)
			return resident != nil && resident != reported.resident && !resident.IsDone()
		}) {
			t.Fatal("the reported client has no fresh resident")
		}

		if bystander.resident.IsDone() || env.exchange.localResident(bystander.clientId) != bystander.resident {
			t.Fatal("the recycle retired another client's resident")
		}
		if connections, _ := env.exchange.testingHoldsClient(bystander.clientId); connections < 2 {
			t.Fatalf("the recycle closed another client's transports: %d left", connections)
		}

		setClientRecycleTestFlag(ctx, t, server.NewId())
		if !waitFor(ctx, 10*time.Second, func() bool {
			return notResidentBefore+1 <= testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleNotResident))
		}) {
			t.Fatal("a flag for a client without a resident here was not counted")
		}
	})
}

// A flag does nothing while the switch is off for this block: off by
// default, turned off at run time, naming another block, or with the code
// ceiling off.
func TestExchangeIgnoresRecycleWhileSwitchOff(t *testing.T) {
	if testing.Short() {
		return
	}
	cases := []struct {
		name string
		// the control before and after the env starts; "" leaves it absent
		initial string
		final   string
		mutate  func(*ExchangeSettings)
		// a handler runs and counts the flag as disabled
		counted bool
	}{
		{name: "the default", counted: true},
		{name: "the kill switch at run time", initial: "test", final: "off", counted: true},
		{name: "another block", initial: "test", final: "g1", counted: true},
		{name: "the ceiling off", initial: "all", mutate: func(settings *ExchangeSettings) {
			settings.EnableStuckProviderRecycle = false
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				if c.initial != "" {
					setClientRecycleTestControl(ctx, t, c.initial)
				}
				env := newClientRecycleTestEnv(ctx, t, c.mutate)
				defer env.Close()
				if c.final != "" {
					if !waitFor(ctx, 10*time.Second, env.exchange.stuckProviderRecycle.allows) {
						t.Fatal("the block was never on")
					}
					setClientRecycleTestControl(ctx, t, c.final)
					if !waitFor(ctx, 10*time.Second, func() bool { return !env.exchange.stuckProviderRecycle.allows() }) {
						t.Fatal("the control did not turn the block off")
					}
				}
				if c.mutate == nil && env.exchange.stuckProviderRecycle.allows() {
					t.Fatal("the block recycles")
				}
				if c.mutate != nil && env.exchange.stuckProviderRecycle != nil {
					t.Fatal("an exchange with the ceiling off has a recycle switch")
				}
				if subscribed := env.exchange.keyEventSubscriber.clientRecyclePattern; subscribed != (c.mutate == nil) {
					t.Fatalf("recycle flags subscribed=%t with the ceiling on=%t", subscribed, c.mutate == nil)
				}

				reported := connectClientRecycleTestClient(ctx, t, env, "reported provider")
				defer reported.close()
				disabledBefore := testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleDisabled))
				dispatchedBefore := testutil.ToFloat64(keyEventsDispatchedClientRecycle)

				setClientRecycleTestFlag(ctx, t, reported.clientId)

				if c.counted {
					if !waitFor(ctx, 10*time.Second, func() bool {
						return disabledBefore+1 <= testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleDisabled))
					}) {
						t.Fatal("the flag was not counted as disabled")
					}
				} else {
					// past the event's delivery
					select {
					case <-time.After(2 * time.Second):
					}
					if dispatched := testutil.ToFloat64(keyEventsDispatchedClientRecycle); dispatched != dispatchedBefore {
						t.Fatalf("an exchange with the ceiling off dispatched %v flags", dispatched-dispatchedBefore)
					}
				}
				if _, closed := readClientRecycleTestClose(t, reported.ws, 2*time.Second); closed {
					t.Fatal("the flag closed the client's transport with the switch off")
				}
				if reported.resident.IsDone() || env.exchange.localResident(reported.clientId) != reported.resident {
					t.Fatal("the flag retired the client's resident with the switch off")
				}
				if connections, _ := env.exchange.testingHoldsClient(reported.clientId); connections < 2 {
					t.Fatalf("the flag closed the client's transports with the switch off: %d left", connections)
				}
			})
		})
	}
}
