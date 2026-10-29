// The runtime switch for the delay header (connect DELAY.md §11): the field
// `transportDelay` of the Redis move control `{connect_move}control`
// (model/connect_move_control.go), which an operator sets to turn the header
// off, and back on, on every edge without a deploy. The control narrows
// EnableTransportDelay and never widens it, so while the field is absent the
// code switch, on by default, decides.
//
// Each handler reads the control at start and then every
// TransportDelayControlRefreshInterval on its own goroutine, and keeps what it
// read, so the H1, H1+ and H3 accept decisions load one atomic and never wait
// on Redis. A new connection follows the last read. A connection keeps what it
// negotiated, since the decision is made once, at its 101 or its auth
// response. A read that fails keeps the last value, and before the first read
// the code switch decides. With the code switch off there is nothing to narrow
// and nothing is read.
//
// P2P is negotiated between the two clients (DELAY.md §6.1), and the edge
// only forwards their signals. The switch still reaches it: a client offers
// the header to a peer only while the latest edge answer to its own platform
// connections accepted it, so P2P follows once the client next connects to an
// edge.
//
// Safe for concurrent use.
package connect

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var transportDelayEnabledGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "transport_delay_enabled",
		Help:      "Whether this process's connect handler accepts the per-message delay header on new connections that offer it: EnableTransportDelay as the Redis control last narrowed it, 1 on, 0 off",
	},
)

func init() {
	prometheus.MustRegister(transportDelayEnabledGauge)
}

// A process runs one handler, whose switch sets the gauge.
func setTransportDelayEnabledGauge(enabled bool) {
	if enabled {
		transportDelayEnabledGauge.Set(1)
	} else {
		transportDelayEnabledGauge.Set(0)
	}
}

// The handler's side of a new connection's delay header: EnableTransportDelay,
// unless the last read of the Redis control turned it off. The connection's
// offer and provide intent decide the rest where it negotiates.
func (self *ConnectHandler) acceptTransportDelay() bool {
	return self.settings.EnableTransportDelay && self.transportDelayControl.allows()
}

// The control's switch for one handler. See the file header.
type connectTransportDelayControl struct {
	ctx      context.Context
	settings *ConnectHandlerSettings
	read     func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)

	// the last good read turned the header off. The zero value allows it, so
	// the code switch decides until a read says otherwise.
	off atomic.Bool

	// owned by the loop: the last read failed. A failure is logged once until
	// a read succeeds.
	failing bool
}

// The handler's switch, which reads the control until ctx ends.
func newConnectTransportDelayControl(ctx context.Context, settings *ConnectHandlerSettings) *connectTransportDelayControl {
	control := newConnectTransportDelayControlWithStore(ctx, settings, model.GetConnectMoveControl)
	setTransportDelayEnabledGauge(settings.EnableTransportDelay)
	if settings.EnableTransportDelay {
		go server.HandleError(control.run)
	}
	return control
}

// The switch over an explicit store, without its loop. Tests drive its reads
// directly.
func newConnectTransportDelayControlWithStore(
	ctx context.Context,
	settings *ConnectHandlerSettings,
	read func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error),
) *connectTransportDelayControl {
	return &connectTransportDelayControl{
		ctx:      ctx,
		settings: settings,
		read:     read,
	}
}

// Whether the control allows the delay header, as of its last good read. A
// handler without a control leaves it to the code switch.
func (self *connectTransportDelayControl) allows() bool {
	return self == nil || !self.off.Load()
}

// Reads the control now, then every TransportDelayControlRefreshInterval until
// ctx ends. A non-positive interval reads once.
func (self *connectTransportDelayControl) run() {
	for {
		self.refresh()
		if self.settings.TransportDelayControlRefreshInterval <= 0 {
			return
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.TransportDelayControlRefreshInterval):
		}
	}
}

// Reads the control once. A read that fails or raises keeps the last value.
// Called only by the loop; tests call it in the loop's place.
func (self *connectTransportDelayControl) refresh() {
	var control *model.ConnectMoveControl
	var err error
	server.HandleError(func() {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.TransportDelayControlReadTimeout)
		defer readCancel()
		control, err = self.read(readCtx, model.ConnectMoveControlCeiling{
			EnableTransportDelay: self.settings.EnableTransportDelay,
		})
	}, func(readErr error) {
		err = readErr
	})
	if err != nil {
		if self.ctx.Err() != nil {
			// the handler is closing
			return
		}
		if !self.failing {
			self.failing = true
			glog.Infof("[c]transport delay control unavailable, keeping accept=%t (%s)\n", self.settings.EnableTransportDelay && self.allows(), err)
		} else if glog.V(1) {
			glog.Infof("[c]transport delay control unavailable (%s)\n", err)
		}
		return
	}
	self.failing = false
	off := !control.TransportDelay
	accept := self.settings.EnableTransportDelay && !off
	if self.off.Swap(off) != off {
		glog.Infof("[c]transport delay control: new connections accept=%t\n", accept)
	}
	setTransportDelayEnabledGauge(accept)
}
