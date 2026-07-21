package server

// http server drain support (APIDRAIN1.md §2.2-2.3, §2.5).
//
// On ctx cancel the serve loop runs a drain sequence instead of an
// immediate `Shutdown`:
//
//  1. grace (opt-in via `HttpServerOptions.KeepaliveDrainTimeout`): keep
//     serving, stamping `Connection: close` on every http/1 response. The
//     lb retires each pooled keepalive connection cleanly after its next
//     response instead of racing the snap-close of an idle pool at
//     `Shutdown`. (`http.Server.SetKeepAlivesEnabled(false)` is NOT usable
//     here: it closes idle connections immediately, which is exactly the
//     race the grace exists to avoid.)
//  2. `Shutdown` with `ShutdownTimeout`: closes the listener and idle
//     connections, waits for in-flight requests.
//  3. outcome: clean (nil) or `*HttpDrainCutError` with the number of
//     connections that outlived the deadline and are hard-cut at exit.
//
// The connection tracker follows http/1 `ConnState` transitions; an h2
// connection pins StateActive for its whole life, so counts are exact for
// the cleartext http/1 servers this repo runs behind nginx.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
)

// the {env, service, block, host} identity comes from the stats pusher
// grouping labels (see grafana.go), so these are service-neutral names
var (
	httpServerDrainingGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "http_server",
			Name:      "draining",
			Help:      "1 while the http server is in its drain sequence (SIGTERM to exit), 0 otherwise",
		},
	)
	httpServerDrainSecondsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "http_server",
			Name:      "drain_seconds",
			Help:      "duration of the last drain sequence (grace + shutdown)",
		},
	)
	httpServerDrainInflightGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "http_server",
			Name:      "drain_inflight",
			Help:      "requests mid-handler when the drain began",
		},
	)
	httpServerDrainCutGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "http_server",
			Name:      "drain_cut_connections",
			Help:      "connections still open when the shutdown deadline fired (hard-cut at exit); alert on nonzero",
		},
	)
)

func init() {
	prometheus.MustRegister(
		httpServerDrainingGauge,
		httpServerDrainSecondsGauge,
		httpServerDrainInflightGauge,
		httpServerDrainCutGauge,
	)
}

// HttpDrainCutError reports a drain that hit `ShutdownTimeout` with
// connections still open: those connections are hard-cut when the process
// exits. A deliberate drain (deploy SIGTERM) treats this as a logged
// outcome, not a crash.
type HttpDrainCutError struct {
	CutConnectionCount int64
}

func (self *HttpDrainCutError) Error() string {
	return fmt.Sprintf(
		"drain deadline with %d connection(s) still open (hard cut at exit)",
		self.CutConnectionCount,
	)
}

// httpConnTracker follows `http.Server.ConnState` transitions to expose how
// many connections are open and how many are mid-request.
type httpConnTracker struct {
	stateLock  sync.Mutex
	connStates map[net.Conn]http.ConnState
	openCount  int64
	// connections currently mid-request (StateActive)
	activeCount int64
}

func newHttpConnTracker() *httpConnTracker {
	return &httpConnTracker{
		connStates: map[net.Conn]http.ConnState{},
	}
}

// connState is the `http.Server.ConnState` hook
func (self *httpConnTracker) connState(conn net.Conn, state http.ConnState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	prevState, seen := self.connStates[conn]
	switch state {
	case http.StateNew:
		self.openCount += 1
		self.connStates[conn] = state
	case http.StateActive:
		if !seen {
			self.openCount += 1
		}
		if prevState != http.StateActive {
			self.activeCount += 1
		}
		self.connStates[conn] = state
	case http.StateIdle:
		if prevState == http.StateActive {
			self.activeCount -= 1
		}
		self.connStates[conn] = state
	case http.StateHijacked, http.StateClosed:
		if prevState == http.StateActive {
			self.activeCount -= 1
		}
		if seen {
			self.openCount -= 1
			delete(self.connStates, conn)
		}
	}
}

func (self *httpConnTracker) counts() (openCount int64, activeCount int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.openCount, self.activeCount
}

// drainHandler stamps `Connection: close` on every http/1 response while
// draining, so the peer retires its pooled keepalive connection after this
// response instead of holding it into the `Shutdown` close.
type drainHandler struct {
	handler  http.Handler
	draining atomic.Bool
}

func newDrainHandler(handler http.Handler) *drainHandler {
	return &drainHandler{
		handler: handler,
	}
}

// `http.Handler`
func (self *drainHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if self.draining.Load() && r.ProtoMajor == 1 {
		w.Header().Set("Connection", "close")
	}
	self.handler.ServeHTTP(w, r)
}

// httpServeWithDrain runs `serve` (which blocks in `Serve`/`ServeTLS`) and,
// on ctx cancel, the drain sequence described in the file comment. The
// caller's server must be constructed with the drainHandler as its Handler
// and the tracker's connState hook.
func httpServeWithDrain(
	cancelCtx context.Context,
	cancel context.CancelFunc,
	server *http.Server,
	handler *drainHandler,
	tracker *httpConnTracker,
	serve func() error,
	httpServerOptions HttpServerOptions,
) error {
	errs := make(chan error)

	go func() {
		defer cancel()
		err := serve()
		select {
		case errs <- err:
		case <-cancelCtx.Done():
		}
	}()

	select {
	case <-cancelCtx.Done():
	case err := <-errs:
		return err
	}

	drainStartTime := time.Now()
	_, inflightCount := tracker.counts()
	httpServerDrainingGauge.Set(1)
	httpServerDrainInflightGauge.Set(float64(inflightCount))

	if 0 < httpServerOptions.KeepaliveDrainTimeout {
		handler.draining.Store(true)
		glog.Infof(
			"[http]draining: %s keepalive retire grace (%d in flight)\n",
			httpServerOptions.KeepaliveDrainTimeout,
			inflightCount,
		)
		select {
		case <-time.After(httpServerOptions.KeepaliveDrainTimeout):
		case <-errs:
			// the server exited during the grace; proceed to shutdown
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpServerOptions.ShutdownTimeout)
	defer shutdownCancel()
	shutdownErr := server.Shutdown(shutdownCtx)

	drainDuration := time.Now().Sub(drainStartTime)
	httpServerDrainSecondsGauge.Set(drainDuration.Seconds())

	if shutdownErr == nil {
		httpServerDrainCutGauge.Set(0)
		glog.Infof("[http]drain complete in %s (clean)\n", drainDuration)
		return nil
	}

	// the deadline fired; whatever is still open is hard-cut at exit
	cutCount, _ := tracker.counts()
	httpServerDrainCutGauge.Set(float64(cutCount))
	glog.Infof(
		"[http]drain deadline after %s: %d connection(s) cut\n",
		drainDuration,
		cutCount,
	)
	return &HttpDrainCutError{
		CutConnectionCount: cutCount,
	}
}
