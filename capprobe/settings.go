// The capability probe's deployment settings (capability_probe.yml), their
// defaults and their bounds.
package capprobe

import (
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The optional deployment resource. Without it the defaults apply.
const SettingsResourceName = "capability_probe.yml"

// The most providers one pass measures at once, a ceiling on the setting.
const maxConcurrency = 256

// The most providers one pass measures in all.
const maxLimit = 4096

// The most pages one pass reads.
const maxPagesPerPass = 1024

// The least a measurement may take beyond the end-to-end establishment
// window: tunnel setup and key registration before the first ClientHello,
// and the acknowledgement that has to follow it.
const minMeasureSetupAllowance = 15 * time.Second

// A pass's whole budget is a prober shard's lifetime, which is at most a day.
const maxPassTime = 24 * time.Hour

// The deployment settings, read at every pass's start and by the
// taskworker's startup schedule, so a change applies to the next pass of a
// process that reads it. Only the max time is carried in the task's
// arguments too, because the task row holds the one it was scheduled with.
type Settings struct {
	// false, the default, switches the probe off: a taskworker schedules no
	// task for it, and a chain already running ends at its next pass
	Enabled bool `yaml:"enabled"`
	// providers one pass measures at once, each in a tunnel of its own
	Concurrency int `yaml:"concurrency"`
	// providers one pass measures in all
	Limit int `yaml:"limit"`
	// providers one sweep page reads
	PageSize int `yaml:"page_size"`
	// sweep pages one pass reads at most
	MaxPagesPerPass int `yaml:"max_pages_per_pass"`
	// one provider's measurement, from opening its tunnel to reading its
	// evidence; the tunnel's teardown follows
	MeasureTimeoutSeconds int `yaml:"measure_timeout_seconds"`
	// how long a negotiated session that has not sealed may take to seal
	// before its unauthenticated reading stands
	SealGraceSeconds int `yaml:"seal_grace_seconds"`
	// a tunnel's graceful teardown (providertunnel.Config.CloseTimeout)
	CloseTimeoutSeconds int `yaml:"close_timeout_seconds"`
	// the age at which a modern provider's evidence is measured again
	ModernRefreshHours int `yaml:"modern_refresh_hours"`
	// the age at which any other provider's evidence is measured again
	OtherRefreshHours int `yaml:"other_refresh_hours"`
	// the least time from the start of one sweep to the start of the next,
	// which also spaces the retries of a provider that gave no evidence
	MinSweepIntervalMinutes int `yaml:"min_sweep_interval_minutes"`
	// the wait between two passes of one sweep
	PassIntervalSeconds int `yaml:"pass_interval_seconds"`
	// a pass's whole budget, the task's max time
	MaxTimeSeconds int `yaml:"max_time_seconds"`
}

// The probe ships switched off: capability_probe.yml turns it on, and these
// values stand for every key the file leaves out. Sixteen tunnels at a time
// measure about 700 providers an hour, so the fleet's Quality and Speed
// providers are measured in about two days and refreshed every three, well
// inside the gate's seven-day staleness window.
func DefaultSettings() Settings {
	return Settings{
		Enabled:                 false,
		Concurrency:             16,
		Limit:                   64,
		PageSize:                1024,
		MaxPagesPerPass:         16,
		MeasureTimeoutSeconds:   120,
		SealGraceSeconds:        10,
		CloseTimeoutSeconds:     30,
		ModernRefreshHours:      72,
		OtherRefreshHours:       72,
		MinSweepIntervalMinutes: 6 * 60,
		PassIntervalSeconds:     5,
		MaxTimeSeconds:          15 * 60,
	}
}

func (self Settings) measureTimeout() time.Duration {
	return time.Duration(self.MeasureTimeoutSeconds) * time.Second
}

func (self Settings) sealGrace() time.Duration {
	return time.Duration(self.SealGraceSeconds) * time.Second
}

func (self Settings) closeTimeout() time.Duration {
	return time.Duration(self.CloseTimeoutSeconds) * time.Second
}

func (self Settings) modernRefresh() time.Duration {
	return time.Duration(self.ModernRefreshHours) * time.Hour
}

func (self Settings) otherRefresh() time.Duration {
	return time.Duration(self.OtherRefreshHours) * time.Hour
}

func (self Settings) minSweepInterval() time.Duration {
	return time.Duration(self.MinSweepIntervalMinutes) * time.Minute
}

// The wait between two passes of one sweep.
func (self Settings) PassInterval() time.Duration {
	return time.Duration(self.PassIntervalSeconds) * time.Second
}

// A pass's whole budget.
func (self Settings) MaxTime() time.Duration {
	return time.Duration(self.MaxTimeSeconds) * time.Second
}

// The whole budget of a pass scheduled with scheduledMaxTimeSeconds, the max
// time its task row holds: that, or these settings' own for a pass scheduled
// without one, or with one no prober shard could live for.
func (self Settings) PassMaxTime(scheduledMaxTimeSeconds int) time.Duration {
	if 0 < scheduledMaxTimeSeconds && scheduledMaxTimeSeconds <= int(maxPassTime/time.Second) {
		return time.Duration(scheduledMaxTimeSeconds) * time.Second
	}
	return self.MaxTime()
}

// What a pass keeps in hand to start one more provider: the measurement,
// the tunnel's teardown, and the evidence write.
func (self Settings) admissionReserve() time.Duration {
	return self.measureTimeout() + self.closeTimeout() + recordTimeout
}

// Every invariant of one settings value. A disabled value is not checked,
// so a broken file can still switch the probe off.
func (self Settings) Validate() error {
	if !self.Enabled {
		return nil
	}
	if self.Concurrency < 1 || maxConcurrency < self.Concurrency {
		return fmt.Errorf("capability probe concurrency must be in [1,%d] (got %d)", maxConcurrency, self.Concurrency)
	}
	if self.Limit < self.Concurrency || maxLimit < self.Limit {
		return fmt.Errorf("capability probe limit must be in [concurrency,%d] (got %d)", maxLimit, self.Limit)
	}
	if self.PageSize < 1 || model.ProviderCapabilityProbeMaxPageSize < self.PageSize {
		return fmt.Errorf("capability probe page_size must be in [1,%d] (got %d)", model.ProviderCapabilityProbeMaxPageSize, self.PageSize)
	}
	if self.MaxPagesPerPass < 1 || maxPagesPerPass < self.MaxPagesPerPass {
		return fmt.Errorf("capability probe max_pages_per_pass must be in [1,%d] (got %d)", maxPagesPerPass, self.MaxPagesPerPass)
	}
	// a provider without the responder is read only after its establishment
	// window has passed, so a measurement must outlast it
	if minMeasureTimeout := tlsTimeout() + minMeasureSetupAllowance; self.measureTimeout() < minMeasureTimeout || 10*time.Minute < self.measureTimeout() {
		return fmt.Errorf("capability probe measure_timeout_seconds must be in [%d,600] (got %d)", int64(minMeasureTimeout/time.Second), self.MeasureTimeoutSeconds)
	}
	if self.SealGraceSeconds < 0 || self.measureTimeout() <= self.sealGrace() {
		return fmt.Errorf("capability probe seal_grace_seconds must be in [0,measure_timeout_seconds) (got %d)", self.SealGraceSeconds)
	}
	if self.CloseTimeoutSeconds < 1 || 300 < self.CloseTimeoutSeconds {
		return fmt.Errorf("capability probe close_timeout_seconds must be in [1,300] (got %d)", self.CloseTimeoutSeconds)
	}
	// a refresh, delayed by up to one sweep interval, must land before the
	// gate reads the evidence as stale
	for _, refresh := range []struct {
		name  string
		hours int
	}{
		{name: "modern_refresh_hours", hours: self.ModernRefreshHours},
		{name: "other_refresh_hours", hours: self.OtherRefreshHours},
	} {
		if refresh.hours < 1 || model.ProviderModernEvidenceMaxAge <= time.Duration(refresh.hours)*time.Hour+self.minSweepInterval() {
			return fmt.Errorf("capability probe %s plus min_sweep_interval_minutes must be under the %s staleness window (got %d hours)", refresh.name, model.ProviderModernEvidenceMaxAge, refresh.hours)
		}
	}
	if self.MinSweepIntervalMinutes < 1 {
		return fmt.Errorf("capability probe min_sweep_interval_minutes must be positive (got %d)", self.MinSweepIntervalMinutes)
	}
	if self.PassIntervalSeconds < 1 {
		return fmt.Errorf("capability probe pass_interval_seconds must be positive (got %d)", self.PassIntervalSeconds)
	}
	if self.MaxTime() < self.admissionReserve()+passSelectionAllowance || maxPassTime < self.MaxTime() {
		return fmt.Errorf(
			"capability probe max_time_seconds must hold one measurement, its teardown and the selection (%d) and be at most %d (got %d)",
			int64((self.admissionReserve()+passSelectionAllowance+time.Second-1)/time.Second),
			int64(maxPassTime/time.Second),
			self.MaxTimeSeconds,
		)
	}
	return nil
}

// The deployment's settings: the defaults overlaid by the optional resource,
// validated. A resource that cannot be read or decoded is an error, never
// the defaults.
func LoadSettings() (Settings, error) {
	settings := DefaultSettings()
	resource, err := server.Config.SimpleResource(SettingsResourceName)
	if err != nil && !errors.Is(err, server.ErrResourceNotFound) {
		return Settings{}, err
	}
	if err == nil {
		if err := resource.UnmarshalYamlE(&settings); err != nil {
			return Settings{}, err
		}
	}
	if err := settings.Validate(); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// The establishment window every capability tunnel's clients run with:
// providertunnel keeps connect's default, and a ClientHello unanswered for
// this long has failed.
func tlsTimeout() time.Duration {
	return connect.DefaultEncryptionSettings().TlsTimeout
}
