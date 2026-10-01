// Independent URL security evidence survives an unmeasured quality outcome.
package prober

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

func securityOnlyUrlResult(tlsFailure bool) *egresshealth.Result {
	now := time.Unix(100, 0)
	destination := egresshealth.Destination{Name: "synthetic-page", Url: "https://synthetic.example/page"}
	return &egresshealth.Result{NotMeasured: 1, TlsAuthenticationFailure: tlsFailure,
		UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
			PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
			Destination: destination, MeasuredAt: now, ContentMatcherVersion: 1,
			ContentClassification: "unmeasured", PerformanceClassification: "unmeasured",
			Security: []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: now,
				TlsFailure: tlsFailure, TlsAuthenticated: !tlsFailure}},
		}}
}

func TestUrlProbeSecurityOnlyEvidenceIsPublishedWithoutQualityCredit(t *testing.T) {
	for _, tlsFailure := range []bool{false, true} {
		reporter := &stubHealthReporter{}
		attempts := &stubAttemptReporter{}
		p := healthProber(answers(securityOnlyUrlResult(tlsFailure)), reporter)
		p.Attempts = attempts
		candidate := provider("synthetic-provider")
		candidate.CycleStartedAt = time.Unix(10, 0)
		scheduler := &Scheduler{Prober: p, Concurrency: 1, CacheTtl: time.Hour}
		for turn := range 2 {
			summary := scheduler.Run(t.Context(), []Provider{candidate})
			if summary.Attempted != 1 || summary.Failed != 1 || summary.NotMeasured != 1 || summary.Submitted != 0 || summary.Skipped != 0 {
				t.Fatalf("security-only receipt became quality evidence or cached success: %+v", summary)
			}
			if reporter.calls != turn+1 || reporter.last == nil || reporter.last.Total != 0 || reporter.last.OkCount != 0 ||
				!reporter.last.CycleStartedAt.Equal(candidate.CycleStartedAt) || reporter.last.TlsAuthenticationFailure != tlsFailure {
				t.Fatalf("independent security receipt was lost or changed: calls%d receipt%+v", reporter.calls, reporter.last)
			}
		}
		if failures := attempts.failures(); len(failures) != 2 || failures[0] != FailureNotMeasured || failures[1] != FailureNotMeasured {
			t.Fatalf("security-only turn lost unmeasured retry semantics: %v", failures)
		}
	}
}

func TestUrlProbeSecurityOnlyPublicationFailureIsNotAcknowledged(t *testing.T) {
	publishErr := errors.New("synthetic security ingest unavailable")
	reporter := &stubHealthReporter{err: publishErr}
	attempts := &stubAttemptReporter{}
	p := healthProber(answers(securityOnlyUrlResult(false)), reporter)
	p.Attempts = attempts
	if err := p.ProbeOne(context.Background(), provider("synthetic-provider")); !errors.Is(err, publishErr) {
		t.Fatalf("security publication failure was silently lost: %v", err)
	}
	if failures := attempts.failures(); reporter.calls != 1 || len(failures) != 1 || failures[0] != FailureSubmit {
		t.Fatalf("security-only publication was falsely acknowledged: calls%d failures%v", reporter.calls, failures)
	}
}

func TestUrlProbeInvalidSecurityOnlyEvidenceIsNotPublished(t *testing.T) {
	result := securityOnlyUrlResult(false)
	result.UrlProbeEvidence.Security[0].TlsAuthenticated = false
	reporter := &stubHealthReporter{}
	p := healthProber(answers(result), reporter)
	if err := p.ProbeOne(t.Context(), provider("synthetic-provider")); err == nil || reporter.calls != 0 {
		t.Fatalf("unvalidated security evidence was published: err%v calls%d", err, reporter.calls)
	}
}

func TestUrlProbeAdmissionDoesNotMutateCheckerEvidence(t *testing.T) {
	result := securityOnlyUrlResult(false)
	reporter := &stubHealthReporter{}
	p := healthProber(answers(result), reporter)
	candidate := provider("synthetic-provider")
	candidate.CycleStartedAt = time.Unix(10, 0)
	if err := p.ProbeOne(t.Context(), candidate); !errors.Is(err, ErrNotMeasured) {
		t.Fatal(err)
	}
	if !result.CycleStartedAt.IsZero() || reporter.last == result || !reporter.last.CycleStartedAt.Equal(candidate.CycleStartedAt) {
		t.Fatal("per-turn admission mutated reusable checker evidence")
	}
}
