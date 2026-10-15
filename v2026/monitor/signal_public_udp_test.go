package monitor

import (
	"context"
	"encoding/json"
	"errors"
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

func publicUdpTestHealthy(request PublicUdpRequest) PublicUdpObservation {
	protocol := ""
	if request.Front == "api" {
		protocol = "h3"
	}
	return PublicUdpObservation{
		AttemptId: request.AttemptId, RequestedTuple: publicUdpTuple(request), PeerTuple: publicUdpTuple(request),
		FreshSocket: true, HandshakeComplete: true, TlsVerified: true, NegotiatedProtocol: protocol,
		SentPackets: 2, SentBytes: 2400, ReceivedPackets: 2, ReceivedBytes: 2400,
	}
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
		func(observed *PublicUdpObservation) { observed.UnexpectedPackets = publicUdpMaxPackets },
		func(observed *PublicUdpObservation) { observed.SentBytes = publicUdpMaxBytes + 1 },
		func(observed *PublicUdpObservation) { observed.Failure = PublicUdpFailure("synthetic-private-error") },
	} {
		observed := publicUdpTestHealthy(request)
		mutate(&observed)
		findings := publicUdpFindings(request, observed, nil)
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
