// A real next-navigation refusal introduces the physically late provider into
// an already full public SDK window while its original CSS socket stays live.
package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Only fixed-size scalar receipts are serialized. Contexts, IDs and socket
// identities remain private. stateLock never spans SDK calls or socket I/O.
type sdkBrowsingScenario struct {
	fixture                                                                      *sdkBrowsingSetupFixture
	native                                                                       *sdkBrowsingNative
	plan                                                                         *nativeBrowsingPressurePlan
	stateLock                                                                    sync.Mutex
	workCtx                                                                      context.Context
	firstErr                                                                     error
	jsDone, prefixRead, suffixWritten, readHeld, release, firstRefusal, lateDone chan struct{}
	jsOnce, prefixOnce, heldOnce, releaseOnce, refusalOnce, stopOnce             sync.Once
	stopErr                                                                      error
	lateStarted, lateReady, lateJoined                                           bool
	rejectNew                                                                    atomic.Bool
	coldLinkForTest                                                              bool
	coldLinkConstructorCtxForTest                                                context.Context
	coldLinkClosingForTest                                                       bool
	cssServerStarted, cssServerFinished                                          atomic.Bool
	cssGotConn                                                                   atomic.Int64
	cssClientBytes                                                               atomic.Int64
	htmlFetches, assetFetches                                                    atomic.Int64
	cssSocket                                                                    *sdkBrowsingOriginConn
	cssClientTuple                                                               string
	readArmed, readClaimed                                                       bool
	positiveHeldBytes                                                            int
	cssReceivedBytes                                                             int
	cssComplete, heldThroughHtml                                                 bool
	cssOwner                                                                     *sdk.Id
	cssFlowsBefore, cssFlowsAfter                                                int32
	cssOwnerRetained                                                             bool
	page1Provider                                                                int
	acceptOwnedSpeedRecovery                                                     bool
	tapStarted                                                                   time.Time
	preTapNs, combinedFirstByteNs, page1FirstByteNs                              int64
	refusalNs                                                                    [6]int64
	refusalCounts                                                                [6]uint64
	lateConstructedNs, lateApiVisibleNs                                          int64
	initial, atCss, atTap, afterFirstRefusal                                     sdkBrowsingSetupSnapshot
}

// Channels expose actual lifecycle edges without driving SDK wake or timers.
func newSdkBrowsingScenario(plan *nativeBrowsingPressurePlan) *sdkBrowsingScenario {
	return &sdkBrowsingScenario{plan: plan, jsDone: make(chan struct{}), prefixRead: make(chan struct{}), suffixWritten: make(chan struct{}), readHeld: make(chan struct{}), release: make(chan struct{}), firstRefusal: make(chan struct{}), lateDone: make(chan struct{}), page1Provider: -1}
}

// Handler and callback errors are returned later on the owning test goroutine.
func (self *sdkBrowsingScenario) recordError(err error) {
	if err == nil {
		return
	}
	self.stateLock.Lock()
	if self.firstErr == nil {
		self.firstErr = err
	}
	self.stateLock.Unlock()
}

// A public view is an observation, not an atomic per-flow generation receipt.
func (self *sdkBrowsingScenario) snapshot() sdkBrowsingSetupSnapshot {
	return sdkBrowsingSetupSnapshotFrom(self.fixture.device.GetExits(), self.fixture.device.GetWindowStatus(), self.fixture.device.GetConnectedProviderLocations(), self.fixture.readyProviderIds())
}

// The expected local CSS owner must remain unique, live, quality and flowful.
// Warning is allowed after setup because existing CSS may survive a warning.
func (self *sdkBrowsingScenario) cssOwnerSnapshot(expected *sdk.Id) (*sdk.Id, int32, error) {
	exits, bindings := self.fixture.device.GetExits(), self.fixture.device.GetDestinationExits()
	if exits == nil || bindings == nil {
		return nil, 0, errors.New("CSS public flow views unavailable")
	}
	var owner *sdk.Id
	for index := 0; index < bindings.Len(); index++ {
		binding := bindings.Get(index)
		if binding == nil || binding.DestinationIp != "203.0.113.10" {
			continue
		}
		if binding.ClientId == nil || binding.FlowCount <= 0 || owner != nil && owner.String() != binding.ClientId.String() {
			return nil, 0, errors.New("CSS public local owner ambiguous or empty")
		}
		owner = binding.ClientId
	}
	if owner == nil || expected != nil && owner.String() != expected.String() {
		return nil, 0, errors.New("CSS public local owner missing or changed")
	}
	for index := 0; index < exits.Len(); index++ {
		exit := exits.Get(index)
		if exit != nil && exit.ClientId != nil && exit.ClientId.String() == owner.String() && exit.WindowType == "quality" && !exit.Done && exit.FlowCount > 0 {
			return owner, exit.FlowCount, nil
		}
	}
	return nil, 0, errors.New("CSS local quality owner no longer live with a flow")
}

// The callback must register ownership before it can escape Stop's sample.
// The same stateLock seals registration before teardown joins existing owners.
func (self *sdkBrowsingScenario) registerColdLinkConstructorWithLock() (context.Context, bool) {
	if !self.coldLinkForTest || self.coldLinkClosingForTest || self.lateStarted {
		return nil, false
	}
	ctx := self.coldLinkConstructorCtxForTest
	if ctx == nil {
		if self.firstErr == nil {
			self.firstErr = errors.New("cold-link refusal preceded its bounded work owner")
		}
		return nil, false
	}
	self.lateStarted = true
	return ctx, true
}

// Only the actual old-provider origin1 TCP dial is refused. All other traffic,
// including the established origin0 CSS socket, keeps its ordinary lifetime.
func (self *sdkBrowsingScenario) rejectNavigationDial(ordinal int, address string) bool {
	if !sdkBrowsingNavigationDialRefused(self.coldLinkForTest, self.rejectNew.Load(), ordinal, address) {
		return false
	}
	self.stateLock.Lock()
	self.refusalCounts[ordinal]++
	if self.refusalNs[ordinal] == 0 && !self.tapStarted.IsZero() {
		self.refusalNs[ordinal] = int64(time.Since(self.tapStarted))
	}
	constructorCtx, start := self.registerColdLinkConstructorWithLock()
	self.stateLock.Unlock()
	self.refusalOnce.Do(func() { close(self.firstRefusal) })
	if start {
		// Construction stays off the NAT dial callback stack. Even if Stop
		// seals now, it sees this registered owner and joins lateDone first.
		go self.buildLate(constructorCtx)
	}
	return true
}

// Holding occurs only after a positive read on the exact original socket.
// The original buffer and returned count/error are unchanged on release.
func (self *sdkBrowsingScenario) holdOriginRead(connection *sdkBrowsingOriginConn, count int) {
	if count <= 0 {
		return
	}
	self.stateLock.Lock()
	claim := self.readArmed && !self.readClaimed && self.cssSocket == connection
	if claim {
		self.readClaimed = true
		self.positiveHeldBytes = count
	}
	ctx := self.workCtx
	self.stateLock.Unlock()
	if !claim {
		return
	}
	self.heldOnce.Do(func() { close(self.readHeld) })
	select {
	case <-self.release:
	case <-ctx.Done():
	}
}

// Original generated URLs alone select callbacks; no requests are added.
func sdkBrowsingRequestPath(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}
	return parsed.Path
}

// GotConn counts prohibit reconnect/replay of the controlled CSS request.
func (self *sdkBrowsingScenario) requestTrace(target string) *httptrace.ClientTrace {
	switch sdkBrowsingRequestPath(target) {
	case "/asset/0/0":
		return &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			self.cssGotConn.Add(1)
			self.stateLock.Lock()
			if self.cssClientTuple == "" {
				self.cssClientTuple = info.Conn.LocalAddr().String() + "|" + info.Conn.RemoteAddr().String()
			}
			self.stateLock.Unlock()
		}}
	case "/page/1":
		return &httptrace.ClientTrace{GotFirstResponseByte: func() {
			self.stateLock.Lock()
			if self.combinedFirstByteNs == 0 && !self.tapStarted.IsZero() {
				self.combinedFirstByteNs = int64(time.Since(self.tapStarted))
			}
			self.stateLock.Unlock()
		}}
	}
	return nil
}

// The controller setup remains inside the original300-second application owner.
func (self *sdkBrowsingScenario) beforePage(ctx context.Context, page int, closeIdle func()) error {
	if page == 0 {
		self.stateLock.Lock()
		self.workCtx = ctx
		self.stateLock.Unlock()
		return nil
	}
	if page != 1 {
		return nil
	}
	self.stateLock.Lock()
	self.tapStarted = time.Now()
	self.stateLock.Unlock()
	for _, event := range []<-chan struct{}{self.prefixRead, self.suffixWritten, self.readHeld, self.jsDone} {
		select {
		case <-event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	self.stateLock.Lock()
	err := self.firstErr
	socket := self.cssSocket
	self.stateLock.Unlock()
	if err != nil {
		return err
	}
	if socket == nil || socket.ordinal < 0 || socket.ordinal >= 4 || self.cssClientBytes.Load() != nativeBrowsingHeldPrefixBytes || self.cssGotConn.Load() != 1 || self.cssServerFinished.Load() {
		return errors.New("SDK_BROWSING_CSS_ORIGINAL_OWNERSHIP")
	}
	snapshot, locations := self.snapshotForOwnership()
	self.stateLock.Lock()
	self.atTap = snapshot
	self.stateLock.Unlock()
	if err := self.validateOwnedPhase(snapshot, locations, sdkBrowsingPremiseTap); err != nil {
		return fmt.Errorf("SDK_BROWSING_FULL_WINDOW_AT_TAP: %s: %v", snapshot, err)
	}
	owner, flows, err := self.cssOwnerSnapshot(nil)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	self.cssOwner, self.cssFlowsBefore = owner, flows
	self.lateStarted = true
	self.stateLock.Unlock()
	self.rejectNew.Store(true)
	closeIdle()
	go self.buildLate(ctx)
	self.stateLock.Lock()
	self.preTapNs = int64(time.Since(self.tapStarted))
	self.stateLock.Unlock()
	return nil
}

// The cold constructor shares the exact application deadline through real
// fixture cancellation, including work that uses fixture/setup contexts inside
// buildProvider. Its cancellation callback only invokes those CancelFuncs.
func (self *sdkBrowsingScenario) linkColdLinkConstructorCancellation(ctx context.Context) func() {
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(joined)
		self.fixture.cancelSetup()
		self.fixture.cancel()
	})
	return func() {
		if !stop() {
			<-joined
		}
	}
}

// The followup view is taken off the provider dial callback stack, after the
// first actual refusal. It is explicitly a followup, not an atomic SYN snapshot.
func (self *sdkBrowsingScenario) buildLate(ctx context.Context) {
	defer func() {
		if recover() != nil {
			self.recordError(errors.New("late provider constructor panic"))
		}
		close(self.lateDone)
	}()
	if self.coldLinkForTest {
		// This defer runs before lateDone closes, including error/panic paths.
		defer self.linkColdLinkConstructorCancellation(ctx)()
	}
	select {
	case <-self.firstRefusal:
	case <-ctx.Done():
		self.recordError(ctx.Err())
		return
	}
	if err := ctx.Err(); err != nil {
		self.recordError(err)
		return
	}
	snapshot, locations := self.snapshotForOwnership()
	self.stateLock.Lock()
	self.afterFirstRefusal = snapshot
	self.stateLock.Unlock()
	if err := self.validateOwnedPhase(snapshot, locations, sdkBrowsingPremiseAfterRefusal); err != nil {
		self.recordError(fmt.Errorf("SDK_BROWSING_FULL_WINDOW_AFTER_REFUSAL: %s: %v", snapshot, err))
		return
	}
	if err := self.fixture.buildProvider(4, self.plan); err != nil {
		self.recordError(fmt.Errorf("late public provider: %w", err))
		return
	}
	self.stateLock.Lock()
	self.lateConstructedNs = int64(time.Since(self.tapStarted))
	self.stateLock.Unlock()
	id := self.fixture.providers[4].id
	details := model.GetProvideRelationshipDetails(ctx, self.fixture.consumerClientId, server.Id(id))
	if details.Mode != model.ProvideModePublic || details.SourceLifecycle != model.NetworkClientLifecycleActiveTop || details.DestinationLifecycle != model.NetworkClientLifecycleActiveTop {
		self.recordError(errors.New("late actual public relationship missing"))
		return
	}
	// This named fixture probe observes visibility; it supplies no SDK candidate,
	// clears no exclusion and is excluded from the wrapper's SDK-call counters.
	result, err := self.fixture.discovery.base.FindProviders2(ctx, self.fixture.callerToken, &clientconnect.FindProviders2Args{Specs: []*clientconnect.ProviderSpec{{ClientId: &id}}, Count: 1, RankMode: "quality", IpFamily: clientconnect.IpFamilyFilterV4Capable})
	if err != nil || result == nil || len(result.Providers) != 1 || result.Providers[0].ClientId != id {
		self.recordError(errors.New("late actual named API visibility missing"))
		return
	}
	self.stateLock.Lock()
	self.lateReady = true
	self.lateApiVisibleNs = int64(time.Since(self.tapStarted))
	self.stateLock.Unlock()
}

// One handler wraps the original body generator and server ownership receipts.
func (self *sdkBrowsingScenario) wrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/page/1" {
			ordinal, known := self.native.provider(request)
			if !known {
				self.recordError(errors.New("page1 upstream provider unknown"))
			}
			self.stateLock.Lock()
			self.page1Provider = ordinal
			self.stateLock.Unlock()
		}
		if request.URL.Path != "/asset/0/0" {
			handler.ServeHTTP(writer, request)
			return
		}
		if !self.cssServerStarted.CompareAndSwap(false, true) {
			self.recordError(errors.New("CSS request replay"))
			http.Error(writer, "duplicate CSS", http.StatusConflict)
			return
		}
		defer self.cssServerFinished.Store(true)
		socket := self.native.originConnection(request)
		if socket == nil {
			self.recordError(errors.New("CSS upstream socket unknown"))
			http.Error(writer, "unowned CSS", http.StatusServiceUnavailable)
			return
		}
		snapshot, locations := self.snapshotForOwnership()
		self.stateLock.Lock()
		self.cssSocket, self.atCss = socket, snapshot
		self.stateLock.Unlock()
		if err := self.validateOwnedPhase(snapshot, locations, sdkBrowsingPremiseCss); err != nil {
			self.recordError(fmt.Errorf("SDK_BROWSING_FULL_WINDOW_AT_CSS: %s: %v", snapshot, err))
		}
		handler.ServeHTTP(&sdkBrowsingCssWriter{ResponseWriter: writer, ctx: request.Context(), scenario: self}, request)
	})
}

// Counts and errors belong to the actual underlying writer, including partials.
type sdkBrowsingCssWriter struct {
	http.ResponseWriter
	ctx      context.Context
	scenario *sdkBrowsingScenario
}

// Split only this generated12KiB body; all other writers and handlers are intact.
func (self *sdkBrowsingCssWriter) Write(body []byte) (int, error) {
	if len(body) != nativeBrowsingAssetBytes(0, 0) {
		return 0, errors.New("unexpected CSS extent")
	}
	count, err := self.ResponseWriter.Write(body[:nativeBrowsingHeldPrefixBytes])
	if err != nil || count != nativeBrowsingHeldPrefixBytes {
		if err == nil {
			err = io.ErrShortWrite
		}
		return count, err
	}
	flusher, ok := self.ResponseWriter.(http.Flusher)
	if !ok {
		return count, errors.New("CSS writer cannot flush")
	}
	flusher.Flush()
	select {
	case <-self.scenario.prefixRead:
	case <-self.ctx.Done():
		return count, self.ctx.Err()
	}
	self.scenario.stateLock.Lock()
	self.scenario.readArmed = true
	self.scenario.stateLock.Unlock()
	suffix, err := self.ResponseWriter.Write(body[nativeBrowsingHeldPrefixBytes:])
	count += suffix
	if err != nil || suffix != len(body)-nativeBrowsingHeldPrefixBytes {
		if err == nil {
			err = io.ErrShortWrite
		}
		return count, err
	}
	flusher.Flush()
	close(self.scenario.suffixWritten)
	select {
	case <-self.scenario.release:
		return count, nil
	case <-self.ctx.Done():
		return count, self.ctx.Err()
	}
}

// The original reader supplies every byte and error; only its first extent caps.
type sdkBrowsingCssReader struct {
	reader   io.Reader
	scenario *sdkBrowsingScenario
	read     int
}

// The1024-byte boundary precedes arming the actual provider socket read hold.
func (self *sdkBrowsingCssReader) Read(buffer []byte) (int, error) {
	if self.read < nativeBrowsingHeldPrefixBytes && nativeBrowsingHeldPrefixBytes-self.read < len(buffer) {
		buffer = buffer[:nativeBrowsingHeldPrefixBytes-self.read]
	}
	count, err := self.reader.Read(buffer)
	self.read += count
	self.scenario.cssClientBytes.Add(int64(count))
	if self.read == nativeBrowsingHeldPrefixBytes {
		self.scenario.prefixOnce.Do(func() { close(self.scenario.prefixRead) })
	}
	return count, err
}

// This callback runs after validation, Body.Close and original progress defers.
func (self *sdkBrowsingScenario) afterFetch(target string, result nativeBrowsingAssetResult) {
	path := sdkBrowsingRequestPath(target)
	if strings.HasPrefix(path, "/page/") {
		self.htmlFetches.Add(1)
	} else if strings.HasPrefix(path, "/asset/") {
		self.assetFetches.Add(1)
	}
	switch path {
	case "/asset/0/1":
		if result.err != nil || result.bytes != nativeBrowsingAssetBytes(0, 1) {
			self.recordError(errors.New("page0 JS did not validate"))
		}
		self.jsOnce.Do(func() { close(self.jsDone) })
	case "/page/1":
		self.stateLock.Lock()
		expected := self.cssOwner
		self.stateLock.Unlock()
		_, flows, err := self.cssOwnerSnapshot(expected)
		self.stateLock.Lock()
		self.cssFlowsAfter, self.cssOwnerRetained = flows, err == nil && expected != nil
		self.page1FirstByteNs = int64(result.firstByte)
		self.heldThroughHtml = result.err == nil && result.bytes == nativeBrowsingHTMLBytes && !self.cssServerFinished.Load() && self.cssClientBytes.Load() == nativeBrowsingHeldPrefixBytes && self.readClaimed && self.positiveHeldBytes > 0 && self.cssGotConn.Load() == 1 && self.cssOwnerRetained
		self.stateLock.Unlock()
		if result.err == nil {
			self.recordError(err)
		}
		self.rejectNew.Store(false)
		self.releaseOnce.Do(func() { close(self.release) })
	case "/asset/0/0":
		self.stateLock.Lock()
		self.cssReceivedBytes = result.bytes
		self.cssComplete = result.err == nil && result.bytes == nativeBrowsingAssetBytes(0, 0) && self.cssGotConn.Load() == 1 && self.cssClientTuple != "" && self.cssSocket != nil && 0 <= self.cssSocket.ordinal && self.cssSocket.ordinal < 4 && self.readClaimed
		self.stateLock.Unlock()
	}
}

// No database reset can overtake late constructor disposal. If the bounded
// guard fires, retain the existing TestEnv owner until the canceled work joins.
func (self *sdkBrowsingScenario) stop() error {
	self.stopOnce.Do(func() {
		self.rejectNew.Store(false)
		self.releaseOnce.Do(func() { close(self.release) })
		if self.coldLinkForTest {
			self.stateLock.Lock()
			self.coldLinkClosingForTest = true
			self.stateLock.Unlock()
		}
		self.fixture.cancelSetup()
		self.stateLock.Lock()
		started := self.lateStarted
		self.stateLock.Unlock()
		if !started {
			return
		}
		select {
		case <-self.lateDone:
		default:
			self.fixture.cancel()
			select {
			case <-self.lateDone:
			case <-time.After(30 * time.Second):
				self.stopErr = errors.New("late constructor join guard expired; retaining database owner")
				self.fixture.t.Error(self.stopErr)
				<-self.lateDone
			}
		}
		self.stateLock.Lock()
		self.lateJoined = true
		self.stateLock.Unlock()
	})
	return self.stopErr
}

// Exact body ownership complements the existing body/hash/cancellation verdict.
func nativeBrowsingAssetBodyOwnershipError(pressure *nativeBrowsingPressureObservation) error {
	if pressure == nil {
		return errors.New("asset response body ownership unavailable")
	}
	for page := range pressure.assets {
		for asset := range pressure.assets[page] {
			progress := &pressure.assets[page][asset]
			opened, closed := progress.bodyOpened.Load(), progress.bodyClosed.Load()
			if !progress.started.Load() || !progress.finished.Load() || opened != closed || progress.bodyCloseError.Load() || !opened && (!progress.cancelRequested.Load() || progress.responseStatus.Load() != 0) {
				return fmt.Errorf("asset response body ownership: page=%d asset=%d opened=%t closed=%t", page, asset, opened, closed)
			}
		}
	}
	drain := pressure.assetOwnershipSnapshot()
	if drain.ClientStarted != 400 || drain.ClientFinished != 400 || drain.BodiesOpened != drain.BodiesClosed || 400-drain.BodiesOpened > drain.Canceled || drain.ServerStarted != 400 || drain.ServerFinished != 400 || drain.ServerAttempts != 400 || drain.ServerFinishedAttempts != 400 || drain.ServerActiveAttempts != 0 || drain.InvalidResponseStatuses != 0 || drain.ServerWriteErrors != 0 || drain.ServerShortWrites != 0 || drain.ClientRequestWriteErrors != 0 || drain.ClientReacquisitions != 0 || drain.RetriedAssets != 0 {
		return fmt.Errorf("asset attempt/disposal census: %+v", drain)
	}
	return nil
}

// Original request timing and the controller-inclusive tap both retain the
// five-second threshold; full-window input receives no eligibility waiver.
func (self *sdkBrowsingScenario) verdict(observation *nativeBrowsingObservation) error {
	self.stateLock.Lock()
	started, ctx := self.lateStarted, self.workCtx
	self.stateLock.Unlock()
	if started {
		if err := sdkBrowsingAwaitConstructor(ctx, self.lateDone); err != nil {
			return err
		}
	}
	self.stateLock.Lock()
	err := self.firstErr
	qualified := self.cssComplete && self.heldThroughHtml && self.lateReady && self.cssOwnerRetained && self.cssFlowsBefore > 0 && self.cssFlowsAfter > 0
	provider := self.page1Provider
	combined := time.Duration(self.combinedFirstByteNs)
	self.stateLock.Unlock()
	if err != nil {
		return err
	}
	if !qualified || self.htmlFetches.Load() != 100 || self.assetFetches.Load() != 400 || observation.CompletedNavigations != 100 || observation.OverlapProofs != 99 {
		return errors.New("SDK_BROWSING_PUBLIC_FULL_WINDOW_CUSTODY_OR_WORKLOAD")
	}
	var mechanismErr, latencyErr error
	if provider != 4 && !(self.acceptOwnedSpeedRecovery && provider == 5) {
		mechanismErr = fmt.Errorf("SDK_BROWSING_LATE_PROVIDER_NOT_EXERCISED: page1_provider=%d; existing-route recovery is not a latency regression", provider)
	}
	if combined <= 0 || sdk.DefaultDeviceLocalSettings().SendTimeout <= combined {
		latencyErr = fmt.Errorf("NATIVE_BROWSING_NAVIGATION_STALL: controller_tap_to_next_html_first_byte=%s", combined)
	}
	return errors.Join(mechanismErr, latencyErr, nativeBrowsingAssetBodyOwnershipError(observation.Pressure), nativeBrowsingPressureLatencyError(observation))
}

// A completed result survives parent cancellation; pending work retains its
// cancellation error. The second check closes the done-vs-cancel selection race.
func sdkBrowsingAwaitConstructor(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// Bounded receipts precede assertions and contain no synthetic IDs or tuples.
func (self *sdkBrowsingScenario) log(t testing.TB, observation *nativeBrowsingObservation) {
	t.Helper()
	self.stateLock.Lock()
	report := struct {
		AcceptOwnedSpeedRecovery                 bool   `json:"accept_owned_speed_recovery"`
		RuntimeProcs                             int    `json:"gomaxprocs"`
		Scope                                    string `json:"scope"`
		Initial, AtCss, AtTap, AfterFirstRefusal sdkBrowsingSetupSnapshot
		Html                                     int64                           `json:"html"`
		Assets                                   int64                           `json:"assets"`
		CssExpected                              int                             `json:"css_expected"`
		CssReceived                              int                             `json:"css_received"`
		CssOriginal                              bool                            `json:"css_original_tuple"`
		HeldThroughHtml                          bool                            `json:"css_held_through_html"`
		OwnerRetained                            bool                            `json:"css_public_owner_retained"`
		FlowsBefore                              int32                           `json:"css_flows_before"`
		FlowsAfter                               int32                           `json:"css_flows_after"`
		PositiveHeldRead                         int                             `json:"positive_held_socket_read_bytes"`
		Page1Provider                            int                             `json:"page1_provider_ordinal"`
		PreTapNs                                 int64                           `json:"pre_tap_ns"`
		CombinedNs                               int64                           `json:"combined_first_byte_ns"`
		Page1Ns                                  int64                           `json:"page1_first_byte_ns"`
		RefusalNs                                [6]int64                        `json:"first_refusal_ns"`
		Refusals                                 [6]uint64                       `json:"refusal_counts"`
		LateConstructedNs                        int64                           `json:"late_constructed_ns"`
		LateApiVisibleNs                         int64                           `json:"late_api_visible_ns"`
		LateReady                                bool                            `json:"late_public_api_visible"`
		LateJoined                               bool                            `json:"late_constructor_joined"`
		Discovery                                sdkBrowsingSetupDiscoveryReport `json:"discovery"`
		AfterJoin                                *sdk.DeviceLocalMemoryUsage     `json:"owner_after_join"`
		UnpublishedKnown                         bool                            `json:"unpublished_offers_known"`
		QuiescenceKnown                          bool                            `json:"quiescence_known"`
	}{AcceptOwnedSpeedRecovery: self.acceptOwnedSpeedRecovery, RuntimeProcs: runtime.GOMAXPROCS(0), Scope: "changed public full-window input; actual SDK32/H1; canonical aggregate pressure boundaries unavailable; one named fixture API probe is observational; source refusal followup views are non-atomic; no forced wake or fixture aging", Initial: self.initial, AtCss: self.atCss, AtTap: self.atTap, AfterFirstRefusal: self.afterFirstRefusal, Html: self.htmlFetches.Load(), Assets: self.assetFetches.Load(), CssExpected: nativeBrowsingAssetBytes(0, 0), CssReceived: self.cssReceivedBytes, CssOriginal: self.cssComplete, HeldThroughHtml: self.heldThroughHtml, OwnerRetained: self.cssOwnerRetained, FlowsBefore: self.cssFlowsBefore, FlowsAfter: self.cssFlowsAfter, PositiveHeldRead: self.positiveHeldBytes, Page1Provider: self.page1Provider, PreTapNs: self.preTapNs, CombinedNs: self.combinedFirstByteNs, Page1Ns: self.page1FirstByteNs, RefusalNs: self.refusalNs, Refusals: self.refusalCounts, LateConstructedNs: self.lateConstructedNs, LateApiVisibleNs: self.lateApiVisibleNs, LateReady: self.lateReady, LateJoined: self.lateJoined, Discovery: self.fixture.discovery.snapshot(), AfterJoin: self.fixture.afterJoin}
	self.stateLock.Unlock()
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) > 16*1024 {
		t.Errorf("bounded public full-window report: bytes=%d err=%v", len(encoded), err)
	} else {
		t.Logf("[perfvar] sdk-browsing-full-window %s", encoded)
	}
	encoded, err = json.Marshal(struct {
		Started            uint64    `json:"started"`
		Completed          uint64    `json:"completed"`
		Accepted           uint64    `json:"accepted"`
		Refused            uint64    `json:"refused"`
		Maximum            int64     `json:"maximum_batch_packets"`
		Invalid            bool      `json:"invalid"`
		Dials              [6]uint64 `json:"origin_dials"`
		Closes             [6]uint64 `json:"origin_closes"`
		PerPacketMaskKnown bool      `json:"per_packet_mask_known"`
	}{Started: self.native.bridgeStarted.Load(), Completed: self.native.bridgeCompleted.Load(), Accepted: self.native.bridgeAccepted.Load(), Refused: self.native.bridgeRefused.Load(), Maximum: self.native.bridgeMaximum.Load(), Invalid: self.native.bridgeInvalid.Load(), Dials: [6]uint64{self.native.originDials[0].Load(), self.native.originDials[1].Load(), self.native.originDials[2].Load(), self.native.originDials[3].Load(), self.native.originDials[4].Load(), self.native.originDials[5].Load()}, Closes: [6]uint64{self.native.originCloses[0].Load(), self.native.originCloses[1].Load(), self.native.originCloses[2].Load(), self.native.originCloses[3].Load(), self.native.originCloses[4].Load(), self.native.originCloses[5].Load()}})
	if err != nil || len(encoded) > 2048 {
		t.Errorf("bounded SDK ABI custody: bytes=%d err=%v", len(encoded), err)
	} else {
		t.Logf("[perfvar] sdk-browsing-native-custody %s", encoded)
	}
	encoded, err = json.Marshal(observation)
	if err != nil || len(encoded) > nativeBrowsingPressureReportByteLimit {
		t.Errorf("bounded public browsing report: bytes=%d err=%v", len(encoded), err)
	} else {
		t.Logf("[perfvar] sdk-browsing-workload %s", encoded)
	}
}

// Manual provider4 research is separate from automatic qualification. Opting
// in retains the original workload, strict verdict and every timing/custody gate.
func TestFullTunH1Sdk32LateQualityPublicFullWindow100Pages(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_LATE_QUALITY_RESEARCH") != "1" {
		t.Skip("manual provider4 research; not qualification; set CONNECT_PERFVAR_LATE_QUALITY_RESEARCH=1")
	}
	testSdkBrowsingPublicServiceRecovery(t, false)
}

// Successful existing speed5 recovery satisfies this separate realistic case.
// The strict late4 case above remains an unchanged mechanism diagnostic.
func TestFullTunH1Sdk32ServiceRefusalRecovery100Pages(t *testing.T) {
	testSdkBrowsingPublicServiceRecovery(t, true)
}

// Both roots share the exact input and driver. This flag changes only the
// post-workload mechanism predicate and its report, never scheduling or gates.
func testSdkBrowsingPublicServiceRecovery(t *testing.T, acceptOwnedSpeed bool) {
	if testing.Short() {
		t.Skip("actual SDK/H1/provider topology needs shared PERFVAR services")
	}
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		plan := newNativeBrowsingPressurePlan()
		scenario := newSdkBrowsingScenario(plan)
		scenario.acceptOwnedSpeedRecovery = acceptOwnedSpeed
		fixture, err := newSdkBrowsingSetupFixture(t, plan, scenario)
		if err != nil {
			t.Fatal("public full-window constructor: ", err)
		}
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		initial, err := fixture.waitInitial()
		if err != nil {
			t.Fatal(err)
		}
		scenario.initial = initial
		serverTls, clientTls, err := nativeBrowsingTlsConfigs()
		if err != nil {
			t.Fatal(err)
		}
		pressure := &nativeBrowsingPressureObservation{Profile: plan.profile, ProviderProfile: plan.provider.Name, MaximumLivePages: plan.maxLivePages, HttpConnectionsPerOrigin: nativeBrowsingConnectionsPerOrigin, slowNavigation: sdk.DefaultDeviceLocalSettings().SendTimeout, PhaseNames: [8]string{"get_conn", "got_conn", "connect_start", "connect_done", "tls_start", "tls_done", "wrote_request", "first_byte"}, Scope: "actual public SDK32; four origins/four H1 connections each; four live pages; TLS1.3; 100HTML400assets99overlaps; canonical multi-client aggregate boundaries unavailable"}
		pressure.captureBoundaryForTest = func(*fullTunPath, int, *nativeBrowsingPressureObservation) *nativeBrowsingPressureBoundary {
			return nil
		}
		for page := range pressure.Navigations {
			pressure.Navigations[page].ConnectionAcquireNs, pressure.Navigations[page].WrittenToFirstByteNs = -1, -1
			for phase := range pressure.Navigations[page].Phases {
				pressure.Navigations[page].Phases[phase] = -1
			}
		}
		observation := &nativeBrowsingObservation{NativeCapability: true, Baseline: nativeBrowsingMemoryNow(), Pressure: pressure}
		observation.Peak = observation.Baseline
		settings := scenario.runSettings(pressure, serverTls, clientTls)
		start := time.Now()
		workloadErr := measureNativeBrowsing100WithSettings(fixture.ctx, scenario.native.path, observation, settings)
		pressure.finish(start)
		verdictErr := error(nil)
		if workloadErr == nil {
			verdictErr = scenario.verdict(observation)
		}
		closeErr := fixture.close()
		observation.Drain = nativeBrowsingMemoryNow()
		scenario.log(t, observation)
		if err := errors.Join(workloadErr, verdictErr, closeErr); err != nil {
			t.Fatal("public full-window browsing failure: ", err)
		}
	})
}
