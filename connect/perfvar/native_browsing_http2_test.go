// Inner HTTP/2 keeps the original workload and its outer H1 carrier. A
// request-owned JS reader pauses after real bytes, never the shared TLS reader.
package perfvar

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// One body owns one pause and release; cancellation never waits for a tap.
type nativeBrowsingHttp2BodyGate struct {
	paused      chan struct{}
	released    chan struct{}
	pauseOnce   sync.Once
	releaseOnce sync.Once
	wrapped     atomic.Bool
}

func newNativeBrowsingHttp2BodyGate() *nativeBrowsingHttp2BodyGate {
	return &nativeBrowsingHttp2BodyGate{paused: make(chan struct{}), released: make(chan struct{})}
}

func (self *nativeBrowsingHttp2BodyGate) release() {
	self.releaseOnce.Do(func() { close(self.released) })
}

// The first 1 KiB is returned unchanged. Only this body's next Read waits.
type nativeBrowsingHttp2BodyReader struct {
	ctx    context.Context
	reader io.Reader
	gate   *nativeBrowsingHttp2BodyGate
	read   int
}

func (self *nativeBrowsingHttp2BodyReader) Read(buffer []byte) (int, error) {
	if self.read >= nativeBrowsingHeldPrefixBytes {
		self.gate.pauseOnce.Do(func() { close(self.gate.paused) })
		select {
		case <-self.ctx.Done():
			return 0, self.ctx.Err()
		case <-self.gate.released:
		}
	} else if remaining := nativeBrowsingHeldPrefixBytes - self.read; remaining < len(buffer) {
		buffer = buffer[:remaining]
	}
	count, err := self.reader.Read(buffer)
	self.read += count
	return count, err
}

// Slots are the original 100 HTML and 400 asset requests. Session identities
// remain fixed to four origins; reports contain counts, never socket objects.
type nativeBrowsingHttp2Request struct {
	requestOwner       *nativeBrowsingBodyRequestOwner
	bodyGate           *nativeBrowsingHttp2BodyGate
	clientAcquisitions int
	serverAttempts     int
	reused             bool
}

type nativeBrowsingHttp2Observation struct {
	ClientAcquisitions  int                            `json:"client_acquisitions"`
	ServerAttempts      int                            `json:"server_attempts"`
	ClientSessions      int                            `json:"client_sessions"`
	ServerSessions      int                            `json:"server_sessions"`
	ValidatedHtml       int                            `json:"validated_html"`
	UnreadBodyOverlaps  int                            `json:"unread_body_overlaps"`
	SameSessionHtml     int                            `json:"same_session_html"`
	TlsHandshakes       int                            `json:"tls_handshakes"`
	PreNavigationGateNs [nativeBrowsingPageCount]int64 `json:"pre_navigation_gate_ns"`
	CombinedFirstByteNs [nativeBrowsingPageCount]int64 `json:"combined_first_byte_ns"`
}

type nativeBrowsingHttp2Witness struct {
	stateLock           sync.Mutex
	pressure            *nativeBrowsingPressureObservation
	gates               [nativeBrowsingPageCount - 1]*nativeBrowsingHttp2BodyGate
	requests            [nativeBrowsingPageCount][nativeBrowsingAssetCount + 1]nativeBrowsingHttp2Request
	clientSessions      [nativeBrowsingOriginCount]*tls.Conn
	serverSessions      [nativeBrowsingOriginCount]string
	validatedHtml       int
	unreadBodyOverlaps  int
	sameSessionHtml     int
	tlsHandshakes       int
	tapStarted          [nativeBrowsingPageCount]time.Time
	preNavigationGateNs [nativeBrowsingPageCount]int64
	combinedFirstByteNs [nativeBrowsingPageCount]int64
	firstErr            error
}

func newNativeBrowsingHttp2Witness(pressure *nativeBrowsingPressureObservation) *nativeBrowsingHttp2Witness {
	self := &nativeBrowsingHttp2Witness{pressure: pressure}
	for page := range self.gates {
		self.gates[page] = newNativeBrowsingHttp2BodyGate()
		self.requests[page][1].bodyGate = self.gates[page]
	}
	return self
}

// Parse only the existing synthetic request plan; no new requests are added.
func nativeBrowsingHttp2RequestSlot(target string) (page, slot, origin int, err error) {
	parts := strings.Split(strings.Trim(sdkBrowsingRequestPath(target), "/"), "/")
	if len(parts) < 2 {
		return 0, 0, 0, errors.New("unowned HTTP/2 request path")
	}
	page, err = strconv.Atoi(parts[1])
	if err != nil || page < 0 || nativeBrowsingPageCount <= page {
		return 0, 0, 0, errors.New("unowned HTTP/2 page")
	}
	if len(parts) == 2 && parts[0] == "page" {
		return page, nativeBrowsingAssetCount, page % nativeBrowsingOriginCount, nil
	}
	if len(parts) != 3 || parts[0] != "asset" {
		return 0, 0, 0, errors.New("unowned HTTP/2 asset path")
	}
	slot, err = strconv.Atoi(parts[2])
	if err != nil || slot < 0 || nativeBrowsingAssetCount <= slot {
		return 0, 0, 0, errors.New("unowned HTTP/2 asset")
	}
	return page, slot, (page + slot) % nativeBrowsingOriginCount, nil
}

func (self *nativeBrowsingHttp2Witness) recordError(err error) {
	if err == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.firstErr == nil {
		self.firstErr = err
	}
}

// Existing request/session receipts prove the unread owner independently of
// how many response-body bytes have arrived before the next click.
func (self *nativeBrowsingHttp2Witness) checkUnreadBody(page int) error {
	if page < 0 || len(self.gates) <= page || self.pressure == nil {
		return errors.New("HTTP/2 prior JS owner is missing")
	}
	origin := (page + 1) % nativeBrowsingOriginCount
	self.stateLock.Lock()
	request, firstErr := self.requests[page][1], self.firstErr
	clientSession, serverSession := self.clientSessions[origin], self.serverSessions[origin]
	self.stateLock.Unlock()
	if firstErr != nil {
		return firstErr
	}
	target := strings.Replace(nativeBrowsingURL(page, 1), "http://", "https://", 1)
	if err := nativeBrowsingBodyRequestDispatched(request.requestOwner, self.gates[page], target); err != nil {
		return err
	}
	if request.bodyGate != self.gates[page] || request.clientAcquisitions < 0 || 1 < request.clientAcquisitions ||
		request.serverAttempts < 0 || 1 < request.serverAttempts || request.serverAttempts > request.clientAcquisitions ||
		request.clientAcquisitions == 1 && clientSession == nil || request.serverAttempts == 1 && serverSession == "" {
		return errors.New("HTTP/2 prior JS has invalid observed client/server ownership")
	}
	progress := &self.pressure.assets[page][1]
	if (progress.bodyOpened.Load() || progress.receivedBytes.Load() != 0) &&
		(request.clientAcquisitions != 1 || request.serverAttempts != 1) {
		return errors.New("HTTP/2 opened body lacks established original ownership")
	}
	return nativeBrowsingUnfinishedBodyOwner(self.gates[page], &self.pressure.assets[page][1],
		nativeBrowsingAssetBytes(page, 1))
}

// Only the exact JS request is gated. Its next HTML has the same origin;
// image 2 retains the independent original 99 server-tail overlap proofs.
func (self *nativeBrowsingHttp2Witness) wrapReader(ctx context.Context, target string, reader io.Reader) io.Reader {
	page, slot, _, err := nativeBrowsingHttp2RequestSlot(target)
	if err != nil {
		self.recordError(err)
		return reader
	}
	if slot != 1 || len(self.gates) <= page {
		return reader
	}
	gate := self.gates[page]
	if !gate.wrapped.CompareAndSwap(false, true) {
		self.recordError(errors.New("HTTP/2 original JS body was wrapped twice"))
		return reader
	}
	return &nativeBrowsingHttp2BodyReader{ctx: ctx, reader: reader, gate: gate}
}

// Keep tap accounting unchanged and inspect the existing owner without a
// body-read dependency before the immediately following HTML request.
func (self *nativeBrowsingHttp2Witness) beforeNavigation(ctx context.Context, page int, _ func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	self.stateLock.Lock()
	self.tapStarted[page] = time.Now()
	self.stateLock.Unlock()
	if page == 0 {
		return nil
	}
	if err := self.checkUnreadBody(page - 1); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.firstErr != nil {
		return self.firstErr
	}
	self.unreadBodyOverlaps++
	self.preNavigationGateNs[page] = int64(time.Since(self.tapStarted[page]))
	return nil
}

// GotConn records the selected live TLS session, not a speculative dial.
// Repeated acquisitions are retained as a failing ambiguity, never merged.
func (self *nativeBrowsingHttp2Witness) requestTrace(target string) *httptrace.ClientTrace {
	return self.requestTraceWithContext(context.Background(), target)
}

func (self *nativeBrowsingHttp2Witness) requestTraceWithContext(ctx context.Context, target string) *httptrace.ClientTrace {
	page, slot, origin, err := nativeBrowsingHttp2RequestSlot(target)
	if err != nil {
		self.recordError(err)
		return nil
	}
	var onGetConn func(string)
	self.stateLock.Lock()
	request := &self.requests[page][slot]
	if gate := request.bodyGate; gate != nil {
		if ctx == nil || request.requestOwner != nil {
			self.stateLock.Unlock()
			self.recordError(errors.New("HTTP/2 original request trace registered more than once or without context"))
			return nil
		}
		owner := &nativeBrowsingBodyRequestOwner{ctx: ctx, gate: gate, target: target}
		request.requestOwner = owner
		onGetConn = func(hostPort string) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			current := &self.requests[page][slot]
			if current.requestOwner != owner || current.bodyGate != owner.gate || hostPort != fmt.Sprintf("origin%d.test:443", origin) {
				if self.firstErr == nil {
					self.firstErr = errors.New("HTTP/2 stale or foreign request dispatch")
				}
				return
			}
			owner.dispatched.Store(true)
		}
	}
	self.stateLock.Unlock()
	return &httptrace.ClientTrace{
		GetConn: onGetConn,
		GotConn: func(info httptrace.GotConnInfo) {
			secure, ok := info.Conn.(*tls.Conn)
			if !ok || secure.ConnectionState().NegotiatedProtocol != "h2" {
				self.recordError(errors.New("HTTP/2 request did not acquire a negotiated TLS session"))
				return
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			request := &self.requests[page][slot]
			request.clientAcquisitions++
			request.reused = info.Reused
			if self.clientSessions[origin] == nil {
				self.clientSessions[origin] = secure
			}
			if (self.clientSessions[origin] != secure || request.clientAcquisitions != 1) && self.firstErr == nil {
				self.firstErr = errors.New("HTTP/2 original request changed or reacquired its origin session")
			}
		},
		TLSHandshakeDone: func(state tls.ConnectionState, handshakeErr error) {
			if handshakeErr != nil || state.NegotiatedProtocol != "h2" || len(state.VerifiedChains) == 0 {
				self.recordError(errors.New("HTTP/2 TLS handshake failed root, name or ALPN verification"))
				return
			}
			self.stateLock.Lock()
			self.tlsHandshakes++
			self.stateLock.Unlock()
		},
		GotFirstResponseByte: func() {
			if slot == nativeBrowsingAssetCount {
				self.stateLock.Lock()
				if !self.tapStarted[page].IsZero() && self.combinedFirstByteNs[page] == 0 {
					self.combinedFirstByteNs[page] = int64(time.Since(self.tapStarted[page]))
				}
				self.stateLock.Unlock()
			}
			if slot == nativeBrowsingAssetCount && page != 0 {
				self.recordError(self.checkUnreadBody(page - 1))
			}
		},
	}
}

// The two sides have different NAT tuples. Each side independently proves
// reuse; no source-port-only join is used for provider or session identity.
func (self *nativeBrowsingHttp2Witness) wrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		page, slot, origin, err := nativeBrowsingHttp2RequestSlot(request.URL.String())
		local, known := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
		if err != nil || !known || request.ProtoMajor != 2 || request.TLS == nil || request.TLS.NegotiatedProtocol != "h2" {
			self.recordError(errors.New("HTTP/2 origin did not receive an owned negotiated request"))
			http.Error(writer, "HTTP/2 fixture protocol", http.StatusBadRequest)
			return
		}
		tuple := request.RemoteAddr + "|" + local.String()
		self.stateLock.Lock()
		self.requests[page][slot].serverAttempts++
		if self.serverSessions[origin] == "" {
			self.serverSessions[origin] = tuple
		}
		if (self.serverSessions[origin] != tuple || self.requests[page][slot].serverAttempts != 1) && self.firstErr == nil {
			self.firstErr = errors.New("HTTP/2 origin replaced its upstream session or replayed a request")
		}
		self.stateLock.Unlock()
		handler.ServeHTTP(writer, request)
	})
}

// This existing hook runs after HTML validation and Body.Close. The original
// JS request/body must still be unfinished on its same owned session.
func (self *nativeBrowsingHttp2Witness) afterFetch(target string, result nativeBrowsingAssetResult) {
	page, slot, _, err := nativeBrowsingHttp2RequestSlot(target)
	if err != nil {
		self.recordError(err)
		return
	}
	if slot != nativeBrowsingAssetCount {
		return
	}
	if page != 0 {
		defer self.gates[page-1].release()
		self.recordError(self.checkUnreadBody(page - 1))
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if result.err != nil || result.bytes != nativeBrowsingHTMLBytes {
		if self.firstErr == nil {
			self.firstErr = errors.New("HTTP/2 HTML did not validate before releasing prior JS")
		}
		return
	}
	self.validatedHtml++
	if page != 0 {
		prior, current := self.requests[page-1][1], self.requests[page][nativeBrowsingAssetCount]
		if prior.clientAcquisitions != 1 || prior.serverAttempts != 1 || current.clientAcquisitions != 1 || current.serverAttempts != 1 || !current.reused {
			if self.firstErr == nil {
				self.firstErr = errors.New("HTTP/2 next HTML lacks exact original-session reuse")
			}
			return
		}
		self.sameSessionHtml++
	}
}

// Copies only bounded scalar evidence. Connection references end at cleanup.
func (self *nativeBrowsingHttp2Witness) snapshot() (nativeBrowsingHttp2Observation, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result := nativeBrowsingHttp2Observation{ValidatedHtml: self.validatedHtml, UnreadBodyOverlaps: self.unreadBodyOverlaps,
		SameSessionHtml: self.sameSessionHtml, TlsHandshakes: self.tlsHandshakes,
		PreNavigationGateNs: self.preNavigationGateNs, CombinedFirstByteNs: self.combinedFirstByteNs}
	for _, requests := range self.requests {
		for _, request := range requests {
			result.ClientAcquisitions += request.clientAcquisitions
			result.ServerAttempts += request.serverAttempts
		}
	}
	for origin := range self.clientSessions {
		if self.clientSessions[origin] != nil {
			result.ClientSessions++
		}
		if self.serverSessions[origin] != "" {
			result.ServerSessions++
		}
	}
	return result, self.firstErr
}

func (self *nativeBrowsingHttp2Witness) verdict(firstByteBudget time.Duration) error {
	result, err := self.snapshot()
	if err != nil {
		return err
	}
	if result.ClientAcquisitions != nativeBrowsingPageCount*(nativeBrowsingAssetCount+1) || result.ServerAttempts != result.ClientAcquisitions ||
		result.ClientSessions != nativeBrowsingOriginCount || result.ServerSessions != nativeBrowsingOriginCount ||
		result.ValidatedHtml != nativeBrowsingPageCount || result.UnreadBodyOverlaps != nativeBrowsingPageCount-1 || result.SameSessionHtml != nativeBrowsingPageCount-1 {
		return fmt.Errorf("HTTP/2 workload/session census: %+v", result)
	}
	for page, elapsed := range result.CombinedFirstByteNs {
		if elapsed <= 0 || int64(firstByteBudget) <= elapsed {
			return fmt.Errorf("NATIVE_BROWSING_NAVIGATION_STALL: HTTP/2 page=%d combined_first_byte=%s pre_navigation_gate=%s", page, time.Duration(elapsed), time.Duration(result.PreNavigationGateNs[page]))
		}
	}
	return nil
}

// Error cleanup releases every response owner without waiting for navigation.
func (self *nativeBrowsingHttp2Witness) close() {
	for _, gate := range self.gates {
		gate.release()
	}
	self.stateLock.Lock()
	clear(self.clientSessions[:])
	self.stateLock.Unlock()
}
