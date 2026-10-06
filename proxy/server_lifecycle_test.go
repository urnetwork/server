// Forces the actual server-side listener ownership seam, including constructor
// failures, recovery, concurrent bounded cleanup and production run wiring.
package proxy

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	proxycore "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
)

// Uses the real Http/Socks close methods while retaining deterministic listener
// work behind the same production supervisor seam those constructors acquire.
func proxyListenerTestOwner(kind string, ctx context.Context, cancel context.CancelFunc) (proxyTestJoiner, *proxyListenerLifecycle) {
	lifecycle := newProxyListenerLifecycle(ctx, cancel)
	if kind == "http" {
		return &httpServer{ctx: ctx, cancel: cancel, lifecycle: lifecycle}, lifecycle
	}
	return &socks5Server{ctx: ctx, cancel: cancel, lifecycle: lifecycle, socksProxy: proxycore.NewSocksProxy(proxycore.DefaultSocksProxySettings())}, lifecycle
}

// Keeps a real listener owner held after cancellation. Both concurrent bounded
// cleanup calls must report incomplete ownership before a successful retry.
func TestProxyListenerCloseJoinsSupervisorAndWorkers(t *testing.T) {
	for _, kind := range []string{"http", "socks"} {
		func() {
			bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopBound()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			owner, lifecycle := proxyListenerTestOwner(kind, ctx, cancel)
			started, canceled, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			lifecycle.start(func() {
				lifecycle.startListener(func() {
					defer close(workerDone)
					close(started)
					<-ctx.Done()
					close(canceled)
					<-release
				})
				<-started
			})
			defer func() {
				unblock()
				joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := owner.CloseAndWait(joinCtx); err != nil {
					t.Errorf("%s rescue: %v", kind, err)
				}
				for _, done := range []<-chan struct{}{workerDone, lifecycle.done} {
					select {
					case <-done:
					case <-joinCtx.Done():
						t.Errorf("%s owned listener did not join", kind)
					}
				}
			}()
			select {
			case <-canceled:
			case <-bound.Done():
				t.Fatalf("%s supervisor waited before canceling its listener", kind)
			}
			firstCtx, stopFirst := context.WithCancel(bound)
			defer stopFirst()
			secondCtx, stopSecond := context.WithCancel(bound)
			defer stopSecond()
			waitContexts := []*proxyTestWaitContext{
				{Context: firstCtx, entered: make(chan struct{})},
				{Context: secondCtx, entered: make(chan struct{})},
			}
			results := make(chan error, 2)
			var callers sync.WaitGroup
			for _, waitCtx := range waitContexts {
				callers.Go(func() { results <- owner.CloseAndWait(waitCtx) })
			}
			defer func() { stopFirst(); stopSecond(); unblock(); callers.Wait() }()
			for _, waitCtx := range waitContexts {
				select {
				case <-waitCtx.entered:
				case err := <-results:
					t.Errorf("%s cleanup bypassed still-held listener: %v", kind, err)
					return
				case <-bound.Done():
					t.Errorf("%s close did not enter its actual completion wait", kind)
					return
				}
			}
			stopFirst()
			stopSecond()
			for range waitContexts {
				select {
				case err := <-results:
					if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "listener workers did not join") {
						t.Errorf("%s held listener reported joined: %v", kind, err)
					}
				case <-bound.Done():
					t.Errorf("%s canceled caller did not join", kind)
				}
			}
			unblock()
			if err := owner.CloseAndWait(bound); err != nil {
				t.Errorf("%s released retry: %v", kind, err)
			}
		}()
	}
}

// Exercises harness.close itself with a real supervised listener owner. A
// bounded failure is retained while later owners are still attempted; restoring
// the old admission-only harness close must fail before the worker is released.
func TestProxyHarnessJoinsSupervisedListeners(t *testing.T) {
	bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopBound()
	harness := newProxyTestHarness(t, func(*proxyTestHarness) {})
	defer harness.cancel()
	ctx, cancel := harness.newFrontendContext()
	defer cancel()
	owner, lifecycle := proxyListenerTestOwner("http", ctx, cancel)
	started, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	lifecycle.start(func() {
		lifecycle.startListener(func() { defer close(workerDone); close(started); <-release })
		<-ctx.Done()
	})
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		for _, done := range []<-chan struct{}{workerDone, lifecycle.done} {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("harness listener worker did not join")
			}
		}
	}()
	select {
	case <-started:
	case <-bound.Done():
		t.Fatal("listener worker did not start")
	}
	joinEntered := make(chan struct{})
	var stopJoin context.CancelFunc
	harness.lifecycle.httpListener = proxyTestJoinFunc(func(ctx context.Context) error {
		joinCtx, stop := context.WithCancel(ctx)
		stopJoin = stop
		defer stop()
		return owner.CloseAndWait(&proxyTestWaitContext{Context: joinCtx, entered: joinEntered})
	})
	managerJoined := false
	harness.lifecycle.manager = proxyTestJoinFunc(func(context.Context) error { managerJoined = true; return nil })
	tb := &proxyTestErrorTb{TB: t}
	closed := make(chan struct{})
	go func() { defer close(closed); harness.close(tb) }()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-closed:
		case <-joinCtx.Done():
			t.Error("harness listener cleanup caller did not join")
		}
	}()
	select {
	case <-joinEntered:
	case <-closed:
		t.Fatal("actual harness skipped supervised listener ownership")
	case <-bound.Done():
		t.Fatal("harness never reached the real listener completion wait")
	}
	stopJoin()
	select {
	case <-closed:
	case <-bound.Done():
		t.Fatal("bounded incomplete listener close did not return")
	}
	if errorText := tb.errorText(); !strings.Contains(errorText, "proxy http listeners") || !strings.Contains(errorText, "listener workers did not join") {
		t.Errorf("harness erased actual listener join failure: %s", errorText)
	}
	if !managerJoined {
		t.Error("incomplete listener join skipped a later owner")
	}
}

// A supervisor panic or Goexit still cancels and joins already-started workers;
// real startup errors survive successful retries rather than becoming clean.
func TestProxyListenerPartialStartupJoinsAndRetainsFailure(t *testing.T) {
	for _, kind := range []string{"http", "socks"} {
		for _, panicExit := range []bool{false, true} {
			func() {
				bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopBound()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				owner, lifecycle := proxyListenerTestOwner(kind, ctx, cancel)
				started, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				sentinel := errors.New("synthetic listener supervisor setup failure")
				lifecycle.start(func() {
					lifecycle.startListener(func() { defer close(workerDone); close(started); <-ctx.Done(); <-release })
					<-started
					if panicExit {
						panic(sentinel)
					}
					runtime.Goexit()
				})
				defer func() {
					unblock()
					joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					err := owner.CloseAndWait(joinCtx)
					if (panicExit && !errors.Is(err, sentinel)) || (!panicExit && err != nil) {
						t.Errorf("%s partial rescue changed failure: %v", kind, err)
					}
					for _, done := range []<-chan struct{}{workerDone, lifecycle.done} {
						select {
						case <-done:
						case <-joinCtx.Done():
							t.Errorf("%s partial setup worker did not join", kind)
						}
					}
				}()
				select {
				case <-ctx.Done():
				case <-bound.Done():
					t.Fatalf("%s partial setup failed to cancel before joining", kind)
				}
				expired, stop := context.WithCancel(context.Background())
				stop()
				if err := owner.CloseAndWait(expired); !errors.Is(err, context.Canceled) || (panicExit && !errors.Is(err, sentinel)) {
					t.Errorf("%s partial setup forged completion/lost failure: %v", kind, err)
				}
				unblock()
				for i := 0; i < 2; i++ {
					err := owner.CloseAndWait(bound)
					if (panicExit && !errors.Is(err, sentinel)) || (!panicExit && err != nil) {
						t.Errorf("%s retry %d erased startup failure: %v", kind, i, err)
					}
				}
			}()
		}
	}
}

// Real constructors bind a deliberately occupied loopback port. Both workers'
// recovery and any sibling listener finish before repeated close reports error.
func TestProxyListenerConstructorsRetainBindFailure(t *testing.T) {
	for _, kind := range []string{"http", "socks"} {
		func() {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("WARP_HOST_IPV4", "127.0.0.1")
			t.Setenv("WARP_HOST_IPV6", "")
			t.Setenv("WARP_PORTS", fmt.Sprintf("1:%d,2:0", listener.Addr().(*net.TCPAddr).Port))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopBound()
			settings := &ProxySettings{HttpPort: 1, HttpsPort: 2, SocksPort: 1}
			var owner proxyTestJoiner
			var listenerDone <-chan struct{}
			var waitStats func(context.Context) error
			if kind == "http" {
				frontend := NewHttpServer(ctx, cancel, nil, &server.TransportTls{}, settings)
				owner, listenerDone = frontend, frontend.lifecycle.done
			} else {
				frontend := NewSocks5Server(ctx, cancel, nil, &server.TransportTls{}, settings)
				owner, listenerDone, waitStats = frontend, frontend.lifecycle.done, frontend.socksProxy.WaitStats
			}
			defer func() {
				joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := owner.CloseAndWait(joinCtx); !errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("%s constructor rescue did not retain/join failure: %v", kind, err)
				}
				select {
				case <-listenerDone:
					if waitStats != nil {
						if err := waitStats(joinCtx); err != nil {
							t.Errorf("%s constructor stats did not join: %v", kind, err)
						}
					}
				case <-joinCtx.Done():
					t.Errorf("%s constructor supervisor did not join", kind)
				}
			}()
			select {
			case <-ctx.Done():
			case <-bound.Done():
				t.Fatalf("%s actual bind failure did not cancel startup", kind)
			}
			for i := 0; i < 2; i++ {
				if err := owner.CloseAndWait(bound); !errors.Is(err, syscall.EADDRINUSE) {
					t.Errorf("%s actual bind error lost on close %d: %v", kind, i, err)
				}
			}
		}()
	}
}

// Pins both real constructors and every address-family worker to the tested
// supervisor seam rather than qualifying an unused helper.
//
//go:embed server.go
var proxyListenerSource string

// Direct untracked goroutine launches cannot silently bypass lifecycle joins.
func TestProxyListenerProductionRunsUseOwnedWorkers(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "server.go", proxyListenerSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := function.Name.Name
		want := 1
		target := "start"
		if name == "run" && function.Recv != nil {
			pointer, ok := function.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			receiver, ok := pointer.X.(*ast.Ident)
			if !ok || (receiver.Name != "httpServer" && receiver.Name != "socks5Server") {
				continue
			}
			name = receiver.Name + ".run"
			target, want = "startListener", 2
			if receiver.Name == "httpServer" {
				want = 4
			}
		} else if name != "NewHttpServer" && name != "NewSocks5Server" {
			continue
		}
		count := 0
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if _, ok := node.(*ast.GoStmt); ok {
				t.Errorf("%s launches a goroutine outside its listener supervisor", name)
			}
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == target {
					count++
				}
			}
			return true
		})
		if count != want {
			t.Errorf("%s owned launches = %d, want %d", name, count, want)
		}
		seen[name]++
	}
	for _, name := range []string{"NewHttpServer", "NewSocks5Server", "httpServer.run", "socks5Server.run"} {
		if seen[name] != 1 {
			t.Errorf("actual production wiring %s was not checked exactly once", name)
		}
	}
}

// The recovered error predicate must preserve every non-cancellation cause,
// even when teardown canceled the parent before the listener reports failure.
func TestProxyListenerRetainsMixedCancellationFailure(t *testing.T) {
	for _, worker := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			func() {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				owner, lifecycle := proxyListenerTestOwner("http", ctx, cancel)
				sentinel := errors.New("synthetic mixed listener failure")
				fail := func() {
					cancel()
					if mixed {
						panic(errors.Join(context.Canceled, sentinel))
					}
					panic(context.Canceled)
				}
				lifecycle.start(func() {
					if worker {
						lifecycle.startListener(fail)
						<-ctx.Done()
					} else {
						fail()
					}
				})
				defer func() {
					joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					_ = owner.CloseAndWait(joinCtx)
					select {
					case <-lifecycle.done:
					case <-joinCtx.Done():
						t.Error("mixed-error supervisor did not join")
					}
				}()
				bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopBound()
				for retry := 0; retry < 2; retry++ {
					err := owner.CloseAndWait(bound)
					if mixed {
						if !errors.Is(err, sentinel) || !errors.Is(err, context.Canceled) {
							t.Errorf("worker=%t retry=%d discarded a real joined cause: %v", worker, retry, err)
						}
					} else if err != nil {
						t.Errorf("worker=%t retry=%d misclassified pure cancellation: %v", worker, retry, err)
					}
				}
			}()
		}
	}
}
