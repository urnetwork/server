package monitor

import (
	"testing"
	"time"
)

func TestUrlProbeCoverageFailedMeasurementsSupplyTotalRate(t *testing.T) {
	now := time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	for _, process := range []*urlProbeCoverageProcess{first, second} {
		process.values["success"] = 0
		process.values["error"] = 12500
	}
	if alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second)); len(alerts) != 0 {
		t.Fatalf("accepted failures were omitted from total measured capacity: %+v", alerts)
	}
	delete(second.values, "error")
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
	requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if len(alerts) != 1 {
		t.Fatalf("missing failure series became zero or measured deficit: %+v", alerts)
	}
}

func TestUrlProbeCoverageSuccessOnlyProducerIsNotMeasuredQuotaProof(t *testing.T) {
	now := time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	first.values["capability"] = 1
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
	requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if len(alerts) != 1 {
		t.Fatalf("old success-only census was relabeled as total measured coverage: %+v", alerts)
	}
}
