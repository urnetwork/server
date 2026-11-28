package monitor

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func setUrlProbeAdmissionCohortFixture(process *urlProbeCoverageProcess, counts [3][3]float64) {
	for stateIndex, state := range urlProbeAdmissionCohortStates {
		total := 0.0
		for cohortIndex, cohort := range urlProbeAdmissionCohorts {
			value := counts[cohortIndex][stateIndex]
			process.values["cohort:"+cohort+":"+state] = value
			total += value
		}
		process.values["fleet:"+state] = total
	}
	v := process.values
	v["fleet:complete"], v["fleet:secure_complete"] = v["fleet:quota_complete"], v["fleet:quota_complete"]
	v["fleet:successes_needed"] = v["fleet:runs_needed"]
	v["fleet:warming"] = counts[1][0] - counts[1][1]
	v["fleet:overdue"] = v["fleet:eligible"] - v["fleet:quota_complete"] - v["fleet:warming"]
}

func TestUrlProbeMatureQuotaKeepsNewcomersAndUnknownAgeSeparate(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		counts     [3][3]float64
		deficit    bool
		unknown    bool
		projection string
	}{
		{"warming does not dilute mature", [3][3]float64{{100, 100, 0}, {30, 2, 140}, {0, 0, 0}}, false, false, "known_mature_quota_percent=100.000000"},
		{"aged deficit survives warming", [3][3]float64{{100, 99, 4}, {30, 2, 140}, {0, 0, 0}}, true, false, "known_mature_quota_percent=99.000000"},
		{"missing age cannot prove fleet", [3][3]float64{{100, 100, 0}, {0, 0, 0}, {30, 2, 140}}, false, true, "known_mature_quota_percent=100.000000"},
		{"unknown cannot hide known deficit", [3][3]float64{{100, 99, 4}, {0, 0, 0}, {30, 2, 140}}, true, true, "known_mature_quota_percent=99.000000"},
		{"no mature providers is not 100 percent", [3][3]float64{{0, 0, 0}, {30, 2, 140}, {0, 0, 0}}, false, false, "known_mature_quota_percent=na"},
		{"all unknown is not empty warming", [3][3]float64{{0, 0, 0}, {0, 0, 0}, {30, 2, 140}}, false, true, "known_mature_quota_percent=na"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := urlProbeCoverageFixture(now, "worker-a.example", 0)
			second := urlProbeCoverageFixture(now, "worker-b.example", 1)
			setUrlProbeAdmissionCohortFixture(first, test.counts)
			diagnostic := diagnoseUrlProbeAdmissionCohort(first)
			if !diagnostic.valid || !strings.Contains(diagnostic.projection(first.values), test.projection) {
				t.Fatalf("incorrect mature projection: %s", diagnostic.projection(first.values))
			}
			alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
			wantCount := 0
			if test.deficit {
				wantCount++
				alert := requireAlertClass(t, alerts, "url-probe-coverage-deficit")
				if !strings.Contains(alert.Observed, test.projection) || alert.Severity != SeverityWarn {
					t.Fatalf("mature deficit lost its separate denominator: %+v", alert)
				}
			}
			if test.unknown {
				wantCount++
				alert := requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
				for _, part := range []string{"admission_age_unknown_blocks_whole_fleet_verdict", "admission_age_unknown=30", "census_reason=ok", test.projection} {
					if !strings.Contains(alert.Observed, part) {
						t.Fatalf("unknown cohort hid current evidence %q: %+v", part, alert)
					}
				}
			}
			if len(alerts) != wantCount {
				t.Fatalf("newcomers changed mature verdict: got %+v, want %d findings", alerts, wantCount)
			}
		})
	}
}

func TestUrlProbeMatureContractGapsRetainAllCurrentCensus(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		change func(map[string]float64)
		reason string
	}{
		{"old producer", func(v map[string]float64) {
			for key := range v {
				if strings.HasPrefix(key, "cohort:") || strings.HasPrefix(key, "cohort_time:") || strings.HasPrefix(key, "cohort_contract") {
					delete(v, key)
				}
			}
		}, "contract_missing"},
		{"unsupported contract", func(v map[string]float64) { v["cohort_contract"] = 2 }, "contract_unsupported"},
		{"contract stale generation", func(v map[string]float64) { v["cohort_contract_time"]-- }, "generation_mismatch"},
		{"missing state", func(v map[string]float64) { delete(v, "cohort:age_unknown:eligible") }, "state_missing"},
		{"missing generation", func(v map[string]float64) { delete(v, "cohort_time:warming:eligible") }, "generation_mismatch"},
		{"population partition", func(v map[string]float64) {
			v["cohort:age_unknown:eligible"], v["cohort:age_unknown:runs_needed"] = 1, 1
		}, "eligible_partition"},
		{"quota partition", func(v map[string]float64) {
			v["cohort:mature:quota_complete"], v["cohort:mature:runs_needed"] = 9999, 1
		}, "quota_complete_partition"},
		{"deficit upper bound", func(v map[string]float64) { v["cohort:mature:runs_needed"] = 1 }, "cohort_bounds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := urlProbeCoverageFixture(now, "worker-a.example", 0)
			second := urlProbeCoverageFixture(now, "worker-b.example", 1)
			test.change(first.values)
			alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
			alert := requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
			if len(alerts) != 1 {
				t.Fatalf("missing mature evidence became numeric deficit: %+v", alerts)
			}
			for _, part := range []string{"mature_cohort_reason=" + test.reason, "known_mature_quota_percent=na", "eligible=10000", "quota_complete=10000", "census_reason=ok"} {
				if !strings.Contains(alert.Observed, part) {
					t.Fatalf("lost independent all-current evidence %q: %+v", part, alert)
				}
			}
		})
	}
}

func TestUrlProbeMatureEveryCellRejectsInvalidValueAndMixedGeneration(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	for _, cohort := range urlProbeAdmissionCohorts {
		for _, state := range urlProbeAdmissionCohortStates {
			key := cohort + ":" + state
			for _, value := range []float64{-1, .5, 1e12 + 1, math.NaN(), math.Inf(1), math.Inf(-1)} {
				process := urlProbeCoverageFixture(now, "worker-a.example", 0)
				process.values["cohort:"+key] = value
				if got := diagnoseUrlProbeAdmissionCohort(process); got.reason != "state_invalid" {
					t.Fatalf("accepted %s=%v: %+v", key, value, got)
				}
			}
			process := urlProbeCoverageFixture(now, "worker-a.example", 0)
			process.values["cohort_time:"+key]--
			if got := diagnoseUrlProbeAdmissionCohort(process); got.reason != "generation_mismatch" {
				t.Fatalf("mixed %s generation: %+v", key, got)
			}
		}
	}
}

func TestUrlProbeMatureMalformedExtensionCannotPoisonOldCensusOrBecomeZero(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	for _, mode := range []string{"duplicate", "unknown cohort", "unknown state", "NaN", "+Inf", "-1"} {
		t.Run(mode, func(t *testing.T) {
			first := urlProbeCoverageFixture(now, "worker-a.example", 0)
			second := urlProbeCoverageFixture(now, "worker-b.example", 1)
			var payload map[string]any
			if err := json.Unmarshal([]byte(urlProbeCoverageFixtureJson(t, now, first, second)), &payload); err != nil {
				t.Fatal(err)
			}
			data := payload["data"].(map[string]any)
			rows := data["result"].([]any)
			for _, row := range rows {
				series := row.(map[string]any)
				labels := series["metric"].(map[string]any)
				if labels["monitor_metric"] != "cohort" {
					continue
				}
				switch mode {
				case "duplicate":
					data["result"] = append(rows, row)
				case "unknown cohort":
					labels["cohort"] = "invented"
				case "unknown state":
					labels["state"] = "invented"
				default:
					series["value"].([]any)[1] = mode
				}
				break
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			alerts := runUrlProbeCoverageFixture(t, now, string(encoded))
			alert := requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
			if len(alerts) != 1 || !strings.Contains(alert.Observed, "census_reason=ok") || !strings.Contains(alert.Observed, "mature_cohort_reason=malformed") {
				t.Fatalf("malformed extension changed independent census: %+v", alerts)
			}
		})
	}
}
