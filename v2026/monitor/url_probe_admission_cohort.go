package monitor

import (
	"fmt"
	"math"
)

var urlProbeAdmissionCohorts = [...]string{"mature", "warming", "age_unknown"}
var urlProbeAdmissionCohortStates = [...]string{"eligible", "quota_complete", "runs_needed"}

func urlProbeAdmissionCohortLabelValid(cohort, state string) bool {
	for _, knownCohort := range urlProbeAdmissionCohorts {
		if cohort == knownCohort {
			for _, knownState := range urlProbeAdmissionCohortStates {
				if state == knownState {
					return true
				}
			}
		}
	}
	return false
}

type urlProbeAdmissionCohortDiagnostic struct {
	valid  bool
	reason string
}

// This additive extension cannot invalidate an independently coherent old
// census. Missing rollout cells remain unknown, never an empty mature cohort.
func diagnoseUrlProbeAdmissionCohort(process *urlProbeCoverageProcess) urlProbeAdmissionCohortDiagnostic {
	fail := func(reason string) urlProbeAdmissionCohortDiagnostic {
		return urlProbeAdmissionCohortDiagnostic{reason: reason}
	}
	if process.cohortInvalid {
		return fail("malformed")
	}
	v := process.values
	contract, present := v["cohort_contract"]
	if !present {
		return fail("contract_missing")
	}
	if contract != 1 {
		return fail("contract_unsupported")
	}
	if v["cohort_contract_time"] != v["observed_time"] {
		return fail("generation_mismatch")
	}
	totals := map[string]float64{}
	for _, cohort := range urlProbeAdmissionCohorts {
		for _, state := range urlProbeAdmissionCohortStates {
			key := cohort + ":" + state
			value, present := v["cohort:"+key]
			if !present {
				return fail("state_missing")
			}
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value != math.Trunc(value) || value > 1e12 {
				return fail("state_invalid")
			}
			if v["cohort_time:"+key] != v["observed_time"] {
				return fail("generation_mismatch")
			}
			totals[state] += value
		}
		eligible, quota := v["cohort:"+cohort+":eligible"], v["cohort:"+cohort+":quota_complete"]
		runs := v["cohort:"+cohort+":runs_needed"]
		if quota > eligible || runs < eligible-quota || runs > 10*(eligible-quota) {
			return fail("cohort_bounds")
		}
	}
	for _, state := range urlProbeAdmissionCohortStates {
		if totals[state] != v["fleet:"+state] {
			return fail(state + "_partition")
		}
	}
	return urlProbeAdmissionCohortDiagnostic{valid: true, reason: "ok"}
}

func (self urlProbeAdmissionCohortDiagnostic) projection(v map[string]float64) string {
	if !self.valid {
		return "mature_cohort_reason=" + self.reason + " known_mature_quota_percent=na"
	}
	mature, complete := v["cohort:mature:eligible"], v["cohort:mature:quota_complete"]
	percent := "na"
	if mature > 0 {
		percent = fmt.Sprintf("%.6f", 100*complete/mature)
	}
	status := "ready"
	if v["cohort:age_unknown:eligible"] > 0 {
		status = "age_unknown"
	} else if mature == 0 {
		status = "empty_mature_cohort"
	}
	return fmt.Sprintf("mature_cohort_reason=%s mature_eligible=%.0f mature_quota_complete=%.0f mature_runs_needed=%.0f warming_eligible=%.0f warming_quota_complete=%.0f warming_runs_needed=%.0f admission_age_unknown=%.0f age_unknown_quota_complete=%.0f age_unknown_runs_needed=%.0f known_mature_quota_percent=%s",
		status, mature, complete, v["cohort:mature:runs_needed"], v["cohort:warming:eligible"], v["cohort:warming:quota_complete"], v["cohort:warming:runs_needed"],
		v["cohort:age_unknown:eligible"], v["cohort:age_unknown:quota_complete"], v["cohort:age_unknown:runs_needed"], percent)
}
