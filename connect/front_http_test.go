package connect

// The front's tcp sockets: TLS through the nginx front's one config, its
// HTTPS and HTTP handlers per interface, and the error log that keeps client
// addresses out of the logs.

import (
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
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
