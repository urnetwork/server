// The logical timers that wait on the process's timer sweep (LBALTOPT2.md
// §8.1): each converted owner's timer fires at its deadline or at most one
// sweep tick after it, never before, and leaves the sweep when its owner ends.
// Each test runs the production owner in a synctest bubble with a sweep of
// the bubble's own, armed so that the owner's deadline falls between ticks.
package connect

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/timersweep"
	"github.com/urnetwork/server/model"
)

// A sweep made inside the bubble whose every shard counts its ticks from the
// epoch: one pacer timer per shard keeps each shard's driver running, so an
// owner armed at an offset that is not a multiple of the tick has a deadline
// between two ticks.
type bubbleTimerSweep struct {
	sweep  *timersweep.Sweep
	epoch  time.Time
	pacers []*timersweep.Timer
}

func newBubbleTimerSweep() *bubbleTimerSweep {
	sweep := timersweep.New(timersweep.DefaultTick)
	epoch := time.Now()
	// new timers take the shards in turn, so this many reach every shard
	pacers := make([]*timersweep.Timer, runtime.GOMAXPROCS(0))
	for i := range pacers {
		pacers[i] = sweep.NewTimer(1000 * time.Hour)
	}
	return &bubbleTimerSweep{
		sweep:  sweep,
		epoch:  epoch,
		pacers: pacers,
	}
}

// The sweep's first tick at or after deadline: when a timer with that
// deadline fires.
func (self *bubbleTimerSweep) fireTime(deadline time.Time) time.Time {
	tick := self.sweep.Tick()
	elapsed := deadline.Sub(self.epoch)
	return self.epoch.Add((elapsed + tick - 1) / tick * tick)
}

// The owners' timers armed now, the pacers aside.
func (self *bubbleTimerSweep) ownerArmedCount() int {
	return self.sweep.ArmedCount() - len(self.pacers)
}

// Stops the pacers, so the drivers end with the bubble.
func (self *bubbleTimerSweep) close() {
	for _, pacer := range self.pacers {
		pacer.Stop()
	}
}

// A fire within the sweep's bound: at or after the deadline, less than a tick
// after it, and at the tick the sweep fires that deadline.
func requireSweepFire(t *testing.T, sweep *bubbleTimerSweep, what string, deadline time.Time, at time.Time) {
	t.Helper()
	if at.Before(deadline) {
		t.Fatalf("%s fired %s before its deadline", what, deadline.Sub(at))
	}
	if late := at.Sub(deadline); sweep.sweep.Tick() <= late {
		t.Fatalf("%s fired %s after its deadline, a sweep tick or more", what, late)
	}
	if want := sweep.fireTime(deadline); !at.Equal(want) {
		t.Fatalf("%s fired %s after its deadline, want %s", what, at.Sub(deadline), want.Sub(deadline))
	}
}

// The time the channel closes, recorded on a goroutine of its own.
type closeTime struct {
	stateLock sync.Mutex
	at        time.Time
}

func watchClose(done <-chan struct{}) *closeTime {
	closed := &closeTime{}
	go func() {
		<-done
		closed.stateLock.Lock()
		defer closed.stateLock.Unlock()
		closed.at = time.Now()
	}()
	return closed
}

func (self *closeTime) time() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.at
}

// The resident's idle watcher retires it at the existing idle deadline or at
// most one sweep tick after it, after activity moved that deadline once, and
// takes its timer out of the sweep as it returns.
func TestResidentIdleWatcherRetiresWithinASweepTickOfItsDeadline(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettings()
		resident := newResidentCallbackLifecycleFixture(t, parent, settings)
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		resident.exchange.timerSweep = sweep.sweep

		time.Sleep(37 * time.Millisecond)
		resident.UpdateActivity()
		retired := watchClose(resident.Done())
		done := make(chan struct{})
		go func() { defer close(done); resident.runIdleWatcher() }()

		// activity moves the deadline after the watcher armed for the first
		time.Sleep(100*time.Second + 13*time.Millisecond)
		if !resident.UpdateActivity() {
			t.Fatal("a live resident refused activity")
		}
		deadline := time.Unix(0, resident.lastActivityNanos.Load()).Add(settings.ExchangeResidentTtl)

		time.Sleep(time.Until(deadline) - time.Nanosecond)
		synctest.Wait()
		select {
		case <-done:
			t.Fatalf("the resident retired %s before its idle deadline", time.Until(deadline))
		default:
		}
		time.Sleep(sweep.sweep.Tick())
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the resident outlived its idle deadline by a sweep tick")
		}
		requireSweepFire(t, sweep, "the resident's idle watcher", deadline, retired.time())
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the idle watcher returned", count)
		}
	})
}

// The watcher of a resident whose parent ends returns at once and takes its
// timer out of the sweep.
func TestResidentIdleWatcherLeavesTheSweepWhenItsResidentEnds(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		resident := newResidentCallbackLifecycleFixture(t, parent, DefaultExchangeSettings())
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		resident.exchange.timerSweep = sweep.sweep
		resident.UpdateActivity()
		done := make(chan struct{})
		go func() { defer close(done); resident.runIdleWatcher() }()
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 1 {
			t.Fatalf("the idle watcher armed %d sweep timers, want 1", count)
		}
		time.Sleep(time.Minute)
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the idle watcher outlived its resident")
		}
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the idle watcher returned", count)
		}
	})
}

// A forward's idle watcher expires it at the last payload's idle deadline or
// at most one sweep tick after it, never before, and a canceled forward's
// watcher leaves the sweep.
func TestResidentForwardIdleWatcherExpiresWithinASweepTickOfItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExchangeSettingsWithBufferSize(4)
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		exchange := &Exchange{settings: settings, timerSweep: sweep.sweep}

		time.Sleep(59 * time.Millisecond)
		forward := NewResidentForward(context.Background(), exchange, server.NewId())
		defer forward.Cancel()
		expired := watchClose(forward.Done())
		done := make(chan struct{})
		go func() { defer close(done); forward.runIdleWatcher(server.NewId()) }()

		time.Sleep(time.Minute + 71*time.Millisecond)
		if !forward.UpdateActivity() {
			t.Fatal("a live forward refused payload activity")
		}
		deadline := time.Unix(0, forward.lastActivityNanos.Load()).Add(settings.ForwardIdleTimeout)

		time.Sleep(time.Until(deadline) - time.Nanosecond)
		synctest.Wait()
		if forward.IsDone() {
			t.Fatalf("the forward expired %s before its idle deadline", time.Until(deadline))
		}
		time.Sleep(sweep.sweep.Tick())
		synctest.Wait()
		if !forward.IsDone() {
			t.Fatal("the forward outlived its idle deadline by a sweep tick")
		}
		<-done
		requireSweepFire(t, sweep, "the forward's idle watcher", deadline, expired.time())
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the idle watcher returned", count)
		}

		// a canceled forward
		canceled := NewResidentForward(context.Background(), exchange, server.NewId())
		defer canceled.Cancel()
		canceledDone := make(chan struct{})
		go func() { defer close(canceledDone); canceled.runIdleWatcher(server.NewId()) }()
		time.Sleep(time.Minute)
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 1 {
			t.Fatalf("the idle watcher armed %d sweep timers, want 1", count)
		}
		canceled.Cancel()
		synctest.Wait()
		select {
		case <-canceledDone:
		default:
			t.Fatal("the idle watcher outlived its canceled forward")
		}
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the canceled forward's watcher returned", count)
		}
	})
}

// Each registration refresh comes ExchangeResidentTtl/4 after the last one
// ended, or at most one sweep tick later, never sooner. A refresh that finds
// the resident replaced ends the loop, which cancels the resident and takes
// its timer out of the sweep.
func TestResidentRegistrationRefreshWaitsOnTheSweep(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettings()
		resident := newResidentCallbackLifecycleFixture(t, parent, settings)
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		exchange := resident.exchange
		exchange.timerSweep = sweep.sweep
		const refreshCount = 5
		// each refresh takes a while, as its Redis and database work does
		const refreshDuration = 23 * time.Millisecond
		var refreshTimes []time.Time
		exchange.refreshResidentRegistrationForTest = func(r *Resident) bool {
			if r != resident {
				t.Error("refreshed another resident")
			}
			refreshTimes = append(refreshTimes, time.Now())
			time.Sleep(refreshDuration)
			return len(refreshTimes) < refreshCount
		}

		time.Sleep(41 * time.Millisecond)
		start := time.Now()
		done := make(chan struct{})
		go func() { defer close(done); exchange.runResidentRegistrationRefresh(resident) }()
		<-done
		if len(refreshTimes) != refreshCount {
			t.Fatalf("%d refreshes, want %d", len(refreshTimes), refreshCount)
		}
		interval := settings.ExchangeResidentTtl / 4
		previousEnd := start
		for i, at := range refreshTimes {
			requireSweepFire(t, sweep, "a registration refresh", previousEnd.Add(interval), at)
			previousEnd = at.Add(refreshDuration)
			if i == 0 && at.Sub(start) < interval {
				t.Fatalf("the first refresh came %s after the start, before %s", at.Sub(start), interval)
			}
		}
		select {
		case <-resident.Done():
		default:
			t.Fatal("a replaced resident's refresh loop ended without canceling it")
		}
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the refresh loop returned", count)
		}
	})
}

// A resident that ends stops its refresh loop at once, before the next
// refresh, and the loop's timer leaves the sweep.
func TestResidentRegistrationRefreshLeavesTheSweepWhenItsResidentEnds(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		resident := newResidentCallbackLifecycleFixture(t, parent, DefaultExchangeSettings())
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		exchange := resident.exchange
		exchange.timerSweep = sweep.sweep
		exchange.refreshResidentRegistrationForTest = func(*Resident) bool {
			t.Error("an ended resident was refreshed")
			return false
		}
		done := make(chan struct{})
		go func() { defer close(done); exchange.runResidentRegistrationRefresh(resident) }()
		time.Sleep(time.Minute)
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 1 {
			t.Fatalf("the refresh loop armed %d sweep timers, want 1", count)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the refresh loop outlived its resident")
		}
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the refresh loop returned", count)
		}
	})
}

// A resident's stream authorizations republish on one ticker of the
// exchange's sweep, which leaves the sweep when the resident ends. The
// ticker's cadence and bound are the sweep's, tested in internal/timersweep.
func TestResidentStreamAuthorizationsTickOnTheSweep(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		resident := newResidentCallbackLifecycleFixture(t, parent, DefaultExchangeSettings())
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		resident.exchange.timerSweep = sweep.sweep
		snapshots := make(chan []model.StreamHop)
		done := make(chan struct{})
		go func() { defer close(done); resident.runStreamAuthorizations(snapshots) }()
		time.Sleep(10*streamAuthorizationRepublishInterval + 7*time.Millisecond)
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 1 {
			t.Fatalf("the stream authorizations armed %d sweep timers, want their one ticker", count)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the stream authorizations outlived their resident")
		}
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the stream authorizations returned", count)
		}
	})
}

// Each passive sample closes its window PassiveSpeedWindowDuration after the
// last sample or at most one sweep tick later, never sooner. The publisher's
// end takes its timer out of the sweep.
func TestConnectionAnnounceMeasurementPublisherSamplesWithinASweepTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newBubbleTimerSweep()
		defer sweep.close()
		settings := DefaultConnectionAnnounceSettings()
		settings.timerSweep = sweep.sweep
		time.Sleep(43 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		connectionId := server.NewId()
		start := time.Now()
		announce := &ConnectionAnnounce{
			ctx: ctx, cancel: cancel, connectionId: &connectionId,
			settings: settings, testConfig: V0TestConfig(),
			passiveWindowStartTime: start,
			measurementWake:        make(chan struct{}, 1),
			measurementSnapshot:    connectionMeasurements{connectionId: connectionId},
		}
		windowStart := func() time.Time {
			announce.stateLock.Lock()
			defer announce.stateLock.Unlock()
			return announce.passiveWindowStartTime
		}
		announce.startWorker(announce.runMeasurementPublisher)
		window := settings.PassiveSpeedWindowDuration
		previous := start
		for range 20 {
			deadline := previous.Add(window)
			time.Sleep(time.Until(deadline) - time.Nanosecond)
			synctest.Wait()
			if sampled := windowStart(); !sampled.Equal(previous) {
				t.Fatalf("a passive sample came %s before its window's end", deadline.Sub(sampled))
			}
			time.Sleep(sweep.sweep.Tick())
			synctest.Wait()
			sampled := windowStart()
			if sampled.Equal(previous) {
				t.Fatal("a passive window ran a sweep tick past its end")
			}
			requireSweepFire(t, sweep, "a passive sample", deadline, sampled)
			previous = sampled
		}
		cancel()
		announce.closeWorkersAndWait()
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the publisher returned", count)
		}
	})
}

// One timer sweep per process: the exchange makes it, and a handler on that
// exchange gives it to its announcements. The handler's two lease starts pass
// the same exchange's sweep.
func TestExchangeTimerSweepReachesTheHandlersAnnouncements(t *testing.T) {
	settings := DefaultExchangeSettings()
	settings.KeyEventDelivery.Enabled = false
	exchangeCtx, stopExchange := context.WithCancel(context.Background())
	stopExchange()
	exchange := NewExchange(exchangeCtx, "synthetic", "connect", "synthetic", nil, nil, settings)
	defer exchange.Close()
	if exchange.timerSweep == nil {
		t.Fatal("the exchange made no timer sweep")
	}
	if tick := exchange.timerSweep.Tick(); tick != timersweep.DefaultTick {
		t.Fatalf("the exchange's sweep ticks every %s, want %s", tick, timersweep.DefaultTick)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlerSettings := DefaultConnectHandlerSettings()
	handlerSettings.ListenH3Port = 0
	handlerSettings.ListenDnsPort = 0
	handlerSettings.TransportTlsSettings.EnableSelfSign = true
	handlerSettings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	handlerSettings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
		return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
	}
	handler, err := newConnectHandlerWithPacketConns(ctx, server.NewId(), exchange, handlerSettings, ConnectHandlerPacketConns{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		handler.Close()
		idleCtx, idleCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer idleCancel()
		if !handler.WaitForIdle(idleCtx) {
			t.Error("the handler did not stop")
		}
	}()
	if handlerSettings.ConnectionAnnounceSettings.timerSweep != exchange.timerSweep {
		t.Fatal("the handler's announcements do not wait on the exchange's sweep")
	}
}
