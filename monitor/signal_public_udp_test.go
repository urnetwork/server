package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type publicUdpTestSource struct {
	*syntheticSource
	observe func(context.Context, PublicUdpRequest) (PublicUdpObservation, error)
}

func (self *publicUdpTestSource) PublicUdp(ctx context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
	return self.observe(ctx, request)
}

func publicUdpTestSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "edge-synthetic", Roles: []string{"edge"}})
	settings.PublicUdp = PublicUdpSettings{
		Enabled: true, ExpectedTargets: 1,
		Targets: []PublicUdpTargetSettings{{
			Name: "synthetic-connect", Host: "edge-synthetic", Interface: "eth1", Service: "connect", Alias: "primary", Front: "connect", Carrier: "quic",
			Families: []string{"ipv4", "ipv6"}, IPv4Address: "192.0.2.20", IPv6Address: "2001:db8::20", Port: 443,
			ServerName: "connect.synthetic.example",
		}},
	}
	return settings
}

// A complete healthy attempt; a connect api front's also completed /hello.
func publicUdpTestHealthy(request PublicUdpRequest) PublicUdpObservation {
	protocol := ""
	if request.Front == "api" {
		protocol = "h3"
	}
	observation := PublicUdpObservation{
		AttemptId: request.AttemptId, RequestedTuple: publicUdpTuple(request), PeerTuple: publicUdpTuple(request),
		FreshSocket: true, HandshakeComplete: true, TlsVerified: true, NegotiatedProtocol: protocol,
		SentPackets: 2, SentBytes: 2400, ReceivedPackets: 2, ReceivedBytes: 2400,
	}
	if publicUdpCompletesApiRequest(request) {
		observation.ApplicationRequested = true
		observation.ApplicationStatus = http.StatusOK
		observation.ApplicationContentTypeJson = true
		observation.ApplicationBodyValid = true
		observation.ApplicationComplete = true
	}
	return observation
}

func TestPublicUdpCompleteConfiguredMatrixAndFreshAttempts(t *testing.T) {
	var stateLock sync.Mutex
	requests := []PublicUdpRequest{}
	source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
		stateLock.Lock()
		requests = append(requests, request)
		stateLock.Unlock()
		return publicUdpTestHealthy(request), nil
	}}
	settings := publicUdpTestSettings(source)
	alt := settings.PublicUdp.Targets[0]
	alt.Name, alt.Service, alt.Alias, alt.Front, alt.Carrier = "synthetic-api", "alt", "secondary", "api", "dns"
	alt.ServerName, alt.DnsTld, alt.Port = "api.synthetic.example", "codec.synthetic.example.", 8053
	settings.PublicUdp.Targets = append(settings.PublicUdp.Targets, alt)
	settings.PublicUdp.ExpectedTargets++
	for range 2 {
		alerts, err := NewPublicUdpSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Fatal("complete exact configured matrix was not healthy")
		}
	}
	if len(requests) != 8 {
		t.Fatal("a configured family/service/alias was omitted")
	}
	seen := map[string]bool{}
	for _, request := range requests {
		if len(request.AttemptId) != 32 || seen[request.AttemptId] || publicUdpTuple(request) == "" {
			t.Fatal("attempt correlation or exact pinned destination was reused or absent")
		}
		seen[request.AttemptId] = true
		if request.Front == "api" && (request.Carrier != "dns" || request.Port != 8053) {
			t.Fatal("explicit Alt direct UDP authority was rewritten as a Connect-private leak")
		}
	}
}

func TestPublicUdpIncompleteInventoryMakesNoTransportCall(t *testing.T) {
	for _, mutate := range []func(*PublicUdpSettings){
		func(settings *PublicUdpSettings) { settings.ExpectedTargets = 2 },
		func(settings *PublicUdpSettings) { settings.Targets[0].IPv6Address = "" },
		func(settings *PublicUdpSettings) { settings.Targets[0].Families = []string{"ipv4"} },
		func(settings *PublicUdpSettings) { settings.Targets[0].Families = []string{"ipv4", "ipv4"} },
		func(settings *PublicUdpSettings) {
			settings.Targets[0].ServerName = "https://private.synthetic.example"
		},
		func(settings *PublicUdpSettings) { settings.Targets[0].Host = "unenrolled-synthetic" },
		func(settings *PublicUdpSettings) { settings.Targets[0].Carrier = "dns" },
		func(settings *PublicUdpSettings) { settings.Invalid = true },
	} {
		var calls atomic.Int32
		source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
			calls.Add(1)
			return publicUdpTestHealthy(request), nil
		}}
		settings := publicUdpTestSettings(source)
		mutate(&settings.PublicUdp)
		alerts, err := NewPublicUdpSignal().Run(context.Background(), settings)
		if err != nil || calls.Load() != 0 {
			t.Fatal("incomplete inventory reached transport or returned an unbounded error")
		}
		requireAlertClass(t, alerts, "public-udp-inventory")
	}
}

func TestPublicUdpMissingSourceCapabilityNeverFallsBack(t *testing.T) {
	settings := publicUdpTestSettings(&syntheticSource{})
	alerts, err := NewPublicUdpSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatal("missing source capability did not remain unknown for both families")
	}
	for _, alert := range alerts {
		if alert.Class != "public-udp-observation" || !strings.Contains(alert.Observed, "source-unavailable") {
			t.Fatal("missing capability was guessed healthy or classified as remote failure")
		}
	}
}

func TestPublicUdpObservationAuthorityCannotBeForgedByPartialEvidence(t *testing.T) {
	request := PublicUdpRequest{AttemptId: "synthetic-attempt", Host: "edge-synthetic", Target: "synthetic", Family: "ipv4", Service: "alt", Front: "api", Carrier: "quic", Address: "192.0.2.20", Port: 443}
	for _, mutate := range []func(*PublicUdpObservation){
		func(observed *PublicUdpObservation) { observed.AttemptId = "previous-attempt" },
		func(observed *PublicUdpObservation) { observed.RequestedTuple = "192.0.2.21:443" },
		func(observed *PublicUdpObservation) { observed.PeerTuple = "192.0.2.20:8053" },
		func(observed *PublicUdpObservation) { observed.FreshSocket = false },
		func(observed *PublicUdpObservation) { observed.HandshakeComplete = false },
		func(observed *PublicUdpObservation) { observed.TlsVerified = false },
		func(observed *PublicUdpObservation) { observed.NegotiatedProtocol = "" },
		func(observed *PublicUdpObservation) { observed.ReceivedPackets = 0 },
		func(observed *PublicUdpObservation) { observed.SentBytes = 0 },
		func(observed *PublicUdpObservation) {
			observed.UnexpectedPackets = DefaultPublicUdpProbeSettings().MaxPacketCount
		},
		func(observed *PublicUdpObservation) {
			observed.SentBytes = DefaultPublicUdpProbeSettings().MaxByteCount + 1
		},
		func(observed *PublicUdpObservation) { observed.Failure = PublicUdpFailure("synthetic-private-error") },
	} {
		observed := publicUdpTestHealthy(request)
		mutate(&observed)
		findings := publicUdpFindings(request, observed, nil, DefaultPublicUdpProbeSettings())
		if len(findings) != 1 || findings[0].healthy || findings[0].class != "public-udp-observation" {
			t.Fatal("partial, stale or contradictory evidence claimed an authenticated exact return path")
		}
	}
}

func TestPublicUdpFailureDoesNotResolveOtherFamilyOrClaimNatCause(t *testing.T) {
	source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
		observed := publicUdpTestHealthy(request)
		if request.Family == "ipv6" {
			observed.HandshakeComplete, observed.TlsVerified = false, false
			observed.PeerTuple, observed.ReceivedPackets, observed.ReceivedBytes = "", 0, 0
			observed.Failure, observed.UnexpectedPackets = PublicUdpFailureTimeout, 3
		}
		return observed, nil
	}}
	settings := publicUdpTestSettings(source)
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (publicUdpProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	var broken finding
	for _, result := range findings {
		if result.class == "public-udp-path" && !result.healthy {
			broken = result
		}
	}
	if broken.target == "" || broken.sustain != 2 || broken.pageSustain != 0 || !strings.Contains(broken.frame, "ipv6") {
		t.Fatal("bounded handshake failure lost family/sustain authority")
	}
	for _, result := range findings {
		if result.healthy && result.target == broken.target && result.class == broken.class {
			t.Fatal("another healthy family could clear the failing path")
		}
	}
	if !strings.Contains(broken.evidence, "alone cannot identify an SNAT fault") {
		t.Fatal("mismatched datagrams lost their causal limitation")
	}
}

func TestPublicUdpUnknownRouteTlsBudgetAndRawErrorsStayPrivate(t *testing.T) {
	const sentinel = "synthetic-private-error-sentinel"
	for _, failure := range []PublicUdpFailure{PublicUdpFailureRoute, PublicUdpFailureTls, PublicUdpFailureBudget, PublicUdpFailureSocket, PublicUdpFailureConfiguration} {
		source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
			observed := publicUdpTestHealthy(request)
			observed.Failure = failure
			return observed, errors.New(sentinel + " " + request.ServerName + " " + request.AttemptId)
		}}
		settings := publicUdpTestSettings(source)
		alerts, err := NewPublicUdpSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 {
			t.Fatal("unknown transport authority was reduced to healthy or raw error")
		}
		for _, alert := range alerts {
			if alert.Class != "public-udp-observation" || alert.PageSustain != 0 {
				t.Fatal("unknown prerequisite became an outage or automatic page")
			}
			requireAlertOmits(t, alert, sentinel, "192.0.2.20", "2001:db8::20", "connect.synthetic.example")
			encoded, err := json.Marshal(alert)
			if err != nil || strings.Contains(string(encoded), sentinel) || strings.Contains(string(encoded), "connect.synthetic.example") || strings.Contains(string(encoded), "192.0.2.20") {
				t.Fatal("public JSON retained raw errors or private endpoint authority")
			}
		}
	}
}

func TestPublicUdpExcludedAndSharedPausedEndpointMakesNoCall(t *testing.T) {
	for _, shared := range []bool{false, true} {
		var calls atomic.Int32
		source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
			calls.Add(1)
			return publicUdpTestHealthy(request), nil
		}}
		settings := publicUdpTestSettings(source)
		settings.PublicUdp.Targets[0].Families = []string{"ipv4"}
		settings.PublicUdp.Targets[0].IPv6Address = ""
		if shared {
			settings.disabledHosts = []HostSettings{{Name: "paused-synthetic", scopeEndpoints: []string{"192.0.2.20"}}}
		} else {
			settings.ExcludedHosts = []string{"edge-synthetic"}
		}
		alerts, err := NewPublicUdpSignal().Run(context.Background(), settings)
		if err != nil || calls.Load() != 0 {
			t.Fatal("an excluded or shared paused endpoint reached transport")
		}
		requireAlertClass(t, alerts, "monitor-host-scope-partial")
	}
}

func TestPublicUdpDisabledEnrollmentMakesNoCallOrRecovery(t *testing.T) {
	var calls atomic.Int32
	source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
		calls.Add(1)
		return publicUdpTestHealthy(request), nil
	}}
	settings := publicUdpTestSettings(source)
	settings.PublicUdp.Enabled = false
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (publicUdpProbe{}).check(context.Background(), env)
	if err != nil || len(findings) != 0 || calls.Load() != 0 {
		t.Fatal("disabled enrollment contacted a path or established recovery")
	}
}

func TestPublicUdpCancellationJoinsSourcesWithoutPartialFindings(t *testing.T) {
	entered := make(chan struct{}, 2)
	var exited atomic.Int32
	source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(ctx context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
		entered <- struct{}{}
		<-ctx.Done()
		exited.Add(1)
		return publicUdpTestHealthy(request), ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		alerts, err := NewPublicUdpSignal().Run(ctx, publicUdpTestSettings(source))
		if len(alerts) != 0 {
			done <- errors.New("partial canceled findings were published")
		} else {
			done <- err
		}
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("source-entry barrier not reached")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || exited.Load() != 2 {
			t.Fatal("cancellation did not join both sources without partial observations")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded source cancellation did not join")
	}
}

func TestPublicUdpCloneRetainsIndependentFamilyDenominator(t *testing.T) {
	settings := publicUdpTestSettings(&syntheticSource{}).PublicUdp
	copy := clonePublicUdpSettings(settings)
	settings.Targets[0].Families[0] = "invalid"
	settings.Targets[0].IPv4Address = "192.0.2.99"
	if copy.Targets[0].Families[0] != "ipv4" || copy.Targets[0].IPv4Address != "192.0.2.20" {
		t.Fatal("later caller mutation changed the retained enrollment matrix")
	}
}

type publicUdpTestDeadlineContext struct {
	context.Context
	expired chan struct{}
}

func (self *publicUdpTestDeadlineContext) Done() <-chan struct{} { return self.expired }
func (self *publicUdpTestDeadlineContext) Err() error {
	select {
	case <-self.expired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestPublicUdpLatePositiveSourceCannotOutliveAttemptAuthority(t *testing.T) {
	var returned atomic.Int32
	source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(ctx context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
		// This explicit state transition occurs after source entry but before
		// its positive completion. No sleep or scheduler ordering is involved.
		close(ctx.(*publicUdpTestDeadlineContext).expired)
		returned.Add(1)
		return publicUdpTestHealthy(request), nil
	}}
	settings := publicUdpTestSettings(source)
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	probe := publicUdpProbe{attemptContext: func(parent context.Context) (context.Context, context.CancelFunc) {
		return &publicUdpTestDeadlineContext{Context: parent, expired: make(chan struct{})}, func() {}
	}}
	findings, err := probe.check(context.Background(), env)
	if err != nil || returned.Load() != 2 {
		t.Fatal("per-attempt expiration was conflated with outer cancellation or source completion was not exercised")
	}
	unknown := 0
	for _, finding := range findings {
		if finding.class == "public-udp-inventory" {
			continue
		}
		if finding.healthy || finding.class != "public-udp-observation" {
			t.Fatal("late positive source evidence established transport health")
		}
		unknown++
	}
	if unknown != 2 {
		t.Fatal("each expired attempt must retain its own unknown boundary")
	}
}

// One connect api row in both families, the pair LBALT adds: connect's
// lb_alt front serves the api names over h3 and forwards them to the api.
func publicUdpTestConnectApiSignalSettings(source SignalSource) SignalSettings {
	settings := publicUdpTestSettings(source)
	target := &settings.PublicUdp.Targets[0]
	target.Name, target.Front, target.ServerName = "synthetic-connect-api", "api", "api.synthetic.example"
	return settings
}

// A failed /hello after an exact authenticated handshake is its own finding:
// the application class, with the api's status, held for two evaluations.
// It is neither an unknown observation nor recovery of any class of the row.
func TestPublicUdpFailedHelloIsItsOwnApplicationFinding(t *testing.T) {
	source := &publicUdpTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
		observed := publicUdpTestHealthy(request)
		observed.ApplicationStatus, observed.ApplicationBodyValid, observed.ApplicationComplete = http.StatusServiceUnavailable, false, false
		observed.Failure = PublicUdpFailureApplication
		return observed, nil
	}}
	settings := publicUdpTestConnectApiSignalSettings(source)
	alerts, err := NewPublicUdpSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("a failed /hello in both families gave %d alerts: %v", len(alerts), err)
	}
	for _, alert := range alerts {
		if alert.Class != "public-udp-application" || alert.Sustain != 2 || alert.PageSustain != 0 {
			t.Fatalf("a failed /hello was reported as %s with sustain %d", alert.Class, alert.Sustain)
		}
		if !strings.Contains(alert.Observed, "result=application-failed application_status=503") || !strings.Contains(alert.Mechanism, "/hello") {
			t.Fatalf("the application finding lost its outcome: %s / %s", alert.Observed, alert.Mechanism)
		}
		requireAlertOmits(t, alert, "192.0.2.20", "2001:db8::20", "api.synthetic.example")
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (publicUdpProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range findings {
		if result.healthy && result.class != "public-udp-inventory" {
			t.Fatalf("a failed /hello resolved %s of its row", result.class)
		}
	}
}

// Only an exact authenticated handshake with a connect api front carries a
// /hello, so an application failure without one, or from any other front,
// is unknown rather than a failure of the api.
func TestPublicUdpApplicationFailureNeedsAnExactAuthenticatedHandshake(t *testing.T) {
	request := PublicUdpRequest{
		AttemptId: "synthetic-attempt", Host: "edge-synthetic", Target: "synthetic", Family: "ipv4", Service: "connect", Front: "api", Carrier: "quic",
		Address: "192.0.2.20", Port: 443, ServerName: "api.synthetic.example",
	}
	failed := func(request PublicUdpRequest) PublicUdpObservation {
		observed := publicUdpTestHealthy(request)
		observed.ApplicationRequested, observed.ApplicationStatus, observed.ApplicationComplete = true, 0, false
		observed.ApplicationContentTypeJson, observed.ApplicationBodyValid = false, false
		observed.Failure = PublicUdpFailureApplication
		return observed
	}
	if findings := publicUdpFindings(request, failed(request), errors.New("synthetic"), DefaultPublicUdpProbeSettings()); len(findings) != 1 ||
		findings[0].healthy || findings[0].class != "public-udp-application" || findings[0].sustain != 2 {
		t.Fatalf("an unanswered /hello after a verified handshake gave %+v", findings)
	}
	altApi := request
	altApi.Service = "alt"
	cases := []struct {
		name    string
		request PublicUdpRequest
		mutate  func(*PublicUdpObservation)
		reason  string
	}{
		{name: "no request", request: request, mutate: func(observed *PublicUdpObservation) { observed.ApplicationRequested = false }, reason: "application-outcome-unverified"},
		{name: "a completed request", request: request, mutate: func(observed *PublicUdpObservation) { observed.ApplicationComplete = true }, reason: "application-outcome-unverified"},
		{name: "an unverified certificate", request: request, mutate: func(observed *PublicUdpObservation) { observed.TlsVerified = false }, reason: "application-outcome-unverified"},
		{name: "an incomplete handshake", request: request, mutate: func(observed *PublicUdpObservation) { observed.HandshakeComplete = false }, reason: "application-outcome-unverified"},
		{name: "another peer", request: request, mutate: func(observed *PublicUdpObservation) { observed.PeerTuple = "192.0.2.21:443" }, reason: "application-outcome-unverified"},
		{name: "no h3", request: request, mutate: func(observed *PublicUdpObservation) { observed.NegotiatedProtocol = "" }, reason: "application-outcome-unverified"},
		{name: "nothing received", request: request, mutate: func(observed *PublicUdpObservation) { observed.ReceivedPackets, observed.ReceivedBytes = 0, 0 }, reason: "application-outcome-unverified"},
		{name: "a reused socket", request: request, mutate: func(observed *PublicUdpObservation) { observed.FreshSocket = false }, reason: "application-outcome-unverified"},
		{name: "an impossible status", request: request, mutate: func(observed *PublicUdpObservation) { observed.ApplicationStatus = 700 }, reason: "observation-authority-invalid"},
		{name: "alt's api front", request: altApi, mutate: func(*PublicUdpObservation) {}, reason: "observation-authority-invalid"},
	}
	for _, c := range cases {
		observed := failed(c.request)
		c.mutate(&observed)
		findings := publicUdpFindings(c.request, observed, nil, DefaultPublicUdpProbeSettings())
		if len(findings) != 1 || findings[0].healthy || findings[0].class != "public-udp-observation" || findings[0].observed != "verified=false reason="+c.reason {
			t.Errorf("%s: %+v", c.name, findings)
		}
	}
}

// A connect api row is healthy only when its /hello completed with 200 and
// the api's JSON shape, and then every class of the row resolves, its
// application class included. A handshake alone is unknown, never health.
func TestPublicUdpConnectApiHealthRequiresACompletedHello(t *testing.T) {
	request := PublicUdpRequest{
		AttemptId: "synthetic-attempt", Host: "edge-synthetic", Target: "synthetic", Family: "ipv6", Service: "connect", Front: "api", Carrier: "dns",
		Address: "2001:db8::20", Port: 53, ServerName: "api.synthetic.example", DnsTld: "codec.synthetic.example.",
	}
	findings := publicUdpFindings(request, publicUdpTestHealthy(request), nil, DefaultPublicUdpProbeSettings())
	classes := []string{}
	for _, result := range findings {
		if !result.healthy {
			t.Fatalf("a completed /hello gave %s %s", result.class, result.observed)
		}
		classes = append(classes, result.class)
	}
	if strings.Join(classes, ",") != "public-udp-path,public-udp-observation,public-udp-application" {
		t.Fatalf("a completed /hello resolved %v", classes)
	}
	for _, mutate := range []func(*PublicUdpObservation){
		func(observed *PublicUdpObservation) {
			observed.ApplicationRequested, observed.ApplicationStatus, observed.ApplicationComplete = false, 0, false
			observed.ApplicationContentTypeJson, observed.ApplicationBodyValid = false, false
		},
		func(observed *PublicUdpObservation) { observed.ApplicationComplete = false },
		func(observed *PublicUdpObservation) { observed.ApplicationStatus = http.StatusInternalServerError },
		func(observed *PublicUdpObservation) { observed.ApplicationStatus = http.StatusNoContent },
		func(observed *PublicUdpObservation) { observed.ApplicationContentTypeJson = false },
		func(observed *PublicUdpObservation) { observed.ApplicationBodyValid = false },
		func(observed *PublicUdpObservation) { observed.ApplicationRequested = false },
	} {
		observed := publicUdpTestHealthy(request)
		mutate(&observed)
		findings := publicUdpFindings(request, observed, nil, DefaultPublicUdpProbeSettings())
		if len(findings) != 1 || findings[0].healthy || findings[0].observed != "verified=false reason=application-outcome-unverified" {
			t.Fatalf("an incomplete /hello claimed health: %+v", findings)
		}
	}
}

// A front that ends at its handshake carries no application evidence, so a
// source reporting some for it is not to be believed.
func TestPublicUdpHandshakeOnlyFrontCarriesNoApplicationEvidence(t *testing.T) {
	for _, path := range []struct{ service, front string }{{service: "connect", front: "connect"}, {service: "alt", front: "connect"}, {service: "alt", front: "api"}} {
		request := PublicUdpRequest{
			AttemptId: "synthetic-attempt", Host: "edge-synthetic", Target: "synthetic", Family: "ipv4", Service: path.service, Front: path.front, Carrier: "quic",
			Address: "192.0.2.20", Port: 443, ServerName: "connect.synthetic.example",
		}
		if findings := publicUdpFindings(request, publicUdpTestHealthy(request), nil, DefaultPublicUdpProbeSettings()); len(findings) != 2 {
			t.Fatalf("%s/%s: a verified handshake gave %d findings", path.service, path.front, len(findings))
		}
		for _, mutate := range []func(*PublicUdpObservation){
			func(observed *PublicUdpObservation) { observed.ApplicationRequested = true },
			func(observed *PublicUdpObservation) { observed.ApplicationStatus = http.StatusOK },
			func(observed *PublicUdpObservation) { observed.ApplicationComplete = true },
		} {
			observed := publicUdpTestHealthy(request)
			mutate(&observed)
			findings := publicUdpFindings(request, observed, nil, DefaultPublicUdpProbeSettings())
			if len(findings) != 1 || findings[0].healthy || findings[0].observed != "verified=false reason=observation-authority-invalid" {
				t.Fatalf("%s/%s: application evidence from a handshake-only front gave %+v", path.service, path.front, findings)
			}
		}
	}
}

// A connect api row's texts describe its /hello, and no row's mechanism
// still says only Alt's api front negotiates h3.
func TestPublicUdpFindingTextsDescribeTheHello(t *testing.T) {
	apiRequest := PublicUdpRequest{Host: "edge-synthetic", Target: "synthetic", Family: "ipv4", Service: "connect", Front: "api", Carrier: "quic"}
	handshakeRequest := apiRequest
	handshakeRequest.Front = "connect"
	apiFinding := publicUdpPathFinding(apiRequest, "public-udp-path")
	handshakeFinding := publicUdpPathFinding(handshakeRequest, "public-udp-path")
	if !strings.Contains(apiFinding.mechanism, "GET /hello") || !strings.Contains(apiFinding.baseline, "/hello") ||
		!strings.Contains(apiFinding.context, "/hello answered through the front") || strings.Contains(apiFinding.context, "API/application success") {
		t.Fatalf("a connect api row's texts do not describe its /hello: %+v", apiFinding)
	}
	if strings.Contains(handshakeFinding.mechanism, "/hello") || !strings.Contains(handshakeFinding.context, "transport path only") {
		t.Fatalf("a handshake-only row's texts claim a /hello: %+v", handshakeFinding)
	}
	for _, text := range []string{apiFinding.mechanism, handshakeFinding.mechanism} {
		if strings.Contains(text, "only an enrolled Alt API front") {
			t.Fatalf("a mechanism still says only Alt's api front negotiates h3: %s", text)
		}
	}
}

// SIGNALS.md §16.9 describes the /hello: the application class, the
// connect api pair, and no claim that no HTTP request is made.
func TestPublicUdpCatalogDescribesTheHello(t *testing.T) {
	catalog, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(catalog), "\n### 16.9 ")
	if start < 0 {
		t.Fatal("SIGNALS.md has no §16.9")
	}
	section := string(catalog[start+1:])
	if end := strings.Index(section, "\n## "); 0 <= end {
		section = section[:end]
	}
	if end := strings.Index(section[1:], "\n### "); 0 <= end {
		section = section[:end+1]
	}
	for _, required := range []string{"| `public-udp-application` |", "`GET /hello`", "`probe-settings`", "`application-outcome-unverified`", "PublicUdpProbeSettings"} {
		if !strings.Contains(section, required) {
			t.Errorf("§16.9 does not say %q", required)
		}
	}
	for _, stale := range []string{"only Alt's API front offers", "No HTTP request", "(`connect` or Alt `api`)"} {
		if strings.Contains(section, stale) {
			t.Errorf("§16.9 still says %q", stale)
		}
	}
}
