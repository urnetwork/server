// The announcement's own waits on the process's timer sweep (LBALTOPT2.md
// §8.1): the two halves of the announce window, the sync rounds, the
// connection test schedule and the verify egress refreshes. run() registers
// its connection on the database, so this runs in real time on the test DB
// env, like the rest of this package's announcement lifecycle tests, rather
// than in a synctest bubble. Each fire is checked against its own arm: never
// early, and within a sweep tick and a scheduling allowance. The sweep is the
// announcement's alone, so its end leaves no timer armed there.
package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/internal/timersweep"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Real-time wakes beyond the sweep's own lateness: the sweep driver's and the
// owner goroutine's, on a test host that may be busy.
const announceTimerSchedulingAllowance = 250 * time.Millisecond

type announceTimerArm struct {
	at      time.Time
	timeout time.Duration
}

// Every arm and fire of the announcement's sweep timers, by kind. For one
// kind the arms and fires alternate, each fire answering the arm before it.
type announceTimerLog struct {
	stateLock sync.Mutex
	arms      map[string][]announceTimerArm
	fires     map[string][]time.Time
}

func newAnnounceTimerLog() *announceTimerLog {
	return &announceTimerLog{
		arms:  map[string][]announceTimerArm{},
		fires: map[string][]time.Time{},
	}
}

func (self *announceTimerLog) armed(kind string, timeout time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.arms[kind] = append(self.arms[kind], announceTimerArm{at: time.Now(), timeout: timeout})
}

func (self *announceTimerLog) fired(kind string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fires[kind] = append(self.fires[kind], time.Now())
}

// Checks every fire of kind against its arm and returns how many fired.
func (self *announceTimerLog) requireFires(t testing.TB, kind string, tick time.Duration) int {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	arms := self.arms[kind]
	fires := self.fires[kind]
	if len(arms) < len(fires) || len(fires)+1 < len(arms) {
		t.Fatalf("%d %s arms for %d fires", len(arms), kind, len(fires))
	}
	maxLate := time.Duration(0)
	for i, at := range fires {
		arm := arms[i]
		if i+1 < len(arms) && arms[i+1].at.Before(at) {
			t.Fatalf("the %s timer was armed again before fire %d", kind, i)
		}
		waited := at.Sub(arm.at)
		if waited < arm.timeout {
			t.Fatalf("%s fire %d came %s after its arm, before its %s timeout", kind, i, waited, arm.timeout)
		}
		late := waited - arm.timeout
		if tick+announceTimerSchedulingAllowance <= late {
			t.Fatalf("%s fire %d came %s after its %s timeout, past a sweep tick", kind, i, late, arm.timeout)
		}
		maxLate = max(maxLate, late)
	}
	t.Logf("%d %s fires, the latest %s after its timeout", len(fires), kind, maxLate)
	return len(fires)
}

func TestConnectionAnnounceTimersWaitOnTheSweep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		// a public provider, so each round records stats and the window tests
		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
			model.ProvideModePublic: make([]byte, 32),
		})

		sweep := timersweep.New(timersweep.DefaultTick)
		verifySettings := model.DefaultVerifySettings()
		verifySettings.EgressRefreshInterval = 450 * time.Millisecond
		settings := DefaultConnectionAnnounceSettings()
		settings.timerSweep = sweep
		settings.SyncConnectionTimeout = 350 * time.Millisecond
		settings.LocationRetryTimeout = 0
		settings.MaxLatencyCount = 1
		// the schedule's first test a MaxTestTimeout in, then every 550 ms
		settings.MinTestTimeout = 550 * time.Millisecond
		settings.MaxTestTimeout = settings.MinTestTimeout + time.Nanosecond
		settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
			return verifySettings, true
		}
		timers := newAnnounceTimerLog()
		settings.announceTimerArmedForTest = timers.armed
		settings.announceTimerFiredForTest = timers.fired

		transportCtx, transportCancel := context.WithCancel(ctx)
		defer transportCancel()
		announce := NewConnectionAnnounceWithIpFamily(
			controller.Testing_WithConnectionLocationWriter(
				transportCtx,
				func(context.Context, server.Id, string) error {
					return nil
				},
			),
			transportCancel,
			networkId,
			clientId,
			locationAnnounceClientAddress,
			0,
			"",
			server.NewId(),
			time.Second,
			&TestConfig{MaxLatencyCount: 1},
			settings,
		)
		// answers each connection test, so the next one waits for the schedule
		answered := make(chan struct{})
		go func() {
			defer close(answered)
			for {
				select {
				case <-transportCtx.Done():
					return
				case test := <-announce.PendingLatencyTest:
					announce.SendLatency(test)
					announce.ReceiveLatency(test)
				}
			}
		}()
		time.Sleep(4 * time.Second)
		if err := transportCtx.Err(); err != nil {
			t.Fatalf("the announcement ended its transport: %v", err)
		}
		announce.CloseAndWait()
		<-answered

		tick := sweep.Tick()
		if n := timers.requireFires(t, announceTimerWindow, tick); n != 2 {
			t.Fatalf("the announce window fired %d times, want its 2 halves", n)
		}
		if n := timers.requireFires(t, announceTimerSync, tick); n < 4 {
			t.Fatalf("%d sync rounds in 3 s at %s", n, settings.SyncConnectionTimeout)
		}
		if n := timers.requireFires(t, announceTimerTest, tick); n < 3 {
			t.Fatalf("%d scheduled connection tests in 3 s at %s", n, settings.MinTestTimeout)
		}
		if n := timers.requireFires(t, announceTimerVerify, tick); n < 3 {
			t.Fatalf("%d verify egress refreshes in 3 s at %s", n, verifySettings.EgressRefreshInterval)
		}
		if count := sweep.ArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the announcement ended", count)
		}
	})
}
