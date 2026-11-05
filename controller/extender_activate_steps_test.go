package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
)

// The probe steps an activation answers with (ExtenderProbeStep, C2): every
// step probed -- the required test's in the order probed, then the optional
// carriers', which run together -- with its family, port, how long it ran,
// how much of the shared budget was left when it started, and how it ended.
// The refused carriers carry the same two times. Judged over fake probes on
// the fake activation clock, so each time is exact.

func testProbeStepText(step *ExtenderProbeStep) string {
	return fmt.Sprintf(
		"%s v%d :%d %dms of %dms %s",
		step.Name,
		step.IpVersion,
		step.Port,
		step.ElapsedMs,
		step.BudgetLeftMs,
		step.Outcome,
	)
}

func testProbeStepTexts(steps []*ExtenderProbeStep) []string {
	texts := []string{}
	for _, step := range steps {
		texts = append(texts, testProbeStepText(step))
	}
	return texts
}

// An accepted activation lists the required test's steps, in the order
// probed, and then every optional step, listed as the dns ports and then
// quic, each with its own times. The optional carriers start together after
// the forward, so each had the same 9.5 s of the budget left; a dns port that
// waited it out timed out at the end of the shared budget, and its refused
// carrier entry carries the same times as its step.
func TestExtenderActivationListsEveryProbeStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probeNames := testOptionalCarrierProbes
		probes := newTestActivationProbes()
		probes.answer = func(ctx context.Context, probe string) error {
			switch probe {
			case testTimingTcpProbe:
				return testAnswerAfter(ctx, 200*time.Millisecond)
			case probeNames.dns53:
				return testNeverAnswer(ctx)
			case probeNames.dns4053:
				return testAnswerAfter(ctx, 100*time.Millisecond)
			case probeNames.quic:
				return testAnswerAfter(ctx, 400*time.Millisecond)
			}
			return nil
		}
		probes.forwardAnswer = func(ctx context.Context) error {
			return testAnswerAfter(ctx, 300*time.Millisecond)
		}
		outcome := probes.judgeOnTheRealClock(
			context.Background(),
			connect.ExtenderCarrierTcp,
			connect.ExtenderCarrierQuic,
			connect.ExtenderCarrierDns,
		)
		if outcome.refusal != "" {
			t.Fatalf("the activation was refused: %s", outcome.refusal)
		}
		connect.AssertEqual(t, testProbeStepTexts(outcome.steps), []string{
			"tcp v4 :8443 200ms of 10000ms ok",
			// the forward runs over the tcp carrier
			"forward v4 :8443 300ms of 9800ms ok",
			"dns v4 :53 9500ms of 9500ms timeout",
			"dns v4 :4053 100ms of 9500ms ok",
			"quic v4 :8444 400ms of 9500ms ok",
		})
		connect.AssertEqual(t, len(outcome.refusedCarriers), 1)
		refusedCarrier := outcome.refusedCarriers[0]
		connect.AssertEqual(t, refusedCarrier.Carrier, connect.ExtenderCarrierDns)
		connect.AssertEqual(t, refusedCarrier.Port, 53)
		connect.AssertEqual(t, refusedCarrier.ElapsedMs, int64(9500))
		connect.AssertEqual(t, refusedCarrier.BudgetLeftMs, int64(9500))
	})
}

// A refusal lists the steps up to the one that refused it, which is last,
// and says which step that was and how it ended. Nothing after it is probed.
func TestExtenderActivationRefusalStepsEndWithTheStepThatRefused(t *testing.T) {
	cases := []struct {
		name           string
		setup          func(probes *testActivationProbes)
		wantSteps      []string
		wantStep       string
		wantOutcome    string
		wantRefusalHas string
	}{
		{
			name: "a tcp carrier that refuses at once",
			setup: func(probes *testActivationProbes) {
				probes.carrierErrs[testTimingTcpProbe] = errors.New("connection refused")
			},
			wantSteps:      []string{"tcp v4 :8443 0ms of 10000ms error"},
			wantStep:       connect.ExtenderCarrierTcp,
			wantOutcome:    extenderProbeStepOutcomeError,
			wantRefusalHas: "the tcp carrier failed after 0 s: connection refused",
		},
		{
			name: "a forward that never answers after a slow tcp carrier",
			setup: func(probes *testActivationProbes) {
				probes.answer = func(ctx context.Context, probe string) error {
					probes.clock.Advance(9500 * time.Millisecond)
					return nil
				}
				probes.forwardAnswer = func(ctx context.Context) error {
					return probes.clock.WaitOut(ctx)
				}
			},
			wantSteps: []string{
				"tcp v4 :8443 9500ms of 10000ms ok",
				"forward v4 :8443 500ms of 500ms timeout",
			},
			wantStep:       extenderProbeStepForward,
			wantOutcome:    extenderProbeStepOutcomeTimeout,
			wantRefusalHas: "0.5 s left",
		},
		{
			name: "a forward on the other family",
			setup: func(probes *testActivationProbes) {
				probes.forwardClientAddress = "[2001:db8::10]:443"
				probes.forwardAnswer = func(ctx context.Context) error {
					probes.clock.Advance(250 * time.Millisecond)
					return nil
				}
			},
			wantSteps: []string{
				"tcp v4 :8443 0ms of 10000ms ok",
				"forward v4 :8443 250ms of 10000ms error",
			},
			wantStep:       extenderProbeStepForward,
			wantOutcome:    extenderProbeRefusalOtherFamily,
			wantRefusalHas: "the forward arrived on ipv6, not ipv4, after 0.25 s",
		},
	}
	for _, c := range cases {
		probes := newTestActivationProbes()
		c.setup(probes)
		outcome := probes.judge(
			context.Background(),
			connect.ExtenderCarrierTcp,
			connect.ExtenderCarrierQuic,
			connect.ExtenderCarrierDns,
		)
		if got := testProbeStepTexts(outcome.steps); fmt.Sprint(got) != fmt.Sprint(c.wantSteps) {
			t.Fatalf("%s: steps = %v, want %v", c.name, got, c.wantSteps)
		}
		if outcome.refusalStep != c.wantStep || outcome.refusalOutcome != c.wantOutcome {
			t.Fatalf("%s: refused at %s %s, want %s %s", c.name, outcome.refusalStep, outcome.refusalOutcome, c.wantStep, c.wantOutcome)
		}
		if !strings.Contains(outcome.refusal, c.wantRefusalHas) {
			t.Fatalf("%s: refusal = %q, want it to say %q", c.name, outcome.refusal, c.wantRefusalHas)
		}
	}
}

// Every step names the family the activation probes, which is the caller's.
func TestExtenderActivationStepsNameTheProbedFamily(t *testing.T) {
	probes := newTestActivationProbes()
	probes.forwardClientAddress = "[2001:db8::10]:443"
	outcome := probeExtenderActivation(
		context.Background(),
		probes.clock.activationClock(),
		probes.activationProbes(),
		[]string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic},
		testAcceptanceTcpPort,
		testAcceptanceUdpPort,
		testAcceptanceDnsPorts,
		6,
	)
	if outcome.refusal != "" {
		t.Fatalf("the activation was refused: %s", outcome.refusal)
	}
	connect.AssertEqual(t, testProbeStepTexts(outcome.steps), []string{
		"tcp v6 :8443 0ms of 10000ms ok",
		"forward v6 :8443 0ms of 10000ms ok",
		"quic v6 :8444 0ms of 10000ms ok",
	})
}
