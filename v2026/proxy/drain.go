package proxy

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
)

// drainActiveRemainingGauge is the drain progress without ssh-ing to find the
// `docker stop` child: in-flight socks/http connections remaining while the
// service drains, 0 once the drain completes (PROXYDRAIN1.md §3.1)
var drainActiveRemainingGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "drain_active_remaining",
		Help:      "In-flight socks/http connections remaining while the service drains; 0 when the drain completes",
	},
)

func init() {
	prometheus.MustRegister(drainActiveRemainingGauge)
}

// drainable is the graceful-drain surface of an ingress: stop accepting, keep
// relaying, report progress. The socks and http servers implement it by
// delegating to the lib proxies (github.com/urnetwork/proxy drainState).
type drainable interface {
	// Drain closes the listeners and refuses new requests; in-flight
	// connections keep relaying until the process's root ctx is canceled.
	Drain()
	// ActiveCount reports in-flight connections.
	ActiveCount() int
	// WaitIdle blocks until a drain has begun and no connections are active,
	// or ctx is done; true when idle was reached.
	WaitIdle(ctx context.Context) bool
}

// DrainCoordinator runs the proxy's graceful shutdown (PROXYDRAIN1.md §3.2),
// filling in the `FIXME drain` in cli/proxy/main.go.
//
// Sequence, on SIGTERM:
//  1. flip readiness to draining (observability; the DNAT flip already
//     steered new flows to the replacement container before the SIGTERM)
//  2. drain the socks/http ingress: listeners close, established tunnels
//     keep relaying
//  3. wait until the tunnels end or `DrainGraceTimeout` passes — the wait
//     is socks/http-idle-bounded, with the grace as its CEILING, not its
//     duration. The wg ingress needs no eviction step: its conntrack-pinned
//     clients CANNOT migrate until this process exits and warpctl flushes
//     their conntrack entries, so wg serves for as long as the socks/http
//     drain keeps the process alive. A wg-only block therefore exits soon
//     after SIGTERM — beneficial: the prompt exit advances the drain-end
//     export (the replacement's drain-complete beacon, step 4) and the
//     conntrack flush, and the replacement re-establishes the wg sessions
//     from its side in ~1 RTT (PROXYDRAIN1.md §3.4). There is no straggler
//     pacing on purpose — unlike connect, evicting a tunnel here does not
//     push a redial onto this process (new connections land on the
//     replacement container), so there is no refill loop to pace against.
//  4. run the before-exit hooks (e.g. the wg endpoint handoff export,
//     PROXYDRAIN1.md §3.4), bounded by `DrainBeforeExitTimeout`
//  5. return, letting main cancel the root ctx (the hard teardown for
//     stragglers and the wg device) and exit immediately — never idling
//     toward the `docker stop -t` ceiling (CONNECTDRAIN2.md §3.4's lesson)
type DrainCoordinator struct {
	readiness *Readiness
	settings  *ProxySettings

	targets     []drainable
	beforeExits []func(context.Context)

	drained atomic.Bool
}

func NewDrainCoordinator(readiness *Readiness, settings *ProxySettings) *DrainCoordinator {
	return &DrainCoordinator{
		readiness: readiness,
		settings:  settings,
	}
}

// AddTarget registers an ingress to drain. Not safe to call concurrently
// with Drain; register everything at startup.
func (self *DrainCoordinator) AddTarget(target drainable) {
	self.targets = append(self.targets, target)
}

// AddBeforeExit registers a hook to run after the grace wait, right before
// the process tears down — e.g. exporting the wg peer endpoints for the
// replacement container (PROXYDRAIN1.md §3.4). Not safe to call concurrently
// with Drain; register everything at startup.
func (self *DrainCoordinator) AddBeforeExit(beforeExit func(context.Context)) {
	self.beforeExits = append(self.beforeExits, beforeExit)
}

// Drained reports whether a drain ran to completion (idle or deadline), so
// main can exit 0 for a clean drained shutdown.
func (self *DrainCoordinator) Drained() bool {
	return self.drained.Load()
}

func (self *DrainCoordinator) activeCount() int {
	count := 0
	for _, target := range self.targets {
		count += target.ActiveCount()
	}
	return count
}

// Drain runs the graceful shutdown sequence. It returns when the ingress is
// idle or the grace deadline passes; the caller then cancels the root ctx
// and exits.
func (self *DrainCoordinator) Drain(ctx context.Context) {
	self.readiness.SetDraining()

	if !self.settings.EnableDrain {
		glog.Infof("[proxy]drain disabled; shutting down immediately\n")
		self.runBeforeExits(ctx)
		self.drained.Store(true)
		return
	}

	startTime := time.Now()
	for _, target := range self.targets {
		target.Drain()
	}
	activeCount := self.activeCount()
	drainActiveRemainingGauge.Set(float64(activeCount))
	glog.Infof("[proxy]drain start (%d active, grace %s)\n", activeCount, self.settings.DrainGraceTimeout)

	graceCtx, graceCancel := context.WithTimeout(ctx, self.settings.DrainGraceTimeout)
	defer graceCancel()

	idleCtx, idleCancel := context.WithCancel(graceCtx)
	go func() {
		defer idleCancel()
		for _, target := range self.targets {
			if !target.WaitIdle(graceCtx) {
				return
			}
		}
	}()

	// progress: keep the gauge and log current while waiting, so a deploy's
	// drain is observable without ssh-ing to the host
	func() {
		for i := 1; ; i += 1 {
			select {
			case <-idleCtx.Done():
				return
			case <-time.After(1 * time.Second):
				drainActiveRemainingGauge.Set(float64(self.activeCount()))
				if i%10 == 0 {
					glog.Infof("[proxy]drain in progress (%d active)\n", self.activeCount())
				}
			}
		}
	}()

	remainingCount := self.activeCount()
	drainActiveRemainingGauge.Set(float64(remainingCount))
	if remainingCount == 0 {
		glog.Infof("[proxy]drain complete in %s\n", time.Since(startTime).Round(time.Second))
	} else {
		glog.Infof("[proxy]drain deadline with %d active in %s\n", remainingCount, time.Since(startTime).Round(time.Second))
	}

	self.runBeforeExits(ctx)
	self.drained.Store(true)
}

func (self *DrainCoordinator) runBeforeExits(ctx context.Context) {
	if len(self.beforeExits) == 0 {
		return
	}
	beforeExitCtx, beforeExitCancel := context.WithTimeout(ctx, self.settings.DrainBeforeExitTimeout)
	defer beforeExitCancel()
	for _, beforeExit := range self.beforeExits {
		beforeExit(beforeExitCtx)
	}
}
