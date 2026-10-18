// URL turns publish individually and are re-admitted by the durable due queue.
package work

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func testUrlProbePass() (*providerEgressProbePass, *ProviderEgressProbeArgs, *recordingEgressProbeIngest) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("example.test"), 0)
	inner := newRecordingEgressProbeIngest()
	pass := &providerEgressProbePass{
		urlProbes: true,
		loadPool: func(context.Context) (*egresshealth.Pool, error) {
			return &egresshealth.Pool{Destinations: []egresshealth.Destination{{Name: "synthetic-site", Url: "https://site.example/", Class: egresshealth.ClassSite}}}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) { return nil, nil },
		fullSink: testFullBatchSink(inner),
	}
	return pass, args, inner
}

func testUrlProbeSecurityOnlyResult() *egresshealth.Result {
	now := time.Now()
	destination := egresshealth.Destination{Name: "synthetic-page", Url: "https://synthetic.example/page"}
	return &egresshealth.Result{NotMeasured: 1, UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
		PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
		Destination: destination, MeasuredAt: now, ContentMatcherVersion: 1,
		Security: []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: now, TlsAuthenticated: true}},
	}}
}

func TestUrlProbeDueCanReadmitAProviderAfterPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, _ := testUrlProbePass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 1, 1
		calls := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			if calls > 2 {
				return nil, nil
			}
			if calls == 2 {
				time.Sleep(time.Minute)
			}
			return testDueProviders("same-provider"), nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			for _, provider := range providers {
				if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, testHealthRun(1, 0)); err != nil {
					return prober.Summary{}, err
				}
				if err := options.Attempts.ReportAttempt(ctx, provider.ClientId, ""); err != nil {
					return prober.Summary{}, err
				}
			}
			return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
		defer cancel()
		result, err := pass.run(ctx, args)
		if err != nil || result.Attempted != 2 || result.Submitted != 2 || calls != 3 {
			t.Fatalf("durably requeued URL provider was suppressed: result=%+v due_reads=%d error=%v", result, calls, err)
		}
	})
}

func TestUrlProbePublishesBeforeUnrelatedSlowTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, inner := testUrlProbePass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 2, 2
		selected := false
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			if selected {
				return nil, nil
			}
			selected = true
			return testDueProviders("fast-provider", "slow-provider"), nil
		}
		slowRelease := make(chan struct{})
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if !options.UrlProbe || options.LoadAttempts != 1 || options.AllDestinations {
				t.Error("URL turn retained sampled retry geometry")
			}
			for _, provider := range providers {
				if provider.ClientId == "slow-provider" {
					<-slowRelease
				}
				if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, testHealthRun(1, 0)); err != nil {
					return prober.Summary{}, err
				}
				if err := options.Attempts.ReportAttempt(ctx, provider.ClientId, ""); err != nil {
					return prober.Summary{}, err
				}
			}
			return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			if result, err := pass.run(ctx, args); err != nil || result.Submitted != 2 {
				t.Errorf("URL turn release: result=%+v error=%v", result, err)
			}
		}()
		synctest.Wait()
		inner.stateLock.Lock()
		fast := inner.health["fast-provider"]
		inner.stateLock.Unlock()
		if fast == nil || fast.Total != 1 || fast.OkCount != 0 {
			t.Error("an unrelated slow URL delayed or discarded the measured URL error")
		}
		close(slowRelease)
		<-done
	})
}

func TestUrlProbeMissingCatalogDoesNotClaimOrOpen(t *testing.T) {
	for _, catalogErr := range []error{nil, errors.New("synthetic catalog unavailable")} {
		pass, args, _ := testUrlProbePass()
		pass.loadPool = func(context.Context) (*egresshealth.Pool, error) { return nil, catalogErr }
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			t.Fatal("claimed providers without a catalog")
			return nil, nil
		}
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			t.Fatal("opened a tunnel without a catalog")
			return prober.Summary{}, nil
		}
		if _, err := pass.run(context.Background(), args); err == nil {
			t.Fatal("missing catalog was accepted")
		}
	}
}

// The turn's provisional reporter is buffered. Its final release must retain
// independently authenticated TLS even when no quality trial was measured.
func TestUrlProbePublishesSecurityOnlyReceiptWithoutQualityCredit(t *testing.T) {
	pass, args, inner := testUrlProbePass()
	args.UrlProbe.Limit, args.UrlProbe.Concurrency = 1, 1
	selected := false
	pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
		if selected {
			return nil, nil
		}
		selected = true
		return []ingest.DueProvider{{ClientId: "synthetic-provider", CycleStartedAt: time.Unix(10, 0)}}, nil
	}
	pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
		p := &prober.Prober{
			Open: func(context.Context, string) (*http.Client, func() error, error) {
				return &http.Client{}, func() error { return nil }, nil
			},
			Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
				return testUrlProbeSecurityOnlyResult(), nil
			},
			HealthResults: options.HealthResults, Attempts: options.Attempts,
		}
		return (&prober.Scheduler{Prober: p, Concurrency: 1}).Run(ctx, providers), nil
	}
	result, err := pass.run(t.Context(), args)
	if err != nil || result.Attempted != 1 || result.Submitted != 0 || result.Failed != 1 || result.UrlNotMeasured != 1 {
		t.Fatalf("security-only turn became a quality trial: result%+v error%v", result, err)
	}
	if receipt := inner.health["synthetic-provider"]; receipt == nil || receipt.Total != 0 || receipt.OkCount != 0 || receipt.UrlProbeEvidence == nil {
		t.Fatalf("release dropped independent TLS authentication: %+v", receipt)
	}
}
