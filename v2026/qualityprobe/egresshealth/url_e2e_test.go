package egresshealth

import (
	"net/http"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"
)

// A post-quantum turn's observer as the turn drives it: the leg that was
// writing when it read the seal, and the IPv4 HTTPS outcome it was given.
type e2eTestObserver struct {
	sealWait time.Duration
	evidence *UrlProbeE2eEvidence

	stateLock    sync.Mutex
	currentLeg   string
	startedLegs  []string
	writtenLegs  []string
	httpsPassed  []bool
	evidenceRead int
}

func (self *e2eTestObserver) SealWait() time.Duration {
	return self.sealWait
}

// Records the leg before its request writes.
func (self *e2eTestObserver) RequestStarted() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.startedLegs = append(self.startedLegs, self.currentLeg)
}

func (self *e2eTestObserver) RequestWritten() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writtenLegs = append(self.writtenLegs, self.currentLeg)
}

func (self *e2eTestObserver) Evidence(httpsPassed bool) *UrlProbeE2eEvidence {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.httpsPassed = append(self.httpsPassed, httpsPassed)
	self.evidenceRead++
	return self.evidence
}

// A leg that names itself to the observer, then writes its request as
// net/http does, telling the request's trace, and is answered by inner.
type e2eTestLeg struct {
	observer *e2eTestObserver
	leg      string
	inner    http.RoundTripper
}

// Implements http.RoundTripper.
func (self *e2eTestLeg) RoundTrip(request *http.Request) (*http.Response, error) {
	func() {
		self.observer.stateLock.Lock()
		defer self.observer.stateLock.Unlock()
		self.observer.currentLeg = self.leg
	}()
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil {
		if trace.GotConn != nil {
			trace.GotConn(httptrace.GotConnInfo{})
		}
		if trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
	}
	return self.inner.RoundTrip(request)
}

// A leg of the observer's turn named leg, answered by inner.
func (self *e2eTestObserver) writing(leg string, inner http.RoundTripper) http.RoundTripper {
	return &e2eTestLeg{observer: self, leg: leg, inner: inner}
}

// A dual-stack post-quantum turn reads its session once, when its IPv4 HTTPS
// leg writes its request (never on the HTTP/3 or IPv6 legs), and once its
// legs end, with whether that leg passed; its evidence carries what the
// observer read. A failed IPv4 HTTPS leg is passed on as failed.
func TestUrlProbePostQuantumTurnReadsItsSessionAtTheIpv4HttpsRequest(t *testing.T) {
	positive := &UrlProbeE2eEvidence{Pq: UrlProbePqPositive, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: UrlProbeWindowResizable}
	for _, c := range []struct {
		name        string
		https4      http.RoundTripper
		httpsPassed bool
		written     []string
	}{
		{name: "every leg passes", https4: familyTestContent(), httpsPassed: true, written: []string{"https4"}},
		{name: "the IPv4 HTTPS leg fails", https4: familyTestFails("dial_tcp"), httpsPassed: false, written: []string{"https4"}},
	} {
		evidence := *positive
		if !c.httpsPassed {
			evidence = UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured, Window: UrlProbeWindowFixed}
		}
		observer := &e2eTestObserver{sealWait: 3 * time.Second, evidence: &evidence}
		path := familyTestPath(observer.writing("https4", c.https4), observer.writing("https6", familyTestContent()),
			observer.writing("h3_4", familyTestContent()), observer.writing("h3_6", familyTestContent()))
		result, err := Check(t.Context(), &http.Client{Transport: path}, Options{
			UrlProbe: true, Destinations: h3TestDestinations(t, familyTestDualCatalog),
			ProviderIpFamily: IpFamilyDualstack, E2e: observer,
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if result.UrlProbeEvidence == nil || result.UrlProbeEvidence.E2e == nil || *result.UrlProbeEvidence.E2e != evidence {
			t.Fatalf("%s: evidence carries %+v", c.name, result.UrlProbeEvidence)
		}
		if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		observer.stateLock.Lock()
		if len(observer.startedLegs) != len(c.written) || (0 < len(c.written) && observer.startedLegs[0] != c.written[0]) {
			t.Errorf("%s: the session was read before the writes of %v, want %v", c.name, observer.startedLegs, c.written)
		}
		if len(observer.writtenLegs) != len(c.written) || (0 < len(c.written) && observer.writtenLegs[0] != c.written[0]) {
			t.Errorf("%s: the session was read at the writes of %v, want %v", c.name, observer.writtenLegs, c.written)
		}
		if observer.evidenceRead != 1 || observer.httpsPassed[0] != c.httpsPassed {
			t.Errorf("%s: evidence read %d times, IPv4 HTTPS passed %v, want once with %t", c.name, observer.evidenceRead, observer.httpsPassed, c.httpsPassed)
		}
		observer.stateLock.Unlock()
	}

	// a turn without an observer opens and reports no session
	result, err := Check(t.Context(), &http.Client{Transport: familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), familyTestContent())}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, familyTestDualCatalog), ProviderIpFamily: IpFamilyDualstack,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.UrlProbeEvidence.E2e != nil {
		t.Fatalf("a turn without a session reported %+v", result.UrlProbeEvidence.E2e)
	}
}

// The seal wait is counted in the run's budget, once per tunnel generation
// the run may open, for a URL turn that measures post-quantum only.
func TestUrlProbeRunBudgetCountsTheSealWait(t *testing.T) {
	plain := Options{UrlProbe: true}
	observer := &e2eTestObserver{sealWait: 3 * time.Second}
	postQuantum := Options{UrlProbe: true, E2e: observer}
	generations := 1 + plain.tunnelRecreateAttempts()
	if got, want := postQuantum.RunBudget(1)-plain.RunBudget(1), time.Duration(generations)*observer.sealWait; got != want {
		t.Fatalf("the seal wait added %s to the budget, want %s", got, want)
	}
	legacy := Options{E2e: observer}
	if legacy.RunBudget(1) != (Options{}).RunBudget(1) {
		t.Fatal("a probe that is not a URL turn counted a seal wait")
	}
}
