package perfvar

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Body custody requires completed HTML and a live original CSS owner. Public
// availability is validated separately; affinity may use one historical carrier.
func TestPrivateMidSessionArmRequiresCompletedPagesAndOriginalBody(t *testing.T) {
	var html [nativeBrowsingPageCount]bool
	for page := 0; page < privateSdkMidPage; page++ {
		html[page] = true
	}
	valid := func(pages [nativeBrowsingPageCount]bool, providers uint8, clientAttempts, serverAttempts int, read, nat int64, live, clientTuple, serverTuple bool) error {
		return privateSdkMidArmPremise(pages, providers, clientAttempts, serverAttempts, read, nat, live, clientTuple, serverTuple)
	}
	for _, providers := range []uint8{1, 2, 3} {
		if err := valid(html, providers, 1, 1, nativeBrowsingHeldPrefixBytes, 1, true, true, true); err != nil {
			t.Fatal(err)
		}
	}
	missing := html
	missing[9] = false
	if valid(missing, 3, 1, 1, nativeBrowsingHeldPrefixBytes, 1, true, true, true) == nil {
		t.Fatal("nine completed pages armed the eleventh navigation")
	}
	for _, providers := range []uint8{0, 129} {
		if valid(html, providers, 1, 1, nativeBrowsingHeldPrefixBytes, 1, true, true, true) == nil {
			t.Fatal("missing or foreign historical body provider accepted")
		}
	}
	for _, attempts := range []int{0, 2} {
		if valid(html, 3, attempts, 1, nativeBrowsingHeldPrefixBytes, 1, true, true, true) == nil || valid(html, 3, 1, attempts, nativeBrowsingHeldPrefixBytes, 1, true, true, true) == nil {
			t.Fatal("missing or replayed original CSS accepted")
		}
	}
	for _, read := range []int64{0, nativeBrowsingHeldPrefixBytes - 1, nativeBrowsingHeldPrefixBytes + 1} {
		if valid(html, 3, 1, 1, read, 1, true, true, true) == nil {
			t.Fatal("exact held prefix was not required")
		}
	}
	if valid(html, 3, 1, 1, nativeBrowsingHeldPrefixBytes, 0, true, true, true) == nil || valid(html, 3, 1, 1, nativeBrowsingHeldPrefixBytes, 1, false, true, true) == nil || valid(html, 3, 1, 1, nativeBrowsingHeldPrefixBytes, 1, true, false, true) == nil || valid(html, 3, 1, 1, nativeBrowsingHeldPrefixBytes, 1, true, true, false) == nil {
		t.Fatal("missing live original socket/body evidence accepted")
	}
}

// The actual arming boundary accepts one historical carrier with two public
// alternatives. The exact old body predicate makes the first assertion fail.
func TestPrivateMidSessionReadyAlternativesDoNotRequireHistoricalDiversity(t *testing.T) {
	newArm := func() *privateSdkMidSession {
		mid := newPrivateSdkMidSession(true, true)
		for page := 0; page < privateSdkMidPage; page++ {
			mid.html[page] = true
		}
		mid.providers = 1
		mid.cssClientAttempts, mid.cssServerAttempts = 1, 1
		mid.cssRead.Store(nativeBrowsingHeldPrefixBytes)
		mid.cssNatRead, mid.cssServerLive = 1, true
		mid.cssClientTuple, mid.cssSource = "synthetic-app|synthetic-origin", "original-owned-socket"
		return mid
	}
	ready := privateSdkBrowsingReadinessSnapshot{Quality: 1, Speed: 1, Exits: 2, LocalDistinct: 2, MinSatisfied: true, Generation: 1, Locations: 2, DistinctOwned: 2, OwnedMask: 33}
	mid := newArm()
	defer mid.close()
	if err := mid.armWithReadiness(ready, true, 50*1024-1024, 50*1024); err != nil {
		t.Fatalf("MID_SESSION_OLD_BODY_DIVERSITY_REJECTED_READY_ALTERNATIVES: %v", err)
	}
	if !mid.armAttempted || !mid.armGenerationStable || !mid.armPremiseAccepted || !mid.armed.Load() || mid.before != ready || mid.armProviderMask != 1 || mid.armHtmlCompleted != privateSdkMidPage || mid.armCssBytes != nativeBrowsingHeldPrefixBytes || !mid.armCssServerLive || mid.before.Proven != 0 {
		t.Fatal("public alternatives replaced actual one-carrier/body evidence or required proven warmup")
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*privateSdkBrowsingReadinessSnapshot, *bool)
	}{
		{name: "one-ready-exit", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) {
			*v = privateSdkBrowsingReadinessSnapshot{Quality: 1, Exits: 1, LocalDistinct: 1, MinSatisfied: true, Generation: 1, Locations: 1, DistinctOwned: 1, OwnedMask: 1}
		}},
		{name: "one-healthy-of-two", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.Quality = 0 }},
		{name: "minimum-unsatisfied", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.MinSatisfied = false }},
		{name: "missing-public-view", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { *v = privateSdkBrowsingReadinessSnapshot{} }},
		{name: "missing-local-identity", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.MissingLocalIdentities++ }},
		{name: "missing-owned-identity", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.MissingIdentities++ }},
		{name: "duplicate-local-identity", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.LocalDistinct-- }},
		{name: "duplicate-owned-identity", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.DistinctOwned-- }},
		{name: "foreign-destination", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.Unowned++ }},
		{name: "missing-owned-mask", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.OwnedMask = 0 }},
		{name: "foreign-owned-bit", mutate: func(v *privateSdkBrowsingReadinessSnapshot, _ *bool) { v.OwnedMask = 129 }},
		{name: "mixed-generation", mutate: func(_ *privateSdkBrowsingReadinessSnapshot, stable *bool) { *stable = false }},
	} {
		bad := newArm()
		view, stable := ready, true
		testCase.mutate(&view, &stable)
		err := bad.armWithReadiness(view, stable, 50*1024-1024, 50*1024)
		armed := bad.armed.Load()
		bad.close()
		if err == nil || armed || bad.armPremiseAccepted {
			t.Fatalf("invalid alternatives armed the fault: %s", testCase.name)
		}
		if !bad.armAttempted || bad.before != view || bad.armGenerationStable != stable || bad.armProviderMask != 1 || bad.armHtmlCompleted != privateSdkMidPage || bad.armCssBytes != nativeBrowsingHeldPrefixBytes || !bad.armCssServerLive {
			t.Fatalf("failed arming erased actual attempted evidence: %s", testCase.name)
		}
	}
	if !privateSdkMidRefusalRecovery(0, 1, [6]uint64{0: 1}) {
		t.Fatal("actual different-provider response after scoped refusal was rejected")
	}
	for _, testCase := range []struct {
		name              string
		blocked, response int
		refusals          [6]uint64
	}{
		{name: "same-blocked-exit", blocked: 0, response: 0, refusals: [6]uint64{0: 1}},
		{name: "no-refusal", blocked: 0, response: 1},
		{name: "unknown-blocked", blocked: -1, response: 1, refusals: [6]uint64{0: 1}},
		{name: "unknown-response", blocked: 0, response: -1, refusals: [6]uint64{0: 1}},
		{name: "foreign-response", blocked: 0, response: 6, refusals: [6]uint64{0: 1}},
		{name: "refused-sibling", blocked: 0, response: 1, refusals: [6]uint64{0: 1, 1: 1}},
	} {
		if privateSdkMidRefusalRecovery(testCase.blocked, testCase.response, testCase.refusals) {
			t.Fatalf("public availability replaced actual scoped recovery: %s", testCase.name)
		}
	}
}

// Concurrent first dials bind one real ordinal. Refusal never widens to a
// healthy sibling, an established socket, or any other destination.
func TestPrivateMidSessionFaultBindsOneRealDial(t *testing.T) {
	for _, fault := range []bool{false, true} {
		s := newPrivateSdkMidSession(true, fault)
		defer s.close()
		if s.rejectDial(1, "203.0.113.14:443") {
			t.Fatal("unarmed fault")
		}
		s.tap = time.Now()
		s.armed.Store(true)
		for _, address := range []string{"203.0.113.10:443", "203.0.113.14:8443", "198.51.100.2:443"} {
			if s.rejectDial(1, address) {
				t.Fatal("unrelated destination refused")
			}
		}
		if s.blocked.Load() != -1 {
			t.Fatal("unrelated traffic bound the owner")
		}
		start := make(chan struct{})
		var workers sync.WaitGroup
		results := make(chan int, 6)
		for ordinal := 0; ordinal < 6; ordinal++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				if s.rejectDial(ordinal, "203.0.113.14:443") {
					results <- ordinal
				}
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		owner := int(s.blocked.Load())
		if owner < 0 || owner >= 6 {
			t.Fatal("no actual ordinal was bound")
		}
		count := 0
		for rejected := range results {
			count++
			if rejected != owner {
				t.Fatal("two providers were refused")
			}
		}
		if fault && count != 1 || !fault && count != 0 {
			t.Fatalf("wrong real-dial refusal count %d", count)
		}
		select {
		case <-s.cssRelease:
		default:
			t.Fatal("first actual dial did not release the old body in both twins")
		}
		if s.rejectDial((owner+1)%6, "203.0.113.14:443") {
			t.Fatal("healthy sibling lost isolation")
		}
		s.armed.Store(false)
		if s.rejectDial(owner, "203.0.113.14:443") {
			t.Fatal("fault survived its first-byte cutoff")
		}
	}
	// The contender passes its cheap check, then the actual first-byte/owner
	// cutoff completes before it can enter the locked admission decision.
	for _, ownerStop := range []bool{false, true} {
		s := newPrivateSdkMidSession(true, true)
		defer s.close()
		s.tap = time.Now()
		s.armed.Store(true)
		entered, continueDecision := make(chan struct{}), make(chan struct{})
		s.beforeDialDecisionForTest = func() { close(entered); <-continueDecision }
		result := make(chan bool, 1)
		go func() { result <- s.rejectDial(2, "203.0.113.14:443") }()
		<-entered
		if ownerStop {
			s.close()
		} else {
			s.requestTrace("https://origin4.test/page/10").GotFirstResponseByte()
		}
		close(continueDecision)
		if <-result || s.blocked.Load() != -1 || s.refusals[2] != 0 {
			t.Fatal("post-cutoff contender bound or refused a new original")
		}
	}
}

// The original writer is positively blocked after a flushed prefix. Release
// and child cancellation join it without closing the shared parent or sibling.
func TestPrivateMidSessionOriginalBodyReleaseAndCancellation(t *testing.T) {
	for _, disposition := range []string{"release", "request-cancel", "owner-stop"} {
		synctest.Test(t, func(t *testing.T) {
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			sibling, siblingCancel := context.WithCancel(parent)
			defer siblingCancel()
			s := newPrivateSdkMidSession(true, true)
			defer s.close()
			response := httptest.NewRecorder()
			body := nativeBrowsingAsset(9, 0)
			writer := &privateSdkMidWriter{ResponseWriter: response, ctx: ctx, owner: s}
			type result struct {
				n   int
				err error
			}
			done := make(chan result, 1)
			go func() { n, err := writer.Write(body); done <- result{n: n, err: err} }()
			synctest.Wait()
			if !response.Flushed || !bytes.Equal(response.Body.Bytes(), body[:nativeBrowsingHeldPrefixBytes]) {
				t.Fatal("original flushed prefix missing")
			}
			select {
			case <-done:
				t.Fatal("suffix completed before the barrier")
			default:
			}
			switch disposition {
			case "request-cancel":
				cancel()
			case "owner-stop":
				s.close()
			default:
				s.release()
			}
			synctest.Wait()
			got := <-done
			if parent.Err() != nil || sibling.Err() != nil {
				t.Fatal("child cleanup canceled a healthy shared lifetime")
			}
			if disposition != "release" {
				if disposition == "owner-stop" && ctx.Err() != nil {
					t.Fatal("test accidentally canceled the independent server request")
				}
				if got.n != nativeBrowsingHeldPrefixBytes || !errors.Is(got.err, context.Canceled) || response.Body.Len() != nativeBrowsingHeldPrefixBytes {
					t.Fatal("canceled writer invented a suffix")
				}
			} else if got.n != len(body) || got.err != nil || !bytes.Equal(response.Body.Bytes(), body) {
				t.Fatal("released original body changed bytes or replayed")
			}
		})
	}
	// A rejected duplicate cannot overwrite the first active handler's tuple.
	s := newPrivateSdkMidSession(true, false)
	defer s.close()
	s.fixture = &privateSdkBrowsingFixture{origins: privateSdkBrowsingOrigins{providerBySource: map[string]int{"original": 1, "duplicate": 2}}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { releaseFirst(); <-done }()
	handler := s.wrapHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }))
	first := httptest.NewRequest(http.MethodGet, "https://origin1.test/asset/9/0", nil)
	first.RemoteAddr = "original"
	go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), first) }()
	<-entered
	second := httptest.NewRequest(http.MethodGet, "https://origin1.test/asset/9/0", nil)
	second.RemoteAddr = "duplicate"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, second)
	s.stateLock.Lock()
	correct := s.cssSource == "original" && s.cssProvider == 1 && s.cssServerLive && s.cssServerAttempts == 2 && s.firstErr != nil
	s.stateLock.Unlock()
	if !correct || response.Code != http.StatusConflict {
		t.Fatal("duplicate replaced the original CSS owner")
	}
	releaseFirst()
	<-done
	s.stateLock.Lock()
	correct = s.cssSource == "original" && s.cssProvider == 1 && !s.cssServerLive
	s.stateLock.Unlock()
	if !correct {
		t.Fatal("original handler did not clear its own live receipt")
	}
}

// A reader may return its last bytes together with EOF. Only returned bytes
// count; the prefix receipt cannot imply the remaining body was consumed.
type privateMidReturnedReader struct {
	body []byte
	done bool
}

func (self *privateMidReturnedReader) Read(p []byte) (int, error) {
	if self.done {
		return 0, io.EOF
	}
	self.done = true
	return copy(p, self.body), io.EOF
}
func TestPrivateMidSessionCountsReturnedPrefix(t *testing.T) {
	s := newPrivateSdkMidSession(true, false)
	defer s.close()
	body := bytes.Repeat([]byte{7}, nativeBrowsingHeldPrefixBytes)
	reader := &privateSdkMidReader{reader: &privateMidReturnedReader{body: body}, owner: s}
	output := make([]byte, len(body))
	n, err := reader.Read(output)
	if n != len(body) || err != io.EOF || s.cssRead.Load() != int64(n) || !bytes.Equal(body, output) {
		t.Fatal("returned prefix accounting changed")
	}
	select {
	case <-s.cssPrefix:
	default:
		t.Fatal("positive returned prefix did not publish")
	}
	if n, _ := reader.Read(output); n != 0 || s.cssRead.Load() != int64(len(body)) {
		t.Fatal("EOF double counted bytes")
	}
}

// Both cold twins keep every asset and byte extent; only page9's actual link
// and page10 HTML host change. The warm-origin twin changes no URL or body.
func TestPrivateMidSessionPreservesHundredPageWorkload(t *testing.T) {
	for _, cold := range []bool{false, true} {
		s := newPrivateSdkMidSession(cold, false)
		defer s.close()
		for page := 0; page < nativeBrowsingPageCount; page++ {
			body := s.rewriteHtml(nativeBrowsingURL, nativeBrowsingHTML, page)
			assets, next, err := nativeBrowsingReferences(body)
			if err != nil {
				t.Fatal(err)
			}
			if len(body) != nativeBrowsingHTMLBytes || next != s.rewriteUrl(nativeBrowsingURL, page+1, -1) {
				t.Fatal("original link or HTML extent changed")
			}
			if (!cold || page != 9) && !bytes.Equal(body, nativeBrowsingHTML(page)) {
				t.Fatal("unselected document changed")
			}
			planned := len(body)
			for asset, target := range assets {
				if target != nativeBrowsingURL(page, asset) || s.rewriteUrl(nativeBrowsingURL, page, asset) != target {
					t.Fatal("original asset origin changed")
				}
				planned += len(nativeBrowsingAsset(page, asset))
			}
			if planned != nativeBrowsingPageBytes(page) || planned < 50*1024 || 300*1024 < planned {
				t.Fatal("50..300KiB original extent changed")
			}
			target := s.rewriteUrl(nativeBrowsingURL, page, -1)
			if cold && page == 10 {
				if target != "http://origin4.test/page/10" {
					t.Fatal("cold destination not exact")
				}
			} else if target != nativeBrowsingURL(page, -1) {
				t.Fatal("unselected navigation changed")
			}
		}
	}
}

// This packet is metadata-only test input; it is never sent to a network.
func privateMidColdSynPacket() []byte {
	p := make([]byte, 40)
	p[0] = 0x45
	p[9] = 6
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:16], []byte{192, 0, 2, 1})
	copy(p[16:20], []byte{203, 0, 113, 14})
	binary.BigEndian.PutUint16(p[20:22], 41001)
	binary.BigEndian.PutUint16(p[22:24], 443)
	binary.BigEndian.PutUint32(p[24:28], 123)
	p[32] = 5 << 4
	p[33] = 2
	return p
}

// Selected-call records own scalar copies. A mixed aggregate remains unknown
// even when the original contains a selected SYN beside another packet.
func TestPrivateMidSessionNativeBoundaryKeepsExactScalarScope(t *testing.T) {
	packet := privateMidColdSynPacket()
	if !privateSdkMidColdSyn(packet) {
		t.Fatal("selected SYN missing")
	}
	for _, mutate := range []func([]byte){func(p []byte) { p[19] = 13 }, func(p []byte) { p[33] = 16 }, func(p []byte) { p[9] = 17 }, func(p []byte) { p[0] = 0x65 }} {
		bad := append([]byte(nil), packet...)
		mutate(bad)
		if privateSdkMidColdSyn(bad) {
			t.Fatal("unselected packet matched")
		}
	}
	var trace privateSdkMidBridgeTrace
	trace.arm()
	trace.readReturned(4, [][]byte{packet, make([]byte, 8)})
	packet[19] = 99
	packet[12] = 99
	trace.send(4, false, -1)
	trace.send(4, true, 1)
	trace.readBegin(5)
	trace.readReturned(5, [][]byte{make([]byte, 8)})
	trace.freeze()
	if trace.count != 2 || !trace.rows[0].ColdTupleKnown || trace.rows[0].ColdSourceIp != [4]byte{192, 0, 2, 1} || trace.rows[0].ColdSourcePort != 41001 || trace.rows[0].ColdSequence != 123 || trace.rows[0].ColdSyn != 1 || trace.rows[0].Other != 1 || trace.rows[0].Disposition != "mixed_packet_identity_unknown" || !trace.rows[0].NextReadEntered {
		t.Fatal("exact scalar boundary lost or mixed result invented a packet disposition")
	}
	if !privateSdkMidTraceComplete(trace.rows[:trace.count], 0, 0) {
		t.Fatal("complete selected boundary was not recognized")
	}
	missing := append([]privateSdkMidBridgeRow(nil), trace.rows[:trace.count]...)
	missing[0].NextReadEntered = false
	if privateSdkMidTraceComplete(missing, 0, 0) || privateSdkMidTraceComplete(nil, 0, 0) || privateSdkMidTraceComplete(trace.rows[:trace.count], 1, 0) || privateSdkMidTraceComplete(trace.rows[:trace.count], 0, 1) {
		t.Fatal("missing boundary or event loss accepted as complete")
	}
	missing[0] = trace.rows[0]
	missing[0].ColdTupleKnown = false
	if privateSdkMidTraceComplete(missing, 0, 0) {
		t.Fatal("multiple selected tuples treated as one exact SYN")
	}
	if privateSdkMidAggregateDisposition(2, 0) != "all_refused" || privateSdkMidAggregateDisposition(2, 2) != "all_accepted" || privateSdkMidAggregateDisposition(2, 3) != "invalid" {
		t.Fatal("aggregate boundary classification changed")
	}
}

// Real pre-cutoff overflow and try-lock loss remain visible. A later flood
// cannot erase or expand the frozen selected trace.
func TestPrivateMidSessionNativeTraceFreezeAndLoss(t *testing.T) {
	var trace privateSdkMidBridgeTrace
	trace.arm()
	packet := privateMidColdSynPacket()
	trace.stateLock.Lock()
	trace.readReturned(1, [][]byte{packet})
	trace.stateLock.Unlock()
	if trace.dropped.Load() != 1 {
		t.Fatal("contended hot append loss was hidden")
	}
	for i := 1; i <= 33; i++ {
		trace.readReturned(uint64(i), [][]byte{packet})
	}
	if trace.count != 32 || trace.overflow != 1 {
		t.Fatal("bounded selected ring overflow was hidden")
	}
	trace.freeze()
	before := trace.rows
	for i := 34; i < 200; i++ {
		trace.readReturned(uint64(i), [][]byte{packet})
		trace.send(uint64(i), true, 1)
		trace.readBegin(uint64(i))
	}
	if trace.rows != before || trace.overflow != 1 || trace.dropped.Load() != 1 {
		t.Fatal("post-cutoff flood changed the selected proof")
	}
}

// Existing observers still receive each callback. New CSS custody is a
// separate exact connection receipt, not a replacement for the old observer.
func TestPrivateMidSessionRequestTraceChainsObservers(t *testing.T) {
	for _, cold := range []bool{false, true} {
		s := newPrivateSdkMidSession(cold, false)
		defer s.close()
		serverTls, clientTls, err := privateSdkBrowsingTlsConfigs()
		if err != nil {
			t.Fatal(err)
		}
		var got, first, parentGot, parentFirst int
		settings := nativeBrowsingRunSettings{originServerTlsForTest: serverTls, originClientTlsForTest: clientTls, pressure: &nativeBrowsingPressureObservation{}, maxLivePages: 4, requestTraceForTest: func(context.Context, string) *httptrace.ClientTrace {
			return &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { got++ }, GotFirstResponseByte: func() { first++ }}
		}}
		s.configure(&settings)
		if err := settings.validate(); err != nil {
			t.Fatal(err)
		}
		for _, mutate := range []func(*nativeBrowsingRunSettings){func(v *nativeBrowsingRunSettings) { v.originServerTlsForTest = nil; v.originClientTlsForTest = nil }, func(v *nativeBrowsingRunSettings) { v.originServerTlsForTest = nil }, func(v *nativeBrowsingRunSettings) { v.originClientTlsForTest = nil }, func(v *nativeBrowsingRunSettings) { v.coldLinkForTest = true }, func(v *nativeBrowsingRunSettings) { v.nextNavigationOverlap = false }, func(v *nativeBrowsingRunSettings) { v.maxLivePages = 3 }, func(v *nativeBrowsingRunSettings) { v.innerHttp2ForTest = true }} {
			bad := settings
			mutate(&bad)
			if bad.validate() == nil {
				t.Fatal("mid-session accepted an incompatible TLS/overlap/inner-protocol plan")
			}
		}
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { parentGot++ }, GotFirstResponseByte: func() { parentFirst++ }})
		observer := settings.requestTraceForTest(ctx, "https://origin2.test/page/10")
		trace := httptrace.ContextClientTrace(httptrace.WithClientTrace(ctx, observer))
		trace.GotConn(httptrace.GotConnInfo{Conn: left, Reused: !cold})
		trace.GotFirstResponseByte()
		if got != 1 || first != 1 || parentGot != 1 || parentFirst != 1 || s.navAttempts[10] != 1 || s.navReuse[10] != !cold || s.firstByte.IsZero() {
			t.Fatal("composed trace lost a callback")
		}
		select {
		case <-s.cssRelease:
		default:
			t.Fatal("first-byte cleanup did not release original body")
		}
	}
}
