package proxy

// Setup-time metrics (P90SPEED.md §8): how long a customer's CONNECT takes
// from the proxy taking it to the destination's first byte, where that time
// goes (the wait for the window's first exit, the first dial through a new
// device), and how the CONNECT's device was opened. Before these, no server
// metric measured a customer's first CONNECT; the hosted proxy test runner was
// the only view.
//
// The `open` label says how the device was opened:
//   - cold: the construction of the proxy's first device;
//   - reopen: a construction for an older proxy, whose device closed idle or
//     whose process restarted;
//   - warm: the CONNECT found its device installed before it began.
//
// A construction is cold when its proxy is younger than
// proxyDeviceColdOpenMaxAge, which the proxy id says: ids lead with their
// creation millisecond (connect.NewId). A CONNECT that began before its
// device was built, or as the build ended, waited on the construction, so it
// takes the construction's kind; so does each of its retried dial attempts,
// since the CONNECT's start is read from its dial budget.
//
// The series carry no proxy, client or network id; the scrape adds the host
// and block. Every histogram shares proxySetupBuckets, so their cardinality is
// fixed: 13 buckets for each of 18 label sets per process.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
)

// The values of the `open` label.
const (
	proxyOpenCold   = "cold"
	proxyOpenReopen = "reopen"
	proxyOpenWarm   = "warm"
)

// No device of a proxy younger than the unused-device hold
// (ProxyDeviceUnusedIdleTimeout, ten minutes) has closed idle, so a
// construction that begins within ten minutes of the proxy's creation is the
// proxy's first: the proxy test runner's case, which creates a proxy and
// connects at once. A later construction counts as a reopen. That is what it
// is, unless the proxy's first use came more than ten minutes after its
// creation.
const proxyDeviceColdOpenMaxAge = 10 * time.Minute

// A proxy created by an api host whose clock runs slightly ahead of this one
// still reads as new.
const proxyDeviceCreateClockSkew = time.Minute

// Seconds. Warm CONNECTs and their route waits sit in the first buckets; a
// CONNECT's dial budget is 30 s, and the first byte can come later than that.
var proxySetupBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 8, 13, 20, 30, 60}

var proxyConnectSetupSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "connect_setup_seconds",
		Help:      "Time from the proxy taking a CONNECT (http: the start of its dial budget; socks: the request) to the destination's first byte through the new tunnel, by protocol and how the device was opened (cold, reopen, warm)",
		Buckets:   proxySetupBuckets,
	},
	[]string{"protocol", "open"},
)

var proxyConnectRouteWaitSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "connect_route_wait_seconds",
		Help:      "Each CONNECT dial attempt's wait for its device's window to hold an exit (WaitForRoute), by how the device was opened and whether an exit came (routed) or the wait ended without one (unrouted)",
		Buckets:   proxySetupBuckets,
	},
	[]string{"open", "outcome"},
)

var proxyDeviceFirstExitSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_first_exit_seconds",
		Help:      "Time from the start of a device's construction to its window's first added exit, by how the device was opened (cold, reopen)",
		Buckets:   proxySetupBuckets,
	},
	[]string{"open"},
)

var proxyDeviceFirstDialSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_first_dial_seconds",
		Help:      "The first dial through each new device (its DNS and TCP handshake through the window), by how the device was opened (cold, reopen) and outcome (ok, error)",
		Buckets:   proxySetupBuckets,
	},
	[]string{"open", "outcome"},
)

var proxyDeviceBuilds = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_builds_total",
		Help:      "Proxy devices constructed and installed, by how they were opened (cold: the proxy's first device; reopen: an older proxy's)",
	},
	[]string{"open"},
)

func init() {
	prometheus.MustRegister(
		proxyConnectSetupSeconds,
		proxyConnectRouteWaitSeconds,
		proxyDeviceFirstExitSeconds,
		proxyDeviceFirstDialSeconds,
		proxyDeviceBuilds,
	)
}

// How a construction for the proxy that began at buildStart opened its device.
func proxyDeviceOpenKind(proxyId server.Id, buildStart time.Time) string {
	// server.NewId leads every id with its creation millisecond, as it does a
	// contract id's
	age := buildStart.Sub(server.ContractIdTime(proxyId))
	if -proxyDeviceCreateClockSkew < age && age < proxyDeviceColdOpenMaxAge {
		return proxyOpenCold
	}
	return proxyOpenReopen
}

// When the CONNECT whose dial context is ctx began. The proxy library gives a
// CONNECT one budget for all its dial attempts and carries it as the dial
// context's deadline (proxy http.go handleHttps), so the CONNECT began one
// budget before that deadline, whichever attempt this is. A context without a
// deadline began now, and a start after now is now.
func proxyConnectStart(ctx context.Context, budget time.Duration, now time.Time) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok || budget <= 0 {
		return now
	}
	start := deadline.Add(-budget)
	if now.Before(start) {
		return now
	}
	return start
}

// How and when the manager opened a device. Set once, before the device is
// published, so every opener that finds the device finds it.
type proxyDeviceOpen struct {
	// cold or reopen
	kind string
	// the construction began
	start time.Time
	// the construction returned the device, just before its publication
	built time.Time
}

// A device's setup state. The zero value is a device the manager did not
// build (tests), which counts as warm and observes nothing per device.
type proxyDeviceSetup struct {
	open          atomic.Pointer[proxyDeviceOpen]
	firstDialDone atomic.Bool
	firstExit     atomic.Pointer[proxyDeviceFirstExitWatch]
}

// Records how a construction that began at buildStart opened the device,
// before the device is published.
func (self *proxyDeviceSetup) built(proxyId server.Id, buildStart time.Time, builtTime time.Time) {
	self.open.Store(&proxyDeviceOpen{
		kind:  proxyDeviceOpenKind(proxyId, buildStart),
		start: buildStart,
		built: builtTime,
	})
}

// Counts an installed device and watches for its window's first exit.
func (self *proxyDeviceSetup) installed(source proxyDeviceWindowStatusSource) {
	open := self.open.Load()
	if open == nil {
		return
	}
	proxyDeviceBuilds.WithLabelValues(open.kind).Inc()
	if source == nil {
		return
	}
	watch := &proxyDeviceFirstExitWatch{
		start:    open.start,
		observer: proxyDeviceFirstExitSeconds.WithLabelValues(open.kind),
	}
	self.firstExit.Store(watch)
	watch.watch(source)
}

// Releases the first-exit watch of a device that closes without an exit.
func (self *proxyDeviceSetup) closed() {
	if watch := self.firstExit.Load(); watch != nil {
		watch.finish(false)
	}
}

// How a CONNECT that began at connectStart found the device: warm when it
// began after the device was built, else the construction's kind, which the
// CONNECT started or waited on.
func (self *proxyDeviceSetup) connectOpenKind(connectStart time.Time) string {
	open := self.open.Load()
	if open == nil || connectStart.After(open.built) {
		return proxyOpenWarm
	}
	return open.kind
}

// Observes one CONNECT's wait for an exit.
func (self *proxyDeviceSetup) observeRouteWait(connectStart time.Time, routed bool, waited time.Duration) {
	outcome := "routed"
	if !routed {
		outcome = "unrouted"
	}
	proxyConnectRouteWaitSeconds.WithLabelValues(self.connectOpenKind(connectStart), outcome).Observe(waited.Seconds())
}

// Observes the device's first dial, once.
func (self *proxyDeviceSetup) observeDial(elapsed time.Duration, err error) {
	open := self.open.Load()
	if open == nil || self.firstDialDone.Load() || !self.firstDialDone.CompareAndSwap(false, true) {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	proxyDeviceFirstDialSeconds.WithLabelValues(open.kind, outcome).Observe(elapsed.Seconds())
}

// The first-byte observation of one CONNECT, which the connection's relay
// makes on the destination's first byte.
func (self *proxyDeviceSetup) connectSetup(protocol string, connectStart time.Time) *proxyConnectFirstByte {
	return &proxyConnectFirstByte{
		start:    connectStart,
		observer: proxyConnectSetupSeconds.WithLabelValues(boundedProxyProtocol(protocol), self.connectOpenKind(connectStart)),
	}
}

// Observes the time from a CONNECT's start to the destination's first byte,
// once. The relay calls observe on every read that returned bytes, so after
// the first it costs one atomic load.
type proxyConnectFirstByte struct {
	start    time.Time
	observer prometheus.Observer
	observed atomic.Bool
}

func (self *proxyConnectFirstByte) observe() {
	if self.observed.Load() || !self.observed.CompareAndSwap(false, true) {
		return
	}
	self.observer.Observe(time.Since(self.start).Seconds())
}

// Waits for a device's window to hold its first exit, as WaitForRoute does,
// without a goroutine: a status listener observes the time since the device's
// construction began and removes itself. A device that closes first observes
// nothing.
type proxyDeviceFirstExitWatch struct {
	start    time.Time
	observer prometheus.Observer

	stateLock sync.Mutex
	sub       sdk.Sub
	done      bool
}

// Subscribes before it reads the status, so an exit added between the two is
// still seen (waitForProxyDeviceWindow's order).
func (self *proxyDeviceFirstExitWatch) watch(source proxyDeviceWindowStatusSource) {
	sub := source.AddWindowStatusChangeListener(self)
	self.stateLock.Lock()
	if self.done {
		self.stateLock.Unlock()
		sub.Close()
		return
	}
	self.sub = sub
	self.stateLock.Unlock()
	if windowRoutable(source.GetWindowStatus()) {
		self.finish(true)
	}
}

// conforms to `sdk.WindowStatusChangeListener`
func (self *proxyDeviceFirstExitWatch) WindowStatusChanged(windowStatus *sdk.WindowStatus) {
	if windowRoutable(windowStatus) {
		self.finish(true)
	}
}

// Ends the watch once, observing when the window holds its first exit. The
// listener is removed outside the lock: the status callbacks run from a
// snapshot of the listeners, so removing it from within one is safe.
func (self *proxyDeviceFirstExitWatch) finish(observe bool) {
	self.stateLock.Lock()
	if self.done {
		self.stateLock.Unlock()
		return
	}
	self.done = true
	sub := self.sub
	self.sub = nil
	self.stateLock.Unlock()
	if observe {
		self.observer.Observe(time.Since(self.start).Seconds())
	}
	if sub != nil {
		sub.Close()
	}
}
