// Real TLS/h2 controls exercise stream isolation and canceled unread bodies.
// Positive barriers establish each ordering; deadlines only bound test owners.
package perfvar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

type nativeBrowsingHttp2Control struct {
	ctx                context.Context
	cancel             context.CancelFunc
	client             *http.Client
	settings           nativeBrowsingRunSettings
	accepted           atomic.Int64
	tail               chan struct{}
	tailOnce           sync.Once
	serverCanceled     chan struct{}
	serverCanceledOnce sync.Once
	handlerWorkers     sync.WaitGroup
}

// Both synthetic names still use ordinary certificate hostname verification.
// Only this control's private loopback listener is an allowed dial destination.
func newNativeBrowsingHttp2Control(t *testing.T, serverHttp2 bool, trustRoot bool) *nativeBrowsingHttp2Control {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sdk.DefaultDeviceLocalSettings().SendTimeout)
	self := &nativeBrowsingHttp2Control{ctx: ctx, cancel: cancel, tail: make(chan struct{}), serverCanceled: make(chan struct{})}
	serverTls, clientTls, err := nativeBrowsingTlsConfigs()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !trustRoot {
		clientTls.RootCAs = x509.NewCertPool()
	}
	self.settings = nativeBrowsingRunSettings{maxLivePages: 4, originServerTlsForTest: serverTls, originClientTlsForTest: clientTls, innerHttp2ForTest: true}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		self.handlerWorkers.Add(1)
		defer self.handlerWorkers.Done()
		if serverHttp2 && (request.ProtoMajor != 2 || request.TLS == nil || request.TLS.NegotiatedProtocol != "h2") {
			http.Error(writer, "HTTP/2 required", http.StatusBadRequest)
			return
		}
		if request.URL.Path != "/asset/0/1" {
			_, _ = writer.Write(nativeBrowsingHTML(1))
			return
		}
		body := nativeBrowsingAsset(0, 1)
		if _, err := writer.Write(body[:len(body)-nativeBrowsingHeldPrefixBytes]); err != nil {
			return
		}
		writer.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
			self.serverCanceledOnce.Do(func() { close(self.serverCanceled) })
			return
		case <-self.tail:
			_, _ = writer.Write(body[len(body)-nativeBrowsingHeldPrefixBytes:])
		}
	})
	serverSettings := self.settings
	serverSettings.innerHttp2ForTest = serverHttp2
	server, secureListener := newNativeBrowsingInnerServer(handler, listener, serverSettings)
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			self.accepted.Add(1)
		}
	}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); _ = server.Serve(secureListener) }()
	transport := newNativeBrowsingInnerTransport(func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || host != "origin1.test" && host != "unowned.test" {
			return nil, errors.New("unowned HTTP/2 control destination")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	}, self.settings)
	self.client = &http.Client{Transport: transport}
	t.Cleanup(func() {
		self.cancel()
		self.tailOnce.Do(func() { close(self.tail) })
		transport.CloseIdleConnections()
		_ = server.Close()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), sdk.DefaultDeviceLocalSettings().SendTimeout)
		defer joinCancel()
		joined := make(chan struct{})
		go func() { <-serverDone; self.handlerWorkers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-joinCtx.Done():
			t.Error("HTTP/2 control server/handlers did not join")
		}
	})
	return self
}

// Work contexts are canceled before t.Cleanup runs. Joining the owner gets
// its own existing five-second budget, independent of request cancellation.
func nativeBrowsingHttp2OwnerJoinContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), sdk.DefaultDeviceLocalSettings().SendTimeout)
}

// The returned session is the actual GotConn receipt. Response validation
// still uses the same shared TLS/protocol check and exact body validator.
func (self *nativeBrowsingHttp2Control) fetch(ctx context.Context, target string, expected []byte, gate *nativeBrowsingHttp2BodyGate, session **tls.Conn) nativeBrowsingAssetResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nativeBrowsingAssetResult{err: err}
	}
	request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if secure, ok := info.Conn.(*tls.Conn); ok {
			*session = secure
		}
	}}))
	response, err := self.client.Do(request)
	if err != nil {
		return nativeBrowsingAssetResult{err: err}
	}
	defer response.Body.Close()
	if err := nativeBrowsingVerifyTlsResponse(response, self.settings); err != nil {
		return nativeBrowsingAssetResult{err: err}
	}
	var reader io.Reader = response.Body
	if gate != nil {
		reader = &nativeBrowsingHttp2BodyReader{ctx: ctx, reader: reader, gate: gate}
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, int64(len(expected)+1)))
	return nativeBrowsingAssetResult{status: response.StatusCode, bytes: len(body), err: nativeBrowsingResponseError(response.StatusCode, body, expected, readErr)}
}

// One actual body is paused after a real 1-KiB read while the origin keeps its
// stream open. A same-session HTML must validate before either hold is released.
func TestNativeBrowsingHttp2HeldBodyAllowsSameSessionHtml(t *testing.T) {
	self := newNativeBrowsingHttp2Control(t, true, true)
	gate := newNativeBrowsingHttp2BodyGate()
	jsCtx, cancelJs := context.WithCancel(self.ctx)
	var jsSession *tls.Conn
	jsResult := make(chan nativeBrowsingAssetResult, 1)
	jsDone := make(chan struct{})
	go func() {
		defer close(jsDone)
		jsResult <- self.fetch(jsCtx, "https://origin1.test/asset/0/1", nativeBrowsingAsset(0, 1), gate, &jsSession)
	}()
	t.Cleanup(func() {
		cancelJs()
		gate.release()
		joinCtx, joinCancel := nativeBrowsingHttp2OwnerJoinContext()
		defer joinCancel()
		select {
		case <-jsDone:
		case <-joinCtx.Done():
			t.Error("HTTP/2 JS owner did not join")
		}
	})
	select {
	case <-gate.paused:
	case <-self.ctx.Done():
		t.Fatal("HTTP/2 JS body never paused")
	}
	var htmlSession *tls.Conn
	html := self.fetch(self.ctx, "https://origin1.test/page/1", nativeBrowsingHTML(1), nil, &htmlSession)
	if html.err != nil || html.bytes != nativeBrowsingHTMLBytes || jsSession == nil || htmlSession != jsSession || self.accepted.Load() != 1 {
		t.Fatalf("held stream blocked or replaced its session: html=%+v accepted=%d", html, self.accepted.Load())
	}
	select {
	case <-gate.released:
		t.Fatal("body released before actual same-session HTML completed")
	default:
	}
	gate.release()
	self.tailOnce.Do(func() { close(self.tail) })
	select {
	case result := <-jsResult:
		if result.err != nil || result.bytes != nativeBrowsingAssetBytes(0, 1) {
			t.Fatalf("original JS body changed: %+v", result)
		}
	case <-self.ctx.Done():
		t.Fatal("released original JS did not finish")
	}
}

// A canceled child wakes its own paused reader. The real origin observes
// cancellation, and a later HTML still acquires the original TLS/TCP session.
func TestNativeBrowsingHttp2CanceledBodyKeepsSameSessionHtml(t *testing.T) {
	self := newNativeBrowsingHttp2Control(t, true, true)
	gate := newNativeBrowsingHttp2BodyGate()
	jsCtx, cancelJs := context.WithCancel(self.ctx)
	var jsSession *tls.Conn
	jsResult := make(chan nativeBrowsingAssetResult, 1)
	jsDone := make(chan struct{})
	go func() {
		defer close(jsDone)
		jsResult <- self.fetch(jsCtx, "https://origin1.test/asset/0/1", nativeBrowsingAsset(0, 1), gate, &jsSession)
	}()
	t.Cleanup(func() {
		cancelJs()
		gate.release()
		joinCtx, joinCancel := nativeBrowsingHttp2OwnerJoinContext()
		defer joinCancel()
		select {
		case <-jsDone:
		case <-joinCtx.Done():
			t.Error("canceled HTTP/2 JS owner did not join")
		}
	})
	select {
	case <-gate.paused:
	case <-self.ctx.Done():
		t.Fatal("HTTP/2 JS body never paused")
	}
	cancelJs()
	select {
	case result := <-jsResult:
		if !errors.Is(result.err, context.Canceled) || errors.Is(result.err, errNativeBrowsingResponseMismatch) || result.bytes != nativeBrowsingHeldPrefixBytes {
			t.Fatalf("canceled original JS lost byte/error authority: %+v", result)
		}
	case <-self.ctx.Done():
		t.Fatal("child cancellation waited for navigation release")
	}
	select {
	case <-gate.released:
		t.Fatal("child cancellation consumed an invented navigation release")
	default:
	}
	select {
	case <-self.serverCanceled:
	case <-self.ctx.Done():
		t.Fatal("origin did not observe stream cancellation")
	}
	var htmlSession *tls.Conn
	html := self.fetch(self.ctx, "https://origin1.test/page/1", nativeBrowsingHTML(1), nil, &htmlSession)
	if html.err != nil || jsSession == nil || htmlSession != jsSession || self.accepted.Load() != 1 {
		t.Fatalf("canceled body retired a healthy shared session: html=%+v accepted=%d", html, self.accepted.Load())
	}
}

// Receiving a buffered result does not join its worker. Go cancels this
// test's work context before cleanup, while the explicit release still owns
// the worker's final step. Cleanup must retain a live, independent join budget.
func TestNativeBrowsingHttp2CleanupJoinsCanceledOwner(t *testing.T) {
	workCtx := t.Context()
	jsResult := make(chan nativeBrowsingAssetResult, 1)
	jsDone := make(chan struct{})
	workCanceled := make(chan struct{})
	releaseOwner := make(chan struct{})
	t.Cleanup(func() {
		joinCtx, joinCancel := nativeBrowsingHttp2OwnerJoinContext()
		defer joinCancel()
		if !errors.Is(workCtx.Err(), context.Canceled) {
			t.Error("work context was not canceled before cleanup")
		}
		if joinCtx.Err() != nil {
			t.Error("owner join inherited the canceled work lifetime")
		}
		select {
		case <-workCanceled:
		case <-joinCtx.Done():
			t.Error("worker did not observe work cancellation")
		}
		select {
		case <-jsDone:
			t.Error("buffered result incorrectly proved owner completion")
		default:
		}
		close(releaseOwner)
		select {
		case <-jsDone:
		case <-joinCtx.Done():
			t.Error("canceled work owner did not join within its independent budget")
		}

		// An already-finished owner must also have only its completion arm
		// ready, even though the same work context is already canceled.
		finished := make(chan struct{})
		close(finished)
		finishedCtx, finishedCancel := nativeBrowsingHttp2OwnerJoinContext()
		defer finishedCancel()
		if finishedCtx.Err() != nil {
			t.Error("finished owner inherited the canceled work lifetime")
		}
		select {
		case <-finished:
		case <-finishedCtx.Done():
			t.Error("finished owner lost to canceled work")
		}
	})
	go func() {
		defer close(jsDone)
		jsResult <- nativeBrowsingAssetResult{status: http.StatusOK}
		<-workCtx.Done()
		close(workCanceled)
		<-releaseOwner
	}()
	select {
	case result := <-jsResult:
		if result.status != http.StatusOK {
			t.Fatal("worker did not publish its buffered result")
		}
	case <-workCtx.Done():
		t.Fatal("work canceled before the buffered result")
	}
}

// Actual TLS handshakes reject an unowned root/name and a server that cannot
// negotiate h2; no response flag or protocol assertion is mocked.
func TestNativeBrowsingHttp2RejectsFallbackRootAndName(t *testing.T) {
	for _, test := range []struct {
		name        string
		serverHttp2 bool
		trustRoot   bool
		host        string
	}{
		{name: "http1-only", trustRoot: true, host: "origin1.test"},
		{name: "wrong-root", serverHttp2: true, host: "origin1.test"},
		{name: "wrong-name", serverHttp2: true, trustRoot: true, host: "unowned.test"},
	} {
		self := newNativeBrowsingHttp2Control(t, test.serverHttp2, test.trustRoot)
		var session *tls.Conn
		result := self.fetch(self.ctx, "https://"+test.host+"/page/1", nativeBrowsingHTML(1), nil, &session)
		if result.err == nil {
			t.Fatalf("%s accepted an unverified/non-h2 origin", test.name)
		}
		self.cancel()
	}
}
