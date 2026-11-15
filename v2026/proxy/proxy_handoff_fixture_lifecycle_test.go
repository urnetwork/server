// Owns only the proxy acceptance fixture's deployment worker and synthetic
// encryption resource; production startup keeps its existing error boundary.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Supplies no deployment secret, and restores the prior cached key before
// removing its resource override. The caller joins its users before restoring.
func pushProxyTestHandoffKey() func() {
	pop := server.Vault.PushSimpleResource("wireguard.yml", []byte(
		"handoff_encryption_key: synthetic-proxy-handoff-fixture-key\n",
	))
	restore := server.Testing_ReloadWgHandoffKey()
	return func() {
		restore()
		pop()
	}
}

// The result becomes readable only after done closes. Cancellation belongs to
// this worker, not to the shared fixture manager or platform context.
type proxyTestDeploymentHandoff struct {
	cancel     context.CancelFunc
	done       chan struct{}
	readyCount int
	raised     any
}

// Mirrors main's outer error scope, retaining its raise for the test owner
// rather than letting an unjoined background panic replace a foreground error.
func startProxyTestDeploymentHandoff(
	parent context.Context,
	complete func(context.Context) int,
) *proxyTestDeploymentHandoff {
	ctx, cancel := context.WithCancel(parent)
	worker := &proxyTestDeploymentHandoff{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(worker.done)
		worker.raised = server.HandleError(func() {
			worker.readyCount = complete(ctx)
		})
	}()
	return worker
}

// Joins before reading result state. Only an all-shutdown raise is benign;
// joining cancellation with a physical or unrelated error still fails.
func (self *proxyTestDeploymentHandoff) CloseAndWait(ctx context.Context) error {
	self.cancel()
	select {
	case <-self.done:
		if self.raised == nil || server.IsDoneError(self.raised) {
			return nil
		}
		if err, ok := self.raised.(error); ok {
			return fmt.Errorf("deployment handoff worker: %w", err)
		}
		return fmt.Errorf("deployment handoff worker: %v", self.raised)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Runs on every test exit, before the fixture's manager or database teardown.
func closeProxyTestDeploymentHandoff(t testing.TB, worker *proxyTestDeploymentHandoff) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := worker.CloseAndWait(ctx); err != nil {
		t.Errorf("deployment handoff worker did not finish cleanly: %v", err)
	}
}

// A key override cannot silently reuse another fixture's cached secret. The
// outer resource is synthetic too, so this control never reads a live vault.
func TestProxyDeploymentHandoffSyntheticKeyIsScoped(t *testing.T) {
	pop := server.Vault.PushSimpleResource("wireguard.yml", []byte(
		"handoff_encryption_key: synthetic-proxy-handoff-outer-key\n",
	))
	defer pop()
	restore := server.Testing_ReloadWgHandoffKey()
	defer restore()
	plain := []byte("synthetic proxy deployment handoff")
	outer, err := server.WgHandoffSeal(plain)
	if err != nil {
		t.Fatal(err)
	}

	var inner string
	func() {
		restoreInner := pushProxyTestHandoffKey()
		defer restoreInner()
		inner, err = server.WgHandoffSeal(plain)
		if err != nil {
			t.Fatal(err)
		}
		opened, sealed, openErr := server.WgHandoffOpen(inner)
		if openErr != nil || !sealed || string(opened) != string(plain) {
			t.Fatalf("synthetic handoff key did not round trip: %v", openErr)
		}
	}()

	if _, sealed, err := server.WgHandoffOpen(inner); !sealed || err == nil {
		t.Fatal("PROXY_HANDOFF_SYNTHETIC_KEY_NOT_SCOPED: inner handoff used the outer key")
	}
	opened, sealed, err := server.WgHandoffOpen(outer)
	if err != nil || !sealed || string(opened) != string(plain) {
		t.Fatalf("outer synthetic handoff key was not restored: %v", err)
	}
}

// The barrier holds the real completion call until foreground unwind cancels
// its worker. Its first Redis admission then raises before opening a client.
func TestProxyDeploymentHandoffPreservesForegroundOnCanceledPrewarm(t *testing.T) {
	t.Setenv("WARP_HOST", "proxy-handoff-cancel.fixture.example")
	t.Setenv("WARP_BLOCK", "fixture")
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	managerSettings := DefaultProxyDeviceManagerSettings()
	managerSettings.HoldWindowIdentityRestore = true
	manager := NewProxyDeviceManager(parent, managerSettings)
	defer closeProxyDeviceManager(t, manager)
	settings := DefaultProxySettings()
	settings.EnableWgHandoff = false
	wg := &wgServer{
		proxyDeviceManager: manager,
		settings:           settings,
	}
	entered := make(chan struct{})
	worker := startProxyTestDeploymentHandoff(parent, func(ctx context.Context) int {
		close(entered)
		<-ctx.Done()
		return wg.CompleteDeploymentHandoff(ctx)
	})
	defer closeProxyTestDeploymentHandoff(t, worker)
	watchCtx, stopWatch := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopWatch()
	select {
	case <-entered:
	case <-watchCtx.Done():
		t.Fatal("deployment worker did not reach the cancellation barrier")
	}

	foreground := errors.New("synthetic foreground handoff failure")
	var observed any
	func() {
		defer func() { observed = recover() }()
		defer closeProxyTestDeploymentHandoff(t, worker)
		panic(foreground)
	}()
	if observed != foreground {
		t.Fatalf("foreground failure was replaced: %v", observed)
	}
	select {
	case <-worker.done:
	default:
		t.Fatal("foreground unwind returned before deployment worker joined")
	}
	raised, ok := worker.raised.(error)
	if !ok || !errors.Is(raised, context.Canceled) || !errors.Is(raised, server.DbContextDoneError) {
		t.Fatalf("real canceled prewarm did not reach its Redis admission: %v", worker.raised)
	}
	if parent.Err() != nil || !manager.windowIdentityGate.restorationAllowed() {
		t.Fatal("worker cancellation closed its shared parent or skipped identity release")
	}
}

// The owner retains a non-shutdown cause even when cancellation is also in its
// cause graph. One deliberate synthetic unexpected-error log is part of this test.
func TestProxyDeploymentHandoffReportsUnexpectedWorkerFailure(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	unexpected := errors.New("synthetic unexpected deployment handoff failure")
	entered := make(chan struct{})
	worker := startProxyTestDeploymentHandoff(parent, func(ctx context.Context) int {
		close(entered)
		<-ctx.Done()
		panic(errors.Join(context.Canceled, unexpected))
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := worker.CloseAndWait(ctx); !errors.Is(err, unexpected) {
			t.Errorf("unexpected worker cause was lost during final join: %v", err)
		}
	}()
	watchCtx, stopWatch := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopWatch()
	select {
	case <-entered:
	case <-watchCtx.Done():
		t.Fatal("deployment worker did not reach the unexpected-failure barrier")
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	err := worker.CloseAndWait(ctx)
	if !errors.Is(err, unexpected) || !errors.Is(err, context.Canceled) || server.IsDoneError(worker.raised) {
		t.Fatalf("unexpected worker failure was suppressed as cancellation: %v", err)
	}
	if parent.Err() != nil {
		t.Fatal("failed deployment worker canceled its shared parent")
	}
}
