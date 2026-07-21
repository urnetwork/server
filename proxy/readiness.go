package proxy

import (
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/router"
)

// readyGauge is 1 while the instance is ready to take the traffic flip
// (initial peer sync applied, not draining), 0 otherwise (PROXYDRAIN1.md §3.1)
var readyGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "ready",
		Help:      "1 when the proxy instance is ready (initial peer sync applied, not draining), 0 otherwise",
	},
)

func init() {
	prometheus.MustRegister(readyGauge)
}

// Readiness gates the /status health signal (PROXYDRAIN1.md §3.1).
//
// warpctl health-polls GET /status on the new container and flips the DNAT
// rules only once it returns 200 (`warp/warpctl/run.go` pollContainerStatus).
// Today's constant-ok status lets the flip race the initial wg peer sync: a
// client whose handshake lands before its peer is installed is silently
// dropped. Readiness makes /status truthful: 503 until the initial
// proxy-client sync has been applied, 503 again once a drain begins.
type Readiness struct {
	initialSyncDone atomic.Bool
	draining        atomic.Bool
}

func NewReadiness() *Readiness {
	readyGauge.Set(0)
	return &Readiness{}
}

// SetInitialSyncDone marks the initial proxy-client sync applied: every
// watched (host, block) stream has completed its first successful read and
// delivery (wg peers restored).
func (self *Readiness) SetInitialSyncDone() {
	self.initialSyncDone.Store(true)
	self.updateGauge()
}

// SetDraining marks the instance draining (PROXYDRAIN1.md §3.2). Nothing
// routes on this today — the DNAT flip already happened when the drain
// begins — but the truthful signal is what deploy dashboards and operators
// see.
func (self *Readiness) SetDraining() {
	self.draining.Store(true)
	self.updateGauge()
}

func (self *Readiness) Draining() bool {
	return self.draining.Load()
}

func (self *Readiness) updateGauge() {
	if ready, _ := self.Status(); ready {
		readyGauge.Set(1)
	} else {
		readyGauge.Set(0)
	}
}

// Status reports readiness and, when not ready, the reason.
func (self *Readiness) Status() (bool, string) {
	if self.draining.Load() {
		return false, "draining"
	}
	if !self.initialSyncDone.Load() {
		return false, "initial proxy client sync in progress"
	}
	return true, ""
}

// StatusHandler serves GET /status: the standard warp status once ready, 503
// with a reason while not. warpctl's new-container health poll gates the
// traffic flip on the 200.
func (self *Readiness) StatusHandler(w http.ResponseWriter, r *http.Request) {
	if ready, reason := self.Status(); !ready {
		http.Error(w, fmt.Sprintf("not ready: %s", reason), http.StatusServiceUnavailable)
		return
	}
	router.WarpStatus(w, r)
}
