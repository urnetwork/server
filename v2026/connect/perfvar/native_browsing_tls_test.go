// Authenticated synthetic origins default to HTTP/1.1 over the same TUN dial.
// Only the explicit inner-H2 row changes ALPN; nil TLS keeps plain workloads.
package perfvar

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Certificates and keys belong only to this synthetic fixture lifetime.
func nativeBrowsingTlsConfigs() (*tls.Config, *tls.Config, error) {
	return nativeBrowsingTlsConfigsForOrigins(nativeBrowsingOriginCount)
}

// The cold-link comparison alone owns one additional verified origin name.
func nativeBrowsingTlsConfigsForOrigins(origins int) (*tls.Config, *tls.Config, error) {
	if origins != nativeBrowsingOriginCount && origins != nativeBrowsingOriginCount+1 {
		return nil, nil, errors.New("unowned synthetic origin count")
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic browsing fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return nil, nil, err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, origins)
	for index := range names {
		names[index] = fmt.Sprintf("origin%d.test", index)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, DNSNames: names, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, certificate, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certificateBytes, caBytes}, PrivateKey: serverKey}}, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}},
		&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}, nil
}

// H1 connection and idle limits remain the original four-per-origin geometry.
func newNativeBrowsingInnerTransport(dial func(context.Context, string, string) (net.Conn, error), settings nativeBrowsingRunSettings) *http.Transport {
	transport := &http.Transport{MaxConnsPerHost: nativeBrowsingConnectionsPerOrigin, MaxIdleConns: nativeBrowsingOriginCount * nativeBrowsingConnectionsPerOrigin, MaxIdleConnsPerHost: nativeBrowsingConnectionsPerOrigin, DialContext: dial}
	if settings.originClientTlsForTest != nil {
		transport.TLSClientConfig = settings.originClientTlsForTest.Clone()
		if settings.innerHttp2ForTest {
			transport.Protocols = new(http.Protocols)
			transport.Protocols.SetHTTP2(true)
			transport.TLSClientConfig.NextProtos = []string{"h2"}
		} else {
			transport.ForceAttemptHTTP2 = false
			transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
	}
	return transport
}

// The server accepts the same handler and listener; only TLS wraps the socket.
func newNativeBrowsingInnerServer(handler http.Handler, listener net.Listener, settings nativeBrowsingRunSettings) (*http.Server, net.Listener) {
	server := &http.Server{Handler: handler}
	if settings.originServerTlsForTest == nil {
		return server, listener
	}
	serverTls := settings.originServerTlsForTest.Clone()
	if settings.innerHttp2ForTest {
		server.Protocols = new(http.Protocols)
		server.Protocols.SetHTTP2(true)
		serverTls.NextProtos = []string{"h2"}
		server.TLSConfig = serverTls
	}
	return server, tls.NewListener(listener, serverTls)
}

// Every actual HTTPS response proves its selected protocol and certificate.
func nativeBrowsingVerifyTlsResponse(response *http.Response, settings nativeBrowsingRunSettings) error {
	if settings.originClientTlsForTest == nil {
		return nil
	}
	wantProtocol := 1
	if settings.innerHttp2ForTest {
		wantProtocol = 2
	}
	if response.TLS == nil || len(response.TLS.VerifiedChains) == 0 || response.ProtoMajor != wantProtocol ||
		settings.innerHttp2ForTest && response.TLS.NegotiatedProtocol != "h2" {
		return errors.New("synthetic HTTPS origin did not verify TLS and the selected HTTP protocol")
	}
	return nil
}

// Successful verification and two independent failures use real TLS handshakes.
func TestNativeBrowsingTlsRequiresOwnedRootAndName(t *testing.T) {
	serverConfig, clientConfig, err := nativeBrowsingTlsConfigs()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 1 {
			t.Error("synthetic origin changed HTTP protocol")
		}
		_, _ = writer.Write([]byte("synthetic browsing origin"))
	}))
	server.TLS = serverConfig
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	for _, test := range []struct {
		name       string
		root       bool
		serverName string
		wantError  bool
	}{{"owned", true, "origin0.test", false}, {"wrong-root", false, "origin0.test", true}, {"wrong-name", true, "unowned.test", true}} {
		config := clientConfig.Clone()
		config.ServerName = test.serverName
		if !test.root {
			config.RootCAs = x509.NewCertPool()
		}
		transport := newNativeBrowsingInnerTransport((&net.Dialer{}).DialContext, nativeBrowsingRunSettings{originClientTlsForTest: config})
		client := &http.Client{Transport: transport}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if response != nil {
			_, readErr := io.Copy(io.Discard, response.Body)
			err = errors.Join(err, readErr, response.Body.Close())
		}
		transport.CloseIdleConnections()
		if (err != nil) != test.wantError {
			t.Fatalf("%s verification error=%v, expected error=%t", test.name, err, test.wantError)
		}
	}
}
