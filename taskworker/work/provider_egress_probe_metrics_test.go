package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/geolocate"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
)

// fakeEgressProbeIngest records what reached the operator so the tests can
// prove the reporter forwards every submission unchanged.
type fakeEgressProbeIngest struct {
	submitted   []string
	attempts    map[string]string
	health      []string
	bandwidth   []string
	blackhole   []ingest.BlackholeCheck
	attemptErr  error
	healthCalls int
}

func newFakeEgressProbeIngest() *fakeEgressProbeIngest {
	return &fakeEgressProbeIngest{attempts: map[string]string{}}
}

func (self *fakeEgressProbeIngest) Submit(_ context.Context, providerClientId string, _ *geolocate.ConsensusLocation) error {
	self.submitted = append(self.submitted, providerClientId)
	return nil
}

func (self *fakeEgressProbeIngest) ReportAttempt(_ context.Context, providerClientId string, probeFailure string) error {
	self.attempts[providerClientId] = probeFailure
	return self.attemptErr
}

func (self *fakeEgressProbeIngest) SubmitEgressHealth(_ context.Context, providerClientId string, _ *egresshealth.Result) error {
	self.healthCalls += 1
	self.health = append(self.health, providerClientId)
	return nil
}

func (self *fakeEgressProbeIngest) ReserveBandwidth(context.Context, string, int64) error {
	return nil
}

func (self *fakeEgressProbeIngest) SubmitBandwidth(_ context.Context, providerClientId string, _ string, _ float64, _ int64) error {
	self.bandwidth = append(self.bandwidth, providerClientId)
	return nil
}

func (self *fakeEgressProbeIngest) SubmitBlackholeChecks(_ context.Context, checks []ingest.BlackholeCheck) error {
	self.blackhole = append(self.blackhole, checks...)
	return nil
}

func TestEgressProbeMetricsReporterLabelsAttemptsByOutcomeAndCountry(t *testing.T) {
	egressProbeAttemptsTotal.Reset()
	inner := newFakeEgressProbeIngest()
	lookups := 0
	reporter := newEgressProbeMetricsReporter(inner, func(_ context.Context, providerClientId string) string {
		lookups += 1
		if providerClientId == "known-de" {
			return "DE"
		}
		return ""
	})
	ctx := context.Background()

	// a located provider carries the country it just submitted
	if err := reporter.Submit(ctx, "fresh-us", &geolocate.ConsensusLocation{CountryCode: "us", CityConfident: true}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := reporter.ReportAttempt(ctx, "fresh-us", prober.FailureSubmit); err != nil {
		t.Fatalf("report: %v", err)
	}
	// a provider located on an earlier pass is looked up once and cached
	for range 2 {
		if err := reporter.ReportAttempt(ctx, "known-de", ""); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
	// a provider never located lands under unknown
	if err := reporter.ReportAttempt(ctx, "never", prober.FailureTunnel); err != nil {
		t.Fatalf("report: %v", err)
	}

	if got := testutil.ToFloat64(egressProbeAttemptsTotal.WithLabelValues("submit_failed", "us")); got != 1 {
		t.Fatalf("submit_failed/us = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbeAttemptsTotal.WithLabelValues("ok", "de")); got != 2 {
		t.Fatalf("ok/de = %v, want 2", got)
	}
	if got := testutil.ToFloat64(egressProbeAttemptsTotal.WithLabelValues("tunnel_failed", "unknown")); got != 1 {
		t.Fatalf("tunnel_failed/unknown = %v, want 1", got)
	}
	if lookups != 2 {
		t.Fatalf("durable lookups = %d, want 2 (known-de once, never once)", lookups)
	}
	if inner.attempts["fresh-us"] != prober.FailureSubmit || inner.attempts["known-de"] != "" || inner.attempts["never"] != prober.FailureTunnel {
		t.Fatalf("forwarded attempts = %v", inner.attempts)
	}
	if len(inner.submitted) != 1 || inner.submitted[0] != "fresh-us" {
		t.Fatalf("forwarded submissions = %v", inner.submitted)
	}
	if got := testutil.ToFloat64(egressProbeLocationsTotal.WithLabelValues("us", "true")); got < 1 {
		t.Fatalf("locations us/confident = %v, want >= 1", got)
	}
}

func TestEgressProbeMetricsReporterForwardsErrorsUnchanged(t *testing.T) {
	inner := newFakeEgressProbeIngest()
	inner.attemptErr = errors.New("operator down")
	reporter := newEgressProbeMetricsReporter(inner, nil)
	err := reporter.ReportAttempt(context.Background(), "x", prober.FailureLocate)
	if !errors.Is(err, inner.attemptErr) {
		t.Fatalf("err = %v, want the operator error", err)
	}
}

func TestEgressProbeMetricsReporterCountsHealthChecksByDestinationAndCountry(t *testing.T) {
	egressProbeHealthChecksTotal.Reset()
	egressProbeHealthChecksByCountryTotal.Reset()
	egressProbeHealthResultsTotal.Reset()
	egressProbeHealthTLSFailuresTotal.Reset()
	inner := newFakeEgressProbeIngest()
	reporter := newEgressProbeMetricsReporter(inner, func(context.Context, string) string { return "jp" })
	ctx := context.Background()

	res := &egresshealth.Result{
		Checks: []egresshealth.CheckResult{
			{Name: "cloudflare-dns", Class: egresshealth.ClassDNS, OK: true, Latency: 120 * time.Millisecond},
			{Name: "gstatic-204", Class: egresshealth.ClassConnectivity, OK: false, Err: "timeout"},
			{Name: "example-site", Class: egresshealth.ClassSite, OK: false, TLSAuthenticationFailure: true},
		},
		OKCount: 1,
		Total:   3,
	}
	if err := reporter.SubmitEgressHealth(ctx, "p1", res); err != nil {
		t.Fatalf("submit health: %v", err)
	}
	if err := reporter.SubmitEgressHealth(ctx, "p2", &egresshealth.Result{OKCount: 0, Total: 2}); err != nil {
		t.Fatalf("submit health: %v", err)
	}
	if err := reporter.SubmitEgressHealth(ctx, "p3", &egresshealth.Result{OKCount: 4, Total: 4}); err != nil {
		t.Fatalf("submit health: %v", err)
	}

	if got := testutil.ToFloat64(egressProbeHealthChecksTotal.WithLabelValues("cloudflare-dns", "dns", "ok")); got != 1 {
		t.Fatalf("cloudflare-dns ok = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbeHealthChecksTotal.WithLabelValues("gstatic-204", "connectivity", "fail")); got != 1 {
		t.Fatalf("gstatic-204 fail = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbeHealthChecksByCountryTotal.WithLabelValues("jp", "site", "fail")); got != 1 {
		t.Fatalf("jp/site/fail = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbeHealthTLSFailuresTotal.WithLabelValues("example-site")); got != 1 {
		t.Fatalf("tls failures example-site = %v, want 1", got)
	}
	for state, want := range map[string]float64{"degraded": 1, "dead": 1, "healthy": 1} {
		if got := testutil.ToFloat64(egressProbeHealthResultsTotal.WithLabelValues("jp", state)); got != want {
			t.Fatalf("health results jp/%s = %v, want %v", state, got, want)
		}
	}
	if inner.healthCalls != 3 {
		t.Fatalf("forwarded health results = %d, want 3", inner.healthCalls)
	}
}

func TestEgressProbeMetricsReporterCountsBlackholeChecksByResult(t *testing.T) {
	egressProbeBlackholeChecksTotal.Reset()
	inner := newFakeEgressProbeIngest()
	reporter := newEgressProbeMetricsReporter(inner, nil)
	checks := []ingest.BlackholeCheck{
		{ClientId: "a", OK: true},
		{ClientId: "b", OK: false},
		{ClientId: "c", OK: false, Failure: "tunnel_failed"},
	}
	if err := reporter.SubmitBlackholeChecks(context.Background(), checks); err != nil {
		t.Fatalf("submit: %v", err)
	}
	for result, want := range map[string]float64{"ok": 1, "dark": 1, "tunnel_failed": 1} {
		if got := testutil.ToFloat64(egressProbeBlackholeChecksTotal.WithLabelValues(result)); got != want {
			t.Fatalf("blackhole %s = %v, want %v", result, got, want)
		}
	}
	if len(inner.blackhole) != 3 {
		t.Fatalf("forwarded checks = %d, want 3", len(inner.blackhole))
	}
}

func TestEgressProbeHealthState(t *testing.T) {
	cases := []struct {
		ok, total int
		want      string
	}{
		{4, 4, "healthy"},
		{1, 4, "degraded"},
		{0, 4, "dead"},
		{0, 0, "dead"},
	}
	for _, c := range cases {
		if got := egressProbeHealthState(c.ok, c.total); got != c.want {
			t.Fatalf("state(%d/%d) = %s, want %s", c.ok, c.total, got, c.want)
		}
	}
}

func TestEgressProbePassRecordsBatchMetricsAndRefreshesFleet(t *testing.T) {
	egressProbePassesTotal.Reset()
	egressProbePassProvidersTotal.Reset()
	egressProbePassErrorsTotal.Reset()
	egressProbeGeolocationDiagnosticsTotal.Reset()
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(4), 1)
	refreshed := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			return nil, errors.New("due unavailable")
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return []string{"full-1", "full-2"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error { return nil },
		runBlackhole: func(context.Context, []string, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			t.Fatalf("blackhole batch must not run without due providers")
			return fleetprobe.BlackholeSummary{}, nil
		},
		runFull: func(context.Context, []string, fleetprobe.FullOptions) (prober.Summary, error) {
			return prober.Summary{
				Attempted: 2,
				Submitted: 1,
				Failed:    1,
				GeolocationSourceOutcomes: []prober.GeolocationSourceOutcomeCount{
					{Source: "ipinfo", Class: "timeout", Stage: "request", Count: 3},
				},
			}, nil
		},
		refreshFleet: func(context.Context) { refreshed += 1 },
	}
	if _, err := pass.run(context.Background(), args); err == nil {
		t.Fatalf("run must retain the due error")
	}
	if got := testutil.ToFloat64(egressProbePassesTotal.WithLabelValues("full", "ok")); got != 1 {
		t.Fatalf("full ok passes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbePassProvidersTotal.WithLabelValues("full", "failed")); got != 1 {
		t.Fatalf("full failed providers = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbePassErrorsTotal.WithLabelValues("blackhole_due")); got != 1 {
		t.Fatalf("blackhole_due errors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbeGeolocationDiagnosticsTotal.WithLabelValues("ipinfo", "timeout", "request")); got != 3 {
		t.Fatalf("geolocation diagnostics = %v, want 3", got)
	}
	if refreshed != 1 {
		t.Fatalf("fleet refreshes = %d, want 1", refreshed)
	}
}

func TestSetEgressProbeFleetGaugesClearsStaleDominantClass(t *testing.T) {
	firstRefresh := time.Unix(1_700_000_000, 0)
	setEgressProbeFleetGauges(egressProbeFleetSnapshot{
		outcomeTally:  map[string]int{"": 30, "tunnel_failed": 70},
		healthStates:  map[string]int{"healthy": 20, "dead": 5},
		blackholed:    2,
		tlsAuthFailed: 1,
		dominantClass: "tunnel_failed",
		dominantShare: 0.7,
		refreshedAt:   firstRefresh,
	})
	if got := testutil.ToFloat64(egressProbeFleetAttemptProviders.WithLabelValues("ok")); got != 30 {
		t.Fatalf("fleet ok providers = %v, want 30", got)
	}
	if got := testutil.ToFloat64(egressProbeFleetDominantFailure.WithLabelValues("tunnel_failed")); got != 1 {
		t.Fatalf("dominant tunnel_failed = %v, want 1", got)
	}
	if got := testutil.ToFloat64(egressProbeFleetHealthProviders.WithLabelValues("degraded")); got != 0 {
		t.Fatalf("degraded = %v, want 0", got)
	}
	setEgressProbeFleetGauges(egressProbeFleetSnapshot{
		outcomeTally: map[string]int{"": 100},
		healthStates: map[string]int{},
		refreshedAt:  firstRefresh.Add(time.Minute),
	})
	if got, want := testutil.CollectAndCount(
		egressProbeFleetAttemptProviders,
		"urnetwork_egress_probe_fleet_attempt_providers",
	), len(egressProbeFleetOutcomeClasses); got != want {
		t.Fatalf("fleet outcome series = %d, want fixed live set %d", got, want)
	}
	if got := testutil.ToFloat64(egressProbeFleetDominantFailure.WithLabelValues("tunnel_failed")); got != 0 {
		t.Fatalf("dominant tunnel_failed after clear = %v, want 0", got)
	}
	if got := testutil.ToFloat64(egressProbeFleetAttemptProviders.WithLabelValues("ok")); got != 100 {
		t.Fatalf("fleet ok providers after reset = %v, want 100", got)
	}
	if got := testutil.ToFloat64(egressProbeFleetSnapshotTimestamp); got != float64(firstRefresh.Add(time.Minute).Unix()) {
		t.Fatalf("fleet snapshot timestamp = %v, want %d", got, firstRefresh.Add(time.Minute).Unix())
	}
}
