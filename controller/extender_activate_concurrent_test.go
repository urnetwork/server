package controller

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
)

// The optional carriers of an activation against the time they take to
// answer (C2a, connect#230). An extender's udp carriers answered 2.4-4.3 s
// after the probe's first hello, inside the 10 s probe budget. The optional
// carriers were probed one after the other, each dns port under a 3 s window
// of its own (L2), so a port that answered after 3 s was left out as if
// nothing listened on it, and dns ports with nothing bound spent the budget
// of the quic carrier probed after them. These run in a synctest bubble on the
// activation's real clock: time is virtual, so a probe that answers 4 s in
// does so at once and to the instant, and probes that run together overlap
// as they would in production.

// What a fake extender that answers after d does: it waits d on the clock, or
// until its probe's context ends, whichever is first.
func testAnswerAfter(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// What a carrier with nothing bound does: a udp port gives no answer to fail
// fast on, so the probe waits until its context ends.
func testNeverAnswer(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Judges an ipv4 activation that offers the given carriers, with both dns
// ports, on the activation's real clock. Run it inside a synctest bubble.
func (self *testActivationProbes) judgeOnTheRealClock(ctx context.Context, carriers ...string) *extenderActivationOutcome {
	return probeExtenderActivation(
		ctx,
		newExtenderActivationClock(),
		self.activationProbes(),
		testActivationTcpChallengeTimeout,
		carriers,
		testAcceptanceTcpPort,
		testAcceptanceUdpPort,
		testAcceptanceDnsPorts,
		4,
	)
}

// A dns port that answers 4 s in, with 8.5 s of the probe budget left, is
// published. Under a 3 s window of its own it was left out with "no answer
// within 3 s", which reads as if nothing listened on it (connect#230).
func TestExtenderActivationGivesASlowDnsPortTheRestOfTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probeNames := testOptionalCarrierProbes
		probes := newTestActivationProbes()
		probes.answer = func(ctx context.Context, probe string) error {
			switch probe {
			case testTimingTcpProbe:
				return testAnswerAfter(ctx, 500*time.Millisecond)
			case probeNames.dns4053:
				// a slow first reply, as captured on connect#230
				return testAnswerAfter(ctx, 4*time.Second)
			}
			return nil
		}
		probes.forwardAnswer = func(ctx context.Context) error {
			return testAnswerAfter(ctx, time.Second)
		}
		outcome := probes.judgeOnTheRealClock(context.Background(), connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns)
		if outcome.refusal != "" {
			t.Fatalf("the activation was refused: %s", outcome.refusal)
		}
		for _, refusedCarrier := range outcome.refusedCarriers {
			t.Fatalf(
				"%s:%d answered 4 s in with 8.5 s of the probe budget left, and was left out: %s",
				refusedCarrier.Carrier,
				refusedCarrier.Port,
				refusedCarrier.Error,
			)
		}
		connect.AssertEqual(t, outcome.dnsPorts, []int{53, 4053})
		connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns})
	})
}

// Two dns ports with nothing bound no longer spend the budget of the quic
// carrier. One after the other, each under its own 3 s, they took 6 s, and a
// quic carrier that answers 4 s in was probed 7.5 s into the budget and left
// out with 2.5 s to answer in (the first quic reply on connect#230 came 4.27 s
// after the hello). Probed together, no carrier waits on another, and quic
// has all of the 8.5 s left after the required test.
func TestExtenderActivationDeadDnsPortsDoNotStarveQuic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probeNames := testOptionalCarrierProbes
		probes := newTestActivationProbes()
		probes.answer = func(ctx context.Context, probe string) error {
			switch probe {
			case testTimingTcpProbe:
				return testAnswerAfter(ctx, 500*time.Millisecond)
			case probeNames.dns53, probeNames.dns4053:
				return testNeverAnswer(ctx)
			case probeNames.quic:
				return testAnswerAfter(ctx, 4*time.Second)
			}
			return nil
		}
		probes.forwardAnswer = func(ctx context.Context) error {
			return testAnswerAfter(ctx, time.Second)
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
		if refused := testRefusedCarrierNames(t, outcome); !slices.Equal(refused, []string{probeNames.dns53, probeNames.dns4053}) {
			t.Fatalf("refused carriers = %v, want only the two dns ports with nothing bound", refused)
		}
		connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic})
	})
}

// A dns port with nothing bound no longer takes the time of the port after
// it. The tcp challenge takes 8 s and leaves 2 s: dns:53 waits them out while
// dns:4053, which answers 1 s in, is published. One after the other, dns:53
// spent the 2 s and dns:4053 was probed with nothing left.
func TestExtenderActivationDeadDnsPortDoesNotStarveTheNextPort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probeNames := testOptionalCarrierProbes
		probes := newTestActivationProbes()
		probes.answer = func(ctx context.Context, probe string) error {
			switch probe {
			case testTimingTcpProbe:
				return testAnswerAfter(ctx, ExtenderActivateProbeTimeout-2*time.Second)
			case probeNames.dns53:
				return testNeverAnswer(ctx)
			case probeNames.dns4053:
				return testAnswerAfter(ctx, time.Second)
			}
			return nil
		}
		outcome := probes.judgeOnTheRealClock(context.Background(), connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns)
		if outcome.refusal != "" {
			t.Fatalf("the activation was refused: %s", outcome.refusal)
		}
		connect.AssertEqual(t, outcome.dnsPorts, []int{4053})
		if refused := testRefusedCarrierNames(t, outcome); !slices.Equal(refused, []string{probeNames.dns53}) {
			t.Fatalf("refused carriers = %v, want only dns:53", refused)
		}
	})
}

// Probing the optional carriers together never stretches an activation, and
// the required test alone still decides it. With no optional carrier
// answering, the judging ends with the shared budget, 10 s after it started,
// and the activation is accepted on the tcp carrier and the forward.
func TestExtenderActivationOptionalCarriersEndWithTheSharedBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probeNames := testOptionalCarrierProbes
		probes := newTestActivationProbes()
		probes.answer = func(ctx context.Context, probe string) error {
			switch probe {
			case testTimingTcpProbe:
				return testAnswerAfter(ctx, 500*time.Millisecond)
			case probeNames.dns53, probeNames.dns4053, probeNames.quic:
				return testNeverAnswer(ctx)
			}
			return nil
		}
		probes.forwardAnswer = func(ctx context.Context) error {
			return testAnswerAfter(ctx, time.Second)
		}
		start := time.Now()
		outcome := probes.judgeOnTheRealClock(
			context.Background(),
			connect.ExtenderCarrierTcp,
			connect.ExtenderCarrierQuic,
			connect.ExtenderCarrierDns,
		)
		if elapsed := time.Since(start); elapsed != ExtenderActivateProbeTimeout {
			t.Fatalf("the judging took %s, want exactly the %s probe budget", elapsed, ExtenderActivateProbeTimeout)
		}
		if outcome.refusal != "" {
			t.Fatalf("the activation was refused for optional carriers: %s", outcome.refusal)
		}
		connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierTcp})
		connect.AssertEqual(t, len(outcome.refusedCarriers), 3)
	})
}
