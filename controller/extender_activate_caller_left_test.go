package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// An activation whose caller leaves while its required test runs
// (probeExtenderActivation), judged over the fake probes of
// extender_activate_acceptance_test.go on the fake clock. The caller's
// context is the api's request context, which net/http cancels when the lb
// closes the upstream connection because its own client went away.
//
// Root cause of all but the last: the probe did not tell the caller's
// departure from a failure of the extender's. A step the departure cut off was
// an error, and the webrtc path ran in place of a tcp path that failed only so,
// reading the operator's cap on the canceled context, which every database
// read refuses. On Main from 2026-11-23 about 4,000 activations a day whose
// callers gave up 4-5 s into the tcp challenge were counted as refused at the
// webrtc stage with an error, no webrtc test run, and each logged "activation
// webrtc cap could not be read" with its stack.

// The caller's departure as label values. Literal, as the dashboards read
// them.
const (
	testCallerLeftOutcome     = "canceled"
	testCallerLeftStepOutcome = "canceled"
)

// What the database raises for a read on a canceled context: the pool's
// acquire refused, joined with the context's error, as the cap's count raised
// it on Main ("Done", "context canceled", "context canceled").
func testCanceledReadPanic(ctx context.Context) {
	panic(errors.Join(server.DbContextDoneError, ctx.Err(), ctx.Err()))
}

// A cap that reads as the database does: refused once its context has ended,
// and room for one more otherwise. Each ask is counted.
func testDatabaseWebRtcOnlyRoom(asked *int) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		*asked += 1
		if ctx.Err() != nil {
			testCanceledReadPanic(ctx)
		}
		return extenderWebRtcOnlyRoom(2000, 853)
	}
}

// Judges an ipv4 activation for the caller whose context is callerCtx, with
// the cap answered by room.
func (self *testActivationProbes) judgeForCaller(
	callerCtx context.Context,
	room func(ctx context.Context) error,
	carriers ...string,
) *extenderActivationOutcome {
	activationProbes := self.activationProbes()
	activationProbes.webRtcOnlyRoom = room
	return probeExtenderActivation(
		callerCtx,
		self.clock.activationClock(),
		activationProbes,
		testActivationTcpChallengeTimeout,
		carriers,
		testAcceptanceTcpPort,
		testAcceptanceUdpPort,
		testAcceptanceDnsPorts,
		4,
	)
}

// The refusal is counted under a series that exists from the start.
func testRefusalHasASeries(t testing.TB, outcome *extenderActivationOutcome) {
	t.Helper()
	if outcomes := extenderActivationStageOutcomes[outcome.refusalStep]; !slices.Contains(outcomes, outcome.refusalOutcome) {
		t.Fatalf("the refusal is counted at %s %s, which has no series", outcome.refusalStep, outcome.refusalOutcome)
	}
}

// Observable: of an activation that offers tcp, webrtc and quic, a caller that
// leaves 4.6 s into a tcp challenge that gets no answer ends the activation at
// the tcp step, canceled. The cap is never read and the webrtc test never
// runs, and the tcp step is listed as cut off at 4.6 s, not as an error.
func TestExtenderActivationEndsAtTheTcpChallengeItsCallerLeft(t *testing.T) {
	probes := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	callerCtx, leave := context.WithCancel(context.Background())
	defer leave()
	probes.answer = func(ctx context.Context, probe string) error {
		if probe != tcpProbe {
			return fmt.Errorf("unexpected probe %s", probe)
		}
		// a black-holed port, which the caller gives up on before the
		// challenge's share runs out
		probes.clock.Advance(4600 * time.Millisecond)
		leave()
		<-ctx.Done()
		return ctx.Err()
	}
	webRtcDials := 0
	probes.webRtcAnswer = func(ctx context.Context) (string, error) {
		webRtcDials += 1
		return probes.webRtcClientAddress, nil
	}
	asked := 0
	outcome := probes.judgeForCaller(
		callerCtx,
		testDatabaseWebRtcOnlyRoom(&asked),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierWebRtc,
		connect.ExtenderCarrierQuic,
	)

	connect.AssertEqual(t, asked, 0)
	connect.AssertEqual(t, webRtcDials, 0)
	connect.AssertEqual(t, probes.probesSoFar(), []string{tcpProbe})
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierTcp)
	connect.AssertEqual(t, outcome.refusalOutcome, testCallerLeftOutcome)
	testRefusalHasASeries(t, outcome)
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp})
	connect.AssertEqual(t, outcome.steps[0].Outcome, testCallerLeftStepOutcome)
	connect.AssertEqual(t, outcome.steps[0].ElapsedMs, int64(4600))
	for _, want := range []string{
		"the tcp carrier was cut off after 4.6 s, when the activation's caller left",
		"; the webrtc carrier was not tried: the activation's caller left",
	} {
		if !strings.Contains(outcome.refusal, want) {
			t.Fatalf("refusal = %q, want it to name %q", outcome.refusal, want)
		}
	}
	if strings.Contains(outcome.refusal, "could not count") {
		t.Fatalf("refusal = %q, which reads the cap for a caller that left", outcome.refusal)
	}
	connect.AssertEqual(t, len(outcome.carriers), 0)
}

// Observable: a tcp challenge that runs out its 5 s share on its own is a
// timeout, and a caller that leaves as it does gets no webrtc path in its
// place: the cap is not read, and the activation ends at the tcp step,
// canceled.
func TestExtenderActivationRunsNoWebRtcPathForACallerThatLeftAsTheTcpChallengeTimedOut(t *testing.T) {
	probes := newTestActivationProbes()
	callerCtx, leave := context.WithCancel(context.Background())
	defer leave()
	probes.answer = func(ctx context.Context, probe string) error {
		err := probes.clock.WaitOut(ctx)
		leave()
		return err
	}
	asked := 0
	outcome := probes.judgeForCaller(
		callerCtx,
		testDatabaseWebRtcOnlyRoom(&asked),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierWebRtc,
	)

	connect.AssertEqual(t, asked, 0)
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp})
	connect.AssertEqual(t, outcome.steps[0].Outcome, extenderProbeStepOutcomeTimeout)
	connect.AssertEqual(t, outcome.steps[0].ElapsedMs, testActivationTcpChallengeTimeout.Milliseconds())
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierTcp)
	connect.AssertEqual(t, outcome.refusalOutcome, testCallerLeftOutcome)
	testRefusalHasASeries(t, outcome)
}

// Observable: a caller that leaves while its cap is read -- the count waiting
// for a connection when the departure cancels it -- ends the activation at the
// webrtc stage, canceled rather than an error, with no webrtc test run.
func TestExtenderActivationEndsAtTheWebRtcCapReadItsCallerStopped(t *testing.T) {
	probes := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	probes.carrierErrs[tcpProbe] = errors.New("connection refused")
	callerCtx, leave := context.WithCancel(context.Background())
	defer leave()
	asked := 0
	room := func(ctx context.Context) error {
		asked += 1
		leave()
		<-ctx.Done()
		testCanceledReadPanic(ctx)
		return nil
	}
	outcome := probes.judgeForCaller(callerCtx, room, connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)

	connect.AssertEqual(t, asked, 1)
	connect.AssertEqual(t, probes.probesSoFar(), []string{tcpProbe})
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp})
	// the tcp port refused on its own, before the caller left
	connect.AssertEqual(t, outcome.steps[0].Outcome, extenderProbeStepOutcomeError)
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierWebRtc)
	connect.AssertEqual(t, outcome.refusalOutcome, testCallerLeftOutcome)
	testRefusalHasASeries(t, outcome)
	if !strings.Contains(outcome.refusal, "the webrtc carrier was not tried") {
		t.Fatalf("refusal = %q, want the webrtc path untried", outcome.refusal)
	}
}

// Observable: a caller that leaves 2 s into the webrtc test ends the
// activation at the webrtc stage, canceled, with the webrtc step listed as cut
// off at 2 s rather than failed.
func TestExtenderActivationEndsAtTheWebRtcTestItsCallerLeft(t *testing.T) {
	probes := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	probes.carrierErrs[tcpProbe] = errors.New("connection refused")
	callerCtx, leave := context.WithCancel(context.Background())
	defer leave()
	probes.webRtcAnswer = func(ctx context.Context) (string, error) {
		probes.clock.Advance(2 * time.Second)
		leave()
		<-ctx.Done()
		return "", ctx.Err()
	}
	asked := 0
	outcome := probes.judgeForCaller(
		callerCtx,
		testDatabaseWebRtcOnlyRoom(&asked),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierWebRtc,
	)

	// the cap was read while the caller was there
	connect.AssertEqual(t, asked, 1)
	connect.AssertEqual(t, probes.probesSoFar(), []string{tcpProbe, testWebRtcProbe})
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc})
	connect.AssertEqual(t, outcome.steps[0].Outcome, extenderProbeStepOutcomeError)
	connect.AssertEqual(t, outcome.steps[1].Outcome, testCallerLeftStepOutcome)
	connect.AssertEqual(t, outcome.steps[1].ElapsedMs, int64(2000))
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierWebRtc)
	connect.AssertEqual(t, outcome.refusalOutcome, testCallerLeftOutcome)
	testRefusalHasASeries(t, outcome)
	if !strings.Contains(outcome.refusal, "the webrtc carrier was cut off after 2 s, when the activation's caller left") {
		t.Fatalf("refusal = %q, want the webrtc test cut off", outcome.refusal)
	}
}

// Observable: an activation that offers webrtc alone, whose caller has gone
// by the time it is probed, reads no cap and signals no offer, and ends at
// the webrtc stage, canceled, with no step.
func TestExtenderActivationOnWebRtcAloneReadsNoCapForACallerThatHasGone(t *testing.T) {
	probes := newTestActivationProbes()
	callerCtx, leave := context.WithCancel(context.Background())
	leave()
	asked := 0
	outcome := probes.judgeForCaller(callerCtx, testDatabaseWebRtcOnlyRoom(&asked), connect.ExtenderCarrierWebRtc)

	connect.AssertEqual(t, asked, 0)
	connect.AssertEqual(t, len(probes.probesSoFar()), 0)
	connect.AssertEqual(t, len(outcome.steps), 0)
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierWebRtc)
	connect.AssertEqual(t, outcome.refusalOutcome, testCallerLeftOutcome)
	testRefusalHasASeries(t, outcome)
}

// Observable: of an activation that offers tcp without webrtc, a caller that
// leaves 1.5 s into the forward ends it at the forward, canceled, and the
// tcp challenge that passed before it stays ok.
func TestExtenderActivationWithoutWebRtcEndsAtTheForwardItsCallerLeft(t *testing.T) {
	probes := newTestActivationProbes()
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	callerCtx, leave := context.WithCancel(context.Background())
	defer leave()
	probes.forwardAnswer = func(ctx context.Context) error {
		probes.clock.Advance(1500 * time.Millisecond)
		leave()
		<-ctx.Done()
		return ctx.Err()
	}
	outcome := probes.judge(callerCtx, connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic)

	connect.AssertEqual(t, probes.probesSoFar(), []string{tcpProbe, extenderProbeStepForward})
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp, extenderProbeStepForward})
	connect.AssertEqual(t, outcome.steps[0].Outcome, extenderProbeStepOutcomeOk)
	connect.AssertEqual(t, outcome.steps[1].Outcome, testCallerLeftStepOutcome)
	connect.AssertEqual(t, outcome.refusalStep, extenderProbeStepForward)
	connect.AssertEqual(t, outcome.refusalOutcome, testCallerLeftOutcome)
	testRefusalHasASeries(t, outcome)
}

// The composed activation (extenderActivateService.activate) on the local
// test database, with the cap the activation builds itself, the real count
// (model.CountActiveWebRtcOnlyNetworkExtenders), and the caller's session
// canceled as the tcp challenge waits on a black-holed port. Before, the
// count ran on the canceled context, the database refused it, and the
// activation was counted at the webrtc stage with an error, as on Main.
// Observable: it is counted at the tcp stage, canceled, the webrtc test never
// runs, its answer lists the tcp step as cut off, and nothing is counted at
// the webrtc stage.
func TestExtenderActivateCountsACallerThatLeftAtTheTcpChallengeAsCanceled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		settings := DefaultExtenderActivateSettings()
		metrics := newExtenderActivationMetrics(prometheus.NewRegistry())
		settings.metrics = metrics
		service := newExtenderActivateService(settings)

		account := newTestExtenderAccount(ctx)
		host := account.newHost(t, ctx)
		clientSession := host.session(ctx, "198.51.100.40")

		webRtcDials := 0
		service.testProbes = func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes {
			probes := testFakeExtenderProbes(nil)(clientIp, ipVersion)
			probes.carrier = func(ctx context.Context, connectMode connect.ExtenderConnectMode, port int) error {
				// the caller gives up while the challenge gets no answer
				clientSession.Cancel()
				<-ctx.Done()
				return ctx.Err()
			}
			probes.webRtc = func(ctx context.Context) (string, error) {
				webRtcDials += 1
				return netip.AddrPortFrom(clientIp, 443).String(), nil
			}
			// no webRtcOnlyRoom: the activation reads the operator's real cap
			return probes
		}

		args := host.activateArgs()
		args.Carriers = []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc}
		result, err := service.activate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.Activated {
			t.Fatalf("activated for a caller that left")
		}
		connect.AssertEqual(t, webRtcDials, 0)
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("webrtc", "error", "ipv4")), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("tcp", testCallerLeftOutcome, "ipv4")), float64(1))
		if len(result.ProbeSteps) != 1 {
			t.Fatalf("probe steps = %d, want the tcp challenge alone", len(result.ProbeSteps))
		}
		connect.AssertEqual(t, result.ProbeSteps[0].Name, connect.ExtenderCarrierTcp)
		connect.AssertEqual(t, result.ProbeSteps[0].Outcome, testCallerLeftStepOutcome)
		if strings.Contains(result.Error, "could not count") {
			t.Fatalf("refusal = %q, which read the cap for a caller that left", result.Error)
		}
	})
}

// A deadline of the caller's own is a budget, not a departure: a step it ends
// times out within what it left, as before.
func TestExtenderActivationTimesOutAgainstTheCallersOwnDeadline(t *testing.T) {
	probes := newTestActivationProbes()
	callerCtx, cancel := probes.clock.WithDeadline(context.Background(), probes.clock.Now().Add(3*time.Second))
	defer cancel()
	probes.answer = func(ctx context.Context, probe string) error {
		return probes.clock.WaitOut(ctx)
	}
	outcome := probes.judge(callerCtx, connect.ExtenderCarrierTcp)

	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp})
	connect.AssertEqual(t, outcome.steps[0].Outcome, extenderProbeStepOutcomeTimeout)
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierTcp)
	connect.AssertEqual(t, outcome.refusalOutcome, extenderProbeStepOutcomeTimeout)
	if !strings.Contains(outcome.refusal, "did not answer within 3 s") {
		t.Fatalf("refusal = %q, want the caller's 3 s", outcome.refusal)
	}
}
