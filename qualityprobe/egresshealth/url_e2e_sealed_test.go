package egresshealth

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A post-quantum session that sealed, as a turn's path would report it: which
// of the turn's request hooks fired, and a seal without a request unless one
// was written.
type sealedE2eTestObserver struct {
	stateLock sync.Mutex
	started   int
	written   int
}

func (self *sealedE2eTestObserver) SealWait() time.Duration {
	return 0
}

func (self *sealedE2eTestObserver) RequestStarted() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.started += 1
}

func (self *sealedE2eTestObserver) RequestWritten() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.written += 1
}

func (self *sealedE2eTestObserver) Evidence(httpsPassed bool) *UrlProbeE2eEvidence {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &UrlProbeE2eEvidence{
		Pq: UrlProbePqUnmeasured, KeyExchange: "X25519MLKEM768", Window: UrlProbeWindowResizable,
		Sealed: self.written == 0 && !httpsPassed,
	}
}

// A leg that fails at its DNS lookup through the provider before any request
// is written, as net/http's transport fails a dial: unlike roundTripperFunc,
// which reports a written request first, it fires none of the request's
// trace.
type sealedE2eTestDnsFailure struct{}

func (sealedE2eTestDnsFailure) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &echoStageSyntheticError{stage: "dial_dns", cause: errors.New("synthetic dial_dns")}
}

// A turn whose IPv4 HTTPS leg failed at its DNS lookup through the provider
// writes no request, so the session's request hooks never fire. The seal the
// session reports without a request is evidence the ingest accepts, it
// survives the wire, and the outcome's pq_outcome stays unmeasured, so strict
// post-quantum selection's eight-hour window does not count the turn (owner,
// 2026-11-24).
func TestUrlProbeE2eSealWithoutARequestKeepsTheOutcomeUnmeasured(t *testing.T) {
	observer := &sealedE2eTestObserver{}
	result, err := Check(context.Background(), &http.Client{Transport: familyTestPath(sealedE2eTestDnsFailure{}, nil, nil, nil)}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, familyTestIpv4Catalog), Rand: rand.New(rand.NewPCG(3, 0)),
		E2e: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := result.UrlProbeEvidence
	if evidence == nil || result.OkCount != 0 || result.Total != 1 {
		t.Fatalf("a failed DNS lookup measured ok=%d/%d evidence %+v", result.OkCount, result.Total, evidence)
	}
	if evidence.FailureStage != "dial_dns" || evidence.RequestWritten {
		t.Fatalf("the leg failed at %q with a request written %t", evidence.FailureStage, evidence.RequestWritten)
	}
	if observer.started != 0 || observer.written != 0 {
		t.Fatalf("request hooks fired without a request: started %d written %d", observer.started, observer.written)
	}
	if evidence.E2e == nil || !evidence.E2e.Sealed || evidence.E2e.Pq != UrlProbePqUnmeasured {
		t.Fatalf("the turn's e2e evidence is %+v", evidence.E2e)
	}
	if err := evidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatalf("the ingest refuses a seal without a request: %v", err)
	}
	if outcome := evidence.PqOutcome(result.Total); outcome != UrlProbePqOutcomeUnmeasured {
		t.Fatalf("a seal without a request stores pq_outcome %d", outcome)
	}

	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	const field = `"e2e":{"pq":"unmeasured","key_exchange":"X25519MLKEM768","sealed_before_request":false,"window":"resizable","sealed":true}`
	if !strings.Contains(string(encoded), field) {
		t.Fatalf("e2e on the wire is %s", encoded)
	}
	var decoded UrlProbeEvidence
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.E2e == nil || *decoded.E2e != *evidence.E2e {
		t.Fatalf("decoded e2e %+v", decoded.E2e)
	}
	if err := decoded.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatal(err)
	}
}
