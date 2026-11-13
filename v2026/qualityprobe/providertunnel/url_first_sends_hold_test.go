// A URL turn that measures post-quantum opens its tunnel with a capability
// observation whose clients hold their first sends for the end-to-end
// session's seal, up to the turn's seal wait (fleetprobe
// UrlProbePostQuantumSettings.SealWait, 3 s). A provider without the
// end-to-end responder releases them only when the hold ends. These tests
// stand in for the provider path with firstSendsHoldPath, which holds the
// tunnel's sends as that session does, and check that the URL phase making
// the first sends is timed from its own budget plus the hold
// (providerFirstSendsHold), and every other phase exactly as before.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The session's establish hold as the tunnel's sends meet it. The first send
// starts the hold, and a send waits until the hold ends, then goes: at the
// seal for a provider whose responder seals sealAfter into the hold, at the
// hold's end for one without the responder (sealAfter zero). A zero hold
// holds nothing, as an ordinary turn's tunnel. Safe for concurrent use.
type firstSendsHoldPath struct {
	hold      time.Duration
	sealAfter time.Duration

	stateLock sync.Mutex
	holdStart time.Time
}

// When a send made now goes to the provider.
func (self *firstSendsHoldPath) release() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := time.Now()
	if self.holdStart.IsZero() {
		self.holdStart = now
	}
	end := self.holdStart.Add(self.hold)
	if 0 < self.sealAfter && self.sealAfter < self.hold {
		end = self.holdStart.Add(self.sealAfter)
	}
	if end.Before(now) {
		return now
	}
	return end
}

// The capability observation fleetprobe opens a post-quantum turn's tunnel
// with (url_e2e.go), at the default seal wait.
func postQuantumTurnObservation() *CapabilityObservation {
	return &CapabilityObservation{
		Recorder:                connect.NewPeerCapabilityRecorder(8),
		EstablishHold:           3 * time.Second,
		KeepTunnelOnReplacement: true,
	}
}

// A tunnel's resolver over path: a query answers dnsLatency after its send
// goes, a connect completes tcpLatency after its send goes and then returns
// connected. firstSends is the hold Open gives the tunnel (nil for none).
// The address policy admits the documentation addresses the tests use.
func newFirstSendsHoldResolver(path *firstSendsHoldPath, firstSends *providerFirstSendsHold, dnsLatency time.Duration, tcpLatency time.Duration, connected error) *providerUrlResolver {
	return &providerUrlResolver{
		query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
			if waitUrlPhase(ctx, time.Until(path.release().Add(dnsLatency))) != nil {
				return nil, false
			}
			return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, true
		},
		dial: func(ctx context.Context, _, _ string, _ []netip.Addr) (net.Conn, error) {
			if err := waitUrlPhase(ctx, time.Until(path.release().Add(tcpLatency))); err != nil {
				return nil, err
			}
			return nil, connected
		},
		publicAddress: func(netip.Addr, int) bool { return true },
		phases:        defaultProviderUrlPhaseSettings(),
		firstSends:    firstSends,
	}
}

// One URL request's dial of address through resolver, as the HTTPS leg's
// DialTLSContext makes it: the stage it reads as, how long it took, and its
// error.
func dialFirstSendsHoldResolver(ctx context.Context, resolver *providerUrlResolver, address string) (string, time.Duration, error) {
	start := time.Now()
	ctx, trace := traceProviderHttpDial(ctx, "tcp", address)
	_, err := resolver.dialContext(ctx, "tcp", address, trace)
	return trace.finish(err), time.Since(start), err
}

// The root cause of the 2026-11-13 Quality pool drop. On a turn that measures
// post-quantum, a provider without the end-to-end responder gets the turn's
// first sends, the DNS query among them, only when the 3 s hold ends, and here
// resolves 2.5 s after the query goes: 5.5 s into the DNS phase. That phase
// makes the tunnel's first sends, so its 5 s budget carries the hold and the
// answer is in time. Timed from the phase alone, the request failed at
// dial_dns 5 s in, for the measurement's hold rather than the provider.
func TestPostQuantumTurnDnsPhaseCarriesTheSealHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ctx = urlPhaseTestContext(t, ctx, "phase.example")
		connected := errors.New("connected")
		resolver := newFirstSendsHoldResolver(
			&firstSendsHoldPath{hold: 3 * time.Second},
			newProviderFirstSendsHold(postQuantumTurnObservation()),
			2500*time.Millisecond, 0, connected,
		)
		stage, elapsed, err := dialFirstSendsHoldResolver(ctx, resolver, "phase.example:443")
		if !errors.Is(err, connected) || elapsed != 5500*time.Millisecond {
			t.Fatalf("the answer 2.5 s after the hold was not in time: elapsed=%s stage=%s err=%v", elapsed, stage, err)
		}
	})
}

// An ordinary turn's tunnel holds nothing, and its DNS phase is exactly 5 s:
// an answer 5.5 s after the query, or none, fails at dial_dns 5 s in, as a
// timeout.
func TestOrdinaryTurnDnsPhaseStaysFiveSeconds(t *testing.T) {
	for _, dnsLatency := range []time.Duration{5500 * time.Millisecond, time.Hour} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			ctx = urlPhaseTestContext(t, ctx, "phase.example")
			connected := errors.New("connected")
			resolver := newFirstSendsHoldResolver(&firstSendsHoldPath{}, newProviderFirstSendsHold(nil), dnsLatency, 0, connected)
			stage, elapsed, err := dialFirstSendsHoldResolver(ctx, resolver, "phase.example:443")
			var timeout interface{ Timeout() bool }
			if err == nil || errors.Is(err, connected) || stage != "dial_dns" || elapsed != 5*time.Second || !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("dns latency %s: elapsed=%s stage=%s err=%v", dnsLatency, elapsed, stage, err)
			}
		})
	}
}

// A provider with the responder seals 200 ms into the hold, which releases
// the first sends, and answers 2.5 s after its query goes: 2.7 s into the
// phase. Its turn reads exactly as one whose phase is timed without the hold,
// the timing of every turn before the hold was counted.
func TestPostQuantumTurnWithAnEarlySealIsUnchanged(t *testing.T) {
	for _, firstSends := range []*providerFirstSendsHold{newProviderFirstSendsHold(postQuantumTurnObservation()), nil} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			ctx = urlPhaseTestContext(t, ctx, "phase.example")
			connected := errors.New("connected")
			resolver := newFirstSendsHoldResolver(
				&firstSendsHoldPath{hold: 3 * time.Second, sealAfter: 200 * time.Millisecond},
				firstSends, 2500*time.Millisecond, 0, connected,
			)
			stage, elapsed, err := dialFirstSendsHoldResolver(ctx, resolver, "phase.example:443")
			if !errors.Is(err, connected) || elapsed != 2700*time.Millisecond {
				t.Fatalf("hold given %t: elapsed=%s stage=%s err=%v", firstSends != nil, elapsed, stage, err)
			}
		})
	}
}

// The hold goes to the phase that makes the tunnel's first sends, once. The
// turn's later phases -- its HTTP/3 and IPv6 legs, a redirect hop -- send
// unheld and keep exactly the 5 s budget: after the first resolution, a
// second that is never answered fails at dial_dns 5 s in.
func TestPostQuantumTurnSealHoldIsGivenOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ctx = urlPhaseTestContext(t, ctx, "phase.example")
		connected := errors.New("connected")
		resolver := newFirstSendsHoldResolver(
			&firstSendsHoldPath{hold: 3 * time.Second},
			newProviderFirstSendsHold(postQuantumTurnObservation()),
			time.Second, 0, connected,
		)
		stage, elapsed, err := dialFirstSendsHoldResolver(ctx, resolver, "phase.example:443")
		if !errors.Is(err, connected) || elapsed != 4*time.Second {
			t.Fatalf("first resolution: elapsed=%s stage=%s err=%v", elapsed, stage, err)
		}
		resolver.query = func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
			<-ctx.Done()
			return nil, false
		}
		stage, elapsed, err = dialFirstSendsHoldResolver(ctx, resolver, "phase.example:443")
		if err == nil || stage != "dial_dns" || elapsed != 5*time.Second {
			t.Fatalf("second resolution: elapsed=%s stage=%s err=%v", elapsed, stage, err)
		}
	})
}

// A literal host makes no query, so its TCP connect is the tunnel's first
// send and waits out the hold. A provider without the responder, connecting
// 1 s after the hold, is in time 4 s into the connect's 3 s budget plus the
// hold; timed from the connect alone, it failed 3 s in. An ordinary turn's
// literal connect keeps exactly 3 s. The grant refuses a documentation
// literal as nonpublic, so the request's scope is set directly, and the
// resolver's address policy admits it.
func TestPostQuantumTurnLiteralHostConnectCarriesTheSealHold(t *testing.T) {
	for _, c := range []struct {
		path       *firstSendsHoldPath
		firstSends *providerFirstSendsHold
		tcpLatency time.Duration
		connected  bool
		elapsed    time.Duration
	}{
		{path: &firstSendsHoldPath{hold: 3 * time.Second}, firstSends: newProviderFirstSendsHold(postQuantumTurnObservation()), tcpLatency: time.Second, connected: true, elapsed: 4 * time.Second},
		{path: &firstSendsHoldPath{}, tcpLatency: 4 * time.Second, elapsed: 3 * time.Second},
	} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			ctx = context.WithValue(ctx, providerUrlProbeKey{}, providerUrlProbeTarget{host: "192.0.2.10", ipVersion: 4})
			connected := errors.New("connected")
			resolver := newFirstSendsHoldResolver(c.path, c.firstSends, 0, c.tcpLatency, connected)
			resolver.query = func(context.Context, string, string) ([]netip.Addr, bool) {
				t.Error("a literal host was queried")
				return nil, false
			}
			stage, elapsed, err := dialFirstSendsHoldResolver(ctx, resolver, "192.0.2.10:443")
			if errors.Is(err, connected) != c.connected || elapsed != c.elapsed {
				t.Fatalf("hold given %t: elapsed=%s stage=%s err=%v", c.firstSends != nil, elapsed, stage, err)
			}
		})
	}
}

// Open gives a tunnel the hold its capability observation sets, which goes to
// one claim: a post-quantum turn's tunnel. The capability probe's observation
// holds nothing (EstablishHold zero), and neither does a tunnel without one.
func TestFirstSendsHoldIsTheObservationsOnce(t *testing.T) {
	firstSends := newProviderFirstSendsHold(postQuantumTurnObservation())
	if first, second := firstSends.claim(), firstSends.claim(); first != 3*time.Second || second != 0 {
		t.Fatalf("claims gave %s then %s", first, second)
	}
	capabilityProbe := &CapabilityObservation{Recorder: connect.NewPeerCapabilityRecorder(8)}
	for _, observation := range []*CapabilityObservation{capabilityProbe, nil} {
		if firstSends := newProviderFirstSendsHold(observation); firstSends != nil || firstSends.claim() != 0 {
			t.Fatalf("a tunnel that holds no sends got a hold: %+v", observation)
		}
	}
}

// A post-quantum turn's e2e observer whose session recorded nothing of the
// provider, as one without the responder leaves a single load.
type unrecordedUrlProbeE2e struct {
	sealWait time.Duration
}

// Implements egresshealth.UrlProbeE2eObserver.
func (self *unrecordedUrlProbeE2e) SealWait() time.Duration {
	return self.sealWait
}

// Implements egresshealth.UrlProbeE2eObserver.
func (self *unrecordedUrlProbeE2e) RequestStarted() {}

// Implements egresshealth.UrlProbeE2eObserver.
func (self *unrecordedUrlProbeE2e) RequestWritten() {}

// Implements egresshealth.UrlProbeE2eObserver.
func (self *unrecordedUrlProbeE2e) Evidence(bool) *egresshealth.UrlProbeE2eEvidence {
	return nil
}

// The composed turn: egresshealth's URL load through the tunnel's HTTP
// client, with real TLS and result classification, on a turn that measures
// post-quantum, for a provider without the responder that resolves 2.5 s after
// the hold lets its query go. The load passes 5.5 s in. Timed from the DNS
// phase alone it failed at dial_dns 5 s in, which is what took such providers
// out of the Quality, Speed and Online pools.
func TestPostQuantumUrlLoadWithoutAResponderPasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := &urlPhaseFixture{
			dns: 2500 * time.Millisecond, body: "synthetic real content",
			path:       &firstSendsHoldPath{hold: 3 * time.Second},
			firstSends: newProviderFirstSendsHold(postQuantumTurnObservation()),
		}
		client := fixture.client(t)
		start := time.Now()
		result, err := egresshealth.Check(context.Background(), client, egresshealth.Options{
			UrlProbe: true, LoadAttempts: 1, Concurrency: 1, PerRequestTimeout: 10 * time.Second,
			ColdStartTimeout: time.Minute,
			Destinations:     []egresshealth.Destination{{Name: "synthetic", Class: egresshealth.ClassSite, Url: "https://phase.example/", Expect: egresshealth.ExpectBody}},
			E2e:              &unrecordedUrlProbeE2e{sealWait: 3 * time.Second},
		})
		elapsed := time.Since(start)
		if err != nil || result == nil || len(result.Checks) != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if check := result.Checks[0]; result.OkCount != 1 || check.FailureStage != "" || elapsed != 5500*time.Millisecond {
			t.Fatalf("ok=%d stage=%s elapsed=%s error=%s", result.OkCount, check.FailureStage, elapsed, check.Err)
		}
		if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
			t.Fatal(err)
		}
	})
}
