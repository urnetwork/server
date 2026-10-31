// Exercises the production CONNECT handler's setup budget without external I/O.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	proxylib "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
)

// Lets the real hijacked handler use a pipe and synctest's virtual clock.
type httpBudgetTestHijacker struct {
	*httptest.ResponseRecorder
	connection net.Conn
}

// Transfers the accepted connection exactly as net/http does for CONNECT.
func (self *httpBudgetTestHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return self.connection, bufio.NewReadWriter(bufio.NewReader(self.connection), bufio.NewWriter(self.connection)), nil
}

// Installs synthetic signing and plan data before entering a clock bubble.
func configureHttpBudgetTest(t *testing.T) {
	t.Helper()
	t.Cleanup(server.Vault.PushSimpleResource("proxy.yml", []byte("secrets:\n  - synthetic-connect-budget-secret\n")))
	t.Cleanup(server.Config.PushSimpleResource("pro.yml", []byte("enforce_features: false\nfree:\n  data: 1GiB\n  data_period: 1h\npro:\n  data: 1GiB\n  data_period: 1h\nreferral:\n  bonus_per_referral: 1GiB\n  period: 1h\ndata_code:\n  duration: 1h\n")))
	disableHttpConnectTestPlanEnforcement(t)
	// Global packet-pool channels must be created outside the clock bubble.
	connect.MessagePoolCounts()
}

// Returns the observed status and handler completion, retaining no real socket.
func startHttpBudgetTestConnect(t *testing.T, core *proxylib.HttpProxy, proxyId server.Id) (<-chan int, <-chan struct{}) {
	t.Helper()
	client, accepted := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	request := httptest.NewRequestWithContext(server.WithoutPostgres(context.Background()), http.MethodConnect, "https://target.example:443", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("Proxy-Authorization", httpConnectTestBasic(proxyId))
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		core.ServeHTTP(&httpBudgetTestHijacker{ResponseRecorder: httptest.NewRecorder(), connection: accepted}, request)
	}()
	responseDone := make(chan int, 1)
	go func() {
		response, err := http.ReadResponse(bufio.NewReader(client), request)
		if err != nil {
			responseDone <- 0
			return
		}
		_ = response.Body.Close()
		responseDone <- response.StatusCode
	}()
	return responseDone, handlerDone
}

// A held constructor must not retain the CONNECT that first started it.
func TestHttpConnectBudgetIncludesDeviceCreation(t *testing.T) {
	testHttpConnectCreationBudget(t, false)
}

// A held constructor must not retain a later CONNECT for the same proxy id.
func TestHttpConnectBudgetIncludesCoalescedDeviceCreation(t *testing.T) {
	testHttpConnectCreationBudget(t, true)
}

// Pins both ownership positions at the constructor barrier. These tests also
// compile against the synchronous manager implementation that exceeded budget.
func testHttpConnectCreationBudget(t *testing.T, coalesced bool) {
	t.Helper()
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		entered, release := make(chan struct{}), make(chan struct{})
		releaseBuild := sync.OnceFunc(func() { close(release) })
		defer releaseBuild()
		setupErr := errors.New("synthetic construction failure")
		var builds atomic.Int32
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			if builds.Add(1) == 1 {
				close(entered)
			}
			<-release
			return nil, setupErr
		}
		legacyResult := make(chan error, 1)
		if coalesced {
			go func() { _, err := manager.OpenProxyDevice(proxyId); legacyResult <- err }()
			<-entered
		}
		core := newHttpConnectTestProxy(manager)
		core.Settings().ProxyConnectBudget = time.Second
		responseDone, handlerDone := startHttpBudgetTestConnect(t, core, proxyId)
		<-entered
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case code := <-responseDone:
			if code != http.StatusGatewayTimeout {
				t.Errorf("status %d, want 504", code)
			}
		default:
			t.Error("CONNECT retained device construction beyond its setup budget")
		}
		if manager.ctx.Err() != nil || builds.Load() != 1 {
			t.Error("request timeout canceled shared setup or duplicated construction")
		}
		releaseBuild()
		<-handlerDone
		if coalesced {
			if err := <-legacyResult; !errors.Is(err, setupErr) {
				t.Errorf("legacy creator lost its construction result: %v", err)
			}
		}
		synctest.Wait()
		if _, err := manager.OpenProxyDevice(proxyId); !errors.Is(err, setupErr) || builds.Load() != 1 {
			t.Error("canceled waiter bypassed the shared construction failure cooldown")
		}
		stats := core.Stats()
		if stats.ConnectTimeouts != 1 || stats.DialRefusedForbidden != 0 {
			t.Errorf("setup timeout was classified as admission refusal: %+v", stats)
		}
	})
}
