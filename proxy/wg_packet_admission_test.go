// Pins the signed WireGuard setup exception and isolates established traffic
// from a failed, panicking, or still-running device owner construction.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	wgproxy "github.com/urnetwork/proxy"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The production manager owns the admission, while the unopened NetworkSpace
// keeps these construction-failure controls independent of external services.
func wgAdmissionTestManager(t *testing.T) *ProxyDeviceManager {
	t.Helper()
	ctx := server.WithoutPostgres(t.Context())
	manager := NewProxyDeviceManager(ctx, &ProxyDeviceManagerSettings{
		CheckProxyDeviceIdleTimeout: time.Minute,
		NetworkSpace:                &sdk.NetworkSpace{},
	})
	t.Cleanup(func() {
		if err := manager.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return manager
}

// Uses the real post-decryption packet entry and peer factory. The encrypted
// receive owner's Noise/source checks are separately exercised by the native
// userspace WireGuard integration; this fixture needs no socket or listener.
func wgAdmissionTestProxy(t *testing.T, manager *ProxyDeviceManager, ids map[netip.Addr]server.Id) *wgproxy.WgProxy {
	t.Helper()
	settings := wgproxy.DefaultWgProxySettings()
	settings.Log = connect.NewNoopLogger()
	wg := wgproxy.NewWgProxy(manager.ctx, settings)
	t.Cleanup(func() { _ = wg.Close() })
	clients := map[netip.Addr]*wgproxy.WgClient{}
	for addr, proxyId := range ids {
		_, publicKey, err := wgproxy.WgGenKeyPairStrings()
		if err != nil {
			t.Fatal(err)
		}
		clients[addr] = &wgproxy.WgClient{
			PublicKey: publicKey, ClientIpv4: addr, Tun: wgTunFactory(manager, proxyId),
		}
	}
	if err := wg.SetClients(clients); err != nil {
		t.Fatal(err)
	}
	return wg
}

// The real packet entry may attempt failed setup once per second. A sustained
// refused packet stream does not postpone the next admitted attempt.
func TestWgPacketFailedSetupHasBoundedRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		var calls atomic.Int32
		setupErr := errors.New("synthetic configuration read failure")
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			calls.Add(1)
			return nil, setupErr
		}
		addr := netip.MustParseAddr("192.0.2.42")
		wg := wgAdmissionTestProxy(t, manager, map[netip.Addr]server.Id{addr: server.NewId()})
		packet := buildWgTestPacket(t, addr.String(), "198.51.100.21", "bounded cold setup")
		wantPacket := bytes.Clone(packet)
		for range 128 {
			if count, err := wg.Write([][]byte{packet}, 0); count != 0 || !errors.Is(err, setupErr) {
				t.Fatalf("failed setup result: count=%d err=%v", count, err)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("failed packet cohort constructed %d devices, want 1", got)
		}
		time.Sleep(time.Second - time.Nanosecond)
		_, _ = wg.Write([][]byte{packet}, 0)
		if calls.Load() != 1 {
			t.Fatal("setup retried before the one-second boundary")
		}
		time.Sleep(time.Nanosecond)
		_, _ = wg.Write([][]byte{packet}, 0)
		if calls.Load() != 2 {
			t.Fatal("refused packets moved the retry boundary")
		}
		if !bytes.Equal(packet, wantPacket) || manager.DeviceCount() != 0 {
			t.Fatal("refusal retained a device or mutated the borrowed packet")
		}
		if got := server.PacketPostgresAttempts(manager.ctx); got != 0 {
			t.Fatalf("refused packet path attempted PostgreSQL %d times", got)
		}
	})
}

// Concurrent packets refuse immediately while one peer owns construction;
// they do not become waiting calls or suppress an unrelated peer's attempt.
func TestWgTunFactoryRefusesConcurrentSetupWithoutWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		blockedId, otherId := server.NewId(), server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		setupErr := errors.New("synthetic construction failure")
		var calls atomic.Int32
		manager.proxyDeviceBuilder = func(id server.Id) (*ProxyDevice, error) {
			calls.Add(1)
			if id == blockedId {
				close(entered)
				<-release
			}
			return nil, setupErr
		}
		factory := wgTunFactory(manager, blockedId)
		first := make(chan error, 1)
		go func() { _, err := factory(); first <- err }()
		<-entered
		const count = 32
		refused := make(chan error, count)
		for range count {
			go func() { _, err := factory(); refused <- err }()
		}
		synctest.Wait()
		if len(refused) != count {
			t.Errorf("%d of %d cold packets returned while setup was held", len(refused), count)
		}
		if _, err := wgTunFactory(manager, otherId)(); !errors.Is(err, setupErr) || calls.Load() != 2 {
			t.Error("one peer's construction blocked another peer")
		}
		close(release)
		if err := <-first; !errors.Is(err, setupErr) {
			t.Errorf("construction returned %v", err)
		}
		for range count {
			if err := <-refused; err == nil {
				t.Error("unchecked cold packet was admitted")
			}
		}
	})
}

// Recovery belongs inside the manager's creation owner. Catching the panic
// only in the outer WireGuard factory leaves every coalesced opener parked.
func TestWgTunFactoryPanicCompletesCoalescedManagerOpeners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		proxyId := server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseSetup := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseSetup()
		setupErr := errors.New("synthetic constructor panic")
		var calls atomic.Int32
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
				panic(setupErr)
			}
			return nil, setupErr
		}
		first := make(chan error, 1)
		go func() { _, err := wgTunFactory(manager, proxyId)(); first <- err }()
		<-entered
		waiter := make(chan error, 1)
		go func() { _, err := manager.OpenProxyDevice(proxyId); waiter <- err }()
		synctest.Wait()
		manager.stateLock.RLock()
		state := manager.proxyDevices[proxyId]
		manager.stateLock.RUnlock()
		if state == nil || state.users.Load() != 2 {
			t.Fatal("fixture did not coalesce both openers")
		}
		releaseSetup()
		if err := <-first; !errors.Is(err, setupErr) {
			t.Errorf("factory panic result=%v", err)
		}
		synctest.Wait()
		select {
		case err := <-waiter:
			if !errors.Is(err, setupErr) {
				t.Errorf("coalesced panic result=%v", err)
			}
		default:
			t.Error("constructor panic left a coalesced opener waiting")
			manager.Close()
			<-waiter
			return
		}
		if manager.DeviceCount() != 0 {
			t.Fatal("constructor panic retained an unfinished per-id marker")
		}
		if _, err := manager.OpenProxyDevice(proxyId); !errors.Is(err, setupErr) || calls.Load() != 2 {
			t.Fatal("completed panic prevented a later construction attempt")
		}
	})
}

// Cancellation closes construction admission even when the parent context
// was canceled before the manager's explicit Close method runs.
func TestWgTunFactoryCanceledOwnerDoesNotConstruct(t *testing.T) {
	manager := wgAdmissionTestManager(t)
	manager.cancel()
	var calls atomic.Int32
	manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
		calls.Add(1)
		return nil, errors.New("construction should not run")
	}
	if _, err := wgTunFactory(manager, server.NewId())(); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled owner result=%v", err)
	}
	if calls.Load() != 0 || manager.DeviceCount() != 0 {
		t.Fatal("canceled owner admitted construction")
	}
}

// Established traffic keeps its real WireGuard-to-ProxyDevice handoff and
// buffer ownership while another authenticated device owner remains blocked.
func TestWgPacketLiveOwnerBypassesUnrelatedSetup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		liveId, coldId := server.NewId(), server.NewId()
		liveAddr, coldAddr := netip.MustParseAddr("192.0.2.42"), netip.MustParseAddr("192.0.2.43")
		entered, release := make(chan struct{}), make(chan struct{})
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			close(entered)
			<-release
			return nil, errors.New("synthetic cold owner failure")
		}
		var delivered atomic.Int32
		device := &ProxyDevice{
			ctx: manager.ctx, cancel: func() {}, deviceState: newProxyDeviceReadinessTestSource(true),
			receiveMonitor: connect.NewMonitor(),
			sendOwnedPacketsForTest: func(packets [][]byte) int {
				for _, packet := range packets {
					if !connect.MessagePoolReturn(packet) {
						t.Error("accepted packet did not own its pooled copy")
					}
					delivered.Add(1)
				}
				return len(packets)
			},
		}
		manager.proxyDevices[liveId] = &proxyDeviceState{ProxyDevice: device}
		wg := wgAdmissionTestProxy(t, manager, map[netip.Addr]server.Id{liveAddr: liveId, coldAddr: coldId})
		livePacket := buildWgTestPacket(t, liveAddr.String(), "198.51.100.21", "established owner")
		wantPacket := bytes.Clone(livePacket)
		beforeTaken, beforeReturned, _ := connect.MessagePoolCounts()
		if n, err := wg.Write([][]byte{livePacket}, 0); n != 1 || err != nil {
			t.Fatalf("healthy activation failed: count=%d err=%v", n, err)
		}
		coldPacket := buildWgTestPacket(t, coldAddr.String(), "198.51.100.21", "cold owner")
		coldResult := make(chan error, 1)
		go func() { _, err := wg.Write([][]byte{coldPacket}, 0); coldResult <- err }()
		<-entered
		liveResult := make(chan error, 1)
		go func() {
			for range 128 {
				if n, err := wg.Write([][]byte{livePacket}, 0); n != 1 || err != nil {
					liveResult <- errors.New("live packet refused during unrelated setup")
					return
				}
			}
			liveResult <- nil
		}()
		synctest.Wait()
		select {
		case err := <-liveResult:
			if err != nil {
				t.Error(err)
			}
		default:
			t.Error("established packet waited behind unrelated setup")
		}
		close(release)
		if err := <-coldResult; err == nil {
			t.Error("cold setup failure admitted a packet")
		}
		synctest.Wait()
		afterTaken, afterReturned, _ := connect.MessagePoolCounts()
		if delivered.Load() != 129 || !bytes.Equal(livePacket, wantPacket) || afterTaken-beforeTaken != afterReturned-beforeReturned {
			t.Error("healthy packet ownership or established reuse changed")
		}
		if got := server.PacketPostgresAttempts(manager.ctx); got != 0 {
			t.Fatalf("established packet path attempted PostgreSQL %d times", got)
		}
	})
}

// A bad signature must not reach even the cold feature-policy query. The
// PostgreSQL tripwire covers the actual validator and all indirect model calls.
func TestWgSignedIdentityPrecedesPolicyAndDeviceReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		config := model.Pro()
		previous := config.EnforceFeatures
		config.EnforceFeatures = true
		defer func() { config.EnforceFeatures = previous }()
		for _, mismatch := range []bool{false, true} {
			ctx := server.WithoutPostgres(context.Background())
			client := newTestProxyClient(t, netip.MustParseAddr("192.0.2.42"))
			client.AuthToken = "invalid-synthetic-signature"
			if mismatch {
				client.AuthToken = model.SignProxyId(server.NewId())
			}
			wg := &wgServer{ctx: ctx}
			var counts *wgClientCounts
			var clients map[netip.Addr]*wgproxy.WgClient
			r := server.HandleError(func() { clients, counts = wg.validWgClients([]*model.ProxyClient{client}) })
			if r != nil || counts == nil || counts.invalidAuthToken != 1 || len(clients) != 0 || server.PacketPostgresAttempts(ctx) != 0 {
				t.Fatalf("invalid signed identity reached durable work: mismatch=%t panic=%v attempts=%d", mismatch, r, server.PacketPostgresAttempts(ctx))
			}
		}
	})
}

// The real cold constructor may read the signed owner's configuration from
// PostgreSQL. A tripwire substitutes its failure before pool acquisition and
// proves that the actual factory completes and releases that failed owner.
func TestWgPacketColdConfigFailureCompletesItsOwner(t *testing.T) {
	packet := buildWgTestPacket(t, "192.0.2.42", "198.51.100.21", "cold configuration")
	server.DefaultTestEnv().Run(t, func(test testing.TB) {
		ctx := server.WithoutPostgres(context.Background())
		manager := NewProxyDeviceManager(ctx, &ProxyDeviceManagerSettings{NetworkSpace: &sdk.NetworkSpace{}})
		defer func() { _ = manager.CloseAndWait(context.Background()) }()
		config := model.Pro()
		previous := config.EnforceFeatures
		config.EnforceFeatures = false
		defer func() { config.EnforceFeatures = previous }()
		client := newTestProxyClient(test, netip.MustParseAddr("192.0.2.42"))
		serverOwner := &wgServer{ctx: ctx, proxyDeviceManager: manager}
		clients, counts := serverOwner.validWgClients([]*model.ProxyClient{client})
		if len(clients) != 1 || counts.invalidAuthToken != 0 || server.PacketPostgresAttempts(ctx) != 0 {
			test.Fatal("signed peer registration did not defer device construction")
		}
		settings := wgproxy.DefaultWgProxySettings()
		settings.Log = connect.NewNoopLogger()
		wg := wgproxy.NewWgProxy(ctx, settings)
		defer wg.Close()
		if err := wg.SetClients(clients); err != nil {
			test.Fatal(err)
		}
		count, err := wg.Write([][]byte{packet}, 0)
		if count != 0 || !errors.Is(err, server.ErrPacketPostgres) || server.PacketPostgresAttempts(ctx) != 1 {
			test.Fatalf("cold configuration failure: count=%d err=%v attempts=%d", count, err, server.PacketPostgresAttempts(ctx))
		}
		if manager.DeviceCount() != 0 {
			test.Fatal("actual cold configuration panic retained its construction marker")
		}
	})
}
