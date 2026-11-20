package controller

import (
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A drift test's verdict as the customer reads it (lag.dev/DRIFTTEST.md P4,
// V6, V7), stored with the test after each completed run and replaced by the
// next. It names the countries, the distances and the uncertainty, never the
// fitted coordinates (Q6) or the extenders (R2).

// The verdicts (decision 6).
const (
	LagDriftVerdictAgree        = "agree"
	LagDriftVerdictDisagree     = "disagree"
	LagDriftVerdictInconclusive = "inconclusive"
)

// Why the verdict is what it is (P4, V6, V10, K3, K4, E5).
const (
	// agree: the fit is in the IP country
	LagDriftReasonInsideIpCountry = "inside_ip_country"
	// agree: the fit is outside, but its region touches the IP country
	LagDriftReasonRegionOverlaps = "region_overlaps"
	// disagree: the whole region is outside the IP country
	LagDriftReasonRegionOutside = "region_outside"
	// inconclusive: the round trips are physically impossible for any place
	// the region allows in the country (V5)
	LagDriftReasonInconsistent = "inconsistent"
	// inconclusive: at least min_extenders sessions reported, but fewer
	// were usable (E5)
	LagDriftReasonTooFewExtenders = "too_few_extenders"
	// inconclusive: the ellipse is too wide to place anyone (V6)
	LagDriftReasonLowPrecision = "low_precision"
	// inconclusive: no run started from client_ip (K3)
	LagDriftReasonIpMismatch = "ip_mismatch"
	// inconclusive: the sessions mostly took the start's other family (K4)
	LagDriftReasonIpFamilyMismatch = "ip_family_mismatch"
	// inconclusive: the start address's country is unknown, or has no
	// borders (V6)
	LagDriftReasonIpCountryUnknown = "ip_country_unknown"
	// inconclusive: fewer than min_extenders sessions reported at all by
	// report_wait_ms (E5, P6)
	LagDriftReasonReportsMissing = "reports_missing"
	// inconclusive: the browser stopped the run before its reports support
	// a stop (V10)
	LagDriftReasonStoppedEarly = "stopped_early"
)

// What processing made of a run (E5, V10, K3, K4): counted in the window, or
// the reason it stays out, which is the verdict's reason when no run counts.
const (
	LagDriftRunOutcomeCounted          = "counted"
	LagDriftRunOutcomeIpMismatch       = LagDriftReasonIpMismatch
	LagDriftRunOutcomeIpFamilyMismatch = LagDriftReasonIpFamilyMismatch
	LagDriftRunOutcomeStoppedEarly     = LagDriftReasonStoppedEarly
	LagDriftRunOutcomeTooFewExtenders  = LagDriftReasonTooFewExtenders
	LagDriftRunOutcomeReportsMissing   = LagDriftReasonReportsMissing
	// the test failed by an internal error, which closed its open runs (P4)
	LagDriftRunOutcomeFailed = model.LagDriftRunOutcomeFailed
)

// The verdict over the window (P4).
type LagDriftResult struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	// the runs pooled for the verdict (V7)
	RunsInVerdict []int `json:"runs_in_verdict"`
	// every run in the verdict matched client_ip in its family; false when a
	// run from the other family counted (K3, K6), or no run is in it
	Bound bool `json:"bound"`
	// a run whose verdict is not in yet, else null; set as the test is read
	PendingRun *int    `json:"pending_run"`
	Confidence float64 `json:"confidence"`
	// the region's k the verdict was judged at (V11): the largest of its
	// runs', each from V11's table at its start; absent with no run
	K float64 `json:"k,omitempty"`
	// the borders the verdict was judged against (G1)
	GeometryVersion string      `json:"geometry_version,omitempty"`
	Ip              *LagDriftIp `json:"ip"`
	// where the pings place the visitor, when the window has a fit
	Drift *LagDriftDrift `json:"drift,omitempty"`
	// the model the verdict was judged with, so it can be reproduced (V4)
	Calibration *LagDriftCalibrationView `json:"calibration,omitempty"`
}

// The calibration a verdict used (V3, V4, K2): its id and when it ran, null
// for the design's defaults, κ, σ₀, and the relay threshold of the window's
// newest run.
type LagDriftCalibrationView struct {
	CalibrationId    *server.Id `json:"calibration_id"`
	CalibratedAt     *string    `json:"calibrated_at"`
	Kappa            float64    `json:"kappa"`
	Sigma0Ms         float64    `json:"sigma0_ms"`
	RelayThresholdMs float64    `json:"relay_threshold_ms"`
}

// The address's side (P4): client_ip's country, which every run is judged
// against (K3), and the family of the runs judged.
type LagDriftIp struct {
	CountryCode string `json:"country_code,omitempty"`
	CountryName string `json:"country_name,omitempty"`
	Family      int    `json:"family"`
}

// The pings' side (P4, V6, V7). Distances are toward the boundary point of
// the IP country nearest the fit in the region's own metric (V6).
type LagDriftDrift struct {
	// the country containing the fit, else the nearest within 300 km; null
	// at sea (V6)
	CountryCode *string `json:"country_code"`
	// from the fit to the IP country, 0 inside; null when the IP country has
	// no borders
	DistanceKm *float64 `json:"distance_km"`
	// the region's radius in that direction, at the confidence
	SignificanceKm *float64 `json:"significance_km"`
	// distance_km − significance_km: positive only when the whole region is
	// outside
	MarginKm  *float64         `json:"margin_km"`
	EllipseKm *LagDriftEllipse `json:"ellipse_km"`
	// b per run in the verdict (V7)
	LastMileMs []float64 `json:"last_mile_ms"`
	// advisory: a b far above normal (K2, Q12); the verdict is unchanged
	RelaySuspected bool    `json:"relay_suspected"`
	FitChi2PerDof  float64 `json:"fit_chi2_per_dof"`
	Extenders      int     `json:"extenders"`
}

// The fit's 1σ ellipse (V5): its axes in km and the major axis's bearing,
// clockwise from north.
type LagDriftEllipse struct {
	Major      float64 `json:"major"`
	Minor      float64 `json:"minor"`
	BearingDeg float64 `json:"bearing_deg"`
}
