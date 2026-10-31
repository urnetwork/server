// SDK-roster boundary controls use verified TLS and the actual full-tuple
// registry for the selected request. Other census rows are scalar models;
// this is component evidence, not a new public browsing qualification.
package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// Both TLS peers and the real registered origin owner are joined by cleanup.
type nativeBrowsingRosterControl struct {
	witness *nativeBrowsingColdLinkWitness
	client  *tls.Conn
	server  *tls.Conn
	owned   *sdkBrowsingOriginConn
}

// A positive handshake supplies verified chains and ALPN; the native registry
// supplies the registered controlled full tuple and the fixture's last valid role.
func newNativeBrowsingRosterControl(t *testing.T) *nativeBrowsingRosterControl {
	t.Helper()
	fixture := &sdkBrowsingSetupFixture{}
	scenario := newSdkBrowsingScenario(nil)
	native := &sdkBrowsingNative{fixture: fixture, scenario: scenario, upstreamConns: map[sdkBrowsingOriginTuple]*sdkBrowsingOriginConn{}, originCount: nativeBrowsingOriginCount}
	native.ports[0] = 51006
	scenario.fixture, scenario.native = fixture, native
	witness := newNativeBrowsingColdLinkWitness(scenario, &nativeBrowsingPressureObservation{}, false)
	witness.row.Html, witness.row.Assets, witness.row.ColdEdges, witness.row.VisitedDestinationEdges = 100, 400, 1, 98
	witness.row.CssOwnerRetained, witness.row.ColdTupleOwned = true, true
	for page := range witness.requests {
		witness.row.CombinedFirstByteNs[page] = 1
		for slot := range witness.requests[page] {
			witness.requests[page][slot] = nativeBrowsingOwnedColdRequestForTest(witness.requests[page][slot])
		}
	}
	t.Cleanup(witness.close)
	clientPipe, serverPipe := net.Pipe()
	raw := &sdkBrowsingTupleControlConn{Conn: clientPipe,
		localAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49006},
		remoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51006}}
	owned, err := native.ownOriginConnection(raw, len(fixture.providers)-1)
	if err != nil {
		clientPipe.Close()
		serverPipe.Close()
		t.Fatal("register actual origin owner: ", err)
	}
	serverConfig, clientConfig, err := nativeBrowsingTlsConfigsForOrigins(nativeBrowsingOriginCount + 1)
	if err != nil {
		owned.Close()
		serverPipe.Close()
		t.Fatal(err)
	}
	serverConfig = serverConfig.Clone()
	clientConfig = clientConfig.Clone()
	// One positive handshake has no ticket or resumption work to leave pending.
	serverConfig.SessionTicketsDisabled, clientConfig.SessionTicketsDisabled = true, true
	clientConfig.ServerName = "origin0.test"
	client, server := tls.Client(owned, clientConfig), tls.Server(serverPipe, serverConfig)
	t.Cleanup(func() {
		// Close the raw pipe before TLS Close so close_notify cannot wait on a
		// peer that has already completed its only positive handshake.
		owned.Close()
		serverPipe.Close()
		client.Close()
		server.Close()
		if native.originDials[owned.ordinal].Load() != 1 || native.originCloses[owned.ordinal].Load() != 1 {
			t.Error("actual origin ownership did not close once")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.HandshakeContext(ctx) }()
	clientErr := client.HandshakeContext(ctx)
	serverErr := <-serverResult
	if clientErr != nil || serverErr != nil {
		t.Fatalf("positive verified TLS handshake: client=%v server=%v", clientErr, serverErr)
	}
	control := &nativeBrowsingRosterControl{witness: witness, client: client, server: server, owned: owned}
	witness.requests[36][nativeBrowsingAssetCount] = nativeBrowsingColdRequest{}
	trace := witness.requestTraceWithContext(context.Background(), "https://origin0.test/page/36")
	trace.GotConn(httptrace.GotConnInfo{Conn: client})
	writer := httptest.NewRecorder()
	witness.wrapHandler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(nativeBrowsingHTML(36))
	})).ServeHTTP(writer, control.request("https://origin0.test/page/36"))
	request := witness.requests[36][nativeBrowsingAssetCount]
	if request.clientAcquisitions != 1 || request.clientSession != client || !request.verifiedClientTls ||
		request.serverAttempts != 1 || !request.serverOwned || request.serverTuple == "" || request.serverProvider != len(fixture.providers)-1 {
		t.Fatal("real TLS/owned tuple prerequisite failed")
	}
	return control
}

// The modeled server request carries the registered local endpoint and real TLS state
// and both sides of the same real registry tuple; nothing is source-guessed.
func (self *nativeBrowsingRosterControl) request(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = self.owned.LocalAddr().String()
	state := self.server.ConnectionState()
	request.TLS = &state
	return request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, self.owned.RemoteAddr()))
}

// One original CSS request uses the same real TLS and tuple, then a positive
// read retains an unfinished body behind its exact original gate/context.
func (self *nativeBrowsingRosterControl) css(t *testing.T) {
	t.Helper()
	witness := self.witness
	target := "https://origin0.test/asset/0/0"
	witness.requests[0][0] = nativeBrowsingColdRequest{bodyGate: witness.css.gate}
	trace := witness.requestTraceWithContext(context.Background(), target)
	trace.GetConn("origin0.test:443")
	trace.GotConn(httptrace.GotConnInfo{Conn: self.client, Reused: true})
	writer := httptest.NewRecorder()
	witness.wrapHandler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(nativeBrowsingAsset(0, 0))
	})).ServeHTTP(writer, self.request(target))
	progress := &witness.pressure.assets[0][0]
	progress.expectedBytes = int64(nativeBrowsingAssetBytes(0, 0))
	progress.started.Store(true)
	progress.bodyOpened.Store(true)
	reader := &nativeBrowsingProgressReader{reader: witness.wrapReader(context.Background(), target, bytes.NewReader(writer.Body.Bytes())), pressure: witness.pressure, progress: progress}
	buffer := make([]byte, nativeBrowsingHeldPrefixBytes)
	count, err := reader.Read(buffer)
	if count != len(buffer) || err != nil {
		t.Fatalf("positive original CSS prefix: count=%d err=%v", count, err)
	}
}

// OLD's exclusive6 bound rejects this verified-TLS/provider6 receipt. The
// configured SDK roster admits it without changing any TLS/tuple/census gate.
func TestNativeBrowsingColdLinkAcceptsConfiguredProviderTuple(t *testing.T) {
	control := newNativeBrowsingRosterControl(t)
	if err := control.witness.verdict(); err != nil {
		t.Fatalf("SDK_COLD_LINK_CONFIGURED_PROVIDER_REJECTED: provider=%d page=36 slot=%d err=%v", control.owned.ordinal, nativeBrowsingAssetCount, err)
	}
}

// The same stale bound must not reject an established original CSS owner.
func TestNativeBrowsingColdLinkHoldAcceptsConfiguredProviderTuple(t *testing.T) {
	control := newNativeBrowsingRosterControl(t)
	control.css(t)
	if err := control.witness.checkHold(&control.witness.css, nativeBrowsingColdCss); err != nil {
		t.Fatalf("SDK_COLD_LINK_HELD_PROVIDER_REJECTED: provider=%d err=%v", control.owned.ordinal, err)
	}
}

// Initial provider6 shares the old-quality refusal role; late4 and speed5 do
// not satisfy that premise. This is scalar receipt-policy coverage only.
func TestNativeBrowsingColdLinkCountsConfiguredQualityRefusal(t *testing.T) {
	for _, ordinal := range []int{0, 3, 6, 4, 5} {
		scenario := newSdkBrowsingScenario(nil)
		scenario.refusalCounts[ordinal], scenario.lateConstructedNs, scenario.lateApiVisibleNs, scenario.lateReady = 1, 1, 2, true
		witness := newNativeBrowsingColdLinkWitness(scenario, nil, false)
		t.Cleanup(witness.close)
		witness.row.ColdEdges, witness.row.ColdServerProvider, witness.row.ColdTupleOwned = 1, 4, true
		request := &witness.requests[1][nativeBrowsingAssetCount]
		request.clientSession, request.verifiedClientTls, request.clientAcquisitions, request.serverAttempts = &tls.Conn{}, true, 1, 1
		row, err := witness.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		want := "UNEXERCISED_ADMISSION_CHAIN"
		if ordinal == 0 || ordinal == 3 || ordinal == 6 {
			want = "LATE_PROVIDER_ADMITTED"
		}
		if row.AdmissionChain != want {
			t.Fatalf("SDK_COLD_LINK_INITIAL_QUALITY_REFUSAL_UNCOUNTED: provider=%d chain=%s want=%s", ordinal, row.AdmissionChain, want)
		}
		witness.close()
	}
}

// Bound changes do not admit foreign roles, missing/ambiguous TLS or tuple
// proof. Native registry misses remain failures, never guessed attribution.
func TestNativeBrowsingColdLinkRejectsForeignRosterAndBrokenProof(t *testing.T) {
	control := newNativeBrowsingRosterControl(t)
	control.css(t)
	witness := control.witness
	if err := witness.verdict(); err != nil {
		t.Fatal("valid full-proof premise: ", err)
	}
	baseline := witness.requests[36][nativeBrowsingAssetCount]
	for _, broken := range []string{"negative", "outside-roster", "missing-tls", "unverified-tls", "ambiguous-acquisition", "missing-server", "ambiguous-server", "empty-tuple", "unowned-tuple"} {
		request := baseline
		switch broken {
		case "negative":
			request.serverProvider = -1
		case "outside-roster":
			request.serverProvider = len(witness.row.Refusals)
		case "missing-tls":
			request.clientSession = nil
		case "unverified-tls":
			request.verifiedClientTls = false
		case "ambiguous-acquisition":
			request.clientAcquisitions++
		case "missing-server":
			request.serverAttempts = 0
		case "ambiguous-server":
			request.serverAttempts++
		case "empty-tuple":
			request.serverTuple = ""
		case "unowned-tuple":
			request.serverOwned = false
		}
		witness.requests[36][nativeBrowsingAssetCount] = request
		if err := witness.verdict(); err == nil {
			t.Errorf("invalid %s request passed", broken)
		} else if broken == "outside-roster" && (!strings.Contains(err.Error(), fmt.Sprintf("provider=%d provider_limit=%d", request.serverProvider, len(witness.row.Refusals))) || strings.Contains(err.Error(), baseline.serverTuple)) {
			t.Error("failure-only scalar diagnostic lost bound or exposed tuple identity: ", err)
		}
		witness.requests[36][nativeBrowsingAssetCount] = baseline
	}
	for _, ordinal := range []int{-1, len(witness.row.Refusals)} {
		request := witness.requests[0][0]
		witness.requests[0][0].serverProvider = ordinal
		if witness.checkHold(&witness.css, nativeBrowsingColdCss) == nil {
			t.Errorf("invalid held provider%d passed", ordinal)
		}
		witness.requests[0][0] = request
	}
	for _, missing := range []string{"local-endpoint", "foreign-tuple"} {
		witness.firstErr = nil
		witness.requests[36][nativeBrowsingAssetCount] = baseline
		witness.requests[36][nativeBrowsingAssetCount].serverAttempts = 0
		witness.requests[36][nativeBrowsingAssetCount].serverTuple = ""
		witness.requests[36][nativeBrowsingAssetCount].serverOwned = false
		request := control.request("https://origin0.test/page/36")
		if missing == "local-endpoint" {
			request = request.WithContext(context.Background())
		} else {
			request.RemoteAddr = "127.0.0.1:49007"
		}
		witness.wrapHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), request)
		if witness.firstErr == nil || witness.requests[36][nativeBrowsingAssetCount].serverOwned || witness.verdict() == nil {
			t.Errorf("invalid %s server tuple passed", missing)
		}
	}
}
