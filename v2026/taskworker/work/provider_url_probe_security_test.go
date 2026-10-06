// Shared readiness failures cannot erase independently authenticated URL hops.
package work

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func TestUrlProbeReadinessKeepsSecurityWithoutAnUncertainQualityTrial(t *testing.T) {
	for _, total := range []int{0, 1} {
		inner := newRecordingEgressProbeIngest()
		reporter := &providerEgressProbeReadinessReporter{
			egressProbeIngest: inner,
			readiness: &providerEgressProbeReadiness{minimum: 1,
				available: func(context.Context) (model.ByteCount, error) { return 0, nil }},
		}
		result := testUrlProbeSecurityOnlyResult()
		result.Total, result.NotMeasured = total, 1-total
		result.ByClass = map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Total: total}}
		result.Checks = []egresshealth.CheckResult{{Name: "synthetic-page", Class: egresshealth.ClassSite, NotMeasured: total == 0}}
		err := reporter.SubmitEgressHealth(t.Context(), "synthetic-provider", result)
		if !errors.Is(err, errProviderEgressProbeUnfunded) {
			t.Fatalf("independent publication concealed the local readiness error: %v", err)
		}
		receipt := inner.health["synthetic-provider"]
		if receipt == nil || receipt.Total != 0 || receipt.OkCount != 0 || receipt.NotMeasured != 1 || len(receipt.ByClass) != 0 ||
			receipt.UrlProbeEvidence != result.UrlProbeEvidence || receipt.TlsAuthenticationFailure || !receipt.UrlProbeEvidence.Security[0].TlsAuthenticated {
			t.Fatalf("readiness erased authentication or assigned uncertain quality credit: %+v", receipt)
		}
		if err := receipt.UrlProbeEvidence.ValidateOutcome(receipt.OkCount, receipt.Total, receipt.TlsAuthenticationFailure); err != nil {
			t.Fatal(err)
		}
		if result.Total != total || result.Checks[0].NotMeasured != (total == 0) {
			t.Fatal("security-only publication mutated the original quality evidence")
		}
	}
}

func TestUrlProbeReadinessKeepsSecurityPublicationErrors(t *testing.T) {
	publishErr := errors.New("synthetic security publication failed")
	inner := newFakeEgressProbeIngest()
	inner.healthErr = publishErr
	reporter := &providerEgressProbeReadinessReporter{
		egressProbeIngest: inner,
		readiness: &providerEgressProbeReadiness{minimum: 1,
			available: func(context.Context) (model.ByteCount, error) { return 0, nil }},
	}
	err := reporter.SubmitEgressHealth(t.Context(), "synthetic-provider", testUrlProbeSecurityOnlyResult())
	if !errors.Is(err, errProviderEgressProbeUnfunded) || !errors.Is(err, publishErr) || inner.healthCalls != 1 {
		t.Fatalf("security publication or readiness cause disappeared: calls%d error%v", inner.healthCalls, err)
	}
}

func TestUrlProbeReadinessNeverSanitizesTlsFailure(t *testing.T) {
	inner := newRecordingEgressProbeIngest()
	reporter := &providerEgressProbeReadinessReporter{
		egressProbeIngest: inner,
		readiness:         &providerEgressProbeReadiness{failure: errProviderEgressProbeUnfunded},
	}
	result := testUrlProbeSecurityOnlyResult()
	result.Total, result.NotMeasured, result.TlsAuthenticationFailure = 1, 0, true
	failed := result.UrlProbeEvidence.Security[0]
	failed.Destination.Url = "https://redirected.synthetic.example/page"
	failed.TlsAuthenticated, failed.TlsFailure = false, true
	result.UrlProbeEvidence.Security = append(result.UrlProbeEvidence.Security, failed)
	if err := reporter.SubmitEgressHealth(t.Context(), "synthetic-provider", result); err != nil {
		t.Fatal(err)
	}
	receipt := inner.health["synthetic-provider"]
	if receipt != result || !receipt.TlsAuthenticationFailure || len(receipt.UrlProbeEvidence.Security) != 2 ||
		!receipt.UrlProbeEvidence.Security[0].TlsAuthenticated || !receipt.UrlProbeEvidence.Security[1].TlsFailure {
		t.Fatal("readiness manufactured a clean security result or dropped authenticated prior hops")
	}
}

// A runner can finish after the last funded admission. Its owning pass still
// releases independently valid security, then fails with that readiness cause.
func TestUrlProbeReadinessFailureStillReleasesBufferedSecurity(t *testing.T) {
	pass, args, inner := testUrlProbePass()
	pass.readiness = &providerEgressProbeReadiness{minimum: 1,
		available: func(context.Context) (model.ByteCount, error) { return 1, nil }}
	selected := false
	pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
		if selected {
			t.Error("readiness failure admitted another turn")
			return nil, nil
		}
		selected = true
		return testDueProviders("synthetic-provider"), nil
	}
	pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
		pass.readiness.stateLock.Lock()
		pass.readiness.failure = errProviderEgressProbeUnfunded
		pass.readiness.stateLock.Unlock()
		result := testUrlProbeSecurityOnlyResult()
		result.Total, result.NotMeasured = 1, 0
		if err := options.HealthResults.SubmitEgressHealth(ctx, providers[0].ClientId, result); !errors.Is(err, errProviderEgressProbeUnfunded) {
			t.Errorf("runner concealed its shared failure: %v", err)
		}
		return prober.Summary{Attempted: 1, Failed: 1}, nil
	}
	result, err := pass.run(t.Context(), args)
	if !errors.Is(err, errProviderEgressProbeUnfunded) || result.Attempted != 1 || result.Submitted != 0 || result.Failed != 1 {
		t.Fatalf("owning pass lost local readiness or gained quality credit: result%+v error%v", result, err)
	}
	if receipt := inner.health["synthetic-provider"]; receipt == nil || receipt.Total != 0 || receipt.UrlProbeEvidence == nil || !receipt.UrlProbeEvidence.Security[0].TlsAuthenticated {
		t.Fatalf("failed pass discarded independently authenticated URL evidence: %+v", receipt)
	}
}
