// Coverage observations retain healthy and unavailable samples independently of alerts.
package monitor

import (
	"encoding/json"
	"io"
	"time"
)

// Counts use unique accepted measured successes plus failures in the trailing
// four hours. Missing observations are nil, never synthetic zero populations.
type UrlProbeQuotaObservation struct {
	Eligible      int64 `json:"eligible"`
	QuotaComplete int64 `json:"quota_complete"`
	RunsNeeded    int64 `json:"runs_needed"`
}

// One existing signal execution supplies one record. Cycle age is the durable
// first probe-admission timestamp, not physical first join or continuous eligibility.
// Source clocks remain distinct from evaluation time; no provider identity is retained.
type UrlProbeCoverageObservation struct {
	SchemaVersion           int                       `json:"schema_version"`
	Environment             string                    `json:"environment"`
	ObservedAt              time.Time                 `json:"observed_at"`
	AgeDomain               string                    `json:"age_domain"`
	CensusReason            string                    `json:"census_reason"`
	MatureCohortReason      string                    `json:"mature_cohort_reason"`
	SourceObservedSeconds   *float64                  `json:"source_observed_timestamp_seconds"`
	SourceSampleSeconds     *float64                  `json:"source_sample_timestamp_seconds"`
	AllCurrent              *UrlProbeQuotaObservation `json:"all_current"`
	Mature                  *UrlProbeQuotaObservation `json:"mature"`
	Warming                 *UrlProbeQuotaObservation `json:"warming"`
	AgeUnknown              *UrlProbeQuotaObservation `json:"age_unknown"`
	KnownMatureQuotaPercent *float64                  `json:"known_mature_quota_percent"`
	SecureComplete          *int64                    `json:"secure_complete"`
	SecurityPending         *int64                    `json:"security_pending"`
	SecurityUnknownTargets  *int64                    `json:"security_unknown_targets"`
	WholeFleetAgeKnown      bool                      `json:"whole_fleet_age_known"`
	SourceCoverageComplete  bool                      `json:"source_coverage_complete"`
	Gaps                    []string                  `json:"gaps"`
}

// The existing short-write guard prevents a partial record from reporting success.
func (self UrlProbeCoverageObservation) WriteJSONL(output io.Writer) error {
	return json.NewEncoder(shortWriteErrorWriter{Writer: output}).Encode(self)
}

// Initialize every execution as unavailable; only validated census cells can
// replace the null counts. This includes configuration and transport failures.
func newUrlProbeCoverageObservation(environment string, now time.Time) UrlProbeCoverageObservation {
	return UrlProbeCoverageObservation{
		SchemaVersion: 1, Environment: environment, ObservedAt: now,
		AgeDomain:    "immutable_probe_cycle_started_at",
		CensusReason: "observation_unavailable", MatureCohortReason: "census_unavailable",
		Gaps: []string{},
	}
}

// Populate only after the ordinary census and additive cohort reducers pass.
func (self *UrlProbeCoverageObservation) recordCensus(process *urlProbeCoverageProcess, cohort urlProbeAdmissionCohortDiagnostic) {
	v := process.values
	observed, sample := v["observed"], v["observed_time"]
	self.SourceObservedSeconds, self.SourceSampleSeconds = &observed, &sample
	counts := func(prefix string) *UrlProbeQuotaObservation {
		return &UrlProbeQuotaObservation{Eligible: int64(v[prefix+"eligible"]), QuotaComplete: int64(v[prefix+"quota_complete"]), RunsNeeded: int64(v[prefix+"runs_needed"])}
	}
	self.AllCurrent = counts("fleet:")
	secure, security, unknownSecurity := int64(v["fleet:secure_complete"]), int64(v["fleet:security_pending"]), int64(v["fleet:security_unknown_targets"])
	self.SecureComplete, self.SecurityPending, self.SecurityUnknownTargets = &secure, &security, &unknownSecurity
	self.MatureCohortReason = cohort.reason
	if !cohort.valid {
		return
	}
	self.Mature, self.Warming, self.AgeUnknown = counts("cohort:mature:"), counts("cohort:warming:"), counts("cohort:age_unknown:")
	self.WholeFleetAgeKnown = self.AgeUnknown.Eligible == 0
	if self.Mature.Eligible > 0 {
		percent := 100 * float64(self.Mature.QuotaComplete) / float64(self.Mature.Eligible)
		self.KnownMatureQuotaPercent = &percent
	}
}
