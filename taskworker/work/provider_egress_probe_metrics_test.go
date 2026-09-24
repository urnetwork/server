package work

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"

	"github.com/urnetwork/server/controller"
)

func TestEgressProbeHealthLatencyAvoidsBucketCardinalityAndKeepsFreshMaximum(t *testing.T) {
	metrics := newEgressProbeHealthLatencyMetrics()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)

	now := time.Unix(1_700_000_000, 0).UTC()
	metrics.now = func() time.Time { return now }
	metrics.observe("fixture-destination", "connectivity", 0.2)
	metrics.observe("fixture-destination", "connectivity", 0.5)
	now = now.Add(egressProbeHealthLatencyInterval + time.Second)
	metrics.observe("fixture-destination", "connectivity", 0.1)

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*dto.MetricFamily{}
	for _, family := range families {
		byName[family.GetName()] = family
		if family.GetName() == "urnetwork_egress_probe_health_check_seconds_bucket" {
			t.Fatal("bounded destination/class latency unexpectedly exported classic histogram buckets")
		}
	}

	duration := byName["urnetwork_egress_probe_health_check_seconds"]
	if duration == nil || len(duration.Metric) != 1 || duration.Metric[0].Summary == nil {
		t.Fatalf("sum/count latency family = %#v", duration)
	}
	if got := duration.Metric[0].Summary.GetSampleCount(); got != 3 {
		t.Fatalf("latency count = %d, want 3", got)
	}
	if got := duration.Metric[0].Summary.GetSampleSum(); math.Abs(got-0.8) > 1e-12 {
		t.Fatalf("latency sum = %v, want 0.8", got)
	}
	if got := len(duration.Metric[0].Summary.Quantile); got != 0 {
		t.Fatalf("client-side quantiles = %d, want 0", got)
	}

	maximum := byName["urnetwork_egress_probe_health_check_interval_max_seconds"]
	if maximum == nil || len(maximum.Metric) != 1 || maximum.Metric[0].Gauge == nil ||
		maximum.Metric[0].Gauge.GetValue() != 0.1 {
		t.Fatalf("next-interval maximum = %#v, want 0.1", maximum)
	}
	timestamp := byName["urnetwork_egress_probe_health_check_interval_max_timestamp_seconds"]
	if timestamp == nil || len(timestamp.Metric) != 1 || timestamp.Metric[0].Gauge == nil ||
		timestamp.Metric[0].Gauge.GetValue() != float64(now.Unix()) {
		t.Fatalf("maximum timestamp = %#v, want %d", timestamp, now.Unix())
	}
}

// fakeEgressProbeIngest records what reached the operator so the tests can
// prove the reporter forwards every submission unchanged.
type fakeEgressProbeIngest struct {
	submitted    []string
	attempts     map[string]string
	health       []string
	bandwidth    []string
	blackhole    []ingest.BlackholeCheck
	attemptErr   error
	healthErr    error
	beforeReturn func()
	healthCalls  int
}

func newFakeEgressProbeIngest() *fakeEgressProbeIngest {
	return &fakeEgressProbeIngest{attempts: map[string]string{}}
}

func (self *fakeEgressProbeIngest) Submit(_ context.Context, providerClientId string, _ string, _ time.Time) error {
	self.submitted = append(self.submitted, providerClientId)
	return nil
}

func (self *fakeEgressProbeIngest) ReportAttempt(_ context.Context, providerClientId string, probeFailure string) error {
	self.attempts[providerClientId] = probeFailure
	if self.beforeReturn != nil {
		self.beforeReturn()
	}
	return self.attemptErr
}

func (self *fakeEgressProbeIngest) SubmitEgressHealth(_ context.Context, providerClientId string, _ *egresshealth.Result) error {
	self.healthCalls += 1
	self.health = append(self.health, providerClientId)
	if self.beforeReturn != nil {
		self.beforeReturn()
	}
	return self.healthErr
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
	// the exit is labelled with the ingest's own GeoLite2 resolution; a fake
	// keeps this test off the database file
	reporter.resolveExit = func(exitIp string) (*controller.ProviderEgressExit, error) {
		if exitIp != "203.0.113.7" {
			t.Fatalf("resolved exit %q, want the submitted one", exitIp)
		}
		return &controller.ProviderEgressExit{CountryCode: "us", CityConfident: true}, nil
	}
	ctx := context.Background()

	// a located provider carries the country its exit was just placed in
	if err := reporter.Submit(ctx, "fresh-us", "203.0.113.7", time.Now()); err != nil {
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
	err := reporter.ReportAttempt(context.Background(), "x", prober.FailureNoExitIp)
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
			{Name: "cloudflare-dns", Class: egresshealth.ClassDns, Ok: true, Latency: 120 * time.Millisecond},
			{Name: "gstatic-204", Class: egresshealth.ClassConnectivity, Ok: false, Err: "timeout"},
			{Name: "example-site", Class: egresshealth.ClassSite, Ok: false, TlsAuthenticationFailure: true},
		},
		OkCount: 1,
		Total:   3,
	}
	if err := reporter.SubmitEgressHealth(ctx, "p1", res); err != nil {
		t.Fatalf("submit health: %v", err)
	}
	if err := reporter.SubmitEgressHealth(ctx, "p2", &egresshealth.Result{OkCount: 0, Total: 2}); err != nil {
		t.Fatalf("submit health: %v", err)
	}
	if err := reporter.SubmitEgressHealth(ctx, "p3", &egresshealth.Result{OkCount: 4, Total: 4}); err != nil {
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
		{ClientId: "a", Ok: true},
		{ClientId: "b", Ok: false},
		{ClientId: "c", Ok: false, Failure: "tunnel_failed"},
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
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(4), 1)
	refreshed := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			return nil, errors.New("due unavailable")
		},
		fullDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			return []ingest.DueProvider{{ClientId: "full-1"}, {ClientId: "full-2"}}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error { return nil },
		runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			t.Fatalf("blackhole batch must not run without due providers")
			return fleetprobe.BlackholeSummary{}, nil
		},
		runFull: func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			return prober.Summary{
				Attempted:   2,
				Submitted:   1,
				Failed:      1,
				NotMeasured: 1,
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
	if got := testutil.ToFloat64(egressProbePassNotMeasuredTotal.WithLabelValues("full")); got != 1 {
		t.Fatalf("full not-measured providers = %v, want 1", got)
	}
	if refreshed != 1 {
		t.Fatalf("fleet refreshes = %d, want 1", refreshed)
	}
}

// An all-failed control-plane pass must still expose zero submitted counters.
// If these children are not preseeded, the monitor cannot distinguish that
// outage from an old/missing executable metric family.
func TestEgressProbePassMetricDomainsArePreseeded(t *testing.T) {
	egressProbePassProvidersTotal.Reset()
	egressProbePassErrorsTotal.Reset()
	preseedEgressProbePassMetrics()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, family := range families {
		switch family.GetName() {
		case "urnetwork_egress_probe_pass_providers_total":
			for _, metric := range family.Metric {
				schedule, result := "", ""
				for _, pair := range metric.Label {
					if pair.GetName() == "schedule" {
						schedule = pair.GetValue()
					}
					if pair.GetName() == "result" {
						result = pair.GetValue()
					}
				}
				if schedule == "full" {
					seen["full/"+result] = true
				}
			}
		case "urnetwork_egress_probe_pass_errors_total":
			for _, metric := range family.Metric {
				for _, pair := range metric.Label {
					if pair.GetName() == "step" {
						seen["error/"+pair.GetValue()] = true
					}
				}
			}
		}
	}
	for _, key := range []string{
		"full/attempted", "full/submitted", "full/skipped", "full/failed",
		"error/blackhole_due", "error/full_due", "error/pins", "error/blackhole_run", "error/blackhole_submit", "error/full_run", "error/canceled", "error/funding_unavailable", "error/funding_unknown",
	} {
		if !seen[key] {
			t.Fatalf("missing preseeded pass metric child %q", key)
		}
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
