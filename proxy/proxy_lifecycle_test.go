// Proxy harness lifecycle tests force each in-process connect owner to remain
// live at teardown so no Redis or database cleanup can escape the harness.
package proxy

import (
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	proxycore "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
)

// Provides exact close and idle barriers for the harness lifecycle helper.
type blockingProxyConnectLifecycle struct {
	closeOnce   sync.Once
	waitOnce    sync.Once
	closed      chan struct{}
	waitEntered chan struct{}
	release     <-chan struct{}
}

// Adapts an owned join while preserving the caller's finite deadline.
type proxyTestJoinFunc func(context.Context) error

// Calls the owned join without substituting cancellation for completion.
func (self proxyTestJoinFunc) CloseAndWait(ctx context.Context) error { return self(ctx) }

// Adapts a synchronous owned release.
type proxyTestCloseFunc func()

// Executes the release in the lifecycle owner's goroutine.
func (self proxyTestCloseFunc) Close() { self() }

// Records the two stages of connect cleanup independently.
type proxyTestConnectFuncs struct {
	close func()
	wait  func(context.Context) bool
}

// Closes connection admission before the final idle join.
func (self proxyTestConnectFuncs) Close() { self.close() }

// Joins the connection owner's deferred cleanup.
func (self proxyTestConnectFuncs) WaitForIdle(ctx context.Context) bool { return self.wait(ctx) }

// Records proxy frontend admission separately from admitted request completion.
type proxyTestIngressFuncs struct {
	drain func()
	wait  func(context.Context) bool
}

// Stops frontend admission without waiting under a state lock.
func (self proxyTestIngressFuncs) Drain() { self.drain() }

// Waits for admitted authentication and handler work.
func (self proxyTestIngressFuncs) WaitIdle(ctx context.Context) bool { return self.wait(ctx) }

// Observes the production proxy barrier without replacing its request counter.
type proxyTestHttpIngressBarrier struct {
	*httpServer
	entered chan struct{}
}

// Publishes evaluation of the real WaitIdle select, not wrapper entry.
func (self *proxyTestHttpIngressBarrier) WaitIdle(ctx context.Context) bool {
	return self.httpServer.WaitIdle(&proxyTestWaitContext{Context: ctx, entered: self.entered})
}

// Exposes a real context wait while preserving the owner's cancellation bound.
type proxyTestWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

// Signals only when the production join evaluates its cancellation branch.
func (self *proxyTestWaitContext) Done() <-chan struct{} {
	self.once.Do(func() { close(self.entered) })
	return self.Context.Done()
}

// Retains deliberately induced teardown failures without failing their caller.
type proxyTestErrorTb struct {
	testing.TB
	stateLock sync.Mutex
	errors    []string
}

// Preserves every reported failure for assertions after teardown joins.
func (self *proxyTestErrorTb) Errorf(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errors = append(self.errors, fmt.Sprintf(format, args...))
}

// Copies the evidence while excluding concurrent error reporters.
func (self *proxyTestErrorTb) errorText() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.errors, "\n")
}

// Exposes the real startup guard's fatal boundary without exiting this test.
type proxyTestStartupTb struct{ testing.TB }

// Turns only the expected constructor failure into a locally recoverable value.
func (self *proxyTestStartupTb) Fatalf(string, ...any) { panic("synthetic frontend startup fatal") }

// Drives the actual frontend-context acquisition used by both constructors.
// A worker's cancellation must fail bring-up without canceling final controls.
func TestProxyHarnessFrontendsKeepPlatformContext(t *testing.T) {
	harness := newProxyTestHarness(t, func(*proxyTestHarness) {})
	defer harness.close(t)
	frontendCtx, cancelFrontend := harness.newFrontendContext()
	defer cancelFrontend()
	cancelFrontend()
	if frontendCtx.Err() == nil || harness.ctx.Err() != nil {
		t.Error("frontend worker cancellation escaped into the platform root")
	}
	var observedPanic any
	func() {
		defer func() { observedPanic = recover() }()
		harness.requireFrontendStarted(&proxyTestStartupTb{TB: t}, frontendCtx)
	}()
	if observedPanic != "synthetic frontend startup fatal" {
		t.Errorf("isolated listener failure was no longer a startup failure: %v", observedPanic)
	}
	var events []string
	harness.lifecycle.manager = proxyTestJoinFunc(func(context.Context) error {
		if harness.ctx.Err() != nil {
			t.Error("manager cleanup lost platform context after frontend cancellation")
		}
		events = append(events, "manager")
		return nil
	})
	harness.lifecycle.provider.oob = proxyTestJoinFunc(func(context.Context) error {
		if harness.ctx.Err() != nil {
			t.Error("out-of-band cleanup lost platform context after frontend cancellation")
		}
		events = append(events, "out-of-band")
		return nil
	})
	harness.close(t)
	if !reflect.DeepEqual(events, []string{"manager", "out-of-band"}) || harness.ctx.Err() == nil {
		t.Errorf("ordered cleanup did not complete: %v", events)
	}
}

// Pins actual constructor arguments to the tested scoped frontend boundary.
//
//go:embed proxy_test.go
var proxyTestSetupSource string

// Constructor/context wiring is checked alongside the runtime rollback proof.
func TestProxyHarnessFrontendSetupUsesOwnedContext(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "proxy_test.go", proxyTestSetupSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	constructors, startupChecks := 0, 0
	listenerAssignments := map[string]int{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "setupProxyTestWithOptions" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if assignment, ok := node.(*ast.AssignStmt); ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 {
				if selector, ok := assignment.Lhs[0].(*ast.SelectorExpr); ok {
					if want, found := map[string]string{"httpListener": "httpS", "socksListener": "socks5"}[selector.Sel.Name]; found {
						listenerAssignments[selector.Sel.Name]++
						if value, ok := assignment.Rhs[0].(*ast.Ident); !ok || value.Name != want {
							t.Errorf("%s did not retain its actual constructor owner", selector.Sel.Name)
						}
					}
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if target, ok := call.Fun.(*ast.Ident); ok && (target.Name == "NewHttpServer" || target.Name == "NewSocks5Server") {
				constructors++
				if len(call.Args) < 2 {
					t.Errorf("%s omitted context ownership", target.Name)
					return true
				}
				for i, want := range []string{"frontendCtx", "frontendCancel"} {
					if argument, ok := call.Args[i].(*ast.Ident); !ok || argument.Name != want {
						t.Errorf("%s argument %d bypassed %s", target.Name, i, want)
					}
				}
			}
			if target, ok := call.Fun.(*ast.SelectorExpr); ok && target.Sel.Name == "requireFrontendStarted" {
				startupChecks++
			}
			return true
		})
	}
	if constructors != 2 || startupChecks != 1 {
		t.Errorf("actual setup constructor/startup wiring = %d/%d", constructors, startupChecks)
	}
	for _, name := range []string{"httpListener", "socksListener"} {
		if listenerAssignments[name] != 1 {
			t.Errorf("actual setup retained %s %d times", name, listenerAssignments[name])
		}
	}
}

// An actual HttpProxy request is held in the pre-device callback lane, which
// includes authentication work outside manager admission accounting.
func TestProxyHarnessJoinsPreDeviceHttpAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	entered, release, clientDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var enterOnce sync.Once
	core := proxycore.NewHttpProxy(proxycore.DefaultHttpProxySettings())
	core.ConnectDialContextWithRequest = func(context.Context, *http.Request, string, string) (net.Conn, error) {
		enterOnce.Do(func() { close(entered) })
		<-release
		return nil, errors.New("synthetic pre-device owner released")
	}
	origin := httptest.NewServer(core)
	proxyUrl, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyUrl)}
	defer transport.CloseIdleConnections()
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://upstream.example/", nil)
		response, _ := (&http.Client{Transport: transport}).Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	defer func() {
		cancelRequest()
		unblock()
		origin.Close()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-clientDone:
		case <-joinCtx.Done():
			t.Error("proxy test client did not join")
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("proxy callback did not enter")
	}
	if core.ActiveCount() != 1 {
		t.Fatalf("active pre-device owners = %d", core.ActiveCount())
	}
	ingress := &proxyTestHttpIngressBarrier{httpServer: &httpServer{httpProxy: core}, entered: make(chan struct{})}
	harness := newProxyTestHarness(t, func(harness *proxyTestHarness) {
		harness.lifecycle.httpIngress = ingress
		harness.lifecycle.manager = proxyTestJoinFunc(func(context.Context) error {
			if core.ActiveCount() != 0 {
				t.Error("manager teardown preceded pre-device request join")
			}
			return nil
		})
	})
	closed := make(chan struct{})
	go func() { defer close(closed); harness.close(t) }()
	defer func() {
		cancelRequest()
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-closed:
		case <-joinCtx.Done():
			t.Error("proxy harness close did not join")
		}
	}()
	select {
	case <-ingress.entered:
	case <-closed:
		t.Error("actual harness close bypassed a live pre-device request")
		return
	case <-ctx.Done():
		t.Error("harness did not reach real ingress join")
		return
	}
	cancelRequest()
	unblock()
	core.Drain()
	if !core.WaitIdle(ctx) {
		t.Error("released proxy request did not leave the real counter")
	}
}

// Actual harness close must retain a held platform handler even after the
// underlying server sockets have closed and the route context is canceled.
func TestProxyHarnessJoinsApiHandlerAfterSocketClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, handlerDone, clientDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { defer close(handlerDone); close(entered); <-release })}
	owner := server.NewTestHttpServer(ctx, listener, httpServer)
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
	}()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String(), nil)
		response, _ := (&http.Client{Transport: transport}).Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		for _, done := range []<-chan struct{}{handlerDone, clientDone} {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("api test child did not join")
			}
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("api handler did not enter")
	}
	if err := httpServer.Close(); err != nil {
		t.Fatal(err)
	}
	joinEntered := make(chan struct{})
	var stopJoin context.CancelFunc
	tb := &proxyTestErrorTb{TB: t}
	harness := newProxyTestHarness(t, func(harness *proxyTestHarness) {
		harness.lifecycle.apiHttp = proxyTestJoinFunc(func(ctx context.Context) error {
			joinCtx, stop := context.WithCancel(ctx)
			stopJoin = stop
			defer stop()
			return owner.CloseAndWait(&proxyTestWaitContext{Context: joinCtx, entered: joinEntered})
		})
	})
	closed := make(chan struct{})
	go func() { defer close(closed); harness.close(tb) }()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-closed:
		case <-joinCtx.Done():
			t.Error("api harness caller did not join")
		}
	}()
	select {
	case <-joinEntered:
	case <-closed:
		t.Error("actual harness close bypassed its still-live api handler")
		return
	case <-ctx.Done():
		t.Error("harness api join was not reached")
		return
	}
	stopJoin()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("bounded harness failure did not return while handler remained held")
	}
	if errorText := tb.errorText(); !strings.Contains(errorText, "http handlers did not join") || !strings.Contains(errorText, context.Canceled.Error()) {
		t.Errorf("socket close was mistaken for handler completion: %s", errorText)
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Error(err)
	}
}

// Forces Goexit and panic after each actual owner-acquisition stage. The same
// scoped constructor and actual harness close used by setup must complete their
// joins before the simulated enclosing TestEnv scope can be released.
func TestProxyHarnessPartialSetupJoinsEveryAcquiredStage(t *testing.T) {
	stages := []string{"exchange", "handler", "connect http", "platform api", "network space", "strategy", "out-of-band", "client", "transport", "local nat", "remote nat", "manager", "device rpc", "frontend context", "socks listener", "socks ingress", "http listener", "http ingress", "wg", "tls frontend"}
	for _, panicExit := range []bool{false, true} {
		for failAt, heldStage := range stages {
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				entered, release, scopeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				var events []string
				var ownedCtx context.Context
				var observedPanic any
				visit := func(name string) {
					events = append(events, name)
					if name == heldStage {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							t.Errorf("%s: held cleanup expired", name)
						}
					}
				}
				go func() {
					defer close(scopeDone)
					defer func() { observedPanic = recover() }()
					newProxyTestHarness(t, func(harness *proxyTestHarness) {
						ownedCtx = harness.ctx
						join := func(name string) proxyTestJoiner {
							return proxyTestJoinFunc(func(context.Context) error { visit(name); return nil })
						}
						closer := func(name string) proxyTestCloser { return proxyTestCloseFunc(func() { visit(name) }) }
						connection := func(name string) proxyConnectLifecycle {
							return proxyTestConnectFuncs{close: func() {}, wait: func(context.Context) bool { visit(name); return true }}
						}
						ingress := func(name string) proxyTestIngressOwner {
							return proxyTestIngressFuncs{drain: func() {}, wait: func(context.Context) bool { visit(name); return true }}
						}
						acquire := []func(){
							func() { harness.lifecycle.exchange = connection("exchange") },
							func() { harness.lifecycle.handler = connection("handler") },
							func() { harness.lifecycle.connectHttp = join("connect http") },
							func() { harness.lifecycle.apiHttp = join("platform api") },
							func() { harness.lifecycle.networkSpace = closer("network space") },
							func() { harness.lifecycle.provider.strategy = closer("strategy") },
							func() { harness.lifecycle.provider.oob = join("out-of-band") },
							func() { harness.lifecycle.provider.client = join("client") },
							func() { harness.lifecycle.provider.transport = join("transport") },
							func() { harness.lifecycle.provider.localNat = join("local nat") },
							func() { harness.lifecycle.provider.remoteNat = closer("remote nat") },
							func() { harness.lifecycle.manager = join("manager") },
							func() { harness.lifecycle.deviceRpc = join("device rpc") },
							func() { harness.lifecycle.frontendCancel = func() { visit("frontend context") } },
							func() { harness.lifecycle.socksListener = join("socks listener") },
							func() { harness.lifecycle.socksIngress = ingress("socks ingress") },
							func() { harness.lifecycle.httpListener = join("http listener") },
							func() { harness.lifecycle.httpIngress = ingress("http ingress") },
							func() { harness.lifecycle.wgCancel = func() { visit("wg") } },
							func() { harness.lifecycle.tlsFrontend = join("tls frontend") },
						}
						for _, step := range acquire[:failAt+1] {
							step()
						}
						if panicExit {
							panic("synthetic partial construction")
						}
						runtime.Goexit()
					})
				}()
				defer func() {
					unblock()
					joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					select {
					case <-scopeDone:
					case <-joinCtx.Done():
						t.Errorf("%s: construction owner did not join", heldStage)
					}
				}()
				select {
				case <-entered:
				case <-scopeDone:
					t.Errorf("%s panic=%t: scope escaped before acquired owner joined", heldStage, panicExit)
					return
				case <-ctx.Done():
					t.Errorf("%s: acquired join not reached", heldStage)
					return
				}
				select {
				case <-scopeDone:
					t.Errorf("%s: scope popped with held owner", heldStage)
					return
				default:
				}
				unblock()
				select {
				case <-scopeDone:
				case <-ctx.Done():
					t.Errorf("%s: rollback did not finish", heldStage)
					return
				}
				if ownedCtx.Err() == nil {
					t.Errorf("%s: root context remains owned after scope exit", heldStage)
				}
				var want []string
				for _, name := range []string{"wg", "http ingress", "socks ingress", "frontend context", "http listener", "socks listener", "tls frontend", "device rpc", "manager", "remote nat", "transport", "local nat", "client", "out-of-band", "strategy", "network space", "handler", "exchange", "connect http", "platform api"} {
					for _, acquired := range stages[:failAt+1] {
						if name == acquired {
							want = append(want, name)
						}
					}
				}
				if !reflect.DeepEqual(events, want) {
					t.Errorf("%s panic=%t: complete rollback = %v, want %v", heldStage, panicExit, events, want)
				}
				if panicExit && observedPanic != "synthetic partial construction" {
					t.Errorf("panic changed: %v", observedPanic)
				}
			}()
		}
	}
}

// The provider's external final request must still reach the real platform
// server after the client has joined and before the root context is canceled.
func TestProxyHarnessKeepsPlatformApiForOutOfBandCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	clientJoined := false
	harness := newProxyTestHarness(t, func(harness *proxyTestHarness) {
		harness.lifecycle.apiHttp = server.NewTestHttpServer(harness.ctx, listener, &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })})
		for _, frontend := range []struct {
			name string
			set  func(proxyTestJoiner)
		}{
			{name: "tls", set: func(owner proxyTestJoiner) { harness.lifecycle.tlsFrontend = owner }},
			{name: "device rpc", set: func(owner proxyTestJoiner) { harness.lifecycle.deviceRpc = owner }},
		} {
			frontend.set(proxyTestJoinFunc(func(context.Context) error { events = append(events, frontend.name); return nil }))
		}
		harness.lifecycle.manager = proxyTestJoinFunc(func(context.Context) error {
			if !reflect.DeepEqual(events, []string{"tls", "device rpc"}) {
				t.Errorf("manager preceded frontends: %v", events)
			}
			events = append(events, "manager")
			return nil
		})
		harness.lifecycle.provider.client = proxyTestJoinFunc(func(context.Context) error { clientJoined = true; events = append(events, "client"); return nil })
		harness.lifecycle.provider.oob = proxyTestJoinFunc(func(context.Context) error {
			if !clientJoined || harness.ctx.Err() != nil {
				t.Error("out-of-band handoff lost client-before-control ordering or live platform context")
			}
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String(), nil)
			response, err := (&http.Client{Transport: transport}).Do(request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				return fmt.Errorf("platform status %d", response.StatusCode)
			}
			events = append(events, "out-of-band")
			return nil
		})
	})
	defer harness.close(t)
	harness.close(t)
	harness.close(t)
	if harness.ctx.Err() == nil {
		t.Error("root was not canceled after all consumers joined")
	}
	if !reflect.DeepEqual(events, []string{"tls", "device rpc", "manager", "client", "out-of-band"}) {
		t.Errorf("cleanup order/idempotence = %v", events)
	}
}

// No method on an unacquired typed-nil interface may be called during rollback.
type proxyTestUnacquiredOwner struct{}

// Panics if typed-nil cleanup incorrectly calls the join surface.
func (self *proxyTestUnacquiredOwner) CloseAndWait(context.Context) error { panic("unacquired join") }

// Panics if typed-nil cleanup incorrectly calls the close surface.
func (self *proxyTestUnacquiredOwner) Close() { panic("unacquired close") }

// Panics if typed-nil cleanup incorrectly calls frontend admission.
func (self *proxyTestUnacquiredOwner) Drain() { panic("unacquired drain") }

// Panics if typed-nil cleanup incorrectly calls frontend wait.
func (self *proxyTestUnacquiredOwner) WaitIdle(context.Context) bool { panic("unacquired wait") }

// Panics if typed-nil cleanup incorrectly calls connect wait.
func (self *proxyTestUnacquiredOwner) WaitForIdle(context.Context) bool {
	panic("unacquired connect wait")
}

// A failed frontend join retains failure, cancels the root and still attempts
// later acquired owners; absent interfaces include typed nils.
func TestProxyHarnessFailureAttemptsRemainingOwnersAndTypedNils(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var unacquired *proxyTestUnacquiredOwner
	sentinel := errors.New("synthetic frontend join failure")
	var events []string
	lifecycle := proxyTestLifecycle{
		cancel: cancel, httpIngress: unacquired, socksIngress: unacquired, deviceRpc: unacquired,
		httpListener: unacquired, socksListener: unacquired,
		networkSpace: unacquired, handler: unacquired, exchange: unacquired, connectHttp: unacquired,
		tlsFrontend: proxyTestJoinFunc(func(context.Context) error { return sentinel }),
		manager: proxyTestJoinFunc(func(context.Context) error {
			if ctx.Err() == nil {
				t.Error("failure did not force cancellation")
			}
			events = append(events, "manager")
			return nil
		}),
		apiHttp:  proxyTestJoinFunc(func(context.Context) error { events = append(events, "api"); return nil }),
		provider: proxyTestProviderLifecycle{remoteNat: unacquired, transport: unacquired, localNat: unacquired, client: unacquired, oob: unacquired, strategy: unacquired},
	}
	bound, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := lifecycle.close(bound); !errors.Is(err, sentinel) {
		t.Errorf("cleanup failure lost: %v", err)
	}
	if err := lifecycle.close(bound); !errors.Is(err, sentinel) {
		t.Errorf("idempotent cleanup erased failure: %v", err)
	}
	if !reflect.DeepEqual(events, []string{"manager", "api"}) {
		t.Errorf("later owners skipped/repeated: %v", events)
	}
}

// Embeds the exact compiled production assembly, including overlay builds.
//
//go:embed proxy_api.go
var proxyApiAssemblySource string

// Production run must consume the same assembly that the owned tls fixture
// serves; a copied test route table cannot satisfy this source-wiring control.
func TestProxyApiRunUsesSharedHttpAssembly(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "proxy_api.go", proxyApiAssemblySource, 0)
	if err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, declaration := range file.Decls {
		method, ok := declaration.(*ast.FuncDecl)
		if !ok || method.Name.Name != "run" || method.Recv == nil {
			continue
		}
		ast.Inspect(method.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "httpConfiguration" {
				matched = true
			}
			return true
		})
	}
	if !matched {
		t.Error("actual apiServer.run bypasses the shared handler/configuration assembly")
	}
}

// The extracted assembly preserves every method/path/auth boundary and the
// production tls callback/timeouts. Requests use only missing authorization,
// so neither a real hosted device nor model/fixture access can occur.
func TestProxyApiSharedAssemblyPreservesRoutesAuthAndTls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transportTls := server.NewTransportTls(map[string]bool{}, &server.TransportTlsSettings{EnableSelfSign: true, DefaultHostName: "fixture.example"})
	assembly := &apiServer{ctx: ctx, cancel: cancel, proxyDeviceManager: &ProxyDeviceManager{}, transportTls: transportTls, settings: DefaultProxySettings()}
	handler, options, tlsConfig := assembly.httpConfiguration()
	if options.ReadTimeout != 15*time.Second || options.WriteTimeout != 30*time.Second || options.IdleTimeout != 5*time.Minute || options.ShutdownTimeout != 30*time.Second || options.KeepaliveDrainTimeout != 0 {
		t.Errorf("production timeouts changed: %+v", options)
	}
	for _, c := range []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodPost, path: "/warmup", status: http.StatusUnauthorized},
		{method: http.MethodGet, path: deviceRpcPath, status: http.StatusUnauthorized},
		{method: http.MethodPost, path: flowTracePath, status: http.StatusUnauthorized},
		{method: http.MethodGet, path: flowTracePath, status: http.StatusUnauthorized},
		{method: http.MethodGet, path: "/warmup", status: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: deviceRpcPath, status: http.StatusMethodNotAllowed},
		{method: http.MethodDelete, path: flowTracePath, status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(c.method, c.path, nil))
		if response.Code != c.status {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, response.Code, c.status)
		}
	}
	if tlsConfig.GetConfigForClient == nil {
		t.Fatal("production tls callback missing")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler, TLSConfig: tlsConfig, ReadTimeout: options.ReadTimeout, WriteTimeout: options.WriteTimeout, IdleTimeout: options.IdleTimeout}
	owner := server.NewTestHttpServer(ctx, listener, httpServer)
	defer func() {
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
	}()
	transport := &http.Transport{TLSClientConfig: &tls.Config{ServerName: "fixture.example", InsecureSkipVerify: true}}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+listener.Addr().String()+deviceRpcPath, nil)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("owned tls route bypassed auth: %d", response.StatusCode)
	}
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || response.TLS.PeerCertificates[0].VerifyHostname("fixture.example") != nil {
		t.Error("shared tls callback did not select the synthetic host certificate")
	}
}

// Records that admission was closed.
func (self *blockingProxyConnectLifecycle) Close() {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
}

// Holds the cleanup owner until the test releases it or the deadline expires.
func (self *blockingProxyConnectLifecycle) WaitForIdle(ctx context.Context) bool {
	self.waitOnce.Do(func() {
		close(self.waitEntered)
	})
	select {
	case <-ctx.Done():
		return false
	case <-self.release:
		return true
	}
}

// Teardown closes both owners before waiting and cannot return while the
// exchange still retains its final Redis cleanup.
func TestCloseProxyConnectLifecyclesJoinsHandlerAndExchange(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	handlerRelease := make(chan struct{})
	close(handlerRelease)
	exchangeRelease := make(chan struct{})
	handler := &blockingProxyConnectLifecycle{
		closed:      make(chan struct{}),
		waitEntered: make(chan struct{}),
		release:     handlerRelease,
	}
	exchange := &blockingProxyConnectLifecycle{
		closed:      make(chan struct{}),
		waitEntered: make(chan struct{}),
		release:     exchangeRelease,
	}
	closeDone := make(chan struct{})
	go func() {
		closeProxyConnectLifecycles(t, handler, exchange, func() {})
		close(closeDone)
	}()

	for _, closed := range []<-chan struct{}{handler.closed, exchange.closed} {
		select {
		case <-closed:
		case <-testCtx.Done():
			close(exchangeRelease)
			t.Fatal("proxy connect owner did not close admission")
		}
	}
	select {
	case <-exchange.waitEntered:
	case <-testCtx.Done():
		close(exchangeRelease)
		t.Fatal("proxy teardown did not enter exchange cleanup join")
	}
	select {
	case <-closeDone:
		close(exchangeRelease)
		t.Fatal("proxy teardown returned before exchange cleanup")
	default:
	}

	close(exchangeRelease)
	select {
	case <-closeDone:
	case <-testCtx.Done():
		t.Fatal("proxy teardown did not join exchange cleanup")
	}
}

// Joins an owner under the single fixture teardown deadline.
type proxyTestJoiner interface {
	CloseAndWait(context.Context) error
}

// Closes a synchronous owner after its asynchronous consumers have joined.
type proxyTestCloser interface {
	Close()
}

// Refuses frontend requests before joining every admitted authentication path.
type proxyTestIngressOwner interface {
	Drain()
	WaitIdle(context.Context) bool
}

// Treats an unacquired pointer carried by an interface as absent. Concrete
// constructors register only returned owners; this also makes partial tests
// and optional factory results safe without calling methods on typed nils.
func proxyTestOwnerPresent(owner any) bool {
	if owner == nil {
		return false
	}
	value := reflect.ValueOf(owner)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

// Owns the provider's producer-to-external-control handoff. Some existing tests
// explicitly stop the provider before ending the full fixture, so close shares
// one result with the enclosing lifecycle rather than issuing a second teardown.
type proxyTestProviderLifecycle struct {
	remoteNat proxyTestCloser
	transport proxyTestJoiner
	localNat  proxyTestJoiner
	client    proxyTestJoiner
	oob       proxyTestJoiner
	strategy  proxyTestCloser
	once      sync.Once
	err       error
}

// Joins clients before external out-of-band control, whose final requests may
// be handed off during client cleanup. Every acquired owner is attempted.
func (self *proxyTestProviderLifecycle) CloseAndWait(ctx context.Context) error {
	self.once.Do(func() {
		if proxyTestOwnerPresent(self.remoteNat) {
			self.remoteNat.Close()
		}
		for _, owner := range []struct {
			name string
			join proxyTestJoiner
		}{
			{name: "provider transport", join: self.transport},
			{name: "provider local nat", join: self.localNat},
			{name: "provider client", join: self.client},
			{name: "provider out-of-band control", join: self.oob},
		} {
			if proxyTestOwnerPresent(owner.join) {
				if err := owner.join.CloseAndWait(ctx); err != nil {
					self.err = errors.Join(self.err, fmt.Errorf("%s: %w", owner.name, err))
				}
			}
		}
		if proxyTestOwnerPresent(self.strategy) {
			self.strategy.Close()
		}
	})
	return self.err
}

// Records each owner immediately after acquisition. Fields stop changing before
// close starts. Concurrent and repeated closes share the same ordered result.
type proxyTestLifecycle struct {
	frontendCancel context.CancelFunc
	httpListener   proxyTestJoiner
	socksListener  proxyTestJoiner
	httpIngress    proxyTestIngressOwner
	socksIngress   proxyTestIngressOwner
	tlsFrontend    proxyTestJoiner
	deviceRpc      proxyTestJoiner
	wgCancel       context.CancelFunc
	manager        proxyTestJoiner
	provider       proxyTestProviderLifecycle
	networkSpace   proxyTestCloser
	handler        proxyConnectLifecycle
	exchange       proxyConnectLifecycle
	connectHttp    proxyTestJoiner
	apiHttp        proxyTestJoiner
	cancel         context.CancelFunc
	once           sync.Once
	err            error
}

// Frontend auth/request owners finish before manager shutdown. The platform
// rest/connect servers remain available for manager and provider cleanup,
// including the external out-of-band join. Cancellation cannot replace joins;
// a timeout is retained while every later known owner is still attempted.
func (self *proxyTestLifecycle) close(ctx context.Context) error {
	self.once.Do(func() {
		record := func(name string, err error) {
			if err != nil {
				self.err = errors.Join(self.err, fmt.Errorf("%s: %w", name, err))
				self.cancel()
			}
		}
		join := func(name string, owner proxyTestJoiner) {
			if proxyTestOwnerPresent(owner) {
				record(name, owner.CloseAndWait(ctx))
			}
		}
		wait := func(name string, idle bool) {
			if !idle {
				err := ctx.Err()
				if err == nil {
					err = errors.New("owner did not become idle")
				}
				record(name, err)
			}
		}
		for _, ingress := range []proxyTestIngressOwner{self.httpIngress, self.socksIngress} {
			if proxyTestOwnerPresent(ingress) {
				ingress.Drain()
			}
		}
		if self.wgCancel != nil {
			self.wgCancel()
		}
		if proxyTestOwnerPresent(self.httpIngress) {
			wait("http ingress", self.httpIngress.WaitIdle(ctx))
		}
		if proxyTestOwnerPresent(self.socksIngress) {
			wait("socks ingress", self.socksIngress.WaitIdle(ctx))
		}
		if self.frontendCancel != nil {
			self.frontendCancel()
		}
		join("proxy http listeners", self.httpListener)
		join("proxy socks listeners", self.socksListener)
		join("tls frontend", self.tlsFrontend)
		join("device rpc frontend", self.deviceRpc)
		join("proxy device manager", self.manager)
		join("provider", &self.provider)
		if proxyTestOwnerPresent(self.networkSpace) {
			self.networkSpace.Close()
		}
		if proxyTestOwnerPresent(self.handler) {
			self.handler.Close()
		}
		if proxyTestOwnerPresent(self.exchange) {
			self.exchange.Close()
		}
		if proxyTestOwnerPresent(self.handler) {
			wait("connect handlers", self.handler.WaitForIdle(ctx))
		}
		if proxyTestOwnerPresent(self.exchange) {
			wait("connect exchange", self.exchange.WaitForIdle(ctx))
		}
		self.cancel()
		join("connect http", self.connectHttp)
		join("platform api http", self.apiHttp)
	})
	return self.err
}
