// Keeps cold admission reads inside the connection budget, including raised
// database cancellation and a canceled read that returns an empty result.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	proxylib "github.com/urnetwork/proxy/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// An uncached caller lock belongs to the CONNECT's budget, not the manager.
func TestHttpConnectBudgetIncludesCallerAdmission(t *testing.T) {
	testHttpConnectAdmissionBudget(t, false)
}

// An uncached plan lookup has the same deadline and must not become a 403.
func TestHttpConnectBudgetIncludesPlanAdmission(t *testing.T) {
	testHttpConnectAdmissionBudget(t, true)
}

// The selected reader blocks at the actual callback boundary, without using
// Redis or PostgreSQL. Both model return conventions preserve cancellation.
func testHttpConnectAdmissionBudget(t *testing.T, plan bool) {
	t.Helper()
	configureHttpBudgetTest(t)
	for _, raise := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			manager := newHttpConnectTestManager(t)
			proxyId, siblingId := server.NewId(), server.NewId()
			entered, release := make(chan struct{}), make(chan struct{})
			releaseRead := sync.OnceFunc(func() { close(release) })
			defer releaseRead()
			wait := func(ctx context.Context) {
				close(entered)
				select {
				case <-ctx.Done():
				case <-release:
				}
				if raise {
					server.Raise(fmt.Errorf("synthetic admission read: %w", ctx.Err()))
				}
			}
			manager.proxyDeviceConfigReader = func(ctx context.Context, id server.Id) *model.ProxyDeviceConfig {
				if !plan && id == proxyId {
					wait(ctx)
					return nil
				}
				return &model.ProxyDeviceConfig{}
			}
			manager.proxyFeatureReader = func(ctx context.Context, id server.Id, _ string) bool {
				if plan && id == proxyId {
					wait(ctx)
					return false
				}
				return true
			}
			if plan {
				seedHttpConnectTestLock(manager, proxyId, true)
			}
			var builds atomic.Int32
			manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
				builds.Add(1)
				return nil, fmt.Errorf("unexpected construction after canceled admission")
			}
			core := newHttpConnectTestProxy(manager)
			core.Settings().ProxyConnectBudget = time.Second
			responseDone, handlerDone := startHttpBudgetTestConnect(t, core, proxyId)
			<-entered
			callerAddr := netip.MustParseAddr("192.0.2.1")
			if err := manager.checkCallerWithContext(manager.ctx, siblingId, callerAddr); err != nil {
				t.Fatal(err)
			}
			if err := manager.checkFeature(manager.ctx, siblingId, model.FeatureHttpsProxy); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)
			synctest.Wait()
			select {
			case code := <-responseDone:
				if code != http.StatusGatewayTimeout {
					t.Errorf("raise=%t: canceled admission returned %d, want 504", raise, code)
				}
			default:
				t.Errorf("raise=%t: admission outlived the CONNECT budget", raise)
			}
			releaseRead()
			<-handlerDone
			if builds.Load() != 0 || manager.ctx.Err() != nil {
				t.Error("canceled admission started setup or canceled its shared owner")
			}
			if !plan && manager.lockCache.get(proxyId, time.Now()).found {
				t.Error("canceled caller lookup published a false missing-proxy cache entry")
			}
			stats := core.Stats()
			if stats.ConnectTimeouts != 1 || stats.DialRefusedForbidden != 0 || stats.DialRefusedUnknownProxy != 0 {
				t.Errorf("raise=%t: cancellation changed authorization: %+v", raise, stats)
			}
		})
	}
}

// The SOCKS callback has no request context, so its authorization reads use
// the listener's configured setup budget instead of waiting for server exit.
func TestSocksAdmissionReadUsesSetupBudget(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		entered := make(chan struct{})
		manager.proxyFeatureReader = func(ctx context.Context, _ server.Id, _ string) bool {
			close(entered)
			<-ctx.Done()
			server.Raise(ctx.Err())
			return false
		}
		settings := DefaultProxySettings()
		settings.ProxyConnectBudget = time.Second
		core := (&socks5Server{ctx: manager.ctx, settings: settings, proxyDeviceManager: manager}).newSocksProxy()
		result := make(chan bool, 1)
		go func() { result <- core.ValidUser(model.SignProxyId(proxyId), "synthetic", "192.0.2.1:1234") }()
		<-entered
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case valid := <-result:
			if valid {
				t.Error("canceled admission authenticated a SOCKS client")
			}
		default:
			t.Fatal("SOCKS authorization outlived its setup budget")
		}
		if manager.ctx.Err() != nil {
			t.Fatal("SOCKS admission canceled its manager")
		}
	})
}

// A SOCKS dial passes its bounded setup context through the same manager API.
func TestSocksConnectBudgetIncludesDeviceCreation(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		releaseBuild := sync.OnceFunc(func() { close(release) })
		defer releaseBuild()
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			close(entered)
			<-release
			return nil, errors.New("synthetic construction failure")
		}
		settings := DefaultProxySettings()
		settings.ProxyConnectBudget = time.Second
		core := (&socks5Server{ctx: manager.ctx, settings: settings, proxyDeviceManager: manager}).newSocksProxy()
		request := &proxylib.Request{
			AuthContext: &proxylib.AuthContext{Payload: map[string]string{"username": model.SignProxyId(proxyId)}},
			DestAddr:    &proxylib.AddrSpec{FQDN: "target.example", Port: 443},
		}
		result := make(chan error, 1)
		go func() {
			_, err := core.ConnectDialWithRequest(manager.ctx, request, "tcp", "target.example:443")
			result <- err
		}()
		<-entered
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-result:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("SOCKS setup returned %v, want deadline exceeded", err)
			}
		default:
			t.Error("SOCKS construction outlived its setup budget")
		}
		if manager.ctx.Err() != nil {
			t.Error("SOCKS setup canceled its manager")
		}
		releaseBuild()
	})
}
