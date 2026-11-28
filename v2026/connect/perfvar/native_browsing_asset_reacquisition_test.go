package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeBrowsingAssetCensusAcceptsReusedH1RetryBeforeHandler(t *testing.T) {
	testNativeBrowsingAssetCensusReusedH1Retry(t, false)
}

func TestNativeBrowsingAssetCensusRejectsReusedH1RetryAfterHandler(t *testing.T) {
	testNativeBrowsingAssetCensusReusedH1Retry(t, true)
}

// Both cases issue one logical GET, write it twice, and receive its exact
// body once. The loss differs only in whether the first asset handler ran.
func testNativeBrowsingAssetCensusReusedH1Retry(t *testing.T, loseAfterHandler bool) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const page, asset = 20, 0
	const assetPath = "/asset/20/0"
	expected := nativeBrowsingAsset(page, asset)
	var pages [nativeBrowsingPageCount]*nativeBrowsingPageState
	pages[page] = newNativeBrowsingPageState()
	pressure := sdkBrowsingBodyOwnershipControl()
	// The other 399 slots have the existing completed control's receipts. Start
	// the measured slot with only its real, initially empty server ledger.
	pressure.assets[page][asset] = nativeBrowsingRequestProgress{serverReceipt: pages[page].serverReceipts[asset]}
	progress := &pressure.assets[page][asset]
	var live, peak, handlerCalls atomic.Int64
	server := &nativeBrowsingAssetServer{workCtx: ctx, pages: &pages, pressure: pressure, liveHandlers: &live, peakHandlers: &peak}
	originTls, clientTls, err := nativeBrowsingTlsConfigs()
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prime" {
			_, _ = io.WriteString(w, "prime")
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != assetPath || r.Proto != "HTTP/1.1" {
			t.Errorf("unexpected asset request %s %s %s", r.Method, r.URL.Path, r.Proto)
			http.NotFound(w, r)
			return
		}
		if handlerCalls.Add(1) == 1 && loseAfterHandler {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack first response: %v", err)
				return
			}
			defer conn.Close()
			discarded := httptest.NewRecorder()
			server.serveAsset(discarded, r, page, asset)
			if !bytes.Equal(discarded.Body.Bytes(), expected) {
				t.Error("first handler did not render the complete asset")
			}
			return
		}
		server.serveAsset(w, r, page, asset)
	}))
	origin.TLS = originTls
	origin.StartTLS()
	clientTls.ServerName = "origin0.test"
	settings := nativeBrowsingRunSettings{originClientTlsForTest: clientTls}
	client := &http.Client{Transport: newNativeBrowsingInnerTransport(nil, settings)}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		origin.Close()
		server.workers.Wait()
	})
	primeRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/prime", nil)
	if err != nil {
		t.Fatal(err)
	}
	var primed atomic.Pointer[tls.Conn]
	primeRequest = primeRequest.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			conn, _ := info.Conn.(*tls.Conn)
			primed.Store(conn)
		},
	}))
	prime, err := client.Do(primeRequest)
	if err != nil {
		t.Fatal(err)
	}
	primeBody, readErr := io.ReadAll(prime.Body)
	closeErr := prime.Body.Close()
	if readErr != nil || closeErr != nil || prime.StatusCode != http.StatusOK || prime.Proto != "HTTP/1.1" || string(primeBody) != "prime" || primed.Load() == nil ||
		prime.TLS == nil || prime.TLS.NegotiatedProtocol != "http/1.1" || nativeBrowsingVerifyTlsResponse(prime, settings) != nil {
		t.Fatalf("prime TLS/H1 connection: proto=%s body=%q read=%v close=%v", prime.Proto, primeBody, readErr, closeErr)
	}

	var checkouts, writes, firstByteCallbacks atomic.Int64
	var first, second atomic.Pointer[tls.Conn]
	var firstReused, secondReused atomic.Bool
	var responseAttempt atomic.Int64
	secondWrite := make(chan struct{})
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			conn, _ := info.Conn.(*tls.Conn)
			switch checkouts.Add(1) {
			case 1:
				first.Store(conn)
				firstReused.Store(info.Reused)
			case 2:
				second.Store(conn)
				secondReused.Store(info.Reused)
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			switch writes.Add(1) {
			case 1:
				// Request.write invokes WroteRequest before the HTTP/1 transport
				// flushes this small bodyless GET. Close only its reused socket.
				if conn := first.Load(); !loseAfterHandler && info.Err == nil && conn != nil && conn == primed.Load() && firstReused.Load() {
					if err := conn.NetConn().Close(); err != nil {
						t.Errorf("close first socket: %v", err)
					}
				}
			case 2:
				close(secondWrite)
			}
		},
		GotFirstResponseByte: func() {
			firstByteCallbacks.Add(1)
			responseAttempt.Store(checkouts.Load())
		},
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, origin.URL+assetPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), progress.retryTrace()))
	progress.started.Store(true)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	progress.bodyOpened.Store(true)
	progress.responseStatus.Store(int64(response.StatusCode))
	body, readErr := io.ReadAll(response.Body)
	closeErr = response.Body.Close()
	progress.bodyCloseError.Store(closeErr != nil)
	progress.bodyClosed.Store(true)
	progress.finished.Store(true)
	if err := nativeBrowsingResponseError(response.StatusCode, body, expected, readErr); err != nil || closeErr != nil ||
		response.TLS == nil || response.TLS.NegotiatedProtocol != "http/1.1" || nativeBrowsingVerifyTlsResponse(response, settings) != nil {
		t.Fatalf("validate replayed response: proto=%s body=%v close=%v", response.Proto, err, closeErr)
	}
	if checkouts.Load() != 2 {
		t.Fatalf("the original GET used %d checkouts", checkouts.Load())
	}
	select {
	case <-secondWrite:
	case <-ctx.Done():
		t.Fatalf("second write missing: checkouts=%d writes=%d", checkouts.Load(), writes.Load())
	}
	wantHandlers := int64(1)
	if loseAfterHandler {
		wantHandlers = 2
	}
	receipt, err := pages[page].waitAssetServer(ctx, asset)
	server.workers.Wait()
	if err != nil || handlerCalls.Load() != wantHandlers || receipt.started != wantHandlers ||
		receipt.finished != wantHandlers || receipt.canceled != 0 || live.Load() != 0 {
		t.Fatalf("asset handler custody: calls=%d receipt=%+v live=%d err=%v", handlerCalls.Load(), receipt, live.Load(), err)
	}
	census := pressure.assetOwnershipSnapshot()
	if first.Load() != primed.Load() || !firstReused.Load() || second.Load() == nil || second.Load() == first.Load() || secondReused.Load() ||
		writes.Load() != 2 || firstByteCallbacks.Load() != 1 || responseAttempt.Load() != 2 ||
		census.ClientAcquisitions != 401 || census.ClientReacquisitions != 1 || census.ClientRequestWrites != 401 || census.ClientRequestWriteErrors != 0 ||
		census.RetriedAssets != 1 || census.ServerAttempts != 399+wantHandlers || census.ServerFinishedAttempts != 399+wantHandlers {
		t.Fatalf("HTTP/1 transport and handler receipts: checkouts=%d writes=%d first_bytes=%d response_attempt=%d census=%+v",
			checkouts.Load(), writes.Load(), firstByteCallbacks.Load(), responseAttempt.Load(), census)
	}
	err = nativeBrowsingAssetBodyOwnershipError(pressure)
	if loseAfterHandler && err == nil {
		t.Fatal("two actual asset handlers passed the ownership census")
	}
	if !loseAfterHandler && err != nil {
		t.Fatalf("NATIVE_BROWSING_SINGLE_HANDLER_H1_RETRY: %v", err)
	}
}
