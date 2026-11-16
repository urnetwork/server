package fleetprobe

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// A classical request followed by a PQ rekey cannot become positive when
// evidence is collected; the request and final observations name different epochs.
func TestUrlProbeE2eObservationDoesNotCombineRequestAndRekey(t *testing.T) {
	observation := connect.PeerCapabilityObservation{
		E2eAttempted: true, E2eNegotiated: true, E2eSealed: true,
		KeyExchange: "X25519", AckObserved: true,
		Session: connect.PeerCapabilitySession{ClientId: connect.NewId(), EpochId: connect.NewId(), KeyExchange: "X25519"},
	}
	observer := &urlProbeE2eObservation{
		read:         func() (connect.PeerCapabilityObservation, bool) { return observation, true },
		settings:     DefaultUrlProbePostQuantumSettings(),
		unmeasurable: func() bool { return false },
		now:          time.Now,
	}
	observer.RequestStarted()
	observer.RequestWritten()
	observation.KeyExchange, observation.PostQuantum = "X25519MLKEM768", true
	observation.Session.EpochId = connect.NewId()
	observation.Session.KeyExchange, observation.Session.PostQuantum = "X25519MLKEM768", true
	if evidence := observer.Evidence(true); evidence.Pq == egresshealth.UrlProbePqPositive {
		t.Fatalf("a classical request borrowed the later PQ seal: %+v", evidence)
	}
}

// A PQ request needs its own live epoch at completion. A second PQ epoch,
// a demoted session and a sticky seal already stale at the write cannot prove it.
func TestUrlProbeE2eObservationRejectsChangedOrStaleSession(t *testing.T) {
	for _, transition := range []string{"rekey", "classical rekey", "demoted after write", "demoted before write", "unchanged"} {
		observation := connect.PeerCapabilityObservation{
			E2eAttempted: true, E2eNegotiated: true, E2eSealed: true,
			KeyExchange: "X25519MLKEM768", PostQuantum: true,
			Session: connect.PeerCapabilitySession{ClientId: connect.NewId(), EpochId: connect.NewId(), KeyExchange: "X25519MLKEM768", PostQuantum: true},
		}
		observer := &urlProbeE2eObservation{
			read:     func() (connect.PeerCapabilityObservation, bool) { return observation, true },
			settings: DefaultUrlProbePostQuantumSettings(), unmeasurable: func() bool { return false }, now: time.Now,
		}
		if transition == "demoted before write" {
			observation.Session = connect.PeerCapabilitySession{}
		}
		observer.RequestStarted()
		observer.RequestWritten()
		switch transition {
		case "rekey":
			observation.Session.EpochId = connect.NewId()
		case "classical rekey":
			observation.Session.EpochId = connect.NewId()
			observation.Session.KeyExchange, observation.Session.PostQuantum = "X25519", false
			observation.KeyExchange, observation.PostQuantum = "X25519", false
		case "demoted after write":
			observation.Session = connect.PeerCapabilitySession{}
		}
		evidence := observer.Evidence(true)
		wantPositive := transition == "unchanged"
		if (evidence.Pq == egresshealth.UrlProbePqPositive) != wantPositive || evidence.SealedBeforeRequest != wantPositive {
			t.Errorf("%s: %+v", transition, evidence)
		}
		if transition == "classical rekey" && evidence.Pq != egresshealth.UrlProbePqNegative {
			t.Errorf("a later classical session lost its negative evidence: %+v", evidence)
		}
	}
}

// WroteRequest runs after the bytes have gone. A rekey between acquiring the
// HTTP connection and that callback cannot supply proof for the earlier write.
func TestUrlProbeE2eObservationRejectsRekeyDuringRequestWrite(t *testing.T) {
	for _, group := range []string{"X25519", "X25519MLKEM768", ""} {
		clientId := connect.NewId()
		observation := connect.PeerCapabilityObservation{
			E2eAttempted: true, E2eNegotiated: group != "", E2eSealed: group != "",
			KeyExchange: group, PostQuantum: group == "X25519MLKEM768",
		}
		if group != "" {
			observation.Session = connect.PeerCapabilitySession{ClientId: clientId, EpochId: connect.NewId(), KeyExchange: group, PostQuantum: observation.PostQuantum}
		}
		observer := &urlProbeE2eObservation{
			read:     func() (connect.PeerCapabilityObservation, bool) { return observation, true },
			settings: DefaultUrlProbePostQuantumSettings(), unmeasurable: func() bool { return false }, now: time.Now,
		}
		observer.RequestStarted()
		observation.Session = connect.PeerCapabilitySession{ClientId: clientId, EpochId: connect.NewId(), KeyExchange: "X25519MLKEM768", PostQuantum: true}
		observation.E2eNegotiated, observation.E2eSealed = true, true
		observation.KeyExchange, observation.PostQuantum = "X25519MLKEM768", true
		observer.RequestWritten()
		if evidence := observer.Evidence(true); evidence.Pq == egresshealth.UrlProbePqPositive || evidence.SealedBeforeRequest {
			t.Errorf("request started under %q borrowed a seal observed after its write: %+v", group, evidence)
		}
	}
}

// A turn's e2e evidence from what its recorder holds, case by case
// (FP2-PROTOCOL.md §12.2): positive only for a post-quantum group sealed
// before the IPv4 HTTPS leg's request with that leg passed, on one tunnel and
// client; negative for a classical group, a post-quantum session unsealed
// past its grace, or a ClientHello unanswered for a whole establishment
// window while the provider acknowledged; unmeasured otherwise. Each result
// is evidence the ingest accepts.
func TestReadUrlProbeE2eCases(t *testing.T) {
	settings := DefaultUrlProbePostQuantumSettings()
	attempt := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	early := attempt.Add(time.Second)
	afterGrace := attempt.Add(settings.UnsealedGrace)
	afterWindow := attempt.Add(settings.NoResponderWindow)
	pqSealed := connect.PeerCapabilityObservation{E2eAttempted: true, E2eFirstAttemptTime: attempt, E2eNegotiated: true, E2eSealed: true,
		KeyExchange: "X25519MLKEM768", PostQuantum: true, AckObserved: true, WindowAdvertised: true}
	pqUnsealed := pqSealed
	pqUnsealed.E2eSealed = false
	classical := connect.PeerCapabilityObservation{E2eAttempted: true, E2eFirstAttemptTime: attempt, E2eNegotiated: true, E2eSealed: true,
		KeyExchange: "X25519", AckObserved: true}
	unanswered := connect.PeerCapabilityObservation{E2eAttempted: true, E2eFirstAttemptTime: attempt, AckObserved: true}
	unacknowledged := connect.PeerCapabilityObservation{E2eAttempted: true, E2eFirstAttemptTime: attempt}
	for _, c := range []struct {
		name            string
		observation     connect.PeerCapabilityObservation
		now             time.Time
		sealedAtRequest bool
		httpsPassed     bool
		unmeasurable    bool
		pq              string
		window          string
		sealed          bool
	}{
		{name: "post-quantum sealed before the request", observation: pqSealed, now: early, sealedAtRequest: true, httpsPassed: true,
			pq: egresshealth.UrlProbePqPositive, window: egresshealth.UrlProbeWindowResizable, sealed: true},
		{name: "post-quantum sealed after the request", observation: pqSealed, now: early, httpsPassed: true,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowResizable},
		{name: "post-quantum sealed, IPv4 HTTPS leg failed", observation: pqSealed, now: early, sealedAtRequest: true,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowResizable, sealed: true},
		{name: "post-quantum sealed on a replaced client", observation: pqSealed, now: early, sealedAtRequest: true, httpsPassed: true, unmeasurable: true,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowResizable, sealed: true},
		{name: "post-quantum unsealed within its grace", observation: pqUnsealed, now: early, httpsPassed: true,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowResizable},
		{name: "post-quantum unsealed past its grace", observation: pqUnsealed, now: afterGrace, httpsPassed: true,
			pq: egresshealth.UrlProbePqNegative, window: egresshealth.UrlProbeWindowResizable},
		{name: "post-quantum unsealed past its grace on a replaced client", observation: pqUnsealed, now: afterGrace, httpsPassed: true, unmeasurable: true,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowResizable},
		{name: "classical sealed", observation: classical, now: early, sealedAtRequest: true, httpsPassed: true,
			pq: egresshealth.UrlProbePqNegative, window: egresshealth.UrlProbeWindowFixed, sealed: true},
		{name: "classical on a replaced client", observation: classical, now: early, unmeasurable: true,
			pq: egresshealth.UrlProbePqNegative, window: egresshealth.UrlProbeWindowFixed},
		{name: "unanswered within the window", observation: unanswered, now: afterGrace, httpsPassed: true,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowFixed},
		{name: "unanswered for the whole window", observation: unanswered, now: afterWindow, httpsPassed: true,
			pq: egresshealth.UrlProbePqNegative, window: egresshealth.UrlProbeWindowFixed},
		{name: "unanswered for the whole window on a replaced client", observation: unanswered, now: afterWindow, unmeasurable: true,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowFixed},
		{name: "unanswered and unacknowledged", observation: unacknowledged, now: afterWindow,
			pq: egresshealth.UrlProbePqUnmeasured, window: egresshealth.UrlProbeWindowNotTested},
	} {
		e2e := readUrlProbeE2e(c.observation, true, c.now, c.sealedAtRequest, c.httpsPassed, c.unmeasurable, settings)
		if e2e == nil {
			t.Fatalf("%s: no evidence", c.name)
		}
		if e2e.Pq != c.pq || e2e.Window != c.window || e2e.SealedBeforeRequest != c.sealed {
			t.Errorf("%s: read %+v, want pq %s window %s sealed %t", c.name, e2e, c.pq, c.window, c.sealed)
		}
		if negotiated := c.observation.E2eNegotiated; (e2e.KeyExchange != "") != negotiated {
			t.Errorf("%s: key exchange %q for a session negotiated %t", c.name, e2e.KeyExchange, negotiated)
		}
		// the ingest accepts it, as a passed outcome when the leg passed
		evidence := urlE2eTestEvidence(t)
		evidence.E2e = e2e
		ok := 0
		if c.httpsPassed {
			ok = 1
		} else {
			evidence.ContentClassification, evidence.PerformanceClassification, evidence.FailureStage = "captcha", "", "response_content"
		}
		if err := evidence.ValidateOutcome(ok, 1, false); err != nil {
			t.Errorf("%s: the ingest refuses the turn's evidence: %v", c.name, err)
		}
	}
	if e2e := readUrlProbeE2e(connect.PeerCapabilityObservation{}, false, early, false, true, false, settings); e2e != nil {
		t.Fatalf("a recorder that holds nothing reads %+v", e2e)
	}
}

// A minimal passed evidence document of the ingest's contract, to carry e2e.
func urlE2eTestEvidence(t *testing.T) *egresshealth.UrlProbeEvidence {
	t.Helper()
	at := time.Date(2026, 11, 12, 12, 0, 30, 0, time.UTC)
	destination := egresshealth.Destination{Name: "synthetic-document", Class: egresshealth.ClassSite, Url: "https://document.example/"}
	evidence := &egresshealth.UrlProbeEvidence{
		PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
		Destination: destination, MeasuredAt: at, ContentMatcherVersion: 1,
		Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}},
		StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
		RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
		ContentClassification: "content", PerformanceClassification: "insufficient_sample",
	}
	// a wire round trip, as the ingest receives it
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var decoded egresshealth.UrlProbeEvidence
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return &decoded
}

// The observation keeps whether the session had sealed at the IPv4 HTTPS
// leg's first request write, whatever it reads later, and reads the
// recorder again, with the path's state, at the run's end.
func TestUrlProbeE2eObservationReadsTheSealAtTheFirstRequest(t *testing.T) {
	observation := connect.PeerCapabilityObservation{E2eAttempted: true, E2eFirstAttemptTime: time.Now(), AckObserved: true}
	unmeasurable := false
	e2e := &urlProbeE2eObservation{
		read: func() (connect.PeerCapabilityObservation, bool) {
			return observation, true
		},
		settings:     DefaultUrlProbePostQuantumSettings(),
		unmeasurable: func() bool { return unmeasurable },
		now:          time.Now,
	}
	if e2e.SealWait() != 3*time.Second {
		t.Fatalf("seal wait %s", e2e.SealWait())
	}
	// the request goes before the seal; a later write does not move it
	e2e.RequestStarted()
	e2e.RequestWritten()
	observation.E2eNegotiated, observation.E2eSealed, observation.KeyExchange, observation.PostQuantum = true, true, "X25519MLKEM768", true
	observation.Session = connect.PeerCapabilitySession{ClientId: connect.NewId(), EpochId: connect.NewId(), KeyExchange: "X25519MLKEM768", PostQuantum: true}
	e2e.RequestStarted()
	e2e.RequestWritten()
	if evidence := e2e.Evidence(true); evidence.Pq != egresshealth.UrlProbePqUnmeasured || evidence.SealedBeforeRequest {
		t.Fatalf("a session sealed after the request read %+v", evidence)
	}

	sealed := &urlProbeE2eObservation{
		read: func() (connect.PeerCapabilityObservation, bool) {
			return observation, true
		},
		settings:     DefaultUrlProbePostQuantumSettings(),
		unmeasurable: func() bool { return unmeasurable },
		now:          time.Now,
	}
	sealed.RequestStarted()
	sealed.RequestWritten()
	if evidence := sealed.Evidence(true); evidence.Pq != egresshealth.UrlProbePqPositive || !evidence.SealedBeforeRequest {
		t.Fatalf("a session sealed before the request read %+v", evidence)
	}
	unmeasurable = true
	if evidence := sealed.Evidence(true); evidence.Pq != egresshealth.UrlProbePqUnmeasured {
		t.Fatalf("a re-created tunnel's session read %+v", evidence)
	}
}

// Only a URL turn the claim marks for post-quantum opens its tunnel with the
// turn's observation; an unmarked turn, and a full probe whatever its claim,
// open theirs as before, and the pass's configuration is not changed.
func TestPostQuantumTurnReachesTheTunnel(t *testing.T) {
	original := openProviderTunnel
	t.Cleanup(func() { openProviderTunnel = original })
	stop := errors.New("synthetic stop at provider tunnel boundary")
	var captured []*providertunnel.CapabilityObservation
	openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
		captured = append(captured, cfg.CapabilityObservation)
		return nil, stop
	}
	due := []ingest.DueProvider{
		{ClientId: connect.NewId().String(), PostQuantum: true},
		{ClientId: connect.NewId().String()},
	}
	cfg := providertunnel.Config{}
	for _, urlProbe := range []bool{true, false} {
		captured = nil
		full := NewFullProber(FullOptions{UrlProbe: urlProbe, TunnelConfig: cfg, ProbeTimeout: time.Minute, Pool: func() *egresshealth.Pool { return smallPool() }})
		for _, provider := range ProvidersFromDue(due) {
			if err := full.ProbeOne(t.Context(), provider); !errors.Is(err, stop) {
				t.Fatalf("boundary failed: %v", err)
			}
		}
		if len(captured) != 2 || captured[1] != nil {
			t.Fatalf("url probe %t: observations %v", urlProbe, captured)
		}
		if !urlProbe {
			if captured[0] != nil {
				t.Fatal("a full probe opened a post-quantum session")
			}
			continue
		}
		observation := captured[0]
		if observation == nil || observation.Recorder == nil || observation.EstablishHold != DefaultUrlProbePostQuantumSettings().SealWait ||
			!observation.KeepTunnelOnReplacement || observation.RedialMin != 0 {
			t.Fatalf("a post-quantum turn's observation %+v", observation)
		}
	}
	if cfg.CapabilityObservation != nil {
		t.Fatal("a post-quantum turn mutated the pass configuration")
	}
}

// The health run of a post-quantum turn carries the turn's observation; any
// other probe's carries none.
func TestPostQuantumTurnReachesItsLoads(t *testing.T) {
	pool := smallPool()
	options := FullOptions{UrlProbe: true, ProbeTimeout: time.Minute, Pool: func() *egresshealth.Pool { return pool }}
	e2e := &urlProbeE2eObservation{settings: DefaultUrlProbePostQuantumSettings()}
	opts, err := fullHealthOptions(options, &probeState{pool: pool, e2e: e2e}, egresshealth.Place{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.E2e != e2e {
		t.Fatalf("a post-quantum turn's loads observe %v", opts.E2e)
	}
	opts, err = fullHealthOptions(options, &probeState{pool: pool}, egresshealth.Place{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.E2e != nil {
		t.Fatalf("an unmarked turn's loads observe %v", opts.E2e)
	}
}

// A post-quantum turn's tunnels apply the URL turn's observation: the turn's
// recorder, the seal wait as connect's establish hold, the key fetcher, and
// a tunnel kept on a replaced client; never the capability probe's re-dial
// pace.
func TestUrlProbeE2eCapabilityObservation(t *testing.T) {
	recorder := connect.NewPeerCapabilityRecorder(urlProbeE2eMaxObservedPeerCount)
	settings := DefaultUrlProbePostQuantumSettings()
	fetched := connect.Id{}
	fetcher := func(peerId connect.Id) func(context.Context) ([]byte, error) {
		fetched = peerId
		return nil
	}
	observation := urlProbeE2eCapabilityObservation(recorder, settings, fetcher)
	if observation.Recorder != recorder || observation.EstablishHold != settings.SealWait || !observation.KeepTunnelOnReplacement || observation.RedialMin != 0 {
		t.Fatalf("observation %+v", observation)
	}
	peerId := connect.NewId()
	observation.NewPeerClientPublicKeyFetcher(peerId)
	if fetched != peerId {
		t.Fatal("the turn's key fetcher was not passed through")
	}
}
