package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The acceptance rule with the peer-to-peer webrtc carrier as a required path
// (connect/EXTENDER.md C2a, S8), judged over the fake probes of
// extender_activate_acceptance_test.go. Nothing here dials or reads the
// database. The two required paths run at once, so where an order matters a
// fake waits on what the other paths have done rather than on time.

const testWebRtcProbe = connect.ExtenderCarrierWebRtc

// The probes logged so far, under the log's lock.
func (self *testActivationProbes) probesSoFar() []string {
	self.probesLock.Lock()
	defer self.probesLock.Unlock()
	return slices.Clone(self.probes)
}

// The step names of an outcome, in the order listed.
func testStepNames(outcome *extenderActivationOutcome) []string {
	names := []string{}
	for _, step := range outcome.steps {
		names = append(names, step.Name)
	}
	return names
}

// Root cause: the operator refused every activation without the tcp carrier,
// so a host behind nat, which has no public tcp port, could never extend.
// Observable: an activation that offers webrtc alone passes on the webrtc
// test, is stored with webrtc as its carrier, and its one step is the webrtc
// dial, which names no port.
func TestExtenderActivationAcceptsOnWebRtcAlone(t *testing.T) {
	probes := newTestActivationProbes()
	outcome := probes.judge(context.Background(), connect.ExtenderCarrierWebRtc)
	if outcome.refusal != "" {
		t.Fatalf("refused: %s", outcome.refusal)
	}
	connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierWebRtc})
	connect.AssertEqual(t, probes.probesSoFar(), []string{testWebRtcProbe})
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierWebRtc})
	step := outcome.steps[0]
	connect.AssertEqual(t, step.Port, 0)
	connect.AssertEqual(t, step.IpVersion, 4)
	connect.AssertEqual(t, step.Outcome, extenderProbeStepOutcomeOk)
	connect.AssertEqual(t, len(outcome.refusedCarriers), 0)
}

// Root cause: a host behind nat that binds tcp 443 gets no answer on it, and a
// tcp test that waited out the probe budget before the webrtc test would
// starve it. Observable: the tcp test waits out the budget while the webrtc
// test passes; the optional carriers start once webrtc passed, before tcp gave
// up; the activation is stored on webrtc and quic, and tcp is listed as left
// out with its timeout.
func TestExtenderActivationAcceptsOnWebRtcWhileTcpGetsNoAnswer(t *testing.T) {
	probes := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	optionalAnswered := make(chan struct{})
	probes.answer = func(ctx context.Context, probe string) error {
		switch probe {
		case tcpProbe:
			// no answer, and only once the optional carrier has answered, so
			// the shared budget is still whole when the optional carriers run
			<-optionalAnswered
			return probes.clock.WaitOut(ctx)
		case testOptionalCarrierProbes.quic:
			close(optionalAnswered)
			return nil
		default:
			return fmt.Errorf("unexpected probe %s", probe)
		}
	}
	outcome := probes.judge(
		context.Background(),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierWebRtc,
		connect.ExtenderCarrierQuic,
	)
	if outcome.refusal != "" {
		t.Fatalf("refused: %s", outcome.refusal)
	}
	connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic})
	connect.AssertEqual(t, testStepNames(outcome), []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierWebRtc,
		connect.ExtenderCarrierQuic,
	})
	connect.AssertEqual(t, outcome.steps[0].Outcome, extenderProbeStepOutcomeTimeout)
	connect.AssertEqual(t, outcome.steps[1].Outcome, extenderProbeStepOutcomeOk)
	connect.AssertEqual(t, outcome.steps[2].Outcome, extenderProbeStepOutcomeOk)
	// the optional carrier had all of the budget: it ran before tcp gave up
	connect.AssertEqual(t, outcome.steps[2].BudgetLeftMs, ExtenderActivateProbeTimeout.Milliseconds())
	if refused := testRefusedCarrierNames(t, outcome); !slices.Equal(refused, []string{tcpProbe}) {
		t.Fatalf("refused carriers = %v, want tcp alone", refused)
	}
	if !strings.HasPrefix(outcome.refusedCarriers[0].Error, "the tcp carrier did not answer within the 10 s probe budget") {
		t.Fatalf("tcp was left out for %q", outcome.refusedCarriers[0].Error)
	}
}

// The optional carriers start only once a required path passed: with tcp
// refused at once and webrtc still running, nothing optional is probed; when
// webrtc passes they are.
func TestExtenderActivationProbesNoOptionalCarrierBeforeARequiredPathPasses(t *testing.T) {
	probes := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	tcpRefused := make(chan struct{})
	probes.answer = func(ctx context.Context, probe string) error {
		if probe == tcpProbe {
			defer close(tcpRefused)
			return errors.New("connection refused")
		}
		return nil
	}
	probedBeforeWebRtc := []string{}
	probes.webRtcAnswer = func(ctx context.Context) (string, error) {
		<-tcpRefused
		probedBeforeWebRtc = probes.probesSoFar()
		return probes.webRtcClientAddress, nil
	}
	outcome := probes.judge(
		context.Background(),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierWebRtc,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	)
	if outcome.refusal != "" {
		t.Fatalf("refused: %s", outcome.refusal)
	}
	for _, probe := range probedBeforeWebRtc {
		if probe != tcpProbe && probe != testWebRtcProbe {
			t.Fatalf("%s was probed before a required path passed (%v)", probe, probedBeforeWebRtc)
		}
	}
	connect.AssertEqual(t, outcome.carriers, []string{
		connect.ExtenderCarrierWebRtc,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	})
	optionalProbed := 0
	for _, probe := range probes.probesSoFar() {
		if probe != tcpProbe && probe != testWebRtcProbe {
			optionalProbed += 1
		}
	}
	// quic and both dns ports
	connect.AssertEqual(t, optionalProbed, 3)
}

// An extender that answers on both required paths is stored on both, beside
// every optional carrier that answered, in the order offered.
func TestExtenderActivationAcceptsOnBothRequiredPaths(t *testing.T) {
	probes := newTestActivationProbes()
	outcome := probes.judge(
		context.Background(),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
		connect.ExtenderCarrierWebRtc,
	)
	if outcome.refusal != "" {
		t.Fatalf("refused: %s", outcome.refusal)
	}
	connect.AssertEqual(t, outcome.carriers, []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
		connect.ExtenderCarrierWebRtc,
	})
	connect.AssertEqual(t, outcome.dnsPorts, testAcceptanceDnsPorts)
	connect.AssertEqual(t, len(outcome.refusedCarriers), 0)
	// the tcp path's steps, then the webrtc path's, then the optional ones
	names := testStepNames(outcome)
	connect.AssertEqual(t, names[:3], []string{
		connect.ExtenderCarrierTcp,
		extenderProbeStepForward,
		connect.ExtenderCarrierWebRtc,
	})
}

// An activation that passes neither required path it offered is refused with
// each failure named, at the webrtc step, and probes no optional carrier.
func TestExtenderActivationRefusesWhenNeitherRequiredPathPasses(t *testing.T) {
	probes := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	probes.carrierErrs[tcpProbe] = errors.New("connection refused")
	probes.webRtcErr = errors.New("the extender's client did not answer the webrtc offer")
	outcome := probes.judge(
		context.Background(),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierWebRtc,
		connect.ExtenderCarrierQuic,
	)
	for _, want := range []string{
		"the tcp carrier failed after",
		"connection refused",
		"; the webrtc carrier failed after",
		"did not answer the webrtc offer",
	} {
		if !strings.Contains(outcome.refusal, want) {
			t.Fatalf("refusal = %q, want it to name %q", outcome.refusal, want)
		}
	}
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierWebRtc)
	connect.AssertEqual(t, outcome.refusalOutcome, extenderProbeStepOutcomeError)
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc})
	probed := probes.probesSoFar()
	slices.Sort(probed)
	connect.AssertEqual(t, probed, []string{tcpProbe, testWebRtcProbe})
	connect.AssertEqual(t, len(outcome.carriers), 0)
	if outcomes := extenderActivationStageOutcomes[outcome.refusalStep]; !slices.Contains(outcomes, outcome.refusalOutcome) {
		t.Fatalf("the refusal is counted at %s %s, which has no series", outcome.refusalStep, outcome.refusalOutcome)
	}
}

// The webrtc forward is held to the family the activation was posted for
// (A7): one that arrived on the other family refuses a webrtc-only activation
// as other_family, and a webrtc test that waits out the budget is a timeout.
// Both are counted stages.
func TestExtenderActivationRefusesTheWebRtcTestsFailures(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(probes *testActivationProbes)
		wantRefusal string
		wantOutcome string
	}{
		{
			name: "the forward on the other family",
			setup: func(probes *testActivationProbes) {
				probes.webRtcClientAddress = "[2001:db8::10]:443"
			},
			wantRefusal: "the webrtc forward arrived on ipv6, not ipv4",
			wantOutcome: extenderProbeRefusalOtherFamily,
		},
		{
			name: "no answer within the budget",
			setup: func(probes *testActivationProbes) {
				probes.webRtcAnswer = func(ctx context.Context) (string, error) {
					return "", probes.clock.WaitOut(ctx)
				}
			},
			wantRefusal: "the webrtc carrier did not answer within the 10 s probe budget",
			wantOutcome: extenderProbeStepOutcomeTimeout,
		},
		{
			name: "no signaling path",
			setup: func(probes *testActivationProbes) {
				probes.webRtcErr = ErrExtenderWebRtcClientNotConnected
			},
			wantRefusal: "is not connected to the exchange",
			wantOutcome: extenderProbeStepOutcomeError,
		},
	}
	for _, c := range cases {
		probes := newTestActivationProbes()
		c.setup(probes)
		outcome := probes.judge(context.Background(), connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic)
		if !strings.Contains(outcome.refusal, c.wantRefusal) {
			t.Fatalf("%s: refusal = %q, want %q", c.name, outcome.refusal, c.wantRefusal)
		}
		connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierWebRtc)
		connect.AssertEqual(t, outcome.refusalOutcome, c.wantOutcome)
		if outcomes := extenderActivationStageOutcomes[outcome.refusalStep]; !slices.Contains(outcomes, outcome.refusalOutcome) {
			t.Fatalf("%s: counted at %s %s, which has no series", c.name, outcome.refusalStep, outcome.refusalOutcome)
		}
		// nothing optional after a refusal
		connect.AssertEqual(t, probes.probesSoFar(), []string{testWebRtcProbe})
	}
}

// An operator whose probes carry no webrtc test refuses a webrtc-only
// activation for want of a signaling path, rather than accepting or hanging.
func TestExtenderActivationRefusesWebRtcWithoutASignalingPath(t *testing.T) {
	probes := newTestActivationProbes()
	activationProbes := probes.activationProbes()
	activationProbes.webRtc = nil
	outcome := probeExtenderActivation(
		context.Background(),
		probes.clock.activationClock(),
		activationProbes,
		[]string{connect.ExtenderCarrierWebRtc},
		testAcceptanceTcpPort,
		testAcceptanceUdpPort,
		testAcceptanceDnsPorts,
		4,
	)
	if !strings.Contains(outcome.refusal, ErrExtenderWebRtcSignalingUnavailable.Error()) {
		t.Fatalf("refusal = %q", outcome.refusal)
	}
}

// A webrtc test that panics fails that path alone: the activation passes on
// tcp, and webrtc is left out with the panic as its reason.
func TestExtenderActivationContainsAWebRtcProbePanic(t *testing.T) {
	probes := newTestActivationProbes()
	probes.webRtcAnswer = func(ctx context.Context) (string, error) {
		panic("synthetic webrtc probe failure")
	}
	outcome := probes.judge(context.Background(), connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)
	if outcome.refusal != "" {
		t.Fatalf("refused: %s", outcome.refusal)
	}
	connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierTcp})
	if len(outcome.refusedCarriers) != 1 || outcome.refusedCarriers[0].Carrier != connect.ExtenderCarrierWebRtc ||
		!strings.Contains(outcome.refusedCarriers[0].Error, "synthetic webrtc probe failure") {
		t.Fatalf("refused carriers = %+v, want webrtc with the panic", outcome.refusedCarriers)
	}
}

// Without webrtc the activation is judged as it always was: an offer of tcp
// probes the tcp path, then the optional carriers, and its steps and
// refusals are the tcp path's alone. The webrtc test is never run.
func TestExtenderActivationWithoutWebRtcNeverRunsTheWebRtcTest(t *testing.T) {
	probes := newTestActivationProbes()
	outcome := probes.judge(context.Background(), connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic)
	if outcome.refusal != "" {
		t.Fatalf("refused: %s", outcome.refusal)
	}
	if slices.Contains(probes.probesSoFar(), testWebRtcProbe) {
		t.Fatalf("the webrtc test ran for an activation that did not offer it")
	}
	connect.AssertEqual(t, testStepNames(outcome)[:2], []string{connect.ExtenderCarrierTcp, extenderProbeStepForward})

	refused := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	refused.carrierErrs[tcpProbe] = errors.New("connection refused")
	refusedOutcome := refused.judge(context.Background(), connect.ExtenderCarrierTcp)
	if !strings.HasPrefix(refusedOutcome.refusal, "the tcp carrier failed after") || strings.Contains(refusedOutcome.refusal, ";") {
		t.Fatalf("refusal = %q, want the tcp carrier's alone", refusedOutcome.refusal)
	}
	connect.AssertEqual(t, refusedOutcome.refusalStep, connect.ExtenderCarrierTcp)
	if slices.Contains(refused.probesSoFar(), testWebRtcProbe) {
		t.Fatalf("the webrtc test ran for an activation that did not offer it")
	}
}
