// HTTP over in-memory connections forces every runnable body to drain before
// a delayed tap. No real network, sleeps, retries or finalizer timing is used.
package perfvar

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Closing the listener joins Serve independently of every HTTP body handler.
// The connection handoff and all pipe I/O are durable synctest barriers.
type nativeBrowsingOverlapListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

// Only in-memory connections supplied by the fixture's dialer are accepted.
func (self *nativeBrowsingOverlapListener) Accept() (net.Conn, error) {
	select {
	case connection := <-self.connections:
		return connection, nil
	case <-self.closed:
		return nil, net.ErrClosed
	}
}

// Repeated cleanup cannot close a listener or completion gate twice.
func (self *nativeBrowsingOverlapListener) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

// A documentation address describes the listener; no socket is opened.
func (self *nativeBrowsingOverlapListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 80}
}

// The existing page factory, HTTP handler, reader, handoff and server receipts
// are composed here. The fixture owns all HTTP client/server workers and
// retains only the original fixed request/result slots.
type nativeBrowsingOverlapFixture struct {
	ctx             context.Context
	cancel          context.CancelFunc
	settings        nativeBrowsingRunSettings
	pressure        *nativeBrowsingPressureObservation
	observation     *nativeBrowsingObservation
	pages           [nativeBrowsingPageCount]*nativeBrowsingPageState
	assetServer     *nativeBrowsingAssetServer
	server          *http.Server
	serverDone      chan error
	transport       *http.Transport
	client          *http.Client
	clients         sync.WaitGroup
	liveHandlers    atomic.Int64
	peakHandlers    atomic.Int64
	liveAssets      atomic.Int64
	peakAssets      atomic.Int64
	htmlRequests    [nativeBrowsingPageCount]atomic.Int64
	assetRequests   [nativeBrowsingPageCount][nativeBrowsingAssetCount]atomic.Int64
	completedAssets int
	closeOnce       sync.Once
	closeErr        error
}

// Both workload versions use the same real HTTP/1 transport and body consumer.
func newNativeBrowsingOverlapFixture(t *testing.T, revised bool) *nativeBrowsingOverlapFixture {
	ctx, cancel := context.WithCancel(context.Background())
	pressure := &nativeBrowsingPressureObservation{}
	settings := nativeBrowsingRunSettings{maxLivePages: 4, pressure: pressure, nextNavigationOverlap: revised}
	if err := settings.validate(); err != nil {
		cancel()
		t.Fatal(err)
	}
	fixture := &nativeBrowsingOverlapFixture{ctx: ctx, cancel: cancel, settings: settings, pressure: pressure,
		observation: &nativeBrowsingObservation{Pressure: pressure}, pages: settings.newPages(), serverDone: make(chan error, 1)}
	fixture.assetServer = &nativeBrowsingAssetServer{workCtx: ctx, pages: &fixture.pages, streaming: true,
		pressure: pressure, liveHandlers: &fixture.liveHandlers, peakHandlers: &fixture.peakHandlers}
	listener := &nativeBrowsingOverlapListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) < 2 {
			http.NotFound(writer, request)
			return
		}
		page, err := strconv.Atoi(parts[1])
		if err != nil || page < 0 || nativeBrowsingPageCount <= page {
			http.NotFound(writer, request)
			return
		}
		if parts[0] == "page" && len(parts) == 2 {
			fixture.htmlRequests[page].Add(1)
			_, _ = writer.Write(nativeBrowsingHTML(page))
			return
		}
		if parts[0] != "asset" || len(parts) != 3 {
			http.NotFound(writer, request)
			return
		}
		asset, err := strconv.Atoi(parts[2])
		if err != nil || asset < 0 || nativeBrowsingAssetCount <= asset {
			http.NotFound(writer, request)
			return
		}
		fixture.assetRequests[page][asset].Add(1)
		fixture.assetServer.serveAsset(writer, request, page, asset)
	})
	fixture.server, _ = newNativeBrowsingInnerServer(handler, listener, settings)
	go func() { fixture.serverDone <- fixture.server.Serve(listener) }()
	fixture.transport = newNativeBrowsingInnerTransport(func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		clientConnection, serverConnection := net.Pipe()
		select {
		case listener.connections <- serverConnection:
			return clientConnection, nil
		case <-listener.closed:
			_ = clientConnection.Close()
			_ = serverConnection.Close()
			return nil, net.ErrClosed
		case <-dialCtx.Done():
			_ = clientConnection.Close()
			_ = serverConnection.Close()
			return nil, dialCtx.Err()
		}
	}, settings)
	fixture.client = &http.Client{Transport: fixture.transport}
	return fixture
}

// The ordinary progress reader observes actual HTTP body reads. Fetch failure
// signals the same gate as the real driver and never fabricates body progress.
func (self *nativeBrowsingOverlapFixture) fetch(ctx context.Context, page, asset int) (result nativeBrowsingAssetResult) {
	expected := nativeBrowsingHTML(page)
	var progress *nativeBrowsingRequestProgress
	if 0 <= asset {
		expected = nativeBrowsingAsset(page, asset)
		progress = &self.pressure.assets[page][asset]
		progress.started.Store(true)
		defer func() {
			progress.finished.Store(true)
			progress.nextNavigation.signalBodyProgress()
		}()
	}
	self.pressure.beginFetch(len(expected))
	defer self.pressure.activeRequests.Add(-1)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, nativeBrowsingURL(page, asset), nil)
	if err != nil {
		return nativeBrowsingAssetResult{err: err}
	}
	if progress != nil {
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), progress.retryTrace()))
	}
	response, err := self.client.Do(request)
	if err != nil {
		return nativeBrowsingAssetResult{err: err}
	}
	if progress != nil {
		progress.bodyOpened.Store(true)
		progress.responseStatus.Store(int64(response.StatusCode))
	}
	defer func() {
		closeErr := response.Body.Close()
		if progress != nil {
			progress.bodyClosed.Store(true)
			progress.bodyCloseError.Store(closeErr != nil)
		}
	}()
	reader := &nativeBrowsingProgressReader{reader: response.Body, pressure: self.pressure, progress: progress}
	body, readErr := io.ReadAll(io.LimitReader(reader, int64(len(expected)+1)))
	return nativeBrowsingAssetResult{bytes: len(body), status: response.StatusCode,
		err: nativeBrowsingResponseError(response.StatusCode, body, expected, readErr)}
}

// A handoff immediately starts the real next HTML request. Tests settle every
// earlier body before calling this method, just as a delayed pre-tap barrier can.
func (self *nativeBrowsingOverlapFixture) navigate(t *testing.T, page int) {
	if _, err := self.settings.startPressureNavigation(self.observation, &self.pages, page); err != nil {
		t.Fatal(err)
	}
	result := self.fetch(self.ctx, page, -1)
	if result.err != nil || result.bytes != nativeBrowsingHTMLBytes {
		t.Fatalf("page %d HTML: status=%d bytes=%d err=%v", page, result.status, result.bytes, result.err)
	}
	self.observation.Pages[page].CompletedBytes = result.bytes
}

// Each request keeps its original cancellation owner. The optional canceled
// image never starts a live HTTP request, exercising failure before first read.
func (self *nativeBrowsingOverlapFixture) startAssets(t *testing.T, page int, cancelImageBeforeRead bool) {
	assets, next, err := nativeBrowsingReferences(nativeBrowsingHTML(page))
	if err != nil || next != nativeBrowsingURL(page+1, -1) {
		t.Fatalf("page %d references: %v", page, err)
	}
	state := self.pages[page]
	for asset, target := range assets {
		if target != nativeBrowsingURL(page, asset) {
			t.Fatalf("page %d asset %d changed its original reference", page, asset)
		}
		assetCtx, cancel := context.WithCancel(self.ctx)
		state.assetCancel[asset] = cancel
		progress := &self.pressure.assets[page][asset]
		progress.expectedBytes = int64(nativeBrowsingAssetBytes(page, asset))
		if cancelImageBeforeRead && asset == 2 {
			progress.cancelRequested.Store(true)
			cancel()
		}
		self.clients.Add(1)
		live := self.liveAssets.Add(1)
		for old := self.peakAssets.Load(); old < live && !self.peakAssets.CompareAndSwap(old, live); old = self.peakAssets.Load() {
		}
		go func() {
			defer self.clients.Done()
			defer self.liveAssets.Add(-1)
			state.clientDone[asset] <- self.fetch(assetCtx, page, asset)
		}()
	}
}

// These are the original two server-prefix receipts. They are deliberately
// consumed only after all runnable body work has settled.
func (self *nativeBrowsingOverlapFixture) takeHeld(t *testing.T, page int) {
	var seen [nativeBrowsingAssetCount]bool
	for range 2 {
		select {
		case asset := <-self.pages[page].held:
			if asset < 2 || seen[asset] {
				t.Fatalf("page %d duplicate or unrelated prefix token: %d", page, asset)
			}
			seen[asset] = true
		default:
			t.Fatalf("page %d did not publish both original prefix tokens", page)
		}
	}
}

// Retirement joins the original client and every server attempt. It releases
// only the old retirement gate; a missing next-tap handoff remains observable.
func (self *nativeBrowsingOverlapFixture) finishPage(t *testing.T, page int) {
	state := self.pages[page]
	state.releaseAssets()
	row := &self.observation.Pages[page]
	for asset := range nativeBrowsingAssetCount {
		result := <-state.clientDone[asset]
		progress := &self.pressure.assets[page][asset]
		if result.err != nil {
			if !progress.cancelRequested.Load() || !nativeBrowsingStreamingCancellationValid(result, nativeBrowsingAssetBytes(page, asset)) {
				t.Fatalf("page %d asset %d invalid original cancellation: bytes=%d err=%v", page, asset, result.bytes, result.err)
			}
			row.CanceledAssets++
			row.CanceledPrefixBytes += result.bytes
		} else {
			if result.bytes != nativeBrowsingAssetBytes(page, asset) {
				t.Fatalf("page %d asset %d lost its complete body", page, asset)
			}
			row.CompletedBytes += result.bytes
			if progress.cancelRequested.Load() {
				self.pressure.CompletedDespiteCancellation++
			}
		}
		receipt, err := state.waitAssetServer(self.ctx, asset)
		if err != nil || receipt.started != 1 || receipt.finished != 1 {
			t.Fatalf("page %d asset %d did not join its original handler: receipt=%+v err=%v", page, asset, receipt, err)
		}
		self.pressure.ServerObservedCancellation += int(receipt.canceled)
		state.assetCancel[asset]()
		self.completedAssets++
	}
	self.observation.CompletedNavigations++
	self.observation.CanceledAssets += row.CanceledAssets
	self.observation.CanceledPrefixBytes += row.CanceledPrefixBytes
}

// Cancellation and gate release precede every client/handler/Serve join.
// Repeated fixture cleanup has the same idempotence as the page owners.
func (self *nativeBrowsingOverlapFixture) close() error {
	self.closeOnce.Do(func() {
		self.cancel()
		nativeBrowsingReleasePages(self.pages[:])
		self.transport.CloseIdleConnections()
		self.closeErr = self.server.Close()
		synctest.Wait()
		self.clients.Wait()
		self.assetServer.workers.Wait()
		if err := <-self.serverDone; !errors.Is(err, http.ErrServerClosed) {
			self.closeErr = errors.Join(self.closeErr, err)
		}
		synctest.Wait()
	})
	return self.closeErr
}

// The original streaming writer can finish all four bodies while both prefix
// tokens remain queued. Delaying the tap deterministically exposes zero overlap.
func TestNativeBrowsingDelayedTapExposesHistoricalPrefixReceipts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeBrowsingOverlapFixture(t, false)
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		fixture.navigate(t, 0)
		fixture.startAssets(t, 0, false)
		synctest.Wait()
		fixture.takeHeld(t, 0)
		for asset := range nativeBrowsingAssetCount {
			progress := &fixture.pressure.assets[0][asset]
			if !progress.finished.Load() || progress.receivedBytes.Load() != progress.expectedBytes {
				t.Fatalf("original streaming asset %d did not finish before the delayed tap", asset)
			}
		}
		fixture.navigate(t, 1)
		row := fixture.pressure.Navigations[1]
		if fixture.pressure.WorkloadVersion != "streaming-v1" || fixture.observation.OverlapProofs != 0 ||
			row.UnfinishedPriorPages != 0 || row.UnfinishedPriorAssets != 0 || row.PriorRemainingBodyBytes != 0 || row.ActiveRequestsAtStart != 0 {
			t.Fatalf("historical tokens were mistaken for unfinished bodies: %+v", row)
		}
		fixture.finishPage(t, 0)
	})
}

// All other assets and every historical token may finish before each tap. The
// identified original image alone must retain positive received and remaining
// bytes at all 99 real HTML handoffs, then complete exactly or accept its
// original cancellation. The final page has no completion latch.
func TestNativeBrowsingNextNavigationImageRetainsBodyForAll99Edges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeBrowsingOverlapFixture(t, true)
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		livePages := map[int]bool{}
		for page := range nativeBrowsingPageCount {
			fixture.navigate(t, page)
			if page != 0 {
				row := fixture.pressure.Navigations[page]
				if row.UnfinishedPriorPages != 1 || row.UnfinishedPriorAssets != 1 ||
					row.PriorRemainingBodyBytes != nativeBrowsingHeldPrefixBytes || row.ActiveRequestsAtStart != 1 {
					t.Fatalf("page %d lost its original unfinished image at HTML handoff: %+v", page, row)
				}
			}
			fixture.startAssets(t, page, false)
			livePages[page] = true
			if len(livePages) > fixture.settings.maxLivePages || fixture.peakAssets.Load() > int64(fixture.settings.maxLivePages*nativeBrowsingAssetCount) {
				t.Fatalf("page %d exceeded the original four-page ownership bound", page)
			}
			if retire := nativeBrowsingRetirePage(page, fixture.settings.maxLivePages); 0 <= retire {
				fixture.finishPage(t, retire)
				delete(livePages, retire)
			}
			if page != 0 {
				// Repeating a previous handoff's release cannot release this page.
				fixture.pages[page-1].nextNavigation.release()
				fixture.pages[page-1].nextNavigation.release()
			}
			synctest.Wait()
			fixture.takeHeld(t, page)
			for asset := range nativeBrowsingAssetCount {
				progress := &fixture.pressure.assets[page][asset]
				if asset == 2 && page+1 < nativeBrowsingPageCount {
					state := fixture.pages[page]
					if err := state.nextNavigation.waitForBodyProgress(fixture.ctx, progress); err != nil {
						t.Fatalf("page %d original image readiness: %v", page, err)
					}
					if progress.receivedBytes.Load() != progress.expectedBytes-nativeBrowsingHeldPrefixBytes ||
						progress.serverWritten.Load() != progress.expectedBytes-nativeBrowsingHeldPrefixBytes || progress.finished.Load() {
						t.Fatalf("page %d image did not stream all but its bounded final fragment", page)
					}
				} else if !progress.finished.Load() || progress.receivedBytes.Load() != progress.expectedBytes {
					t.Fatalf("page %d asset %d was unnecessarily held before navigation", page, asset)
				}
			}
		}
		if fixture.pages[nativeBrowsingPageCount-1].nextNavigation != nil {
			t.Fatal("final page requires a nonexistent next tap")
		}
		for page := nativeBrowsingFinalPageStart(fixture.settings.maxLivePages); page < nativeBrowsingPageCount; page++ {
			fixture.finishPage(t, page)
			delete(livePages, page)
		}
		fixture.clients.Wait()
		fixture.assetServer.workers.Wait()
		ownership := fixture.pressure.assetOwnershipSnapshot()
		if fixture.pressure.WorkloadVersion != "next-navigation-image-v2" || fixture.observation.OverlapProofs != 99 ||
			fixture.observation.CompletedNavigations != 100 || fixture.completedAssets != 400 || len(livePages) != 0 ||
			fixture.pressure.CancellationAttempts != 100 || fixture.observation.CanceledAssets+fixture.pressure.CompletedDespiteCancellation != 100 ||
			ownership.ClientStarted != 400 || ownership.ClientFinished != 400 || ownership.BodiesOpened != 400 || ownership.BodiesClosed != 400 ||
			ownership.ServerStarted != 400 || ownership.ServerFinished != 400 || ownership.ServerAttempts != 400 ||
			ownership.ServerFinishedAttempts != 400 || ownership.ServerActiveAttempts != 0 || ownership.CanceledClientPending != 0 || ownership.CanceledServerPending != 0 ||
			fixture.liveHandlers.Load() != 0 || fixture.liveAssets.Load() != 0 || fixture.pressure.activeRequests.Load() != 0 {
			t.Fatalf("revised workload lost exact completion/cancellation or joined ownership: pages=%d assets=%d overlaps=%d ownership=%+v",
				fixture.observation.CompletedNavigations, fixture.completedAssets, fixture.observation.OverlapProofs, ownership)
		}
		for page := range nativeBrowsingPageCount {
			if fixture.htmlRequests[page].Load() != 1 {
				t.Fatalf("page %d changed the 100-original-HTML request census", page)
			}
			for asset := range nativeBrowsingAssetCount {
				if fixture.assetRequests[page][asset].Load() != 1 {
					t.Fatalf("page %d asset %d changed the 400-original-asset request census", page, asset)
				}
			}
		}
	})
}

// Cancellation before the first body read and while the tail is held both
// wake the owner and reject a false overlap. Healthy siblings still complete.
func TestNativeBrowsingNextNavigationImageRejectsCanceledBody(t *testing.T) {
	for _, beforeRead := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newNativeBrowsingOverlapFixture(t, true)
			defer func() {
				if err := fixture.close(); err != nil {
					t.Error(err)
				}
			}()
			fixture.navigate(t, 0)
			fixture.startAssets(t, 0, beforeRead)
			synctest.Wait()
			progress := &fixture.pressure.assets[0][2]
			if !beforeRead {
				if err := fixture.pages[0].nextNavigation.waitForBodyProgress(fixture.ctx, progress); err != nil {
					t.Fatal(err)
				}
				progress.cancelRequested.Store(true)
				fixture.pages[0].assetCancel[2]()
				synctest.Wait()
			}
			if err := fixture.pages[0].nextNavigation.waitForBodyProgress(fixture.ctx, progress); err == nil ||
				!progress.finished.Load() || beforeRead && progress.receivedBytes.Load() != 0 {
				t.Fatalf("canceled image was accepted as live body progress: before_read=%t err=%v", beforeRead, err)
			}
			if _, err := fixture.settings.startPressureNavigation(fixture.observation, &fixture.pages, 1); err == nil || fixture.observation.OverlapProofs != 0 {
				t.Fatal("canceled original image fabricated a next-navigation overlap")
			}
			for asset := range nativeBrowsingAssetCount {
				result := <-fixture.pages[0].clientDone[asset]
				if asset == 2 {
					if !nativeBrowsingStreamingCancellationValid(result, nativeBrowsingAssetBytes(0, asset)) {
						t.Fatalf("original image cancellation lost validated prefix bytes: %+v", result)
					}
				} else if result.err != nil || result.bytes != nativeBrowsingAssetBytes(0, asset) {
					t.Fatalf("healthy sibling %d did not complete exactly: %+v", asset, result)
				}
			}
			if fixture.ctx.Err() != nil || fixture.htmlRequests[1].Load() != 0 {
				t.Fatal("child cancellation changed the parent or started a false next HTML request")
			}
			if err := fixture.close(); err != nil {
				t.Fatal(err)
			}
			ownership := fixture.pressure.assetOwnershipSnapshot()
			if ownership.ClientStarted != 4 || ownership.ClientFinished != 4 || ownership.BodiesOpened != ownership.BodiesClosed ||
				ownership.ServerActiveAttempts != 0 || fixture.liveHandlers.Load() != 0 || fixture.liveAssets.Load() != 0 {
				t.Fatalf("canceled image or healthy sibling retained a client/handler owner: %+v", ownership)
			}
		})
	}
}

// Only a proved original handler abort may account for the exact held extent.
// HTTP can expose its short Content-Length as unexpected EOF before client
// transport cancellation; unrelated/joined errors keep their own authority.
func nativeBrowsingOverlapCleanupImageCanceled(result nativeBrowsingAssetResult, expected int, ownerCtx context.Context, receipt nativeBrowsingServerReceipt) bool {
	return ownerCtx.Err() == context.Canceled && result.status == http.StatusOK &&
		result.bytes == expected-nativeBrowsingHeldPrefixBytes &&
		!errors.Is(result.err, errNativeBrowsingResponseMismatch) &&
		(result.err == context.Canceled || result.err == io.ErrUnexpectedEOF) &&
		receipt.started == 1 && receipt.finished == 1 && receipt.canceled == 1
}

// Retirement alone never releases the image tail. With no next navigation,
// final owner cleanup releases every visited and unvisited gate. Cancellation
// settles before release so its exact withheld extent and handler are proved.
func TestNativeBrowsingNextNavigationCleanupWithoutTapJoinsOwners(t *testing.T) {
	for _, cancelOwner := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newNativeBrowsingOverlapFixture(t, true)
			defer func() {
				if err := fixture.close(); err != nil {
					t.Error(err)
				}
			}()
			fixture.navigate(t, 0)
			fixture.startAssets(t, 0, false)
			synctest.Wait()
			fixture.takeHeld(t, 0)
			state := fixture.pages[0]
			progress := &fixture.pressure.assets[0][2]
			state.releaseAssets()
			state.releaseAssets()
			synctest.Wait()
			if err := state.nextNavigation.checkBodyProgress(progress); err != nil || progress.receivedBytes.Load() != progress.expectedBytes-nativeBrowsingHeldPrefixBytes {
				t.Fatalf("retirement released a body before its absent next tap: %v", err)
			}
			if cancelOwner {
				fixture.cancel()
				synctest.Wait()
			}
			nativeBrowsingReleasePages(fixture.pages[:])
			nativeBrowsingReleasePages(fixture.pages[:])
			synctest.Wait()
			fixture.clients.Wait()
			fixture.assetServer.workers.Wait()
			for asset := range nativeBrowsingAssetCount {
				result := <-state.clientDone[asset]
				complete := result.err == nil && result.bytes == nativeBrowsingAssetBytes(0, asset)
				receipt := state.serverReceipts[asset].Value()
				canceled := cancelOwner && asset == 2 && nativeBrowsingOverlapCleanupImageCanceled(result, nativeBrowsingAssetBytes(0, asset), fixture.ctx, receipt)
				if !complete && !canceled {
					t.Fatalf("cleanup changed original body/cancellation: cancelOwner=%t asset=%d bytes=%d status=%d err=%v (%T) receipt=%+v", cancelOwner, asset, result.bytes, result.status, result.err, result.err, receipt)
				}
				if cancelOwner && asset == 2 && !canceled {
					t.Fatal("owner cancellation completed a tail whose latch was still closed")
				}
			}
			for page, state := range fixture.pages {
				select {
				case <-state.release:
				default:
					t.Fatalf("cleanup retained page %d retirement gate", page)
				}
				if state.nextNavigation != nil {
					select {
					case <-state.nextNavigation.released:
					default:
						t.Fatalf("cleanup retained page %d next-navigation gate", page)
					}
				}
			}
			if err := fixture.close(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.close(); err != nil {
				t.Fatal(err)
			}
			ownership := fixture.pressure.assetOwnershipSnapshot()
			if fixture.observation.OverlapProofs != 0 || ownership.ClientStarted != 4 || ownership.ClientFinished != 4 ||
				ownership.ServerAttempts != 4 || ownership.ServerFinishedAttempts != 4 || ownership.ServerActiveAttempts != 0 ||
				ownership.BodiesOpened != 4 || ownership.BodiesClosed != 4 || ownership.BodyCloseErrors != 0 ||
				fixture.liveHandlers.Load() != 0 || fixture.liveAssets.Load() != 0 || fixture.pressure.activeRequests.Load() != 0 {
				t.Fatalf("no-next-tap cleanup retained an owner: %+v", ownership)
			}
		})
	}
}

// Cancel only the server work owner while the client request stays live until
// the actual HTTP body ends. This forces the server-first EOF independently of
// scheduler ordering and keeps corruption and unrelated failures observable.
func TestNativeBrowsingCleanupServerAbortKeepsErrorAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeBrowsingOverlapFixture(t, true)
		serverCtx, cancelServer := context.WithCancel(fixture.ctx)
		fixture.assetServer.workCtx = serverCtx
		defer func() {
			cancelServer()
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		fixture.navigate(t, 0)
		fixture.startAssets(t, 0, false)
		synctest.Wait()
		fixture.takeHeld(t, 0)
		state := fixture.pages[0]
		progress := &fixture.pressure.assets[0][2]
		if err := state.nextNavigation.checkBodyProgress(progress); err != nil || progress.receivedBytes.Load() != progress.expectedBytes-nativeBrowsingHeldPrefixBytes {
			t.Fatalf("server-first control did not reach its original held extent: %v", err)
		}
		cancelServer()
		synctest.Wait()
		fixture.clients.Wait()
		fixture.assetServer.workers.Wait()
		result := <-state.clientDone[2]
		receipt := state.serverReceipts[2].Value()
		expected := nativeBrowsingAsset(0, 2)
		if fixture.ctx.Err() != nil || result.err != io.ErrUnexpectedEOF ||
			!nativeBrowsingOverlapCleanupImageCanceled(result, len(expected), serverCtx, receipt) ||
			nativeBrowsingStreamingCancellationValid(result, len(expected)) {
			t.Fatalf("server-first control lost EOF/cancellation distinction: ownerErr=%v clientErr=%v bytes=%d status=%d err=%v (%T) receipt=%+v",
				serverCtx.Err(), fixture.ctx.Err(), result.bytes, result.status, result.err, result.err, receipt)
		}
		t.Logf("server-first owner cancellation: bytes=%d status=%d err=%v (%T); client context remains live", result.bytes, result.status, result.err, result.err)
		corruptPrefix := append([]byte(nil), expected[:result.bytes]...)
		corruptPrefix[0] ^= 1
		corrupt := result
		corrupt.err = nativeBrowsingResponseError(result.status, corruptPrefix, expected, result.err)
		badStatus := result
		badStatus.status = http.StatusBadGateway
		badStatus.err = nativeBrowsingResponseError(badStatus.status, expected[:result.bytes], expected, result.err)
		unrelated := result
		unrelated.err = io.ErrClosedPipe
		joined := result
		joined.err = errors.Join(io.ErrUnexpectedEOF, io.ErrClosedPipe)
		joinedCancel := result
		joinedCancel.err = errors.Join(context.Canceled, io.ErrClosedPipe)
		for _, test := range []struct {
			name     string
			result   nativeBrowsingAssetResult
			ownerCtx context.Context
			receipt  nativeBrowsingServerReceipt
		}{
			{name: "live-owner-truncation", result: result, ownerCtx: fixture.ctx, receipt: receipt},
			{name: "corrupt-prefix", result: corrupt, ownerCtx: serverCtx, receipt: receipt},
			{name: "invalid-status", result: badStatus, ownerCtx: serverCtx, receipt: receipt},
			{name: "unrelated-read-error", result: unrelated, ownerCtx: serverCtx, receipt: receipt},
			{name: "joined-unrelated-error", result: joined, ownerCtx: serverCtx, receipt: receipt},
			{name: "joined-unrelated-cancel", result: joinedCancel, ownerCtx: serverCtx, receipt: receipt},
			{name: "uncanceled-handler", result: result, ownerCtx: serverCtx, receipt: nativeBrowsingServerReceipt{started: 1, finished: 1}},
			{name: "unjoined-handler", result: result, ownerCtx: serverCtx, receipt: nativeBrowsingServerReceipt{started: 1, canceled: 1}},
		} {
			if nativeBrowsingOverlapCleanupImageCanceled(test.result, len(expected), test.ownerCtx, test.receipt) {
				t.Errorf("%s was masked as cleanup cancellation: err=%v (%T) receipt=%+v", test.name, test.result.err, test.result.err, test.receipt)
			}
		}
		for _, asset := range []int{0, 1, 3} {
			result := <-state.clientDone[asset]
			if result.err != nil || result.bytes != nativeBrowsingAssetBytes(0, asset) {
				t.Fatalf("server-first cleanup affected healthy sibling %d: bytes=%d err=%v (%T)", asset, result.bytes, result.err, result.err)
			}
		}
		if err := fixture.close(); err != nil {
			t.Fatal(err)
		}
		ownership := fixture.pressure.assetOwnershipSnapshot()
		if ownership.ClientStarted != 4 || ownership.ClientFinished != 4 || ownership.BodiesOpened != 4 || ownership.BodiesClosed != 4 ||
			ownership.BodyCloseErrors != 0 || ownership.ServerAttempts != 4 || ownership.ServerFinishedAttempts != 4 || ownership.ServerActiveAttempts != 0 ||
			fixture.liveHandlers.Load() != 0 || fixture.liveAssets.Load() != 0 || fixture.pressure.activeRequests.Load() != 0 {
			t.Fatalf("server-first owner cleanup retained an owner: %+v", ownership)
		}
	})
}
