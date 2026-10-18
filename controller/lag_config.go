package controller

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The lag API's settings (lag.dev/PLAN.md A), from the optional vault resource
// `lag.yml`. Every key is optional and defaults to the plan's value, so a
// deployment without the resource runs the plan as written:
//
//	# guest sessions (A1, A2)
//	guest_ttl: 24h
//	guest_limit: 10              # per /24 (/48) per guest_limit_window
//	guest_limit_window: 1h
//	guest_sweep_interval: 15m
//	# the extender sample (A4)
//	extender_sample: 64
//	extender_sample_cache: 30s
//	# tracks and their tickets (A6)
//	track_client_limit: 12       # per client per track_limit_window
//	track_prefix_limit: 60       # per /24 (/48) per track_limit_window
//	track_signature_limit: 30    # reserved; requires authenticated agent identity, not a header claim
//	track_limit_window: 1h
//	track_max_extenders: 12      # 8 dots and 4 spares
//	track_reissue_max: 8         # reissued tickets per track, for dots that reconnect
//	track_reissue_window: 1h     # how long after a track's issue it can be reissued
//	ticket_ttl: 2m               # to open the session; an open one has no limit
//	ping_interval_ms: 250
//	download_ms: 5000
//	upload_ms: 5000
//	max_streams: 4
//	max_download_bytes: 2147483648
//	max_upload_bytes: 2147483648
//	# reports and what they are kept for (A5, A7)
//	report_limit: 720            # posts per extender client per report_limit_window
//	report_limit_window: 1h
//	report_max_count: 32         # reports in one post (M8 batches 32)
//	report_max_age: 24h          # from a ticket's issue to its report's arrival
//	ticket_retention: 48h        # past report_max_age, so a late report still finds its ticket
//	result_retention: 168h       # 7 days: results, and the tracks with their client fields
//
// A value that is not positive keeps its default, so a typo can turn a limit
// down but never off.
type LagSettings struct {
	GuestTtl           time.Duration `yaml:"guest_ttl"`
	GuestLimit         int           `yaml:"guest_limit"`
	GuestLimitWindow   time.Duration `yaml:"guest_limit_window"`
	GuestSweepInterval time.Duration `yaml:"guest_sweep_interval"`

	ExtenderSample      int           `yaml:"extender_sample"`
	ExtenderSampleCache time.Duration `yaml:"extender_sample_cache"`

	TrackClientLimit    int           `yaml:"track_client_limit"`
	TrackPrefixLimit    int           `yaml:"track_prefix_limit"`
	TrackSignatureLimit int           `yaml:"track_signature_limit"`
	TrackLimitWindow    time.Duration `yaml:"track_limit_window"`
	TrackMaxExtenders   int           `yaml:"track_max_extenders"`
	// reissued tickets per track, and how long a track can be reissued (A6)
	TrackReissueMax    int           `yaml:"track_reissue_max"`
	TrackReissueWindow time.Duration `yaml:"track_reissue_window"`
	TicketTtl          time.Duration `yaml:"ticket_ttl"`
	PingIntervalMs     int           `yaml:"ping_interval_ms"`
	DownloadMs         int           `yaml:"download_ms"`
	UploadMs           int           `yaml:"upload_ms"`
	MaxStreams         int           `yaml:"max_streams"`
	MaxDownloadBytes   int64         `yaml:"max_download_bytes"`
	MaxUploadBytes     int64         `yaml:"max_upload_bytes"`

	ReportLimit       int           `yaml:"report_limit"`
	ReportLimitWindow time.Duration `yaml:"report_limit_window"`
	ReportMaxCount    int           `yaml:"report_max_count"`
	// a session has no time limit, so its report can arrive long after its
	// ticket was issued; past this it is refused (A7)
	ReportMaxAge    time.Duration `yaml:"report_max_age"`
	TicketRetention time.Duration `yaml:"ticket_retention"`
	ResultRetention time.Duration `yaml:"result_retention"`
}

// The plan's values (lag.dev/PLAN.md A1, A4-A7).
func DefaultLagSettings() *LagSettings {
	return &LagSettings{
		GuestTtl:           24 * time.Hour,
		GuestLimit:         10,
		GuestLimitWindow:   time.Hour,
		GuestSweepInterval: 15 * time.Minute,

		ExtenderSample:      64,
		ExtenderSampleCache: 30 * time.Second,

		TrackClientLimit:    12,
		TrackPrefixLimit:    60,
		TrackSignatureLimit: 30,
		TrackLimitWindow:    time.Hour,
		TrackMaxExtenders:   12,
		// a dropped session reconnects once (L4), so 8 covers every dot once
		TrackReissueMax:    8,
		TrackReissueWindow: time.Hour,
		TicketTtl:          2 * time.Minute,
		PingIntervalMs:     250,
		DownloadMs:         5000,
		UploadMs:           5000,
		MaxStreams:         4,
		MaxDownloadBytes:   2 * 1024 * 1024 * 1024,
		MaxUploadBytes:     2 * 1024 * 1024 * 1024,

		// a track's 8 to 12 sessions report about a minute apart, and an
		// operator runs every extender of an account under one client, so the
		// budget is generous; it only stops a reporter that loops
		ReportLimit:       720,
		ReportLimitWindow: time.Hour,
		ReportMaxCount:    32,
		ReportMaxAge:      24 * time.Hour,
		TicketRetention:   48 * time.Hour,
		ResultRetention:   7 * 24 * time.Hour,
	}
}

// Fills every value that is not positive from the defaults.
func (self *LagSettings) withDefaults() *LagSettings {
	d := DefaultLagSettings()
	positiveDuration := func(value *time.Duration, fallback time.Duration) {
		if *value <= 0 {
			*value = fallback
		}
	}
	positiveInt := func(value *int, fallback int) {
		if *value <= 0 {
			*value = fallback
		}
	}
	positiveInt64 := func(value *int64, fallback int64) {
		if *value <= 0 {
			*value = fallback
		}
	}
	positiveDuration(&self.GuestTtl, d.GuestTtl)
	positiveInt(&self.GuestLimit, d.GuestLimit)
	positiveDuration(&self.GuestLimitWindow, d.GuestLimitWindow)
	positiveDuration(&self.GuestSweepInterval, d.GuestSweepInterval)
	positiveInt(&self.ExtenderSample, d.ExtenderSample)
	positiveDuration(&self.ExtenderSampleCache, d.ExtenderSampleCache)
	positiveInt(&self.TrackClientLimit, d.TrackClientLimit)
	positiveInt(&self.TrackPrefixLimit, d.TrackPrefixLimit)
	positiveInt(&self.TrackSignatureLimit, d.TrackSignatureLimit)
	positiveDuration(&self.TrackLimitWindow, d.TrackLimitWindow)
	positiveInt(&self.TrackMaxExtenders, d.TrackMaxExtenders)
	positiveInt(&self.TrackReissueMax, d.TrackReissueMax)
	positiveDuration(&self.TrackReissueWindow, d.TrackReissueWindow)
	positiveDuration(&self.TicketTtl, d.TicketTtl)
	positiveInt(&self.PingIntervalMs, d.PingIntervalMs)
	positiveInt(&self.DownloadMs, d.DownloadMs)
	positiveInt(&self.UploadMs, d.UploadMs)
	positiveInt(&self.MaxStreams, d.MaxStreams)
	positiveInt64(&self.MaxDownloadBytes, d.MaxDownloadBytes)
	positiveInt64(&self.MaxUploadBytes, d.MaxUploadBytes)
	positiveInt(&self.ReportLimit, d.ReportLimit)
	positiveDuration(&self.ReportLimitWindow, d.ReportLimitWindow)
	positiveInt(&self.ReportMaxCount, d.ReportMaxCount)
	positiveDuration(&self.ReportMaxAge, d.ReportMaxAge)
	positiveDuration(&self.TicketRetention, d.TicketRetention)
	positiveDuration(&self.ResultRetention, d.ResultRetention)
	return self
}

// Reads `lag.yml` over the defaults. An absent resource is the plan's
// defaults; an unreadable one is logged and also falls back, since a typo in
// an optional resource must not take the guest sessions down.
func readLagSettings() *LagSettings {
	settings := DefaultLagSettings()
	resource, err := server.Vault.SimpleResource("lag.yml")
	if err != nil {
		return settings
	}
	if err := resource.UnmarshalYamlE(settings); err != nil {
		glog.Errorf("[lag]lag.yml is not readable, using the defaults: %s\n", err)
		return DefaultLagSettings()
	}
	return settings.withDefaults()
}

var lagSettingsOnce = sync.OnceValue(readLagSettings)

// A test's settings, in place of the resource's.
var lagSettingsOverride atomic.Pointer[LagSettings]

// The lag API's settings, read once per process.
func GetLagSettings() *LagSettings {
	if settings := lagSettingsOverride.Load(); settings != nil {
		return settings
	}
	return lagSettingsOnce()
}

// Testing_SetLagSettings replaces the settings until the returned restore runs.
func Testing_SetLagSettings(settings *LagSettings) (restore func()) {
	previous := lagSettingsOverride.Swap(settings)
	return func() {
		lagSettingsOverride.Store(previous)
	}
}
