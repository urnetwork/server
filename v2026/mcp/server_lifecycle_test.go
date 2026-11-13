// Forces the actual mcp server/client test helpers through bounded teardown and
// partial-construction exits without invoking tokens, models or shared fixtures.
package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Records expected helper failures while retaining the actual testing contract.
type mcpLifecycleErrorTb struct {
	testing.TB
	stateLock sync.Mutex
	errors    []string
}

// Observes evaluation of the real bounded wait without changing cancellation.
type mcpLifecycleWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

// Publishes the caller's actual context wait, not just entry to a wrapper.
func (self *mcpLifecycleWaitContext) Done() <-chan struct{} {
	self.once.Do(func() { close(self.entered) })
	return self.Context.Done()
}

// A session-once guard must not suppress the independently retryable server
// join after an earlier bounded failure. The handler stays held through the
// second cleanup result, then a final retry must join it successfully.
func TestMcpFetchClientCleanupRetriesServerAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	retryCtx, cancelRetry := context.WithCancel(ctx)
	defer cancelRetry()
	waitCtx := &mcpLifecycleWaitContext{Context: retryCtx, entered: make(chan struct{})}
	entered, release, handlerDone, clientDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var calls atomic.Int32
	var address string
	var directCleanup func()
	tb := &mcpLifecycleErrorTb{TB: t}
	_, cleanup := connectFetchTestClient(tb, ctx, &fetchTestStack{}, fetchTestClientOptions{
		server: mcpTestServerOptions{
			createHandler: func(context.Context) http.Handler {
				return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { defer close(handlerDone); close(entered); <-release })
			},
			closeContext: func() (context.Context, context.CancelFunc) {
				switch calls.Add(1) {
				case 1:
					expired, stop := context.WithCancel(context.Background())
					stop()
					return expired, stop
				case 2:
					return waitCtx, cancelRetry
				default:
					return context.WithTimeout(context.Background(), 5*time.Second)
				}
			},
		},
		connect: func(url string, closeServer func()) *mcpsdk.ClientSession {
			address, directCleanup = url, closeServer
			return nil
		},
	})
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		response, _ := (&http.Client{Transport: transport}).Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	defer func() {
		unblock()
		cancelRetry()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		directCleanup()
		for _, done := range []<-chan struct{}{handlerDone, clientDone} {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("mcp retry test child did not join")
			}
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("mcp retry handler did not enter")
	}
	cleanup()
	if tb.errorCount() != 1 {
		t.Errorf("first incomplete join errors = %d", tb.errorCount())
	}
	retried := make(chan struct{})
	go func() { defer close(retried); cleanup() }()
	defer func() {
		cancelRetry()
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-retried:
		case <-joinCtx.Done():
			t.Error("retry caller did not join")
		}
	}()
	select {
	case <-waitCtx.entered:
	case <-retried:
		t.Error("actual client cleanup skipped the still-owned server retry")
		return
	case <-ctx.Done():
		t.Error("actual server retry did not reach its wait")
		return
	}
	cancelRetry()
	select {
	case <-retried:
	case <-ctx.Done():
		t.Fatal("canceled retry did not finish")
	}
	if tb.errorCount() != 2 {
		t.Errorf("held handler retry was not reported incomplete: errors=%d", tb.errorCount())
	}
	unblock()
	cleanup()
	if calls.Load() != 3 || tb.errorCount() != 2 {
		t.Errorf("released server retry skipped or failed: calls=%d errors=%d", calls.Load(), tb.errorCount())
	}
}

// Captures explicit incomplete-join reports from the real cleanup helper.
func (self *mcpLifecycleErrorTb) Errorf(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errors = append(self.errors, fmt.Sprintf(format, args...))
}

// Counts retained reports after the cleanup caller has returned.
func (self *mcpLifecycleErrorTb) errorCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.errors)
}

// Captures retained error text without racing concurrent cleanup reporters.
func (self *mcpLifecycleErrorTb) errorText() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.errors, "\n")
}

// Observes real listener closure during an in-frame rollback.
type mcpLifecycleListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

// Records closure only after the real listener has been retired.
func (self *mcpLifecycleListener) Close() error {
	err := self.Listener.Close()
	self.once.Do(func() { close(self.closed) })
	return err
}

// An already-expired deadline must report the still-held handler as a failure;
// cancellation/socket closure does not replace its independent ownership join.
func TestMcpTestServerReportsUnjoinedHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, handlerDone, clientDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	tb := &mcpLifecycleErrorTb{TB: t}
	firstClose := true
	var routeCtx context.Context
	address, cleanup := startTestServer(tb, mcpTestServerOptions{
		createHandler: func(ctx context.Context) http.Handler {
			routeCtx = ctx
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { defer close(handlerDone); close(entered); <-release })
		},
		closeContext: func() (context.Context, context.CancelFunc) {
			if firstClose {
				firstClose = false
				expired, stop := context.WithCancel(context.Background())
				stop()
				return expired, stop
			}
			return context.WithTimeout(context.Background(), 5*time.Second)
		},
	})
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		response, _ := (&http.Client{Transport: transport}).Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		cleanup()
		for _, done := range []<-chan struct{}{handlerDone, clientDone} {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("mcp synthetic child did not join")
			}
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("mcp handler did not enter")
	}
	cleanup()
	if tb.errorCount() != 1 {
		t.Errorf("failed join produced %d explicit errors, want 1", tb.errorCount())
	}
	if routeCtx.Err() == nil {
		t.Error("failed cleanup did not cancel route context")
	}
	select {
	case <-handlerDone:
		t.Fatal("held handler unexpectedly completed")
	default:
	}
	unblock()
	cleanup()
	if tb.errorCount() != 1 {
		t.Errorf("released retry still failed: %d errors", tb.errorCount())
	}
}

// Both route-construction and client-construction failures unwind their acquired
// resources before the enclosing scope resumes. Failures use per-fixture options.
func TestMcpPartialConstructionReleasesServerBeforeScopeExit(t *testing.T) {
	for _, phase := range []string{"route assembly", "client connection"} {
		for _, panicExit := range []bool{false, true} {
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				observed := &mcpLifecycleListener{Listener: listener, closed: make(chan struct{})}
				defer observed.Close()
				var routeCtx context.Context
				var observedPanic any
				var cleanupServer func()
				defer func() {
					if cleanupServer != nil {
						cleanupServer()
					}
				}()
				stopSetup := func() {
					if panicExit {
						panic("synthetic mcp construction")
					}
					runtime.Goexit()
				}
				options := fetchTestClientOptions{
					server: mcpTestServerOptions{
						listen: func() (net.Listener, error) { return observed, nil },
						createHandler: func(ctx context.Context) http.Handler {
							routeCtx = ctx
							if phase == "route assembly" {
								stopSetup()
							}
							return http.NotFoundHandler()
						},
					},
					connect: func(_ string, cleanup func()) *mcpsdk.ClientSession { cleanupServer = cleanup; stopSetup(); return nil },
				}
				scopeDone := make(chan struct{})
				go func() {
					defer close(scopeDone)
					defer func() { observedPanic = recover() }()
					connectFetchTestClient(t, ctx, &fetchTestStack{}, options)
				}()
				defer func() {
					joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					select {
					case <-scopeDone:
					case <-joinCtx.Done():
						t.Errorf("%s construction owner did not join", phase)
					}
				}()
				select {
				case <-scopeDone:
				case <-ctx.Done():
					t.Fatalf("%s construction did not exit", phase)
				}
				if routeCtx == nil || routeCtx.Err() == nil {
					t.Errorf("%s panic=%t escaped without canceling its route owner", phase, panicExit)
				}
				select {
				case <-observed.closed:
				default:
					t.Errorf("%s panic=%t escaped without closing its acquired listener", phase, panicExit)
				}
				if panicExit && observedPanic != "synthetic mcp construction" {
					t.Errorf("panic changed: %v", observedPanic)
				}
			}()
		}
	}
}

// Two cleanup callers share actual server ownership and cannot finish while the
// admitted request still retains the scope's resources.
func TestMcpTestServerConcurrentCleanupJoinsHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, handlerDone, clientDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	waitContexts := []*mcpLifecycleWaitContext{
		{Context: firstCtx, entered: make(chan struct{})},
		{Context: secondCtx, entered: make(chan struct{})},
	}
	var calls atomic.Int32
	tb := &mcpLifecycleErrorTb{TB: t}
	address, cleanup := startTestServer(tb, mcpTestServerOptions{
		createHandler: func(context.Context) http.Handler {
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { defer close(handlerDone); close(entered); <-release })
		},
		closeContext: func() (context.Context, context.CancelFunc) {
			switch calls.Add(1) {
			case 1:
				return waitContexts[0], stopFirst
			case 2:
				return waitContexts[1], stopSecond
			default:
				return context.WithTimeout(context.Background(), 5*time.Second)
			}
		},
	})
	defer func() { unblock(); stopFirst(); stopSecond(); cleanup() }()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, strings.NewReader(""))
		response, _ := (&http.Client{Transport: transport}).Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	defer func() {
		unblock()
		cleanup()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, done := range []<-chan struct{}{handlerDone, clientDone} {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("mcp client or handler did not join")
			}
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("mcp handler did not enter")
	}
	closed := make(chan struct{}, 2)
	var callers sync.WaitGroup
	for i := 0; i < 2; i++ {
		callers.Go(func() { cleanup(); closed <- struct{}{} })
	}
	defer func() { stopFirst(); stopSecond(); unblock(); callers.Wait() }()
	for _, waitCtx := range waitContexts {
		select {
		case <-waitCtx.entered:
		case <-closed:
			t.Fatal("concurrent cleanup bypassed its held handler")
		case <-ctx.Done():
			t.Fatal("both cleanup calls did not enter their real context wait")
		}
	}
	stopFirst()
	stopSecond()
	for i := 0; i < 2; i++ {
		select {
		case <-closed:
		case <-ctx.Done():
			t.Error("concurrent mcp cleanup did not join")
		}
	}
	if tb.errorCount() != 2 || strings.Count(tb.errorText(), "http handlers did not join") != 2 {
		t.Errorf("concurrent held-handler failures lost: %s", tb.errorText())
	}
	unblock()
	cleanup()
	if tb.errorCount() != 2 {
		t.Errorf("released cleanup failed: %s", tb.errorText())
	}
}
