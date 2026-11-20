package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestUrlProbeCensusDiagnosticsReasonsAndCheckOrder(t *testing.T) {
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		change func(map[string]float64)
		want   urlProbeCensusReason
	}{
		{"ok", func(map[string]float64) {}, urlProbeCensusOK},
		{"observed_missing", func(v map[string]float64) { delete(v, "observed") }, urlProbeCensusObservedMissing},
		{"observed_stale", func(v map[string]float64) { v["observed"] = float64(now.Unix()) - 181 }, urlProbeCensusObservedStale},
		{"observed_future", func(v map[string]float64) { v["observed"] = float64(now.Unix()) + 31 }, urlProbeCensusObservedFuture},
		{"observed_sample_missing", func(v map[string]float64) { delete(v, "observed_time") }, urlProbeCensusObservedSampleUnfresh},
		{"observed_sample_stale", func(v map[string]float64) { v["observed_time"] = float64(now.Unix()) - 181 }, urlProbeCensusObservedSampleUnfresh},
		{"observed_before_start", func(v map[string]float64) { v["start"] = v["observed"] + 1 }, urlProbeCensusObservedBeforeStart},
		{"fleet_missing", func(v map[string]float64) { delete(v, "fleet:eligible") }, urlProbeCensusFleetStateMissing},
		{"fleet_fractional", func(v map[string]float64) { v["fleet:eligible"] = 10000.5 }, urlProbeCensusFleetStateInvalid},
		{"fleet_bound", func(v map[string]float64) { v["fleet:eligible"] = 1e12 + 1 }, urlProbeCensusFleetStateInvalid},
		{"generation", func(v map[string]float64) { v["fleet_time:eligible"]-- }, urlProbeCensusGenerationMismatch},
		{"auxiliary_missing", func(v map[string]float64) { delete(v, "oldest") }, urlProbeCensusAuxiliaryMissing},
		{"population_bound", func(v map[string]float64) { v["fleet:due"] = 10001 }, urlProbeCensusPopulationBound},
		{"deficit_alias", func(v map[string]float64) { v["fleet:successes_needed"] = 1 }, urlProbeCensusDeficitPartition},
		{"deficit_range", func(v map[string]float64) { v["fleet:runs_needed"], v["fleet:successes_needed"] = 1, 1 }, urlProbeCensusDeficitPartition},
		{"cohort_clock", func(v map[string]float64) { v["cohort_started"] = v["observed"] + 1 }, urlProbeCensusCohortClockInvalid},
		{"quota_alias", func(v map[string]float64) { v["fleet:complete"] = 9999 }, urlProbeCensusQuotaSecurityPartition},
		{"security_partition", func(v map[string]float64) { v["fleet:security_unknown_targets"] = 1 }, urlProbeCensusQuotaSecurityPartition},
		{"cohort_partition", func(v map[string]float64) { v["fleet:warming"] = 1 }, urlProbeCensusCohortPartition},
		{"first_clock_before_cells", func(v map[string]float64) { delete(v, "observed"); delete(v, "fleet:eligible") }, urlProbeCensusObservedMissing},
		{"first_cell_before_later_missing", func(v map[string]float64) { v["fleet_time:eligible"]--; delete(v, "fleet:due") }, urlProbeCensusGenerationMismatch},
		{"alias_before_cohort_clock", func(v map[string]float64) { v["fleet:successes_needed"] = 1; v["cohort_started"] = v["observed"] + 1 }, urlProbeCensusDeficitPartition},
	} {
		t.Run(test.name, func(t *testing.T) {
			process := urlProbeCoverageFixture(now, "private-owner.example", 0)
			test.change(process.values)
			got := diagnoseUrlProbeCoverageCensus(process, now)
			if got.reason != test.want || urlProbeCoverageCensusValid(process, now) != (test.want == urlProbeCensusOK) {
				t.Fatalf("reason=%s, want%s", urlProbeCensusReasonLabels[got.reason], urlProbeCensusReasonLabels[test.want])
			}
			if strings.Contains(got.projection(), process.host) || strings.Contains(got.projection(), process.instance) {
				t.Fatal("diagnostic exposed process identity")
			}
		})
	}
}

func TestUrlProbeCensusDiagnosticsFreshnessAndOptionalAges(t *testing.T) {
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		age  float64
		want urlProbeCensusReason
	}{
		{180, urlProbeCensusOK}, {180.001, urlProbeCensusObservedStale},
		{-30, urlProbeCensusOK}, {-30.001, urlProbeCensusObservedFuture},
	} {
		process := urlProbeCoverageFixture(now, "private-owner.example", 0)
		process.values["observed"] = float64(now.Unix()) - test.age
		got := diagnoseUrlProbeCoverageCensus(process, now)
		if got.reason != test.want || !got.observedAge.present || math.Abs(got.observedAge.seconds-test.age) > .000001 || got.sampleAge.seconds != 15 || got.processAge.seconds != 7200 {
			t.Fatalf("boundary age%v: %+v", test.age, got)
		}
	}
	for _, age := range []float64{180, 180.001, -30, -30.001} {
		process := urlProbeCoverageFixture(now, "private-owner.example", 0)
		scrape := float64(now.Unix()) - age
		process.values["observed_time"], process.values["oldest_time"], process.values["cohort_started_time"] = scrape, scrape, scrape
		for _, state := range urlProbeFleetStates {
			process.values["fleet_time:"+state] = scrape
		}
		want := urlProbeCensusOK
		if age > 180 || age < -30 {
			want = urlProbeCensusObservedSampleUnfresh
		}
		if got := diagnoseUrlProbeCoverageCensus(process, now); got.reason != want || !got.sampleAge.present || math.Abs(got.sampleAge.seconds-age) > .000001 {
			t.Fatalf("sample boundary age%v: %+v", age, got)
		}
	}
	process := urlProbeCoverageFixture(now, "private-owner.example", 0)
	process.values["start"] = process.values["observed"]
	if !urlProbeCoverageCensusValid(process, now) {
		t.Fatal("equal process start no longer accepted")
	}
	delete(process.values, "observed")
	got := diagnoseUrlProbeCoverageCensus(process, now)
	if got.observedPresent || got.observedAge.present || strings.Contains(got.projection(), "census_observed_age_seconds=") || !strings.Contains(got.projection(), "census_observed_present=false") {
		t.Fatal("missing clock acquired a false zero age")
	}
	for _, clock := range []float64{0, math.NaN(), math.Inf(1), math.Inf(-1)} {
		process.values["observed"] = clock
		got := diagnoseUrlProbeCoverageCensus(process, now)
		if !got.observedPresent || got.observedAge.present || strings.Contains(got.projection(), "census_observed_age_seconds=") {
			t.Fatal("invalid clock escaped as an age")
		}
	}
	// The durable clock can be stale while its repeatedly scraped sample is fresh.
	process.values["observed"] = float64(now.Unix()) - 181
	process.values["observed_time"] = float64(now.Unix())
	got = diagnoseUrlProbeCoverageCensus(process, now)
	if got.reason != urlProbeCensusObservedStale || got.sampleAge.seconds != 0 || got.observedAge.seconds != 181 {
		t.Fatal("fresh scrape masked retained census age")
	}
}

func TestUrlProbeCensusDiagnosticsEveryCellKeepsGenerationGuard(t *testing.T) {
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for _, state := range urlProbeFleetStates {
		for _, missing := range []bool{false, true} {
			process := urlProbeCoverageFixture(now, "private-owner.example", 0)
			if missing {
				delete(process.values, "fleet_time:"+state)
			} else {
				process.values["fleet_time:"+state]--
			}
			if got := diagnoseUrlProbeCoverageCensus(process, now); got.reason != urlProbeCensusGenerationMismatch {
				t.Fatalf("state%s missing%t: reason%s", state, missing, urlProbeCensusReasonLabels[got.reason])
			}
		}
	}
	for _, name := range []string{"oldest", "cohort_started"} {
		for _, key := range []string{name, name + "_time"} {
			process := urlProbeCoverageFixture(now, "private-owner.example", 0)
			delete(process.values, key)
			want := urlProbeCensusAuxiliaryMissing
			if key != name {
				want = urlProbeCensusGenerationMismatch
			}
			if got := diagnoseUrlProbeCoverageCensus(process, now); got.reason != want {
				t.Fatalf("missing%s reason%s", key, urlProbeCensusReasonLabels[got.reason])
			}
		}
	}
}

func TestUrlProbeCensusDiagnosticsOwnerAndAlertProjection(t *testing.T) {
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	first.values["observed"] = float64(now.Unix()) - 181
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
	alert := requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	for _, field := range []string{"census_reason=observed_stale", "census_observed_present=true", "census_observed_age_seconds=181", "census_observed_sample_age_seconds=15", "census_selected_process_age_seconds=7200"} {
		if !strings.Contains(alert.Observed, field) {
			t.Fatalf("missing%s in%s", field, alert.Observed)
		}
	}
	requireAlertOmits(t, alert, first.instance, second.instance, first.host, second.host)
	for _, processes := range [][]*urlProbeCoverageProcess{nil, {second}, {first, urlProbeCoverageFixture(now, "worker-b.example", 0)}} {
		alerts = runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, processes...))
		alert = requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
		if !strings.Contains(alert.Observed, "source=census-owner census_reason=owner_unavailable") || strings.Contains(alert.Observed, "_age_seconds=") || strings.Contains(alert.Observed, "census_observed_present=") {
			t.Fatalf("unknown owner borrowed a census clock: %s", alert.Observed)
		}
	}
}

// Frozen pre-instrumentation predicate from94329db1. Differential mutations
// guard the acceptance contract while the new helper adds first-failure detail.
func legacyUrlProbeCensusAccepted(process *urlProbeCoverageProcess, now time.Time) bool {
	values := process.values
	observed, present := values["observed"]
	if !present || !urlProbeCoverageFresh(observed, now) || !urlProbeCoverageFresh(values["observed_time"], now) || observed < values["start"] {
		return false
	}
	for _, state := range urlProbeFleetStates {
		value, present := values["fleet:"+state]
		if !present || value != math.Trunc(value) || value > 1e12 || values["fleet_time:"+state] != values["observed_time"] {
			return false
		}
	}
	for _, name := range []string{"oldest", "cohort_started"} {
		if _, present := values[name]; !present || values[name+"_time"] != values["observed_time"] {
			return false
		}
	}
	eligible := values["fleet:eligible"]
	for _, state := range []string{"due", "quota_complete", "secure_complete", "overdue", "security_pending", "security_unknown_targets", "warming", "uninitialized"} {
		if values["fleet:"+state] > eligible {
			return false
		}
	}
	quota, complete, security := values["fleet:quota_complete"], values["fleet:secure_complete"], values["fleet:security_pending"]
	deficit := values["fleet:runs_needed"]
	return values["fleet:successes_needed"] == deficit && values["cohort_started"] <= observed && values["fleet:complete"] == complete && complete <= quota && complete+security <= eligible &&
		values["fleet:security_unknown_targets"] <= security && values["fleet:overdue"] <= eligible-complete &&
		complete+values["fleet:overdue"]+values["fleet:warming"] == eligible &&
		deficit >= eligible-quota && deficit <= 10*(eligible-quota) && quota-complete <= security
}

func TestUrlProbeCensusDiagnosticsPreserveAcceptance(t *testing.T) {
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	baseline := urlProbeCoverageFixture(now, "private-owner.example", 0)
	for name := range baseline.values {
		for _, value := range []float64{-1, 0, .5, 1, 9999, 10000, 10001, 1e12, 1e12 + 1, math.NaN(), math.Inf(1), math.Inf(-1)} {
			process := urlProbeCoverageFixture(now, "private-owner.example", 0)
			process.values[name] = value
			if legacyUrlProbeCensusAccepted(process, now) != urlProbeCoverageCensusValid(process, now) {
				t.Fatalf("changed acceptance for%s=%v", name, value)
			}
		}
		process := urlProbeCoverageFixture(now, "private-owner.example", 0)
		delete(process.values, name)
		if legacyUrlProbeCensusAccepted(process, now) != urlProbeCoverageCensusValid(process, now) {
			t.Fatalf("changed missing-cell acceptance for%s", name)
		}
	}
}
