package proxy

// End-to-end drain behavior over the real proxy servers (PROXYDRAIN1.md
// §3.2), using the full harness (local connect+api servers, a real provider,
// real internet egress — see proxy_test.go):
//
//   - an established socks tunnel carrying a live TLS session keeps working
//     after Drain (the deploy already flipped new flows to the replacement
//     container; established flows are exactly what the drain preserves)
//   - new connections to the drained listeners are refused
//   - the drain observes the tunnel ending (WaitIdle), which is what lets
//     the process exit the moment it is idle instead of holding the full
//     grace

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/urnetwork/server"
)

func TestProxyDrainGraceful(t *testing.T) {
	if testing.Short() {
		return
	}
	// see TestProxy: fixed ports cannot be rebound on a rerun in the same process
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyDrainGraceful\n")
		h := setupProxyTest(t)
		defer h.cancel()

		// ---- establish a socks tunnel with a live keep-alive TLS session ----
		dialer, err := xproxy.SOCKS5(
			"tcp",
			fmt.Sprintf("127.0.0.1:%d", InternalSocksPort),
			&xproxy.Auth{User: h.signedProxyId, Password: "x"},
			xproxy.Direct,
		)
		if err != nil {
			t.Fatalf("socks: dialer: %v", err)
		}
		contextDialer := dialer.(xproxy.ContextDialer)

		// the transport dials through the socks tunnel freely BEFORE the
		// drain (requireProxyGet may retry transient real-network failures);
		// after the drain begins, dialing is blocked so the post-drain
		// request MUST reuse the pooled tunnel — reproducing the deploy
		// reality that no new tunnel can reach this instance
		var dialLock sync.Mutex
		dialAllowed := true
		transport := &http.Transport{
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				dialLock.Lock()
				allowed := dialAllowed
				dialLock.Unlock()
				if !allowed {
					return nil, fmt.Errorf("no new dials after drain (the pooled tunnel must be reused)")
				}
				return contextDialer.DialContext(ctx, network, addr)
			},
			// keep-alive on so the tunnel is pooled between the two requests
			DisableKeepAlives: false,
		}

		// one client for BOTH legs: requireProxyGet closes idle conns when it
		// returns, which would drop the pooled tunnel this test must hold
		// across the drain
		client := &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		}
		get := func() (int, error) {
			request, err := http.NewRequest("GET", testTargetUrl, nil)
			if err != nil {
				return 0, err
			}
			response, err := client.Do(request)
			if err != nil {
				return 0, err
			}
			defer response.Body.Close()
			io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			return response.StatusCode, nil
		}

		// prove the path end to end before the drain (with retry for
		// transient real-network failures), leaving the tunnel pooled
		waitFor(t, 120*time.Second, "pre-drain request through the tunnel", func() bool {
			statusCode, err := get()
			if err != nil {
				fmt.Printf("[progress][drain-before]retry err=%v\n", err)
				return false
			}
			if statusCode < 200 || 400 <= statusCode {
				fmt.Printf("[progress][drain-before]retry status=%d\n", statusCode)
				return false
			}
			return true
		})

		// ---- drain both tcp ingresses ----
		func() {
			dialLock.Lock()
			defer dialLock.Unlock()
			dialAllowed = false
		}()
		h.socks5.Drain()
		h.httpS.Drain()

		// new connections are refused: the listeners are closed
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", InternalSocksPort), 500*time.Millisecond); err == nil {
			c.Close()
			t.Fatalf("socks dial after drain must be refused")
		}
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", InternalHttpsPort), 500*time.Millisecond); err == nil {
			c.Close()
			t.Fatalf("https dial after drain must be refused")
		}

		// the established tunnel still carries a full request over its live
		// TLS session, reusing the pooled conn — the dial guard turns any
		// re-dial attempt (i.e. the tunnel died) into a hard error
		if statusCode, err := get(); err != nil {
			t.Fatalf("drain-after request over the held tunnel: %v", err)
		} else if statusCode < 200 || 400 <= statusCode {
			t.Fatalf("drain-after status = %d", statusCode)
		}
		fmt.Printf("[progress][drain-after]OK over the held tunnel\n")
		if activeCount := h.socks5.ActiveCount(); activeCount < 1 {
			t.Fatalf("socks active = %d, want the held tunnel counted", activeCount)
		}

		// ---- ending the tunnel completes the drain ----
		idleCh := make(chan bool, 2)
		go func() {
			waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer waitCancel()
			idleCh <- h.socks5.WaitIdle(waitCtx)
			idleCh <- h.httpS.WaitIdle(waitCtx)
		}()
		transport.CloseIdleConnections()
		for i := 0; i < 2; i += 1 {
			select {
			case idle := <-idleCh:
				if !idle {
					t.Fatalf("drain did not observe the tunnel ending")
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("WaitIdle did not return")
			}
		}
		fmt.Printf("[progress]drain graceful e2e complete\n")
	})
}
