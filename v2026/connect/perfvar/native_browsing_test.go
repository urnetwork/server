// Rapid navigation crosses the real full-TUN/H1/provider path. The clean control
// gates unfinished assets; revised pressure retains one image tail per tap.
package perfvar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"golang.org/x/net/html"
)

const (
	nativeBrowsingPageCount            = 100
	nativeBrowsingOriginCount          = 4
	nativeBrowsingConnectionsPerOrigin = 4
	nativeBrowsingAssetCount           = 4
	nativeBrowsingHTMLBytes            = 2048
	nativeBrowsingHeldPrefixBytes      = 1024
)

// Structural dispatch keeps the SAME workload source buildable on OLD. The
// fallback is its former SDK-native five-second policy, never a zero-wait fake.
type nativeBrowsingResultsClient interface {
	SendNativePacketBatchWithResults(clientconnect.TransferPath, protocol.ProvideMode, [][]byte, time.Duration, []bool) int
}

func nativeBrowsingBatchSender(multi *clientconnect.RemoteUserNatMultiClient, source clientconnect.TransferPath) func([][]byte, []bool) int {
	budget := sdk.DefaultDeviceLocalSettings().SendTimeout
	if native, ok := any(multi).(nativeBrowsingResultsClient); ok {
		return func(packets [][]byte, accepted []bool) int {
			return native.SendNativePacketBatchWithResults(source, protocol.ProvideMode_Network, packets, budget, accepted)
		}
	}
	return func(packets [][]byte, accepted []bool) int {
		return multi.SendPacketBatchWithResults(source, protocol.ProvideMode_Network, packets, budget, accepted)
	}
}

// Fixed per-navigation gates retain no payload. The client owns release;
// receipts join every HTTP attempt of each logical asset, including GET replay.
type nativeBrowsingPageState struct {
	release        chan struct{}
	releaseOnce    sync.Once
	held           chan int
	heldOnce       [nativeBrowsingAssetCount]sync.Once
	serverReceipts [nativeBrowsingAssetCount]*clientconnect.MonitorValue[nativeBrowsingServerReceipt]
	clientDone     [nativeBrowsingAssetCount]chan nativeBrowsingAssetResult
	assetCancel    [nativeBrowsingAssetCount]context.CancelFunc
	nextNavigation *nativeBrowsingNextNavigationBody
}

func newNativeBrowsingPageState() *nativeBrowsingPageState {
	state := &nativeBrowsingPageState{release: make(chan struct{}), held: make(chan int, 2)}
	for index := range nativeBrowsingAssetCount {
		state.serverReceipts[index] = clientconnect.NewMonitorValue(nativeBrowsingServerReceipt{})
		state.clientDone[index] = make(chan nativeBrowsingAssetResult, 1)
	}
	return state
}

func (self *nativeBrowsingPageState) releaseAssets() {
	self.releaseOnce.Do(func() { close(self.release) })
}

type nativeBrowsingAssetResult struct {
	bytes     int
	status    int
	firstByte time.Duration
	complete  time.Duration
	err       error
}

// A validation failure remains independently classifiable when cancellation
// is also returned by the body reader.
var errNativeBrowsingResponseMismatch = errors.New("synthetic response status/body mismatch")

// Complete bodies retain their exact length/hash contract; canceled reads
// still validate every byte actually received and the response status.
func nativeBrowsingResponseError(status int, body, expected []byte, readErr error) error {
	if status != http.StatusOK || len(expected) < len(body) ||
		!bytes.Equal(body, expected[:min(len(body), len(expected))]) {
		return errors.Join(readErr, errNativeBrowsingResponseMismatch)
	}
	if readErr == nil && (len(body) != len(expected) || sha256.Sum256(body) != sha256.Sum256(expected)) {
		return errNativeBrowsingResponseMismatch
	}
	return readErr
}

// Cancellation does not excuse a concurrently observed invalid response.
func nativeBrowsingCanceledAssetValid(result nativeBrowsingAssetResult) bool {
	return errors.Is(result.err, context.Canceled) &&
		!errors.Is(result.err, errNativeBrowsingResponseMismatch) &&
		0 <= result.bytes && result.bytes <= nativeBrowsingHeldPrefixBytes
}

type nativeBrowsingPageObservation struct {
	Page                  int   `json:"page"`
	PlannedBytes          int   `json:"planned_bytes"`
	CompletedBytes        int   `json:"completed_bytes"`
	CanceledPrefixBytes   int   `json:"canceled_prefix_bytes"`
	CanceledAssets        int   `json:"canceled_assets"`
	FirstByteNanoseconds  int64 `json:"first_byte_ns"`
	CompletionNanoseconds int64 `json:"completion_ns"`
	PreviousAssetsHeld    int   `json:"previous_assets_held"`
	NavigationStalled     bool  `json:"navigation_stalled"`
}

// Process-scoped samples are explicitly test attribution, not an application
// 32/64-MiB qualification. No GC or global memory setting changes the workload.
type nativeBrowsingMemory struct {
	HeapAlloc  uint64                  `json:"heap_alloc"`
	HeapInuse  uint64                  `json:"heap_inuse"`
	PoolRoots  uint64                  `json:"pool_roots"`
	PoolBytes  clientconnect.ByteCount `json:"pool_bytes"`
	Goroutines int                     `json:"goroutines"`
}

func nativeBrowsingMemoryNow() nativeBrowsingMemory {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return nativeBrowsingMemory{HeapAlloc: memory.HeapAlloc, HeapInuse: memory.HeapInuse,
		PoolRoots: clientconnect.MessagePoolOutstandingCount(), PoolBytes: clientconnect.MessagePoolOutstandingByteCount(),
		Goroutines: runtime.NumGoroutine()}
}

type nativeBrowsingObservation struct {
	NativeCapability               bool                                                   `json:"native_capability"`
	Pages                          [nativeBrowsingPageCount]nativeBrowsingPageObservation `json:"pages"`
	CompletedNavigations           int                                                    `json:"completed_navigations"`
	OverlapProofs                  int                                                    `json:"overlap_proofs"`
	PlannedBytes                   int                                                    `json:"planned_bytes"`
	CompletedBytes                 int                                                    `json:"completed_bytes"`
	CanceledPrefixBytes            int                                                    `json:"canceled_prefix_bytes"`
	CanceledAssets                 int                                                    `json:"canceled_assets"`
	MaximumConcurrentAssetHandlers int64                                                  `json:"maximum_concurrent_asset_handlers"`
	MaximumConcurrentLogicalAssets int64                                                  `json:"maximum_concurrent_logical_assets"`
	Baseline                       nativeBrowsingMemory                                   `json:"baseline"`
	Peak                           nativeBrowsingMemory                                   `json:"sampled_peak"`
	Steady                         nativeBrowsingMemory                                   `json:"steady"`
	Drain                          nativeBrowsingMemory                                   `json:"drain"`
	Bridge                         fullTunBridgeBatchObservation                          `json:"bridge"`
	Pressure                       *nativeBrowsingPressureObservation                     `json:"pressure,omitempty"`
}

func (self *nativeBrowsingObservation) sampleMemory() {
	current := nativeBrowsingMemoryNow()
	self.Steady = current
	self.Peak.HeapAlloc = max(self.Peak.HeapAlloc, current.HeapAlloc)
	self.Peak.HeapInuse = max(self.Peak.HeapInuse, current.HeapInuse)
	self.Peak.PoolRoots = max(self.Peak.PoolRoots, current.PoolRoots)
	self.Peak.PoolBytes = max(self.Peak.PoolBytes, current.PoolBytes)
	self.Peak.Goroutines = max(self.Peak.Goroutines, current.Goroutines)
}

// 73 is coprime with 251: the fixed distribution spans 50..300 KiB without
// retaining the entire session's payload or changing OLD/NEW body generation.
func nativeBrowsingPageBytes(page int) int { return (50 + page*73%251) * 1024 }

func nativeBrowsingAssetBytes(page, asset int) int {
	remaining := nativeBrowsingPageBytes(page) - nativeBrowsingHTMLBytes
	bytes := remaining / nativeBrowsingAssetCount
	if asset == nativeBrowsingAssetCount-1 {
		bytes += remaining % nativeBrowsingAssetCount
	}
	return bytes
}

func nativeBrowsingURL(page, asset int) string {
	if asset < 0 {
		return fmt.Sprintf("http://origin%d.test/page/%d", page%nativeBrowsingOriginCount, page)
	}
	return fmt.Sprintf("http://origin%d.test/asset/%d/%d", (page+asset)%nativeBrowsingOriginCount, page, asset)
}

func nativeBrowsingHTML(page int) []byte {
	text := fmt.Sprintf("<!doctype html><html><head><title>page %d</title><link rel=\"stylesheet\" href=\"%s\"><script src=\"%s\"></script></head><body><img src=\"%s\"><img src=\"%s\"><a href=\"%s\">next</a></body></html>",
		page, nativeBrowsingURL(page, 0), nativeBrowsingURL(page, 1), nativeBrowsingURL(page, 2), nativeBrowsingURL(page, 3), nativeBrowsingURL(page+1, -1))
	return append([]byte(text), bytes.Repeat([]byte(" "), nativeBrowsingHTMLBytes-len(text))...)
}

func nativeBrowsingAsset(page, asset int) []byte {
	body := make([]byte, nativeBrowsingAssetBytes(page, asset))
	for index := range body {
		body[index] = byte(33 + (page*17+asset*11+index)%91)
	}
	return body
}

// Follow the generated document rather than a hidden request-size list.
func nativeBrowsingReferences(body []byte) ([nativeBrowsingAssetCount]string, string, error) {
	var assets [nativeBrowsingAssetCount]string
	next, count := "", 0
	tokens := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			if tokens.Err() != io.EOF || count != len(assets) || next == "" {
				return assets, next, errors.New("HTML references are incomplete")
			}
			return assets, next, nil
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokens.Token()
			for _, attribute := range token.Attr {
				if token.Data == "a" && attribute.Key == "href" {
					next = attribute.Val
				}
				if (token.Data == "link" && attribute.Key == "href") || ((token.Data == "script" || token.Data == "img") && attribute.Key == "src") {
					if count == len(assets) {
						return assets, next, errors.New("HTML has excess assets")
					}
					assets[count], count = attribute.Val, count+1
				}
			}
		}
	}
}

// All four origins are private listeners reached through the actual app TUN.
// The fixed URLs need neither public DNS nor Internet access.
func measureNativeBrowsing100(ctx context.Context, path *fullTunPath, observation *nativeBrowsingObservation) (resultErr error) {
	return measureNativeBrowsing100WithSettings(ctx, path, observation, nativeBrowsingRunSettings{maxLivePages: 2})
}

// The clean control keeps its two-page gates and navigation deadline. The
// four-page pressure workload streams bodies and records slow navigation.
func measureNativeBrowsing100WithSettings(ctx context.Context, path *fullTunPath, observation *nativeBrowsingObservation, settings nativeBrowsingRunSettings) (resultErr error) {
	if err := settings.validate(); err != nil {
		return err
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	browsingURL := nativeBrowsingURL
	browsingHTML := nativeBrowsingHTML
	if settings.originServerTlsForTest != nil {
		browsingURL = func(page, asset int) string {
			return strings.Replace(nativeBrowsingURL(page, asset), "http://", "https://", 1)
		}
		browsingHTML = func(page int) []byte {
			// HTTPS adds one character to each of five URL references. Trim
			// only existing trailing padding; the HTML remains exactly 2048B.
			text := strings.ReplaceAll(string(nativeBrowsingHTML(page)), "http://", "https://")
			return []byte(text[:nativeBrowsingHTMLBytes])
		}
	}
	if settings.coldLinkForTest {
		originalURL, originalHTML := browsingURL, browsingHTML
		browsingURL = func(page, asset int) string { return nativeBrowsingColdLinkURL(originalURL, page, asset) }
		browsingHTML = func(page int) []byte { return nativeBrowsingColdLinkHTML(originalURL, originalHTML, page) }
	}
	pages := settings.newPages()
	var liveHandlers, peakHandlers atomic.Int64
	var liveAssets, peakAssets atomic.Int64
	assetServer := &nativeBrowsingAssetServer{workCtx: workCtx, pages: &pages,
		streaming: settings.pressure != nil, pressure: settings.pressure,
		liveHandlers: &liveHandlers, peakHandlers: &peakHandlers}
	var handler http.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) < 2 {
			http.NotFound(writer, request)
			return
		}
		page, err := strconv.Atoi(parts[1])
		if err != nil || page < 0 || len(pages) <= page {
			http.NotFound(writer, request)
			return
		}
		if parts[0] == "page" && len(parts) == 2 {
			writer.Header().Set("Content-Type", "text/html")
			_, _ = writer.Write(browsingHTML(page))
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
		assetServer.serveAsset(writer, request, page, asset)
	})
	if settings.wrapOriginHandlerForTest != nil {
		handler = settings.wrapOriginHandlerForTest(handler)
	}
	originCount := nativeBrowsingOriginCount
	if settings.coldLinkForTest {
		originCount++
	}
	listeners := make([]net.Listener, originCount)
	servers := make([]*http.Server, originCount)
	var serverWorkers sync.WaitGroup
	var assetWorkers sync.WaitGroup
	dialOrigin := func(dialCtx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		wantPort := "80"
		if settings.originServerTlsForTest != nil {
			wantPort = "443"
		}
		if err != nil || port != wantPort {
			return nil, errors.New("unexpected synthetic origin port")
		}
		for index, listener := range listeners {
			if host != fmt.Sprintf("origin%d.test", index) || listener == nil {
				continue
			}
			destination := listener.Addr().String()
			if settings.originDestinationForTest != nil {
				destination = settings.originDestinationForTest(index, listener)
			}
			var trace *httptrace.ClientTrace
			if settings.pressure != nil {
				trace = httptrace.ContextClientTrace(dialCtx)
			}
			if trace != nil && trace.ConnectStart != nil {
				trace.ConnectStart(network, address)
			}
			connection, dialErr := path.appTun.DialContext(dialCtx, network, destination)
			if trace != nil && trace.ConnectDone != nil {
				trace.ConnectDone(network, address, dialErr)
			}
			return connection, dialErr
		}
		return nil, errors.New("unexpected synthetic origin")
	}
	transport := newNativeBrowsingInnerTransport(dialOrigin, settings)

	defer func() {
		cancelWork()
		nativeBrowsingReleasePages(pages[:])
		transport.CloseIdleConnections()
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelJoin()
		for _, httpServer := range servers {
			if httpServer != nil {
				if err := httpServer.Shutdown(joinCtx); err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("synthetic browsing handler shutdown: %w", err), httpServer.Close())
				}
			}
		}
		joined := make(chan struct{})
		go func() { assetWorkers.Wait(); serverWorkers.Wait(); assetServer.workers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-joinCtx.Done():
			resultErr = errors.Join(resultErr, errors.New("synthetic browsing owners did not join"))
		}
		if settings.afterOriginsClosedForTest != nil {
			settings.afterOriginsClosedForTest()
		}
		observation.MaximumConcurrentAssetHandlers = peakHandlers.Load()
		observation.MaximumConcurrentLogicalAssets = peakAssets.Load()
		if liveHandlers.Load() != 0 {
			resultErr = errors.Join(resultErr, errors.New("synthetic browsing retained asset handlers"))
		}
		if liveAssets.Load() != 0 {
			resultErr = errors.Join(resultErr, errors.New("synthetic browsing retained logical assets"))
		}
	}()
	for index := range listeners {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		listeners[index] = listener
		if settings.originListenerForTest != nil {
			settings.originListenerForTest(listener)
		}
		httpServer, serveListener := newNativeBrowsingInnerServer(handler, listener, settings)
		servers[index] = httpServer
		serverWorkers.Add(1)
		go func() { defer serverWorkers.Done(); _ = httpServer.Serve(serveListener) }()
	}
	if settings.applicationDeadlineForTest > 0 {
		applicationCtx, cancelApplication := context.WithTimeout(workCtx, settings.applicationDeadlineForTest)
		defer cancelApplication()
		workCtx = applicationCtx
	}
	client := &http.Client{Transport: transport}
	fetch := func(requestCtx context.Context, target string, expected []byte, trace *nativeBrowsingHttpTrace, progress *nativeBrowsingRequestProgress) (result nativeBrowsingAssetResult) {
		if settings.afterFetchForTest != nil {
			// Runs after the original body-close/progress/request-owner defers,
			// with the same validated result and original measured durations.
			defer func() { settings.afterFetchForTest(target, result) }()
		}
		start := time.Now()
		if settings.pressure != nil {
			settings.pressure.beginFetch(len(expected))
			defer settings.pressure.activeRequests.Add(-1)
		}
		if progress != nil {
			progress.started.Store(true)
			defer func() {
				progress.finished.Store(true)
				progress.nextNavigation.signalBodyProgress()
			}()
		}
		var firstByte atomic.Int64
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
		if err != nil {
			return nativeBrowsingAssetResult{err: err}
		}
		clientTrace := &httptrace.ClientTrace{
			GotFirstResponseByte: func() { firstByte.CompareAndSwap(0, int64(time.Since(start))) },
		}
		if trace != nil {
			trace.started = start
			clientTrace = trace.clientTrace(&firstByte)
		}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), clientTrace))
		if progress != nil {
			request = request.WithContext(httptrace.WithClientTrace(request.Context(), progress.retryTrace()))
		}
		if settings.requestTraceForTest != nil {
			if observer := settings.requestTraceForTest(request.Context(), target); observer != nil {
				request = request.WithContext(httptrace.WithClientTrace(request.Context(), observer))
			}
		}
		response, err := client.Do(request)
		if err != nil {
			return nativeBrowsingAssetResult{firstByte: time.Duration(firstByte.Load()), complete: time.Since(start), err: err}
		}
		if progress != nil {
			progress.bodyOpened.Store(true)
		}
		defer func() {
			closeErr := response.Body.Close()
			if progress != nil {
				progress.bodyCloseError.Store(closeErr != nil)
				progress.bodyClosed.Store(true)
			}
		}()
		if err := nativeBrowsingVerifyTlsResponse(response, settings); err != nil {
			return nativeBrowsingAssetResult{status: response.StatusCode, firstByte: time.Duration(firstByte.Load()), complete: time.Since(start), err: err}
		}
		if progress != nil {
			progress.responseStatus.Store(int64(response.StatusCode))
		}
		var reader io.Reader = response.Body
		if settings.wrapResponseReaderForTest != nil {
			reader = settings.wrapResponseReaderForTest(target, reader)
		}
		if settings.wrapResponseReaderContextForTest != nil {
			reader = settings.wrapResponseReaderContextForTest(requestCtx, target, reader)
		}
		if settings.pressure != nil {
			reader = &nativeBrowsingProgressReader{reader: reader, pressure: settings.pressure, progress: progress}
		}
		body, err := io.ReadAll(io.LimitReader(reader, int64(len(expected)+1)))
		result = nativeBrowsingAssetResult{bytes: len(body), status: response.StatusCode, firstByte: time.Duration(firstByte.Load()), complete: time.Since(start), err: err}
		result.err = nativeBrowsingResponseError(response.StatusCode, body, expected, result.err)
		return result
	}
	finishPage := func(page int, cancelAbandoned bool) error {
		state := pages[page]
		row := &observation.Pages[page]
		var canceled [nativeBrowsingAssetCount]bool
		if settings.pressure != nil {
			for asset := range nativeBrowsingAssetCount {
				canceled[asset] = settings.pressure.assets[page][asset].cancelRequested.Load()
			}
		} else if cancelAbandoned && page%2 == 0 {
			for asset := 2; asset < nativeBrowsingAssetCount; asset++ {
				state.assetCancel[asset]()
				canceled[asset] = true
			}
			// A canceled client call alone is not a server ownership receipt.
			for asset := 2; asset < nativeBrowsingAssetCount; asset++ {
				receipt, err := state.waitAssetServer(workCtx, asset)
				if err != nil {
					return fmt.Errorf("page %d cancellation never reached its handler: %w", page, workCtx.Err())
				}
				if receipt.canceled != receipt.started {
					return fmt.Errorf("page %d asset %d completed instead of observing cancel", page, asset)
				}
			}
		}
		state.releaseAssets()
		for asset := range nativeBrowsingAssetCount {
			select {
			case result := <-state.clientDone[asset]:
				if canceled[asset] && (settings.pressure == nil || result.err != nil) {
					valid := nativeBrowsingCanceledAssetValid(result)
					if settings.pressure != nil {
						valid = nativeBrowsingStreamingCancellationValid(result, nativeBrowsingAssetBytes(page, asset))
					}
					if !valid {
						return fmt.Errorf("page %d canceled asset %d: bytes=%d err=%v", page, asset, result.bytes, result.err)
					}
					row.CanceledAssets++
					row.CanceledPrefixBytes += result.bytes
				} else {
					if result.err != nil || result.bytes != nativeBrowsingAssetBytes(page, asset) {
						return fmt.Errorf("page %d completed asset %d: status=%d bytes=%d err=%v", page, asset, result.status, result.bytes, result.err)
					}
					row.CompletedBytes += result.bytes
					if canceled[asset] && settings.pressure != nil {
						settings.pressure.CompletedDespiteCancellation++
					}
				}
				row.CompletionNanoseconds = max(row.CompletionNanoseconds, int64(result.complete))
			case <-workCtx.Done():
				return fmt.Errorf("page %d asset %d unfinished: %w", page, asset, workCtx.Err())
			}
			if settings.pressure != nil || !canceled[asset] {
				receipt, err := state.waitAssetServer(workCtx, asset)
				if err != nil {
					return err
				}
				if settings.pressure != nil {
					settings.pressure.ServerObservedCancellation += int(receipt.canceled)
				} else if receipt.canceled != 0 {
					return fmt.Errorf("page %d asset %d unexpectedly canceled", page, asset)
				}
			}
			state.assetCancel[asset]()
		}
		observation.CompletedBytes += row.CompletedBytes
		observation.CanceledAssets += row.CanceledAssets
		observation.CanceledPrefixBytes += row.CanceledPrefixBytes
		observation.CompletedNavigations++
		return nil
	}
	navigation := browsingURL(0, -1)
	for page := range nativeBrowsingPageCount {
		state := pages[page]
		row := &observation.Pages[page]
		row.Page, row.PlannedBytes = page, nativeBrowsingPageBytes(page)
		observation.PlannedBytes += row.PlannedBytes
		if page != 0 && settings.pressure == nil {
			prior := pages[page-1]
			for asset := 2; asset < nativeBrowsingAssetCount; asset++ {
				receipt := prior.serverReceipts[asset].Value()
				if receipt.started != 0 && receipt.finished == receipt.started {
					return fmt.Errorf("page %d started after prior assets finished", page)
				}
			}
			select {
			case <-prior.release:
				return fmt.Errorf("page %d started after prior release", page)
			default:
			}
			row.PreviousAssetsHeld = 2
			observation.OverlapProofs++
		}
		if navigation != browsingURL(page, -1) {
			return errors.New("HTML next-navigation link changed origin or page")
		}
		if settings.beforePageNavigationForTest != nil {
			if err := settings.beforePageNavigationForTest(workCtx, page, transport.CloseIdleConnections); err != nil {
				return err
			}
		}
		var trace *nativeBrowsingHttpTrace
		if settings.pressure != nil {
			var err error
			trace, err = settings.startPressureNavigation(observation, &pages, page)
			if err != nil {
				return err
			}
		}
		htmlResult := fetch(workCtx, navigation, browsingHTML(page), trace, nil)
		if settings.pressure != nil {
			// Capture even failed/canceled HTML before its error can start teardown.
			settings.pressure.finishNavigation(path, page, trace, htmlResult)
		}
		if htmlResult.err != nil {
			return fmt.Errorf("page %d HTML: %w", page, htmlResult.err)
		}
		row.FirstByteNanoseconds = int64(htmlResult.firstByte)
		row.CompletedBytes = htmlResult.bytes
		row.NavigationStalled = sdk.DefaultDeviceLocalSettings().SendTimeout <= htmlResult.firstByte
		if row.NavigationStalled && settings.pressure == nil {
			return fmt.Errorf("NATIVE_BROWSING_NAVIGATION_STALL: page=%d first_byte=%s prior_assets_held=%d", page, htmlResult.firstByte, row.PreviousAssetsHeld)
		}
		assets, next, err := nativeBrowsingReferences(browsingHTML(page))
		if err != nil {
			return err
		}
		navigation = next
		for asset, target := range assets {
			parsed, err := url.Parse(target)
			if err != nil || parsed.String() != browsingURL(page, asset) {
				return errors.New("HTML asset reference changed")
			}
			assetCtx, cancel := context.WithCancel(workCtx)
			state.assetCancel[asset] = cancel
			var progress *nativeBrowsingRequestProgress
			if settings.pressure != nil {
				progress = &settings.pressure.assets[page][asset]
				progress.expectedBytes = int64(nativeBrowsingAssetBytes(page, asset))
			}
			assetWorkers.Add(1)
			live := liveAssets.Add(1)
			for old := peakAssets.Load(); old < live && !peakAssets.CompareAndSwap(old, live); old = peakAssets.Load() {
			}
			go func() {
				defer assetWorkers.Done()
				defer liveAssets.Add(-1)
				state.clientDone[asset] <- fetch(assetCtx, target, nativeBrowsingAsset(page, asset), nil, progress)
			}()
		}
		if retire := nativeBrowsingRetirePage(page, settings.maxLivePages); 0 <= retire {
			if err := finishPage(retire, true); err != nil {
				return err
			}
		}
		var held [nativeBrowsingAssetCount]bool
		for range 2 {
			select {
			case asset := <-state.held:
				if asset < 2 || held[asset] {
					return errors.New("held asset receipt was duplicate or unrelated")
				}
				held[asset] = true
			case <-workCtx.Done():
				return fmt.Errorf("page %d never established asset overlap: %w", page, workCtx.Err())
			}
		}
		if state.nextNavigation != nil {
			if err := state.nextNavigation.waitForBodyProgress(workCtx, &settings.pressure.assets[page][2]); err != nil {
				return fmt.Errorf("page %d never established image body progress: %w", page, err)
			}
		}
		observation.sampleMemory()
	}
	for page := nativeBrowsingFinalPageStart(settings.maxLivePages); page < nativeBrowsingPageCount; page++ {
		if err := finishPage(page, page != nativeBrowsingPageCount-1); err != nil {
			return err
		}
	}
	if observation.CompletedNavigations != nativeBrowsingPageCount || peakAssets.Load() > int64(settings.maxLivePages*nativeBrowsingAssetCount) {
		return fmt.Errorf("navigation ownership census: pages=%d overlaps=%d canceled=%d peak_assets=%d peak_handlers=%d", observation.CompletedNavigations, observation.OverlapProofs, observation.CanceledAssets, peakAssets.Load(), peakHandlers.Load())
	}
	if settings.pressure == nil && (observation.OverlapProofs != nativeBrowsingPageCount-1 || observation.CanceledAssets != 100 || peakHandlers.Load() > 8) {
		return fmt.Errorf("clean navigation ownership census: overlaps=%d canceled=%d peak_handlers=%d", observation.OverlapProofs, observation.CanceledAssets, peakHandlers.Load())
	}
	if settings.pressure != nil && (settings.pressure.CancellationAttempts != 100 || observation.CanceledAssets+settings.pressure.CompletedDespiteCancellation != 100) {
		return fmt.Errorf("streaming cancellation census: attempts=%d canceled=%d completed=%d", settings.pressure.CancellationAttempts, observation.CanceledAssets, settings.pressure.CompletedDespiteCancellation)
	}
	return nil
}

// Ordinary PERFVAR coverage, not a tagged diagnostic: 100 generated HTML pages
// and their actual JS/CSS/image references, bounded overlap and abandonment,
// exact valid bodies, native full batches, and source-owned joined cleanup.
func TestFullTunH1NativeRapidBrowsing100Pages(t *testing.T) {
	if testing.Short() {
		t.Skip("full H1/TUN browsing topology")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4013)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		hooks := &fullTunConstructionTestHooks{afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
			if stage == fullTunConstructionStageMultiClient {
				path.nativeIngressForTest = true
			}
			return nil
		}}
		path, err := tryNewFullTunPathWithTopologyHooks(ctx, t, environment, fullTunRouteExchangeH1, false, defaultTunResourceProfile(), 1, hooks)
		if err != nil {
			t.Fatal(err)
		}
		observation := &nativeBrowsingObservation{Baseline: nativeBrowsingMemoryNow()}
		observation.Peak = observation.Baseline
		_, observation.NativeCapability = any(path.multiClient).(nativeBrowsingResultsClient)
		before := path.bridgeSends.batchSnapshot()
		defer func() {
			observation.Bridge = observeFullTunBridgeBatches(before, path.bridgeSends.batchSnapshot())
			path.close()
			observation.Drain = nativeBrowsingMemoryNow()
			encoded, encodeErr := json.Marshal(observation)
			if encodeErr != nil || 64*1024 < len(encoded) {
				t.Errorf("bounded browsing observation: bytes=%d err=%v", len(encoded), encodeErr)
				return
			}
			t.Logf("[perfvar] native-browsing100 %s", encoded)
		}()
		if err := measureNativeBrowsing100(ctx, path, observation); err != nil {
			t.Fatal(err)
		}
		if err := path.verifyRoute(); err != nil {
			t.Fatal(err)
		}
	})
}
