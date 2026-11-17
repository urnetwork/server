package monitor

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type urlProbeCensusReason uint8

const (
	urlProbeCensusOK urlProbeCensusReason = iota
	urlProbeCensusObservedMissing
	urlProbeCensusObservedStale
	urlProbeCensusObservedFuture
	urlProbeCensusObservedSampleUnfresh
	urlProbeCensusObservedBeforeStart
	urlProbeCensusFleetStateMissing
	urlProbeCensusFleetStateInvalid
	urlProbeCensusGenerationMismatch
	urlProbeCensusAuxiliaryMissing
	urlProbeCensusPopulationBound
	urlProbeCensusDeficitPartition
	urlProbeCensusCohortClockInvalid
	urlProbeCensusQuotaSecurityPartition
	urlProbeCensusCohortPartition
	urlProbeCensusReasonCount
)

var urlProbeCensusReasonLabels = [urlProbeCensusReasonCount]string{
	"ok", "observed_missing", "observed_stale", "observed_future", "observed_sample_unfresh",
	"observed_before_start", "fleet_state_missing", "fleet_state_invalid", "generation_mismatch",
	"auxiliary_missing", "population_bound", "deficit_partition", "cohort_clock_invalid",
	"quota_security_partition", "cohort_partition",
}

// A missing/invalid clock has no age. Future clocks retain a negative finite
// age so a clock error is visible instead of being clamped to a healthy zero.
type urlProbeCensusAge struct {
	seconds float64
	present bool
}

func urlProbeCensusClockAge(values map[string]float64, key string, now time.Time) urlProbeCensusAge {
	clock, present := values[key]
	age := float64(now.Unix()) - clock
	if !present || clock <= 0 || math.IsNaN(age) || math.IsInf(age, 0) {
		return urlProbeCensusAge{}
	}
	return urlProbeCensusAge{seconds: age, present: true}
}

// Only a uniquely selected shard-zero owner supplies these diagnostics. The
// projection contains a fixed enum and scalar clocks, never process identity,
// metric labels, raw responses, or arbitrary error text.
type urlProbeCensusDiagnostic struct {
	reason          urlProbeCensusReason
	observedPresent bool
	observedAge     urlProbeCensusAge
	sampleAge       urlProbeCensusAge
	processAge      urlProbeCensusAge
}

func (self urlProbeCensusDiagnostic) projection() string {
	parts := []string{fmt.Sprintf("census_reason=%s census_observed_present=%t", urlProbeCensusReasonLabels[self.reason], self.observedPresent)}
	for _, item := range []struct {
		name string
		age  urlProbeCensusAge
	}{
		{"census_observed_age_seconds", self.observedAge},
		{"census_observed_sample_age_seconds", self.sampleAge},
		{"census_selected_process_age_seconds", self.processAge},
	} {
		if item.age.present {
			// General format bounds the projection even for an absurd finite
			// clock; no rounding can turn a boundary rejection into age180.
			parts = append(parts, item.name+"="+strconv.FormatFloat(item.age.seconds, 'g', -1, 64))
		}
	}
	return strings.Join(parts, " ")
}

// The first failed check follows the former boolean predicate's exact order.
// Parser/process-owner admission still rejects negative/nonfinite cells and
// malformed identities before this helper. No acceptance boundary is widened.
func diagnoseUrlProbeCoverageCensus(process *urlProbeCoverageProcess, now time.Time) urlProbeCensusDiagnostic {
	values := process.values
	observed, present := values["observed"]
	diagnostic := urlProbeCensusDiagnostic{
		observedPresent: present,
		observedAge:     urlProbeCensusClockAge(values, "observed", now),
		sampleAge:       urlProbeCensusClockAge(values, "observed_time", now),
		processAge:      urlProbeCensusClockAge(values, "start", now),
	}
	fail := func(reason urlProbeCensusReason) urlProbeCensusDiagnostic {
		diagnostic.reason = reason
		return diagnostic
	}
	if !present {
		return fail(urlProbeCensusObservedMissing)
	}
	if !urlProbeCoverageFresh(observed, now) {
		if observed-float64(now.Unix()) > 30 {
			return fail(urlProbeCensusObservedFuture)
		}
		// A present zero comparison clock also failed the old >0 check.
		// Its age remains absent rather than manufacturing an epoch age.
		return fail(urlProbeCensusObservedStale)
	}
	if !urlProbeCoverageFresh(values["observed_time"], now) {
		return fail(urlProbeCensusObservedSampleUnfresh)
	}
	if observed < values["start"] {
		return fail(urlProbeCensusObservedBeforeStart)
	}
	for _, state := range urlProbeFleetStates {
		value, present := values["fleet:"+state]
		if !present {
			return fail(urlProbeCensusFleetStateMissing)
		}
		if value != math.Trunc(value) || value > 1e12 {
			return fail(urlProbeCensusFleetStateInvalid)
		}
		if values["fleet_time:"+state] != values["observed_time"] {
			return fail(urlProbeCensusGenerationMismatch)
		}
	}
	for _, name := range []string{"oldest", "cohort_started"} {
		if _, present := values[name]; !present {
			return fail(urlProbeCensusAuxiliaryMissing)
		}
		if values[name+"_time"] != values["observed_time"] {
			return fail(urlProbeCensusGenerationMismatch)
		}
	}
	eligible := values["fleet:eligible"]
	for _, state := range []string{"due", "quota_complete", "secure_complete", "overdue", "security_pending", "security_unknown_targets", "warming", "uninitialized"} {
		if values["fleet:"+state] > eligible {
			return fail(urlProbeCensusPopulationBound)
		}
	}
	quota, complete, security := values["fleet:quota_complete"], values["fleet:secure_complete"], values["fleet:security_pending"]
	deficit := values["fleet:runs_needed"]
	if values["fleet:successes_needed"] != deficit {
		return fail(urlProbeCensusDeficitPartition)
	}
	if !(values["cohort_started"] <= observed) {
		return fail(urlProbeCensusCohortClockInvalid)
	}
	if values["fleet:complete"] != complete || !(complete <= quota) || !(complete+security <= eligible) || !(values["fleet:security_unknown_targets"] <= security) {
		return fail(urlProbeCensusQuotaSecurityPartition)
	}
	if !(values["fleet:overdue"] <= eligible-complete) || complete+values["fleet:overdue"]+values["fleet:warming"] != eligible {
		return fail(urlProbeCensusCohortPartition)
	}
	if !(deficit >= eligible-quota) || !(deficit <= 10*(eligible-quota)) {
		return fail(urlProbeCensusDeficitPartition)
	}
	if !(quota-complete <= security) {
		return fail(urlProbeCensusQuotaSecurityPartition)
	}
	return diagnostic
}
