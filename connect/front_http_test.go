package connect

// The front's tcp sockets: TLS through the nginx front's one config, its
// HTTPS and HTTP handlers per interface, and the error log that keeps client
// addresses out of the logs.

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/net/http2"
)

// TCP 443 terminates TLS with the front's config and serves the interface's
// HTTPS handler, over HTTP/1.1 and over h2 where the name's ALPN allows it;
// a connect name negotiates HTTP/1.1 only, so its websocket and H1+ carriers
// can hijack. TCP 80 serves the interface's HTTP handler.
func TestFrontHttpsServesTheInterfaceHandlerOverTls(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	for _, routeInterface := range env.routeTable.Interfaces {
		for _, family := range []string{frontFamilyIpv4, frontFamilyIpv6} {
			address, ok := env.address(routeInterface.Name, family)
			if !ok {
				continue
			}
			cases := []struct {
				serverName string
				http2      bool
				want       string
			}{
				{serverName: testFrontApiName, http2: false, want: "https " + routeInterface.Name + " HTTP/1.1 " + testFrontApiName},
				{serverName: testFrontApiName, http2: true, want: "https " + routeInterface.Name + " HTTP/2.0 " + testFrontApiName},
				{serverName: testFrontForwardedName, http2: true, want: "https " + routeInterface.Name + " HTTP/2.0 " + testFrontForwardedName},
				{serverName: testFrontConnectName, http2: true, want: "https " + routeInterface.Name + " HTTP/1.1 " + testFrontConnectName},
			}
			for _, c := range cases {
				client := env.httpsClient(address, c.serverName, c.http2, nil)
				response, err := client.Get("https://" + c.serverName + "/")
				if err != nil {
					t.Fatalf("%s %s: %v", address, c.serverName, err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || string(body) != c.want {
					t.Fatalf("%s %s h2=%t: %q %v, want %q", address, c.serverName, c.http2, body, err, c.want)
				}
			}

			response, err := http.Get("http://" + env.bindAddress(address, frontPortHttp) + "/")
			if err != nil {
				t.Fatalf("%s http: %v", address, err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if want := "http " + routeInterface.Name + " false"; err != nil || string(body) != want {
				t.Fatalf("%s http: %q %v, want %q", address, body, err, want)
			}
		}
	}
}

// Every https socket serves the front's one TLS config itself, never a
// clone, so a session ticket issued on one interface's socket resumes on
// another's (D6c). A clone per socket would initialize ticket keys of its own.
func TestFrontHttpsResumesSessionsAcrossSockets(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	addressA, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	addressB, _ := env.address(testFrontInterfaceB, frontFamilyIpv6)
	sessionCache := &testFrontSessionCache{
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
		stored:             make(chan struct{}, 4),
	}

	first, err := env.httpsClient(addressA, testFrontApiName, false, sessionCache).Get("https://" + testFrontApiName + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	testFrontWait(t, sessionCache.stored, "a session ticket")
	if first.TLS.DidResume {
		t.Fatal("the first connection resumed")
	}

	second, err := env.httpsClient(addressB, testFrontApiName, false, sessionCache).Get("https://" + testFrontApiName + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, second.Body)
	second.Body.Close()
	if !second.TLS.DidResume {
		t.Fatal("a ticket from one interface's socket did not resume on another's")
	}
}

// net/http quotes the client's address in what it logs, so the front counts
// each message by kind instead, and logs a handler panic only with the
// address cut out.
func TestFrontHttpErrorLogKeepsClientAddressesOut(t *testing.T) {
	cases := []struct {
		message string
		kind    string
	}{
		{message: "http: TLS handshake error from 203.0.113.7:4242: EOF\n", kind: frontHttpErrorTlsHandshake},
		{message: "http: panic serving [2001:db8::7]:4242: synthetic\ngoroutine 1 [running]:\n", kind: frontHttpErrorPanic},
		{message: "http: Accept error: synthetic; retrying in 5ms\n", kind: frontHttpErrorAccept},
		{message: "http2: server: error reading preface from client 198.51.100.7:4242: EOF\n", kind: frontHttpErrorOther},
	}
	errorLog := &frontHttpErrorLog{portKind: frontPortHttps}
	for _, c := range cases {
		counter := frontHttpServerErrorsCounter.WithLabelValues(frontPortHttps, c.kind)
		before := testutil.ToFloat64(counter)
		if n, err := errorLog.Write([]byte(c.message)); err != nil || n != len(c.message) {
			t.Fatalf("write = %d %v", n, err)
		}
		if after := testutil.ToFloat64(counter); after != before+1 {
			t.Fatalf("%q counted %v times as %s", c.message, after-before, c.kind)
		}
	}

	scrubbed := scrubFrontHttpPanic("http: panic serving [2001:db8::7]:4242: synthetic value\ngoroutine 1 [running]:\n")
	if strings.Contains(scrubbed, "2001:db8::7") || strings.Contains(scrubbed, "4242") {
		t.Fatalf("the scrubbed panic still quotes the address: %q", scrubbed)
	}
	if !strings.Contains(scrubbed, "synthetic value") || !strings.Contains(scrubbed, "goroutine 1") {
		t.Fatalf("the scrubbed panic lost its value or stack: %q", scrubbed)
	}
	if scrubbed := scrubFrontHttpPanic("http: panic serving 203.0.113.7:4242"); strings.Contains(scrubbed, "203.0.113.7") {
		t.Fatalf("a panic message without a value still quotes the address: %q", scrubbed)
	}
}

// One message a public server wrote to its error log, as the front's
// observer saw it.
type testFrontHttpError struct {
	portKind string
	kind     string
	logged   string
}

// The messages the env's public servers write to their error logs, as they
// are counted.
func observeTestFrontHttpErrors(env *testFrontEnv) chan testFrontHttpError {
	httpErrors := make(chan testFrontHttpError, 64)
	env.settings.httpErrorObserverForTest = func(portKind string, kind string, logged string) {
		select {
		case httpErrors <- testFrontHttpError{portKind: portKind, kind: kind, logged: logged}:
		default:
		}
	}
	return httpErrors
}

// The next message a public server wrote to its error log.
func nextTestFrontHttpError(t testing.TB, httpErrors chan testFrontHttpError, what string) testFrontHttpError {
	t.Helper()
	select {
	case httpError := <-httpErrors:
		return httpError
	case <-time.After(testFrontProgressTimeout):
		t.Fatalf("%s never reached the front's error log", what)
		return testFrontHttpError{}
	}
}

// What the process's standard logger writes, which net/http uses for a server
// without an error log of its own.
type testFrontStandardLog struct {
	stateLock sync.Mutex
	buffer    bytes.Buffer
}

// Captures the standard logger until the test ends.
func captureTestFrontStandardLog(t testing.TB) *testFrontStandardLog {
	standardLog := &testFrontStandardLog{}
	writer := log.Writer()
	log.SetOutput(standardLog)
	t.Cleanup(func() {
		log.SetOutput(writer)
	})
	return standardLog
}

// io.Writer
func (self *testFrontStandardLog) Write(p []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.buffer.Write(p)
}

// Everything written so far.
func (self *testFrontStandardLog) String() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.buffer.String()
}

// A failed TLS handshake on a public https socket goes to the front's error
// log, which counts it and logs nothing: net/http quotes the client's address
// in it, and without the front's log writes it to the standard logger.
func TestFrontHttpsCountsAFailedHandshakeWithoutLoggingTheAddress(t *testing.T) {
	standardLog := captureTestFrontStandardLog(t)
	var httpErrors chan testFrontHttpError
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		httpErrors = observeTestFrontHttpErrors(env)
	})
	counter := frontHttpServerErrorsCounter.WithLabelValues(frontPortHttps, frontHttpErrorTlsHandshake)
	before := testutil.ToFloat64(counter)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := net.Dial("tcp", env.bindAddress(address, frontPortHttps))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	clientAddress := conn.LocalAddr().String()
	conn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	// plain HTTP on the TLS port fails the handshake
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", testFrontApiName)
	testFrontReadEnd(t, conn, "the failed handshake's connection")

	httpError := nextTestFrontHttpError(t, httpErrors, "the failed handshake")
	if want := (testFrontHttpError{portKind: frontPortHttps, kind: frontHttpErrorTlsHandshake}); httpError != want {
		t.Fatalf("the failed handshake was %+v, want %+v", httpError, want)
	}
	if after := testutil.ToFloat64(counter); after < before+1 {
		t.Fatalf("the failed handshake counted %v times", after-before)
	}
	if text := standardLog.String(); strings.Contains(text, clientAddress) || strings.Contains(text, "TLS handshake error") {
		t.Fatalf("the standard logger got the failed handshake: %q", text)
	}
}

// A handler panic on a public socket is logged without the client's address,
// which net/http's message quotes.
func TestFrontHttpsLogsAHandlerPanicWithoutTheAddress(t *testing.T) {
	const panicValue = "synthetic front handler panic"
	standardLog := captureTestFrontStandardLog(t)
	var httpErrors chan testFrontHttpError
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		httpErrors = observeTestFrontHttpErrors(env)
		env.front.httpsHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic(panicValue)
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontApiName, []string{"http/1.1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	clientAddress := tlsConn.LocalAddr().String()
	tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", testFrontApiName)
	testFrontReadEnd(t, tlsConn, "the panicking request's connection")

	httpError := nextTestFrontHttpError(t, httpErrors, "the handler panic")
	if httpError.portKind != frontPortHttps || httpError.kind != frontHttpErrorPanic {
		t.Fatalf("the handler panic was %+v", httpError)
	}
	if !strings.Contains(httpError.logged, panicValue) || strings.Contains(httpError.logged, clientAddress) {
		t.Fatalf("the handler panic was logged as %q", httpError.logged)
	}
	if text := standardLog.String(); strings.Contains(text, clientAddress) {
		t.Fatalf("the standard logger got the client's address: %q", text)
	}
}

// A connection that never sends a ClientHello on 443, or a request on 80, is
// closed at nginx's client header timeout, which bounds the TLS handshake
// too, so it can't hold a descriptor and a goroutine.
func TestFrontHttpServersCloseAStalledConnectionAtTheHeaderTimeout(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.ClientHeaderTimeout = 100 * time.Millisecond
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, portKind := range []string{frontPortHttps, frontPortHttp} {
		conn, err := net.Dial("tcp", env.bindAddress(address, portKind))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
		testFrontReadEnd(t, conn, "a stalled "+portKind+" connection")
	}
}

// An idle keepalive connection is closed at nginx's keepalive timeout.
func TestFrontHttpsClosesAnIdleKeepaliveConnectionAtTheKeepaliveTimeout(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.KeepaliveTimeout = 100 * time.Millisecond
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontApiName, []string{"http/1.1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", testFrontApiName)
	reader := bufio.NewReader(tlsConn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.Close {
		t.Fatal("the connection was not kept alive")
	}
	testFrontReadEnd(t, reader, "the idle keepalive connection")
}

// Request line and headers over nginx's header buffers in all, plus
// net/http's 4 KiB slack, are refused by net/http with 431 before the nginx
// front sees them (the parity difference newHttpServer documents); under the
// limit a request reaches the front.
func TestFrontHttpsRefusesHeadersOverTheTotalLimit(t *testing.T) {
	const headerBytes = 4 * 1024
	reached := make(chan string, 4)
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.MaxRequestHeadersTotalBytes = headerBytes
		env.front.httpsHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached <- r.URL.Path
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	request := func(path string, fillCount int) int {
		t.Helper()
		tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontApiName, []string{"http/1.1"}))
		if err != nil {
			t.Fatal(err)
		}
		defer tlsConn.Close()
		tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
		var text strings.Builder
		fmt.Fprintf(&text, "GET %s HTTP/1.1\r\nHost: %s\r\n", path, testFrontApiName)
		// each header well under nginx's 8 KiB line
		for i := range fillCount {
			fmt.Fprintf(&text, "X-Fill-%d: %s\r\n", i, strings.Repeat("a", 1024))
		}
		text.WriteString("\r\n")
		io.WriteString(tlsConn, text.String())
		response, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}

	if status := request("/under", 2); status != http.StatusOK {
		t.Fatalf("headers under the limit: status %d", status)
	}
	if path := <-reached; path != "/under" {
		t.Fatalf("reached %s", path)
	}
	if status := request("/over", 16); status != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("headers over the limit: status %d, want 431", status)
	}
	select {
	case path := <-reached:
		t.Fatalf("headers over the limit reached the front at %s", path)
	default:
	}
}

// An h2 connection is limited to nginx's concurrent streams.
func TestFrontHttpsLimitsH2ConcurrentStreams(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.Http2MaxConcurrentStreams = 3
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontApiName, []string{"h2"}))
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	if _, err := io.WriteString(tlsConn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(tlsConn, tlsConn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	frame, err := framer.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	settings, ok := frame.(*http2.SettingsFrame)
	if !ok {
		t.Fatalf("the server's first frame is %T", frame)
	}
	if maxConcurrentStreams, ok := settings.Value(http2.SettingMaxConcurrentStreams); !ok || maxConcurrentStreams != 3 {
		t.Fatalf("the server allows %d concurrent streams (set %t), want 3", maxConcurrentStreams, ok)
	}
}

// Requests end with the front: a hijacked request, which net/http no longer
// cancels, sees its context end once the front's context does, so the nginx
// front's forwarded websockets end with it.
func TestFrontHttpsRequestContextsEndWithTheFront(t *testing.T) {
	hijacked := make(chan struct{})
	requestDone := make(chan struct{})
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				close(hijacked)
				<-r.Context().Done()
				close(requestDone)
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontConnectName, []string{"http/1.1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	fmt.Fprintf(tlsConn, "GET /upgrade HTTP/1.1\r\nHost: %s\r\n\r\n", testFrontConnectName)
	testFrontWait(t, hijacked, "the hijack")

	env.frontServer.cancel()
	testFrontWait(t, requestDone, "the hijacked request's end with the front")
}

// Every request reaches the nginx front, `OPTIONS *` included, which
// net/http would otherwise answer itself.
func TestFrontHttpsServesOptionsAsteriskThroughTheFront(t *testing.T) {
	requestUris := make(chan string, 1)
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestUris <- r.RequestURI
				io.WriteString(w, "front")
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontApiName, []string{"http/1.1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	fmt.Fprintf(tlsConn, "OPTIONS * HTTP/1.1\r\nHost: %s\r\n\r\n", testFrontApiName)
	response, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "front" {
		t.Fatalf("OPTIONS * answered %d %q %v, not by the front", response.StatusCode, body, err)
	}
	select {
	case requestUri := <-requestUris:
		if requestUri != "*" {
			t.Fatalf("the front saw %q", requestUri)
		}
	default:
		t.Fatal("OPTIONS * did not reach the front")
	}
}
