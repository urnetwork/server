// Both SDK public-service roots consume the same revised pressure settings.
// Registration is exercised directly, including the original CSS/native hooks.
package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
)

// Reuse the explicit revised plan, retaining every original SDK/TLS hook and
// deadline. The shared service driver calls this before its unchanged timer.
func (self *sdkBrowsingScenario) runSettings(pressure *nativeBrowsingPressureObservation, serverTls, clientTls *tls.Config) nativeBrowsingRunSettings {
	settings := self.plan.runSettings(pressure)
	settings.originServerTlsForTest = serverTls
	settings.originClientTlsForTest = clientTls
	settings.originListenerForTest = self.native.addOrigin
	settings.originDestinationForTest = self.native.destination
	settings.afterOriginsClosedForTest = self.native.clearOrigins
	settings.wrapOriginHandlerForTest = self.wrapHandler
	settings.applicationDeadlineForTest = self.plan.deadline
	settings.beforePageNavigationForTest = self.beforePage
	settings.requestTraceForTest = self.requestTrace
	settings.afterFetchForTest = self.afterFetch
	settings.wrapResponseReaderForTest = func(target string, reader io.Reader) io.Reader {
		if sdkBrowsingRequestPath(target) == "/asset/0/0" {
			return &sdkBrowsingCssReader{reader: reader, scenario: self}
		}
		return reader
	}
	return settings
}

// The actual shared SDK settings path must select v2 without constructing a
// service-backed SDK fixture. Hooks retain their original receiver and inputs;
// no source-file reads or overlay-dependent source contract is involved.
func TestSdkBrowsingSettingsRetainRevisedImageOverlapAndOriginalHooks(t *testing.T) {
	plan := newNativeBrowsingPressurePlan()
	scenario := newSdkBrowsingScenario(plan)
	scenario.native = &sdkBrowsingNative{scenario: scenario}
	pressure := &nativeBrowsingPressureObservation{}
	serverTls := &tls.Config{MinVersion: tls.VersionTLS13}
	clientTls := &tls.Config{MinVersion: tls.VersionTLS13}
	settings := scenario.runSettings(pressure, serverTls, clientTls)
	if err := settings.validate(); err != nil || !settings.nextNavigationOverlap || settings.maxLivePages != 4 ||
		settings.pressure != pressure || settings.originServerTlsForTest != serverTls || settings.originClientTlsForTest != clientTls ||
		settings.applicationDeadlineForTest != plan.deadline {
		t.Fatalf("shared SDK settings lost revised pressure/TLS/owner registration: %v", err)
	}
	pages := settings.newPages()
	defer nativeBrowsingReleasePages(pages[:])
	for page, state := range pages {
		if (state.nextNavigation != nil) != (page < nativeBrowsingPageCount-1) ||
			pressure.assets[page][2].nextNavigation != state.nextNavigation {
			t.Fatalf("shared SDK page %d lost its original-image completion gate", page)
		}
		for asset := range nativeBrowsingAssetCount {
			if pressure.assets[page][asset].serverReceipt != state.serverReceipts[asset] ||
				asset != 2 && pressure.assets[page][asset].nextNavigation != nil {
				t.Fatalf("shared SDK page %d asset %d changed ownership or blanket-held assets", page, asset)
			}
		}
	}
	if pressure.WorkloadVersion != "next-navigation-image-v2" {
		t.Fatalf("shared SDK settings selected %q", pressure.WorkloadVersion)
	}
	settings.originListenerForTest(&nativeBrowsingOverlapListener{})
	if scenario.native.originCount != 1 || scenario.native.ports[0] != 80 ||
		settings.originDestinationForTest(2, nil) != "203.0.113.12:443" {
		t.Fatal("shared SDK settings lost original native origin registration/destination")
	}
	settings.afterOriginsClosedForTest()
	if scenario.native.originCount != 0 || scenario.native.ports != [nativeBrowsingOriginCount]int{} {
		t.Fatal("shared SDK settings lost original origin cleanup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idleClosed := false
	if err := settings.beforePageNavigationForTest(ctx, 0, func() { idleClosed = true }); err != nil || scenario.workCtx != ctx || idleClosed {
		t.Fatalf("shared SDK settings lost original pre-navigation owner/hook: %v", err)
	}
	clientConnection, serverConnection := net.Pipe()
	defer clientConnection.Close()
	defer serverConnection.Close()
	trace := settings.requestTraceForTest(nativeBrowsingURL(0, 0))
	if trace == nil || trace.GotConn == nil || settings.requestTraceForTest(nativeBrowsingURL(1, 2)) != nil {
		t.Fatal("shared SDK settings changed selected CSS trace scope")
	}
	trace.GotConn(httptrace.GotConnInfo{Conn: clientConnection})
	if scenario.cssGotConn.Load() != 1 || scenario.cssClientTuple == "" {
		t.Fatal("shared SDK settings lost original CSS request receiver")
	}
	expected := nativeBrowsingAsset(0, 0)
	reader := bytes.NewReader(expected)
	if settings.wrapResponseReaderForTest(nativeBrowsingURL(3, 1), reader) != reader {
		t.Fatal("shared SDK settings wrapped an unrelated body")
	}
	cssReader := settings.wrapResponseReaderForTest(nativeBrowsingURL(0, 0), reader)
	buffer := make([]byte, 2*nativeBrowsingHeldPrefixBytes)
	if count, err := cssReader.Read(buffer); count != nativeBrowsingHeldPrefixBytes || err != nil ||
		!bytes.Equal(buffer[:count], expected[:nativeBrowsingHeldPrefixBytes]) || scenario.cssClientBytes.Load() != nativeBrowsingHeldPrefixBytes {
		t.Fatalf("shared SDK settings changed the original CSS read boundary: bytes=%d err=%v", count, err)
	}
	select {
	case <-scenario.prefixRead:
	default:
		t.Fatal("shared SDK settings lost the original CSS prefix receipt")
	}
	settings.afterFetchForTest(nativeBrowsingURL(0, -1), nativeBrowsingAssetResult{bytes: nativeBrowsingHTMLBytes})
	settings.afterFetchForTest(nativeBrowsingURL(0, 1), nativeBrowsingAssetResult{bytes: nativeBrowsingAssetBytes(0, 1)})
	if scenario.htmlFetches.Load() != 1 || scenario.assetFetches.Load() != 1 || scenario.firstErr != nil {
		t.Fatal("shared SDK settings lost original request accounting")
	}
	select {
	case <-scenario.jsDone:
	default:
		t.Fatal("shared SDK settings lost the original JS completion receipt")
	}
	request := httptest.NewRequest(http.MethodGet, "https://origin.example/asset/3/2", nil)
	writer := httptest.NewRecorder()
	delegated := false
	wrapped := settings.wrapOriginHandlerForTest(http.HandlerFunc(func(receivedWriter http.ResponseWriter, receivedRequest *http.Request) {
		delegated = receivedWriter == writer && receivedRequest == request
	}))
	wrapped.ServeHTTP(writer, request)
	if !delegated {
		t.Fatal("shared SDK settings changed the original unrelated handler handoff")
	}
}
