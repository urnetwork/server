package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	proxylib "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A SOCKS dial into a device window with no provider waits for one, within
// its connect budget, as a CONNECT does (TestHttpConnectWaitsForARouteBeforeDialing),
// and opens no stack connection meanwhile. It used to dial at once: the
// window dropped every SYN while it held no provider, so the dial waited out
// the stack's SYN backoff, and a window that formed late in the dial's 30 s
// missed it even though a provider joined in time.
func TestSocksConnectWaitsForARouteBeforeDialing(t *testing.T) {
	configureHttpBudgetTest(t)
	manager := newHttpConnectTestManager(t)
	proxyId := server.NewId()
	tun := httpConnectTestTun(t)
	source := newProxyDeviceReadinessTestSource(false)
	deviceCtx, cancelDevice := context.WithCancel(manager.ctx)
	defer cancelDevice()
	manager.proxyDevices[proxyId] = &proxyDeviceState{ProxyDevice: &ProxyDevice{
		ctx:         deviceCtx,
		cancel:      cancelDevice,
		deviceState: source,
		tun:         tun,
		settings:    DefaultProxyDeviceSettings(),
	}}
	core := (&socks5Server{ctx: manager.ctx, settings: DefaultProxySettings(), proxyDeviceManager: manager}).newSocksProxy()
	request := &proxylib.Request{
		AuthContext: &proxylib.AuthContext{Payload: map[string]string{"username": model.SignProxyId(proxyId)}},
		DestAddr:    &proxylib.AddrSpec{IP: net.ParseIP("192.0.2.1"), Port: 443},
	}
	dial := func(wait time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		conn, err := core.ConnectDialWithRequest(ctx, request, "tcp", "192.0.2.1:443")
		if conn != nil {
			conn.Close()
		}
		return err
	}
	openings := func() uint64 {
		return tun.Stats().TCP.ActiveConnectionOpenings.Value()
	}

	start := time.Now()
	err := dial(300 * time.Millisecond)
	if got := openings(); got != 0 {
		t.Fatalf("a window with no provider opened %d stack connections", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial into a window with no provider = %v, want the caller's deadline", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("returned after %s, before the caller's deadline", elapsed)
	}

	// one provider routes even below the window's minimum
	source.mutex.Lock()
	source.status.ProviderStateAdded = 1
	source.mutex.Unlock()
	_ = dial(300 * time.Millisecond)
	if got := openings(); got == 0 {
		t.Fatal("a routable window did not dial")
	}
}
