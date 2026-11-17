package controller

import (
	"time"
)

// The drift test's settings (lag.dev/DRIFTTEST.md P7), the `drift:` block of
// `lag.yml` (lag_config.go). Every key is optional and defaults to the
// design's value:
//
//	drift:
//	  measure_ms: 12000           # a run's cap from its sessions' opening (V10)
//	  extenders: 12               # a run's extenders: 4 near, 4 mid, 4 far (E2)
//	  spares: 4                   # in positions 12-15, opened only for a session that fails (E2)
//	  max_runs: 6                 # per token (decision 4)
//	  retest_window: 15m          # after the first start (decision 4)
//	  window_runs: 3              # the runs pooled for the verdict (V7)
//	  confidence_default: 0.99    # when the allocation names none (V6)
//	  max_sigma_km: 1500          # a larger 1σ ellipse places no one (V6)
//	  report_wait_ms: 20000       # from finish to completing without every report (E5)
//	  processing_after: 30s       # a run without finish stops measuring (P6)
//	  usable_round_trips: 12      # an extender's report is usable with this many (E5)
//	  min_extenders: 6            # usable extenders a run needs to count (E5)
//	  stop_area_tolerance: 1.17   # the server's ±8% on the ellipse's area (V10)
//	  allocate_limit: 600         # per network per allocate_limit_window (P7)
//	  allocate_limit_window: 1m
//	  start_limit: 120            # runs per /24 (/48) per start_limit_window (P7)
//	  start_limit_window: 1h
//	  demo_starts_per_hour: 30    # per /24 (/48) (W8)
//	  ticket_ttl: 1m              # to open a run's sessions
//	  sweep_interval: 15s         # the drift sweep (E5, K3, R1)
//	  retention: 168h             # drift tests and runs, 7 days (R1)
//
// The model's constants, κ and σ₀ (V3), and the relay threshold behind
// relay_suspected (K2, Q12) are the weekly calibration's (V4), with its
// defaults until it has run (lagDriftCalibration). A value that is not
// positive keeps its default, as for every lag setting.
type LagDriftSettings struct {
	MeasureMs           int           `yaml:"measure_ms"`
	Extenders           int           `yaml:"extenders"`
	Spares              int           `yaml:"spares"`
	MaxRuns             int           `yaml:"max_runs"`
	RetestWindow        time.Duration `yaml:"retest_window"`
	WindowRuns          int           `yaml:"window_runs"`
	ConfidenceDefault   float64       `yaml:"confidence_default"`
	MaxSigmaKm          float64       `yaml:"max_sigma_km"`
	ReportWaitMs        int           `yaml:"report_wait_ms"`
	ProcessingAfter     time.Duration `yaml:"processing_after"`
	UsableRoundTrips    int           `yaml:"usable_round_trips"`
	MinExtenders        int           `yaml:"min_extenders"`
	StopAreaTolerance   float64       `yaml:"stop_area_tolerance"`
	AllocateLimit       int           `yaml:"allocate_limit"`
	AllocateLimitWindow time.Duration `yaml:"allocate_limit_window"`
	StartLimit          int           `yaml:"start_limit"`
	StartLimitWindow    time.Duration `yaml:"start_limit_window"`
	DemoStartsPerHour   int           `yaml:"demo_starts_per_hour"`
	TicketTtl           time.Duration `yaml:"ticket_ttl"`
	SweepInterval       time.Duration `yaml:"sweep_interval"`
	Retention           time.Duration `yaml:"retention"`
}

// The design's values (lag.dev/DRIFTTEST.md P7, E2, E5, V6, V7, V10, W8).
func DefaultLagDriftSettings() *LagDriftSettings {
	return &LagDriftSettings{
		MeasureMs:           12000,
		Extenders:           12,
		Spares:              4,
		MaxRuns:             6,
		RetestWindow:        15 * time.Minute,
		WindowRuns:          3,
		ConfidenceDefault:   0.99,
		MaxSigmaKm:          1500,
		ReportWaitMs:        20000,
		ProcessingAfter:     30 * time.Second,
		UsableRoundTrips:    12,
		MinExtenders:        6,
		StopAreaTolerance:   1.17,
		AllocateLimit:       600,
		AllocateLimitWindow: time.Minute,
		StartLimit:          120,
		StartLimitWindow:    time.Hour,
		DemoStartsPerHour:   30,
		TicketTtl:           time.Minute,
		SweepInterval:       15 * time.Second,
		Retention:           7 * 24 * time.Hour,
	}
}

// Fills every value that is not positive from the defaults.
func (self *LagDriftSettings) withDefaults() {
	d := DefaultLagDriftSettings()
	positiveInt := func(value *int, fallback int) {
		if *value <= 0 {
			*value = fallback
		}
	}
	positiveFloat := func(value *float64, fallback float64) {
		// written so that a value that is not a number keeps its default
		if !(0 < *value) {
			*value = fallback
		}
	}
	positiveDuration := func(value *time.Duration, fallback time.Duration) {
		if *value <= 0 {
			*value = fallback
		}
	}
	positiveInt(&self.MeasureMs, d.MeasureMs)
	positiveInt(&self.Extenders, d.Extenders)
	positiveInt(&self.Spares, d.Spares)
	positiveInt(&self.MaxRuns, d.MaxRuns)
	positiveDuration(&self.RetestWindow, d.RetestWindow)
	positiveInt(&self.WindowRuns, d.WindowRuns)
	positiveFloat(&self.ConfidenceDefault, d.ConfidenceDefault)
	if !lagDriftConfidenceAllowed(self.ConfidenceDefault) {
		self.ConfidenceDefault = d.ConfidenceDefault
	}
	positiveFloat(&self.MaxSigmaKm, d.MaxSigmaKm)
	positiveInt(&self.ReportWaitMs, d.ReportWaitMs)
	positiveDuration(&self.ProcessingAfter, d.ProcessingAfter)
	positiveInt(&self.UsableRoundTrips, d.UsableRoundTrips)
	positiveInt(&self.MinExtenders, d.MinExtenders)
	positiveFloat(&self.StopAreaTolerance, d.StopAreaTolerance)
	positiveInt(&self.AllocateLimit, d.AllocateLimit)
	positiveDuration(&self.AllocateLimitWindow, d.AllocateLimitWindow)
	positiveInt(&self.StartLimit, d.StartLimit)
	positiveDuration(&self.StartLimitWindow, d.StartLimitWindow)
	positiveInt(&self.DemoStartsPerHour, d.DemoStartsPerHour)
	positiveDuration(&self.TicketTtl, d.TicketTtl)
	positiveDuration(&self.SweepInterval, d.SweepInterval)
	positiveDuration(&self.Retention, d.Retention)
}

// The time a run's report may take after its finish (E5).
func (self *LagDriftSettings) ReportWait() time.Duration {
	return time.Duration(self.ReportWaitMs) * time.Millisecond
}

// The confidences an allocation may name (P2, V6).
var lagDriftConfidences = []float64{0.95, 0.99, 0.999}

func lagDriftConfidenceAllowed(confidence float64) bool {
	for _, allowed := range lagDriftConfidences {
		if confidence == allowed {
			return true
		}
	}
	return false
}
