package connect

// A loopback-only pprof listener for the connect process (LBALTOPT1.md R1).
//
// On the edges connect runs with host networking, so 127.0.0.1 is the host's
// loopback: an operator on the host reaches the listener with curl, and
// nothing off the host does. Five connect blocks share a host, so each binds
// a port the kernel picks, 127.0.0.1:0, and publishes it as
// urnetwork_connect_pprof_port, which the stats push keys by host and block.
// The address is fixed, not a setting, so nothing can make the listener bind
// a wildcard or public address.
//
// Served, on GET only, and nothing else (404):
//   - /debug/pprof/profile: a CPU profile of the process for `seconds`, 30 by
//     default, cut to 60;
//   - /debug/pprof/heap, allocs, mutex and block: a snapshot, protobuf, or
//     text with `debug=1`.
//
// Not served: goroutine, whose dump of a connect process's millions of
// goroutines stops the world for as long as it takes; trace; cmdline;
// symbol; threadcreate; and the index. Nor are delta snapshots (`seconds` on
// a snapshot): take two and compare them with `go tool pprof -base`. The
// mutex and block sampling rates stay at Go's defaults, off, so those two
// profiles are empty unless something turns sampling on. net/http/pprof is
// not imported, since its init registers every profile, goroutine and
// cmdline included, on http.DefaultServeMux.
//
// CONNECT_PPROF=0 (services.yml env_vars) turns the listener off. Only the
// connect command starts it (RunOptions.LoopbackPprof): the operator's
// one-process command and the tests run connect without it.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The environment variable whose value turns the listener off.
const pprofListenerEnvName = "CONNECT_PPROF"

// The loopback, on a port the kernel picks.
const pprofListenAddress = "127.0.0.1:0"

// How the listener serves. Read-only once the listener starts.
type pprofListenerSettings struct {
	// a CPU profile's length when the request names no seconds: 30 s, as
	// net/http/pprof. Non-positive is refused at start.
	DefaultCpuProfileDuration time.Duration
	// the longest CPU profile, to which a longer request is cut: 60 s, twice
	// the default, which bounds how long one request holds the process's
	// profiler. Non-positive is refused at start.
	MaxCpuProfileDuration time.Duration
	// the requests served at once; one more is refused with 503. 2, so a
	// snapshot can be taken during a CPU profile, of which the process runs
	// one at a time anyway. Non-positive is refused at start.
	MaxConcurrentRequestCount int
	// the bound on reading a request's header: 10 s. Non-positive is refused
	// at start.
	ReadHeaderTimeout time.Duration
	// the time to build and write a response beyond MaxCpuProfileDuration;
	// their sum is the HTTP write timeout. 30 s. Non-positive is refused at
	// start.
	WriteTimeoutMargin time.Duration
	// how long an idle connection stays open: 1 m. Non-positive is refused at
	// start.
	IdleTimeout time.Duration

	// Nil outside package tests. Called with the request's context and the CPU
	// profile's length before the profile's wait, which waits the length it
	// returns.
	cpuProfileWaitForTest func(ctx context.Context, duration time.Duration) time.Duration
}

// The listener's defaults, a new instance on every call.
func defaultPprofListenerSettings() *pprofListenerSettings {
	return &pprofListenerSettings{
		DefaultCpuProfileDuration: 30 * time.Second,
		MaxCpuProfileDuration:     60 * time.Second,
		MaxConcurrentRequestCount: 2,
		ReadHeaderTimeout:         10 * time.Second,
		WriteTimeoutMargin:        30 * time.Second,
		IdleTimeout:               1 * time.Minute,
	}
}

// Refuses settings that leave a wait unbounded or serve nothing.
func (self *pprofListenerSettings) validate() error {
	if self.DefaultCpuProfileDuration <= 0 || self.MaxCpuProfileDuration <= 0 {
		return errors.New("pprof listener CPU profile durations must be positive")
	}
	if self.MaxConcurrentRequestCount <= 0 {
		return errors.New("pprof listener concurrent request count must be positive")
	}
	if self.ReadHeaderTimeout <= 0 || self.WriteTimeoutMargin <= 0 || self.IdleTimeout <= 0 {
		return errors.New("pprof listener timeouts must be positive")
	}
	return nil
}

// The length of a CPU profile asked for seconds, cut to MaxCpuProfileDuration.
func (self *pprofListenerSettings) cpuProfileDuration(seconds int64) time.Duration {
	// compared in whole seconds first, so a large request can't overflow
	if int64(self.MaxCpuProfileDuration/time.Second) < seconds {
		return self.MaxCpuProfileDuration
	}
	return min(time.Duration(seconds)*time.Second, self.MaxCpuProfileDuration)
}

// Unset or a true value (1, t, true) serves. Any other value, 0 and false
// among them, turns the listener off, so a mistyped "off" does too.
func pprofListenerEnabled(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}

// The runtime/pprof profile a snapshot path serves.
func pprofSnapshotName(path string) (string, bool) {
	switch path {
	case "/debug/pprof/heap":
		return "heap", true
	case "/debug/pprof/allocs":
		return "allocs", true
	case "/debug/pprof/mutex":
		return "mutex", true
	case "/debug/pprof/block":
		return "block", true
	default:
		return "", false
	}
}

// The value of the one parameter a path accepts. Not ok when the query names
// another parameter or repeats this one.
func pprofQueryValue(query url.Values, name string) (value string, present bool, ok bool) {
	for key, values := range query {
		if key != name || len(values) != 1 {
			return "", false, false
		}
		value, present = values[0], true
	}
	return value, present, true
}

// One process's loopback pprof listener. Its methods are safe for concurrent
// use.
type pprofListener struct {
	settings   *pprofListenerSettings
	address    *net.TCPAddr
	registerer prometheus.Registerer
	portGauge  prometheus.Gauge
	httpServer *http.Server
	// cancels the requests' parent context
	cancel context.CancelFunc
	// a place for each request being served
	admission chan struct{}
	// closed when the HTTP server stops serving
	served    chan struct{}
	closeOnce sync.Once
}

// The listener, or nil when CONNECT_PPROF turns it off.
func newPprofListenerFromEnv(settings *pprofListenerSettings, registerer prometheus.Registerer) (*pprofListener, error) {
	if !pprofListenerEnabled(os.Getenv(pprofListenerEnvName)) {
		glog.Infof("[connect]loopback pprof is off (%s)\n", pprofListenerEnvName)
		return nil, nil
	}
	return newPprofListener(settings, registerer)
}

// Binds the loopback on a port the kernel picks, publishes the port in
// registerer, and serves until Close.
func newPprofListener(settings *pprofListenerSettings, registerer prometheus.Registerer) (*pprofListener, error) {
	if err := settings.validate(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", pprofListenAddress)
	if err != nil {
		return nil, err
	}
	// a guard on the fixed address above
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() {
		listener.Close()
		return nil, fmt.Errorf("the pprof listener bound %s, not the loopback", listener.Addr())
	}
	portGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "pprof_port",
		Help:      "Loopback TCP port of the process's pprof listener, on its host",
	})
	if err := registerer.Register(portGauge); err != nil {
		listener.Close()
		return nil, fmt.Errorf("publish the pprof port: %w", err)
	}
	portGauge.Set(float64(address.Port))

	ctx, cancel := context.WithCancel(context.Background())
	self := &pprofListener{
		settings:   settings,
		address:    address,
		registerer: registerer,
		portGauge:  portGauge,
		cancel:     cancel,
		admission:  make(chan struct{}, settings.MaxConcurrentRequestCount),
		served:     make(chan struct{}),
	}
	self.httpServer = &http.Server{
		Handler:           http.HandlerFunc(self.serveHttp),
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		WriteTimeout:      settings.MaxCpuProfileDuration + settings.WriteTimeoutMargin,
		IdleTimeout:       settings.IdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go server.HandleError(func() {
		self.run(listener)
	})
	glog.Infof("[connect]loopback pprof on port %d\n", address.Port)
	return self, nil
}

// Serves until Close.
func (self *pprofListener) run(listener net.Listener) {
	defer close(self.served)
	err := self.httpServer.Serve(listener)
	if !errors.Is(err, http.ErrServerClosed) {
		glog.Errorf("[connect]loopback pprof stopped serving (%s)\n", err)
	}
}

// Routes by exact path: another path is 404 whatever its method.
func (self *pprofListener) serveHttp(w http.ResponseWriter, r *http.Request) {
	snapshotName, snapshot := pprofSnapshotName(r.URL.Path)
	cpuProfile := r.URL.Path == "/debug/pprof/profile"
	if !snapshot && !cpuProfile {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "only GET is served", http.StatusMethodNotAllowed)
		return
	}
	if cpuProfile {
		self.serveCpuProfile(w, r)
	} else {
		self.serveSnapshot(w, r, snapshotName)
	}
}

// A CPU profile of the whole process for its seconds, or the default, cut to
// MaxCpuProfileDuration. It ends early when the client goes away or the
// listener closes. The process runs one CPU profile at a time, so a second
// is refused with 409.
func (self *pprofListener) serveCpuProfile(w http.ResponseWriter, r *http.Request) {
	value, present, ok := pprofQueryValue(r.URL.Query(), "seconds")
	if !ok {
		http.Error(w, "only one seconds parameter is accepted", http.StatusBadRequest)
		return
	}
	duration := min(self.settings.DefaultCpuProfileDuration, self.settings.MaxCpuProfileDuration)
	if present {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds <= 0 {
			http.Error(w, "seconds must be a positive whole number", http.StatusBadRequest)
			return
		}
		duration = self.settings.cpuProfileDuration(seconds)
	}
	if !self.admit() {
		http.Error(w, "busy: other profiles are being served", http.StatusServiceUnavailable)
		return
	}
	defer self.release()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="profile"`)
	// the profiler writes w from its own goroutine when it stops, and
	// StopCPUProfile waits for that
	if err := pprof.StartCPUProfile(w); err != nil {
		w.Header().Del("Content-Disposition")
		http.Error(w, "a CPU profile is already running in this process", http.StatusConflict)
		return
	}
	// deferred, so even a panic net/http recovers can't leave the process's
	// profiler running
	defer pprof.StopCPUProfile()
	if self.settings.cpuProfileWaitForTest != nil {
		duration = self.settings.cpuProfileWaitForTest(r.Context(), duration)
	}
	select {
	case <-r.Context().Done():
	case <-time.After(duration):
	}
}

// A snapshot of one runtime/pprof profile, as protobuf, or as text with
// debug=1.
func (self *pprofListener) serveSnapshot(w http.ResponseWriter, r *http.Request, name string) {
	value, present, ok := pprofQueryValue(r.URL.Query(), "debug")
	if !ok {
		http.Error(w, "only one debug parameter is accepted; for a delta, compare two snapshots with go tool pprof -base", http.StatusBadRequest)
		return
	}
	debug := 0
	if present {
		switch value {
		case "0":
		case "1":
			debug = 1
		default:
			http.Error(w, "debug must be 0 or 1", http.StatusBadRequest)
			return
		}
	}
	if !self.admit() {
		http.Error(w, "busy: other profiles are being served", http.StatusServiceUnavailable)
		return
	}
	defer self.release()

	if debug == 0 {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	// the predefined profiles always exist. A write error is the client
	// gone, with no one left to tell
	_ = pprof.Lookup(name).WriteTo(w, debug)
}

// Takes one of the places for a request being served, or reports none is
// free.
func (self *pprofListener) admit() bool {
	select {
	case self.admission <- struct{}{}:
		return true
	default:
		return false
	}
}

// Gives back a place admit took.
func (self *pprofListener) release() {
	<-self.admission
}

// Stops serving, ends a CPU profile in flight early, waits for the requests
// in flight, and withdraws the port. Safe on nil and more than once.
func (self *pprofListener) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		self.cancel()
		self.httpServer.Close()
		<-self.served
		// taking every place waits out the requests in flight and refuses one
		// that a closing connection starts late
		for range cap(self.admission) {
			self.admission <- struct{}{}
		}
		self.registerer.Unregister(self.portGauge)
	})
}
