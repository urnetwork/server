// Exercises per-request cancellation while construction and device lifetime
// remain shared and synchronously joined by the manager.
package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A live hosted owner whose construction and cleanup need no external I/O.
func newContextTestProxyDevice(manager *ProxyDeviceManager, closeDevice func()) *ProxyDevice {
	ctx, cancel := context.WithCancel(manager.ctx)
	device := &ProxyDevice{
		ctx: ctx, cancel: cancel,
		deviceState:             newProxyDeviceReadinessTestSource(true),
		settings:                &ProxyDeviceSettings{ProxyDeviceIdleTimeout: time.Hour},
		runForTest:              func() { <-ctx.Done() },
		closeDeviceLocalForTest: closeDevice,
	}
	device.UpdateActivity()
	return device
}

// A timed-out CONNECT may start shared construction; its healthy sibling can
// still adopt that exact device after setup finishes, without another build.
func TestHttpConnectTimeoutPreservesSharedDeviceForSibling(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		entered, release := make(chan struct{}), make(chan struct{})
		releaseBuild := sync.OnceFunc(func() { close(release) })
		defer releaseBuild()
		var builds, closes atomic.Int32
		device := newContextTestProxyDevice(manager, func() { closes.Add(1) })
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			builds.Add(1)
			close(entered)
			<-release
			return device, nil
		}
		core := newHttpConnectTestProxy(manager)
		core.Settings().ProxyConnectBudget = time.Second
		responseDone, handlerDone := startHttpBudgetTestConnect(t, core, proxyId)
		<-entered
		siblingResult := make(chan *ProxyDevice, 1)
		go func() {
			pd, err := manager.OpenProxyDeviceWithContext(manager.ctx, proxyId)
			if err != nil {
				t.Error(err)
			}
			siblingResult <- pd
		}()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if code := <-responseDone; code != http.StatusGatewayTimeout {
			t.Fatalf("timed-out CONNECT returned %d, want 504", code)
		}
		<-handlerDone
		select {
		case <-siblingResult:
			t.Fatal("one request's timeout completed a healthy sibling's wait")
		default:
		}
		if manager.ctx.Err() != nil || !device.Active() || closes.Load() != 0 {
			t.Fatal("request timeout canceled the shared owner")
		}
		releaseBuild()
		if got := <-siblingResult; got != device || !got.Active() {
			t.Fatal("healthy sibling did not adopt the original device")
		}
		if got, err := manager.OpenProxyDevice(proxyId); err != nil || got != device || builds.Load() != 1 {
			t.Fatalf("later legacy caller lost shared setup: device=%p err=%v builds=%d", got, err, builds.Load())
		}
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if closes.Load() != 1 || manager.DeviceCount() != 0 {
			t.Fatal("shared device was not closed and removed exactly once")
		}
	})
}

// A canceled coalesced waiter leaves its creator and another proxy id live.
func TestProxyDeviceCanceledWaiterPreservesCreatorAndOtherId(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		proxyId, otherId := server.NewId(), server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		releaseBuild := sync.OnceFunc(func() { close(release) })
		defer releaseBuild()
		var builds atomic.Int32
		device := newContextTestProxyDevice(manager, nil)
		otherDevice := newContextTestProxyDevice(manager, nil)
		manager.proxyDeviceBuilder = func(id server.Id) (*ProxyDevice, error) {
			builds.Add(1)
			if id == otherId {
				return otherDevice, nil
			}
			close(entered)
			<-release
			return device, nil
		}
		creatorResult := make(chan *ProxyDevice, 1)
		go func() { pd, _ := manager.OpenProxyDevice(proxyId); creatorResult <- pd }()
		<-entered
		ctx, cancel := context.WithCancel(manager.ctx)
		waiterResult := make(chan error, 1)
		go func() { _, err := manager.OpenProxyDeviceWithContext(ctx, proxyId); waiterResult <- err }()
		synctest.Wait()
		cancel()
		if err := <-waiterResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter returned %v", err)
		}
		if got, err := manager.OpenProxyDeviceWithContext(manager.ctx, otherId); err != nil || got != otherDevice {
			t.Fatalf("blocked setup retained another id: device=%p err=%v", got, err)
		}
		releaseBuild()
		if got := <-creatorResult; got != device || !got.Active() || builds.Load() != 2 {
			t.Fatal("canceled waiter changed its creator's result")
		}
	})
}

// A canceled creator's detached construction still belongs to CloseAndWait,
// including cleanup of a device that finishes after publication is closed.
func TestProxyDeviceCanceledCreatorStillJoinsConstructionAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		proxyId := server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		closeEntered, closeRelease := make(chan struct{}), make(chan struct{})
		releaseBuild := sync.OnceFunc(func() { close(release) })
		releaseClose := sync.OnceFunc(func() { close(closeRelease) })
		defer releaseBuild()
		defer releaseClose()
		var builds, closes atomic.Int32
		device := newContextTestProxyDevice(manager, func() {
			closes.Add(1)
			close(closeEntered)
			<-closeRelease
		})
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			builds.Add(1)
			close(entered)
			<-release
			return device, nil
		}
		ctx, cancel := context.WithCancel(manager.ctx)
		result := make(chan error, 1)
		go func() { _, err := manager.OpenProxyDeviceWithContext(ctx, proxyId); result <- err }()
		<-entered
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled creator returned %v", err)
		}
		closed := make(chan error, 1)
		go func() { closed <- manager.CloseAndWait(context.Background()) }()
		<-manager.ctx.Done()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("manager shutdown abandoned the canceled request's construction")
		default:
		}
		if _, err := manager.OpenProxyDeviceWithContext(context.Background(), proxyId); err == nil || builds.Load() != 1 {
			t.Fatal("shutdown admitted a new construction")
		}
		releaseBuild()
		<-closeEntered
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("manager shutdown abandoned unpublished device cleanup")
		default:
		}
		releaseClose()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if closes.Load() != 1 || manager.DeviceCount() != 0 || device.Active() {
			t.Fatal("canceled construction retained or double-closed its device")
		}
	})
}

// The ordinary HTTP forwarding path shares the callback. Its canceled request
// must release the transport's joined dial owner while setup remains shared.
func TestHttpRequestCancellationStopsDeviceWait(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		entered, release := make(chan struct{}), make(chan struct{})
		releaseBuild := sync.OnceFunc(func() { close(release) })
		defer releaseBuild()
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			close(entered)
			<-release
			return nil, errors.New("synthetic construction failure")
		}
		core := newHttpConnectTestProxy(manager)
		ctx, cancel := context.WithCancel(manager.ctx)
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://target.example/resource", nil)
		request.RemoteAddr = "192.0.2.1:1234"
		request.Header.Set("Proxy-Authorization", httpConnectTestBasic(proxyId))
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { core.ServeHTTP(response, request); close(done) }()
		<-entered
		cancel()
		synctest.Wait()
		select {
		case <-done:
			if response.Code == http.StatusForbidden {
				t.Error("request cancellation became a 403")
			}
		default:
			t.Error("canceled forwarding request retained its setup dial")
		}
		if manager.ctx.Err() != nil {
			t.Error("request cancellation stopped its manager")
		}
		releaseBuild()
		<-done
	})
}
