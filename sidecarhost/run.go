package sidecarhost

// Run: one engine as a warp block (§19.4; D-OPS-1, R-OPS-1, R-OPS-7, R-CTX-32), the lifecycle every new
// block shares, modelled on server/mcp/run.go:
//
//   - GET /status on the status port from the first moment, "error not ready: ..." until readiness
//     latches, so warpctl's deploy poll waits for the block and, if it never comes, reverts while the
//     old block keeps serving (TASKDRAIN1);
//   - readiness, once, on its own goroutine: the {vault: no} rule, then each pending group the block
//     reads, then for a block with Postgres router.CheckStartupReadiness, then Engine.Ready polled until
//     ReadyTimeout. A failure latches not ready and Run keeps serving /status, never exiting. A block
//     with no Postgres never runs the shared check, which reads migration_audit
//     (plan-OPS-small-readings #9). A closed gate is the engine's to answer, 503 with Retry-After, and
//     the block stays ready (§19.11);
//   - once ready: the HttpEngine's routes beside /status behind §19.8's route metrics, the
//     StreamEngine's listeners, each bound, wrapped and served on its own goroutine, the stats pusher,
//     and urnetwork_<svc>_ready, which follows Engine.Ready until the drain;
//   - at ctx's end, the block's SIGTERM: draining at /status, the listeners closed with the HTTP
//     server's drain, Engine.Drain, the stats flushed, then Engine.Close.
//
// A block's main calls server.ScrubCommandLogs as its first statement, before anything can log, and
// defers its Flush, as server/cli/mcp/main.go does; CheckMainScrubsLogsFirst holds a host's main.go to
// that, and each host's test calls it.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// Where a {vault: no} block's secret_files mount (warp/warpctl/run.go), and so where its WARP_VAULT_HOME
// points (plan-OPS-small-readings #4).
const ScopedVaultHome = "/srv/warp/secrets"

// A service as urnetwork_<svc>_* names it: lowercase letters and digits.
var runServicePattern = regexp.MustCompile(`^[a-z0-9]+$`)

// One raw listener a StreamEngine serves.
type RunListener struct {
	// The block's port for it, mapped to the host's as warp maps its ports.
	Port int
	// Wraps the bound listener before the engine serves it, as TLS-M5-07's NewProxyHeaderListener does
	// for a port behind the lb's PROXY header, so this package imports no engine. nil serves it as bound.
	Wrap func(listener net.Listener) (net.Listener, error)
}

// One pending migration group a block reads (§19.7): its name for the not-ready reason, and its marker,
// such as server.SidecarMg1Linked.
type RunGroup struct {
	Name   string
	Linked func() bool
}

// Run's tunables.
type RunSettings struct {
	// How long readiness may take before it latches not ready: 300 s, the deploy poll's window, as
	// router.CheckStartupReadiness's.
	ReadyTimeout time.Duration
	// How often Engine.Ready is asked, while readiness waits and afterwards for the ready gauge: 1 s.
	ReadyPollInterval time.Duration
	// The status port's HTTP server, as mcp's: its drain grace and shutdown bound.
	HttpServer server.HttpServerOptions
}

// The defaults.
func DefaultRunSettings() *RunSettings {
	return &RunSettings{
		ReadyTimeout:      300 * time.Second,
		ReadyPollInterval: 1 * time.Second,
		HttpServer: server.HttpServerOptions{
			ReadTimeout:           15 * time.Second,
			WriteTimeout:          30 * time.Second,
			IdleTimeout:           5 * time.Minute,
			ShutdownTimeout:       60 * time.Second,
			KeepaliveDrainTimeout: 10 * time.Second,
		},
	}
}

// What a block's main gives Run.
type RunOptions struct {
	// The <svc> of urnetwork_<svc>_*, lowercase letters and digits, as dnsedge for dnsedge-1 and -2;
	// the stats pusher names the warp service apart.
	Service string
	// GET /status's port, 80 in a block, which also serves an HttpEngine's routes.
	StatusPort int
	// The engine whose Ready, Drain and Close Run calls.
	Engine sidecar.Engine
	// How Engine serves, as the shape it declares (core-types-engine-shape): Http for routes on the
	// status port, Stream for Listeners, both when it is both, each nil or Engine itself. An engine
	// that listens on nothing, as smtpedge, sets neither.
	Http      sidecar.HttpEngine
	Stream    sidecar.StreamEngine
	Listeners []*RunListener
	// Whether the block holds a Postgres pool, §19.4's column.
	Postgres bool
	// The pending groups whose tables the block reads. Each needs Postgres.
	Groups []*RunGroup
	// Where urnetwork_<svc>_* is declared. nil: NewMetrics, the process's.
	Metrics sidecar.Metrics
	// The block's /status latch. nil: one of Run's own, pending until readiness latches.
	WarpStatus *router.WarpStatusState
	// The clock readiness waits on. nil: real time.
	Clock sidecar.Clock
	// nil: the defaults.
	Settings *RunSettings
}

// Refuses options no block could run before anything is read or bound.
func (self *RunOptions) Validate() error {
	if !runServicePattern.MatchString(self.Service) {
		return fmt.Errorf("sidecarhost run: service %q is not lowercase letters and digits", self.Service)
	}
	if self.StatusPort < 1 || 65_535 < self.StatusPort {
		return fmt.Errorf("sidecarhost run: status port %d is outside [1,65535]", self.StatusPort)
	}
	if self.Engine == nil {
		return errors.New("sidecarhost run: no engine")
	}
	if self.Stream == nil && 0 < len(self.Listeners) {
		return errors.New("sidecarhost run: listeners with no stream engine to serve them")
	}
	if self.Stream != nil && len(self.Listeners) == 0 {
		return errors.New("sidecarhost run: a stream engine with no listener")
	}
	ports := map[int]bool{self.StatusPort: true}
	for _, listener := range self.Listeners {
		if listener == nil || listener.Port < 1 || 65_535 < listener.Port {
			return errors.New("sidecarhost run: a listener with no port in [1,65535]")
		}
		if ports[listener.Port] {
			return fmt.Errorf("sidecarhost run: port %d twice", listener.Port)
		}
		ports[listener.Port] = true
	}
	for _, group := range self.Groups {
		if group == nil || group.Name == "" || group.Linked == nil {
			return errors.New("sidecarhost run: a group with no name or marker")
		}
	}
	if 0 < len(self.Groups) && !self.Postgres {
		return errors.New("sidecarhost run: groups' tables read with no Postgres pool")
	}
	return nil
}

// What Run reaches outside itself, so a test can stand in for each.
type runDependencies struct {
	listenAndServe       func(ctx context.Context, addr string, handler http.Handler, reusePort bool, options server.HttpServerOptions) error
	listen               func(ctx context.Context, port int) (net.Listener, error)
	startupReadiness     func(ctx context.Context) error
	startStatsPusher     func(ctx context.Context) func()
	statsPushUnavailable func() error
	scopedVault          func() error
	underWarp            func() bool
	// told readiness's outcome once it is latched, nil for ready: a test's hook, nil in production
	latched func(err error)
}

// The address the block's port maps to on the host, as warp's ports give it.
func runListenAddress(port int) (address string, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = recoveredError(recovered)
		}
	}()
	listenIpv4, _, listenPort := server.RequireListenIpPort(port)
	return net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)), nil
}

// Binds the block's port on the host.
func runListen(ctx context.Context, port int) (net.Listener, error) {
	address, err := runListenAddress(port)
	if err != nil {
		return nil, err
	}
	listenConfig := net.ListenConfig{}
	return listenConfig.Listen(ctx, "tcp", address)
}

// Whether this process runs as a warp block of a deployed environment: WARP_ENV names one other than
// local, and WARP_SERVICE and WARP_BLOCK are set.
func runUnderWarp(lookupEnv func(string) (string, bool)) bool {
	env, _ := lookupEnv("WARP_ENV")
	service, _ := lookupEnv("WARP_SERVICE")
	block, _ := lookupEnv("WARP_BLOCK")
	return env != "" && env != "local" && service != "" && block != ""
}

// The {vault: no} rule (§19.4; plan-OPS-small-readings #4): under warp a new block mounts no vault, only
// its secret_files at ScopedVaultHome, and server.Vault finds them only with WARP_VAULT_HOME there. A
// block with the whole vault mounted (warpctl sets WARP_VAULT then), or one whose vault root is
// elsewhere, holds secrets it should not or reads none it needs, so it stays not ready. Outside warp
// the rule does not apply.
func checkScopedVault(lookupEnv func(string) (string, bool), vaultHomeRoot string) error {
	if !runUnderWarp(lookupEnv) {
		return nil
	}
	if vault, ok := lookupEnv("WARP_VAULT"); ok && vault != "" {
		return errors.New("the whole vault is mounted; a sidecar block mounts {vault: no} and only its secret_files")
	}
	if filepath.Clean(vaultHomeRoot) != ScopedVaultHome {
		return fmt.Errorf("the vault root is %s; a {vault: no} block sets WARP_VAULT_HOME=%s, where its secret_files mount", vaultHomeRoot, ScopedVaultHome)
	}
	return nil
}

// Serves the block until ctx ends, then drains it, and returns once the engine has closed. It returns
// early only for options no block could run or a status port it cannot bind; a block that is not ready
// keeps serving /status until ctx ends.
func Run(ctx context.Context, options *RunOptions) error {
	return run(ctx, options, &runDependencies{
		listenAndServe:       server.HttpListenAndServeWithReusePort,
		listen:               runListen,
		startupReadiness:     router.CheckStartupReadiness,
		startStatsPusher:     server.StartStatsPusher,
		statsPushUnavailable: server.StatsPushUnavailable,
		scopedVault: func() error {
			return checkScopedVault(os.LookupEnv, server.VaultHomeRoot())
		},
		underWarp: func() bool {
			return runUnderWarp(os.LookupEnv)
		},
	})
}

// Readiness, once: the rule, the groups, the shared check, then the engine. Returns the stream
// listeners, bound and wrapped, for the engine to serve.
func (self *runDependencies) readiness(ctx context.Context, options *RunOptions, settings *RunSettings, clock sidecar.Clock) (listeners []net.Listener, returnErr error) {
	timeout := clock.After(settings.ReadyTimeout)
	if err := self.scopedVault(); err != nil {
		return nil, err
	}
	for _, group := range options.Groups {
		if !group.Linked() {
			return nil, fmt.Errorf("the block reads %s's tables, which this binary does not link", group.Name)
		}
	}
	if options.Postgres {
		if err := self.startupReadiness(ctx); err != nil {
			return nil, err
		}
	}
	for !options.Engine.Ready() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("the engine was not ready within %v", settings.ReadyTimeout)
		case <-clock.After(settings.ReadyPollInterval):
		}
	}
	defer func() {
		if returnErr != nil {
			for _, listener := range listeners {
				listener.Close()
			}
			listeners = nil
		}
	}()
	for _, runListener := range options.Listeners {
		listener, err := self.listen(ctx, runListener.Port)
		if err != nil {
			return listeners, fmt.Errorf("port %d: %w", runListener.Port, err)
		}
		if runListener.Wrap != nil {
			wrapped, err := runListener.Wrap(listener)
			if err != nil {
				listener.Close()
				return listeners, fmt.Errorf("port %d: %w", runListener.Port, err)
			}
			listener = wrapped
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

// What readiness gave.
type runReadiness struct {
	listeners []net.Listener
	err       error
}

func run(ctx context.Context, options *RunOptions, deps *runDependencies) error {
	if ctx == nil {
		return errors.New("sidecarhost run: no context")
	}
	if options == nil {
		return errors.New("sidecarhost run: no options")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	settings := options.Settings
	if settings == nil {
		settings = DefaultRunSettings()
	}
	clock := options.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	metrics := options.Metrics
	if metrics == nil {
		metrics = NewMetrics()
	}
	status := options.WarpStatus
	if status == nil {
		status = &router.WarpStatusState{}
	}
	logTag := "[" + options.Service + "]"
	readyGauge := metrics.Gauge(
		"urnetwork_"+options.Service+"_ready",
		"1 while the block is ready and not draining: readiness latched and the engine ready.",
	)
	readyGauge.Set(0)

	processCtx, processCancel := context.WithCancel(context.Background())
	defer processCancel()
	serveCtx, serveCancel := context.WithCancel(processCtx)
	defer serveCancel()

	// the engine's routes answer once readiness latches, and not after the drain begins to matter:
	// the HTTP server's own drain ends them
	var admitted atomic.Bool
	routes := []*router.Route{router.NewRoute("GET", "/status", status.Handler)}
	if options.Http != nil {
		engineHandler := newRouteMetrics(metrics, options.Service, clock).wrap(options.Http.Handler())
		notReadySeconds := strconv.Itoa(max(int(settings.ReadyPollInterval/time.Second), 1))
		routes = append(routes, router.NewRoute("*", ".*", func(w http.ResponseWriter, r *http.Request) {
			if !admitted.Load() {
				w.Header().Set("Retry-After", notReadySeconds)
				http.Error(w, "Service not ready.", http.StatusServiceUnavailable)
				return
			}
			engineHandler.ServeHTTP(w, r)
		}))
	}
	httpRouter := router.NewRouter(processCtx, routes)

	statusAddress, err := runListenAddress(options.StatusPort)
	if err != nil {
		options.Engine.Close()
		return err
	}
	// /status from the first moment
	httpDone := make(chan error, 1)
	go server.HandleError(func() {
		httpDone <- deps.listenAndServe(serveCtx, statusAddress, httpRouter, false, settings.HttpServer)
	})
	// the port alone: the log carries no address (I-13)
	glog.Infof("%sserving status on port %d\n", logTag, options.StatusPort)

	readinessCtx, readinessCancel := context.WithCancel(ctx)
	defer readinessCancel()
	readinessDone := make(chan *runReadiness, 1)
	go server.HandleError(func() {
		listeners, err := deps.readiness(readinessCtx, options, settings, clock)
		readinessDone <- &runReadiness{listeners: listeners, err: err}
	})

	var listeners []net.Listener
	var serveWait sync.WaitGroup
	flushStats := func() {}
	var httpErr error
	httpEnded := false
	drainGauge := make(chan struct{})
	gaugeDone := make(chan struct{})
	close(gaugeDone)

	readinessTaken := false
	select {
	case <-ctx.Done():
	case httpErr = <-httpDone:
		httpEnded = true
	case readiness := <-readinessDone:
		readinessTaken = true
		if readiness.err != nil {
			glog.Errorf("%snot ready (%s)\n", logTag, readiness.err)
			status.SetNotReady(readiness.err)
			if deps.latched != nil {
				deps.latched(readiness.err)
			}
		} else if ctx.Err() != nil {
			for _, listener := range readiness.listeners {
				listener.Close()
			}
		} else {
			listeners = readiness.listeners
			for _, listener := range listeners {
				serveWait.Add(1)
				go server.HandleError(func() {
					defer serveWait.Done()
					if err := options.Stream.Serve(listener); err != nil && ctx.Err() == nil {
						glog.Errorf("%sa listener stopped (%s)\n", logTag, err)
					}
				})
			}
			admitted.Store(true)
			status.SetReady()
			readyGauge.Set(1)
			glog.Infof("%sready\n", logTag)
			// a block that never got ready makes no series in the metrics store
			flushStats = deps.startStatsPusher(processCtx)
			if deps.underWarp() {
				if err := deps.statsPushUnavailable(); err != nil {
					glog.Errorf("%sstats are off under warp, so this block runs unseen: %s\n", logTag, err)
				}
			}
			gaugeDone = make(chan struct{})
			go server.HandleError(func() {
				defer close(gaugeDone)
				for {
					select {
					case <-drainGauge:
						return
					case <-clock.After(settings.ReadyPollInterval):
					}
					if options.Engine.Ready() {
						readyGauge.Set(1)
					} else {
						readyGauge.Set(0)
					}
				}
			})
			if deps.latched != nil {
				deps.latched(nil)
			}
		}
		select {
		case <-ctx.Done():
		case httpErr = <-httpDone:
			httpEnded = true
		}
	}

	// the drain: /status says so, the listeners close, the engine finishes, the stats go, it closes
	status.SetDrainingIfReady()
	close(drainGauge)
	<-gaugeDone
	readyGauge.Set(0)
	readinessCancel()
	if !readinessTaken {
		// readiness ends with its ctx; whatever it bound, nothing serves
		go server.HandleError(func() {
			for _, listener := range (<-readinessDone).listeners {
				listener.Close()
			}
		})
	}
	serveCancel()
	for _, listener := range listeners {
		listener.Close()
	}
	if !httpEnded {
		httpErr = <-httpDone
	}
	if httpErr != nil {
		glog.Infof("%sstatus server ended (%s)\n", logTag, httpErr)
	}
	options.Engine.Drain()
	httpRouter.FlushStats()
	flushStats()
	options.Engine.Close()
	serveWait.Wait()
	glog.Infof("%sclosed\n", logTag)
	if httpErr != nil && ctx.Err() == nil {
		// the status port failed before the drain: warpctl cannot see this block
		return httpErr
	}
	return nil
}

// Whether the main.go at mainPath calls server.ScrubCommandLogs as its first statement, which §19.4 asks
// of every new block's main, so nothing it logs escapes the scrubber. Each server/cli/<svc> host's test
// calls this on its own main.go.
func CheckMainScrubsLogsFirst(mainPath string) error {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, mainPath, nil, 0)
	if err != nil {
		return err
	}
	serverName := "server"
	for _, imported := range file.Imports {
		if imported.Path.Value == `"github.com/urnetwork/server"` && imported.Name != nil {
			serverName = imported.Name.Name
		}
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Name.Name != "main" {
			continue
		}
		if function.Body == nil || len(function.Body.List) == 0 {
			return fmt.Errorf("%s: main is empty", mainPath)
		}
		var call ast.Expr
		switch first := function.Body.List[0].(type) {
		case *ast.AssignStmt:
			if len(first.Rhs) == 1 {
				call = first.Rhs[0]
			}
		case *ast.ExprStmt:
			call = first.X
		}
		if callExpr, ok := call.(*ast.CallExpr); ok {
			if selector, ok := callExpr.Fun.(*ast.SelectorExpr); ok {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == serverName && selector.Sel.Name == "ScrubCommandLogs" {
					return nil
				}
			}
		}
		return fmt.Errorf("%s: main's first statement is not server.ScrubCommandLogs()", mainPath)
	}
	return fmt.Errorf("%s: no main", mainPath)
}
