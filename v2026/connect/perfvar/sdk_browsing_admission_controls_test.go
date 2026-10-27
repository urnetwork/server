// Fixture controls pin socket custody, cancellation and separate mechanism
// prerequisites without changing the public100 workload or SDK scheduling.
package perfvar

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// Refusal is destination- and provider-specific. Existing speed recovery stays
// eligible, as in the original stimulus; provider4 remains a separate premise.
func TestSdkBrowsingAdmissionRefusesOnlyOldQualityNavigation(t *testing.T) {
	self := newSdkBrowsingScenario(newNativeBrowsingPressurePlan())
	self.tapStarted = time.Now()
	self.rejectNew.Store(true)
	for ordinal := range 6 {
		if got := self.rejectNavigationDial(ordinal, "203.0.113.11:443"); got != (ordinal < 4) {
			t.Fatalf("provider=%d refused=%t", ordinal, got)
		}
		if self.rejectNavigationDial(ordinal, "203.0.113.10:443") {
			t.Fatal("original CSS destination was refused")
		}
	}
	select {
	case <-self.firstRefusal:
	default:
		t.Fatal("actual refusal did not publish its edge")
	}
	for ordinal, count := range self.refusalCounts {
		want := uint64(0)
		if ordinal < 4 {
			want = 1
		}
		if count != want {
			t.Fatalf("provider=%d refusals=%d want=%d", ordinal, count, want)
		}
	}
	self.rejectNew.Store(false)
	if self.rejectNavigationDial(0, "203.0.113.11:443") {
		t.Fatal("released fixture kept refusing new sockets")
	}
}

// A positive Read on an unrelated socket stays live. The exact CSS socket
// blocks until release and then returns the same bytes, count and error.
func TestSdkBrowsingAdmissionHoldsOnlyOriginalPositiveSocketRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	self := newSdkBrowsingScenario(newNativeBrowsingPressurePlan())
	self.workCtx, self.readArmed = ctx, true
	native := &sdkBrowsingNative{scenario: self, upstreamConns: map[sdkBrowsingOriginTuple]*sdkBrowsingOriginConn{}}
	reader, writer := net.Pipe()
	defer writer.Close()
	connection := &sdkBrowsingOriginConn{Conn: reader, native: native, ordinal: 0, tuple: sdkBrowsingOriginTuple{sourceAddr: "synthetic-original", destinationAddr: "synthetic-origin"}}
	defer connection.Close()
	self.cssSocket = connection
	native.upstreamConns[connection.tuple] = connection
	self.holdOriginRead(&sdkBrowsingOriginConn{}, 3)
	self.holdOriginRead(connection, 0)
	if self.readClaimed {
		t.Fatal("unrelated or empty read consumed CSS ownership")
	}
	written := make(chan error, 1)
	go func() { _, err := writer.Write([]byte("body")); written <- err }()
	type result struct {
		count int
		err   error
		body  string
	}
	returned := make(chan result, 1)
	go func() {
		buffer := make([]byte, 4)
		count, err := connection.Read(buffer)
		returned <- result{count, err, string(buffer[:count])}
	}()
	select {
	case <-self.readHeld:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
		t.Fatal("positive original read returned before its release")
	default:
	}
	self.releaseOnce.Do(func() { close(self.release) })
	got := <-returned
	if got.count != 4 || got.err != nil || got.body != "body" || self.positiveHeldBytes != 4 {
		t.Fatalf("original read changed: %+v held=%d", got, self.positiveHeldBytes)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if native.originCloses[0].Load() != 1 || len(native.upstreamConns) != 0 {
		t.Fatal("origin socket retired more than once or retained attribution")
	}
}

// The response recorder exposes the selected real writer prefix, including a
// short nil-error write. The wrapper cannot count unwritten CSS as delivered.
type sdkBrowsingShortCssWriter struct {
	*httptest.ResponseRecorder
	shortAt, calls int
	err            error
}

// Every unselected write delegates its original body unchanged.
func (self *sdkBrowsingShortCssWriter) Write(body []byte) (int, error) {
	self.calls++
	if self.calls == self.shortAt {
		return 3, self.err
	}
	return self.ResponseRecorder.Write(body)
}

// Prefix and suffix failures preserve exact underlying counts and errors.
func TestSdkBrowsingAdmissionCssWriterPreservesPartialResults(t *testing.T) {
	for _, shortAt := range []int{1, 2} {
		for _, writeErr := range []error{nil, io.ErrClosedPipe} {
			self := newSdkBrowsingScenario(newNativeBrowsingPressurePlan())
			close(self.prefixRead)
			self.releaseOnce.Do(func() { close(self.release) })
			base := &sdkBrowsingShortCssWriter{ResponseRecorder: httptest.NewRecorder(), shortAt: shortAt, err: writeErr}
			writer := &sdkBrowsingCssWriter{ResponseWriter: base, ctx: t.Context(), scenario: self}
			count, err := writer.Write(nativeBrowsingAsset(0, 0))
			want := 3
			if shortAt == 2 {
				want += nativeBrowsingHeldPrefixBytes
			}
			wantErr := writeErr
			if wantErr == nil {
				wantErr = io.ErrShortWrite
			}
			if count != want || !errors.Is(err, wantErr) {
				t.Fatalf("write=%d count=%d/%d error=%v/%v", shortAt, count, want, err, wantErr)
			}
			select {
			case <-self.suffixWritten:
				t.Fatal("short CSS write fabricated a completed suffix")
			default:
			}
		}
	}
}

// Completed constructor output keeps authority after the driver cancels its
// finished work context; this does not excuse an unfinished constructor.
func TestSdkBrowsingAdmissionCompletedConstructorSurvivesWorkCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	close(done)
	if err := sdkBrowsingAwaitConstructor(ctx, done); err != nil {
		t.Fatal("completed constructor lost to finished-work cancellation: ", err)
	}
}

// Pending work keeps the original cancellation error and never fabricates done.
func TestSdkBrowsingAdmissionPendingConstructorKeepsWorkCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sdkBrowsingAwaitConstructor(ctx, make(chan struct{})); !errors.Is(err, context.Canceled) {
		t.Fatal("pending constructor cancellation lost: ", err)
	}
}

// All400 logical requests finished; some canceled before response creation.
// The joined server receipt is independent from local body ownership.
func sdkBrowsingBodyOwnershipControl() *nativeBrowsingPressureObservation {
	pressure := &nativeBrowsingPressureObservation{}
	for page := range pressure.assets {
		for asset := range pressure.assets[page] {
			progress := &pressure.assets[page][asset]
			progress.started.Store(true)
			progress.finished.Store(true)
			progress.connectionAcquisitions.Store(1)
			progress.requestWrites.Store(1)
			progress.serverReceipt = clientconnect.NewMonitorValue(nativeBrowsingServerReceipt{started: 1, finished: 1})
			if page*nativeBrowsingAssetCount+asset < 78 {
				progress.cancelRequested.Store(true)
			} else {
				progress.bodyOpened.Store(true)
				progress.bodyClosed.Store(true)
				progress.responseStatus.Store(http.StatusOK)
			}
		}
	}
	return pressure
}

// Pre-response cancellation creates no body to close and cannot hide any
// response body that actually opened but missed its own Close.
func TestNativeBrowsingBodyOwnershipAcceptsPreResponseCancellation(t *testing.T) {
	pressure := sdkBrowsingBodyOwnershipControl()
	if err := nativeBrowsingAssetBodyOwnershipError(pressure); err != nil {
		t.Fatal(err)
	}
	drain := pressure.assetOwnershipSnapshot()
	if drain.BodiesOpened != 322 || drain.BodiesClosed != 322 || drain.Canceled != 78 {
		t.Fatalf("body creation/cancel disposition changed: %+v", drain)
	}
}

// Both per-body mismatches remain failures even when aggregate totals balance.
func TestNativeBrowsingBodyOwnershipRejectsUnclosedOrInventedBodies(t *testing.T) {
	pressure := sdkBrowsingBodyOwnershipControl()
	pressure.assets[20][0].bodyClosed.Store(false)
	pressure.assets[0][0].bodyClosed.Store(true)
	drain := pressure.assetOwnershipSnapshot()
	if drain.BodiesOpened != drain.BodiesClosed {
		t.Fatal("same-count counterexample premises changed")
	}
	if err := nativeBrowsingAssetBodyOwnershipError(pressure); err == nil {
		t.Fatal("unclosed opened body was hidden by an invented closed body")
	}
}

// A fast existing speed route satisfies the timing gate while failing this
// fixture's separate provider4 mechanism prerequisite.
func TestSdkBrowsingAdmissionFastExistingRouteIsMechanismPrerequisite(t *testing.T) {
	scenario := newSdkBrowsingScenario(newNativeBrowsingPressurePlan())
	scenario.cssComplete, scenario.heldThroughHtml, scenario.lateReady, scenario.cssOwnerRetained = true, true, true, true
	scenario.cssFlowsBefore, scenario.cssFlowsAfter, scenario.page1Provider = 1, 1, 5
	scenario.combinedFirstByteNs = int64(time.Millisecond)
	scenario.htmlFetches.Store(100)
	scenario.assetFetches.Store(400)
	observation := &nativeBrowsingObservation{CompletedNavigations: 100, OverlapProofs: 99, Pressure: sdkBrowsingBodyOwnershipControl()}
	for page := range observation.Pages {
		observation.Pages[page].FirstByteNanoseconds = int64(time.Millisecond)
	}
	err := scenario.verdict(observation)
	if err == nil || !strings.Contains(err.Error(), "SDK_BROWSING_LATE_PROVIDER_NOT_EXERCISED") || strings.Contains(err.Error(), "NATIVE_BROWSING_NAVIGATION_STALL") {
		t.Fatal("fast fallback lost its separate prerequisite disposition: ", err)
	}
}

// Missing provider4 does not waive an independently observed five-second stall.
func TestSdkBrowsingAdmissionMechanismMissKeepsLatencyFailure(t *testing.T) {
	scenario := newSdkBrowsingScenario(newNativeBrowsingPressurePlan())
	scenario.cssComplete, scenario.heldThroughHtml, scenario.lateReady, scenario.cssOwnerRetained = true, true, true, true
	scenario.cssFlowsBefore, scenario.cssFlowsAfter, scenario.page1Provider = 1, 1, 5
	scenario.combinedFirstByteNs = int64(sdk.DefaultDeviceLocalSettings().SendTimeout)
	scenario.htmlFetches.Store(100)
	scenario.assetFetches.Store(400)
	observation := &nativeBrowsingObservation{CompletedNavigations: 100, OverlapProofs: 99, Pressure: sdkBrowsingBodyOwnershipControl()}
	for page := range observation.Pages {
		observation.Pages[page].FirstByteNanoseconds = int64(time.Millisecond)
	}
	err := scenario.verdict(observation)
	if err == nil || !strings.Contains(err.Error(), "SDK_BROWSING_LATE_PROVIDER_NOT_EXERCISED") || !strings.Contains(err.Error(), "NATIVE_BROWSING_NAVIGATION_STALL") {
		t.Fatal("independent latency failure was lost: ", err)
	}
}

// Fixed endpoint wrappers let controls select legal TCP four-tuples without
// depending on the host's ephemeral-port allocator. I/O and Close stay real.
type sdkBrowsingTupleControlConn struct {
	net.Conn
	localAddr, remoteAddr net.Addr
}

// The dialer's local endpoint is the server request's remote endpoint.
func (self *sdkBrowsingTupleControlConn) LocalAddr() net.Addr { return self.localAddr }

// The dialer's remote endpoint is the accepted server's local endpoint.
func (self *sdkBrowsingTupleControlConn) RemoteAddr() net.Addr { return self.remoteAddr }

// Both ends are joined by cleanup; no operating-system bind or route is used.
func sdkBrowsingTupleControlConnection(t *testing.T, destinationPort int) net.Conn {
	t.Helper()
	connection, peer := net.Pipe()
	t.Cleanup(func() { connection.Close(); peer.Close() })
	return &sdkBrowsingTupleControlConn{Conn: connection, localAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49000}, remoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: destinationPort}}
}

// Standard net/http supplies the actual accepted local address in this key.
func sdkBrowsingTupleControlRequest(connection net.Conn) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "https://origin1.test/page/1", nil)
	request.RemoteAddr = connection.LocalAddr().String()
	return request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, connection.RemoteAddr()))
}

// Equal local ports at different destinations are two live sockets, not a
// collision. Registration and independent removal use the real ownership helper.
func TestSdkBrowsingOriginTupleKeepsSameSourceDifferentDestinations(t *testing.T) {
	native := &sdkBrowsingNative{upstreamConns: map[sdkBrowsingOriginTuple]*sdkBrowsingOriginConn{}}
	first, err := native.ownOriginConnection(sdkBrowsingTupleControlConnection(t, 51001), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := native.ownOriginConnection(sdkBrowsingTupleControlConnection(t, 51002), 1)
	if err != nil {
		t.Fatal("SDK_BROWSING_ORIGIN_TUPLE_ALIAS: ", err)
	}
	defer second.Close()
	if first.tuple.sourceAddr != second.tuple.sourceAddr || first.tuple.destinationAddr == second.tuple.destinationAddr || len(native.upstreamConns) != 2 {
		t.Fatal("forced same-source/different-destination premises or ownership changed")
	}
	if native.originConnection(sdkBrowsingTupleControlRequest(first)) != first || native.originConnection(sdkBrowsingTupleControlRequest(second)) != second {
		t.Fatal("full-tuple ownership did not distinguish both sockets")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if len(native.upstreamConns) != 1 || native.originConnection(sdkBrowsingTupleControlRequest(second)) != second {
		t.Fatal("closing one destination removed its same-source sibling")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if len(native.upstreamConns) != 0 || native.originDials[0].Load() != 1 || native.originDials[1].Load() != 1 || native.originCloses[0].Load() != 1 || native.originCloses[1].Load() != 1 {
		t.Fatal("full-tuple socket custody did not balance")
	}
}

// The actual page1 wrapper joins RemoteAddr and the accepted local endpoint.
// Missing server context stays unknown instead of guessing from the source.
func TestSdkBrowsingOriginTupleHandlerUsesAcceptedDestination(t *testing.T) {
	scenario := newSdkBrowsingScenario(newNativeBrowsingPressurePlan())
	native := &sdkBrowsingNative{scenario: scenario, upstreamConns: map[sdkBrowsingOriginTuple]*sdkBrowsingOriginConn{}}
	scenario.native = native
	first, err := native.ownOriginConnection(sdkBrowsingTupleControlConnection(t, 51001), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := native.ownOriginConnection(sdkBrowsingTupleControlConnection(t, 51002), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	handler := scenario.wrapHandler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("original handler")) }))
	for _, connection := range []*sdkBrowsingOriginConn{first, second} {
		writer := httptest.NewRecorder()
		handler.ServeHTTP(writer, sdkBrowsingTupleControlRequest(connection))
		if writer.Body.String() != "original handler" || scenario.page1Provider != connection.ordinal || scenario.firstErr != nil {
			t.Fatalf("accepted destination lost provider attribution: got=%d want=%d err=%v", scenario.page1Provider, connection.ordinal, scenario.firstErr)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://origin1.test/page/1", nil)
	request.RemoteAddr = first.LocalAddr().String()
	if ordinal, known := native.provider(request); known || ordinal != -1 || native.originConnection(request) != nil {
		t.Fatal("missing accepted endpoint fabricated source-only ownership")
	}
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if scenario.page1Provider != -1 || scenario.firstErr == nil {
		t.Fatal("unknown handler attribution did not remain an explicit failure")
	}
}

// A previously recorded socket cannot erase a different current pointer for
// its full tuple. This pins the existing ABA guard while changing the key.
func TestSdkBrowsingOriginTupleLateClosePreservesReplacement(t *testing.T) {
	native := &sdkBrowsingNative{upstreamConns: map[sdkBrowsingOriginTuple]*sdkBrowsingOriginConn{}}
	old, err := native.ownOriginConnection(sdkBrowsingTupleControlConnection(t, 51001), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	replacement := &sdkBrowsingOriginConn{Conn: sdkBrowsingTupleControlConnection(t, 51001), native: native, ordinal: 1, tuple: old.tuple}
	defer replacement.Close()
	native.stateLock.Lock()
	native.upstreamConns[old.tuple] = replacement
	native.stateLock.Unlock()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if native.originConnection(sdkBrowsingTupleControlRequest(replacement)) != replacement || native.originCloses[0].Load() != 1 || native.originCloses[1].Load() != 0 {
		t.Fatal("late old close removed or retired the replacement tuple owner")
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if len(native.upstreamConns) != 0 || native.originCloses[1].Load() != 1 {
		t.Fatal("replacement did not retain its own terminal cleanup")
	}
}

// This is a verdict input, not a simulated SDK or measured body transfer.
// Original socket/body/cancellation controls independently pin those receipts.
func sdkBrowsingServiceRecoveryControl() (*sdkBrowsingScenario, *nativeBrowsingObservation) {
	scenario := newSdkBrowsingScenario(newNativeBrowsingPressurePlan())
	scenario.acceptOwnedSpeedRecovery = true
	scenario.cssComplete, scenario.heldThroughHtml, scenario.lateReady, scenario.cssOwnerRetained = true, true, true, true
	scenario.cssFlowsBefore, scenario.cssFlowsAfter, scenario.page1Provider = 1, 1, 5
	scenario.combinedFirstByteNs = int64(time.Millisecond)
	scenario.htmlFetches.Store(100)
	scenario.assetFetches.Store(400)
	observation := &nativeBrowsingObservation{CompletedNavigations: 100, OverlapProofs: 99, Pressure: sdkBrowsingBodyOwnershipControl()}
	for page := range observation.Pages {
		observation.Pages[page].FirstByteNanoseconds = int64(time.Millisecond)
	}
	return scenario, observation
}

// Speed5 is already in the positive full-window pool and the original body,
// CSS, timing and request counts remain necessary in this separate case.
func TestSdkBrowsingAdmissionServiceRecoveryAcceptsOwnedSpeed(t *testing.T) {
	scenario, observation := sdkBrowsingServiceRecoveryControl()
	if err := scenario.verdict(observation); err != nil {
		t.Fatal("permitted owned-speed recovery was rejected: ", err)
	}
	scenario.acceptOwnedSpeedRecovery = false
	if err := scenario.verdict(observation); err == nil || !strings.Contains(err.Error(), "SDK_BROWSING_LATE_PROVIDER_NOT_EXERCISED") {
		t.Fatal("strict late4 diagnostic was silently relaxed: ", err)
	}
}

// Permitting speed changes no page0, combined timing, CSS or provider boundary.
func TestSdkBrowsingAdmissionServiceRecoveryPreservesAllOtherGates(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*sdkBrowsingScenario, *nativeBrowsingObservation)
		marker string
	}{
		{name: "page0-threshold", change: func(_ *sdkBrowsingScenario, observation *nativeBrowsingObservation) {
			observation.Pages[0].FirstByteNanoseconds = int64(sdk.DefaultDeviceLocalSettings().SendTimeout)
		}, marker: "NATIVE_BROWSING_NAVIGATION_STALL"},
		{name: "combined-threshold", change: func(scenario *sdkBrowsingScenario, _ *nativeBrowsingObservation) {
			scenario.combinedFirstByteNs = int64(sdk.DefaultDeviceLocalSettings().SendTimeout)
		}, marker: "NATIVE_BROWSING_NAVIGATION_STALL"},
		{name: "original-css", change: func(scenario *sdkBrowsingScenario, _ *nativeBrowsingObservation) { scenario.cssComplete = false }, marker: "SDK_BROWSING_PUBLIC_FULL_WINDOW_CUSTODY_OR_WORKLOAD"},
		{name: "held-through-html", change: func(scenario *sdkBrowsingScenario, _ *nativeBrowsingObservation) { scenario.heldThroughHtml = false }, marker: "SDK_BROWSING_PUBLIC_FULL_WINDOW_CUSTODY_OR_WORKLOAD"},
		{name: "refused-old-provider", change: func(scenario *sdkBrowsingScenario, _ *nativeBrowsingObservation) { scenario.page1Provider = 0 }, marker: "SDK_BROWSING_LATE_PROVIDER_NOT_EXERCISED"},
	} {
		scenario, observation := sdkBrowsingServiceRecoveryControl()
		test.change(scenario, observation)
		if err := scenario.verdict(observation); err == nil || !strings.Contains(err.Error(), test.marker) {
			t.Fatalf("%s lost its unchanged gate: %v", test.name, err)
		}
	}
}
