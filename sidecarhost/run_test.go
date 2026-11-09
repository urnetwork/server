package sidecarhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// How long a step of a run test may take before it fails rather than hang.
const runStepTimeout = 10 * time.Second

// What happened, in order.
type runEventLog struct {
	stateLock sync.Mutex
	events    []string
}

func (self *runEventLog) add(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

func (self *runEventLog) all() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

// The position of event in the log, -1 when it never happened.
func (self *runEventLog) index(event string) int {
	return slices.Index(self.all(), event)
}

// An engine whose readiness the test sets, recording its lifecycle.
type fakeRunEngine struct {
	log     *runEventLog
	ready   atomic.Bool
	handler http.Handler
	served  chan net.Listener
}

func newFakeRunEngine(log *runEventLog) *fakeRunEngine {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /evs/{repo}/head", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("head"))
	})
	return &fakeRunEngine{log: log, handler: mux, served: make(chan net.Listener, 8)}
}

func (self *fakeRunEngine) Ready() bool { return self.ready.Load() }

func (self *fakeRunEngine) Drain() { self.log.add("drain") }

func (self *fakeRunEngine) Close() { self.log.add("close") }

func (self *fakeRunEngine) Handler() http.Handler { return self.handler }

// Serves until listener closes.
func (self *fakeRunEngine) Serve(listener net.Listener) error {
	self.served <- listener
	for {
		conn, err := listener.Accept()
		if err != nil {
			self.log.add(fmt.Sprintf("serve %s ended", listener.Addr()))
			return nil
		}
		conn.Close()
	}
}

// A listener that accepts nothing until it closes.
type fakeRunListener struct {
	log    *runEventLog
	port   int
	closed chan struct{}
	once   sync.Once
}

func (self *fakeRunListener) Accept() (net.Conn, error) {
	<-self.closed
	return nil, net.ErrClosed
}

func (self *fakeRunListener) Close() error {
	self.once.Do(func() {
		self.log.add(fmt.Sprintf("listener %d closed", self.port))
		close(self.closed)
	})
	return nil
}

func (self *fakeRunListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: self.port}
}

// A wrapped listener, as a PROXY header unwrapper would be.
type wrappedRunListener struct {
	net.Listener
}

// Run's world, faked: the status server holds the handler it was given, and each dependency records.
type runFixture struct {
	t        *testing.T
	log      *runEventLog
	clock    *sidecartest.FakeClock
	engine   *fakeRunEngine
	registry *prometheus.Registry
	status   *router.WarpStatusState
	deps     *runDependencies

	handler   atomic.Pointer[http.Handler]
	listening chan struct{}
	statsUp   chan struct{}
	latched   chan error

	sharedChecks     atomic.Int32
	statsPushChecked atomic.Int32
}

func newRunFixture(t *testing.T) *runFixture {
	t.Setenv("WARP_SERVICE", "test")
	t.Setenv("WARP_BLOCK", "test-block")
	log := &runEventLog{}
	fixture := &runFixture{
		t:         t,
		log:       log,
		clock:     sidecartest.NewFakeClock(sidecartest.DefaultClockStart()),
		engine:    newFakeRunEngine(log),
		registry:  prometheus.NewRegistry(),
		status:    &router.WarpStatusState{},
		listening: make(chan struct{}),
		statsUp:   make(chan struct{}, 1),
		latched:   make(chan error, 1),
	}
	var listeningOnce sync.Once
	fixture.deps = &runDependencies{
		listenAndServe: func(ctx context.Context, addr string, handler http.Handler, reusePort bool, options server.HttpServerOptions) error {
			fixture.handler.Store(&handler)
			listeningOnce.Do(func() { close(fixture.listening) })
			<-ctx.Done()
			log.add("status server drained")
			return nil
		},
		listen: func(ctx context.Context, port int) (net.Listener, error) {
			log.add(fmt.Sprintf("listener %d bound", port))
			return &fakeRunListener{log: log, port: port, closed: make(chan struct{})}, nil
		},
		startupReadiness: func(ctx context.Context) error {
			fixture.sharedChecks.Add(1)
			return nil
		},
		startStatsPusher: func(ctx context.Context) func() {
			log.add("stats started")
			fixture.statsUp <- struct{}{}
			return func() { log.add("stats flushed") }
		},
		statsPushUnavailable: func() error {
			fixture.statsPushChecked.Add(1)
			return errors.New("no push credential")
		},
		scopedVault: func() error { return nil },
		underWarp:   func() bool { return false },
		latched: func(err error) {
			fixture.latched <- err
		},
	}
	return fixture
}

// The options of a block that serves the fake engine over HTTP.
func (self *runFixture) httpOptions() *RunOptions {
	return &RunOptions{
		Service:    "test",
		StatusPort: 80,
		Engine:     self.engine,
		Http:       self.engine,
		Metrics:    newMetrics(self.registry),
		WarpStatus: self.status,
		Clock:      self.clock,
	}
}

// Runs the block on a goroutine, returning the channel its result comes back on.
func (self *runFixture) start(ctx context.Context, options *RunOptions) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, options, self.deps)
	}()
	select {
	case <-self.listening:
	case <-time.After(runStepTimeout):
		self.t.Fatal("the status server never started")
	}
	return done
}

// What the block answers a request with.
func (self *runFixture) request(method string, path string) *httptest.ResponseRecorder {
	handler := self.handler.Load()
	if handler == nil {
		self.t.Fatal("no handler")
	}
	recorder := httptest.NewRecorder()
	(*handler).ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// What /status says.
func (self *runFixture) statusText() string {
	recorder := self.request("GET", "/status")
	if recorder.Code != http.StatusOK {
		self.t.Fatalf("/status answered %d", recorder.Code)
	}
	result := &router.WarpStatusResult{}
	if err := json.Unmarshal(recorder.Body.Bytes(), result); err != nil {
		self.t.Fatal(err)
	}
	return result.Status
}

// The readiness latch's outcome.
func (self *runFixture) awaitLatch() error {
	select {
	case err := <-self.latched:
		return err
	case <-time.After(runStepTimeout):
		self.t.Fatal("readiness never latched")
		return nil
	}
}

func (self *runFixture) readyGauge() float64 {
	families, err := self.registry.Gather()
	if err != nil {
		self.t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_test_ready" {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	self.t.Fatal("no ready gauge")
	return 0
}

func awaitRunEnd(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(runStepTimeout):
		t.Fatal("Run did not return")
		return nil
	}
}

// /status answers from the first moment, not ready while the engine is not, and the engine's routes wait;
// once the engine is ready the block latches ready, its routes open and the stats start.
func TestRunServesStatusAtOnceAndLatchesOnceTheEngineIsReady(t *testing.T) {
	fixture := newRunFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := fixture.start(ctx, fixture.httpOptions())

	if status := fixture.statusText(); !strings.HasPrefix(status, "error not ready") {
		t.Fatalf("status %q before readiness", status)
	}
	if recorder := fixture.request("GET", "/evs/r1/head"); recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("a route before readiness answered %d", recorder.Code)
	}
	// readiness waits on its timeout and its next poll
	if err := fixture.clock.AwaitTimers(ctx, 2); err != nil {
		t.Fatal(err)
	}
	fixture.engine.ready.Store(true)
	fixture.clock.Advance(DefaultRunSettings().ReadyPollInterval)
	if err := fixture.awaitLatch(); err != nil {
		t.Fatal(err)
	}
	<-fixture.statsUp
	if status := fixture.statusText(); status != "ok" {
		t.Fatalf("status %q once ready", status)
	}
	if gauge := fixture.readyGauge(); gauge != 1 {
		t.Fatalf("ready gauge %v", gauge)
	}
	if recorder := fixture.request("GET", "/evs/r1/head"); recorder.Code != http.StatusOK || recorder.Body.String() != "head" {
		t.Fatalf("a route once ready answered %d %q", recorder.Code, recorder.Body.String())
	}
	if fixture.sharedChecks.Load() != 0 {
		t.Fatal("a block with no Postgres ran the shared check")
	}
	cancel()
	if err := awaitRunEnd(t, done); err != nil {
		t.Fatal(err)
	}
	if status := fixture.statusText(); status != "draining" {
		t.Fatalf("status %q after the drain", status)
	}
}

// An engine that is never ready latches not ready at the timeout, and Run keeps serving /status rather
// than exit, so warpctl's poll fails and reverts; the error survives the drain.
func TestRunNeverExitsWhenNotReady(t *testing.T) {
	fixture := newRunFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := fixture.start(ctx, fixture.httpOptions())

	if err := fixture.clock.AwaitTimers(ctx, 2); err != nil {
		t.Fatal(err)
	}
	fixture.clock.Advance(DefaultRunSettings().ReadyTimeout)
	if err := fixture.awaitLatch(); err == nil {
		t.Fatal("an engine never ready latched ready")
	}
	status := fixture.statusText()
	if !strings.HasPrefix(status, "error not ready") || !strings.Contains(status, "not ready within") {
		t.Fatalf("status %q", status)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned while not ready: %v", err)
	default:
	}
	// still answering, later on
	fixture.clock.Advance(time.Hour)
	if status := fixture.statusText(); !strings.HasPrefix(status, "error not ready") {
		t.Fatalf("status %q later", status)
	}
	if recorder := fixture.request("GET", "/evs/r1/head"); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("a not-ready block's route answered %d", recorder.Code)
	}
	if fixture.statsPushChecked.Load() != 0 || 0 < len(fixture.statsUp) {
		t.Fatal("a not-ready block started its stats")
	}
	cancel()
	if err := awaitRunEnd(t, done); err != nil {
		t.Fatal(err)
	}
	if status := fixture.statusText(); !strings.HasPrefix(status, "error not ready") {
		t.Fatalf("the drain hid the failure: %q", status)
	}
	if fixture.log.index("drain") < 0 || fixture.log.index("close") < 0 {
		t.Fatalf("a not-ready engine was not drained and closed: %v", fixture.log.all())
	}
}

// SIGTERM's order: draining at /status, the listeners closed with the status server's drain, then
// Engine.Drain, then the stats flushed, then Engine.Close, and every Serve has returned.
func TestRunDrainsInOrder(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.engine.ready.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := fixture.httpOptions()
	options.Stream = fixture.engine
	options.Listeners = []*RunListener{
		{Port: 8443},
		{Port: 25, Wrap: func(listener net.Listener) (net.Listener, error) {
			return &wrappedRunListener{Listener: listener}, nil
		}},
	}
	done := fixture.start(ctx, options)
	if err := fixture.awaitLatch(); err != nil {
		t.Fatal(err)
	}
	<-fixture.statsUp
	servedWrapped := false
	for range 2 {
		select {
		case listener := <-fixture.engine.served:
			if _, ok := listener.(*wrappedRunListener); ok {
				servedWrapped = true
			}
		case <-time.After(runStepTimeout):
			t.Fatal("a listener was never served")
		}
	}
	if !servedWrapped {
		t.Fatal("the wrapped port was served unwrapped")
	}
	cancel()
	if err := awaitRunEnd(t, done); err != nil {
		t.Fatal(err)
	}
	log := fixture.log
	drain := log.index("drain")
	for _, before := range []string{"status server drained", "listener 8443 closed", "listener 25 closed"} {
		if index := log.index(before); index < 0 || drain < index {
			t.Fatalf("%q after the engine's drain: %v", before, log.all())
		}
	}
	if !(drain < log.index("stats flushed") && log.index("stats flushed") < log.index("close")) {
		t.Fatalf("order %v", log.all())
	}
	for _, served := range []string{"serve 192.0.2.1:8443 ended", "serve 192.0.2.1:25 ended"} {
		if log.index(served) < 0 {
			t.Fatalf("%q never happened: %v", served, log.all())
		}
	}
}

// With Postgres the shared check runs and each group's marker is asked first: a block that reads a
// group this binary does not link stays not ready, and so does one whose shared check fails.
func TestRunWithPostgresNeedsTheSharedCheckAndItsGroups(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.engine.ready.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := fixture.httpOptions()
	options.Postgres = true
	options.Groups = []*RunGroup{
		{Name: "MG1", Linked: func() bool { return true }},
		{Name: "MG2", Linked: func() bool { return false }},
	}
	done := fixture.start(ctx, options)
	err := fixture.awaitLatch()
	if err == nil || !strings.Contains(err.Error(), "MG2") {
		t.Fatalf("a block reading an unlinked group latched %v", err)
	}
	if status := fixture.statusText(); !strings.Contains(status, "MG2") {
		t.Fatalf("status %q", status)
	}
	cancel()
	awaitRunEnd(t, done)

	fixture = newRunFixture(t)
	fixture.engine.ready.Store(true)
	fixture.deps.startupReadiness = func(ctx context.Context) error {
		return errors.New("pg: database migration head 1 is below binary-required head 2")
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	options = fixture.httpOptions()
	options.Postgres = true
	done = fixture.start(ctx, options)
	if err := fixture.awaitLatch(); err == nil || !strings.Contains(err.Error(), "migration head") {
		t.Fatalf("a failed shared check latched %v", err)
	}
	cancel()
	awaitRunEnd(t, done)

	fixture = newRunFixture(t)
	fixture.engine.ready.Store(true)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	options = fixture.httpOptions()
	options.Postgres = true
	options.Groups = []*RunGroup{{Name: "MG1", Linked: func() bool { return true }}}
	done = fixture.start(ctx, options)
	if err := fixture.awaitLatch(); err != nil {
		t.Fatal(err)
	}
	if fixture.sharedChecks.Load() != 1 {
		t.Fatalf("the shared check ran %d times", fixture.sharedChecks.Load())
	}
	cancel()
	awaitRunEnd(t, done)
}

// A block with no Postgres starts with an empty vault and config, no pg.yml among them, on the
// production dependencies but its listeners: it never runs the shared check, which reads
// migration_audit (plan-OPS-small-readings #9).
func TestRunWithoutPostgresNeedsNoPgYml(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(home, "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(home, "config"))
	if _, err := server.Vault.SimpleResource("pg.yml"); err == nil {
		t.Fatal("the test's vault holds a pg.yml")
	}
	fixture := newRunFixture(t)
	fixture.engine.ready.Store(true)
	fixture.deps.startupReadiness = func(ctx context.Context) error {
		t.Error("a block with no Postgres ran the shared check")
		return errors.New("refused")
	}
	fixture.deps.startStatsPusher = func(ctx context.Context) func() {
		flush := server.StartStatsPusher(ctx)
		fixture.statsUp <- struct{}{}
		return flush
	}
	fixture.deps.statsPushUnavailable = server.StatsPushUnavailable
	fixture.deps.scopedVault = func() error {
		return checkScopedVault(os.LookupEnv, server.VaultHomeRoot())
	}
	fixture.deps.underWarp = func() bool {
		return runUnderWarp(os.LookupEnv)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := fixture.start(ctx, fixture.httpOptions())
	if err := fixture.awaitLatch(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := awaitRunEnd(t, done); err != nil {
		t.Fatal(err)
	}
}

// A listener that will not bind or wrap leaves the block not ready, with nothing it bound left open.
func TestRunListenerFailureIsNotReady(t *testing.T) {
	for _, failWrap := range []bool{false, true} {
		fixture := newRunFixture(t)
		fixture.engine.ready.Store(true)
		listen := fixture.deps.listen
		fixture.deps.listen = func(ctx context.Context, port int) (net.Listener, error) {
			if port == 25 && !failWrap {
				return nil, errors.New("address in use")
			}
			return listen(ctx, port)
		}
		ctx, cancel := context.WithCancel(t.Context())
		options := fixture.httpOptions()
		options.Stream = fixture.engine
		options.Listeners = []*RunListener{
			{Port: 8443},
			{Port: 25, Wrap: func(listener net.Listener) (net.Listener, error) {
				if failWrap {
					return nil, errors.New("no proxy protocol")
				}
				return listener, nil
			}},
		}
		done := fixture.start(ctx, options)
		if err := fixture.awaitLatch(); err == nil || !strings.Contains(err.Error(), "port 25") {
			t.Fatalf("wrap %t: latched %v", failWrap, err)
		}
		if fixture.log.index("listener 8443 closed") < 0 {
			t.Fatalf("wrap %t: a bound listener stayed open: %v", failWrap, fixture.log.all())
		}
		if failWrap && fixture.log.index("listener 25 closed") < 0 {
			t.Fatalf("the unwrapped listener stayed open: %v", fixture.log.all())
		}
		cancel()
		awaitRunEnd(t, done)
	}
}

// Under warp a ready block whose stats are off says so; outside warp it does not ask.
func TestRunChecksStatsUnderWarp(t *testing.T) {
	for _, underWarp := range []bool{false, true} {
		fixture := newRunFixture(t)
		fixture.engine.ready.Store(true)
		fixture.deps.underWarp = func() bool { return underWarp }
		ctx, cancel := context.WithCancel(t.Context())
		done := fixture.start(ctx, fixture.httpOptions())
		if err := fixture.awaitLatch(); err != nil {
			t.Fatal(err)
		}
		<-fixture.statsUp
		cancel()
		awaitRunEnd(t, done)
		if checked := 0 < fixture.statsPushChecked.Load(); checked != underWarp {
			t.Fatalf("under warp %t: stats checked %t", underWarp, checked)
		}
	}
}

// The {vault: no} rule: under warp a block must not mount the whole vault, and reads its secret_files
// only with WARP_VAULT_HOME at their mount; outside warp, and at local, the rule does not apply.
func TestRunVaultRule(t *testing.T) {
	env := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		}
	}
	block := map[string]string{"WARP_ENV": "main", "WARP_SERVICE": "evs", "WARP_BLOCK": "g1-1"}
	if err := checkScopedVault(env(block), ScopedVaultHome); err != nil {
		t.Fatalf("a scoped block: %v", err)
	}
	if err := checkScopedVault(env(block), ScopedVaultHome+"/"); err != nil {
		t.Fatalf("a scoped block, its root with a slash: %v", err)
	}
	if err := checkScopedVault(env(block), "/srv/warp/vault"); err == nil {
		t.Fatal("a block reading the default vault root passed")
	}
	withVault := map[string]string{"WARP_VAULT": "/srv/warp/vault"}
	for key, value := range block {
		withVault[key] = value
	}
	if err := checkScopedVault(env(withVault), ScopedVaultHome); err == nil {
		t.Fatal("a block with the whole vault mounted passed")
	}
	for _, outside := range []map[string]string{
		{},
		{"WARP_ENV": "local", "WARP_SERVICE": "evs", "WARP_BLOCK": "g1-1"},
		{"WARP_ENV": "main", "WARP_SERVICE": "evs"},
	} {
		if err := checkScopedVault(env(outside), "/anywhere"); err != nil {
			t.Fatalf("outside warp %v: %v", outside, err)
		}
	}
}

// Options no block could run are refused before anything is bound.
func TestRunRefusesBadOptions(t *testing.T) {
	engine := newFakeRunEngine(&runEventLog{})
	good := func() *RunOptions {
		return &RunOptions{Service: "evs", StatusPort: 80, Engine: engine, Http: engine}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(options *RunOptions){
		"a service with a dash":         func(options *RunOptions) { options.Service = "mxedge-platform" },
		"no service":                    func(options *RunOptions) { options.Service = "" },
		"no status port":                func(options *RunOptions) { options.StatusPort = 0 },
		"no engine":                     func(options *RunOptions) { options.Engine = nil },
		"listeners with no stream":      func(options *RunOptions) { options.Listeners = []*RunListener{{Port: 25}} },
		"a stream with no listener":     func(options *RunOptions) { options.Stream = engine },
		"a listener on the status port": func(options *RunOptions) { options.Stream = engine; options.Listeners = []*RunListener{{Port: 80}} },
		"one port twice": func(options *RunOptions) {
			options.Stream = engine
			options.Listeners = []*RunListener{{Port: 25}, {Port: 25}}
		},
		"a group with no Postgres": func(options *RunOptions) {
			options.Groups = []*RunGroup{{Name: "MG1", Linked: server.SidecarMg1Linked}}
		},
		"a group with no marker":    func(options *RunOptions) { options.Postgres = true; options.Groups = []*RunGroup{{Name: "MG1"}} },
		"a listener with no port":   func(options *RunOptions) { options.Stream = engine; options.Listeners = []*RunListener{{}} },
		"a port past 65535":         func(options *RunOptions) { options.StatusPort = 65_536 },
		"a nil listener":            func(options *RunOptions) { options.Stream = engine; options.Listeners = []*RunListener{nil} },
		"a group that is no group":  func(options *RunOptions) { options.Postgres = true; options.Groups = []*RunGroup{nil} },
		"an upper case service":     func(options *RunOptions) { options.Service = "Evs" },
		"a service with underscore": func(options *RunOptions) { options.Service = "evs_x" },
	}
	for name, change := range cases {
		options := good()
		change(options)
		if err := options.Validate(); err == nil {
			t.Errorf("%s was taken", name)
		}
		if err := run(t.Context(), options, &runDependencies{}); err == nil {
			t.Errorf("%s ran", name)
		}
	}
}

// A block's main scrubs its logs first, and the check tells one that does from one that does not.
func TestCheckMainScrubsLogsFirst(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, source string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good.go", `package main

import "github.com/urnetwork/server"

func main() {
	logs := server.ScrubCommandLogs()
	defer logs.Flush()
}
`)
	renamed := write("renamed.go", `package main

import urserver "github.com/urnetwork/server"

func main() {
	urserver.ScrubCommandLogs()
}
`)
	late := write("late.go", `package main

import (
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

func main() {
	glog.Infof("before the scrubber\n")
	logs := server.ScrubCommandLogs()
	defer logs.Flush()
}
`)
	missing := write("missing.go", `package main

func run() {}
`)
	for _, path := range []string{good, renamed} {
		if err := CheckMainScrubsLogsFirst(path); err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{late, missing, filepath.Join(dir, "absent.go")} {
		if err := CheckMainScrubsLogsFirst(path); err == nil {
			t.Errorf("%s passed", filepath.Base(path))
		}
	}
}

// The status latch Run makes when given none starts pending, never at the process latch's "ok", so a
// block is never ready before readiness says so.
func TestRunsOwnStatusStartsPending(t *testing.T) {
	if status := (&router.WarpStatusState{}).Status(); !strings.HasPrefix(status, "error not ready") {
		t.Fatalf("a fresh latch reads %q", status)
	}
}

var _ sidecar.HttpEngine = (*fakeRunEngine)(nil)
var _ sidecar.StreamEngine = (*fakeRunEngine)(nil)
