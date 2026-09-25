package acceptance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/proxy/flowtrace"
)

// This fixture uses a real verified TLS client/server exchange. Its local
// stream reader supplies synthetic return-envelope metadata (not a live
// provider) for each actual socket read; no payload bytes enter the recorder.
type flowTraceReadConnection struct {
	net.Conn
	recorder *flowtrace.Recorder
	provider connect.Id
	sequence uint32
}

func (c *flowTraceReadConnection) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	if n > 0 {
		local := c.LocalAddr().(*net.TCPAddr).AddrPort()
		origin := c.RemoteAddr().(*net.TCPAddr).AddrPort()
		packet := make([]byte, 40+n)
		packet[0], packet[9] = 0x45, 6
		binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
		copy(packet[12:16], origin.Addr().AsSlice())
		copy(packet[16:20], local.Addr().AsSlice())
		binary.BigEndian.PutUint16(packet[20:22], origin.Port())
		binary.BigEndian.PutUint16(packet[22:24], local.Port())
		binary.BigEndian.PutUint32(packet[24:28], c.sequence)
		packet[32], packet[33] = 0x50, 0x18
		c.sequence += uint32(n)
		c.recorder.Return(connect.SourceId(c.provider), [][]byte{packet}, time.Now())
	}
	return n, err
}

func TestFlowTraceCorrelatesRealTLSRejectionWithoutRetryOrTrustBypass(t *testing.T) {
	trusted, root := diagnosticTLSCertificate(t, "trusted root")
	rejected, _ := diagnosticTLSCertificate(t, "None")
	var handshakes, requests atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	origin.Config.ErrorLog = log.New(io.Discard, "", 0)
	origin.TLS = &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if handshakes.Add(1) == 1 {
			return &trusted, nil
		}
		return &rejected, nil
	}}
	origin.StartTLS()
	defer origin.Close()
	originAddress := netip.MustParseAddrPort(origin.Listener.Addr().String())
	recorder, err := flowtrace.New(originAddress.Port(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	session := recorder.Cursor(time.Now()).Session
	provider := connect.Id{42}
	var apiRequests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiRequests.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-UR-Flow-Trace") != session {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		snapshot := recorder.Cursor(time.Now())
		if r.URL.Query().Get("cursor_only") != "1" {
			after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
			if err != nil {
				t.Errorf("invalid after cursor: %v", err)
			}
			afterDropped, err := strconv.ParseUint(r.URL.Query().Get("after_dropped"), 10, 64)
			if err != nil {
				t.Errorf("invalid dropped cursor: %v", err)
			}
			snapshot = recorder.Snapshot(after, afterDropped, time.Now())
		}
		_ = json.NewEncoder(w).Encode(snapshot)
	}))
	defer api.Close()
	pool := x509.NewCertPool()
	pool.AddCert(root)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "validation.example"}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		recorder.Dial("http", connection, time.Now())
		return &flowTraceReadConnection{Conn: connection, recorder: recorder, provider: provider}, nil
	}
	defer transport.CloseIdleConnections()
	config := &proxyConfigResult{flowTrace: &flowTraceClient{url: api.URL, token: "fixture-token", session: session, client: api.Client()}}
	successes, err := probeHTTPSCampaign(
		context.Background(), "HTTP CONNECT", origin.URL, withFlowTrace(config, "http", transport),
		time.Second, 5*time.Second, time.Second,
		func(context.Context, time.Duration) error { return nil }, nil, nil,
	)
	if successes != 1 || requests.Load() != 1 || handshakes.Load() != 2 || apiRequests.Load() != 3 {
		t.Fatalf("successes=%d requests=%d handshakes=%d API=%d; want one verified response, one terminal rejection, two cursors and one failure read", successes, requests.Load(), handshakes.Load(), apiRequests.Load())
	}
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	if !errors.As(err, &verification) || !errors.As(err, &authority) {
		t.Fatalf("normal TLS rejection was replaced: %v", err)
	}
	for _, evidence := range []string{
		"sustained request 1/5 failed after 1 successful requests",
		"peer_certs=2 verified_chains=0",
		"validation.example>None/", "None>None/",
		"flow_provenance={scope=actual-return-flow origin=" + originAddress.String(),
		"provider_count=1 first_providers=[" + flowtrace.ProviderAlias(session, provider) + "]",
		"origin " + originAddress.String(),
	} {
		if !strings.Contains(err.Error(), evidence) {
			t.Errorf("rejection lacks %q: %v", evidence, err)
		}
	}
	for _, forbidden := range []string{"fixture-token", session, provider.String()} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("diagnostic leaked fixture identity %q", forbidden)
		}
	}
}

func TestFlowTraceUnavailableDoesNotChangeTargetVerdict(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not armed", http.StatusGone)
	}))
	defer api.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	client := origin.Client()
	client.Transport = withFlowTrace(&proxyConfigResult{flowTrace: &flowTraceClient{
		url: api.URL, client: api.Client(), session: "unavailable",
	}}, "http", client.Transport)
	if err := probeHTTPSRequest(context.Background(), client, origin.URL); err != nil {
		t.Fatalf("missing diagnostics changed verified target success: %v", err)
	}
}

func TestFlowTraceWillNotSendCredentialsToUnencryptedAPI(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer api.Close()
	if _, err := startFlowTrace(context.Background(), &proxyConfigResult{
		APIBaseURL: api.URL, AuthToken: "must-not-send",
	}, "https://validation.example/"); err == nil || requests.Load() != 0 {
		t.Fatalf("unencrypted API requests=%d err=%v", requests.Load(), err)
	}
}
