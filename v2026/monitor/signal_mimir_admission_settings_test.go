package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The loader is entirely synthetic. A transport callback supplies the explicit
// transition between the pre-contact check and aggregate acceptance.
type mimirAdmissionGenerationFixture struct {
	settings     SignalSettings
	current      SignalSettings
	loadErr      error
	now          time.Time
	processStart int64
	output       string
	afterContact func()
	calls        atomic.Int64
	probe        *mimirAdmissionProbe
}

func newMimirAdmissionGenerationFixture(t *testing.T) *mimirAdmissionGenerationFixture {
	t.Helper()
	fixture := &mimirAdmissionGenerationFixture{now: time.Date(2035, 1, 2, 3, 4, 0, 0, time.UTC)}
	fixture.processStart = fixture.now.Add(-time.Hour).Unix()
	fixture.output = mimirAdmissionHostFixture([]string{
		mimirAdmissionCounterInstanceFixture(fixture.processStart, 5, 3, 123456, 3456),
	}, true, 0, 0, 0)
	fixture.settings = syntheticSettings(&syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		fixture.calls.Add(1)
		if fixture.afterContact != nil {
			fixture.afterContact()
		}
		return fixture.output, nil
	}})
	fixture.settings.Environment = "synthetic-environment"
	fixture.settings.StateDir = t.TempDir()
	fixture.settings.Now = func() time.Time { return fixture.now }
	fixture.settings.Hosts = []HostSettings{{Name: "metrics-a.example", Roles: []string{"services"}}}
	fixture.current = fixture.settings
	fixture.settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		return fixture.current, fixture.loadErr
	})
	fixture.probe = &mimirAdmissionProbe{}
	return fixture
}

func (self *mimirAdmissionGenerationFixture) run(t *testing.T) []finding {
	t.Helper()
	env, err := newProbeEnv(self.settings.withDefaults())
	if err != nil {
		t.Fatal("synthetic environment construction failed")
	}
	findings, err := self.probe.check(context.Background(), env)
	if err != nil {
		t.Fatal("synthetic generation run failed")
	}
	return findings
}

func requireMimirAdmissionGenerationClass(t *testing.T, findings []finding, class string) {
	t.Helper()
	for _, finding := range findings {
		if finding.class == class && !finding.healthy {
			return
		}
	}
	t.Fatal("required nonhealthy finding missing")
}

func requireMimirAdmissionGenerationUnknown(t *testing.T, findings []finding) {
	t.Helper()
	requireMimirAdmissionGenerationClass(t, findings, "cannot-observe")
	for _, finding := range findings {
		if finding.healthy {
			t.Fatal("unknown settings generation authorized recovery")
		}
		alert := alertFromFinding(syntheticSettings(nil), "11.20a", "mimir-admission", "Mimir admission", finding)
		if strings.Contains(alert.Markdown(), "synthetic-private-loader-canary") {
			t.Fatal("settings loader detail entered an alert")
		}
	}
}

func TestMimirAdmissionSettingsChangedBeforeContactRefusesStartupHosts(t *testing.T) {
	fixture := newMimirAdmissionGenerationFixture(t)
	fixture.current.Hosts = append(append([]HostSettings(nil), fixture.current.Hosts...),
		HostSettings{Name: "metrics-b.example", Roles: []string{"services"}})
	alerts := fixture.run(t)
	requireMimirAdmissionGenerationUnknown(t, alerts)
	if fixture.calls.Load() != 0 || len(fixture.probe.snapshotState().histories) != 0 {
		t.Fatal("stale startup inventory contacted a host or advanced history")
	}
}

func TestMimirAdmissionSettingsUnavailableBeforeContactRefusesStartupHosts(t *testing.T) {
	fixture := newMimirAdmissionGenerationFixture(t)
	fixture.loadErr = errors.New("synthetic-private-loader-canary")
	alerts := fixture.run(t)
	requireMimirAdmissionGenerationUnknown(t, alerts)
	if fixture.calls.Load() != 0 || len(fixture.probe.snapshotState().histories) != 0 {
		t.Fatal("unavailable settings authorized contact or advanced history")
	}
}

func TestMimirAdmissionSettingsDriftCannotCreateFreshRejectionFromOldScope(t *testing.T) {
	fixture := newMimirAdmissionGenerationFixture(t)
	fixture.afterContact = func() { fixture.current.Hosts = nil }
	findings := fixture.run(t)
	requireMimirAdmissionGenerationUnknown(t, findings)
	for _, finding := range findings {
		if finding.class == "mimir-series-limit" || finding.class == "mimir-ingestion-rate-limit" {
			t.Fatal("changed-scope positive counter invented a current rejection")
		}
	}
	state := fixture.probe.snapshotState()
	if fixture.calls.Load() != 1 || state.incident || state.rateIncident || len(state.histories) != 0 {
		t.Fatal("changed-scope positive counter advanced incident history")
	}
}

func TestMimirAdmissionSettingsDriftBeforeAcceptanceRetainsIncidentsAndCounters(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		fixture := newMimirAdmissionGenerationFixture(t)
		requireMimirAdmissionGenerationClass(t, fixture.run(t), "mimir-ingestion-rate-limit")
		fixture.now = fixture.now.Add(time.Minute)
		requireMimirAdmissionGenerationClass(t, fixture.run(t), "mimir-series-limit")
		prior := fixture.probe.snapshotState()
		fixture.now = fixture.now.Add(mimirAdmissionQuietWindow)
		fixture.afterContact = func() {
			if unavailable {
				fixture.loadErr = errors.New("synthetic-private-loader-canary")
			} else {
				fixture.current.Hosts = []HostSettings{{Name: "replacement.example", Roles: []string{"services"}}}
			}
		}
		// Same-generation flat discard counters would clear both incidents
		// without the post-contact guard. The changed created counter must not
		// replace history after its inventory authority has been lost.
		fixture.output = mimirAdmissionHostFixture([]string{
			mimirAdmissionCounterInstanceFixture(fixture.processStart, 5, 3, 123457, 3456),
		}, true, 0, 0, 0)
		alerts := fixture.run(t)
		requireMimirAdmissionGenerationUnknown(t, alerts)
		requireMimirAdmissionGenerationClass(t, alerts, "mimir-series-limit")
		requireMimirAdmissionGenerationClass(t, alerts, "mimir-ingestion-rate-limit")
		after := fixture.probe.snapshotState()
		if fixture.calls.Load() != 3 || !reflect.DeepEqual(after.histories, prior.histories) ||
			!after.incident || !after.rateIncident || !after.quietSince.IsZero() || !after.rateQuietSince.IsZero() {
			t.Fatal("changed generation advanced counters, quiet time or incident recovery")
		}
		var persisted mimirAdmissionPersistedState
		loaded, err := loadProviderState(fixture.settings.StateDir, "mimir-admission", mimirAdmissionStateVersion, &persisted)
		if err != nil || !loaded || !persisted.Incident || !persisted.RateIncident || persisted.QuietSinceUnix != 0 || persisted.RateQuietSinceUnix != 0 {
			t.Fatal("changed generation did not durably retain incidents and reset quiet windows")
		}
	}
}

func TestMimirAdmissionSettingsUnknownResetsFullIndependentQuietWindows(t *testing.T) {
	fixture := newMimirAdmissionGenerationFixture(t)
	fixture.run(t)
	fixture.now = fixture.now.Add(time.Minute)
	fixture.run(t)
	fixture.now = fixture.now.Add(mimirAdmissionQuietWindow)
	fixture.loadErr = errors.New("synthetic-private-loader-canary")
	unknown := fixture.run(t)
	requireMimirAdmissionGenerationUnknown(t, unknown)
	requireMimirAdmissionGenerationClass(t, unknown, "mimir-series-limit")
	requireMimirAdmissionGenerationClass(t, unknown, "mimir-ingestion-rate-limit")
	if fixture.calls.Load() != 2 {
		t.Fatal("unknown settings contacted startup inventory")
	}
	fixture.loadErr = nil
	fixture.now = fixture.now.Add(time.Minute)
	fixture.run(t)
	fixture.now = fixture.now.Add(mimirAdmissionQuietWindow - time.Second)
	beforeWindow := fixture.run(t)
	requireMimirAdmissionGenerationClass(t, beforeWindow, "mimir-series-limit")
	requireMimirAdmissionGenerationClass(t, beforeWindow, "mimir-ingestion-rate-limit")
	fixture.now = fixture.now.Add(time.Second)
	afterWindow := fixture.run(t)
	seriesHealthy, rateHealthy := false, false
	for _, alert := range afterWindow {
		seriesHealthy = seriesHealthy || alert.class == "mimir-series-limit" && alert.healthy
		rateHealthy = rateHealthy || alert.class == "mimir-ingestion-rate-limit" && alert.healthy
	}
	if !seriesHealthy || !rateHealthy || fixture.calls.Load() != 5 {
		t.Fatal("unchanged settings failed complete comparable full-window recovery")
	}
}

func TestMimirAdmissionManualSettingsKeepOptionalGenerationCompatibility(t *testing.T) {
	fixture := newMimirAdmissionGenerationFixture(t)
	fixture.settings.SettingsGenerationCheck = nil
	alerts := fixture.run(t)
	requireMimirAdmissionGenerationClass(t, alerts, "mimir-series-limit")
	requireMimirAdmissionGenerationClass(t, alerts, "mimir-ingestion-rate-limit")
	if fixture.calls.Load() != 1 {
		t.Fatal("manually assembled settings lost optional-checker compatibility")
	}
}

func TestMimirAdmissionSettingsDriftDuringFanoutWaitRefusesQueuedHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newMimirAdmissionGenerationFixture(t)
		fixture.settings.Hosts = []HostSettings{
			{Name: "metrics-a.example", Roles: []string{"services"}},
			{Name: "metrics-b.example", Roles: []string{"services"}},
			{Name: "metrics-c.example", Roles: []string{"services"}},
			{Name: "metrics-d.example", Roles: []string{"services"}},
			{Name: "metrics-e.example", Roles: []string{"services"}},
		}
		release := make(chan struct{})
		fixture.settings.Source = &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			fixture.calls.Add(1)
			<-release
			return fixture.output, nil
		}}
		fixture.current = fixture.settings
		env, err := newProbeEnv(fixture.settings.withDefaults())
		if err != nil {
			t.Fatal("synthetic environment construction failed")
		}
		type result struct {
			findings []finding
			err      error
		}
		done := make(chan result, 1)
		go func() {
			findings, err := fixture.probe.check(context.Background(), env)
			done <- result{findings: findings, err: err}
		}()
		synctest.Wait()
		if fixture.calls.Load() != 4 {
			t.Fatal("fanout barrier did not hold four admitted source calls")
		}
		fixture.current.Hosts = nil
		close(release)
		completed := <-done
		if completed.err != nil {
			t.Fatal("synthetic generation run failed")
		}
		requireMimirAdmissionGenerationUnknown(t, completed.findings)
		if fixture.calls.Load() != 4 || len(fixture.probe.snapshotState().histories) != 0 {
			t.Fatal("queued host contacted stale inventory or changed-scope results advanced history")
		}
	})
}
