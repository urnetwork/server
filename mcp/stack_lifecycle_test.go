// Joins the fetch fixture's resource consumers and forces its teardown barriers
// in isolated regressions that do not need PostgreSQL or Redis.
package mcp

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// These are the existing ownership surfaces, not cancellation substitutes.
type fetchTestJoiner interface {
	CloseAndWait(context.Context) error
}

// Releases an owner whose existing close method already joins its workers.
type fetchTestCloser interface {
	Close()
}

// Stops connection admission separately from the final cleanup join.
type fetchTestConnectOwner interface {
	Close()
	WaitForIdle(context.Context) bool
}

// Refuses new proxy requests before waiting for admitted callers.
type fetchTestIngressOwner interface {
	Drain()
	WaitIdle(context.Context) bool
}

// Retains every database/Redis consumer until the enclosing TestEnv can pop.
// A partially built stack uses the same owner; nil entries were never acquired.
// Concurrent close calls share one ordered join after construction finishes.
type fetchTestStackLifecycle struct {
	frontendCancel   context.CancelFunc
	frontendListener fetchTestJoiner
	ingress          fetchTestIngressOwner
	manager          fetchTestJoiner
	provider         fetchTestProviderLifecycle
	networkSpace     fetchTestCloser
	handler          fetchTestConnectOwner
	exchange         fetchTestConnectOwner
	connectHttp      fetchTestJoiner
	apiHttp          fetchTestJoiner
	web              fetchTestCloser
	cancel           context.CancelFunc
	once             sync.Once
	err              error
}

// Preserves the producer-before-control ordering of final contract reports.
type fetchTestProviderLifecycle struct {
	remoteNat fetchTestCloser
	transport fetchTestJoiner
	localNat  fetchTestJoiner
	client    fetchTestJoiner
	oob       fetchTestJoiner
	strategy  fetchTestCloser
}

// Keeps both control paths available while clients hand off their final close
// reports. A failure forces cancellation but does not skip later owners or turn
// an expired wait into a clean join. One caller-owned deadline covers all joins.
func (self *fetchTestStackLifecycle) close(ctx context.Context) error {
	self.once.Do(func() {
		record := func(name string, err error) {
			if err != nil {
				self.err = errors.Join(self.err, fmt.Errorf("%s: %w", name, err))
				self.cancel()
			}
		}
		join := func(name string, owner fetchTestJoiner) {
			if owner != nil {
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

		if self.ingress != nil {
			self.ingress.Drain()
			wait("proxy ingress", self.ingress.WaitIdle(ctx))
		}
		if self.frontendCancel != nil {
			self.frontendCancel()
		}
		join("proxy frontend listeners", self.frontendListener)
		// Manager joins DeviceLocal, its local out-of-band callbacks, and Authority's
		// notification publisher before returning its borrowed NetworkSpace.
		join("proxy device manager", self.manager)
		if self.provider.remoteNat != nil {
			self.provider.remoteNat.Close()
		}
		join("provider transport", self.provider.transport)
		join("provider local nat", self.provider.localNat)
		join("provider client", self.provider.client)
		// Client's contract manager can enqueue caller-context out-of-band closes while
		// joining. Close out-of-band admission only after that producer has finished.
		join("provider out-of-band control", self.provider.oob)
		if self.provider.strategy != nil {
			self.provider.strategy.Close()
		}
		if self.networkSpace != nil {
			self.networkSpace.Close()
		}

		if self.handler != nil {
			self.handler.Close()
		}
		if self.exchange != nil {
			self.exchange.Close()
		}
		if self.handler != nil {
			wait("connect handlers", self.handler.WaitForIdle(ctx))
		}
		if self.exchange != nil {
			wait("connect exchange", self.exchange.WaitForIdle(ctx))
		}
		// Exchange joins resident controllers whose contexts deliberately outlive
		// their transport. http joins cover the independent api ingress too.
		if self.connectHttp != nil {
			record("connect http", self.connectHttp.CloseAndWait(ctx))
		}
		if self.apiHttp != nil {
			record("api http", self.apiHttp.CloseAndWait(ctx))
		}
		self.cancel()
		if self.web != nil {
			self.web.Close()
		}
	})
	return self.err
}

// Adapts a deterministic barrier to the existing context-aware owner surface.
type fetchTestJoinFunc func(context.Context) error

// Forwards the common teardown context without replacing it.
func (self fetchTestJoinFunc) CloseAndWait(ctx context.Context) error { return self(ctx) }

// Adapts a synchronous release action to the existing close surface.
type fetchTestCloseFunc func()

// Runs the release action in the teardown owner's goroutine.
func (self fetchTestCloseFunc) Close() { self() }

// Separates proxy admission closure from its active-request barrier.
type fetchTestIngressFuncs struct {
	drain func()
	wait  func(context.Context) bool
}

// Closes ingress admission through its recording action.
func (self fetchTestIngressFuncs) Drain() { self.drain() }

// Joins the gated request owner with the caller's context.
func (self fetchTestIngressFuncs) WaitIdle(ctx context.Context) bool { return self.wait(ctx) }

// Separates connection cancellation from deferred database/Redis cleanup.
type fetchTestConnectFuncs struct {
	close func()
	wait  func(context.Context) bool
}

// Records connection admission closure.
func (self fetchTestConnectFuncs) Close() { self.close() }

// Joins the gated cleanup owner with the caller's context.
func (self fetchTestConnectFuncs) WaitForIdle(ctx context.Context) bool { return self.wait(ctx) }

// Captures the real constructor's fatal startup check in a scoped pure test.
type fetchTestStartupTb struct{ testing.TB }

// Converts the deliberate failed-listener guard into local failure evidence.
func (self *fetchTestStartupTb) Fatalf(string, ...any) { panic("synthetic fetch startup fatal") }

// Actual stack acquisition keeps a failed frontend independent of the platform
// but still refuses to return a successfully constructed, dead ingress.
func TestFetchStackFrontendKeepsPlatformContext(t *testing.T) {
	stack := newFetchTestStack(&fetchTestStartupTb{TB: t}, func(*fetchTestStack) {})
	defer stack.close()
	frontendCtx, cancelFrontend := stack.newFrontendContext()
	defer cancelFrontend()
	cancelFrontend()
	if frontendCtx.Err() == nil || stack.ctx.Err() != nil {
		t.Error("fetch frontend cancellation escaped into platform controls")
	}
	var observedPanic any
	func() {
		defer func() { observedPanic = recover() }()
		stack.requireFrontendStarted(frontendCtx)
	}()
	if observedPanic != "synthetic fetch startup fatal" {
		t.Errorf("fetch listener failure was not a startup failure: %v", observedPanic)
	}
	var events []string
	stack.lifecycle.manager = fetchTestJoinFunc(func(context.Context) error {
		if stack.ctx.Err() != nil {
			t.Error("fetch manager lost platform context during rollback")
		}
		events = append(events, "manager")
		return nil
	})
	stack.lifecycle.provider.client = fetchTestJoinFunc(func(context.Context) error { events = append(events, "client"); return nil })
	stack.lifecycle.provider.oob = fetchTestJoinFunc(func(context.Context) error {
		if stack.ctx.Err() != nil || !reflect.DeepEqual(events, []string{"manager", "client"}) {
			t.Error("fetch out-of-band cleanup lost live controls or client-before-control ordering")
		}
		events = append(events, "out-of-band")
		return nil
	})
	stack.close()
	if !reflect.DeepEqual(events, []string{"manager", "client", "out-of-band"}) || stack.ctx.Err() == nil {
		t.Errorf("fetch rollback did not complete: %v", events)
	}
}

// The actual stack.close path must enter and report a listener's bounded join
// while that owner remains held, rather than treating Drain/WaitIdle as complete.
func TestFetchStackLifecycleJoinsFrontendListener(t *testing.T) {
	bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopBound()
	tb := &mcpLifecycleErrorTb{TB: t}
	stack := newFetchTestStack(tb, func(*fetchTestStack) {})
	defer stack.cancel()
	frontendCtx, cancelFrontend := stack.newFrontendContext()
	defer cancelFrontend()
	release, workerDone, joinEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	go func() { defer close(workerDone); <-release }()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-workerDone:
		case <-joinCtx.Done():
			t.Error("fetch listener test worker did not join")
		}
	}()
	var stopJoin context.CancelFunc
	stack.lifecycle.frontendListener = fetchTestJoinFunc(func(ctx context.Context) error {
		if frontendCtx.Err() == nil || stack.ctx.Err() != nil {
			t.Error("listener join did not follow child-only cancellation")
		}
		joinCtx, stop := context.WithCancel(ctx)
		stopJoin = stop
		defer stop()
		waitCtx := &mcpLifecycleWaitContext{Context: joinCtx, entered: joinEntered}
		select {
		case <-workerDone:
			return nil
		case <-waitCtx.Done():
			return fmt.Errorf("held frontend listener did not join: %w", waitCtx.Err())
		}
	})
	managerJoined := false
	stack.lifecycle.manager = fetchTestJoinFunc(func(context.Context) error { managerJoined = true; return nil })
	closed := make(chan struct{})
	go func() { defer close(closed); stack.close() }()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-closed:
		case <-joinCtx.Done():
			t.Error("fetch listener cleanup caller did not join")
		}
	}()
	select {
	case <-joinEntered:
	case <-closed:
		t.Fatal("actual stack.close skipped frontend listener ownership")
	case <-bound.Done():
		t.Fatal("stack.close did not reach the listener wait")
	}
	stopJoin()
	select {
	case <-closed:
	case <-bound.Done():
		t.Fatal("bounded fetch listener failure did not return")
	}
	if errorText := tb.errorText(); !strings.Contains(errorText, "proxy frontend listeners") || !strings.Contains(errorText, context.Canceled.Error()) {
		t.Errorf("actual stack.close erased incomplete listener ownership: %s", errorText)
	}
	if !managerJoined {
		t.Error("incomplete listener skipped a later owner")
	}
}

// Pins the actual fixture constructor to the same context and join ownership
// used in these pure regressions, without evaluating model or token setup.
//
//go:embed stack_test.go
var fetchTestSetupSource string

// A test-only scoped helper must not pass while actual setup bypasses its owner.
func TestFetchStackFrontendSetupUsesOwnedContext(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "stack_test.go", fetchTestSetupSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	constructors, listenerAssignments, startupChecks := 0, 0, 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "setupFetchTestStackWithOptions" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if assignment, ok := node.(*ast.AssignStmt); ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 {
				if selector, ok := assignment.Lhs[0].(*ast.SelectorExpr); ok && selector.Sel.Name == "frontendListener" {
					listenerAssignments++
					if value, ok := assignment.Rhs[0].(*ast.Ident); !ok || value.Name != "ingress" {
						t.Error("fetch setup did not retain the actual frontend owner")
					}
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if target, ok := call.Fun.(*ast.SelectorExpr); ok {
				if target.Sel.Name == "NewHttpServer" {
					constructors++
					if len(call.Args) < 2 {
						t.Error("fetch ingress omitted context ownership")
						return true
					}
					for i, want := range []string{"frontendCtx", "frontendCancel"} {
						if argument, ok := call.Args[i].(*ast.Ident); !ok || argument.Name != want {
							t.Errorf("fetch ingress argument %d bypassed %s", i, want)
						}
					}
				} else if target.Sel.Name == "requireFrontendStarted" {
					startupChecks++
				}
			}
			return true
		})
	}
	if constructors != 1 || listenerAssignments != 1 || startupChecks != 1 {
		t.Errorf("actual fetch constructor/listener/startup wiring = %d/%d/%d", constructors, listenerAssignments, startupChecks)
	}
}

// Each barrier stands for a real resource consumer. Drive stack.close itself:
// reverting its wiring to the old handler-only cleanup must fail this test too.
func TestFetchStackLifecycleJoinsOwnersBeforeResourceRelease(t *testing.T) {
	for _, blocked := range []string{"ingress idle", "frontend listener", "manager", "transport", "local nat", "client", "out-of-band", "handler idle", "exchange idle"} {
		func() {
			root, cancel := context.WithCancel(context.Background())
			defer cancel()
			bound, boundCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer boundCancel()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			secondDone := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer func() {
				unblock()
				joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				select {
				case <-done:
				case <-joinCtx.Done():
					t.Error("stack close worker did not join")
				}
			}()
			var events []string
			var commonCtx context.Context
			record := func(name string) { events = append(events, name) }
			join := func(name string) fetchTestJoinFunc {
				return func(ctx context.Context) error {
					if commonCtx == nil {
						commonCtx = ctx
					} else if commonCtx != ctx {
						t.Error("teardown did not use one common deadline")
					}
					record(name)
					if name == blocked {
						close(entered)
						select {
						case <-release:
						case <-bound.Done():
							return bound.Err()
						}
					}
					return nil
				}
			}
			wait := func(name string) func(context.Context) bool {
				return func(ctx context.Context) bool { return join(name)(ctx) == nil }
			}
			stack := &fetchTestStack{t: t, lifecycle: fetchTestStackLifecycle{
				frontendCancel:   func() { record("frontend cancel") },
				frontendListener: join("frontend listener"),
				cancel:           func() { record("cancel"); cancel() },
				ingress:          fetchTestIngressFuncs{drain: func() { record("drain") }, wait: wait("ingress idle")},
				manager:          join("manager"),
				provider: fetchTestProviderLifecycle{
					remoteNat: fetchTestCloseFunc(func() { record("remote nat") }),
					transport: join("transport"), localNat: join("local nat"), client: join("client"), oob: join("out-of-band"),
					strategy: fetchTestCloseFunc(func() { record("strategy") }),
				},
				networkSpace: fetchTestCloseFunc(func() { record("network space") }),
				handler:      fetchTestConnectFuncs{close: func() { record("handler close") }, wait: wait("handler idle")},
				exchange:     fetchTestConnectFuncs{close: func() { record("exchange close") }, wait: wait("exchange idle")},
				web:          fetchTestCloseFunc(func() { record("web") }),
			}}
			go func() { defer close(done); stack.close() }()
			select {
			case <-entered:
			case <-done:
				t.Errorf("stack cleanup returned without joining %s", blocked)
				return
			case <-bound.Done():
				t.Errorf("stack cleanup did not reach %s", blocked)
				return
			}
			if root.Err() != nil {
				t.Errorf("%s: control servers canceled before final client/contract owners joined", blocked)
				return
			}
			go func() { defer close(secondDone); stack.close() }()
			defer func() {
				unblock()
				joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				select {
				case <-secondDone:
				case <-joinCtx.Done():
					t.Error("concurrent stack close did not join")
				}
			}()
			select {
			case <-done:
				t.Errorf("%s: resource scope could pop while a stack owner remained live", blocked)
				return
			default:
			}
			unblock()
			select {
			case <-done:
			case <-bound.Done():
				t.Errorf("%s: stack cleanup did not finish after owner release", blocked)
				return
			}
			select {
			case <-secondDone:
			case <-bound.Done():
				t.Errorf("%s: concurrent cleanup did not finish after owner release", blocked)
				return
			}
			stack.close()
			want := []string{"drain", "ingress idle", "frontend cancel", "frontend listener", "manager", "remote nat", "transport", "local nat", "client", "out-of-band", "strategy", "network space", "handler close", "exchange close", "handler idle", "exchange idle", "cancel", "web"}
			if !reflect.DeepEqual(events, want) {
				t.Errorf("%s: shutdown order/idempotence = %v, want %v", blocked, events, want)
			}
		}()
	}
}

// An incomplete join cancels the root but cannot skip or erase later failures.
func TestFetchStackLifecycleFailureCancelsAndAttemptsRemainingJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("held manager did not join")
	var events []string
	owner := fetchTestStackLifecycle{
		cancel:  cancel,
		manager: fetchTestJoinFunc(func(context.Context) error { return failure }),
		provider: fetchTestProviderLifecycle{
			client: fetchTestJoinFunc(func(joinCtx context.Context) error {
				if ctx.Err() == nil || joinCtx != ctx {
					t.Error("failed join did not force cancellation with the common context")
				}
				events = append(events, "client")
				return joinCtx.Err()
			}),
			oob: fetchTestJoinFunc(func(ctx context.Context) error { events = append(events, "out-of-band"); return ctx.Err() }),
		},
		exchange: fetchTestConnectFuncs{close: func() { events = append(events, "exchange close") }, wait: func(context.Context) bool { events = append(events, "exchange wait"); return false }},
		web:      fetchTestCloseFunc(func() { events = append(events, "web") }),
	}
	err := owner.close(ctx)
	if !errors.Is(err, failure) || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "provider out-of-band control") || !strings.Contains(err.Error(), "connect exchange") {
		t.Fatalf("incomplete joins were not retained: %v", err)
	}
	if want := []string{"client", "out-of-band", "exchange close", "exchange wait", "web"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("failed cleanup skipped owners: %v, want %v", events, want)
	}
	if owner.close(context.Background()) != err {
		t.Fatal("repeated close erased the original incomplete-join error")
	}
}

// Partial construction unwinds inside the resource scope, including Goexit.
func TestFetchStackLifecyclePartialSetupGoexitJoinsBeforeScopeExit(t *testing.T) {
	bound, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, scopeExit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-scopeExit:
		case <-joinCtx.Done():
			t.Error("partial setup worker did not join")
		}
	}()
	go func() {
		defer close(scopeExit) // models TestEnv's enclosing deferred resource pop
		newFetchTestStack(t, func(stack *fetchTestStack) {
			stack.lifecycle.manager = fetchTestJoinFunc(func(context.Context) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-bound.Done():
					return bound.Err()
				}
			})
			runtime.Goexit()
		})
	}()
	select {
	case <-entered:
	case <-scopeExit:
		t.Fatal("Goexit escaped partial stack setup before joining acquired owners")
	case <-bound.Done():
		t.Fatal("partial setup cleanup did not start")
	}
	select {
	case <-scopeExit:
		t.Fatal("TestEnv could restore resources before partial setup cleanup joined")
	default:
	}
	unblock()
}

// Holds a handler after its socket and Serve have both closed. Only explicit
// handler ownership can prevent this late database consumer from escaping teardown.
func TestFetchStackLifecycleJoinsHttpHandlersAfterSocketClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		defer close(handlerDone)
		close(entered)
		<-release
	})}
	owner := server.NewTestHttpServer(ctx, listener, httpServer)
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
	}()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String(), nil)
		response, _ := http.DefaultClient.Do(request)
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
				t.Error("http client or handler did not join")
			}
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("http handler did not start")
	}
	if err := httpServer.Close(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	closeEntered := make(chan struct{})
	var stopJoin context.CancelFunc
	tb := &mcpLifecycleErrorTb{TB: t}
	stack := &fetchTestStack{t: tb, lifecycle: fetchTestStackLifecycle{
		cancel: func() {}, apiHttp: fetchTestJoinFunc(func(ctx context.Context) error {
			joinCtx, stop := context.WithCancel(ctx)
			stopJoin = stop
			defer stop()
			return owner.CloseAndWait(&mcpLifecycleWaitContext{Context: joinCtx, entered: closeEntered})
		}),
	}}
	go func() { defer close(closed); stack.close() }()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-closed:
		case <-joinCtx.Done():
			t.Error("stack close did not join")
		}
	}()
	select {
	case <-closeEntered:
	case <-closed:
		t.Fatal("stack cleanup bypassed api ownership")
	case <-ctx.Done():
		t.Fatal("api cleanup did not close admission")
	}
	stopJoin()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("bounded stack failure did not return while handler remained held")
	}
	if errorText := tb.errorText(); !strings.Contains(errorText, "http handlers did not join") || !strings.Contains(errorText, context.Canceled.Error()) {
		t.Errorf("closed socket/Serve was mistaken for joined handler: %s", errorText)
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Error(err)
	}
}

// Closing sockets after a deadline does not forge a handler join or reopen
// admission; a later successful join requires the held handler to leave.
func TestFetchStackHttpTimeoutRetainsFailureAndRejectsAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}
	owner := server.NewTestHttpServer(ctx, listener, httpServer)
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer close(done)
		httpServer.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-joinCtx.Done():
			t.Error("held http handler did not join")
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler was not admitted")
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	err = owner.CloseAndWait(expired)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http handlers did not join") {
		t.Errorf("timeout/socket close was reported as clean ownership: %v", err)
	}
	response := httptest.NewRecorder()
	httpServer.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("late admission status = %d", response.Code)
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Errorf("released http owner did not join: %v", err)
	}
}
