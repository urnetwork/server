package proxy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The exchange service port the harness bound until the release precheck of
// 2026-10-15.
const formerProxyTestConnectServicePort = 7300

// A listener on a port, bound with SO_REUSEPORT as connect's exchange binds
// its service port, that accepts and closes every connection and counts them.
type exchangePortSquatter struct {
	listener net.Listener
	accepted atomic.Int64
	done     sync.WaitGroup
}

func newExchangePortSquatter(t testing.TB, port int) *exchangePortSquatter {
	t.Helper()
	listenConfig := net.ListenConfig{Control: server.SoReusePort}
	// The loopback address is more specific than the exchange's wildcard bind,
	// so every loopback connection to the port reaches the squatter.
	listener, err := listenConfig.Listen(context.Background(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("squat port %d: %v", port, err)
	}
	squatter := &exchangePortSquatter{listener: listener}
	squatter.done.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("squatter accept: %v", err)
				}
				return
			}
			squatter.accepted.Add(1)
			conn.Close()
		}
	})
	return squatter
}

func (self *exchangePortSquatter) close() {
	self.listener.Close()
	self.done.Wait()
}

// The release precheck of 2026-10-15: the harness bound connect's exchange on
// the fixed service port 7300, which connect binds with SO_REUSEPORT on every
// address. Any other process holding that port, as a second run of this
// package, the providertunnel tests or a second release gate do, took a share
// of the harness's resident connections, each of which waited out the 30s
// resident wait there and failed, and flows through the proxy stalled past the
// churn tests' 90s retry windows. With a squatter on 7300 the harness carries
// a download and an upload through the proxy, and no connection reaches the
// squatter.
func TestProxyHarnessExchangeIsolatedFromAFixedPortSquatter(t *testing.T) {
	if testing.Short() {
		return
	}
	squatter := newExchangePortSquatter(t, formerProxyTestConnectServicePort)
	defer squatter.close()

	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)

		targetUrl, closeTarget := startLocalTarget(t)
		defer closeTarget()

		client := newSocksProxyClient(t, h.signedProxyId, h.socksPort, 60*time.Second)
		defer client.CloseIdleConnections()
		requireFetchData(t, "squatted download", client, targetUrl, 256*1024, 120*time.Second)
		if err := uploadData(client, targetUrl, 256*1024); err != nil {
			t.Fatalf("squatted upload: %v", err)
		}
		if accepted := squatter.accepted.Load(); accepted != 0 {
			t.Fatalf("%d of the harness's connections reached the squatter on port %d", accepted, formerProxyTestConnectServicePort)
		}
	})
}
