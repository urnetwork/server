package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Real activations, probed against the fixture's in-process extender, are
// counted where they ended, with the caller's family, and every step their
// probe ran is timed and listed in the answer (connect#230). Before, the
// operator counted no activation by how it ended, and an answer said nothing
// of how long its probe took.
func TestExtenderActivateCountsItsOutcomeAndTimesItsSteps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		metrics := newExtenderActivationMetrics(prometheus.NewRegistry())
		settings := DefaultExtenderActivateSettings()
		settings.metrics = metrics
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		// nothing listens on the tcp carrier, which refuses at once
		refusedArgs := fixture.activateArgs()
		refusedArgs.TcpPort = testClosedTcpPort(t)
		refused, err := extenderActivate(refusedArgs, clientSession, settings)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, refused.Activated, false)
		if !strings.HasPrefix(refused.Error, "the tcp carrier failed after ") {
			t.Fatalf("error = %q, want the tcp carrier and how long it took", refused.Error)
		}
		connect.AssertEqual(t, len(refused.ProbeSteps), 1)
		refusedStep := refused.ProbeSteps[0]
		connect.AssertEqual(t, refusedStep.Name, connect.ExtenderCarrierTcp)
		connect.AssertEqual(t, refusedStep.IpVersion, 4)
		connect.AssertEqual(t, refusedStep.Port, refusedArgs.TcpPort)
		connect.AssertEqual(t, refusedStep.Outcome, extenderProbeStepOutcomeError)
		if refusedStep.BudgetLeftMs <= 0 || ExtenderActivateProbeTimeout.Milliseconds() < refusedStep.BudgetLeftMs {
			t.Fatalf("the tcp step started with %d ms of the budget left", refusedStep.BudgetLeftMs)
		}
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("tcp", "error", "ipv4")), float64(1))
		connect.AssertEqual(t, testActivationStepHistogram(t, metrics, "tcp", "error").GetSampleCount(), uint64(1))

		// the tcp carrier and the forward through it answer
		activatedArgs := fixture.activateArgs()
		activatedArgs.Carriers = []string{connect.ExtenderCarrierTcp}
		activated, err := extenderActivate(activatedArgs, clientSession, settings)
		if err != nil {
			t.Fatal(err)
		}
		if !activated.Activated {
			t.Fatalf("the activation was refused: %s", activated.Error)
		}
		connect.AssertEqual(t, testProbeStepNamesAndOutcomes(activated.ProbeSteps), []string{"tcp ok", "forward ok"})
		for _, step := range activated.ProbeSteps {
			connect.AssertEqual(t, step.IpVersion, 4)
			connect.AssertEqual(t, step.Port, fixture.tcpPort)
		}
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("store", "activated", "ipv4")), float64(1))
		connect.AssertEqual(t, testActivationStepHistogram(t, metrics, "tcp", "ok").GetSampleCount(), uint64(1))
		connect.AssertEqual(t, testActivationStepHistogram(t, metrics, "forward", "ok").GetSampleCount(), uint64(1))

		// a request refused before any budget is counted too, by the family
		// of its caller
		malformedArgs := fixture.activateArgs()
		malformedArgs.Carriers = []string{connect.ExtenderCarrierQuic}
		malformed, err := extenderActivate(malformedArgs, clientSession, settings)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, malformed.Activated, false)
		connect.AssertEqual(t, len(malformed.ProbeSteps), 0)
		connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("request", "refused", "ipv4")), float64(1))
	})
}

func testProbeStepNamesAndOutcomes(steps []*ExtenderProbeStep) []string {
	namesAndOutcomes := []string{}
	for _, step := range steps {
		namesAndOutcomes = append(namesAndOutcomes, step.Name+" "+step.Outcome)
	}
	return namesAndOutcomes
}
