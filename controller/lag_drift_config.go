package controller

import (
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The drift test's settings (lag.dev/DRIFTTEST.md P7), the `drift:` block of
// `lag.yml` (lag_config.go). Every key is optional and defaults to the
// design's value:
//
//	drift:
//	  enabled: true               # false stops every allocation and start, the demo's too
//	  calibration_enabled: true   # false judges with the design's defaults, not the calibration in force
//	  excluded_extenders: []      # extender ids never ticketed for a run or the demo
//	  measure_ms: 12000           # a run's cap from its sessions' opening (V10)
//	  extenders: 12               # a run's extenders: 4 near, 4 mid, 4 far (E2)
//	  spares: 4                   # in positions 12-15, opened only for a session that fails (E2)
//	  max_runs: 6                 # per token (decision 4)
//	  retest_window: 15m          # after the first start (decision 4)
//	  window_runs: 3              # the runs pooled for the verdict (V7)
//	  confidence_default: 0.99    # when the allocation names none, 0.95 or 0.99 (V6, V11)
//	  max_sigma_km: 1500          # a larger 1σ ellipse places no one (V6)
//	  report_wait_ms: 20000       # from finish to completing without every report (E5)
//	  processing_after: 30s       # a run without finish stops measuring (P6)
//	  usable_round_trips: 8       # an extender is usable with this many, for the verdict and the stop check (E5, V10)
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
//	  pin_origin_ipv4: https://api-v4.bringyour.com   # the API's IPv4-only name, for a start pinned to client_ip's family (K6, P5)
//	  pin_origin_ipv6: https://api-v6.bringyour.com   # the API's IPv6-only name
//
// The model's constants, κ and σ₀ (V3), and the relay threshold behind
// relay_suspected (K2, Q12) are the weekly calibration's (V4), with its
// defaults until it has run (model.GetLagDriftCalibration). A value that is
// not positive keeps its default, as for every lag setting; `enabled` is the
// one switch, on unless set false. The settings are read once per process, so
// a change reaches the api and the taskworker as each restarts.
type LagDriftSettings struct {
	// the operator's kill switch: nil or true is on
	Enabled *bool `yaml:"enabled"`
	// whether verdicts use the weekly calibration in force (V4): nil or true
	// does; false judges with the design's defaults, and its verdicts record
	// no calibration, until it is set true again
	CalibrationEnabled *bool `yaml:"calibration_enabled"`
	// extenders an operator keeps out of every run (E2), by id
	ExcludedExtenders []string `yaml:"excluded_extenders"`

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
	// the API's origins that resolve in one family only (K6): a start that
	// arrives over client_ip's other family is pointed at its family's (P5)
	PinOriginIpv4 string `yaml:"pin_origin_ipv4"`
	PinOriginIpv6 string `yaml:"pin_origin_ipv6"`
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
		UsableRoundTrips:    8,
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
		PinOriginIpv4:       "https://api-v4.bringyour.com",
		PinOriginIpv6:       "https://api-v6.bringyour.com",
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
	for _, origin := range []struct {
		value    *string
		fallback string
	}{
		{&self.PinOriginIpv4, d.PinOriginIpv4},
		{&self.PinOriginIpv6, d.PinOriginIpv6},
	} {
		*origin.value = strings.TrimRight(strings.TrimSpace(*origin.value), "/")
		if *origin.value == "" {
			*origin.value = origin.fallback
		}
	}
}

// The API's origin that resolves in IP family 4 or 6 alone (K6, P5).
func (self *LagDriftSettings) PinOrigin(ipFamily int) string {
	if ipFamily == 6 {
		return self.PinOriginIpv6
	}
	return self.PinOriginIpv4
}

// The time a run's report may take after its finish (E5).
func (self *LagDriftSettings) ReportWait() time.Duration {
	return time.Duration(self.ReportWaitMs) * time.Millisecond
}

// Whether drift tests run at all: on unless an operator set `enabled: false`.
func (self *LagDriftSettings) On() bool {
	return self.Enabled == nil || *self.Enabled
}

// Whether verdicts use the calibration in force: unless an operator set
// `calibration_enabled: false`.
func (self *LagDriftSettings) CalibrationOn() bool {
	return self.CalibrationEnabled == nil || *self.CalibrationEnabled
}

// The extenders kept out of every run, by id; an id that does not parse keeps
// nothing out.
func (self *LagDriftSettings) Excluded() map[server.Id]bool {
	excluded := map[server.Id]bool{}
	for _, text := range self.ExcludedExtenders {
		if extenderId, err := server.ParseId(strings.TrimSpace(text)); err == nil {
			excluded[extenderId] = true
		}
	}
	return excluded
}

// The confidences an allocation may name (P2, V6).
// The confidences a drift test takes (V11): an honest 99.9% region is too
// wide to reject anything near.
var lagDriftConfidences = []float64{0.95, 0.99}

func lagDriftConfidenceAllowed(confidence float64) bool {
	for _, allowed := range lagDriftConfidences {
		if confidence == allowed {
			return true
		}
	}
	return false
}
