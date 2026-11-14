// Separate midpoint workload; frozen phase replay and production policy remain unchanged.
package perfvar

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

const privateSdkMidPage = 10

// Ordinary browsing owns body-overlap evidence; only the mechanism purpose
// requires public alternatives and enables the cold-owner arming boundary.
type privateSdkBrowsingPurpose string

const (
	privateSdkMidMechanismPurpose     privateSdkBrowsingPurpose = "strict-mid-session-mechanism"
	privateSdkSameSiteWorkloadPurpose privateSdkBrowsingPurpose = "ordinary-same-site-workload"
)

// Offsets share one monotonic fixture origin. Controller/retirement/prefix
// intervals remain visible beside the existing HTTP-only navigation phases.
type privateSdkMidNavigationTimes struct {
	EntryNs, FetchNs, HtmlDoneNs                           int64
	RetireBeginNs, RetireEndNs, PrefixBeginNs, PrefixEndNs int64
}

// Configuration is immutable before the SDK/bridge constructor. Runtime state
// belongs to this fixture, never to provider eligibility or production policy.
type privateSdkMidSession struct {
	purpose                                                      privateSdkBrowsingPurpose
	boundaryObserved                                             bool
	bodyPremiseAccepted                                          bool
	alternativesErr                                              error
	readinessForTest                                             func() (privateSdkBrowsingReadinessSnapshot, bool)
	armAttempted, armGenerationStable, armPremiseAccepted        bool
	cold, fault, sameSite                                        bool
	sameSiteRequests                                             int
	beforeDialDecisionForTest                                    func()
	fixture                                                      *privateSdkBrowsingFixture
	pressure                                                     *nativeBrowsingPressureObservation
	armed                                                        atomic.Bool
	blocked                                                      atomic.Int32
	cssRead                                                      atomic.Int64
	cssPrefix                                                    chan struct{}
	cssRelease                                                   chan struct{}
	stopped                                                      chan struct{}
	stopOnce                                                     sync.Once
	prefixOnce, releaseOnce                                      sync.Once
	stateLock                                                    sync.Mutex
	firstErr                                                     error
	html                                                         [nativeBrowsingPageCount]bool
	navReuse                                                     [nativeBrowsingPageCount]bool
	navAttempts                                                  [nativeBrowsingPageCount]int
	providers                                                    uint8
	cssServerAttempts, cssClientAttempts                         int
	cssSource, cssClientTuple                                    string
	cssProvider                                                  int
	cssServerLive, cssComplete                                   bool
	cssNatRead                                                   int64
	refusals                                                     [6]uint64
	firstDialNs, firstRefusalNs                                  int64
	responseProvider                                             int
	tap, firstByte                                               time.Time
	before                                                       privateSdkBrowsingReadinessSnapshot
	armHtmlCompleted                                             int
	armProviderMask                                              uint8
	armCssBytes, armCssNatBytes, armImageBytes, armImageExpected int64
	armCssServerLive                                             bool
	bridge                                                       privateSdkMidBridgeTrace
	finished                                                     bool
	started                                                      time.Time
	navigationTimes                                              [nativeBrowsingPageCount]privateSdkMidNavigationTimes
}

// Construct all gates before the fixture starts any callback.
func newPrivateSdkMidSession(cold, fault bool) *privateSdkMidSession {
	s := &privateSdkMidSession{purpose: privateSdkMidMechanismPurpose, started: time.Now(), cold: cold, fault: fault, cssPrefix: make(chan struct{}), cssRelease: make(chan struct{}), stopped: make(chan struct{}), cssProvider: -1, responseProvider: -1}
	s.blocked.Store(-1)
	return s
}

// Retain the same original-body gates without claiming a provider-failure
// experiment. Construction fixes the purpose before any request can start.
func newPrivateSdkSameSiteWorkload() *privateSdkMidSession {
	s := newPrivateSdkMidSession(false, false)
	s.purpose, s.sameSite = privateSdkSameSiteWorkloadPurpose, true
	return s
}

// Keep the first owned failure and wake a pending arming barrier.
func (self *privateSdkMidSession) recordError(err error) {
	if err == nil {
		return
	}
	self.stateLock.Lock()
	if self.firstErr == nil {
		self.firstErr = err
	}
	self.stateLock.Unlock()
	self.prefixOnce.Do(func() { close(self.cssPrefix) })
}

// Release only this original CSS body; repeated cleanup is harmless.
func (self *privateSdkMidSession) release() { self.releaseOnce.Do(func() { close(self.cssRelease) }) }

// Abort this workload owner independently of the server request context.
func (self *privateSdkMidSession) close() {
	self.stateLock.Lock()
	self.disarmWithLock()
	self.stopOnce.Do(func() { close(self.stopped) })
	self.stateLock.Unlock()
	self.bridge.freeze()
}

// Fault admission and cutoff share this lock; caller owns stateLock.
func (self *privateSdkMidSession) disarmWithLock() { self.armed.Store(false) }

// The matched same-site mode changes only authority: every HTML and asset
// uses the existing origin0 pool. Other modes retain their original URL plan.
func (self *privateSdkMidSession) rewriteUrl(original func(int, int) string, page, asset int) string {
	target := original(page, asset)
	if self.sameSite {
		origin := page % nativeBrowsingOriginCount
		if asset >= 0 {
			origin = (page + asset) % nativeBrowsingOriginCount
		}
		return strings.Replace(target, fmt.Sprintf("origin%d.test", origin), "origin0.test", 1)
	}
	if self.cold && page == privateSdkMidPage && asset < 0 {
		return strings.Replace(target, fmt.Sprintf("origin%d.test", page%nativeBrowsingOriginCount), "origin4.test", 1)
	}
	return target
}

func (self *privateSdkMidSession) rewriteHtml(originalUrl func(int, int) string, originalHtml func(int) []byte, page int) []byte {
	body := originalHtml(page)
	if self.sameSite {
		text := string(body)
		for asset := range nativeBrowsingAssetCount {
			text = strings.Replace(text, originalUrl(page, asset), self.rewriteUrl(originalUrl, page, asset), 1)
		}
		return []byte(strings.Replace(text, originalUrl(page+1, -1), self.rewriteUrl(originalUrl, page+1, -1), 1))
	}
	if self.cold && page+1 == privateSdkMidPage {
		return []byte(strings.Replace(string(body), originalUrl(page+1, -1), self.rewriteUrl(originalUrl, page+1, -1), 1))
	}
	return body
}

// This is the real fixture NAT dial boundary. The first actual caller chooses
// the affected ordinal; no route, winner, warning or qualification is forced.
func (self *privateSdkMidSession) rejectDial(ordinal int, address string) bool {
	if !self.cold || !self.armed.Load() || address != "203.0.113.14:443" || ordinal < 0 || ordinal >= 6 {
		return false
	}
	// Only deterministic controls install this pre-decision barrier.
	if self.beforeDialDecisionForTest != nil {
		self.beforeDialDecisionForTest()
	}
	self.stateLock.Lock()
	if !self.armed.Load() {
		self.stateLock.Unlock()
		return false
	}
	first := self.blocked.CompareAndSwap(-1, int32(ordinal))
	now := time.Now()
	if first {
		self.firstDialNs = int64(now.Sub(self.tap))
	}
	reject := self.fault && self.blocked.Load() == int32(ordinal)
	if reject {
		self.refusals[ordinal]++
		if self.firstRefusalNs == 0 {
			self.firstRefusalNs = int64(now.Sub(self.tap))
		}
	}
	self.stateLock.Unlock()
	// Both twins release the same old body on the first actual cold dial.
	// Refusal is a returned error, never a fabricated peer acknowledgement.
	if first {
		self.release()
	}
	return reject
}

// Count actual returned upstream bytes only while that CSS handler is live.
func (self *privateSdkMidSession) observeRead(source string, n int) {
	if n <= 0 {
		return
	}
	self.stateLock.Lock()
	if source == self.cssSource && self.cssServerLive {
		self.cssNatRead += int64(n)
	}
	self.stateLock.Unlock()
}

// Select generated synthetic paths without keeping a request or body.
func privateSdkMidPath(target string) string {
	p, err := url.Parse(target)
	if err != nil {
		return ""
	}
	return p.Path
}

// Compose optional workload observers; preserve the existing callback chain.
func (self *privateSdkMidSession) configure(settings *nativeBrowsingRunSettings) {
	settings.midSessionForTest = self
	settings.nextNavigationOverlap = true
	self.pressure = settings.pressure
	oldBefore, oldTrace := settings.beforePageNavigationForTest, settings.requestTraceForTest
	oldWrap, oldReader, oldAfter := settings.wrapOriginHandlerForTest, settings.wrapResponseReaderForTest, settings.afterFetchForTest
	settings.beforePageNavigationForTest = func(ctx context.Context, page int, closeIdle func()) error {
		if oldBefore != nil {
			if err := oldBefore(ctx, page, closeIdle); err != nil {
				return err
			}
		}
		return self.beforePage(ctx, page)
	}
	settings.requestTraceForTest = func(ctx context.Context, target string) *httptrace.ClientTrace {
		own := self.requestTrace(target)
		if oldTrace == nil {
			return own
		}
		prior := oldTrace(ctx, target)
		if prior == nil {
			return own
		}
		if own == nil {
			return prior
		}
		// The caller attaches this returned trace to ctx once. Compose only
		// these two observers here; including ctx would duplicate its callbacks.
		return httptrace.ContextClientTrace(httptrace.WithClientTrace(httptrace.WithClientTrace(context.Background(), prior), own))
	}
	settings.wrapOriginHandlerForTest = func(handler http.Handler) http.Handler {
		wrapped := self.wrapHandler(handler)
		if oldWrap != nil {
			return oldWrap(wrapped)
		}
		return wrapped
	}
	settings.wrapResponseReaderForTest = func(target string, reader io.Reader) io.Reader {
		if oldReader != nil {
			reader = oldReader(target, reader)
		}
		if privateSdkMidPath(target) == "/asset/9/0" {
			return &privateSdkMidReader{reader: reader, owner: self}
		}
		return reader
	}
	settings.afterFetchForTest = func(target string, result nativeBrowsingAssetResult) {
		if oldAfter != nil {
			oldAfter(target, result)
		}
		self.afterFetch(target, result)
	}
}

// Ten validated HTML returns and positive unfinished original bodies are the
// barrier. No sleep, retry timer, all-Proven gate or synthetic warmup is used.
func (self *privateSdkMidSession) beforePage(ctx context.Context, page int) error {
	if page != privateSdkMidPage {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	self.stateLock.Lock()
	self.tap = time.Now()
	self.stateLock.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-self.stopped:
		return context.Canceled
	case <-self.cssPrefix:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-self.stopped:
		return context.Canceled
	default:
	}
	progress := &self.pressure.assets[page-1][2]
	if err := progress.nextNavigation.checkBodyProgress(progress); err != nil {
		return err
	}
	read := self.readinessForTest
	if read == nil {
		read = func() (privateSdkBrowsingReadinessSnapshot, bool) {
			return privateSdkProductionReadinessSnapshot(self.fixture)
		}
	}
	view, stable := read()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-self.stopped:
		return context.Canceled
	default:
	}
	if self.purpose == privateSdkSameSiteWorkloadPurpose {
		return self.observeWorkloadWithReadiness(view, stable, progress.receivedBytes.Load(), progress.expectedBytes)
	}
	return self.armWithReadiness(view, stable, progress.receivedBytes.Load(), progress.expectedBytes)
}

// Preserve the boundary sample even when a separate mechanism is unqualified.
// The caller owns stateLock; this changes no provider or request state.
func (self *privateSdkMidSession) captureBeforePageWithLock(view privateSdkBrowsingReadinessSnapshot, stable bool, imageBytes, imageExpected int64) {
	self.boundaryObserved = true
	self.armGenerationStable = stable
	self.before = view
	self.armHtmlCompleted = 0
	for page := 0; page < privateSdkMidPage; page++ {
		if self.html[page] {
			self.armHtmlCompleted++
		}
	}
	self.armProviderMask = self.providers
	self.armCssBytes, self.armCssNatBytes, self.armCssServerLive = self.cssRead.Load(), self.cssNatRead, self.cssServerLive
	self.armImageBytes, self.armImageExpected = imageBytes, imageExpected
	self.alternativesErr = privateSdkMidReadyAlternatives(view, stable)
}

// Ordinary navigation requires the original bodies, but observing public
// alternatives cannot turn it into a provider-failure experiment.
func (self *privateSdkMidSession) observeWorkloadWithReadiness(view privateSdkBrowsingReadinessSnapshot, stable bool, imageBytes, imageExpected int64) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	select {
	case <-self.stopped:
		return context.Canceled
	default:
	}
	self.captureBeforePageWithLock(view, stable, imageBytes, imageExpected)
	if self.purpose != privateSdkSameSiteWorkloadPurpose || !self.sameSite || self.cold || self.fault {
		return errors.New("MID_SESSION_ORDINARY_WORKLOAD_PURPOSE_MISMATCH")
	}
	if self.firstErr != nil {
		return self.firstErr
	}
	ready, err := privateSdkProductionReadiness(view)
	if err != nil {
		return fmt.Errorf("SAME_SITE_PUBLIC_READINESS_UNOBSERVED: %w", err)
	}
	if !stable || !ready {
		return fmt.Errorf("SAME_SITE_PUBLIC_READINESS_UNOBSERVED: %s generation_stable=%t", view, stable)
	}
	if err := privateSdkMidArmPremise(self.html, self.providers, self.cssClientAttempts, self.cssServerAttempts, self.cssRead.Load(), self.cssNatRead, self.cssServerLive, self.cssClientTuple != "", self.cssSource != ""); err != nil {
		return err
	}
	self.bodyPremiseAccepted = true
	return nil
}

// Record attempted evidence before validation; failed arming is not an empty
// traffic history. Public availability and historical body selection stay separate.
func (self *privateSdkMidSession) armWithReadiness(view privateSdkBrowsingReadinessSnapshot, stable bool, imageBytes, imageExpected int64) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	select {
	case <-self.stopped:
		return context.Canceled
	default:
	}
	self.armAttempted = true
	self.captureBeforePageWithLock(view, stable, imageBytes, imageExpected)
	if self.purpose != privateSdkMidMechanismPurpose {
		return errors.New("MID_SESSION_STRICT_MECHANISM_PURPOSE_MISMATCH")
	}
	if self.firstErr != nil {
		return self.firstErr
	}
	if self.alternativesErr != nil {
		return self.alternativesErr
	}
	if err := privateSdkMidArmPremise(self.html, self.providers, self.cssClientAttempts, self.cssServerAttempts, self.cssRead.Load(), self.cssNatRead, self.cssServerLive, self.cssClientTuple != "", self.cssSource != ""); err != nil {
		return err
	}
	self.bodyPremiseAccepted = true
	self.armPremiseAccepted = true
	self.bridge.arm()
	self.armed.Store(true)
	return nil
}

// No past flow must select an alternative. The existing public snapshot must
// show at least two healthy local channels and two distinct owned destinations.
// Actual alternate origin delivery remains a separate post-refusal witness.
func privateSdkMidReadyAlternatives(view privateSdkBrowsingReadinessSnapshot, stable bool) error {
	ready, err := privateSdkProductionReadiness(view)
	if err != nil {
		return fmt.Errorf("MID_SESSION_PUBLIC_ALTERNATIVES_UNOBSERVED: %w", err)
	}
	healthy := view.Quality + view.Speed
	if !stable || !ready || healthy < 2 || view.Exits < healthy || view.DistinctOwned < 2 || view.OwnedMask&^uint8(63) != 0 || bits.OnesCount8(view.OwnedMask) != view.DistinctOwned {
		return fmt.Errorf("MID_SESSION_PUBLIC_ALTERNATIVES_UNOBSERVED: %s generation_stable=%t", view, stable)
	}
	return nil
}

// Reject missing or replayed original owners before enabling the fault.
func privateSdkMidArmPremise(html [nativeBrowsingPageCount]bool, providers uint8, clientAttempts, serverAttempts int, cssBytes, natBytes int64, serverLive, clientTuple, serverTuple bool) error {
	for page := 0; page < privateSdkMidPage; page++ {
		if !html[page] {
			return errors.New("MID_SESSION_TEN_HTML_PREMISE")
		}
	}
	if providers == 0 || providers&^uint8(63) != 0 {
		return errors.New("MID_SESSION_BODY_PROVIDER_UNOBSERVED")
	}
	if clientAttempts != 1 || serverAttempts != 1 || cssBytes != nativeBrowsingHeldPrefixBytes || natBytes <= 0 || !serverLive || !clientTuple || !serverTuple {
		return errors.New("MID_SESSION_ORIGINAL_CSS_PREMISE")
	}
	return nil
}

// Keep real acquisition/reuse receipts and close the fault at first byte.
func (self *privateSdkMidSession) requestTrace(target string) *httptrace.ClientTrace {
	path := privateSdkMidPath(target)
	var page int
	if n, err := fmt.Sscanf(path, "/page/%d", &page); n == 1 && err == nil && page >= 0 && page < nativeBrowsingPageCount {
		return &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			self.stateLock.Lock()
			self.navAttempts[page]++
			self.navReuse[page] = info.Reused
			self.stateLock.Unlock()
			if page == privateSdkMidPage && !self.cold {
				self.release()
			}
		}, GotFirstResponseByte: func() {
			if page == privateSdkMidPage {
				self.stateLock.Lock()
				if self.firstByte.IsZero() {
					self.firstByte = time.Now()
				}
				self.disarmWithLock()
				self.stateLock.Unlock()
				self.bridge.freeze()
				self.release()
			}
		}}
	}
	if path != "/asset/9/0" {
		return nil
	}
	return &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		self.stateLock.Lock()
		self.cssClientAttempts++
		if self.cssClientTuple == "" {
			self.cssClientTuple = info.Conn.LocalAddr().String() + "|" + info.Conn.RemoteAddr().String()
		}
		self.stateLock.Unlock()
	}}
}

// The driver calls this only at existing boundaries; no waiting is added.
func (self *privateSdkMidSession) markNavigation(page int, phase string) {
	if page < 0 || page >= nativeBrowsingPageCount {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	row := &self.navigationTimes[page]
	now := int64(time.Since(self.started))
	switch phase {
	case "entry":
		row.EntryNs = now
	case "fetch":
		row.FetchNs = now
	case "html_done":
		row.HtmlDoneNs = now
	case "retire_begin":
		row.RetireBeginNs = now
	case "retire_end":
		row.RetireEndNs = now
	case "prefix_begin":
		row.PrefixBeginNs = now
	case "prefix_end":
		row.PrefixEndNs = now
	}
}

// Record the validated result after the original body-close defers run.
func (self *privateSdkMidSession) afterFetch(target string, result nativeBrowsingAssetResult) {
	path := privateSdkMidPath(target)
	if path == "/asset/9/0" {
		self.stateLock.Lock()
		self.cssComplete = result.err == nil && result.status == http.StatusOK && result.bytes == nativeBrowsingAssetBytes(9, 0)
		self.stateLock.Unlock()
		if result.err != nil {
			self.recordError(result.err)
		}
		return
	}
	var page int
	if n, err := fmt.Sscanf(path, "/page/%d", &page); n == 1 && err == nil && page >= 0 && page < nativeBrowsingPageCount {
		self.stateLock.Lock()
		self.navigationTimes[page].HtmlDoneNs = int64(time.Since(self.started))
		self.html[page] = result.err == nil && result.status == http.StatusOK && result.bytes == nativeBrowsingHTMLBytes
		self.stateLock.Unlock()
	}
}

// Attribute the real upstream socket and wrap only the original page9 CSS.
func (self *privateSdkMidSession) wrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if self.sameSite && r.Host != "origin0.test" {
			self.recordError(errors.New("MID_SESSION_SAME_SITE_AUTHORITY_MISMATCH"))
			http.Error(w, "unexpected synthetic origin", http.StatusMisdirectedRequest)
			return
		}
		self.fixture.origins.stateLock.Lock()
		ordinal, known := self.fixture.origins.providerBySource[r.RemoteAddr]
		self.fixture.origins.stateLock.Unlock()
		if !known || ordinal < 0 || ordinal >= 6 {
			self.recordError(errors.New("MID_SESSION_UNOWNED_SOCKET"))
			http.Error(w, "unowned synthetic socket", http.StatusServiceUnavailable)
			return
		}
		self.stateLock.Lock()
		if self.sameSite {
			self.sameSiteRequests++
		}
		self.providers |= uint8(1) << ordinal
		if r.URL.Path == "/page/10" {
			self.responseProvider = ordinal
		}
		if r.URL.Path != "/asset/9/0" {
			self.stateLock.Unlock()
			handler.ServeHTTP(w, r)
			return
		}
		self.cssServerAttempts++
		duplicate := self.cssServerAttempts != 1
		if !duplicate {
			self.cssSource = r.RemoteAddr
			self.cssProvider = ordinal
			self.cssServerLive = true
		}
		self.stateLock.Unlock()
		if duplicate {
			self.recordError(errors.New("MID_SESSION_CSS_REPLAY"))
			http.Error(w, "duplicate synthetic CSS", http.StatusConflict)
			return
		}
		defer func() { self.stateLock.Lock(); self.cssServerLive = false; self.stateLock.Unlock() }()
		handler.ServeHTTP(&privateSdkMidWriter{ResponseWriter: w, ctx: r.Context(), owner: self}, r)
	})
}

type privateSdkMidWriter struct {
	http.ResponseWriter
	ctx   context.Context
	owner *privateSdkMidSession
}

// Flush a real prefix and retain the original suffix until release or cancellation.
func (self *privateSdkMidWriter) Write(body []byte) (int, error) {
	if len(body) != nativeBrowsingAssetBytes(9, 0) {
		return 0, errors.New("MID_SESSION_CSS_EXTENT")
	}
	n, err := self.ResponseWriter.Write(body[:nativeBrowsingHeldPrefixBytes])
	if err != nil || n != nativeBrowsingHeldPrefixBytes {
		if err == nil {
			err = io.ErrShortWrite
		}
		return n, err
	}
	flusher, ok := self.ResponseWriter.(http.Flusher)
	if !ok {
		return n, errors.New("MID_SESSION_NO_FLUSH")
	}
	flusher.Flush()
	select {
	case <-self.owner.stopped:
		return n, context.Canceled
	case <-self.ctx.Done():
		return n, self.ctx.Err()
	case <-self.owner.cssRelease:
	}
	// Owner cancellation wins over an already-ready normal release.
	select {
	case <-self.owner.stopped:
		return n, context.Canceled
	default:
	}
	if err := self.ctx.Err(); err != nil {
		return n, err
	}
	m, err := self.ResponseWriter.Write(body[n:])
	return n + m, err
}

type privateSdkMidReader struct {
	reader io.Reader
	owner  *privateSdkMidSession
}

// Count bytes returned by the underlying reader, including bytes with EOF.
func (self *privateSdkMidReader) Read(p []byte) (int, error) {
	n, err := self.reader.Read(p)
	total := self.owner.cssRead.Add(int64(n))
	if total >= nativeBrowsingHeldPrefixBytes {
		self.owner.prefixOnce.Do(func() { close(self.owner.cssPrefix) })
	}
	return n, err
}

// Fixed selected-call rows retain no packet. A partial aggregate never becomes
// a fabricated per-packet disposition. The next-read timestamp is a real call.
type privateSdkMidBridgeRow struct {
	Call                                               uint64 `json:"call"`
	ReturnedReadNs, SendNs, ReturnedSendNs, NextReadNs int64
	Count, ColdSyn, Other, Accepted                    int
	Disposition                                        string `json:"aggregate_disposition"`
	SendEntered, SendReturned, NextReadEntered         bool
	ColdTupleKnown                                     bool
	ColdSourceIp                                       [4]byte
	ColdSourcePort                                     uint16
	ColdSequence                                       uint32
}
type privateSdkMidBridgeTrace struct {
	active          atomic.Bool
	dropped         atomic.Uint64
	stateLock       sync.Mutex
	rows            [32]privateSdkMidBridgeRow
	count, overflow int
	lastCold        uint64
	zero            time.Time
}

// Publish the diagnostic origin before accepting native-call observations.
func (self *privateSdkMidBridgeTrace) arm() {
	self.stateLock.Lock()
	self.zero = time.Now()
	self.stateLock.Unlock()
	self.active.Store(true)
}

// Fence pending appends before reporting the bounded selected trace.
func (self *privateSdkMidBridgeTrace) freeze() {
	self.stateLock.Lock()
	self.active.Store(false)
	self.stateLock.Unlock()
}

// Parse only the exact unfragmented IPv4 cold-target SYN header.
func privateSdkMidColdSyn(p []byte) bool {
	if len(p) < 40 || p[0]>>4 != 4 || p[9] != 6 {
		return false
	}
	h := int(p[0]&15) * 4
	return h >= 20 && len(p) >= h+20 && int(binary.BigEndian.Uint16(p[2:4])) == len(p) && binary.BigEndian.Uint16(p[6:8])&0x3fff == 0 && p[16] == 203 && p[17] == 0 && p[18] == 113 && p[19] == 14 && binary.BigEndian.Uint16(p[h+2:h+4]) == 443 && p[h+13]&2 != 0 && p[h+13]&16 == 0
}

// Stamp the actual next ReadBatch call without claiming a packet is ready.
func (self *privateSdkMidBridgeTrace) readBegin(call uint64) {
	if !self.active.Load() {
		return
	}
	if !self.stateLock.TryLock() {
		self.dropped.Add(1)
		return
	}
	defer self.stateLock.Unlock()
	if !self.active.Load() {
		return
	}
	for i := 0; i < self.count; i++ {
		if self.rows[i].Call+1 == call {
			self.rows[i].NextReadNs = int64(time.Since(self.zero))
			self.rows[i].NextReadEntered = true
		}
	}
}

// Copy role counts only; the native bridge retains sole packet ownership.
func (self *privateSdkMidBridgeTrace) readReturned(call uint64, packets [][]byte) {
	if !self.active.Load() {
		return
	}
	cold := 0
	var sourceIp [4]byte
	var sourcePort uint16
	var sequence uint32
	for _, p := range packets {
		if privateSdkMidColdSyn(p) {
			cold++
			h := int(p[0]&15) * 4
			copy(sourceIp[:], p[12:16])
			sourcePort = binary.BigEndian.Uint16(p[h : h+2])
			sequence = binary.BigEndian.Uint32(p[h+4 : h+8])
		}
	}
	if !self.stateLock.TryLock() {
		self.dropped.Add(1)
		return
	}
	defer self.stateLock.Unlock()
	if !self.active.Load() {
		return
	}
	if cold == 0 && (self.lastCold == 0 || call != self.lastCold+1) {
		return
	}
	if cold != 0 {
		self.lastCold = call
	}
	if self.count == len(self.rows) {
		self.overflow++
		return
	}
	self.rows[self.count] = privateSdkMidBridgeRow{Call: call, ReturnedReadNs: int64(time.Since(self.zero)), Count: len(packets), ColdSyn: cold, Other: len(packets) - cold, Accepted: -1, ColdTupleKnown: cold == 1, ColdSourceIp: sourceIp, ColdSourcePort: sourcePort, ColdSequence: sequence}
	self.count++
}

// Record entry and returned aggregate at the existing native ABI boundary.
func (self *privateSdkMidBridgeTrace) send(call uint64, returned bool, accepted int) {
	if !self.active.Load() {
		return
	}
	if !self.stateLock.TryLock() {
		self.dropped.Add(1)
		return
	}
	defer self.stateLock.Unlock()
	if !self.active.Load() {
		return
	}
	for i := 0; i < self.count; i++ {
		if self.rows[i].Call == call {
			if returned {
				self.rows[i].SendReturned = true
				self.rows[i].ReturnedSendNs = int64(time.Since(self.zero))
				self.rows[i].Accepted = accepted
				self.rows[i].Disposition = privateSdkMidAggregateDisposition(self.rows[i].Count, accepted)
			} else {
				self.rows[i].SendEntered = true
				self.rows[i].SendNs = int64(time.Since(self.zero))
			}
			return
		}
	}
}

// An aggregate exposes complete refusal/acceptance only at its endpoints.
// A partial return never identifies which original packet was consumed.
func privateSdkMidAggregateDisposition(count, accepted int) string {
	if count <= 0 || accepted < 0 || count < accepted {
		return "invalid"
	}
	if accepted == 0 {
		return "all_refused"
	}
	if accepted == count {
		return "all_accepted"
	}
	return "mixed_packet_identity_unknown"
}

// Only fully observed selected boundaries can be used for call attribution.
// Read-entry still does not establish an independently queued packet.
func privateSdkMidTraceComplete(rows []privateSdkMidBridgeRow, overflow int, dropped uint64) bool {
	if len(rows) == 0 || overflow != 0 || dropped != 0 {
		return false
	}
	selected := false
	for _, row := range rows {
		if row.ColdSyn > 0 {
			selected = true
			if !row.ColdTupleKnown || !row.SendEntered || !row.SendReturned || !row.NextReadEntered || row.Disposition == "invalid" {
				return false
			}
		}
	}
	return selected
}

// A refusal arm succeeds only after a different actual provider serves the
// request. Public readiness cannot replace this delivery witness.
func privateSdkMidRefusalRecovery(blocked, response int, refusals [6]uint64) bool {
	if blocked < 0 || len(refusals) <= blocked || response < 0 || len(refusals) <= response || response == blocked || refusals[blocked] == 0 {
		return false
	}
	for ordinal, count := range refusals {
		if ordinal != blocked && count != 0 {
			return false
		}
	}
	return true
}

// Reporting distinguishes a completed workload from unavailable native trace
// attribution. Neither a slow request nor a body hold proves saturated credit.
func (self *privateSdkMidSession) finish(t testing.TB, observation *nativeBrowsingObservation) {
	self.close()
	self.stateLock.Lock()
	refusals := uint64(0)
	for _, n := range self.refusals {
		refusals += n
	}
	complete := observation.CompletedNavigations == 100 && observation.OverlapProofs == 99 && self.bodyPremiseAccepted && self.cssComplete && self.cssClientAttempts == 1 && self.cssServerAttempts == 1 && !self.tap.IsZero() && !self.firstByte.IsZero() && self.firstErr == nil
	for _, done := range self.html {
		complete = complete && done
	}
	if self.sameSite {
		complete = complete && self.sameSiteRequests >= nativeBrowsingPageCount*(1+nativeBrowsingAssetCount)
	}
	if self.cold {
		complete = complete && self.firstDialNs > 0 && !self.navReuse[10] && self.navAttempts[10] > 0
	}
	if self.fault {
		complete = complete && privateSdkMidRefusalRecovery(int(self.blocked.Load()), self.responseProvider, self.refusals)
	} else {
		complete = complete && refusals == 0
	}
	reused := 0
	for i, n := range self.navAttempts {
		if n > 0 && self.navReuse[i] {
			reused++
		}
	}
	report := map[string]any{"version": "mid-session-single-owner-v1", "arm_premise": "stable-public-owned-alternatives", "arm_attempted": self.armAttempted, "arm_generation_stable": self.armGenerationStable, "arm_premise_accepted": self.armPremiseAccepted, "cold": self.cold, "fault": self.fault, "complete_workload_witness": complete, "pages": observation.CompletedNavigations, "overlaps": observation.OverlapProofs, "blocked_provider_ordinal": self.blocked.Load(), "response_provider_ordinal": self.responseProvider, "refusals": self.refusals, "first_dial_ns": self.firstDialNs, "first_refusal_ns": self.firstRefusalNs, "tap_to_first_byte_ns": int64(self.firstByte.Sub(self.tap)), "arm_html_completed": self.armHtmlCompleted, "arm_provider_mask": self.armProviderMask, "arm_css_received": self.armCssBytes, "arm_css_nat_bytes": self.armCssNatBytes, "arm_css_server_live": self.armCssServerLive, "arm_image_received": self.armImageBytes, "arm_image_expected": self.armImageExpected, "css_app_tuple_private": self.cssClientTuple, "css_upstream_tuple_private": self.cssSource, "css_provider_ordinal": self.cssProvider, "css_expected": nativeBrowsingAssetBytes(9, 0), "css_received": self.cssRead.Load(), "css_complete": self.cssComplete, "css_nat_socket_read_bytes": self.cssNatRead, "before": self.before, "html_valid": self.html, "navigation_boundaries": self.navigationTimes, "navigation_reused": self.navReuse, "navigation_acquisitions": self.navAttempts, "same_site_reuse_observed": reused, "page10_reused": self.navReuse[10], "backlog_cause_established": false, "scope": "body overlap is not saturated credit; latency is not native-blocking proof; provider access remains cleanLAN; warmup and controller waits retained"}
	report["same_site_mode"], report["same_site_server_requests"] = self.sameSite, self.sameSiteRequests
	report["purpose_protocol"], report["purpose"] = "separate-workload-and-mechanism-v2", self.purpose
	report["boundary_observed"], report["body_premise_accepted"] = self.boundaryObserved, self.bodyPremiseAccepted
	report["public_alternatives_qualified"] = self.boundaryObserved && self.alternativesErr == nil
	report["public_alternatives_error"] = ""
	if self.alternativesErr != nil {
		report["public_alternatives_error"] = self.alternativesErr.Error()
	}
	mechanismQualified := self.purpose == privateSdkMidMechanismPurpose && complete && self.armPremiseAccepted
	report["mechanism_qualified"] = mechanismQualified
	if self.purpose == privateSdkSameSiteWorkloadPurpose {
		report["arm_premise"] = "not-requested-for-ordinary-workload"
	}
	if self.sameSite {
		report["origin_authority"] = "origin0.test"
	}
	self.finished = true
	self.stateLock.Unlock()
	self.bridge.stateLock.Lock()
	rows := append([]privateSdkMidBridgeRow(nil), self.bridge.rows[:self.bridge.count]...)
	overflow := self.bridge.overflow
	self.bridge.stateLock.Unlock()
	report["native_selected_calls"], report["native_trace_overflow"], report["native_trace_dropped"] = rows, overflow, self.bridge.dropped.Load()
	report["native_attribution_complete"] = privateSdkMidTraceComplete(rows, overflow, self.bridge.dropped.Load())
	report["native_scope"] = "selected cold SYN call and immediately following read; no ready-queue or per-packet acceptance inference; warm-origin twin may have no selected call"
	encoded, err := json.Marshal(report)
	if err != nil || 64*1024 < len(encoded) {
		t.Error("MID_SESSION_REPORT_BOUND")
		return
	}
	t.Logf("SDK_MID_SESSION_V1 %s", encoded)
	if !complete {
		t.Error("MID_SESSION_WORKLOAD_PREMISE_INCOMPLETE")
	}
	if self.purpose == privateSdkMidMechanismPurpose && !mechanismQualified {
		t.Error("MID_SESSION_MECHANISM_PREMISE_UNQUALIFIED")
	}
}

// Use the default-opening public-readiness constructor without changing policy.
func privateSdkRunMidSession(t *testing.T, cold, fault bool) {
	privateSdkRunMidSessionOriginMode(t, cold, fault, false)
}

// The original driver, barriers and timers also run the one-origin variant.
func privateSdkRunMidSessionOriginMode(t *testing.T, cold, fault, sameSite bool) {
	if fault && !cold {
		t.Fatal("mid-session refusal requires its owned cold destination")
	}
	if sameSite && (cold || fault) {
		t.Fatal("same-site comparison requires the warm no-fault workload")
	}
	defaults := clientconnect.DefaultContractManagerSettings()
	if defaults.InitialContractTransferByteCount != 1024*1024 || defaults.InitialNetworkPeerContractTransferByteCount != 1024*1024 {
		t.Fatal("opening-credit premise changed")
	}
	mode, readiness := newPrivateSdkDefaultContractMode(), newPrivateSdkProductionReadinessMode()
	var mid *privateSdkMidSession
	if sameSite {
		mid = newPrivateSdkSameSiteWorkload()
	} else {
		mid = newPrivateSdkMidSession(cold, fault)
	}
	readiness.midSession = mid
	defer mode.close()
	defer readiness.dumpIfUnfinished(t)
	defer mid.close()
	testPrivateSdkProductionReadinessBrowsing(t, mode, readiness)
}
func TestPrivateFullTunH1Sdk32MidSessionColdLinkNoFault100Pages(t *testing.T) {
	privateSdkRunMidSession(t, true, false)
}
func TestPrivateFullTunH1Sdk32MidSessionSingleOwnerRefusal100Pages(t *testing.T) {
	privateSdkRunMidSession(t, true, true)
}
func TestPrivateFullTunH1Sdk32MidSessionWarmOriginsNoFault100Pages(t *testing.T) {
	privateSdkRunMidSession(t, false, false)
}
