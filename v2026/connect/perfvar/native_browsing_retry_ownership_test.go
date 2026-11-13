// Logical assets retain bounded counters for every transport replay. Completion
// is a state transition rather than one buffered send per handler attempt.
package perfvar

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The fixed per-asset slot retains no body, connection, request or attempt list.
type nativeBrowsingServerReceipt struct {
	started  int64
	finished int64
	canceled int64
}

// The logical client has finished or was canceled before this is called, so it
// cannot initiate another replay. Earlier attempts still disposing must join.
func (self *nativeBrowsingPageState) waitAssetServer(ctx context.Context, asset int) (nativeBrowsingServerReceipt, error) {
	for {
		receipt, changed := self.serverReceipts[asset].Get()
		if receipt.started != 0 && receipt.finished == receipt.started {
			return receipt, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return receipt, ctx.Err()
		}
	}
}

// One fixture owner joins every real asset handler. A repeated GET regenerates
// the same exact body; the existing client retains response validation duties.
type nativeBrowsingAssetServer struct {
	workCtx      context.Context
	pages        *[nativeBrowsingPageCount]*nativeBrowsingPageState
	streaming    bool
	pressure     *nativeBrowsingPressureObservation
	liveHandlers *atomic.Int64
	peakHandlers *atomic.Int64
	workers      sync.WaitGroup
}

// Takes no pooled payload. The attempt is owned through its completion receipt,
// including cancellation, a short response write or a transport-level replay.
func (self *nativeBrowsingAssetServer) serveAsset(writer http.ResponseWriter, request *http.Request, page, asset int) {
	self.workers.Add(1)
	defer self.workers.Done()
	state := self.pages[page]
	duplicate := false
	state.serverReceipts[asset].Update(func(receipt nativeBrowsingServerReceipt) nativeBrowsingServerReceipt {
		if self.pressure == nil && receipt.started != 0 {
			duplicate = true
			return receipt
		}
		receipt.started++
		return receipt
	})
	if duplicate {
		http.Error(writer, "duplicate asset request", http.StatusConflict)
		return
	}
	live := self.liveHandlers.Add(1)
	for old := self.peakHandlers.Load(); old < live && !self.peakHandlers.CompareAndSwap(old, live); old = self.peakHandlers.Load() {
	}
	var progress *nativeBrowsingRequestProgress
	if self.pressure != nil {
		progress = &self.pressure.assets[page][asset]
		progress.serverStarted.Store(true)
	}
	canceled := false
	defer func() {
		self.liveHandlers.Add(-1)
		state.serverReceipts[asset].Update(func(receipt nativeBrowsingServerReceipt) nativeBrowsingServerReceipt {
			receipt.finished++
			if canceled {
				receipt.canceled++
			}
			return receipt
		})
		if progress != nil {
			progress.serverFinished.Store(true)
		}
	}()
	body := nativeBrowsingAsset(page, asset)
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.Header().Set("Content-Type", [nativeBrowsingAssetCount]string{"text/css", "application/javascript", "image/png", "image/jpeg"}[asset])
	if asset < 2 {
		count, err := writer.Write(body)
		progress.recordServerWrite(len(body), count, err)
		return
	}
	canceled = nativeBrowsingWriteAssetSuffix(writer, request.Context(), self.workCtx, state, body, asset, self.streaming, progress)
}

// GetConn counts actual RoundTrip attempts, including automatic reacquisition.
// WroteRequest may report a failed write that the origin never received.
func (self *nativeBrowsingRequestProgress) retryTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { self.connectionAcquisitions.Add(1) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			self.requestWrites.Add(1)
			if info.Err != nil {
				self.requestWriteErrors.Add(1)
			}
		},
	}
}

// Old trace controls can still set their direct disposal booleans. Real driver
// slots use exact attempt counters and never call one finished attempt "done".
func (self *nativeBrowsingRequestProgress) serverOwnership() (nativeBrowsingServerReceipt, bool) {
	if self.serverReceipt != nil {
		return self.serverReceipt.Value(), true
	}
	return nativeBrowsingServerReceipt{}, false
}

// A reused connection loses its first asset response after the real handler
// finishes. Go's Transport replays that same GET, without another logical fetch.
// The origin must regenerate the exact body and preserve both attempt receipts.
func TestNativeBrowsingAssetGetRetryPreservesBodyAndAttempts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var pages [nativeBrowsingPageCount]*nativeBrowsingPageState
	pages[0] = newNativeBrowsingPageState()
	var live, peak atomic.Int64
	pressure := &nativeBrowsingPressureObservation{}
	progress := &pressure.assets[0][0]
	progress.serverReceipt = pages[0].serverReceipts[0]
	assetServer := &nativeBrowsingAssetServer{workCtx: ctx, pages: &pages,
		streaming: true, pressure: pressure, liveHandlers: &live, peakHandlers: &peak}
	var originAttempts atomic.Int64
	firstResponse := make(chan []byte, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/prime" {
			_, _ = writer.Write([]byte("primed"))
			return
		}
		if originAttempts.Add(1) == 1 {
			connection, _, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack controlled response loss: %v", err)
				return
			}
			defer connection.Close()
			recorder := httptest.NewRecorder()
			assetServer.serveAsset(recorder, request, 0, 0)
			firstResponse <- bytes.Clone(recorder.Body.Bytes())
			return
		}
		assetServer.serveAsset(writer, request, 0, 0)
	}))
	transport := &http.Transport{MaxConnsPerHost: 1}
	client := &http.Client{Transport: transport}
	t.Cleanup(func() {
		cancel()
		transport.CloseIdleConnections()
		origin.Close()
		assetServer.workers.Wait()
	})
	prime, err := client.Get(origin.URL + "/prime")
	if err != nil {
		t.Fatal(err)
	}
	_, primeReadErr := io.Copy(io.Discard, prime.Body)
	primeCloseErr := prime.Body.Close()
	if primeReadErr != nil || primeCloseErr != nil {
		t.Fatalf("prime reusable connection: read=%v close=%v", primeReadErr, primeCloseErr)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/asset/0/0", nil)
	if err != nil {
		t.Fatal(err)
	}
	var firstReused atomic.Bool
	var acquisitions atomic.Int64
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if acquisitions.Add(1) == 1 {
				firstReused.Store(info.Reused)
			}
		},
	}))
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), progress.retryTrace()))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	progress.responseStatus.Store(int64(response.StatusCode))
	expected := nativeBrowsingAsset(0, 0)
	if validationErr := nativeBrowsingResponseError(response.StatusCode, body, expected, readErr); validationErr != nil || closeErr != nil {
		t.Fatalf("one logical GET lost its valid replay: status=%d bytes=%d origin_attempts=%d acquisitions=%d validation=%v close=%v",
			response.StatusCode, len(body), originAttempts.Load(), progress.connectionAcquisitions.Load(), validationErr, closeErr)
	}
	receipt, err := pages[0].waitAssetServer(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !firstReused.Load() || acquisitions.Load() != 2 || originAttempts.Load() != 2 ||
		receipt.started != 2 || receipt.finished != 2 || receipt.canceled != 0 || live.Load() != 0 ||
		!bytes.Equal(<-firstResponse, expected) {
		t.Fatalf("controlled GET replay or handler disposal was not exact: first_reused=%t acquisitions=%d origin_attempts=%d receipt=%+v live=%d",
			firstReused.Load(), acquisitions.Load(), originAttempts.Load(), receipt, live.Load())
	}
	ownership := pressure.assetOwnershipSnapshot()
	if ownership.ServerStarted != 1 || ownership.ServerFinished != 1 || ownership.ServerAttempts != 2 ||
		ownership.ServerFinishedAttempts != 2 || ownership.ServerActiveAttempts != 0 ||
		ownership.ClientAcquisitions != 2 || ownership.ClientReacquisitions != 1 ||
		ownership.ClientRequestWrites != 2 || ownership.RetriedAssets != 1 || ownership.InvalidResponseStatuses != 0 {
		t.Fatalf("logical and attempt receipts were collapsed or lost: %+v", ownership)
	}
	t.Logf("one logical GET: status=%d bytes=%d server_attempts=%d finished=%d acquisitions=%d reacquisitions=%d",
		response.StatusCode, len(body), receipt.started, receipt.finished, ownership.ClientAcquisitions, ownership.ClientReacquisitions)
}

// One canceled attempt cannot complete the logical server owner while two
// retries still hold their prefix. Their completions and overlap receipts must
// coalesce without blocked channel sends, then all three handlers must join.
func TestNativeBrowsingAssetRetriesKeepEveryHandlerJoined(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var pages [nativeBrowsingPageCount]*nativeBrowsingPageState
		state := newNativeBrowsingPageState()
		pages[0] = state
		defer state.releaseAssets()
		var live, peak atomic.Int64
		pressure := &nativeBrowsingPressureObservation{}
		progress := &pressure.assets[0][2]
		progress.serverReceipt = state.serverReceipts[2]
		assetServer := &nativeBrowsingAssetServer{workCtx: ctx, pages: &pages,
			pressure: pressure, liveHandlers: &live, peakHandlers: &peak}
		var cancelAttempts [3]context.CancelFunc
		var recorders [3]*httptest.ResponseRecorder
		for attempt := range cancelAttempts {
			requestCtx, cancelAttempt := context.WithCancel(ctx)
			cancelAttempts[attempt] = cancelAttempt
			defer cancelAttempt()
			recorder := httptest.NewRecorder()
			recorders[attempt] = recorder
			request := httptest.NewRequest(http.MethodGet, "http://origin.example/asset/0/2", nil).WithContext(requestCtx)
			go assetServer.serveAsset(recorder, request, 0, 2)
		}
		synctest.Wait()
		receipt := state.serverReceipts[2].Value()
		if receipt.started != 3 || receipt.finished != 0 || live.Load() != 3 || peak.Load() != 3 {
			t.Fatalf("three real handlers did not reach their prefix gates: receipt=%+v live=%d peak=%d", receipt, live.Load(), peak.Load())
		}
		select {
		case asset := <-state.held:
			if asset != 2 {
				t.Fatal("overlap receipt belongs to another logical asset")
			}
		default:
			t.Fatal("real prefix writers published no overlap receipt")
		}
		select {
		case <-state.held:
			t.Fatal("transport retries published duplicate logical overlap receipts")
		default:
		}
		cancelAttempts[0]()
		synctest.Wait()
		progress.cancelRequested.Store(true)
		before := pressure.assetOwnershipSnapshot()
		if before.ServerFinished != 0 || before.ServerAttempts != 3 || before.ServerFinishedAttempts != 1 ||
			before.ServerCanceledAttempts != 1 || before.ServerActiveAttempts != 2 || before.CanceledServerPending != 1 || ctx.Err() != nil {
			t.Fatalf("one child completion hid its live siblings: %+v", before)
		}
		type waitResult struct {
			receipt nativeBrowsingServerReceipt
			err     error
		}
		joined := make(chan waitResult, 1)
		go func() {
			receipt, err := state.waitAssetServer(ctx, 2)
			joined <- waitResult{receipt: receipt, err: err}
		}()
		synctest.Wait()
		select {
		case <-joined:
			t.Fatal("logical handler join completed while two attempts were held")
		default:
		}
		state.releaseAssets()
		synctest.Wait()
		result := <-joined
		assetServer.workers.Wait()
		after := pressure.assetOwnershipSnapshot()
		if result.err != nil || result.receipt.started != 3 || result.receipt.finished != 3 || result.receipt.canceled != 1 ||
			live.Load() != 0 || after.ServerActiveAttempts != 0 || after.ServerFinished != 1 || after.CanceledServerPending != 0 {
			t.Fatalf("handler completion lost an attempt or left a blocked sender: result=%+v live=%d ownership=%+v", result, live.Load(), after)
		}
		expected := nativeBrowsingAsset(0, 2)
		if !bytes.Equal(recorders[0].Body.Bytes(), expected[:nativeBrowsingHeldPrefixBytes]) ||
			!bytes.Equal(recorders[1].Body.Bytes(), expected) || !bytes.Equal(recorders[2].Body.Bytes(), expected) {
			t.Fatal("retry or cancellation changed the exact synthetic response bytes")
		}
		t.Logf("three handler attempts: finished=%d canceled=%d live=%d logical_overlap_receipts=1",
			result.receipt.finished, result.receipt.canceled, live.Load())
	})
}
