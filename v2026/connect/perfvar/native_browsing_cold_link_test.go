// A matched protocol comparison adds one genuinely unvisited link destination.
// Body receipts describe HTTP consumption, never native packet ownership.
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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sdk/v2026"
)

const nativeBrowsingColdLinkOrigin = nativeBrowsingOriginCount

// Fault injection is armed only for old quality providers and the one fresh
// cold target. Default scenarios retain their original origin1 predicate.
func sdkBrowsingNavigationDialRefused(coldLink, armed bool, ordinal int, address string) bool {
	target := "203.0.113.11:443"
	if coldLink {
		target = "203.0.113.14:443"
	}
	return armed && sdkBrowsingInitialQualityProvider(ordinal) && address == target
}

// Every asset keeps the original four-origin map. Only page1 HTML changes.
func nativeBrowsingColdLinkURL(original func(int, int) string, page, asset int) string {
	target := original(page, asset)
	if page == 1 && asset < 0 {
		return strings.Replace(target, "origin1.test", "origin4.test", 1)
	}
	return target
}

func nativeBrowsingColdLinkHTML(originalURL func(int, int) string, originalHTML func(int) []byte, page int) []byte {
	body := originalHTML(page)
	if page == 0 {
		return []byte(strings.Replace(string(body), originalURL(1, -1), nativeBrowsingColdLinkURL(originalURL, 1, -1), 1))
	}
	return body
}

func nativeBrowsingColdLinkRequestSlot(target string) (page, slot, origin int, err error) {
	page, slot, origin, err = nativeBrowsingHttp2RequestSlot(target)
	if err != nil {
		return
	}
	if page == 1 && slot == nativeBrowsingAssetCount {
		origin = nativeBrowsingColdLinkOrigin
	}
	parsed, parseErr := url.Parse(target)
	if parseErr != nil || parsed.Scheme != "https" || parsed.Host != fmt.Sprintf("origin%d.test", origin) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, 0, 0, errors.New("request does not name its owned cold-link plan slot")
	}
	return
}

type nativeBrowsingColdBodyKind uint8

const (
	nativeBrowsingColdCss nativeBrowsingColdBodyKind = iota + 1
	nativeBrowsingColdJs
)

// Each original typed slot owns a body gate independent of client read progress.
type nativeBrowsingColdBodyHold struct {
	kind nativeBrowsingColdBodyKind
	page int
	slot int
	gate *nativeBrowsingHttp2BodyGate
}

type nativeBrowsingColdRequest struct {
	requestOwner       *nativeBrowsingBodyRequestOwner
	bodyGate           *nativeBrowsingHttp2BodyGate
	clientSession      *tls.Conn
	clientAcquisitions int
	clientReused       bool
	serverTuple        string
	serverAttempts     int
	serverProvider     int
	serverOwned        bool
	verifiedClientTls  bool
}

// Fixed receipts bound all500 original requests; socket identities stay private.
type nativeBrowsingColdLinkObservation struct {
	InitialWindow            sdkBrowsingSetupSnapshot         `json:"initial_window"`
	ColdEdgeWindow           sdkBrowsingSetupSnapshot         `json:"cold_edge_window"`
	ColdEdgeWindowObserved   bool                             `json:"cold_edge_window_observed"`
	Protocol                 string                           `json:"inner_protocol"`
	ColdEdges                int                              `json:"cold_edges"`
	VisitedDestinationEdges  int                              `json:"visited_destination_edges"`
	SameSessionEdges         int                              `json:"same_session_edges"`
	Html                     int                              `json:"validated_html"`
	Assets                   int                              `json:"validated_or_canceled_assets"`
	ClientAcquisitions       int                              `json:"client_acquisitions"`
	ServerAttempts           int                              `json:"server_attempts"`
	ClientSessions           int                              `json:"client_sessions"`
	ServerSessions           int                              `json:"server_sessions"`
	CssBodyReadBytes         int64                            `json:"css_body_read_bytes_at_cold_html"`
	JsBodyReadBytes          int64                            `json:"js_body_read_bytes_at_cold_html"`
	CssOwnerRetained         bool                             `json:"css_public_owner_retained"`
	ColdClientReused         bool                             `json:"cold_client_reused"`
	ColdServerProvider       int                              `json:"cold_server_provider"`
	ColdTupleOwned           bool                             `json:"cold_server_tuple_owned"`
	AdmissionChain           string                           `json:"admission_chain"`
	Refusals                 [sdkBrowsingProviderCount]uint64 `json:"actual_old_provider_refusals"`
	FirstRefusalNs           [sdkBrowsingProviderCount]int64  `json:"first_refusal_ns"`
	LateConstructedNs        int64                            `json:"late_constructed_ns"`
	LatePublicApiVisibleNs   int64                            `json:"late_public_api_visible_ns"`
	LateReady                bool                             `json:"late_ready"`
	ControllerWaitNs         [nativeBrowsingPageCount]int64   `json:"controller_wait_ns"`
	CombinedFirstByteNs      [nativeBrowsingPageCount]int64   `json:"tap_to_first_byte_ns"`
	PublicationIsAdmission   bool                             `json:"publication_is_admission"`
	BodyReadIsPacketCustody  bool                             `json:"body_read_is_packet_custody"`
	PhysicalPauseAttribution string                           `json:"physical_pause_attribution"`
}

type nativeBrowsingColdLinkWitness struct {
	stateLock         sync.Mutex
	scenario          *sdkBrowsingScenario
	pressure          *nativeBrowsingPressureObservation
	http2             bool
	css               nativeBrowsingColdBodyHold
	js                [nativeBrowsingPageCount - 1]nativeBrowsingColdBodyHold
	requests          [nativeBrowsingPageCount][nativeBrowsingAssetCount + 1]nativeBrowsingColdRequest
	tap               [nativeBrowsingPageCount]time.Time
	row               nativeBrowsingColdLinkObservation
	firstErr          error
	constructorCtx    context.Context
	constructorCancel context.CancelFunc
}

func newNativeBrowsingColdLinkWitness(scenario *sdkBrowsingScenario, pressure *nativeBrowsingPressureObservation, http2 bool) *nativeBrowsingColdLinkWitness {
	self := &nativeBrowsingColdLinkWitness{scenario: scenario, pressure: pressure, http2: http2,
		css: nativeBrowsingColdBodyHold{kind: nativeBrowsingColdCss, page: 0, slot: 0, gate: newNativeBrowsingHttp2BodyGate()},
		row: nativeBrowsingColdLinkObservation{Protocol: "http/1.1", ColdServerProvider: -1, AdmissionChain: "UNEXERCISED_ADMISSION_CHAIN", PhysicalPauseAttribution: "UNKNOWN"}}
	if http2 {
		self.row.Protocol = "h2"
	}
	if scenario != nil {
		scenario.stateLock.Lock()
		self.row.InitialWindow = scenario.initial
		scenario.stateLock.Unlock()
	}
	self.requests[0][0].bodyGate = self.css.gate
	for page := range self.js {
		self.js[page] = nativeBrowsingColdBodyHold{kind: nativeBrowsingColdJs, page: page, slot: 1, gate: newNativeBrowsingHttp2BodyGate()}
		self.requests[page][1].bodyGate = self.js[page].gate
	}
	return self
}

func (self *nativeBrowsingColdLinkWitness) recordError(err error) {
	if err != nil {
		self.stateLock.Lock()
		if self.firstErr == nil {
			self.firstErr = err
		}
		self.stateLock.Unlock()
	}
}

// Exact request/session receipts prove an original unfinished owner. A click
// never waits for its next body read or for an arbitrary prefix extent.
func (self *nativeBrowsingColdLinkWitness) checkHold(hold *nativeBrowsingColdBodyHold, kind nativeBrowsingColdBodyKind) error {
	if hold == nil || hold.kind != kind || self.pressure == nil {
		return errors.New("cold-link CSS/JS body receipt type differs")
	}
	if kind == nativeBrowsingColdCss {
		if hold != &self.css || hold.page != 0 || hold.slot != 0 {
			return errors.New("cold-link CSS receipt is not its original owner")
		}
	} else if kind != nativeBrowsingColdJs || hold.page < 0 || len(self.js) <= hold.page ||
		hold != &self.js[hold.page] || hold.slot != 1 {
		return errors.New("cold-link JS receipt is not its original owner")
	}
	self.stateLock.Lock()
	request, firstErr := self.requests[hold.page][hold.slot], self.firstErr
	self.stateLock.Unlock()
	if firstErr != nil {
		return firstErr
	}
	target := strings.Replace(nativeBrowsingURL(hold.page, hold.slot), "http://", "https://", 1)
	if err := nativeBrowsingBodyRequestDispatched(request.requestOwner, hold.gate, target); err != nil {
		return err
	}
	if request.bodyGate != hold.gate || request.clientAcquisitions < 0 || 1 < request.clientAcquisitions ||
		request.serverAttempts < 0 || 1 < request.serverAttempts || request.serverAttempts > request.clientAcquisitions ||
		request.clientAcquisitions == 1 && (request.clientSession == nil || !request.verifiedClientTls) ||
		request.clientAcquisitions == 0 && (request.clientSession != nil || request.verifiedClientTls) ||
		request.serverAttempts == 1 && (!request.serverOwned || request.serverTuple == "" || request.serverProvider < 0 || len(self.row.Refusals) <= request.serverProvider) ||
		request.serverAttempts == 0 && (request.serverOwned || request.serverTuple != "") {
		return errors.New("cold-link original request has invalid observed TLS/server ownership")
	}
	progress := &self.pressure.assets[hold.page][hold.slot]
	if (progress.bodyOpened.Load() || progress.receivedBytes.Load() != 0 || kind == nativeBrowsingColdCss) &&
		(request.clientAcquisitions != 1 || request.serverAttempts != 1) {
		return errors.New("cold-link opened body/CSS premise lacks established original ownership")
	}
	return nativeBrowsingUnfinishedBodyOwner(hold.gate, &self.pressure.assets[hold.page][hold.slot],
		nativeBrowsingAssetBytes(hold.page, hold.slot))
}

func (self *nativeBrowsingColdLinkWitness) wrapReader(ctx context.Context, target string, reader io.Reader) io.Reader {
	page, slot, _, err := nativeBrowsingColdLinkRequestSlot(target)
	if err != nil {
		self.recordError(err)
		return reader
	}
	var hold *nativeBrowsingColdBodyHold
	if page == 0 && slot == 0 {
		hold = &self.css
	} else if slot == 1 && page < len(self.js) {
		hold = &self.js[page]
	}
	if hold == nil {
		return reader
	}
	if !hold.gate.wrapped.CompareAndSwap(false, true) {
		self.recordError(errors.New("cold-link original body was wrapped twice"))
		return reader
	}
	return &nativeBrowsingHttp2BodyReader{ctx: ctx, reader: reader, gate: hold.gate}
}

// The tap retains every controller cost. Validate existing request owners
// without waiting for CSS/JS body progress before dispatching the new HTML.
func (self *nativeBrowsingColdLinkWitness) beforeNavigation(ctx context.Context, page int, _ func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	self.stateLock.Lock()
	self.tap[page] = time.Now()
	self.stateLock.Unlock()
	if page == 0 {
		deadline, bounded := ctx.Deadline()
		if !bounded {
			return errors.New("cold-link work is missing its original application deadline")
		}
		// Copy the same absolute deadline, not a new300-second allowance.
		// Normal measurement cancellation does not abandon a started constructor.
		self.constructorCtx, self.constructorCancel = context.WithDeadline(self.scenario.fixture.ctx, deadline)
		self.scenario.stateLock.Lock()
		self.scenario.workCtx = ctx
		self.scenario.coldLinkConstructorCtxForTest = self.constructorCtx
		self.scenario.stateLock.Unlock()
		return nil
	}
	if err := self.checkHold(&self.js[page-1], nativeBrowsingColdJs); err != nil {
		return err
	}
	if page == 1 {
		if err := self.checkHold(&self.css, nativeBrowsingColdCss); err != nil {
			return err
		}
		device := self.scenario.fixture.device
		exits, status := device.GetExits(), device.GetWindowStatus()
		locations := device.GetConnectedProviderLocations()
		snapshot := sdkBrowsingSetupSnapshotFrom(exits, status, locations, self.scenario.fixture.readyProviderIds())
		self.recordColdEdgeWindow(snapshot)
		if err := self.validateInitialCohort(snapshot, locations); err != nil {
			return err
		}
		owner, flows, err := self.scenario.cssOwnerSnapshot(nil)
		if err != nil {
			return err
		}
		self.scenario.native.stateLock.Lock()
		ownedOrigins := self.scenario.native.originCount == nativeBrowsingOriginCount+1 && self.scenario.native.coldLinkOriginForTest && self.scenario.native.coldLinkPortForTest > 0
		coldEndpoint := net.JoinHostPort("127.0.0.1", fmt.Sprint(self.scenario.native.coldLinkPortForTest))
		for _, port := range self.scenario.native.ports {
			ownedOrigins = ownedOrigins && port > 0
		}
		for _, connection := range self.scenario.native.upstreamConns {
			ownedOrigins = ownedOrigins && connection.tuple.destinationAddr != coldEndpoint
		}
		self.scenario.native.stateLock.Unlock()
		self.stateLock.Lock()
		coldRequest := self.requests[page][nativeBrowsingAssetCount]
		tap := self.tap[page]
		self.stateLock.Unlock()
		if !ownedOrigins || coldRequest.clientAcquisitions != 0 || coldRequest.serverAttempts != 0 {
			return errors.New("cold-link destination was unowned or already visited before its link tap")
		}
		self.scenario.stateLock.Lock()
		self.scenario.tapStarted = tap
		self.scenario.atTap, self.scenario.cssOwner, self.scenario.cssFlowsBefore = snapshot, owner, flows
		self.scenario.stateLock.Unlock()
		// The cold target is203.0.113.14; all original four destinations stay
		// healthy, including the established CSS connection just observed.
		self.scenario.rejectNew.Store(true)
	}
	self.stateLock.Lock()
	self.row.ControllerWaitNs[page] = int64(time.Since(self.tap[page]))
	self.stateLock.Unlock()
	return nil
}

// Compatibility tests retain the original entry; real dispatch supplies the
// original request context through the existing driver trace hook.
func (self *nativeBrowsingColdLinkWitness) requestTrace(target string) *httptrace.ClientTrace {
	return self.requestTraceWithContext(context.Background(), target)
}

func (self *nativeBrowsingColdLinkWitness) requestTraceWithContext(ctx context.Context, target string) *httptrace.ClientTrace {
	page, slot, origin, err := nativeBrowsingColdLinkRequestSlot(target)
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
			self.recordError(errors.New("cold-link original request trace registered more than once or without context"))
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
					self.firstErr = errors.New("cold-link stale or foreign request dispatch")
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
			valid := false
			if ok {
				state := secure.ConnectionState()
				valid = state.HandshakeComplete && state.Version >= tls.VersionTLS13 && len(state.VerifiedChains) != 0 && state.ServerName == fmt.Sprintf("origin%d.test", origin) && state.NegotiatedProtocol == self.row.Protocol
			}
			self.stateLock.Lock()
			request := &self.requests[page][slot]
			request.clientAcquisitions++
			request.clientSession, request.clientReused, request.verifiedClientTls = secure, info.Reused, valid
			self.stateLock.Unlock()
			if !valid {
				self.recordError(errors.New("cold-link request did not acquire its verified TLS/protocol session"))
			}
		},
		GotFirstResponseByte: func() {
			if slot != nativeBrowsingAssetCount {
				return
			}
			if page > 0 {
				self.recordError(self.checkHold(&self.js[page-1], nativeBrowsingColdJs))
			}
			if page == 1 {
				self.recordError(self.checkHold(&self.css, nativeBrowsingColdCss))
			}
			self.stateLock.Lock()
			self.row.CombinedFirstByteNs[page] = int64(time.Since(self.tap[page]))
			self.stateLock.Unlock()
		},
	}
}

func (self *nativeBrowsingColdLinkWitness) wrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		target := "https://" + request.Host + request.URL.RequestURI()
		page, slot, origin, err := nativeBrowsingColdLinkRequestSlot(target)
		if err != nil {
			self.recordError(err)
			http.Error(writer, "unowned cold-link request", http.StatusBadRequest)
			return
		}
		wantProtocol := 1
		if self.http2 {
			wantProtocol = 2
		}
		owned := self.scenario.native.originConnection(request)
		self.scenario.native.stateLock.Lock()
		port := self.scenario.native.coldLinkPortForTest
		if origin < nativeBrowsingOriginCount {
			port = self.scenario.native.ports[origin]
		}
		self.scenario.native.stateLock.Unlock()
		endpointOwned := owned != nil && port > 0 && owned.tuple.destinationAddr == net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
		if request.ProtoMajor != wantProtocol || request.TLS == nil || request.TLS.NegotiatedProtocol != self.row.Protocol || !endpointOwned {
			self.recordError(errors.New("cold-link server lacks negotiated protocol/full owned provider tuple"))
		}
		tuple := ""
		provider := -1
		if owned != nil {
			tuple = owned.tuple.sourceAddr + "|" + owned.tuple.destinationAddr
			provider = owned.ordinal
		}
		self.stateLock.Lock()
		row := &self.requests[page][slot]
		row.serverAttempts++
		row.serverTuple, row.serverProvider, row.serverOwned = tuple, provider, endpointOwned
		self.stateLock.Unlock()
		handler.ServeHTTP(writer, request)
	})
}

func (self *nativeBrowsingColdLinkWitness) afterFetch(target string, result nativeBrowsingAssetResult) {
	page, slot, _, err := nativeBrowsingColdLinkRequestSlot(target)
	if err != nil {
		self.recordError(err)
		return
	}
	if slot != nativeBrowsingAssetCount {
		self.stateLock.Lock()
		self.row.Assets++
		self.stateLock.Unlock()
		return
	}
	if result.err == nil && result.bytes == nativeBrowsingHTMLBytes {
		if page > 0 {
			self.recordError(self.checkHold(&self.js[page-1], nativeBrowsingColdJs))
		}
		if page == 1 {
			self.recordError(self.checkHold(&self.css, nativeBrowsingColdCss))
			self.scenario.stateLock.Lock()
			expected := self.scenario.cssOwner
			self.scenario.stateLock.Unlock()
			_, flows, ownerErr := self.scenario.cssOwnerSnapshot(expected)
			self.recordError(ownerErr)
			self.stateLock.Lock()
			self.row.CssOwnerRetained = ownerErr == nil && expected != nil && flows > 0
			self.row.CssBodyReadBytes = self.pressure.assets[0][0].receivedBytes.Load()
			self.row.JsBodyReadBytes = self.pressure.assets[0][1].receivedBytes.Load()
			self.stateLock.Unlock()
		}
		self.stateLock.Lock()
		self.row.Html++
		current := self.requests[page][slot]
		if page == 1 {
			self.row.ColdEdges++
			self.row.ColdClientReused, self.row.ColdServerProvider, self.row.ColdTupleOwned = current.clientReused, current.serverProvider, current.serverOwned
		} else if page > 1 {
			self.row.VisitedDestinationEdges++
			prior := self.requests[page-1][1]
			if current.clientSession == prior.clientSession && current.serverTuple == prior.serverTuple && current.clientReused {
				self.row.SameSessionEdges++
			}
		}
		self.stateLock.Unlock()
	}
	if page == 1 {
		self.scenario.rejectNew.Store(false)
		self.css.gate.release()
	}
	if page > 0 {
		self.js[page-1].gate.release()
	}
}

func (self *nativeBrowsingColdLinkWitness) joinLateConstructor() error {
	self.scenario.stateLock.Lock()
	// Postmeasurement closes the same admission gate as Stop before sampling.
	// A callback already past rejectNew cannot register after a no-owner result.
	self.scenario.coldLinkClosingForTest = true
	started := self.scenario.lateStarted
	self.scenario.stateLock.Unlock()
	if !started {
		return nil // A healthy alternate path started no optional worker.
	}
	if self.constructorCtx == nil {
		return errors.New("cold-link constructor has no bounded owner")
	}
	return sdkBrowsingAwaitConstructor(self.constructorCtx, self.scenario.lateDone)
}

func (self *nativeBrowsingColdLinkWitness) close() {
	self.css.gate.release()
	for index := range self.js {
		self.js[index].gate.release()
	}
	if self.constructorCancel != nil {
		self.constructorCancel()
	}
}

func (self *nativeBrowsingColdLinkWitness) snapshot() (nativeBrowsingColdLinkObservation, error) {
	self.scenario.stateLock.Lock()
	refusals, times := self.scenario.refusalCounts, self.scenario.refusalNs
	constructed, visible, ready := self.scenario.lateConstructedNs, self.scenario.lateApiVisibleNs, self.scenario.lateReady
	scenarioErr := self.scenario.firstErr
	self.scenario.stateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	row := self.row
	row.Refusals, row.FirstRefusalNs, row.LateConstructedNs, row.LatePublicApiVisibleNs, row.LateReady = refusals, times, constructed, visible, ready
	clientSessions := map[*tls.Conn]bool{}
	serverSessions := map[string]bool{}
	for page := range self.requests {
		for _, request := range self.requests[page] {
			row.ClientAcquisitions += request.clientAcquisitions
			row.ServerAttempts += request.serverAttempts
			if request.clientSession != nil {
				clientSessions[request.clientSession] = true
			}
			if request.serverTuple != "" {
				serverSessions[request.serverTuple] = true
			}
		}
	}
	row.ClientSessions, row.ServerSessions = len(clientSessions), len(serverSessions)
	var count uint64
	for ordinal, value := range refusals {
		if sdkBrowsingInitialQualityProvider(ordinal) {
			count += value
		}
	}
	coldRequest := self.requests[1][nativeBrowsingAssetCount]
	if count > 0 && ready && constructed > 0 && visible >= constructed && row.ColdEdges == 1 && row.ColdServerProvider == 4 && row.ColdTupleOwned && !row.ColdClientReused && coldRequest.clientSession != nil && coldRequest.verifiedClientTls && coldRequest.clientAcquisitions == 1 && coldRequest.serverAttempts == 1 {
		row.AdmissionChain = "LATE_PROVIDER_ADMITTED"
	}
	return row, errors.Join(self.firstErr, scenarioErr)
}

func (self *nativeBrowsingColdLinkWitness) verdict() error {
	row, err := self.snapshot()
	if err != nil {
		return err
	}
	if row.Html != 100 || row.Assets != 400 || row.ClientAcquisitions != 500 || row.ServerAttempts != 500 || row.ColdEdges != 1 || row.VisitedDestinationEdges != 98 || !row.CssOwnerRetained || row.ColdClientReused || !row.ColdTupleOwned {
		return fmt.Errorf("cold-link original request/body/session census: %+v", row)
	}
	if self.http2 && (row.SameSessionEdges != 98 || row.ClientSessions != 5 || row.ServerSessions != 5) {
		return errors.New("cold-link H2 did not retain98warm same-session edges")
	}
	self.stateLock.Lock()
	var requestErr error
	for page := range self.requests {
		for slot, request := range self.requests[page] {
			if request.clientAcquisitions != 1 || request.clientSession == nil || !request.verifiedClientTls || request.serverAttempts != 1 || !request.serverOwned || request.serverTuple == "" || request.serverProvider < 0 || len(self.row.Refusals) <= request.serverProvider {
				requestErr = fmt.Errorf("cold-link request lacked one verified client TLS and owned full server tuple: page=%d slot=%d provider=%d provider_limit=%d client_acquisitions=%d client_tls=%t verified_tls=%t server_attempts=%d server_owned=%t server_tuple_present=%t", page, slot, request.serverProvider, len(self.row.Refusals), request.clientAcquisitions, request.clientSession != nil, request.verifiedClientTls, request.serverAttempts, request.serverOwned, request.serverTuple != "")
				break
			}
		}
		if requestErr != nil {
			break
		}
	}
	self.stateLock.Unlock()
	if requestErr != nil {
		return requestErr
	}
	for page, duration := range row.CombinedFirstByteNs {
		if duration <= 0 || duration >= int64(sdk.DefaultDeviceLocalSettings().SendTimeout) {
			return fmt.Errorf("NATIVE_BROWSING_NAVIGATION_STALL: page=%d combined_tap_to_first_byte=%s", page, time.Duration(duration))
		}
	}
	return nil
}
