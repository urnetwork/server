package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A refusal says how long its step ran and, when the step's budget ran out
// before it answered, how much time it had (C2, connect#230). The steps of an
// activation share one 10 s probe budget, and before this a step that
// answered too late, one that was starved by the steps before it, and one
// that never answered were all "did not answer: context deadline exceeded".
// An extender whose tcp challenge took 9.5 s was told its forward did not
// answer, when the forward had had half a second to.
//
// Each case judges fake probes on the fake activation clock, so a step that
// takes 9.5 s does so at once and the times in the messages are exact.

var testTimingTcpProbe = fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)

// The tcp challenge takes 9.5 s and the forward never answers: the refusal
// names the forward and the 0.5 s of the budget that was left for it.
func TestExtenderActivationRefusalNamesTheBudgetLeftForALateStep(t *testing.T) {
	probes := newTestActivationProbes()
	probes.answer = func(ctx context.Context, probe string) error {
		if probe == testTimingTcpProbe {
			probes.clock.Advance(9500 * time.Millisecond)
		}
		return nil
	}
	probes.forwardAnswer = func(ctx context.Context) error {
		return probes.clock.WaitOut(ctx)
	}
	outcome := probes.judge(
		context.Background(),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	)
	connect.AssertEqual(
		t,
		outcome.refusal,
		"the forward did not answer within the 0.5 s left of the 10 s probe budget: context deadline exceeded",
	)
}

// A forward that fails fast says when it failed and makes no claim about the
// budget, which it did not run out of.
func TestExtenderActivationRefusalOfAStepThatFailsFastMakesNoBudgetClaim(t *testing.T) {
	probes := newTestActivationProbes()
	probes.answer = func(ctx context.Context, probe string) error {
		if probe == testTimingTcpProbe {
			probes.clock.Advance(200 * time.Millisecond)
		}
		return nil
	}
	probes.forwardAnswer = func(ctx context.Context) error {
		probes.clock.Advance(100 * time.Millisecond)
		return errors.New("extender hello answered with status 502")
	}
	outcome := probes.judge(context.Background(), connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns)
	connect.AssertEqual(t, outcome.refusal, "the forward failed after 0.1 s: extender hello answered with status 502")
	for _, budgetClaim := range []string{"within", "budget", "deadline"} {
		if strings.Contains(outcome.refusal, budgetClaim) {
			t.Fatalf("the refusal of a forward that failed fast says %q: %s", budgetClaim, outcome.refusal)
		}
	}
}

// A tcp carrier that never answers had the whole budget, and says so.
func TestExtenderActivationRefusalOfASilentTcpCarrierNamesTheWholeBudget(t *testing.T) {
	probes := newTestActivationProbes()
	probes.answer = func(ctx context.Context, probe string) error {
		if probe == testTimingTcpProbe {
			return probes.clock.WaitOut(ctx)
		}
		return nil
	}
	outcome := probes.judge(context.Background(), connect.ExtenderCarrierTcp)
	connect.AssertEqual(
		t,
		outcome.refusal,
		"the tcp carrier did not answer within the 10 s probe budget: context deadline exceeded",
	)
}

// An optional carrier that never answers is left out with what was left of
// the shared budget when the optional carriers started, the same for each
// since they run together (connect#230). One that fails on its own keeps its
// probe's error alone.
func TestExtenderActivationLeftOutCarrierSaysHowLongItHadToAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probeNames := testOptionalCarrierProbes
		probes := newTestActivationProbes()
		probes.answer = func(ctx context.Context, probe string) error {
			switch probe {
			case testTimingTcpProbe:
				return testAnswerAfter(ctx, 1500*time.Millisecond)
			case probeNames.dns53, probeNames.quic:
				return testNeverAnswer(ctx)
			case probeNames.dns4053:
				if err := testAnswerAfter(ctx, 500*time.Millisecond); err != nil {
					return err
				}
				return errors.New("connection refused")
			}
			return nil
		}
		outcome := probes.judgeOnTheRealClock(
			context.Background(),
			connect.ExtenderCarrierTcp,
			connect.ExtenderCarrierDns,
			connect.ExtenderCarrierQuic,
		)
		if outcome.refusal != "" {
			t.Fatalf("the activation was refused for an optional carrier: %s", outcome.refusal)
		}
		wantErrors := map[string]string{
			// the optional carriers start 1.5 s in, after the required test
			probeNames.dns53:   "no answer within the 8.5 s left of the 10 s probe budget: context deadline exceeded",
			probeNames.dns4053: "connection refused",
			probeNames.quic:    "no answer within the 8.5 s left of the 10 s probe budget: context deadline exceeded",
		}
		connect.AssertEqual(t, len(outcome.refusedCarriers), len(wantErrors))
		for _, refusedCarrier := range outcome.refusedCarriers {
			probe := fmt.Sprintf("%s:%d", refusedCarrier.Carrier, refusedCarrier.Port)
			if refusedCarrier.Error != wantErrors[probe] {
				t.Fatalf("%s was left out for %q, want %q", probe, refusedCarrier.Error, wantErrors[probe])
			}
		}
	})
}
