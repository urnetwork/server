// Quality publication is independent of optional exit-location evidence.
package prober

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Acknowledged website evidence succeeds without issuing a location write.
func TestHealthAcknowledgedWithoutExitSucceeds(t *testing.T) {
	reporter := &stubHealthReporter{}
	submitter := &stubSubmitter{}
	p := healthProber(func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
		return &egresshealth.Result{Total: 1, OkCount: 1}, nil
	}, reporter)
	p.Submit = submitter
	if err := p.ProbeOne(context.Background(), provider("synthetic-provider")); err != nil {
		t.Fatalf("acknowledged quality required an exit: %v", err)
	}
	if reporter.calls != 1 || submitter.calls != 0 {
		t.Fatalf("health=%d location=%d, want 1/0", reporter.calls, submitter.calls)
	}
}

// An unsupported or failed health ingest cannot be counted as measured success.
func TestHealthPublicationFailureFailsAttempt(t *testing.T) {
	for _, publishErr := range []error{errors.New("synthetic ingest unavailable"), egresshealth.ErrUnsupported} {
		p := healthProber(healthy, &stubHealthReporter{err: publishErr})
		if err := p.ProbeOne(context.Background(), provider("synthetic-provider")); !errors.Is(err, publishErr) {
			t.Fatalf("publication %v reported as success: %v", publishErr, err)
		}
	}
}

// Optional location is independent whether its reporter is absent or rejects.
func TestHealthAcknowledgementSurvivesOptionalLocationFailure(t *testing.T) {
	for _, submitter := range []Submitter{nil, &stubSubmitter{err: errors.New("synthetic geo unavailable")}} {
		p := healthProber(healthy, &stubHealthReporter{})
		p.Submit = submitter
		if err := p.ProbeOne(context.Background(), provider("synthetic-provider")); err != nil {
			t.Fatalf("optional geo revoked health: %v", err)
		}
	}
}

// A reporter accepting an empty payload is not acknowledged website evidence.
func TestEmptyHealthEvidenceCannotSucceed(t *testing.T) {
	reporter := &stubHealthReporter{}
	p := healthProber(func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
		return &egresshealth.Result{}, nil
	}, reporter)
	if err := p.ProbeOne(context.Background(), provider("synthetic-provider")); !errors.Is(err, ErrNotMeasured) || reporter.calls != 0 {
		t.Fatalf("empty evidence accepted: err=%v reports=%d", err, reporter.calls)
	}
}
