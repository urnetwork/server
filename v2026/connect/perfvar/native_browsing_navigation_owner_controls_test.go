// Deterministic controller controls use actual held body reads and HTTP client
// dispatch. Request/session receipts are a model; real TLS/h2 controls remain
// separate selected integration coverage, not a claim made by this model.
package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/sdk/v2026"
)

// Preserve constructor-owned fields, including optional exact gate identity.
// These scalar model receipts do not stand in for network provenance tests.
func nativeBrowsingOwnedColdRequestForTest(request nativeBrowsingColdRequest) nativeBrowsingColdRequest {
	request.clientSession = &tls.Conn{}
	request.clientAcquisitions = 1
	request.verifiedClientTls = true
	request.serverTuple = "192.0.2.1:40001|192.0.2.2:443"
	request.serverAttempts, request.serverProvider, request.serverOwned = 1, 0, true
	return request
}

// OLD-v2 retains the original trace API; the same test bytes call the new
// context-aware path when available. GetConn is invoked at actual Transport
// entry by pending-request controls, and explicitly by scalar model fixtures.
func nativeBrowsingContextTraceForTest(witness any, ctx context.Context, target string) *httptrace.ClientTrace {
	if contextual, ok := witness.(interface {
		requestTraceWithContext(context.Context, string) *httptrace.ClientTrace
	}); ok {
		return contextual.requestTraceWithContext(ctx, target)
	}
	return witness.(interface {
		requestTrace(string) *httptrace.ClientTrace
	}).requestTrace(target)
}

func nativeBrowsingModelDispatchForTest(witness any, ctx context.Context, target string) {
	trace := nativeBrowsingContextTraceForTest(witness, ctx, target)
	parsed, err := url.Parse(target)
	if err == nil && trace != nil && trace.GetConn != nil {
		trace.GetConn(parsed.Hostname() + ":443")
	}
}

// One original read returns137 real bytes; its next underlying read waits on
// an explicit owner gate before the body reader can reach its1KiB pause.
type nativeBrowsingPrefixBarrierReader struct {
	ctx         context.Context
	reader      *bytes.Reader
	blocked     chan struct{}
	release     <-chan struct{}
	first       bool
	blockedOnce sync.Once
}

func (self *nativeBrowsingPrefixBarrierReader) Read(buffer []byte) (int, error) {
	if !self.first {
		self.first = true
		return self.reader.Read(buffer[:min(137, len(buffer))])
	}
	self.blockedOnce.Do(func() { close(self.blocked) })
	select {
	case <-self.ctx.Done():
		return 0, self.ctx.Err()
	case <-self.release:
		return self.reader.Read(buffer)
	}
}

// Both paths call the actual witness callback. The source body owner is joined
// before the synctest bubble exits; no timeout is used as negative evidence.
func nativeBrowsingBlockedOwnerForTest(t *testing.T, warm bool) (context.Context, func(context.Context, int, func()) error, func(), *nativeBrowsingPressureObservation) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pressure := &nativeBrowsingPressureObservation{}
	progress := &pressure.assets[1][1]
	body := nativeBrowsingAsset(1, 1)
	progress.expectedBytes = int64(len(body))
	progress.started.Store(true)
	progress.bodyOpened.Store(true)
	release := make(chan struct{})
	underlying := &nativeBrowsingPrefixBarrierReader{ctx: ctx, reader: bytes.NewReader(body), blocked: make(chan struct{}), release: release}
	target := strings.Replace(nativeBrowsingURL(1, 1), "http://", "https://", 1)
	var reader io.Reader
	var before func(context.Context, int, func()) error
	var closeWitness func()
	if warm {
		witness := newNativeBrowsingHttp2Witness(pressure)
		witness.requests[1][1].clientAcquisitions, witness.requests[1][1].serverAttempts = 1, 1
		witness.clientSessions[2], witness.serverSessions[2] = &tls.Conn{}, "192.0.2.1:40001|192.0.2.2:443"
		nativeBrowsingModelDispatchForTest(witness, ctx, target)
		reader = witness.wrapReader(ctx, target, underlying)
		before, closeWitness = witness.beforeNavigation, witness.close
	} else {
		witness := newNativeBrowsingColdLinkWitness(nil, pressure, false)
		witness.requests[1][1] = nativeBrowsingOwnedColdRequestForTest(witness.requests[1][1])
		nativeBrowsingModelDispatchForTest(witness, ctx, target)
		reader = witness.wrapReader(ctx, target, underlying)
		before, closeWitness = witness.beforeNavigation, witness.close
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer progress.finished.Store(true)
		defer progress.bodyClosed.Store(true)
		_, _ = io.ReadAll(&nativeBrowsingProgressReader{reader: reader, pressure: pressure, progress: progress})
	}()
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			close(release)
			closeWitness()
			<-done
		})
	}
	handedOff := false
	defer func() {
		if !handedOff {
			cleanup()
		}
	}()
	<-underlying.blocked
	synctest.Wait()
	if progress.receivedBytes.Load() != 137 || progress.finished.Load() || progress.bodyClosed.Load() {
		t.Fatal("fixture lacks its actual unfinished137-byte original body read")
	}
	handedOff = true
	return ctx, before, cleanup, pressure
}

// A real http.Client dispatch marks the boundary downstream of the controller.
type nativeBrowsingControllerRoundTrip func(*http.Request) (*http.Response, error)

func (self nativeBrowsingControllerRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

func nativeBrowsingControllerDispatchForTest(ctx context.Context, trace *httptrace.ClientTrace, dispatch func(*http.Request)) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://origin2.test/page/2", nil)
	if err != nil {
		return err
	}
	if trace != nil {
		request = request.WithContext(httptrace.WithClientTrace(ctx, trace))
	}
	client := &http.Client{Transport: nativeBrowsingControllerRoundTrip(func(request *http.Request) (*http.Response, error) {
		dispatch(request)
		body := nativeBrowsingHTML(2)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
	})}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return nativeBrowsingResponseError(response.StatusCode, body, nativeBrowsingHTML(2), err)
}

func nativeBrowsingControllerCausalForTest(t *testing.T, warm bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, before, cleanup, pressure := nativeBrowsingBlockedOwnerForTest(t, warm)
		entered, dispatched := make(chan struct{}), make(chan struct{})
		result := make(chan error, 1)
		producerDone := make(chan struct{})
		defer func() { cleanup(); <-producerDone }()
		go func() {
			defer close(producerDone)
			close(entered)
			if err := before(ctx, 2, nil); err != nil {
				result <- err
				return
			}
			result <- nativeBrowsingControllerDispatchForTest(ctx, nil, func(*http.Request) { close(dispatched) })
		}()
		<-entered
		synctest.Wait()
		select {
		case <-dispatched:
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		default:
			select {
			case err := <-result:
				t.Fatalf("controller returned before HTTP dispatch: %v", err)
			default:
			}
			marker := "COLD_LINK_HTML_DISPATCH_WAITED_FOR_PRIOR_PREFIX"
			if warm {
				marker = "HTTP2_HTML_DISPATCH_WAITED_FOR_PRIOR_PREFIX"
			}
			t.Errorf("%s: bytes=%d body_closed=%t; HTML never reached http.Client dispatch while only the prior next-read gate remained blocked", marker, pressure.assets[1][1].receivedBytes.Load(), pressure.assets[1][1].bodyClosed.Load())
		}
	})
}

func TestNativeBrowsingColdLinkDispatchBeforePriorPrefix(t *testing.T) {
	nativeBrowsingControllerCausalForTest(t, false)
}

func TestNativeBrowsingHttp2DispatchBeforePriorPrefix(t *testing.T) {
	nativeBrowsingControllerCausalForTest(t, true)
}

// Zero bytes can describe a real owned request whose response body has not
// arrived. Existing TLS/server receipts, not arbitrary read progress, qualify it.
func TestNativeBrowsingColdLinkPendingBodyOwnership(t *testing.T) {
	for _, received := range []int64{0, 137, nativeBrowsingHeldPrefixBytes} {
		pressure := &nativeBrowsingPressureObservation{}
		witness := newNativeBrowsingColdLinkWitness(nil, pressure, false)
		progress := &pressure.assets[0][0]
		progress.expectedBytes = int64(nativeBrowsingAssetBytes(0, 0))
		progress.started.Store(true)
		progress.receivedBytes.Store(received)
		progress.bodyOpened.Store(received != 0)
		witness.requests[0][0] = nativeBrowsingOwnedColdRequestForTest(witness.requests[0][0])
		nativeBrowsingModelDispatchForTest(witness, context.Background(), "https://origin0.test/asset/0/0")
		if err := witness.checkHold(&witness.css, nativeBrowsingColdCss); err != nil {
			t.Fatalf("received=%d: %v", received, err)
		}
		witness.close()
	}
}

func TestNativeBrowsingHttp2PendingBodyOwnership(t *testing.T) {
	for _, received := range []int64{0, 137, nativeBrowsingHeldPrefixBytes} {
		pressure := &nativeBrowsingPressureObservation{}
		witness := newNativeBrowsingHttp2Witness(pressure)
		progress := &pressure.assets[0][1]
		progress.expectedBytes = int64(nativeBrowsingAssetBytes(0, 1))
		progress.started.Store(true)
		progress.receivedBytes.Store(received)
		progress.bodyOpened.Store(received != 0)
		witness.requests[0][1].clientAcquisitions, witness.requests[0][1].serverAttempts = 1, 1
		witness.clientSessions[1], witness.serverSessions[1] = &tls.Conn{}, "192.0.2.1:40001|192.0.2.2:443"
		nativeBrowsingModelDispatchForTest(witness, context.Background(), "https://origin1.test/asset/0/1")
		if err := witness.checkUnreadBody(0); err != nil {
			t.Fatalf("received=%d: %v", received, err)
		}
		witness.close()
	}
}

func TestNativeBrowsingColdLinkRejectsInvalidBodyOwnership(t *testing.T) {
	for _, kind := range []string{"missing-request", "missing-server", "finished", "closed", "canceled", "expected", "excess-read", "released", "reacquired", "replayed", "copied", "slot", "gate", "stale-error"} {
		pressure := &nativeBrowsingPressureObservation{}
		witness := newNativeBrowsingColdLinkWitness(nil, pressure, false)
		progress := &pressure.assets[0][0]
		progress.expectedBytes = int64(nativeBrowsingAssetBytes(0, 0))
		progress.started.Store(true)
		witness.requests[0][0] = nativeBrowsingOwnedColdRequestForTest(witness.requests[0][0])
		nativeBrowsingModelDispatchForTest(witness, context.Background(), "https://origin0.test/asset/0/0")
		hold := &witness.css
		switch kind {
		case "missing-request":
			witness.requests[0][0].clientSession = nil
		case "missing-server":
			witness.requests[0][0].serverOwned = false
		case "finished":
			progress.finished.Store(true)
		case "closed":
			progress.bodyClosed.Store(true)
		case "canceled":
			progress.cancelRequested.Store(true)
		case "expected":
			progress.expectedBytes--
		case "excess-read":
			progress.receivedBytes.Store(nativeBrowsingHeldPrefixBytes + 1)
		case "released":
			witness.css.gate.release()
		case "reacquired":
			witness.requests[0][0].clientAcquisitions++
		case "replayed":
			witness.requests[0][0].serverAttempts++
		case "copied":
			copy := witness.css
			hold = &copy
		case "slot":
			witness.css.slot = 1
		case "gate":
			witness.css.gate = newNativeBrowsingHttp2BodyGate()
		case "stale-error":
			witness.firstErr = errors.New("prior TLS/session mismatch")
		}
		if err := witness.checkHold(hold, nativeBrowsingColdCss); err == nil {
			t.Errorf("accepted invalid %s original owner", kind)
		}
		witness.close()
	}
}

func TestNativeBrowsingHttp2RejectsInvalidBodyOwnership(t *testing.T) {
	for _, kind := range []string{"missing-request", "missing-server", "finished", "closed", "canceled", "expected", "excess-read", "released", "reacquired", "replayed", "gate", "stale-error"} {
		pressure := &nativeBrowsingPressureObservation{}
		witness := newNativeBrowsingHttp2Witness(pressure)
		progress := &pressure.assets[0][1]
		progress.expectedBytes = int64(nativeBrowsingAssetBytes(0, 1))
		progress.started.Store(true)
		witness.requests[0][1].clientAcquisitions, witness.requests[0][1].serverAttempts = 1, 1
		witness.clientSessions[1], witness.serverSessions[1] = &tls.Conn{}, "192.0.2.1:40001|192.0.2.2:443"
		nativeBrowsingModelDispatchForTest(witness, context.Background(), "https://origin1.test/asset/0/1")
		switch kind {
		case "missing-request":
			witness.clientSessions[1] = nil
		case "missing-server":
			witness.serverSessions[1] = ""
		case "finished":
			progress.finished.Store(true)
		case "closed":
			progress.bodyClosed.Store(true)
		case "canceled":
			progress.cancelRequested.Store(true)
		case "expected":
			progress.expectedBytes--
		case "excess-read":
			progress.receivedBytes.Store(nativeBrowsingHeldPrefixBytes + 1)
		case "released":
			witness.gates[0].release()
		case "reacquired":
			witness.requests[0][1].clientAcquisitions++
		case "replayed":
			witness.requests[0][1].serverAttempts++
		case "gate":
			witness.gates[0] = newNativeBrowsingHttp2BodyGate()
		case "stale-error":
			witness.firstErr = errors.New("prior origin session mismatch")
		}
		if err := witness.checkUnreadBody(0); err == nil {
			t.Errorf("accepted invalid %s original owner", kind)
		}
		witness.close()
	}
}

func nativeBrowsingControllerDelayForTest(t *testing.T, warm bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pressure := &nativeBrowsingPressureObservation{}
		progress := &pressure.assets[1][1]
		progress.expectedBytes = int64(nativeBrowsingAssetBytes(1, 1))
		progress.started.Store(true)
		var before func(context.Context, int, func()) error
		var requestTrace func(string) *httptrace.ClientTrace
		var verdict func() error
		var closeWitness func()
		if warm {
			witness := newNativeBrowsingHttp2Witness(pressure)
			witness.validatedHtml, witness.unreadBodyOverlaps, witness.sameSessionHtml = 100, 98, 99
			for page := range witness.requests {
				witness.combinedFirstByteNs[page] = 1
				for slot := range witness.requests[page] {
					witness.requests[page][slot].clientAcquisitions, witness.requests[page][slot].serverAttempts = 1, 1
				}
			}
			witness.combinedFirstByteNs[2] = 0
			for origin := range witness.clientSessions {
				witness.clientSessions[origin], witness.serverSessions[origin] = &tls.Conn{}, fmt.Sprint("192.0.2.1:", 40000+origin, "|192.0.2.2:443")
			}
			nativeBrowsingModelDispatchForTest(witness, ctx, "https://origin2.test/asset/1/1")
			before, requestTrace, closeWitness = witness.beforeNavigation, witness.requestTrace, witness.close
			verdict = func() error { return witness.verdict(sdk.DefaultDeviceLocalSettings().SendTimeout) }
		} else {
			witness := newNativeBrowsingColdLinkWitness(&sdkBrowsingScenario{}, pressure, false)
			witness.row.Html, witness.row.Assets, witness.row.ColdEdges, witness.row.VisitedDestinationEdges = 100, 400, 1, 98
			witness.row.CssOwnerRetained, witness.row.ColdTupleOwned = true, true
			for page := range witness.requests {
				witness.row.CombinedFirstByteNs[page] = 1
				for slot := range witness.requests[page] {
					witness.requests[page][slot] = nativeBrowsingOwnedColdRequestForTest(witness.requests[page][slot])
				}
			}
			nativeBrowsingModelDispatchForTest(witness, ctx, "https://origin2.test/asset/1/1")
			before, requestTrace, closeWitness, verdict = witness.beforeNavigation, witness.requestTrace, witness.close, witness.verdict
		}
		defer closeWitness()
		if err := before(ctx, 2, nil); err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- nativeBrowsingControllerDispatchForTest(ctx, requestTrace("https://origin2.test/page/2"), func(request *http.Request) {
				close(entered)
				<-release
				trace := httptrace.ContextClientTrace(request.Context())
				trace.GotFirstResponseByte()
			})
		}()
		<-entered
		time.Sleep(sdk.DefaultDeviceLocalSettings().SendTimeout)
		close(release)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		err := verdict()
		if err == nil || !strings.Contains(err.Error(), "NATIVE_BROWSING_NAVIGATION_STALL") || !strings.Contains(err.Error(), "page=2") {
			t.Fatalf("actual posttap HTTP delay escaped unchanged5s gate: %v", err)
		}
	})
}

func TestNativeBrowsingColdLinkPostTapHttpDelayStillFails(t *testing.T) {
	nativeBrowsingControllerDelayForTest(t, false)
}
func TestNativeBrowsingHttp2PostTapHttpDelayStillFails(t *testing.T) {
	nativeBrowsingControllerDelayForTest(t, true)
}

func TestNativeBrowsingControllerCanceledOwnerCannotDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cold := newNativeBrowsingColdLinkWitness(nil, &nativeBrowsingPressureObservation{}, false)
	warm := newNativeBrowsingHttp2Witness(&nativeBrowsingPressureObservation{})
	defer cold.close()
	defer warm.close()
	for _, before := range []func(context.Context, int, func()) error{cold.beforeNavigation, warm.beforeNavigation} {
		if err := before(ctx, 2, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled controller owner was admitted: %v", err)
		}
	}
}
