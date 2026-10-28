// Component controls retain the original request plan and exercise the cold
// helpers without constructing the public SDK/provider runtime.
package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// Only page1 HTML and page0's next href acquire the fifth origin. Every
// original asset reference and page/body byte budget remains unchanged.
func TestNativeBrowsingColdLinkKeepsOriginal100PagePlan(t *testing.T) {
	originalURL := func(page, asset int) string {
		return strings.Replace(nativeBrowsingURL(page, asset), "http://", "https://", 1)
	}
	originalHTML := func(page int) []byte {
		body := strings.ReplaceAll(string(nativeBrowsingHTML(page)), "http://", "https://")
		return []byte(body[:nativeBrowsingHTMLBytes])
	}
	htmlCount, assetCount, coldTargets := 0, 0, 0
	for page := range nativeBrowsingPageCount {
		target := nativeBrowsingColdLinkURL(originalURL, page, -1)
		wantOrigin := page % nativeBrowsingOriginCount
		if page == 1 {
			wantOrigin = nativeBrowsingColdLinkOrigin
			coldTargets++
		}
		wantTarget := fmt.Sprintf("https://origin%d.test/page/%d", wantOrigin, page)
		actualPage, slot, origin, err := nativeBrowsingColdLinkRequestSlot(target)
		if err != nil || target != wantTarget || actualPage != page || slot != nativeBrowsingAssetCount || origin != wantOrigin {
			t.Fatalf("page %d lost its owned HTML target: target=%s page=%d slot=%d origin=%d err=%v", page, target, actualPage, slot, origin, err)
		}
		htmlCount++
		body := nativeBrowsingColdLinkHTML(originalURL, originalHTML, page)
		wantBody := originalHTML(page)
		if page == 0 {
			wantBody = []byte(strings.Replace(string(wantBody), originalURL(1, -1), "https://origin4.test/page/1", 1))
		}
		if len(body) != nativeBrowsingHTMLBytes || !bytes.Equal(body, wantBody) {
			t.Fatalf("page %d changed HTML bytes beyond its one cold href", page)
		}
		assets, next, err := nativeBrowsingReferences(body)
		if err != nil || next != nativeBrowsingColdLinkURL(originalURL, page+1, -1) {
			t.Fatalf("page %d did not expose its actual next link: next=%s err=%v", page, next, err)
		}
		pageBytes := len(body)
		for asset := range nativeBrowsingAssetCount {
			target := nativeBrowsingColdLinkURL(originalURL, page, asset)
			actualPage, slot, origin, err := nativeBrowsingColdLinkRequestSlot(target)
			if err != nil || target != originalURL(page, asset) || assets[asset] != target || actualPage != page || slot != asset || origin != (page+asset)%nativeBrowsingOriginCount {
				t.Fatalf("page %d asset %d left its original four-origin slot: %s err=%v", page, asset, target, err)
			}
			if strings.Contains(target, "origin4.test") {
				t.Fatalf("page %d asset %d warmed the cold destination", page, asset)
			}
			body := nativeBrowsingAsset(page, asset)
			if len(body) != nativeBrowsingAssetBytes(page, asset) {
				t.Fatalf("page %d asset %d changed its original byte budget", page, asset)
			}
			pageBytes += len(body)
			assetCount++
		}
		if pageBytes != nativeBrowsingPageBytes(page) {
			t.Fatalf("page %d changed the original complete byte budget", page)
		}
	}
	if htmlCount != 100 || assetCount != 400 || coldTargets != 1 {
		t.Fatalf("original plan changed: HTML=%d assets=%d cold=%d", htmlCount, assetCount, coldTargets)
	}
	for _, target := range []string{
		"https://origin1.test/page/1", "https://origin4.test/page/0", "https://origin4.test/asset/0/0",
		"https://unowned.test/page/1", "http://origin4.test/page/1", "https://origin4.test/page/1?extra=1", "https://origin4.test/page/1#extra",
	} {
		if _, _, _, err := nativeBrowsingColdLinkRequestSlot(target); err == nil {
			t.Fatalf("strict cold-link slot accepted %s", target)
		}
	}
}

// The real wrapped readers publish independent positive body barriers. Releasing
// or canceling JS cannot release CSS; each worker returns its original bytes.
func TestNativeBrowsingColdLinkTypedBodyHoldsAreIndependent(t *testing.T) {
	for _, cancelJs := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			jsCtx, cancelJsBody := context.WithCancel(ctx)
			pressure := &nativeBrowsingPressureObservation{}
			witness := newNativeBrowsingColdLinkWitness(nil, pressure, false)
			var completed [2]chan struct{}
			var results [2]chan nativeBrowsingAssetResult
			defer func() {
				cancelJsBody()
				cancel()
				witness.close()
				synctest.Wait()
				for _, done := range completed {
					if done != nil {
						<-done
					}
				}
			}()
			if err := witness.checkHold(&witness.css, nativeBrowsingColdCss); err == nil {
				t.Fatal("CSS accepted without its original request/progress owner")
			}
			for asset := 0; asset < 2; asset++ {
				body := nativeBrowsingAsset(0, asset)
				progress := &pressure.assets[0][asset]
				progress.expectedBytes = int64(len(body))
				progress.started.Store(true)
				progress.bodyOpened.Store(true)
				witness.requests[0][asset] = nativeBrowsingOwnedColdRequestForTest(witness.requests[0][asset])
				readCtx := ctx
				if asset == 1 {
					readCtx = jsCtx
				}
				target := strings.Replace(nativeBrowsingURL(0, asset), "http://", "https://", 1)
				nativeBrowsingModelDispatchForTest(witness, readCtx, target)
				wrapped := witness.wrapReader(readCtx, target, bytes.NewReader(body))
				reader := &nativeBrowsingProgressReader{reader: wrapped, pressure: pressure, progress: progress}
				completed[asset], results[asset] = make(chan struct{}), make(chan nativeBrowsingAssetResult, 1)
				go func(done chan struct{}, result chan nativeBrowsingAssetResult) {
					defer close(done)
					defer progress.finished.Store(true)
					defer progress.bodyClosed.Store(true)
					actual, err := io.ReadAll(reader)
					result <- nativeBrowsingAssetResult{status: 200, bytes: len(actual), err: nativeBrowsingResponseError(200, actual, body, err)}
				}(completed[asset], results[asset])
			}
			<-witness.css.gate.paused
			<-witness.js[0].gate.paused
			synctest.Wait()
			if witness.css.kind != nativeBrowsingColdCss || witness.css.page != 0 || witness.css.slot != 0 ||
				witness.js[0].kind != nativeBrowsingColdJs || witness.js[0].page != 0 || witness.js[0].slot != 1 ||
				witness.css.gate == witness.js[0].gate {
				t.Fatal("CSS0 and JS0 did not retain distinct typed original slots")
			}
			if err := witness.checkHold(&witness.css, nativeBrowsingColdCss); err != nil {
				t.Fatal("actual CSS body pause: ", err)
			}
			if err := witness.checkHold(&witness.js[0], nativeBrowsingColdJs); err != nil {
				t.Fatal("actual JS body pause: ", err)
			}
			if witness.checkHold(&witness.css, nativeBrowsingColdJs) == nil || witness.checkHold(&witness.js[0], nativeBrowsingColdCss) == nil {
				t.Fatal("CSS/JS body receipt types were interchangeable")
			}
			css := &pressure.assets[0][0]
			expected := css.expectedBytes
			css.expectedBytes = nativeBrowsingHeldPrefixBytes
			if err := witness.checkHold(&witness.css, nativeBrowsingColdCss); err == nil {
				t.Fatal("an exactly1KiB body was mistaken for an unread response")
			}
			css.expectedBytes = expected
			if cancelJs {
				cancelJsBody()
			} else {
				witness.js[0].gate.release()
				witness.js[0].gate.release()
			}
			synctest.Wait()
			js := <-results[1]
			<-completed[1]
			if cancelJs {
				if js.bytes != nativeBrowsingHeldPrefixBytes || !errors.Is(js.err, context.Canceled) || errors.Is(js.err, errNativeBrowsingResponseMismatch) {
					t.Fatalf("JS cancellation lost its validated original prefix: %+v", js)
				}
				select {
				case <-witness.js[0].gate.released:
					t.Fatal("JS cancellation fabricated a navigation release")
				default:
				}
			} else if js.err != nil || js.bytes != nativeBrowsingAssetBytes(0, 1) {
				t.Fatalf("released JS did not complete its original body: %+v", js)
			}
			if err := witness.checkHold(&witness.css, nativeBrowsingColdCss); err != nil || css.receivedBytes.Load() != nativeBrowsingHeldPrefixBytes {
				t.Fatal("JS changed the independently held CSS body: ", err)
			}
			select {
			case <-completed[0]:
				t.Fatal("CSS owner completed before its own release")
			default:
			}
			witness.css.gate.release()
			witness.css.gate.release()
			synctest.Wait()
			result := <-results[0]
			<-completed[0]
			if result.err != nil || result.bytes != nativeBrowsingAssetBytes(0, 0) || ctx.Err() != nil {
				t.Fatalf("CSS release changed its original body or parent lifetime: %+v", result)
			}
		})
	}
}

// Distinct documentation ports exercise the registry without opening sockets.
type nativeBrowsingColdRegistryListener struct {
	*nativeBrowsingOverlapListener
	port int
}

func (self *nativeBrowsingColdRegistryListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: self.port}
}

// The fifth slot is opt-in and bounded; clearing registration removes every
// admitted endpoint. The fault predicate still targets only old providers.
func TestNativeBrowsingColdLinkEndpointScopeAndRefusal(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		native := &sdkBrowsingNative{coldLinkOriginForTest: enabled}
		limit := nativeBrowsingOriginCount
		if enabled {
			limit++
		}
		for ordinal := 0; ordinal < nativeBrowsingOriginCount+2; ordinal++ {
			listener := &nativeBrowsingColdRegistryListener{nativeBrowsingOverlapListener: &nativeBrowsingOverlapListener{
				connections: make(chan net.Conn), closed: make(chan struct{})}, port: 31000 + ordinal}
			defer listener.Close()
			native.addOrigin(listener)
			if native.originCount != min(ordinal+1, limit) {
				t.Fatalf("cold=%t registration %d escaped its endpoint bound: %d", enabled, ordinal, native.originCount)
			}
		}
		for ordinal, port := range native.ports {
			if port != 31000+ordinal {
				t.Fatal("cold registration changed an original endpoint")
			}
		}
		if enabled && native.coldLinkPortForTest != 31004 || !enabled && native.coldLinkPortForTest != 0 {
			t.Fatal("fifth registration was absent or escaped its opt-in")
		}
		for ordinal := 0; ordinal < nativeBrowsingOriginCount+2; ordinal++ {
			ip := net.IPv4(203, 0, 113, byte(10+ordinal))
			if native.matches(ip, 443, clientconnect.IpProtocolTcp) != (ordinal < limit) ||
				native.matches(ip, 444, clientconnect.IpProtocolTcp) || native.matches(ip, 443, clientconnect.IpProtocolUdp) {
				t.Fatalf("cold=%t endpoint %d lost exact address/port/protocol scope", enabled, ordinal)
			}
		}
		native.clearOrigins()
		native.clearOrigins()
		if native.originCount != 0 || native.ports != [nativeBrowsingOriginCount]int{} || native.coldLinkPortForTest != 0 || native.coldLinkOriginForTest ||
			native.matches(net.IPv4(203, 0, 113, 14), 443, clientconnect.IpProtocolTcp) {
			t.Fatal("origin cleanup retained a default or fifth endpoint")
		}
	}
	for _, cold := range []bool{false, true} {
		for _, armed := range []bool{false, true} {
			for ordinal := -1; ordinal <= sdkBrowsingProviderCount; ordinal++ {
				for _, address := range []string{"203.0.113.10:443", "203.0.113.11:443", "203.0.113.14:443", "203.0.113.14:444", "unowned.test:443"} {
					target := "203.0.113.11:443"
					if cold {
						target = "203.0.113.14:443"
					}
					want := armed && (0 <= ordinal && ordinal < 4 || ordinal == 6) && address == target
					if sdkBrowsingNavigationDialRefused(cold, armed, ordinal, address) != want {
						t.Fatalf("refusal escaped its exact target: cold=%t armed=%t provider=%d address=%s", cold, armed, ordinal, address)
					}
				}
			}
		}
	}
	// Visibility alone, a healthy alternate, or a missing actual request
	// receipt cannot qualify the refusal-to-admission observation.
	for _, missing := range []string{"none", "old-refusal", "visible", "html", "late-provider", "tuple", "fresh", "tls", "one-attempt"} {
		scenario := newSdkBrowsingScenario(nil)
		scenario.refusalCounts[0], scenario.lateConstructedNs, scenario.lateApiVisibleNs, scenario.lateReady = 1, 1, 2, true
		witness := newNativeBrowsingColdLinkWitness(scenario, nil, false)
		witness.row.ColdEdges, witness.row.ColdServerProvider, witness.row.ColdTupleOwned = 1, 4, true
		request := &witness.requests[1][nativeBrowsingAssetCount]
		request.clientSession, request.verifiedClientTls, request.clientAcquisitions, request.serverAttempts = &tls.Conn{}, true, 1, 1
		switch missing {
		case "old-refusal":
			scenario.refusalCounts[0], scenario.refusalCounts[5] = 0, 1
		case "visible":
			scenario.lateApiVisibleNs = 0
		case "html":
			witness.row.ColdEdges = 0
		case "late-provider":
			witness.row.ColdServerProvider = 5
		case "tuple":
			witness.row.ColdTupleOwned = false
		case "fresh":
			witness.row.ColdClientReused = true
		case "tls":
			request.verifiedClientTls = false
		case "one-attempt":
			request.serverAttempts = 2
		}
		row, err := witness.snapshot()
		want := "UNEXERCISED_ADMISSION_CHAIN"
		if missing == "none" {
			want = "LATE_PROVIDER_ADMITTED"
		}
		if err != nil || row.AdmissionChain != want || row.PublicationIsAdmission || row.BodyReadIsPacketCustody {
			t.Fatalf("admission receipt %s: chain=%s want=%s err=%v", missing, row.AdmissionChain, want, err)
		}
	}
}

// Real certificate verification retains CA and hostname rejection. The actual
// shared transport/server factories select H1 or H2 without starting either.
func TestNativeBrowsingColdLinkTlsNamesAndProtocolSettings(t *testing.T) {
	for _, origins := range []int{nativeBrowsingOriginCount, nativeBrowsingOriginCount + 1} {
		var serverTls, clientTls *tls.Config
		var err error
		if origins == nativeBrowsingOriginCount {
			serverTls, clientTls, err = nativeBrowsingTlsConfigs()
		} else {
			serverTls, clientTls, err = nativeBrowsingTlsConfigsForOrigins(origins)
		}
		if err != nil {
			t.Fatal(err)
		}
		if clientTls.InsecureSkipVerify || clientTls.RootCAs == nil || clientTls.MinVersion != tls.VersionTLS13 || serverTls.MinVersion != tls.VersionTLS13 {
			t.Fatal("cold certificate configuration weakened verification or TLS version")
		}
		leaf, err := x509.ParseCertificate(serverTls.Certificates[0].Certificate[0])
		if err != nil || len(leaf.DNSNames) != origins {
			t.Fatal("cold certificate changed its bounded SAN catalogue: ", err)
		}
		for origin := 0; origin <= origins; origin++ {
			_, err := leaf.Verify(x509.VerifyOptions{Roots: clientTls.RootCAs, DNSName: fmt.Sprintf("origin%d.test", origin)})
			if (err == nil) != (origin < origins) {
				t.Fatalf("origins=%d SAN%d verification error=%v", origins, origin, err)
			}
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: x509.NewCertPool(), DNSName: "origin0.test"}); err == nil {
			t.Fatal("cold certificate trusted an unowned CA")
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: clientTls.RootCAs, DNSName: "unowned.test"}); err == nil {
			t.Fatal("cold certificate trusted an unowned name")
		}
		for _, h2 := range []bool{false, true} {
			settings := nativeBrowsingRunSettings{originServerTlsForTest: serverTls, originClientTlsForTest: clientTls, innerHttp2ForTest: h2}
			if origins == nativeBrowsingOriginCount+1 {
				coldSettings := settings
				coldSettings.maxLivePages, coldSettings.pressure = 4, &nativeBrowsingPressureObservation{}
				coldSettings.nextNavigationOverlap, coldSettings.coldLinkForTest = true, true
				if err := coldSettings.validate(); err != nil {
					t.Fatal("owned cold-link settings were refused: ", err)
				}
			}
			transport := newNativeBrowsingInnerTransport(func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("pure protocol settings control must not dial")
			}, settings)
			listener := &nativeBrowsingOverlapListener{connections: make(chan net.Conn), closed: make(chan struct{})}
			server, secure := newNativeBrowsingInnerServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), listener, settings)
			defer secure.Close()
			if transport.MaxConnsPerHost != 4 || transport.MaxIdleConnsPerHost != 4 || transport.MaxIdleConns != 16 || transport.TLSClientConfig.InsecureSkipVerify {
				t.Fatal("cold comparison changed the original per-origin connection geometry")
			}
			if h2 {
				if transport.Protocols == nil || !transport.Protocols.HTTP2() || transport.Protocols.HTTP1() ||
					server.Protocols == nil || !server.Protocols.HTTP2() || server.Protocols.HTTP1() ||
					strings.Join(transport.TLSClientConfig.NextProtos, ",") != "h2" || strings.Join(server.TLSConfig.NextProtos, ",") != "h2" {
					t.Fatal("cold H2 settings admitted a protocol fallback")
				}
			} else if transport.Protocols != nil || transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 ||
				strings.Join(transport.TLSClientConfig.NextProtos, ",") != "http/1.1" || server.Protocols != nil {
				t.Fatal("cold H1 settings changed the default inner protocol")
			}
			if err := secure.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Join(clientTls.NextProtos, ",") != "http/1.1" || strings.Join(serverTls.NextProtos, ",") != "http/1.1" {
			t.Fatal("protocol factory mutated its original TLS configuration")
		}
	}
	for _, count := range []int{0, 3, 6} {
		if _, _, err := nativeBrowsingTlsConfigsForOrigins(count); err == nil {
			t.Fatalf("unowned certificate origin count %d accepted", count)
		}
	}
}

// Existing catalogue hashes, four live pages and both original time budgets
// stay authoritative; this comparison constructs no alternate network shaper.
func TestSdkBrowsingColdLinkPlansRetainOriginalBudgets(t *testing.T) {
	baseline := newNativeBrowsingPressurePlan()
	for _, name := range []string{"wifi-good", cellEdge5mDown1mUpName} {
		plan, err := newSdkBrowsingColdLinkPlan(name)
		if err != nil {
			t.Fatal(err)
		}
		want := initialNetworkProfiles(4013)["wifi-good"]
		if name == cellEdge5mDown1mUpName {
			want = cellEdgeNetworkProfiles(4013)[name]
		}
		actualHash, actualErr := plan.profile.hash()
		wantHash, wantErr := want.hash()
		providerHash, providerErr := plan.provider.hash()
		baselineProviderHash, baselineProviderErr := baseline.provider.hash()
		if errors.Join(actualErr, wantErr, providerErr, baselineProviderErr) != nil || actualHash != wantHash || providerHash != baselineProviderHash ||
			!reflect.DeepEqual(plan.resources, baseline.resources) || plan.deadline != 300*time.Second || plan.maxLivePages != 4 ||
			sdk.DefaultDeviceLocalSettings().SendTimeout != 5*time.Second || plan.resources.LogicalDataLaneCount != 0 {
			t.Fatalf("profile %s changed its catalogue, resources or original budgets", name)
		}
		settings := plan.runSettings(&nativeBrowsingPressureObservation{})
		if err := settings.validate(); err != nil || settings.maxLivePages != 4 || !settings.nextNavigationOverlap || settings.coldLinkForTest || settings.innerHttp2ForTest {
			t.Fatal("cold plan leaked an opt-in into the original workload settings: ", err)
		}
	}
	if _, err := newSdkBrowsingColdLinkPlan("unowned-profile"); err == nil {
		t.Fatal("cold comparison accepted an unowned network profile")
	}
}

// The real stop seals constructor admission before canceling and joining.
// Explicit channels force both lock orders; no SDK/database owner is started.
func TestSdkBrowsingColdLinkStopSealsAndJoinsConstructor(t *testing.T) {
	// A completed builder removes its link. A canceled builder joins the actual
	// propagation callback, including its final explicitly held cancellation.
	for _, finishFirst := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ownerCtx, cancelOwner := context.WithCancel(context.Background())
			fixtureCtx, cancelFixture := context.WithCancel(context.Background())
			setupCtx, cancelSetup := context.WithCancel(context.Background())
			guardCtx, guardCancel := context.WithTimeout(context.Background(), sdk.DefaultDeviceLocalSettings().SendTimeout)
			defer guardCancel()
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce, finishOnce sync.Once
			resume := func() { releaseOnce.Do(func() { close(release) }) }
			linked := newSdkBrowsingScenario(nil)
			linked.coldLinkForTest = true
			linked.fixture = &sdkBrowsingSetupFixture{t: t, ctx: fixtureCtx, setupCtx: setupCtx, cancelSetup: cancelSetup,
				cancel: func() { close(entered); <-release; cancelFixture() }}
			finish := linked.linkColdLinkConstructorCancellation(ownerCtx)
			finishLink := func() { finishOnce.Do(finish) }
			finishStarted := false
			defer func() {
				resume()
				cancelOwner()
				finishLink()
				synctest.Wait()
				if finishStarted {
					<-finished
				}
				cancelFixture()
				cancelSetup()
			}()
			if finishFirst {
				finishLink()
				cancelOwner()
				synctest.Wait()
				if fixtureCtx.Err() != nil || setupCtx.Err() != nil {
					t.Fatal("completed builder left cancellation linked to live fixture owners")
				}
				select {
				case <-entered:
					t.Fatal("removed constructor link still ran its callback")
				default:
				}
				return
			}
			cancelOwner()
			select {
			case <-entered:
			case <-guardCtx.Done():
				t.Fatal("constructor cancellation did not enter the actual fixture callback")
			}
			finishStarted = true
			go func() { defer close(finished); finishLink() }()
			synctest.Wait()
			select {
			case <-finished:
				t.Fatal("constructor finish returned before its cancellation callback joined")
			default:
			}
			resume()
			<-finished
			if !errors.Is(fixtureCtx.Err(), context.Canceled) || !errors.Is(setupCtx.Err(), context.Canceled) {
				t.Fatal("bounded constructor owner did not cancel both actual fixture work contexts")
			}
		})
	}
	for _, registerFirst := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), sdk.DefaultDeviceLocalSettings().SendTimeout)
			defer cancel()
			scenario := newSdkBrowsingScenario(nil)
			scenario.coldLinkForTest, scenario.coldLinkConstructorCtxForTest = true, ctx
			joinEntered := make(chan struct{})
			scenario.fixture = &sdkBrowsingSetupFixture{t: t, cancelSetup: func() {}, cancel: func() { close(joinEntered) }}
			if !registerFirst {
				if err := scenario.stop(); err != nil {
					t.Fatal(err)
				}
				scenario.stateLock.Lock()
				_, registered := scenario.registerColdLinkConstructorWithLock()
				closed, started := scenario.coldLinkClosingForTest, scenario.lateStarted
				scenario.stateLock.Unlock()
				if registered || !closed || started {
					t.Fatal("stop-first order admitted a later constructor")
				}
				return
			}
			scenario.stateLock.Lock()
			ownedCtx, registered := scenario.registerColdLinkConstructorWithLock()
			scenario.stateLock.Unlock()
			if !registered || ownedCtx != ctx {
				t.Fatal("register-first order lost its exact bounded constructor context")
			}
			release, ownerDone, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			var releaseOnce sync.Once
			resume := func() { releaseOnce.Do(func() { close(release) }) }
			defer func() { resume(); synctest.Wait(); <-ownerDone; <-stopped }()
			go func() { defer close(scenario.lateDone); defer close(ownerDone); <-release }()
			go func() { defer close(stopped); result <- scenario.stop() }()
			select {
			case <-joinEntered:
			case <-ctx.Done():
				t.Fatal("actual stop did not reach its constructor join")
			}
			synctest.Wait()
			select {
			case <-stopped:
				t.Fatal("stop returned before its registered constructor receipt")
			default:
			}
			scenario.stateLock.Lock()
			_, registered = scenario.registerColdLinkConstructorWithLock()
			closed := scenario.coldLinkClosingForTest
			scenario.stateLock.Unlock()
			if registered || !closed {
				t.Fatal("joining stop left constructor admission open")
			}
			resume()
			<-ownerDone
			<-stopped
			if err := <-result; err != nil || !scenario.lateJoined {
				t.Fatal("stop did not join the explicitly released constructor: ", err)
			}
			if err := scenario.stop(); err != nil {
				t.Fatal("repeated stop changed its joined result: ", err)
			}
		})
	}
	// Successful postmeasurement seals the identical gate before deciding that
	// a healthy alternate started no constructor. Test both actual API orders.
	for _, registerFirst := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), sdk.DefaultDeviceLocalSettings().SendTimeout)
			defer cancel()
			scenario := newSdkBrowsingScenario(nil)
			scenario.coldLinkForTest, scenario.coldLinkConstructorCtxForTest = true, ctx
			witness := newNativeBrowsingColdLinkWitness(scenario, nil, false)
			witness.constructorCtx = ctx
			if registerFirst {
				scenario.stateLock.Lock()
				ownedCtx, registered := scenario.registerColdLinkConstructorWithLock()
				scenario.stateLock.Unlock()
				if !registered || ownedCtx != ctx {
					t.Fatal("postmeasure register-first order lost its exact constructor owner")
				}
				release, ownerDone, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
				result := make(chan error, 1)
				var releaseOnce sync.Once
				resume := func() { releaseOnce.Do(func() { close(release) }) }
				defer func() { resume(); synctest.Wait(); <-ownerDone; <-joined }()
				go func() { defer close(ownerDone); defer close(scenario.lateDone); <-release }()
				go func() { defer close(joined); result <- witness.joinLateConstructor() }()
				synctest.Wait()
				select {
				case <-joined:
					t.Fatal("postmeasure returned before its registered constructor receipt")
				default:
				}
				scenario.stateLock.Lock()
				closed := scenario.coldLinkClosingForTest
				scenario.stateLock.Unlock()
				if !closed {
					t.Fatal("postmeasure join did not seal constructor registration")
				}
				resume()
				<-ownerDone
				<-joined
				if err := <-result; err != nil {
					t.Fatal("postmeasure did not join its explicitly released constructor: ", err)
				}
			} else if err := witness.joinLateConstructor(); err != nil {
				t.Fatal(err)
			}
			scenario.stateLock.Lock()
			_, registered := scenario.registerColdLinkConstructorWithLock()
			closed, started := scenario.coldLinkClosingForTest, scenario.lateStarted
			scenario.stateLock.Unlock()
			if registered || !closed || started != registerFirst {
				t.Fatal("postmeasure completion admitted a constructor after its owner sample")
			}
		})
	}
}
