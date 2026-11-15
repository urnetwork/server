package connect

// Stuck provider recycles (client_recycle.go): a recycle flag the api sets
// reaches every exchange as a key event. Every exchange with the code ceiling
// on acts, with no control to read, and each one that holds the client's
// transports or its resident excuses the reconnect, closes those transports
// with the moved close and retires the resident there. The dispatch only
// queues; paced, bounded workers recycle. A removed client is left to its
// removal, and an exchange with the ceiling off, as alt's, to nothing.

import (
	"context"
	"encoding/binary"
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

// The connect service turns the ceiling on, so its exchanges recycle on every
// block. Every other exchange, alt's among them, keeps it off and builds no
// recycles.
func TestStuckProviderRecycleIsOnInTheConnectService(t *testing.T) {
	defaults := DefaultExchangeSettings()
	if defaults.EnableStuckProviderRecycle {
		t.Error("the default exchange settings allow stuck provider recycles")
	}
	runSettings := exchangeSettingsForRun(RunOptions{Port: 443})
	if !runSettings.EnableStuckProviderRecycle {
		t.Error("the connect service does not recycle stuck providers")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if newExchangeStuckProviderRecycle(ctx, defaults, func(server.Id) {}) != nil {
		t.Error("the ceiling off still built recycles")
	}
	if runSettings.StuckProviderRecyclesPerSecond <= 0 || runSettings.StuckProviderRecycleBurst <= 0 ||
		runSettings.StuckProviderRecycleQueueSize <= 0 || runSettings.StuckProviderRecycleWorkerCount <= 0 {
		t.Errorf(
			"recycles unpaced or closed: %v a second, burst %d, queue %d, workers %d",
			runSettings.StuckProviderRecyclesPerSecond,
			runSettings.StuckProviderRecycleBurst,
			runSettings.StuckProviderRecycleQueueSize,
			runSettings.StuckProviderRecycleWorkerCount,
		)
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

func stuckProviderRecycleTestOutcome(outcome string) float64 {
	return testutil.ToFloat64(stuckProviderRecyclesCounter.WithLabelValues(outcome))
}

// An exchange built by hand for the dispatch and the workers: its client maps
// and lifecycle, with no residents run.
func newStuckProviderRecycleTestExchange(ctx context.Context, settings *ExchangeSettings) *Exchange {
	exchangeCtx, exchangeCancel := context.WithCancel(ctx)
	return &Exchange{
		ctx:         exchangeCtx,
		cancel:      exchangeCancel,
		settings:    settings,
		block:       "g1",
		residents:   map[server.Id]*Resident{},
		connections: map[server.Id]map[server.Id]context.CancelFunc{},
	}
}

// The code default, through the connect service's own settings and
// constructor: with no control to read, a flag for a client held here reaches
// a worker, which recycles it, and a flag for a client not held here counts
// as not_held and recycles nothing.
func TestStuckProviderRecycleRecyclesAHeldClientWithoutAControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	settings := exchangeSettingsForRun(RunOptions{Port: 443})
	exchange := newStuckProviderRecycleTestExchange(ctx, settings)
	recycled := make(chan server.Id, 4)
	exchange.stuckProviderRecycle = newExchangeStuckProviderRecycle(exchange.ctx, settings, func(clientId server.Id) {
		recycled <- clientId
	})
	if exchange.stuckProviderRecycle == nil {
		t.Fatal("the connect service built no recycles")
	}

	heldClientId := server.NewId()
	exchange.registerConnection(heldClientId, server.NewId(), func() {})
	notHeldBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleNotHeld)
	exchange.handleClientRecycle(server.NewId())
	exchange.handleClientRecycle(heldClientId)
	select {
	case clientId := <-recycled:
		if clientId != heldClientId {
			t.Fatalf("recycled %s, want the held client %s", clientId, heldClientId)
		}
	case <-ctx.Done():
		t.Fatal("the held client was not recycled")
	}
	if notHeld := stuckProviderRecycleTestOutcome(stuckProviderRecycleNotHeld); notHeld != notHeldBefore+1 {
		t.Fatalf("not_held +%v, want +1", notHeld-notHeldBefore)
	}
	if gauge := testutil.ToFloat64(stuckProviderRecycleEnabledGauge); gauge != 1 {
		t.Fatalf("the enabled gauge %v, want 1", gauge)
	}
	exchange.Close()
	if !exchange.WaitForIdle(ctx) {
		t.Fatal("the workers outlived the exchange")
	}
	select {
	case clientId := <-recycled:
		t.Fatalf("recycled %s, a client not held here", clientId)
	default:
	}
}

// The dispatch never blocks and never recycles: a flag for a client not held
// here counts as not_held, and a held client (by its resident or by a
// transport alone) is queued for the workers. A flag past the queue counts as
// queue_full, and a burst of them returns at once.
func TestStuckProviderRecycleDispatchOnlyQueues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	settings := stuckProviderRecycleTestSettings()
	settings.StuckProviderRecycleQueueSize = 2
	exchange := newStuckProviderRecycleTestExchange(ctx, settings)
	recycled := make(chan server.Id, 64)
	exchange.stuckProviderRecycle = newExchangeStuckProviderRecycleWithoutWorkers(exchange.ctx, settings, func(clientId server.Id) {
		recycled <- clientId
	})

	// held here by a resident that is never run, only looked up, and by a
	// transport alone
	residentClientId, transportClientId := server.NewId(), server.NewId()
	exchange.residents[residentClientId] = &Resident{clientId: residentClientId}
	exchange.registerConnection(transportClientId, server.NewId(), func() {})

	notHeldBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleNotHeld)
	queueFullBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleQueueFull)
	exchange.handleClientRecycle(residentClientId)
	exchange.handleClientRecycle(transportClientId)
	exchange.handleClientRecycle(server.NewId())
	if queued := len(exchange.stuckProviderRecycle.queue); queued != 2 {
		t.Fatalf("queued %d held clients, want 2", queued)
	}
	if notHeld := stuckProviderRecycleTestOutcome(stuckProviderRecycleNotHeld); notHeld != notHeldBefore+1 {
		t.Fatalf("not_held +%v, want +1", notHeld-notHeldBefore)
	}
	// a burst past the full queue returns at once, each flag counted
	burstDone := make(chan struct{})
	go func() {
		defer close(burstDone)
		for range 1000 {
			exchange.handleClientRecycle(residentClientId)
		}
	}()
	select {
	case <-burstDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the dispatch blocked on a full queue")
	}
	if queueFull := stuckProviderRecycleTestOutcome(stuckProviderRecycleQueueFull); queueFull != queueFullBefore+1000 {
		t.Fatalf("queue_full +%v, want +1000", queueFull-queueFullBefore)
	}
	select {
	case clientId := <-recycled:
		t.Fatalf("the dispatch recycled %s itself", clientId)
	default:
	}
	exchange.Close()
	if !exchange.WaitForIdle(ctx) {
		t.Fatal("a dispatch left a worker")
	}
	// an exchange with the ceiling off has no recycles and ignores flags
	(&Exchange{ctx: ctx, settings: DefaultExchangeSettings(), residents: map[server.Id]*Resident{}}).handleClientRecycle(server.NewId())
}

// Runs count queued recycles through the workers of settings, each lasting
// duration, the first raising. Returns how long they took from the start of
// the workers and how many ran at once at most. Closing the exchange ends
// the workers.
func runClientRecycleTestWorkers(t *testing.T, settings *ExchangeSettings, count int, duration time.Duration) (time.Duration, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	exchange := newStuckProviderRecycleTestExchange(ctx, settings)
	exchangeCtx := exchange.ctx

	raisingClientId := server.NewId()
	var inFlight, maxInFlight, attempts atomic.Int64
	attempted := make(chan server.Id, count)
	recycle := func(clientId server.Id) {
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			observed := maxInFlight.Load()
			if current <= observed || maxInFlight.CompareAndSwap(observed, current) {
				break
			}
		}
		attempts.Add(1)
		attempted <- clientId
		if clientId == raisingClientId {
			panic("synthetic recycle failure")
		}
		select {
		case <-time.After(duration):
		case <-exchangeCtx.Done():
		}
	}
	exchange.stuckProviderRecycle = newExchangeStuckProviderRecycleWithoutWorkers(exchangeCtx, settings, recycle)
	errorBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleError)

	clientIds := []server.Id{raisingClientId}
	for len(clientIds) < count {
		clientIds = append(clientIds, server.NewId())
	}
	for _, clientId := range clientIds {
		if !exchange.stuckProviderRecycle.enqueue(clientId) {
			t.Fatal("the queue refused a client below its size")
		}
	}
	startTime := time.Now()
	exchange.stuckProviderRecycle.start()
	seen := map[server.Id]bool{}
	for range clientIds {
		select {
		case clientId := <-attempted:
			seen[clientId] = true
		case <-ctx.Done():
			t.Fatalf("%d of %d recycles ran", len(seen), len(clientIds))
		}
	}
	elapsed := time.Since(startTime)
	if len(seen) != len(clientIds) {
		t.Fatalf("recycled %d distinct clients, want %d", len(seen), len(clientIds))
	}
	// the raise counts, and its worker went on to the others
	if !waitFor(ctx, 10*time.Second, func() bool {
		return errorBefore+1 <= stuckProviderRecycleTestOutcome(stuckProviderRecycleError)
	}) {
		t.Fatal("a raising recycle was not counted as an error")
	}
	exchange.Close()
	if !exchange.WaitForIdle(ctx) {
		t.Fatal("the workers outlived the exchange")
	}
	if ran := attempts.Load(); ran != int64(len(clientIds)) {
		t.Fatalf("%d recycles, want %d", ran, len(clientIds))
	}
	return elapsed, maxInFlight.Load()
}

// The workers recycle no faster than the token bucket, however fast each
// recycle is: a token at once, then one each 1/rate.
func TestStuckProviderRecycleWorkersArePaced(t *testing.T) {
	settings := stuckProviderRecycleTestSettings()
	settings.StuckProviderRecyclesPerSecond = 10
	settings.StuckProviderRecycleBurst = 1
	settings.StuckProviderRecycleWorkerCount = 2
	settings.StuckProviderRecycleQueueSize = 64
	// the workers could do 40 a second
	count := 10
	elapsed, _ := runClientRecycleTestWorkers(t, settings, count, 50*time.Millisecond)
	if minElapsed := time.Duration(count-1) * time.Second / 10; elapsed < minElapsed {
		t.Fatalf("%d recycles took %s, faster than the bucket allows (%s)", count, elapsed, minElapsed)
	}
}

// At most StuckProviderRecycleWorkerCount recycles run at once, however many
// are queued and however fast the bucket admits them.
func TestStuckProviderRecycleWorkersAreBounded(t *testing.T) {
	settings := stuckProviderRecycleTestSettings()
	settings.StuckProviderRecyclesPerSecond = 1000
	settings.StuckProviderRecycleBurst = 100
	settings.StuckProviderRecycleWorkerCount = 2
	settings.StuckProviderRecycleQueueSize = 64
	_, most := runClientRecycleTestWorkers(t, settings, 10, 200*time.Millisecond)
	if int64(settings.StuckProviderRecycleWorkerCount) < most {
		t.Fatalf("%d recycles ran at once, want at most %d", most, settings.StuckProviderRecycleWorkerCount)
	}
}

// WaitForIdle joins the workers, even while one is in a recycle slower than
// the exchange's cancellation.
func TestStuckProviderRecycleWaitForIdleJoinsARecycleInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := stuckProviderRecycleTestSettings()
	exchange := newStuckProviderRecycleTestExchange(ctx, settings)
	recycleEntered := make(chan struct{}, 1)
	recycleRelease := make(chan struct{})
	releaseRecycle := sync.OnceFunc(func() { close(recycleRelease) })
	defer releaseRecycle()
	exchange.stuckProviderRecycle = newExchangeStuckProviderRecycle(exchange.ctx, settings, func(server.Id) {
		recycleEntered <- struct{}{}
		// a store slower than the cancellation
		<-recycleRelease
	})
	clientId := server.NewId()
	exchange.registerConnection(clientId, server.NewId(), func() {})
	exchange.handleClientRecycle(clientId)
	select {
	case <-recycleEntered:
	case <-ctx.Done():
		t.Fatal("the held client was not recycled")
	}
	exchange.Close()
	// with the worker held in its recycle, the join cannot finish, so a done
	// context returns at once
	doneCtx, doneCancel := context.WithCancel(ctx)
	doneCancel()
	if exchange.WaitForIdle(doneCtx) {
		t.Fatal("WaitForIdle returned while a worker was in a recycle")
	}
	releaseRecycle()
	if !exchange.WaitForIdle(ctx) {
		t.Fatal("WaitForIdle did not return after the recycle")
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
		if mutate != nil {
			mutate(settings)
		}
	})
}

// Requires the move control to be absent, as the test env starts.
func requireNoClientRecycleTestControl(ctx context.Context, t testing.TB) {
	t.Helper()
	controlCtx, controlCancel := context.WithTimeout(ctx, 10*time.Second)
	defer controlCancel()
	control, err := model.GetConnectMoveControl(controlCtx, model.ConnectMoveControlCeiling{})
	if err != nil {
		t.Fatal(err)
	}
	if control.Present {
		t.Fatal("the move control is set")
	}
}

// Writes value into both retired stuck provider fields of the move control, as
// a hash ops has not cleaned still holds them.
func setClientRecycleTestRetiredControl(ctx context.Context, t testing.TB, value string) {
	t.Helper()
	controlCtx, controlCancel := context.WithTimeout(ctx, 10*time.Second)
	defer controlCancel()
	if err := model.SetConnectMoveControl(controlCtx, map[string]string{
		"stuckProviderSignal":  value,
		"stuckProviderRecycle": value,
	}); err != nil {
		t.Fatal(err)
	}
}

// The api's report: sets the flag as the api would.
func setClientRecycleTestFlag(ctx context.Context, t testing.TB, clientId server.Id) {
	t.Helper()
	setClientRecycleTestFlagWithTtl(ctx, t, clientId, time.Hour)
}

func setClientRecycleTestFlagWithTtl(ctx context.Context, t testing.TB, clientId server.Id, ttl time.Duration) {
	t.Helper()
	flagCtx, flagCancel := context.WithTimeout(ctx, 10*time.Second)
	defer flagCancel()
	set, _, err := model.SetClientRecycle(flagCtx, clientId, ttl)
	if err != nil || !set {
		t.Fatalf("set the recycle flag = %t, %v", set, err)
	}
}

func clientRecycleTestFlagTtl(ctx context.Context, clientId server.Id) time.Duration {
	var ttl time.Duration
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		ttl, err = r.PTTL(ctx, "{"+clientId.String()+"}c_recycle").Result()
		server.Raise(err)
	})
	return ttl
}

// Whether the client's reconnect was excused: its announce consumed the
// marker since consumedBefore, or the marker is still there (taken here).
func clientRecycleTestExcused(ctx context.Context, clientId server.Id, consumedBefore float64) bool {
	return consumedBefore < testutil.ToFloat64(drainExcusesConsumedCounter) || model.TakeDrainExcuse(ctx, clientId) != nil
}

// One connected client with a resident on the env's exchange, and a second
// socket of the same instance that reads the transport's close.
type clientRecycleTestClient struct {
	clientId    server.Id
	instanceId  server.Id
	byClientJwt string
	resident    *Resident
	ws          connectlib.H1MessageConn
	close       func()
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
		clientId:    clientId,
		instanceId:  instanceId,
		byClientJwt: byClientJwt,
		resident:    env.exchange.localResident(clientId),
		ws:          ws,
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

// The code default, with no control: a flag excuses the reported client's
// reconnect, closes its transports here with the moved close and retires its
// resident, so the client nominates a fresh one. Another client is left
// alone, and a flag for a client not held here only counts. A report inside
// the flag's ttl sets nothing and emits no event; once the flag expires, the
// next report recycles the client again, with the retired control fields set
// off.
func TestExchangeRecyclesReportedProviderWithoutAControl(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		requireNoClientRecycleTestControl(ctx, t)
		env := newClientRecycleTestEnv(ctx, t, nil)
		defer env.Close()
		if env.exchange.stuckProviderRecycle == nil {
			t.Fatal("an exchange with the ceiling on has no recycles")
		}
		if !env.exchange.keyEventSubscriber.clientRecyclePattern {
			t.Fatal("an exchange with the ceiling on does not subscribe the recycle flags")
		}

		reported := connectClientRecycleTestClient(ctx, t, env, "reported provider")
		defer reported.close()
		bystander := connectClientRecycleTestClient(ctx, t, env, "bystander")
		defer bystander.close()

		recycledBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleRecycled)
		notHeldBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleNotHeld)
		closesBefore := testutil.ToFloat64(stuckProviderRecycleTransportClosesCounter)
		excusesBefore := testutil.ToFloat64(stuckProviderRecycleExcusesCounter)
		consumedBefore := testutil.ToFloat64(drainExcusesConsumedCounter)
		connectionsBefore, _ := env.exchange.testingHoldsClient(reported.clientId)

		// a short flag, to report again once it expires
		flagTtl := 10 * time.Second
		dispatchedBefore := testutil.ToFloat64(keyEventsDispatchedClientRecycle)
		setClientRecycleTestFlagWithTtl(ctx, t, reported.clientId, flagTtl)
		// a report inside the flag's ttl is a miss with no event: the set's
		// is the only one dispatched
		flagCtx, flagCancel := context.WithTimeout(ctx, 10*time.Second)
		set, _, err := model.SetClientRecycle(flagCtx, reported.clientId, flagTtl)
		flagCancel()
		if err != nil || set {
			t.Fatalf("a report inside the flag's ttl = %t, %v; want a miss", set, err)
		}
		if !waitFor(ctx, 10*time.Second, func() bool {
			return dispatchedBefore+1 <= testutil.ToFloat64(keyEventsDispatchedClientRecycle)
		}) {
			t.Fatal("the flag was not dispatched")
		}
		select {
		case <-time.After(1 * time.Second):
		}
		if dispatched := testutil.ToFloat64(keyEventsDispatchedClientRecycle); dispatched != dispatchedBefore+1 {
			t.Fatalf("a set and a miss dispatched %v flags, want 1", dispatched-dispatchedBefore)
		}

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
			return recycledBefore+1 <= stuckProviderRecycleTestOutcome(stuckProviderRecycleRecycled)
		}) {
			t.Fatal("the recycle was not counted")
		}
		if testutil.ToFloat64(stuckProviderRecycleEnabledGauge) != 1 {
			t.Fatal("the enabled gauge is off")
		}
		// every one of its transports here, after one excuse
		if closes := testutil.ToFloat64(stuckProviderRecycleTransportClosesCounter) - closesBefore; closes != float64(connectionsBefore) {
			t.Fatalf("closed %v transports, want %d", closes, connectionsBefore)
		}
		if excuses := testutil.ToFloat64(stuckProviderRecycleExcusesCounter) - excusesBefore; excuses != 1 {
			t.Fatalf("wrote %v excuses, want 1", excuses)
		}
		// the client comes back to a fresh resident
		var freshResident *Resident
		if !waitFor(ctx, 20*time.Second, func() bool {
			freshResident = env.exchange.localResident(reported.clientId)
			return freshResident != nil && freshResident != reported.resident && !freshResident.IsDone()
		}) {
			t.Fatal("the reported client has no fresh resident")
		}
		if !clientRecycleTestExcused(ctx, reported.clientId, consumedBefore) {
			t.Fatal("the recycle's reconnect was not excused")
		}

		if bystander.resident.IsDone() || env.exchange.localResident(bystander.clientId) != bystander.resident {
			t.Fatal("the recycle retired another client's resident")
		}
		if connections, _ := env.exchange.testingHoldsClient(bystander.clientId); connections < 2 {
			t.Fatalf("the recycle closed another client's transports: %d left", connections)
		}

		setClientRecycleTestFlag(ctx, t, server.NewId())
		if !waitFor(ctx, 10*time.Second, func() bool {
			return notHeldBefore+1 <= stuckProviderRecycleTestOutcome(stuckProviderRecycleNotHeld)
		}) {
			t.Fatal("a flag for a client not held here was not counted")
		}

		// the flag expires, and the next report recycles the client again,
		// past the retired control fields set off
		if !waitFor(ctx, 20*time.Second, func() bool {
			return clientRecycleTestFlagTtl(ctx, reported.clientId) <= 0
		}) {
			t.Fatal("the flag did not expire")
		}
		if freshResident.IsDone() {
			t.Fatal("the flag's expiry retired the fresh resident")
		}
		setClientRecycleTestRetiredControl(ctx, t, "off")
		setClientRecycleTestFlag(ctx, t, reported.clientId)
		if !waitFor(ctx, 20*time.Second, freshResident.IsDone) || !freshResident.moved.Load() {
			t.Fatal("a report after the flag expired did not recycle the client again")
		}
		if !waitFor(ctx, 10*time.Second, func() bool {
			return recycledBefore+2 <= stuckProviderRecycleTestOutcome(stuckProviderRecycleRecycled)
		}) {
			t.Fatal("the second recycle was not counted")
		}
		if !waitFor(ctx, 20*time.Second, func() bool {
			resident := env.exchange.localResident(reported.clientId)
			return resident != nil && resident != freshResident && !resident.IsDone()
		}) {
			t.Fatal("the reported client has no fresh resident after its second recycle")
		}
	})
}

// An exchange with the code ceiling off, as alt's: it builds no recycles and
// subscribes no recycle flags, so a flag for a client it holds does nothing,
// even with the retired control fields set to all.
func TestExchangeWithTheCeilingOffIgnoresRecycleFlags(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		setClientRecycleTestRetiredControl(ctx, t, "all")
		env := newClientRecycleTestEnv(ctx, t, func(settings *ExchangeSettings) {
			settings.EnableStuckProviderRecycle = false
		})
		defer env.Close()
		if env.exchange.stuckProviderRecycle != nil {
			t.Fatal("an exchange with the ceiling off has recycles")
		}
		if env.exchange.keyEventSubscriber.clientRecyclePattern {
			t.Fatal("an exchange with the ceiling off subscribes the recycle flags")
		}

		reported := connectClientRecycleTestClient(ctx, t, env, "reported provider")
		defer reported.close()
		dispatchedBefore := testutil.ToFloat64(keyEventsDispatchedClientRecycle)

		setClientRecycleTestFlag(ctx, t, reported.clientId)

		// past the event's delivery
		select {
		case <-time.After(2 * time.Second):
		}
		if dispatched := testutil.ToFloat64(keyEventsDispatchedClientRecycle); dispatched != dispatchedBefore {
			t.Fatalf("an exchange with the ceiling off dispatched %v flags", dispatched-dispatchedBefore)
		}
		if _, closed := readClientRecycleTestClose(t, reported.ws, 2*time.Second); closed {
			t.Fatal("the flag closed the client's transport with the ceiling off")
		}
		if reported.resident.IsDone() || env.exchange.localResident(reported.clientId) != reported.resident {
			t.Fatal("the flag retired the client's resident with the ceiling off")
		}
		if connections, _ := env.exchange.testingHoldsClient(reported.clientId); connections < 2 {
			t.Fatalf("the flag closed the client's transports with the ceiling off: %d left", connections)
		}
	})
}

// A provider whose transports are split over two exchanges, its resident on
// the first: the flag reaches both, and each closes the transports it holds
// with the moved close after its own excuse. The first retires the resident
// (recycled), the second has only a transport (transport_only), and the
// client comes back to a fresh resident with nothing left on the second.
func TestExchangeRecyclesTheProviderOnEveryExchangeThatHoldsIt(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		requireNoClientRecycleTestControl(ctx, t)
		residentEnv := newClientRecycleTestEnv(ctx, t, nil)
		defer residentEnv.Close()
		transportEnv := newClientRecycleTestEnv(ctx, t, nil)
		defer transportEnv.Close()
		for _, env := range []*peerDiscoveryEnv{residentEnv, transportEnv} {
			if env.exchange.stuckProviderRecycle == nil {
				t.Fatal("an exchange with the ceiling on has no recycles")
			}
		}

		// the resident and two transports on the first exchange
		reported := connectClientRecycleTestClient(ctx, t, residentEnv, "split provider")
		defer reported.close()
		// a third transport of the same instance on the second, which
		// forwards to the resident on the first
		clientId := reported.clientId
		splitWs, closeSplitWs, err := dialSessionAdmissionWithInstance(ctx, transportEnv, reported.instanceId, "h1plus", reported.byClientJwt, "")
		if err != nil {
			t.Fatal(err)
		}
		defer closeSplitWs()
		if !waitFor(ctx, 10*time.Second, func() bool {
			connections, resident := transportEnv.exchange.testingHoldsClient(clientId)
			return 1 <= connections && !resident
		}) {
			connections, resident := transportEnv.exchange.testingHoldsClient(clientId)
			t.Fatalf("the second exchange holds connections=%d resident=%t, want a transport only", connections, resident)
		}
		residentConnections, _ := residentEnv.exchange.testingHoldsClient(clientId)
		transportConnections, _ := transportEnv.exchange.testingHoldsClient(clientId)

		recycledBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleRecycled)
		transportOnlyBefore := stuckProviderRecycleTestOutcome(stuckProviderRecycleTransportOnly)
		closesBefore := testutil.ToFloat64(stuckProviderRecycleTransportClosesCounter)
		excusesBefore := testutil.ToFloat64(stuckProviderRecycleExcusesCounter)
		consumedBefore := testutil.ToFloat64(drainExcusesConsumedCounter)

		setClientRecycleTestFlag(ctx, t, clientId)

		for _, c := range []struct {
			name string
			ws   connectlib.H1MessageConn
		}{
			{name: "the resident's exchange", ws: reported.ws},
			{name: "the second exchange", ws: splitWs},
		} {
			reason, closed := readClientRecycleTestClose(t, c.ws, 10*time.Second)
			if !closed || reason != closeReasonMoved {
				t.Fatalf("the transport on %s closed=%t with reason %d, want the moved close %d", c.name, closed, reason, closeReasonMoved)
			}
		}
		if !waitFor(ctx, 10*time.Second, reported.resident.IsDone) || !reported.resident.moved.Load() {
			t.Fatal("the resident did not retire with the moved teardown")
		}
		// one outcome on each exchange: the second's is transport_only, or
		// recycled if its transport nominated there after the first retired
		// the resident and before the second's recycle ran
		if !waitFor(ctx, 10*time.Second, func() bool {
			recycled := stuckProviderRecycleTestOutcome(stuckProviderRecycleRecycled) - recycledBefore
			transportOnly := stuckProviderRecycleTestOutcome(stuckProviderRecycleTransportOnly) - transportOnlyBefore
			return 1 <= recycled && recycled+transportOnly == 2
		}) {
			t.Fatalf(
				"recycled +%v transport_only +%v, want an outcome on each exchange",
				stuckProviderRecycleTestOutcome(stuckProviderRecycleRecycled)-recycledBefore,
				stuckProviderRecycleTestOutcome(stuckProviderRecycleTransportOnly)-transportOnlyBefore,
			)
		}
		if closes := testutil.ToFloat64(stuckProviderRecycleTransportClosesCounter) - closesBefore; closes != float64(residentConnections+transportConnections) {
			t.Fatalf("closed %v transports, want %d here and %d there", closes, residentConnections, transportConnections)
		}
		// each exchange excused the reconnects of the transports it closed
		if excuses := testutil.ToFloat64(stuckProviderRecycleExcusesCounter) - excusesBefore; excuses != 2 {
			t.Fatalf("wrote %v excuses, want one on each exchange", excuses)
		}
		if !waitFor(ctx, 20*time.Second, func() bool {
			resident := residentEnv.exchange.localResident(clientId)
			return resident != nil && resident != reported.resident && !resident.IsDone()
		}) {
			t.Fatal("the client has no fresh resident")
		}
		if !waitFor(ctx, 10*time.Second, func() bool {
			connections, resident := transportEnv.exchange.testingHoldsClient(clientId)
			return connections == 0 && !resident
		}) {
			connections, resident := transportEnv.exchange.testingHoldsClient(clientId)
			t.Fatalf("the second exchange still holds connections=%d resident=%t", connections, resident)
		}
		if !clientRecycleTestExcused(ctx, clientId, consumedBefore) {
			t.Fatal("the recycle's reconnect was not excused")
		}
	})
}

// A test resident in exchange's map, never run: a real inner client, so the
// done checks and the retire behave as for a running one.
func newClientRecycleTestResident(ctx context.Context, exchange *Exchange, clientId server.Id) *Resident {
	residentCtx, residentCancel := context.WithCancel(ctx)
	resident := &Resident{
		ctx:        residentCtx,
		cancel:     residentCancel,
		exchange:   exchange,
		clientId:   clientId,
		instanceId: server.NewId(),
		residentId: server.NewId(),
		client: connectlib.NewClientWithTag(
			residentCtx,
			connectlib.ControlId,
			"recycle-test",
			connectlib.NewNoContractClientOob(),
			exchange.residentClientSettings(),
		),
	}
	exchange.stateLock.Lock()
	defer exchange.stateLock.Unlock()
	exchange.residents[clientId] = resident
	return resident
}

// Writes the client's resident record naming residentId.
func nominateClientRecycleTestResident(ctx context.Context, t testing.TB, resident *Resident, residentId server.Id) {
	t.Helper()
	nominated := model.NominateResident(ctx, nil, &model.NetworkClientResident{
		ClientId:              resident.clientId,
		InstanceId:            resident.instanceId,
		ResidentId:            residentId,
		ResidentHost:          "host0",
		ResidentService:       "connect",
		ResidentBlock:         "test",
		ResidentInternalPorts: []int{1},
	}, time.Minute)
	if !nominated {
		t.Fatal("the resident record was not written")
	}
}

// A transport registered with the exchange as a real one is: its moved close
// counts and unregisters it.
type clientRecycleTestConnection struct {
	movedCloses atomic.Int64
	cancels     atomic.Int64
}

func registerClientRecycleTestConnection(exchange *Exchange, clientId server.Id) *clientRecycleTestConnection {
	connection := &clientRecycleTestConnection{}
	connectionId := server.NewId()
	exchange.registerConnectionWithMovedClose(clientId, connectionId, func() {
		connection.cancels.Add(1)
		exchange.unregisterConnection(clientId, connectionId)
	}, func() {
		connection.movedCloses.Add(1)
		exchange.unregisterConnection(clientId, connectionId)
	})
	return connection
}

// Each outcome of one recycle on one process, with the excuse it writes and
// what it closes and retires:
//   - recycled: a live resident here and transports here: one excuse naming
//     the resident, every transport closed moved, the resident retired
//   - resident_only: a live resident and no transport here: no excuse, the
//     resident retired
//   - replaced: a resident another replaced in the record: retired, with its
//     transports here closed after an excuse
//   - transport_only: transports and no resident here: an excuse with no
//     resident, every transport closed moved
//   - resident_done: an ended resident and no transport: nothing done
//   - removed: a removed client: nothing done, whatever is here
//   - not_held: nothing of the client here
func TestExchangeRecycleOutcomes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := stuckProviderRecycleTestSettings()
		settings.KeyEventDelivery.Enabled = false
		settings.EnableDrainExcuse = true
		exchange := NewExchange(ctx, "host0", "connect", "test", map[int]int{}, map[string]string{"host0": "127.0.0.1"}, settings)
		defer exchange.Close()

		type recycleCounts struct {
			outcomes map[string]float64
			closes   float64
			excuses  float64
		}
		counts := func() recycleCounts {
			outcomes := map[string]float64{}
			for _, outcome := range []string{
				stuckProviderRecycleRecycled,
				stuckProviderRecycleResidentOnly,
				stuckProviderRecycleReplaced,
				stuckProviderRecycleTransportOnly,
				stuckProviderRecycleResidentDone,
				stuckProviderRecycleNotHeld,
				stuckProviderRecycleRemoved,
				stuckProviderRecycleError,
			} {
				outcomes[outcome] = stuckProviderRecycleTestOutcome(outcome)
			}
			return recycleCounts{
				outcomes: outcomes,
				closes:   testutil.ToFloat64(stuckProviderRecycleTransportClosesCounter),
				excuses:  testutil.ToFloat64(stuckProviderRecycleExcusesCounter),
			}
		}
		// recycles clientId and checks the one outcome and the counts
		recycle := func(name string, clientId server.Id, outcome string, closes int, excuses int) {
			t.Helper()
			before := counts()
			exchange.recycleStuckProvider(clientId)
			after := counts()
			for counted, value := range after.outcomes {
				want := before.outcomes[counted]
				if counted == outcome {
					want += 1
				}
				if value != want {
					t.Fatalf("%s: %s +%v, want +%v", name, counted, value-before.outcomes[counted], want-before.outcomes[counted])
				}
			}
			if after.closes-before.closes != float64(closes) {
				t.Fatalf("%s: closed %v transports, want %d", name, after.closes-before.closes, closes)
			}
			if after.excuses-before.excuses != float64(excuses) {
				t.Fatalf("%s: wrote %v excuses, want %d", name, after.excuses-before.excuses, excuses)
			}
		}
		requireExcuse := func(name string, clientId server.Id, residentId *server.Id) {
			t.Helper()
			excuse := model.TakeDrainExcuse(ctx, clientId)
			switch {
			case residentId == nil && excuse != nil:
				t.Fatalf("%s: an excuse was written", name)
			case residentId != nil && excuse == nil:
				t.Fatalf("%s: no excuse was written", name)
			case residentId != nil && *excuse != *residentId:
				t.Fatalf("%s: the excuse names %s, want %s", name, *excuse, *residentId)
			}
		}
		requireMovedCloses := func(name string, connections ...*clientRecycleTestConnection) {
			t.Helper()
			for i, connection := range connections {
				if connection.movedCloses.Load() != 1 || connection.cancels.Load() != 0 {
					t.Fatalf("%s: transport %d closed moved %d times and canceled %d, want one moved close", name, i, connection.movedCloses.Load(), connection.cancels.Load())
				}
			}
		}
		requireRetired := func(name string, resident *Resident, retired bool) {
			t.Helper()
			if resident.IsDone() != retired || resident.moved.Load() != retired {
				t.Fatalf("%s: resident done=%t moved=%t, want retired=%t", name, resident.IsDone(), resident.moved.Load(), retired)
			}
		}

		recycledClientId := server.NewId()
		recycledResident := newClientRecycleTestResident(ctx, exchange, recycledClientId)
		nominateClientRecycleTestResident(ctx, t, recycledResident, recycledResident.residentId)
		recycledConnections := []*clientRecycleTestConnection{
			registerClientRecycleTestConnection(exchange, recycledClientId),
			registerClientRecycleTestConnection(exchange, recycledClientId),
		}
		recycle("recycled", recycledClientId, stuckProviderRecycleRecycled, 2, 1)
		requireExcuse("recycled", recycledClientId, &recycledResident.residentId)
		requireMovedCloses("recycled", recycledConnections...)
		requireRetired("recycled", recycledResident, true)

		residentOnlyClientId := server.NewId()
		residentOnly := newClientRecycleTestResident(ctx, exchange, residentOnlyClientId)
		nominateClientRecycleTestResident(ctx, t, residentOnly, residentOnly.residentId)
		recycle("resident_only", residentOnlyClientId, stuckProviderRecycleResidentOnly, 0, 0)
		requireExcuse("resident_only", residentOnlyClientId, nil)
		requireRetired("resident_only", residentOnly, true)

		replacedClientId := server.NewId()
		replacedResident := newClientRecycleTestResident(ctx, exchange, replacedClientId)
		nominateClientRecycleTestResident(ctx, t, replacedResident, server.NewId())
		replacedConnection := registerClientRecycleTestConnection(exchange, replacedClientId)
		recycle("replaced", replacedClientId, stuckProviderRecycleReplaced, 1, 1)
		requireExcuse("replaced", replacedClientId, &replacedResident.residentId)
		requireMovedCloses("replaced", replacedConnection)
		requireRetired("replaced", replacedResident, true)

		transportOnlyClientId := server.NewId()
		transportOnlyConnection := registerClientRecycleTestConnection(exchange, transportOnlyClientId)
		recycle("transport_only", transportOnlyClientId, stuckProviderRecycleTransportOnly, 1, 1)
		requireExcuse("transport_only", transportOnlyClientId, &server.Id{})
		requireMovedCloses("transport_only", transportOnlyConnection)

		doneClientId := server.NewId()
		doneResident := newClientRecycleTestResident(ctx, exchange, doneClientId)
		doneResident.Cancel()
		recycle("resident_done", doneClientId, stuckProviderRecycleResidentDone, 0, 0)
		requireExcuse("resident_done", doneClientId, nil)
		if doneResident.moved.Load() {
			t.Fatal("resident_done: an ended resident was retired again")
		}

		removedClientId := server.NewId()
		removedResident := newClientRecycleTestResident(ctx, exchange, removedClientId)
		nominateClientRecycleTestResident(ctx, t, removedResident, removedResident.residentId)
		removedConnection := registerClientRecycleTestConnection(exchange, removedClientId)
		model.SetClientsRemoved(ctx, []server.Id{removedClientId})
		recycle("removed", removedClientId, stuckProviderRecycleRemoved, 0, 0)
		requireExcuse("removed", removedClientId, nil)
		requireRetired("removed", removedResident, false)
		if removedConnection.movedCloses.Load() != 0 || removedConnection.cancels.Load() != 0 {
			t.Fatal("removed: a removed client's transport was closed")
		}

		recycle("not_held", server.NewId(), stuckProviderRecycleNotHeld, 0, 0)
	})
}

// A recycle racing a move's deadline on the same resident, and then the
// exchange's drain: each path closes and retires as it would alone, the
// recycle counts one outcome each time, the client comes back after the
// move, and the exchange still closes and goes idle.
func TestExchangeRecycleRacesAMoveAndADrain(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := newClientRecycleTestEnv(ctx, t, func(settings *ExchangeSettings) {
			settings.DrainAllTimeout = 5 * time.Second
			settings.DrainStragglerSweepTimeout = 1 * time.Second
			settings.DrainOneTimeout = 10 * time.Millisecond
		})
		defer env.Close()

		outcomes := func() float64 {
			total := float64(0)
			for _, outcome := range []string{
				stuckProviderRecycleRecycled,
				stuckProviderRecycleResidentOnly,
				stuckProviderRecycleReplaced,
				stuckProviderRecycleTransportOnly,
				stuckProviderRecycleResidentDone,
				stuckProviderRecycleNotHeld,
				stuckProviderRecycleRemoved,
				stuckProviderRecycleError,
			} {
				total += stuckProviderRecycleTestOutcome(outcome)
			}
			return total
		}
		race := func(name string, other func(), clientId server.Id) {
			t.Helper()
			outcomesBefore := outcomes()
			start := make(chan struct{})
			var done sync.WaitGroup
			done.Add(2)
			go func() {
				defer done.Done()
				<-start
				other()
			}()
			go func() {
				defer done.Done()
				<-start
				env.exchange.recycleStuckProvider(clientId)
			}()
			close(start)
			joined := make(chan struct{})
			go func() {
				done.Wait()
				close(joined)
			}()
			select {
			case <-joined:
			case <-time.After(60 * time.Second):
				t.Fatalf("%s: the race did not finish", name)
			}
			if count := outcomes() - outcomesBefore; count != 1 {
				t.Fatalf("%s: the recycle counted %v outcomes, want 1", name, count)
			}
		}

		// a compulsory move's deadline on the same resident
		moving := connectClientRecycleTestClient(ctx, t, env, "moving provider")
		defer moving.close()
		move := &residentMove{
			moveId:    server.NewId(),
			kind:      model.ResidentMoveKindCompulsory,
			closeTime: time.Now(),
		}
		moving.resident.move.Store(move)
		race("a move", func() {
			env.exchange.enforceResidentMoveDeadline(moving.resident, move)
		}, moving.clientId)
		if reason, closed := readClientRecycleTestClose(t, moving.ws, 10*time.Second); !closed || reason != closeReasonMoved {
			t.Fatalf("a move: the transport closed=%t with reason %d, want the moved close", closed, reason)
		}
		if !moving.resident.IsDone() || !moving.resident.moved.Load() {
			t.Fatal("a move: the resident did not retire with the moved teardown")
		}
		if !waitFor(ctx, 20*time.Second, func() bool {
			resident := env.exchange.localResident(moving.clientId)
			return resident != nil && resident != moving.resident && !resident.IsDone()
		}) {
			t.Fatal("a move: the client has no fresh resident")
		}

		// the exchange's drain
		draining := connectClientRecycleTestClient(ctx, t, env, "draining provider")
		defer draining.close()
		race("a drain", env.exchange.Drain, draining.clientId)
		if !waitFor(ctx, 20*time.Second, func() bool {
			connections, _ := env.exchange.testingHoldsClient(draining.clientId)
			return connections == 0 && draining.resident.IsDone()
		}) {
			connections, _ := env.exchange.testingHoldsClient(draining.clientId)
			t.Fatalf("a drain: the client holds %d transports, resident done=%t", connections, draining.resident.IsDone())
		}
	})
}
