package connect

// The front's tcp servers as the nginx front configures them: connect sets
// only the handler, the error log and the serving context, and the nginx
// front sets the rest (nginx.Front.ConfigureServer). Through connect's own
// public sockets and the real nginx front, nginx's keepalive_requests and
// keepalive_time close connections on h1 and h2. The requests go to the lb
// status route on an lb name, which no limit charges.

import (
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// An h1 GET of the lb status route through client, and the connection it
// went over.
func getTestFrontLbStatus(t *testing.T, env *testFrontEnv, client *http.Client) (*http.Response, net.Conn) {
	t.Helper()
	var conn net.Conn
	r, err := http.NewRequestWithContext(env.ctx, "GET", "https://"+env.routeTable.Lb.Names[0]+env.routeTable.Lb.RoutePrefixes[0]+"/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
		GotConn: func(gotConnInfo httptrace.GotConnInfo) {
			conn = gotConnInfo.Conn
		},
	}))
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("lb status = %d %q, %v", response.StatusCode, body, err)
	}
	return response, conn
}

// connect sets only its own fields on each public server, the handler, the
// counting error log and the serving context, and hands every one to the
// nginx front to configure (LBALT-IMPL.md §7), so their limits are the nginx
// front's, header bytes at twice nginx's buffers included.
func TestFrontHttpServersAreConfiguredByTheNginxFront(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	configuredServers := env.front.recordedConfiguredServers()
	if len(configuredServers) != 2*len(env.routeTable.Interfaces) || !slices.Equal(configuredServers, env.frontServer.httpServers) {
		t.Fatalf("the nginx front configured %d servers, want every one of connect's %d", len(configuredServers), len(env.frontServer.httpServers))
	}
	httpSettings := env.settings.Front.Http
	for _, httpServer := range configuredServers {
		if httpServer.Handler == nil || httpServer.ErrorLog == nil || httpServer.BaseContext == nil || httpServer.BaseContext(nil) != env.frontServer.ctx {
			t.Fatal("a public server lacks connect's handler, error log or serving context")
		}
		if httpServer.ReadHeaderTimeout != httpSettings.ClientHeaderTimeout ||
			httpServer.IdleTimeout != httpSettings.KeepaliveTimeout ||
			httpServer.MaxHeaderBytes != 2*httpSettings.MaxRequestHeadersTotalBytes ||
			httpServer.ConnContext == nil {
			t.Fatalf("a public server's limits are not the nginx front's: header timeout %s, idle timeout %s, header bytes %d", httpServer.ReadHeaderTimeout, httpServer.IdleTimeout, httpServer.MaxHeaderBytes)
		}
	}
}

// Through connect's https socket and the real nginx front, the
// connection's 1000th request, nginx's keepalive_requests, is its last over
// h1: every response before it keeps the connection, its own carries
// Connection: close, and the next request opens another connection.
func TestFrontHttpsKeepaliveRequestsCloseTheConnectionOverH1(t *testing.T) {
	env := newTestNginxFrontEnv(t, nil, http.NotFoundHandler())
	if keepaliveRequests := env.settings.Front.Http.KeepaliveRequests; keepaliveRequests != 1000 {
		t.Fatalf("KeepaliveRequests %d, want nginx's 1000", keepaliveRequests)
	}
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	client := env.httpsClient(address, env.routeTable.Lb.Names[0], false, nil)

	var firstConn net.Conn
	for i := 1; i <= 1001; i += 1 {
		response, conn := getTestFrontLbStatus(t, env, client)
		switch {
		case i == 1:
			firstConn = conn
		case i <= 1000 && conn != firstConn:
			t.Fatalf("request %d went over another connection", i)
		case i == 1001 && conn == firstConn:
			t.Fatal("the 1001st request went over the closed connection")
		}
		if wantClose := i == 1000; response.Close != wantClose {
			t.Fatalf("request %d: Connection close %t, want %t", i, response.Close, wantClose)
		}
	}
}

// Over h2, the connection's 1000th stream is its last: every stream is
// served on the one connection, then the server sends a GOAWAY naming the
// 1000th as the last stream and closes the connection.
func TestFrontHttpsKeepaliveRequestsCloseTheConnectionOverH2(t *testing.T) {
	env := newTestNginxFrontEnv(t, nil, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	lbName := env.routeTable.Lb.Names[0]
	client := env.dialH2Client(env.bindAddress(address, frontPortHttps), lbName)
	for i := 1; i <= 1000; i += 1 {
		if status := client.get(lbName, env.routeTable.Lb.RoutePrefixes[0]+"/status"); status != strconv.Itoa(http.StatusOK) {
			t.Fatalf("stream %d: status %s", i, status)
		}
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testFrontH2StreamId(1000) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testFrontH2StreamId(1000))
	}
}

// A connection past nginx's keepalive_time is closed after its next request
// over h1. With a keepalive_time of 1 ns every connection is past it by its
// first request, which then carries Connection: close, and the next request
// opens another connection.
func TestFrontHttpsKeepaliveTimeClosesTheConnectionOverH1(t *testing.T) {
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.KeepaliveTime = time.Nanosecond
	}, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	client := env.httpsClient(address, env.routeTable.Lb.Names[0], false, nil)
	response, firstConn := getTestFrontLbStatus(t, env, client)
	if !response.Close {
		t.Fatal("a request past keepalive_time kept its connection")
	}
	if _, conn := getTestFrontLbStatus(t, env, client); conn == firstConn {
		t.Fatal("the next request went over the closed connection")
	}
}

// Over h2, the first stream past keepalive_time is the connection's last:
// the server sends a GOAWAY naming it and closes the connection.
func TestFrontHttpsKeepaliveTimeClosesTheConnectionOverH2(t *testing.T) {
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.KeepaliveTime = time.Nanosecond
	}, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	lbName := env.routeTable.Lb.Names[0]
	client := env.dialH2Client(env.bindAddress(address, frontPortHttps), lbName)
	if status := client.get(lbName, env.routeTable.Lb.RoutePrefixes[0]+"/status"); status != strconv.Itoa(http.StatusOK) {
		t.Fatalf("the stream past keepalive_time: status %s", status)
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testFrontH2StreamId(1) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testFrontH2StreamId(1))
	}
}
