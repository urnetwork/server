package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// proxyDeviceReadinessTestSub records exact listener teardown.
type proxyDeviceReadinessTestSub struct {
	close func()
}

// Close removes the readiness test listener.
func (self *proxyDeviceReadinessTestSub) Close() {
	self.close()
}

// proxyDeviceReadinessTestSource is a barrier-driven readiness source. It
// exposes subscription and first-read boundaries without clocks or sockets.
type proxyDeviceReadinessTestSource struct {
	mutex          sync.Mutex
	status         sdk.WindowStatus
	listener       sdk.WindowStatusChangeListener
	subscribed     chan struct{}
	read           chan struct{}
	subscribeOnce  sync.Once
	readOnce       sync.Once
	unsubscribed   atomic.Bool
	done           atomic.Bool
	afterSubscribe func()
}

// newProxyDeviceReadinessTestSource creates a source with exact lifecycle
// barriers and the requested initial readiness value.
func newProxyDeviceReadinessTestSource(minSatisfied bool) *proxyDeviceReadinessTestSource {
	return &proxyDeviceReadinessTestSource{
		status:     sdk.WindowStatus{MinSatisfied: minSatisfied},
		subscribed: make(chan struct{}),
		read:       make(chan struct{}),
	}
}

// GetWindowStatus returns an owned snapshot and exposes the first-read edge.
func (self *proxyDeviceReadinessTestSource) GetWindowStatus() *sdk.WindowStatus {
	self.mutex.Lock()
	status := self.status
	self.mutex.Unlock()
	self.readOnce.Do(func() {
		close(self.read)
	})
	return &status
}

// GetDone reports the independently controlled lifecycle state.
func (self *proxyDeviceReadinessTestSource) GetDone() bool {
	return self.done.Load()
}

// AddWindowStatusChangeListener installs the callback before exposing the
// subscription edge, matching the production subscribe-before-read contract.
func (self *proxyDeviceReadinessTestSource) AddWindowStatusChangeListener(
	listener sdk.WindowStatusChangeListener,
) sdk.Sub {
	self.mutex.Lock()
	self.listener = listener
	self.mutex.Unlock()
	self.subscribeOnce.Do(func() {
		close(self.subscribed)
	})
	if self.afterSubscribe != nil {
		self.afterSubscribe()
	}
	return &proxyDeviceReadinessTestSub{close: func() {
		self.mutex.Lock()
		self.listener = nil
		self.mutex.Unlock()
		self.unsubscribed.Store(true)
	}}
}

// publish synchronously changes status and notifies the installed listener.
func (self *proxyDeviceReadinessTestSource) publish(minSatisfied bool) {
	self.mutex.Lock()
	self.status.MinSatisfied = minSatisfied
	status := self.status
	listener := self.listener
	self.mutex.Unlock()
	if listener != nil {
		listener.WindowStatusChanged(&status)
	}
}

func proxyDeviceTransportTestJwt(clientId connect.Id) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"client_id":"%s"}`, clientId)))
	return fmt.Sprintf("%s.%s.", header, payload)
}

func newProxyDeviceTransportTestDevice(t testing.TB) (*sdk.DeviceLocal, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	proxyDeviceSettings := DefaultProxyDeviceSettings()
	proxyDeviceSettings.DisableWindowIdentityPersistence = true
	deviceLocalSettings := newProxyDeviceLocalSettings(
		ctx,
		&model.ProxyDeviceConfig{},
		proxyDeviceSettings,
	)
	if !deviceLocalSettings.HostedIncompatible {
		cancel()
		t.Fatal("proxy device settings are not hosted; carrier selection could escape H1")
	}

	networkSpace := sdk.NewNetworkSpaceWithUrls(
		ctx,
		"http://127.0.0.1:1",
		"ws://127.0.0.1:1",
		connect.DefaultClientStrategySettings(),
	)
	deviceLocal, err := sdk.NewPlatformDeviceLocal(
		nil,
		networkSpace,
		proxyDeviceTransportTestJwt(connect.NewId()),
		"proxy transport test",
		"proxy transport test",
		"test",
		sdk.NewId(),
		deviceLocalSettings,
	)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return deviceLocal, func() {
		_ = deviceLocal.CloseAndWait(context.Background())
		networkSpace.Close()
		cancel()
	}
}

func TestProxyDeviceLocalSettingsPinH1(t *testing.T) {
	deviceLocal, closeDevice := newProxyDeviceTransportTestDevice(t)
	defer closeDevice()

	if mode := deviceLocal.GetTransportSettings().Mode; mode != sdk.TransportModeH1 {
		t.Fatalf("proxy device transport mode = %q, want %q", mode, sdk.TransportModeH1)
	}
	if mode := deviceLocal.GetProviderTransportSettings().Mode; mode != sdk.TransportModeH1 {
		t.Fatalf("proxy provider transport mode = %q, want %q", mode, sdk.TransportModeH1)
	}

	for _, overrideMode := range []sdk.TransportMode{
		sdk.TransportModeAuto,
		sdk.TransportModeH3,
		sdk.TransportModeH1,
		sdk.TransportModeDns,
		sdk.TransportModeDnsPump,
		"future-unknown-mode",
	} {
		override := sdk.DefaultTransportSettings()
		override.Mode = overrideMode
		deviceLocal.SetTransportSettings(override)
		deviceLocal.SetProviderTransportSettings(override)
		if mode := deviceLocal.GetTransportSettings().Mode; mode != sdk.TransportModeH1 {
			t.Fatalf(
				"proxy device accepted transport override %q as %q, want immutable %q",
				overrideMode,
				mode,
				sdk.TransportModeH1,
			)
		}
		if mode := deviceLocal.GetProviderTransportSettings().Mode; mode != sdk.TransportModeH1 {
			t.Fatalf(
				"proxy provider accepted transport override %q as %q, want immutable %q",
				overrideMode,
				mode,
				sdk.TransportModeH1,
			)
		}
	}
}

// Cancellation and device shutdown are failure exits, not readiness signals.
// The prior shared-cancellation channel returned true for both and could make a
// timed-out warmup look healthy.
func TestProxyDeviceWaitForReadyRejectsCancellation(t *testing.T) {
	deviceLocal, closeDevice := newProxyDeviceTransportTestDevice(t)
	defer closeDevice()

	if deviceLocal.GetWindowStatus().MinSatisfied {
		t.Fatal("transport-only test unexpectedly has a ready provider window")
	}

	proxyCtx, proxyCancel := context.WithCancel(context.Background())
	defer proxyCancel()
	proxyDevice := &ProxyDevice{ctx: proxyCtx, deviceLocal: deviceLocal}
	canceledCallerCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	if proxyDevice.WaitForReady(canceledCallerCtx, -1) {
		t.Fatal("canceled caller was reported as proxy readiness")
	}

	canceledProxyCtx, cancelProxy := context.WithCancel(context.Background())
	canceledProxy := &ProxyDevice{ctx: canceledProxyCtx, deviceLocal: deviceLocal}
	cancelProxy()
	if canceledProxy.WaitForReady(context.Background(), -1) {
		t.Fatal("closed proxy device was reported as ready")
	}
}

// TestProxyDeviceActiveKeepsUnsatisfiedWindowForForeverRetry reproduces the
// main recreation loop without clocks or providers. A device becomes ready,
// then temporarily loses its minimum window. Selection must retain that same
// live device so its multi-window can keep refilling; only actual DeviceLocal
// lifecycle completion makes it inactive.
func TestProxyDeviceActiveKeepsUnsatisfiedWindowForForeverRetry(t *testing.T) {
	proxyCtx, cancelProxy := context.WithCancel(context.Background())
	defer cancelProxy()
	source := newProxyDeviceReadinessTestSource(true)
	proxyDevice := &ProxyDevice{ctx: proxyCtx, deviceState: source}

	if !proxyDevice.Active() {
		t.Fatal("ready live device was inactive")
	}
	source.publish(false)
	if !proxyDevice.Active() {
		t.Fatal("temporary window loss terminally discarded the retrying device")
	}
	source.done.Store(true)
	if proxyDevice.Active() {
		t.Fatal("completed DeviceLocal lifecycle remained active")
	}
}

// TestProxyDeviceActiveRejectsIncompleteLifecycle keeps construction and
// teardown states fail-closed. A partially built device has neither an owning
// context nor a DeviceLocal state source and must not panic or become reusable.
func TestProxyDeviceActiveRejectsIncompleteLifecycle(t *testing.T) {
	if (&ProxyDevice{}).Active() {
		t.Fatal("zero-value proxy device was active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if (&ProxyDevice{ctx: ctx}).Active() {
		t.Fatal("proxy device without a DeviceLocal lifecycle was active")
	}
}

// TestProxyDeviceManagerRetainsRetryingDeviceAcrossWindowLoss drives the
// complete selection path that produced main's churn. The first lookup observes
// a ready device; after the exact window-loss edge, the next lookup must return
// the identical instance without canceling it or entering device creation.
func TestProxyDeviceManagerRetainsRetryingDeviceAcrossWindowLoss(t *testing.T) {
	managerCtx, cancelManager := context.WithCancel(context.Background())
	defer cancelManager()
	manager := NewProxyDeviceManager(managerCtx, DefaultProxyDeviceManagerSettings())
	defer func() {
		_ = manager.CloseAndWait(context.Background())
	}()

	deviceCtx, cancelDevice := context.WithCancel(manager.ctx)
	defer cancelDevice()
	source := newProxyDeviceReadinessTestSource(true)
	proxyDevice := &ProxyDevice{
		ctx:         deviceCtx,
		cancel:      cancelDevice,
		deviceState: source,
	}
	proxyId := server.NewId()
	manager.proxyDevices[proxyId] = &proxyDeviceState{ProxyDevice: proxyDevice}

	readyDevice, err := manager.OpenProxyDevice(proxyId)
	if err != nil || readyDevice != proxyDevice {
		t.Fatalf("ready lookup = %p, %v; want existing %p", readyDevice, err, proxyDevice)
	}
	source.publish(false)
	retryingDevice, err := manager.OpenProxyDevice(proxyId)
	if err != nil || retryingDevice != proxyDevice {
		t.Fatalf("retrying lookup = %p, %v; want existing %p", retryingDevice, err, proxyDevice)
	}
	select {
	case <-deviceCtx.Done():
		t.Fatal("window loss canceled the hosted DeviceLocal retry lifecycle")
	default:
	}
}

// TestWaitForProxyDeviceReadyReturnsReadySnapshot proves an already-ready
// window returns immediately and still releases its temporary listener.
func TestWaitForProxyDeviceReadyReturnsReadySnapshot(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	deviceCtx, cancelDevice := context.WithCancel(context.Background())
	defer cancelDevice()
	source := newProxyDeviceReadinessTestSource(true)

	if !waitForProxyDeviceReady(callerCtx, deviceCtx, source, nil) {
		t.Fatal("ready snapshot was reported as not ready")
	}
	if !source.unsubscribed.Load() {
		t.Fatal("ready snapshot retained its status listener")
	}
}

// TestWaitForProxyDeviceReadyClosesSubscribeReadRace drives readiness in the
// exact gap after listener installation and before the first status read.
func TestWaitForProxyDeviceReadyClosesSubscribeReadRace(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	deviceCtx, cancelDevice := context.WithCancel(context.Background())
	defer cancelDevice()
	source := newProxyDeviceReadinessTestSource(false)
	source.afterSubscribe = func() {
		source.publish(true)
	}

	if !waitForProxyDeviceReady(callerCtx, deviceCtx, source, nil) {
		t.Fatal("readiness transition between subscribe and read was lost")
	}
	if !source.unsubscribed.Load() {
		t.Fatal("subscribe/read transition retained its status listener")
	}
}

// TestWaitForProxyDeviceReadyIgnoresNonReadyEvents proves only a satisfied
// event completes the wait; intermediate status updates retain the listener.
func TestWaitForProxyDeviceReadyIgnoresNonReadyEvents(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	deviceCtx, cancelDevice := context.WithCancel(context.Background())
	defer cancelDevice()
	source := newProxyDeviceReadinessTestSource(false)
	result := make(chan bool, 1)
	go func() {
		result <- waitForProxyDeviceReady(callerCtx, deviceCtx, source, nil)
	}()
	<-source.read

	source.publish(false)
	select {
	case <-result:
		t.Fatal("non-ready event completed the readiness wait")
	default:
	}
	source.publish(true)
	if ready := <-result; !ready {
		t.Fatal("ready event was reported as not ready")
	}
	if !source.unsubscribed.Load() {
		t.Fatal("ready event retained its status listener")
	}
}

// TestWaitForProxyDeviceReadyRejectsExactTimeout injects the timeout edge and
// verifies it cannot be confused with readiness.
func TestWaitForProxyDeviceReadyRejectsExactTimeout(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	deviceCtx, cancelDevice := context.WithCancel(context.Background())
	defer cancelDevice()
	source := newProxyDeviceReadinessTestSource(false)
	timeoutChannel := make(chan time.Time, 1)
	result := make(chan bool, 1)
	go func() {
		result <- waitForProxyDeviceReady(
			callerCtx,
			deviceCtx,
			source,
			timeoutChannel,
		)
	}()
	<-source.read
	timeoutChannel <- time.Time{}

	if ready := <-result; ready {
		t.Fatal("timeout was reported as readiness")
	}
	if !source.unsubscribed.Load() {
		t.Fatal("timeout retained its status listener")
	}
}

// TestWaitForProxyDeviceReadyRejectsCallerCancellation injects caller
// cancellation after subscription and proves it also releases the listener.
func TestWaitForProxyDeviceReadyRejectsCallerCancellation(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	deviceCtx, cancelDevice := context.WithCancel(context.Background())
	defer cancelDevice()
	source := newProxyDeviceReadinessTestSource(false)
	result := make(chan bool, 1)
	go func() {
		result <- waitForProxyDeviceReady(callerCtx, deviceCtx, source, nil)
	}()
	<-source.read
	cancelCaller()

	if ready := <-result; ready {
		t.Fatal("caller cancellation was reported as readiness")
	}
	if !source.unsubscribed.Load() {
		t.Fatal("caller cancellation retained its status listener")
	}
}

// TestWaitForProxyDeviceReadyRejectsDeviceCancellation injects device
// teardown after subscription and proves it cannot become a false success.
func TestWaitForProxyDeviceReadyRejectsDeviceCancellation(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	deviceCtx, cancelDevice := context.WithCancel(context.Background())
	source := newProxyDeviceReadinessTestSource(false)
	result := make(chan bool, 1)
	go func() {
		result <- waitForProxyDeviceReady(callerCtx, deviceCtx, source, nil)
	}()
	<-source.read
	cancelDevice()

	if ready := <-result; ready {
		t.Fatal("device cancellation was reported as readiness")
	}
	if !source.unsubscribed.Load() {
		t.Fatal("device cancellation retained its status listener")
	}
}

// TestProxyDeviceManagerSharesOneNetworkSpaceLifetime proves that device churn
// cannot create one permanent API/strategy worker per retired device. Every
// device borrows the same manager-owned NetworkSpace; only manager shutdown
// cancels its lifetime.
func TestProxyDeviceManagerSharesOneNetworkSpaceLifetime(t *testing.T) {
	managerCtx, managerCancel := context.WithCancel(context.Background())
	defer managerCancel()
	manager := NewProxyDeviceManager(managerCtx, DefaultProxyDeviceManagerSettings())
	clientSettings := connect.DefaultClientStrategySettings()
	clientSettings.EnableNormal = true
	clientSettings.EnableResilient = false
	ownedNetworkSpace := sdk.NewNetworkSpaceWithUrls(
		managerCtx,
		"http://127.0.0.1:1",
		"wss://127.0.0.1:1",
		clientSettings,
	)
	var networkSpaceCtx context.Context
	buildCount := 0
	manager.networkSpaceBuilder = func(ctx context.Context) *sdk.NetworkSpace {
		buildCount++
		networkSpaceCtx = ctx
		return ownedNetworkSpace
	}
	first := manager.networkSpaceForDevice()
	second := manager.networkSpaceForDevice()
	if first != ownedNetworkSpace || second != ownedNetworkSpace || buildCount != 1 {
		t.Fatalf("manager NetworkSpace build count = %d, want one shared instance", buildCount)
	}

	retiredProxyDevice := &ProxyDevice{}
	if err := retiredProxyDevice.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-networkSpaceCtx.Done():
		t.Fatal("retiring one proxy device canceled the shared NetworkSpace")
	default:
	}

	if err := manager.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-networkSpaceCtx.Done():
	default:
		t.Fatal("closing manager retained its shared NetworkSpace lifetime")
	}
}

// Closing a production manager waits for its owned NetworkSpace cleanup. The
// SDK separately pins that NetworkSpace.Close joins API and transport owners;
// this barrier pins the manager-to-SDK ownership edge without relying on a
// remote HTTP handler's lifecycle after the client request has been canceled.
func TestProxyDeviceManagerCloseAndWaitJoinsOwnedNetworkSpace(t *testing.T) {
	closeEntered := make(chan struct{})
	closeRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(closeRelease)
		})
	}

	managerCtx, managerCancel := context.WithCancel(context.Background())
	manager := NewProxyDeviceManager(managerCtx, DefaultProxyDeviceManagerSettings())
	ownedNetworkSpace := &sdk.NetworkSpace{}
	manager.networkSpaceBuilder = func(context.Context) *sdk.NetworkSpace {
		return ownedNetworkSpace
	}
	manager.networkSpaceCloser = func(networkSpace *sdk.NetworkSpace) {
		if networkSpace != ownedNetworkSpace {
			t.Errorf("closed NetworkSpace = %p, want %p", networkSpace, ownedNetworkSpace)
		}
		close(closeEntered)
		<-closeRelease
	}
	if got := manager.networkSpaceForDevice(); got != ownedNetworkSpace {
		t.Fatal("manager did not install its owned NetworkSpace")
	}
	t.Cleanup(func() {
		release()
		_ = manager.CloseAndWait(context.Background())
		managerCancel()
	})

	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- manager.CloseAndWait(context.Background())
	}()
	<-manager.ctx.Done()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-closeEntered:
	}
	select {
	case err := <-closeResult:
		t.Fatalf("manager close returned before NetworkSpace cleanup: %v", err)
	default:
	}
	release()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Shutdown closes admission before waiting: an already admitted constructor
// drains without publication, while a late caller cannot race WaitGroup.Wait.
func TestProxyDeviceManagerCloseJoinsAdmittedOpenAndRejectsLateOpen(t *testing.T) {
	managerCtx, managerCancel := context.WithCancel(context.Background())
	defer managerCancel()
	settings := DefaultProxyDeviceManagerSettings()
	settings.NetworkSpace = &sdk.NetworkSpace{}
	manager := NewProxyDeviceManager(managerCtx, settings)
	constructionEntered := make(chan struct{})
	constructionRelease := make(chan struct{})
	var constructionCount atomic.Int64
	manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
		constructionCount.Add(1)
		close(constructionEntered)
		<-constructionRelease
		deviceCtx, deviceCancel := context.WithCancel(managerCtx)
		return &ProxyDevice{
			ctx:      deviceCtx,
			cancel:   deviceCancel,
			settings: DefaultProxyDeviceSettings(),
		}, nil
	}

	openResult := make(chan error, 1)
	go func() {
		_, err := manager.OpenProxyDevice(server.NewId())
		openResult <- err
	}()
	<-constructionEntered
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- manager.CloseAndWait(context.Background())
	}()
	<-manager.ctx.Done()
	select {
	case err := <-closeResult:
		t.Fatalf("manager close returned before admitted construction: %v", err)
	default:
	}
	if _, err := manager.OpenProxyDevice(server.NewId()); err == nil {
		t.Fatal("manager admitted an open after shutdown")
	}
	if got := constructionCount.Load(); got != 1 {
		t.Fatalf("late open reached constructor: count=%d", got)
	}

	close(constructionRelease)
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case err := <-openResult:
		if err == nil {
			t.Fatal("shutdown-time construction was published")
		}
	}
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestProxyDeviceManagerPreservesInjectedNetworkSpace ensures test and caller
// owned NetworkSpaces are reused without invoking the production builder.
func TestProxyDeviceManagerPreservesInjectedNetworkSpace(t *testing.T) {
	managerCtx, managerCancel := context.WithCancel(context.Background())
	defer managerCancel()
	sharedNetworkSpace := &sdk.NetworkSpace{}
	settings := DefaultProxyDeviceManagerSettings()
	settings.NetworkSpace = sharedNetworkSpace
	manager := NewProxyDeviceManager(managerCtx, settings)
	manager.networkSpaceBuilder = func(context.Context) *sdk.NetworkSpace {
		t.Fatal("production NetworkSpace builder ran for injected NetworkSpace")
		return nil
	}
	if got := manager.networkSpaceForDevice(); got != sharedNetworkSpace {
		t.Fatal("manager replaced injected NetworkSpace")
	}
	if err := manager.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-managerCtx.Done():
		t.Fatal("manager canceled caller-owned parent context")
	default:
	}
}
