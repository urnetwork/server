package work

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

type urlClaimCountrySink struct {
	*testingUrlCompletionIngest
	delay time.Duration
}

func (self *urlClaimCountrySink) SubmitEgressHealth(ctx context.Context, id string, result *egresshealth.Result) error {
	time.Sleep(self.delay)
	return self.recordingEgressProbeIngest.SubmitEgressHealth(ctx, id, result)
}

func (self *urlClaimCountrySink) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	time.Sleep(self.delay)
	return self.testingUrlCompletionIngest.ReportUrlProbeCompletion(ctx, completion)
}

func newUrlClaimCountrySink() *urlClaimCountrySink {
	return &urlClaimCountrySink{testingUrlCompletionIngest: &testingUrlCompletionIngest{
		recordingEgressProbeIngest: newRecordingEgressProbeIngest(),
	}}
}

// Drive the actual 64-owner scheduler, buffered publication, and metrics
// adapter. A loaded eight-slot label datastore takes three seconds per query;
// it must not consume the measured-result publication budget. Real admission,
// evidence, and completion accounting stay on their ordinary paths. Delays
// are synthetic and this is not a claim about Main's current stage costs.
func TestUrlProbeClaimCountryLoadedPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, _ := testUrlProbePass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 64, 64
		inner := newUrlClaimCountrySink()
		inner.delay = 500 * time.Millisecond
		var lookups atomic.Int32
		lookupSlots := make(chan struct{}, 8)
		pass.fullSink = newEgressProbeMetricsReporter(inner, func(context.Context, string) string {
			lookups.Add(1)
			lookupSlots <- struct{}{}
			time.Sleep(3 * time.Second)
			<-lookupSlots
			return "jp"
		})
		countries := []string{" US ", "de", ""}
		providers := make([]ingest.DueProvider, 256)
		for i := range providers {
			providers[i] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-claim-country-%03d", i),
				CountryCode: countries[i%len(countries)], ClaimOrdinal: int64(i + 1), ClaimedAt: time.Now()}
		}
		next := 0
		pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			time.Sleep(10 * time.Millisecond)
			end := min(next+limit, len(providers))
			due := providers[next:end]
			next = end
			return due, nil
		}
		var active, peak atomic.Int32
		pass.runFull = func(ctx context.Context, providers []prober.Provider, opts fleetprobe.FullOptions) (prober.Summary, error) {
			count := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); old < count && !peak.CompareAndSwap(old, count); old = peak.Load() {
			}
			time.Sleep(5 * time.Second)
			p := providers[0]
			summary := prober.Summary{Attempted: 1}
			failure := ""
			if p.ClaimOrdinal%4 == 0 {
				failure = prober.FailureNotMeasured
				summary.Failed, summary.NotMeasured = 1, 1
			} else {
				ok := int(p.ClaimOrdinal % 2)
				if err := opts.HealthResults.SubmitEgressHealth(ctx, p.ClientId, testHealthRun(1, ok)); err != nil {
					return summary, err
				}
				summary.Submitted = 1
			}
			err := opts.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{
				ClientId: p.ClientId, ClaimOrdinal: p.ClaimOrdinal, CompletedAt: time.Now(), ProbeFailure: failure, AllowPacing: true,
			})
			return summary, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
		defer cancel()
		start := time.Now()
		result, err := pass.run(ctx, args)
		elapsed := time.Since(start)
		if err != nil || result == nil || result.UrlDue != 256 || result.Attempted != 256 || result.Submitted != 192 || result.UrlNotMeasured != 64 {
			t.Fatalf("loaded publication changed accounting: result=%+v err=%v", result, err)
		}
		if lookups.Load() != 0 || elapsed > 25*time.Second {
			t.Errorf("country-only datastore consumed URL capacity: queries=%d elapsed=%s; want zero queries and at most 25s", lookups.Load(), elapsed)
		}
		if peak.Load() != 64 || active.Load() != 0 || len(inner.health) != 192 || len(inner.completions) != 256 {
			t.Fatalf("loaded publication lost bounded ownership or receipts: peak=%d active=%d health=%d completion=%d",
				peak.Load(), active.Load(), len(inner.health), len(inner.completions))
		}
		seen := map[string]bool{}
		for _, completion := range inner.completions {
			if seen[completion.ClientId] || completion.ClaimOrdinal < 1 || completion.ClaimOrdinal > 256 || !completion.AllowPacing {
				t.Fatal("loaded publication duplicated or rewrote completion identity")
			}
			seen[completion.ClientId] = true
			health := inner.health[completion.ClientId]
			if completion.ClaimOrdinal%4 == 0 {
				if health != nil || completion.ProbeFailure != prober.FailureNotMeasured {
					t.Fatal("setup-only completion acquired measured quota evidence")
				}
			} else if health == nil || health.Total != 1 || health.OkCount != int(completion.ClaimOrdinal%2) || completion.ProbeFailure != "" {
				t.Fatal("measured success or error changed during country attribution")
			}
		}
		t.Logf("synthetic loaded publication: turns=%d measured=%d label_queries=%d elapsed=%s peak=%d", result.Attempted, result.Submitted, lookups.Load(), elapsed, peak.Load())
	})
}

// A provider can be readmitted with a new claim place during one pass. The
// current claim owns its label, including an explicitly unknown country;
// earlier cached location and a later unrelated provider cannot replace it.
func TestUrlProbeClaimCountryUsesCurrentClaimPlace(t *testing.T) {
	pass, args, _ := testUrlProbePass()
	inner := newUrlClaimCountrySink()
	var lookups int
	pass.fullSink = newEgressProbeMetricsReporter(inner, func(context.Context, string) string {
		lookups++
		return "jp"
	})
	pass.fullSink.rememberCountry("synthetic-moving-provider", "ca")
	pass.runFull = func(ctx context.Context, providers []prober.Provider, opts fleetprobe.FullOptions) (prober.Summary, error) {
		p := providers[0]
		if err := opts.HealthResults.SubmitEgressHealth(ctx, p.ClientId, testHealthRun(1, 0)); err != nil {
			return prober.Summary{}, err
		}
		err := opts.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{
			ClientId: p.ClientId, ClaimOrdinal: p.ClaimOrdinal, CompletedAt: time.Now(), AllowPacing: true,
		})
		return prober.Summary{Attempted: 1, Submitted: 1}, err
	}
	for i, country := range []string{" US ", "de", "", "private-host.example"} {
		want := []string{"us", "de", "unknown", "unknown"}[i]
		attempt := egressProbeAttemptsTotal.WithLabelValues("ok", want)
		health := egressProbeHealthResultsTotal.WithLabelValues(want, "dead")
		beforeAttempt, beforeHealth := testutil.ToFloat64(attempt), testutil.ToFloat64(health)
		outcome := pass.runFullBatch(t.Context(), args, nil, nil, []ingest.DueProvider{{
			ClientId: "synthetic-moving-provider", CountryCode: country, ClaimOrdinal: int64(i + 1),
		}})
		if outcome.err != nil || outcome.summary.Submitted != 1 || testutil.ToFloat64(attempt) != beforeAttempt+1 || testutil.ToFloat64(health) != beforeHealth+1 {
			t.Errorf("claim country %q did not label both health and completion as %q: outcome=%+v", country, want, outcome)
		}
	}
	if lookups != 0 || len(inner.completions) != 4 {
		t.Fatalf("claim country fetched replacement labels or lost completion: queries=%d completions=%d", lookups, len(inner.completions))
	}
}

func TestUrlProbeClaimCountryDoesNotLabelUnrelatedOrManualProvider(t *testing.T) {
	for _, urlProbe := range []bool{true, false} {
		pass, args, _ := testUrlProbePass()
		pass.urlProbes = urlProbe
		inner := newUrlClaimCountrySink()
		lookups := map[string]int{}
		pass.fullSink = newEgressProbeMetricsReporter(inner, func(_ context.Context, id string) string {
			lookups[id]++
			return "jp"
		})
		pass.runFull = func(ctx context.Context, _ []prober.Provider, _ fleetprobe.FullOptions) (prober.Summary, error) {
			want := "jp"
			if urlProbe {
				want = "us"
			}
			if got := pass.fullSink.country(ctx, "synthetic-owned"); got != want {
				t.Errorf("owned country=%s want=%s URL=%t", got, want, urlProbe)
			}
			if got := pass.fullSink.country(ctx, "synthetic-unrelated"); got != "jp" {
				t.Errorf("unrelated provider inherited another claim's country: %s", got)
			}
			return prober.Summary{}, nil
		}
		outcome := pass.runFullBatch(t.Context(), args, nil, nil, []ingest.DueProvider{{
			ClientId: "synthetic-owned", CountryCode: "US", ClaimOrdinal: 1,
		}})
		wantOwned := 1
		if urlProbe {
			wantOwned = 0
		}
		if outcome.err != nil || lookups["synthetic-owned"] != wantOwned || lookups["synthetic-unrelated"] != 1 {
			t.Fatalf("country provenance crossed its owner: URL=%t lookups=%v err=%v", urlProbe, lookups, outcome.err)
		}
	}
}

// Late due responses must release their claim identities even when the
// optional label database is unavailable. They still supply no measurements,
// no provider-failure pacing, and no credit toward the rolling quota.
func TestUrlProbeClaimCountryUnstartedCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, _ := urlAdmissionPass()
		inner := newUrlClaimCountrySink()
		var lookups atomic.Int32
		pass.fullSink = newEgressProbeMetricsReporter(inner, func(context.Context, string) string {
			lookups.Add(1)
			panic("synthetic unavailable optional label database")
		})
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			time.Sleep(591 * time.Second)
			return []ingest.DueProvider{{ClientId: "synthetic-late-us", CountryCode: "US", ClaimOrdinal: 1},
				{ClientId: "synthetic-late-unknown", ClaimOrdinal: 2}}, nil
		}
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			t.Error("late claim opened a tunnel")
			return prober.Summary{}, nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		result, err := pass.run(ctx, args)
		if err != nil || result == nil || result.Attempted != 0 || result.Submitted != 0 || result.UrlDue != 2 || lookups.Load() != 0 || len(inner.completions) != 2 || len(inner.health) != 0 {
			t.Fatalf("late claim publication depends on optional label lookup: result=%+v err=%v queries=%d completions=%d", result, err, lookups.Load(), len(inner.completions))
		}
		for _, completion := range inner.completions {
			if completion.AllowPacing || completion.ProbeFailure != prober.FailureHealthNotRun {
				t.Fatal("unstarted claim acquired measured or provider verdict authority")
			}
		}
	})
}
