package work

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The value of the gauge family name exports with exactly labels, as a
// scrape of this process would read it.
func testingGatheredTlsJudgeGauge(t testing.TB, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			pairs := metric.GetLabel()
			if len(pairs) != len(labels) {
				continue
			}
			matches := true
			for _, pair := range pairs {
				if value, ok := labels[pair.GetName()]; !ok || value != pair.GetValue() {
					matches = false
				}
			}
			if matches && metric.GetGauge() != nil {
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// A finding stays pending, excluding nobody, until a judge run decides it, so
// the run itself, on the task's own path, exports what it leaves behind: the
// unresolved findings by verdict, the pending ones it left undecided although
// a direct check could still decide them, and when it judged. A run without
// a pool judges nothing and moves none of them.
func TestProviderEgressTlsJudgeExportsWhatEachJudgedRunLeaves(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Millisecond)
		fail := func(name string, target string, at time.Time) {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			destination := egresshealth.Destination{Name: name, Class: egresshealth.ClassSite, Url: target}
			model.SetProviderEgressHealth(ctx, &model.ProviderEgressHealth{
				RunId: server.NewId(), ClientId: clientId, MeasuredAt: at, Total: 1, TLSAuthenticationFailure: true,
				UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
					PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
					Destination: destination, MeasuredAt: at, ContentMatcherVersion: 1, FailureStage: "request",
					Security: []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsFailure: true}},
				},
			})
		}
		// a provider's own interception, a host the server cannot reach, and
		// a failure past the direct window
		fail("intercepted-site", "https://intercepted.example/", now.Add(-time.Minute))
		fail("unreachable-site", "https://unreachable.example/", now.Add(-time.Minute))
		fail("aged-site", "https://aged.example/", now.Add(-2*time.Hour))
		sources := &providerEgressTlsJudgeSources{
			served: func(context.Context) (map[string]bool, error) {
				return map[string]bool{"intercepted-site": true, "unreachable-site": true, "aged-site": true}, nil
			},
			siteSettings: func() (*model.ProviderEgressSiteSettings, error) {
				return model.DefaultProviderEgressSiteSettings(), nil
			},
			direct: func(_ context.Context, host string) model.ProviderEgressTlsDirectOutcome {
				if host == "intercepted.example" {
					return model.ProviderEgressTlsDirectVerified
				}
				return model.ProviderEgressTlsDirectInconclusive
			},
		}
		if _, err := testingRunTlsJudge(ctx, now, sources); err != nil {
			t.Fatal(err)
		}
		for verdict, want := range map[string]float64{"pending": 2, "provider": 1, "site": 0, "place": 0, "retired": 0} {
			if got, ok := testingGatheredTlsJudgeGauge(t, "urnetwork_egress_tls_findings", map[string]string{"verdict": verdict}); !ok || got != want {
				t.Errorf("exported %s findings: %v (exported=%t), want %v", verdict, got, ok, want)
			}
		}
		if got, ok := testingGatheredTlsJudgeGauge(t, "urnetwork_egress_tls_undecided_findings", map[string]string{}); !ok || got != 1 {
			t.Errorf("exported undecided findings: %v (exported=%t), want the one a direct check could still decide", got, ok)
		}
		for _, reason := range []string{"error", "budget", "limit"} {
			if got, ok := testingGatheredTlsJudgeGauge(t, "urnetwork_egress_tls_place_withheld_pairs", map[string]string{"reason": reason}); !ok || got != 0 {
				t.Errorf("exported %s place-withheld pairs: %v (exported=%t), want 0", reason, got, ok)
			}
		}
		judgedAt := float64(now.UnixMilli()) / 1000
		if got, ok := testingGatheredTlsJudgeGauge(t, "urnetwork_egress_tls_judge_last_judged_timestamp_seconds", map[string]string{}); !ok || math.Abs(got-judgedAt) > 0.001 {
			t.Fatalf("exported last judged run: %v (exported=%t), want %v", got, ok, judgedAt)
		}

		unavailable := *sources
		unavailable.served = func(context.Context) (map[string]bool, error) {
			return nil, errors.New("synthetic pool refusal")
		}
		if _, err := testingRunTlsJudge(ctx, now.Add(5*time.Minute), &unavailable); err != nil {
			t.Fatal(err)
		}
		if got, _ := testingGatheredTlsJudgeGauge(t, "urnetwork_egress_tls_judge_last_judged_timestamp_seconds", map[string]string{}); math.Abs(got-judgedAt) > 0.001 {
			t.Fatalf("a run that judged nothing moved the last judged run to %v", got)
		}
	})
}
