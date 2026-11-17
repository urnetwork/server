// Same-site planning and purpose controls preserve original body custody.
package perfvar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// Exercise the generated HTML parser and URL plan for every actual request.
// Scheme, paths, byte extents and the original rotating control stay unchanged.
func TestPrivateRapidSameSitePlanPreservesPayloadAndMode(t *testing.T) {
	mid := newPrivateSdkSameSiteWorkload()
	defer mid.close()
	control := newPrivateSdkMidSession(false, false)
	defer control.close()
	for _, secure := range []bool{false, true} {
		originalUrl := nativeBrowsingURL
		originalHtml := nativeBrowsingHTML
		if secure {
			originalUrl = func(page, asset int) string {
				return strings.Replace(nativeBrowsingURL(page, asset), "http://", "https://", 1)
			}
			originalHtml = func(page int) []byte {
				text := strings.ReplaceAll(string(nativeBrowsingHTML(page)), "http://", "https://")
				return []byte(text[:nativeBrowsingHTMLBytes])
			}
		}
		for page := range nativeBrowsingPageCount {
			body := mid.rewriteHtml(originalUrl, originalHtml, page)
			if len(body) != nativeBrowsingHTMLBytes || !bytes.Equal(control.rewriteHtml(originalUrl, originalHtml, page), originalHtml(page)) {
				t.Fatalf("HTML extent or rotating control changed at page %d", page)
			}
			assets, next, err := nativeBrowsingReferences(body)
			if err != nil || next != mid.rewriteUrl(originalUrl, page+1, -1) {
				t.Fatalf("same-site document lost its actual next anchor: page=%d err=%v", page, err)
			}
			total := len(body)
			for asset := -1; asset < nativeBrowsingAssetCount; asset++ {
				target := mid.rewriteUrl(originalUrl, page, asset)
				parsed, err := url.Parse(target)
				original, originalErr := url.Parse(originalUrl(page, asset))
				if err != nil || originalErr != nil || parsed.Host != "origin0.test" || parsed.Scheme != original.Scheme || parsed.Path != original.Path || parsed.RawQuery != original.RawQuery || parsed.Fragment != original.Fragment {
					t.Fatalf("request escaped the same-site pool or changed its route: page=%d asset=%d", page, asset)
				}
				if control.rewriteUrl(originalUrl, page, asset) != originalUrl(page, asset) {
					t.Fatal("matched rotating control changed")
				}
				if asset >= 0 {
					if assets[asset] != target {
						t.Fatalf("HTML asset did not select the same-site URL: page=%d asset=%d", page, asset)
					}
					total += nativeBrowsingAssetBytes(page, asset)
				}
			}
			if total != nativeBrowsingPageBytes(page) || total < 50*1024 || 300*1024 < total {
				t.Fatalf("page payload plan changed at page %d", page)
			}
		}
	}
	serverTls, clientTls, err := privateSdkBrowsingTlsConfigs()
	if err != nil {
		t.Fatal(err)
	}
	settings := nativeBrowsingRunSettings{midSessionForTest: mid, maxLivePages: 4, pressure: &nativeBrowsingPressureObservation{}, nextNavigationOverlap: true, originServerTlsForTest: serverTls, originClientTlsForTest: clientTls}
	if err := settings.validate(); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		mid.cold, mid.fault = flags[0], flags[1]
		if settings.validate() == nil {
			t.Fatal("same-site comparison accepted a cold destination or fault")
		}
	}
	mid.cold, mid.fault = false, false
	mid.sameSite = false
	if settings.validate() == nil {
		t.Fatal("ordinary purpose accepted a rotating-origin plan")
	}
	mid.sameSite = true
	mid.purpose = privateSdkBrowsingPurpose("synthetic-unknown-purpose")
	if settings.validate() == nil {
		t.Fatal("unconfigured browsing purpose was accepted")
	}
	mid.purpose = privateSdkSameSiteWorkloadPurpose
	called := false
	handler := mid.wrapHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://origin1.test/page/0", nil))
	if called || response.Code != http.StatusMisdirectedRequest || mid.firstErr == nil || mid.sameSiteRequests != 0 {
		t.Fatal("actual foreign-authority request was silently counted as same-site")
	}
}

// Completed or not-yet-started work is not a live abandoned request. Preserve
// the entry sample when workers finish later, alongside the exact HTTP phases.
func TestPrivateRapidSameSiteNavigationCensusPreservesPhases(t *testing.T) {
	pressure := &nativeBrowsingPressureObservation{slowNavigation: time.Hour, captureBoundaryForTest: func(*fullTunPath, int, *nativeBrowsingPressureObservation) *nativeBrowsingPressureBoundary {
		return &nativeBrowsingPressureBoundary{}
	}}
	for _, row := range []struct {
		page, asset                 int
		started, canceled, finished bool
	}{
		{page: 9, asset: 0, started: true, canceled: true},
		{page: 9, asset: 1, started: true},
		{page: 9, asset: 2},
		{page: 9, asset: 3, started: true, canceled: true, finished: true},
		{page: 8, asset: 0, started: true, canceled: true},
		{page: 6, asset: 0, started: true, canceled: true},
	} {
		progress := &pressure.assets[row.page][row.asset]
		progress.expectedBytes = 100
		progress.started.Store(row.started)
		progress.cancelRequested.Store(row.canceled)
		progress.finished.Store(row.finished)
	}
	trace := pressure.beginNavigation(10, 4)
	row := pressure.Navigations[10]
	if row.UnfinishedPriorAssets != 4 || row.LivePriorAssets != 3 || row.CanceledPriorAssets != 2 || pressure.MaximumLivePriorAssets != 3 || pressure.MaximumCanceledPriorAssets != 2 {
		t.Fatalf("boundary census mixed finished, unstarted, retired or cancellation-pending work: %+v", row)
	}
	trace.attempts.Store(1)
	trace.reused.Store(true)
	trace.phases[0].Store(101)
	trace.phases[1].Store(301)
	trace.phases[6].Store(501)
	trace.phases[7].Store(801)
	for page := range pressure.assets {
		for asset := range pressure.assets[page] {
			pressure.assets[page][asset].finished.Store(true)
		}
	}
	pressure.finishNavigation(nil, 10, trace, nativeBrowsingAssetResult{complete: 1000 * time.Nanosecond})
	row = pressure.Navigations[10]
	if row.LivePriorAssets != 3 || row.CanceledPriorAssets != 2 || !row.Reused || row.ConnectionAcquireNs != 200 || row.WrittenToFirstByteNs != 300 || row.ElapsedNs != 1000 {
		t.Fatalf("completion erased the entry census or HTTP phases: %+v", row)
	}
	pressure.beginNavigation(11, 4)
	if pressure.Navigations[11].LivePriorAssets != 0 || pressure.Navigations[11].CanceledPriorAssets != 0 || pressure.MaximumLivePriorAssets != 3 || pressure.MaximumCanceledPriorAssets != 2 {
		t.Fatal("joined assets remained live or erased the observed boundary maximum")
	}
}

// The ordinary same-site purpose retains every original body handoff and the
// production path. Strict midpoint experiments retain their separate gates.
func TestPrivateFullTunH1Sdk32RapidSameSiteAssetsInFlight100Pages(t *testing.T) {
	privateSdkRunMidSessionOriginMode(t, false, false, true)
}

// Install the real composed navigation hook with fixed original-owner
// receipts. The readiness read is the only injected boundary.
func privateSdkPreparePurposeBoundary(t testing.TB, mid *privateSdkMidSession, view privateSdkBrowsingReadinessSnapshot, stable bool) (nativeBrowsingRunSettings, *int) {
	t.Helper()
	t.Cleanup(mid.close)
	beforeCalls := new(int)
	settings := nativeBrowsingRunSettings{maxLivePages: 4, pressure: &nativeBrowsingPressureObservation{}, beforePageNavigationForTest: func(context.Context, int, func()) error {
		(*beforeCalls)++
		return nil
	}}
	mid.readinessForTest = func() (privateSdkBrowsingReadinessSnapshot, bool) { return view, stable }
	mid.configure(&settings)
	pages := settings.newPages()
	progress := &settings.pressure.assets[privateSdkMidPage-1][2]
	progress.expectedBytes = int64(nativeBrowsingAssetBytes(privateSdkMidPage-1, 2))
	progress.receivedBytes.Store(progress.expectedBytes - 1)
	progress.started.Store(true)
	progress.bodyOpened.Store(true)
	pages[privateSdkMidPage-1].serverReceipts[2].Set(nativeBrowsingServerReceipt{started: 1})
	for page := 0; page < privateSdkMidPage; page++ {
		settings.afterFetchForTest(fmt.Sprintf("https://origin0.test/page/%d", page), nativeBrowsingAssetResult{status: http.StatusOK, bytes: nativeBrowsingHTMLBytes})
	}
	mid.providers = 1
	mid.cssClientAttempts, mid.cssServerAttempts = 1, 1
	mid.cssNatRead, mid.cssServerLive = 1, true
	mid.cssClientTuple, mid.cssSource = "synthetic-app|synthetic-origin", "original-owned-socket"
	reader := settings.wrapResponseReaderForTest("https://origin0.test/asset/9/0", bytes.NewReader(bytes.Repeat([]byte{7}, nativeBrowsingHeldPrefixBytes)))
	if count, err := io.Copy(io.Discard, reader); err != nil || count != nativeBrowsingHeldPrefixBytes {
		t.Fatal("original prefix receipt was not installed", count, err)
	}
	return settings, beforeCalls
}

// The actual page-ten callback distinguishes a complete ordinary premise from
// a strict alternative-provider arm for both shared and single local channels.
func TestPrivateRapidSameSitePurposeSeparatesStrictAlternativeArming(t *testing.T) {
	for _, name := range []string{"six-local-five-remote", "six-local-one-remote", "one-local-one-remote"} {
		exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
		distinct := 5
		if name == "six-local-five-remote" {
			locations.Get(4).ClientId = locations.Get(0).ClientId
		} else {
			distinct = 1
			for index := 1; index < locations.Len(); index++ {
				locations.Get(index).ClientId = locations.Get(0).ClientId
			}
		}
		if name == "one-local-one-remote" {
			firstExit, firstLocation := exits.Get(0), locations.Get(0)
			exits, locations = sdk.NewExitList(), sdk.NewConnectedProviderLocationList()
			exits.Add(firstExit)
			locations.Add(firstLocation)
			status.ProviderStateAdded = 1
		}
		view, stable := privateSdkProductionReadinessSnapshotFrom(exits, status, status, locations, providers)
		mid := newPrivateSdkSameSiteWorkload()
		settings, beforeCalls := privateSdkPreparePurposeBoundary(t, mid, view, stable)
		if err := settings.beforePageNavigationForTest(t.Context(), privateSdkMidPage, func() {}); err != nil {
			mid.close()
			t.Fatalf("%s ordinary navigation was gated by a different experiment: %v", name, err)
		}
		if *beforeCalls != 1 || !mid.bodyPremiseAccepted || !mid.boundaryObserved || mid.before != view || mid.armAttempted || mid.armPremiseAccepted || mid.armed.Load() || mid.bridge.active.Load() || (mid.alternativesErr == nil) != (distinct >= 2) {
			mid.close()
			t.Fatalf("%s ordinary body evidence armed or qualified the strict mechanism", name)
		}
		select {
		case <-mid.cssRelease:
			t.Fatal("ordinary preparation released the original body before its real acquisition")
		default:
		}
		trace := settings.requestTraceForTest(t.Context(), "https://origin0.test/page/10")
		trace.GotConn(httptrace.GotConnInfo{Reused: true})
		trace.GotFirstResponseByte()
		select {
		case <-mid.cssRelease:
		default:
			t.Fatal("actual warm acquisition did not release the original body")
		}
		if !mid.navReuse[privateSdkMidPage] || mid.navAttempts[privateSdkMidPage] != 1 || mid.firstByte.IsZero() {
			t.Fatal("ordinary purpose lost actual navigation receipts")
		}
		mid.close()

		strict := newPrivateSdkMidSession(false, false)
		strictSettings, _ := privateSdkPreparePurposeBoundary(t, strict, view, stable)
		err := strictSettings.beforePageNavigationForTest(t.Context(), privateSdkMidPage, func() {})
		armed := strict.armed.Load()
		strict.close()
		if (err == nil) != (distinct >= 2) || armed != (distinct >= 2) || strict.armPremiseAccepted != (distinct >= 2) || !strict.armAttempted || strict.before != view {
			t.Fatalf("%s changed the strict alternatives premise: err=%v armed=%t", name, err, armed)
		}
	}
}

// A valid ordinary public minimum never excuses a missing original body,
// unknown identity, mixed generation, or retained fixture failure.
func TestPrivateRapidSameSitePurposeKeepsBodyAndPublicOwnershipGuards(t *testing.T) {
	for _, name := range []string{"missing-html", "css-replay", "missing-css", "ended-css", "missing-image", "released-image", "minimum", "missing-local", "missing-remote", "foreign-remote", "mixed-generation", "prior-error"} {
		exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
		view, stable := privateSdkProductionReadinessSnapshotFrom(exits, status, status, locations, providers)
		switch name {
		case "minimum":
			view.MinSatisfied = false
		case "missing-local":
			view.MissingLocalIdentities++
		case "missing-remote":
			view.MissingIdentities++
		case "foreign-remote":
			view.Unowned++
		case "mixed-generation":
			stable = false
		}
		mid := newPrivateSdkSameSiteWorkload()
		settings, _ := privateSdkPreparePurposeBoundary(t, mid, view, stable)
		switch name {
		case "missing-html":
			mid.html[privateSdkMidPage-1] = false
		case "css-replay":
			mid.cssServerAttempts++
		case "missing-css":
			mid.cssRead.Store(nativeBrowsingHeldPrefixBytes - 1)
		case "ended-css":
			mid.cssServerLive = false
		case "missing-image":
			settings.pressure.assets[privateSdkMidPage-1][2].receivedBytes.Store(0)
		case "released-image":
			settings.pressure.assets[privateSdkMidPage-1][2].nextNavigation.release()
		case "prior-error":
			mid.recordError(errors.New("synthetic prior failure"))
		}
		err := settings.beforePageNavigationForTest(t.Context(), privateSdkMidPage, func() {})
		mid.close()
		if err == nil || mid.bodyPremiseAccepted || mid.armPremiseAccepted || mid.armed.Load() {
			t.Fatalf("%s bypassed the original body or public ownership requirement", name)
		}
	}
}

// Cancellation is forced inside the public read, after the prefix is ready.
// Both purposes must refuse publication without relying on select ordering.
func TestPrivateRapidSameSitePurposeCannotOutliveItsBoundaryOwner(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		for _, ownerStop := range []bool{false, true} {
			exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
			view, stable := privateSdkProductionReadinessSnapshotFrom(exits, status, status, locations, providers)
			var mid *privateSdkMidSession
			if ordinary {
				mid = newPrivateSdkSameSiteWorkload()
			} else {
				mid = newPrivateSdkMidSession(false, false)
			}
			settings, _ := privateSdkPreparePurposeBoundary(t, mid, view, stable)
			ctx, cancel := context.WithCancel(t.Context())
			reads := 0
			mid.readinessForTest = func() (privateSdkBrowsingReadinessSnapshot, bool) {
				reads++
				if ownerStop {
					mid.close()
				} else {
					cancel()
				}
				return view, stable
			}
			err := settings.beforePageNavigationForTest(ctx, privateSdkMidPage, func() {})
			cancel()
			mid.close()
			if !errors.Is(err, context.Canceled) || reads != 1 || mid.bodyPremiseAccepted || mid.armAttempted || mid.armPremiseAccepted || mid.armed.Load() || t.Context().Err() != nil {
				t.Fatalf("expired boundary published or affected the shared parent: ordinary=%t owner_stop=%t reads=%d err=%v", ordinary, ownerStop, reads, err)
			}
		}
	}
}

// Close publishes cancellation under the same lock as admission. Neither
// boundary can reacquire acceptance after that owner has completed.
func TestPrivateRapidSameSitePurposeCannotAcceptAfterClose(t *testing.T) {
	exits, status, locations, providers := privateSdkBrowsingReadinessViews(t)
	view, stable := privateSdkProductionReadinessSnapshotFrom(exits, status, status, locations, providers)
	for _, ordinary := range []bool{false, true} {
		var mid *privateSdkMidSession
		if ordinary {
			mid = newPrivateSdkSameSiteWorkload()
		} else {
			mid = newPrivateSdkMidSession(false, false)
		}
		privateSdkPreparePurposeBoundary(t, mid, view, stable)
		mid.close()
		var err error
		if ordinary {
			err = mid.observeWorkloadWithReadiness(view, stable, 1, 2)
		} else {
			err = mid.armWithReadiness(view, stable, 1, 2)
		}
		if !errors.Is(err, context.Canceled) || mid.bodyPremiseAccepted || mid.armAttempted || mid.armPremiseAccepted || mid.armed.Load() || mid.bridge.active.Load() {
			t.Fatalf("closed boundary accepted a new premise: ordinary=%t err=%v", ordinary, err)
		}
	}
}

// Capture the actual reporting entry point without marking an intentionally
// incomplete control as a failure of its enclosing regression.
type privateSdkPurposeReportRecorder struct {
	testing.TB
	errorMessages []string
	report        map[string]any
	reportErr     error
}

// Keep the fixture's emitted verdict; the enclosing test asserts it.
func (self *privateSdkPurposeReportRecorder) Error(values ...any) {
	self.errorMessages = append(self.errorMessages, fmt.Sprint(values...))
}

// Decode only this fixture's bounded report and retain no application body.
func (self *privateSdkPurposeReportRecorder) Logf(format string, values ...any) {
	message := fmt.Sprintf(format, values...)
	if strings.HasPrefix(message, "SDK_MID_SESSION_V1 ") {
		self.reportErr = json.Unmarshal([]byte(strings.TrimPrefix(message, "SDK_MID_SESSION_V1 ")), &self.report)
	}
}

// Supply positive completion receipts independently of mechanism qualification.
func privateSdkCompletePurposeWorkload(mid *privateSdkMidSession) *nativeBrowsingObservation {
	for page := range mid.html {
		mid.html[page] = true
	}
	mid.cssComplete = true
	mid.sameSiteRequests = nativeBrowsingPageCount * (1 + nativeBrowsingAssetCount)
	mid.firstByte = mid.tap.Add(time.Nanosecond)
	return &nativeBrowsingObservation{CompletedNavigations: nativeBrowsingPageCount, OverlapProofs: nativeBrowsingPageCount - 1}
}

// Ordinary success still needs all hundred html responses, five hundred
// requests, ninety-nine overlaps and the original completed css owner.
func TestPrivateRapidSameSiteFinishRequiresTheCompleteOriginalWorkload(t *testing.T) {
	for _, name := range []string{"complete", "missing-html", "missing-navigation", "missing-overlap", "missing-request", "missing-css", "missing-body-premise"} {
		view := privateSdkBrowsingReadinessSnapshot{Quality: 1, Exits: 1, LocalDistinct: 1, MinSatisfied: true, Locations: 1, DistinctOwned: 1, OwnedMask: 1}
		mid := newPrivateSdkSameSiteWorkload()
		settings, _ := privateSdkPreparePurposeBoundary(t, mid, view, true)
		if err := settings.beforePageNavigationForTest(t.Context(), privateSdkMidPage, func() {}); err != nil {
			t.Fatal(err)
		}
		observation := privateSdkCompletePurposeWorkload(mid)
		switch name {
		case "missing-html":
			mid.html[nativeBrowsingPageCount-1] = false
		case "missing-navigation":
			observation.CompletedNavigations--
		case "missing-overlap":
			observation.OverlapProofs--
		case "missing-request":
			mid.sameSiteRequests--
		case "missing-css":
			mid.cssComplete = false
		case "missing-body-premise":
			mid.bodyPremiseAccepted = false
		}
		recorder := &privateSdkPurposeReportRecorder{TB: t}
		mid.finish(recorder, observation)
		complete := name == "complete"
		if recorder.reportErr != nil || recorder.report == nil || recorder.report["complete_workload_witness"] != complete || (len(recorder.errorMessages) == 0) != complete {
			t.Fatalf("%s incomplete ordinary workload was reported as success: errors=%v report_error=%v", name, recorder.errorMessages, recorder.reportErr)
		}
		if recorder.report["purpose"] != string(privateSdkSameSiteWorkloadPurpose) || recorder.report["mechanism_qualified"] != false || recorder.report["arm_premise_accepted"] != false || recorder.report["public_alternatives_qualified"] != false {
			t.Fatal("ordinary completion masqueraded as strict mechanism qualification")
		}
	}
}

// A complete body census cannot qualify a strict mechanism that never armed.
// This exercises the emitted verdict rather than only its readiness predicate.
func TestPrivateMidSessionFinishRejectsAnUnqualifiedMechanism(t *testing.T) {
	view := privateSdkBrowsingReadinessSnapshot{Quality: 1, Exits: 1, LocalDistinct: 1, MinSatisfied: true, Locations: 1, DistinctOwned: 1, OwnedMask: 1}
	mid := newPrivateSdkMidSession(false, false)
	settings, _ := privateSdkPreparePurposeBoundary(t, mid, view, true)
	if err := settings.beforePageNavigationForTest(t.Context(), privateSdkMidPage, func() {}); err == nil {
		t.Fatal("one provider armed the strict mechanism")
	}
	// Isolate qualification from an otherwise complete original workload.
	mid.bodyPremiseAccepted = true
	observation := privateSdkCompletePurposeWorkload(mid)
	recorder := &privateSdkPurposeReportRecorder{TB: t}
	mid.finish(recorder, observation)
	if recorder.reportErr != nil || recorder.report["complete_workload_witness"] != true || recorder.report["mechanism_qualified"] != false || len(recorder.errorMessages) != 1 || recorder.errorMessages[0] != "MID_SESSION_MECHANISM_PREMISE_UNQUALIFIED" {
		t.Fatalf("complete traffic qualified an unarmed strict mechanism: errors=%v report_error=%v", recorder.errorMessages, recorder.reportErr)
	}
}
