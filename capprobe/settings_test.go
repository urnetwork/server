// The settings' defaults, bounds and resource overlay.
package capprobe

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The defaults switch the probe off, and switched on they validate, measure
// past the establishment window, refresh well inside the gate's staleness
// window, and fit a measurement in a pass.
func TestDefaultSettingsAreValid(t *testing.T) {
	settings := DefaultSettings()
	connect.AssertEqual(t, settings.Enabled, false)
	settings.Enabled = true
	connect.AssertEqual(t, settings.Validate(), nil)
	if settings.measureTimeout() < tlsTimeout()+minMeasureSetupAllowance {
		t.Fatalf("the default measurement %s does not outlast the %s establishment window", settings.measureTimeout(), tlsTimeout())
	}
	if model.ProviderModernEvidenceMaxAge <= settings.modernRefresh()+settings.minSweepInterval() {
		t.Fatal("the default refresh does not land before the evidence is stale")
	}
	connect.AssertEqual(t, tlsTimeout(), 60*time.Second)
}

// Each bound refuses the value just past it and accepts the value at it; a
// disabled value is not checked, so a broken file can still switch the
// probe off.
func TestSettingsBounds(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*Settings)
		valid  bool
	}{
		{name: "no concurrency", change: func(s *Settings) { s.Concurrency = 0 }},
		{name: "most concurrency", change: func(s *Settings) { s.Concurrency = maxConcurrency; s.Limit = maxConcurrency }, valid: true},
		{name: "too much concurrency", change: func(s *Settings) { s.Concurrency = maxConcurrency + 1; s.Limit = maxLimit }},
		{name: "limit under concurrency", change: func(s *Settings) { s.Limit = s.Concurrency - 1 }},
		{name: "limit at concurrency", change: func(s *Settings) { s.Limit = s.Concurrency }, valid: true},
		{name: "no page", change: func(s *Settings) { s.PageSize = 0 }},
		{name: "page past the model's bound", change: func(s *Settings) { s.PageSize = model.ProviderCapabilityProbeMaxPageSize + 1 }},
		{name: "no pages", change: func(s *Settings) { s.MaxPagesPerPass = 0 }},
		{name: "measurement inside the establishment window", change: func(s *Settings) {
			s.MeasureTimeoutSeconds = int((tlsTimeout() + minMeasureSetupAllowance - time.Second) / time.Second)
		}},
		{name: "shortest measurement", change: func(s *Settings) {
			s.MeasureTimeoutSeconds = int((tlsTimeout() + minMeasureSetupAllowance) / time.Second)
		}, valid: true},
		{name: "negative seal grace", change: func(s *Settings) { s.SealGraceSeconds = -1 }},
		{name: "seal grace as long as the measurement", change: func(s *Settings) { s.SealGraceSeconds = s.MeasureTimeoutSeconds }},
		{name: "no teardown", change: func(s *Settings) { s.CloseTimeoutSeconds = 0 }},
		{name: "no modern refresh", change: func(s *Settings) { s.ModernRefreshHours = 0 }},
		{name: "modern refresh reaching staleness", change: func(s *Settings) {
			s.ModernRefreshHours = int(model.ProviderModernEvidenceMaxAge/time.Hour) - s.MinSweepIntervalMinutes/60
		}},
		{name: "longest modern refresh", change: func(s *Settings) {
			s.ModernRefreshHours = int(model.ProviderModernEvidenceMaxAge/time.Hour) - s.MinSweepIntervalMinutes/60 - 1
		}, valid: true},
		{name: "other refresh reaching staleness", change: func(s *Settings) { s.OtherRefreshHours = int(model.ProviderModernEvidenceMaxAge / time.Hour) }},
		{name: "shorter other refresh", change: func(s *Settings) { s.OtherRefreshHours = 12 }, valid: true},
		{name: "no sweep interval", change: func(s *Settings) { s.MinSweepIntervalMinutes = 0 }},
		{name: "no pass interval", change: func(s *Settings) { s.PassIntervalSeconds = 0 }},
		{name: "pass shorter than one measurement", change: func(s *Settings) {
			s.MaxTimeSeconds = int((s.admissionReserve() + passSelectionAllowance - time.Second) / time.Second)
		}},
		{name: "shortest pass", change: func(s *Settings) {
			s.MaxTimeSeconds = int((s.admissionReserve() + passSelectionAllowance) / time.Second)
		}, valid: true},
		{name: "pass past a shard's lifetime", change: func(s *Settings) { s.MaxTimeSeconds = int(maxPassTime/time.Second) + 1 }},
		{name: "disabled and broken", change: func(s *Settings) { s.Enabled = false; s.Concurrency = 0; s.MaxTimeSeconds = 0 }, valid: true},
	} {
		settings := DefaultSettings()
		settings.Enabled = true
		c.change(&settings)
		if err := settings.Validate(); (err == nil) != c.valid {
			t.Errorf("%s: validate = %v, want valid %t", c.name, err, c.valid)
		}
	}
}

// The deployment resource overlays the defaults; one that does not decode,
// or does not validate, is an error rather than the defaults.
func TestLoadSettingsOverlaysTheResource(t *testing.T) {
	pop := server.Config.PushSimpleResource(SettingsResourceName, []byte("enabled: true\nconcurrency: 4\nlimit: 8\n"))
	settings, err := LoadSettings()
	pop()
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultSettings()
	want.Enabled = true
	want.Concurrency = 4
	want.Limit = 8
	connect.AssertEqual(t, settings, want)

	// a file that says nothing of enabled leaves the probe switched off
	pop = server.Config.PushSimpleResource(SettingsResourceName, []byte("concurrency: 4\nlimit: 8\n"))
	settings, err = LoadSettings()
	pop()
	if err != nil || settings.Enabled {
		t.Fatalf("a resource without enabled loaded %+v, %v", settings, err)
	}

	for _, body := range []string{"concurrency: [", "enabled: true\nconcurrency: 0\n"} {
		pop := server.Config.PushSimpleResource(SettingsResourceName, []byte(body))
		_, err := LoadSettings()
		pop()
		if err == nil {
			t.Errorf("%q loaded", body)
		}
	}
	pop = server.Config.PushSimpleResource(SettingsResourceName, []byte("enabled: false\n"))
	settings, err = LoadSettings()
	pop()
	if err != nil || settings.Enabled {
		t.Fatalf("a disabled resource loaded %+v, %v", settings, err)
	}
}

// A pass runs on the max time its task row was scheduled with while a
// prober shard can live that long, and on the settings' own otherwise.
func TestPassMaxTimeIsTheScheduledOne(t *testing.T) {
	settings := DefaultSettings()
	settings.MaxTimeSeconds = 1200
	for _, c := range []struct {
		scheduled int
		want      time.Duration
	}{
		{scheduled: 0, want: 20 * time.Minute},
		{scheduled: -1, want: 20 * time.Minute},
		{scheduled: 600, want: 10 * time.Minute},
		{scheduled: int(maxPassTime / time.Second), want: maxPassTime},
		{scheduled: int(maxPassTime/time.Second) + 1, want: 20 * time.Minute},
	} {
		if got := settings.PassMaxTime(c.scheduled); got != c.want {
			t.Errorf("scheduled %d s: pass max time %s, want %s", c.scheduled, got, c.want)
		}
	}
}
