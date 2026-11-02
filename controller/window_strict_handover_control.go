package controller

// The platform's off switch for the apps' strict window handover (CPUOPT3 D2):
// the Redis move control's `windowStrictHandover`
// (model/connect_move_control.go), which every find-providers2 answer the api
// serves over http carries as `window_strict_handover`
// (api/handlers/network_client_location_handlers.go). An app's windows take it
// from each listing (connect MultiClientGeneratorWithStrictWindowHandoverOverride):
// off turns the handover off, while on and absent leave the app's own setting,
// so the switch can turn the handover off but never on. Hosted proxy devices
// discover in process (localclient) and never see it; the proxy keeps its own
// proxy.yml switch.
//
// The owner reads the control every ControlRefreshInterval and keeps its last
// good read, so an answer never reads Redis. Until the first good read, and
// while the control sets nothing or a value that does not parse, it overrides
// nothing.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var windowStrictHandoverOverrideGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "api",
	Name:      "window_strict_handover_override",
	Help:      "The override of the apps' strict window handover this process's find-providers2 answers carry, as the Redis control last set it: -1 off, 1 on, 0 none",
})

func init() {
	prometheus.MustRegister(windowStrictHandoverOverrideGauge)
}

type WindowStrictHandoverControlSettings struct {
	// How often the owner reads the control. Non-positive reads it once, at
	// start.
	ControlRefreshInterval time.Duration
	// Bounds one read of the control.
	ControlReadTimeout time.Duration
}

func DefaultWindowStrictHandoverControlSettings() *WindowStrictHandoverControlSettings {
	return &WindowStrictHandoverControlSettings{
		ControlRefreshInterval: 10 * time.Second,
		ControlReadTimeout:     5 * time.Second,
	}
}

// The override as the owner holds it.
const (
	windowStrictHandoverOverrideNone int32 = 0
	windowStrictHandoverOverrideOn   int32 = 1
	windowStrictHandoverOverrideOff  int32 = -1
)

// The api's view of the switch. Safe for concurrent use; every method of a nil
// owner overrides nothing.
type WindowStrictHandoverControl struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *WindowStrictHandoverControlSettings

	readControl func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)

	// as of the last good read
	override atomic.Int32

	workers sync.WaitGroup

	// owned by the control loop: the last read failed. A failure is logged
	// once until a read succeeds.
	failing bool
}

// The owner, which reads the control until ctx ends or Close.
func NewWindowStrictHandoverControl(ctx context.Context, settings *WindowStrictHandoverControlSettings) *WindowStrictHandoverControl {
	control := newWindowStrictHandoverControlWithStore(ctx, settings, model.GetConnectMoveControl)
	control.start()
	return control
}

// The owner over an explicit store, without its control loop. Tests drive
// refresh directly.
func newWindowStrictHandoverControlWithStore(
	ctx context.Context,
	settings *WindowStrictHandoverControlSettings,
	readControl func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error),
) *WindowStrictHandoverControl {
	cancelCtx, cancel := context.WithCancel(ctx)
	control := &WindowStrictHandoverControl{
		ctx:         cancelCtx,
		cancel:      cancel,
		settings:    settings,
		readControl: readControl,
	}
	windowStrictHandoverOverrideGauge.Set(float64(windowStrictHandoverOverrideNone))
	return control
}

// The override an answer carries: set is false for none.
func (self *WindowStrictHandoverControl) Override() (enabled bool, set bool) {
	if self == nil {
		return false, false
	}
	switch self.override.Load() {
	case windowStrictHandoverOverrideOn:
		return true, true
	case windowStrictHandoverOverrideOff:
		return false, true
	default:
		return false, false
	}
}

// Starts the control loop.
func (self *WindowStrictHandoverControl) start() {
	self.workers.Add(1)
	go server.HandleError(func() {
		defer self.workers.Done()
		self.run()
	})
}

// Reads the control now, then every ControlRefreshInterval until the owner
// ends, on one reused timer. A non-positive interval reads once.
func (self *WindowStrictHandoverControl) run() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		self.refresh()
		if self.settings.ControlRefreshInterval <= 0 {
			return
		}
		timer.Reset(self.settings.ControlRefreshInterval)
		select {
		case <-self.ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// Reads the control once. A read that fails or raises keeps the last value.
// Called only by the loop; tests call it in the loop's place.
func (self *WindowStrictHandoverControl) refresh() {
	var control *model.ConnectMoveControl
	var err error
	server.HandleError(func() {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.ControlReadTimeout)
		defer readCancel()
		// the field reads no ceiling
		control, err = self.readControl(readCtx, model.ConnectMoveControlCeiling{})
	}, func(readErr error) {
		err = readErr
	})
	if err != nil {
		if self.ctx.Err() != nil {
			// the owner is closing
			return
		}
		if !self.failing {
			self.failing = true
			glog.Infof("[window]strict window handover control unavailable, keeping %s (%s)\n", windowStrictHandoverOverrideName(self.override.Load()), err)
		} else if glog.V(1) {
			glog.Infof("[window]strict window handover control unavailable (%s)\n", err)
		}
		return
	}
	self.failing = false
	override := windowStrictHandoverOverrideNone
	if control.WindowStrictHandover != nil {
		if *control.WindowStrictHandover {
			override = windowStrictHandoverOverrideOn
		} else {
			override = windowStrictHandoverOverrideOff
		}
	}
	if previous := self.override.Swap(override); previous != override {
		glog.Infof("[window]strict window handover override %s -> %s\n", windowStrictHandoverOverrideName(previous), windowStrictHandoverOverrideName(override))
	}
	windowStrictHandoverOverrideGauge.Set(float64(override))
}

func windowStrictHandoverOverrideName(override int32) string {
	switch override {
	case windowStrictHandoverOverrideOn:
		return "on"
	case windowStrictHandoverOverrideOff:
		return "off"
	default:
		return "none"
	}
}

// Ends the control loop and waits for it.
func (self *WindowStrictHandoverControl) Close() {
	if self == nil {
		return
	}
	self.cancel()
	self.workers.Wait()
}
