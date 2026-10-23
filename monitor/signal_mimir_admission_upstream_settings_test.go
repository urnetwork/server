// Composed generation controls for the desired inputs added after the frozen
// Mimir guard graph. These exercise the actual checker and settings-to-probe
// projection without changing the original regression fixture or source.
package monitor

import (
	"reflect"
	"testing"
	"time"
)

// Enroll nonnil synthetic values so unchanged pointer and collection inputs
// must compare equal through the upstream defaulting and clone paths.
func newMimirAdmissionUpstreamSettingsFixture(t *testing.T) *mimirAdmissionGenerationFixture {
	t.Helper()
	fixture := newMimirAdmissionGenerationFixture(t)
	fixture.settings.SubnetCorrectness = &SubnetCorrectnessSettings{
		Schema:     subnetCorrectnessEnrollmentSchema,
		ReviewFrom: "2035-01-01T00:00:00Z", ReviewUntil: "2035-01-03T00:00:00Z",
	}
	fixture.settings.SubnetReplay = DefaultSubnetReplaySettings()
	fixture.settings.SubnetDeployment = &SubnetReplayPin{status: subnetReplayPinMissing}
	fixture.settings.ObserverOverlay = ObserverOverlaySettings{Interfaces: []string{"synthetic0"}}
	fixture.current = fixture.settings
	return fixture
}

// Replace the current input with an independently owned changed value; a
// shared-pointer mutation must not accidentally change both sides of the test.
func changeMimirAdmissionUpstreamCurrentSettings(fixture *mimirAdmissionGenerationFixture, field string) {
	switch field {
	case "correctness":
		fixture.current.SubnetCorrectness = cloneSubnetCorrectnessSettings(fixture.current.SubnetCorrectness)
		fixture.current.SubnetCorrectness.ReviewUntil = "2035-01-04T00:00:00Z"
	case "replay":
		fixture.current.SubnetReplay = cloneSubnetReplaySettings(fixture.current.SubnetReplay)
		fixture.current.SubnetReplay.RunBudget += time.Second
	case "deployment":
		fixture.current.SubnetDeployment = cloneSubnetReplayPin(fixture.current.SubnetDeployment)
		fixture.current.SubnetDeployment.status = subnetReplayPinBlocked
	case "overlay":
		fixture.current.ObserverOverlay = cloneObserverOverlaySettings(fixture.current.ObserverOverlay)
		fixture.current.ObserverOverlay.Interfaces[0] = "synthetic1"
	default:
		panic("unknown synthetic generation field")
	}
}

// A newly changed desired input must refuse startup-host contact even when
// the existing quiet window would otherwise be complete.
func TestMimirAdmissionUpstreamSettingsDriftBeforeContactRetainsIncidents(t *testing.T) {
	for _, field := range []string{"correctness", "replay", "deployment", "overlay"} {
		fixture := newMimirAdmissionUpstreamSettingsFixture(t)
		fixture.run(t)
		fixture.now = fixture.now.Add(time.Minute)
		fixture.run(t)
		prior := fixture.probe.snapshotState()
		changeMimirAdmissionUpstreamCurrentSettings(fixture, field)
		fixture.now = fixture.now.Add(mimirAdmissionQuietWindow)
		findings := fixture.run(t)
		requireMimirAdmissionGenerationUnknown(t, findings)
		requireMimirAdmissionGenerationClass(t, findings, "mimir-series-limit")
		requireMimirAdmissionGenerationClass(t, findings, "mimir-ingestion-rate-limit")
		after := fixture.probe.snapshotState()
		if fixture.calls.Load() != 2 || !reflect.DeepEqual(after.histories, prior.histories) ||
			!after.incident || !after.rateIncident || !after.quietSince.IsZero() || !after.rateQuietSince.IsZero() {
			t.Fatalf("changed %s input contacted startup inventory or advanced recovery", field)
		}
	}
}

// The transport callback supplies the exact transition after pre-contact
// authority but before joined results can replace counters or clear incidents.
func TestMimirAdmissionUpstreamSettingsDriftBeforeAcceptanceRetainsCounters(t *testing.T) {
	for _, field := range []string{"correctness", "replay", "deployment", "overlay"} {
		fixture := newMimirAdmissionUpstreamSettingsFixture(t)
		fixture.run(t)
		fixture.now = fixture.now.Add(time.Minute)
		fixture.run(t)
		prior := fixture.probe.snapshotState()
		fixture.now = fixture.now.Add(mimirAdmissionQuietWindow)
		fixture.output = mimirAdmissionHostFixture([]string{
			mimirAdmissionCounterInstanceFixture(fixture.processStart, 5, 3, 123457, 3456),
		}, true, 0, 0, 0)
		fixture.afterContact = func() { changeMimirAdmissionUpstreamCurrentSettings(fixture, field) }
		findings := fixture.run(t)
		requireMimirAdmissionGenerationUnknown(t, findings)
		requireMimirAdmissionGenerationClass(t, findings, "mimir-series-limit")
		requireMimirAdmissionGenerationClass(t, findings, "mimir-ingestion-rate-limit")
		after := fixture.probe.snapshotState()
		if fixture.calls.Load() != 3 || !reflect.DeepEqual(after.histories, prior.histories) ||
			!after.incident || !after.rateIncident || !after.quietSince.IsZero() || !after.rateQuietSince.IsZero() {
			t.Fatalf("changed %s input authorized result acceptance or recovery", field)
		}
	}
}

// An unchanged nonnil enrollment and observer contract must remain observable;
// equality cannot be inferred merely from the absence of an error finding.
func TestMimirAdmissionUpstreamUnchangedNonnilSettingsRemainHealthy(t *testing.T) {
	fixture := newMimirAdmissionUpstreamSettingsFixture(t)
	fixture.output = mimirAdmissionHostFixture([]string{
		mimirAdmissionCounterInstanceFixture(fixture.processStart, 0, 0, 123456, 3456),
	}, true, 0, 0, 0)
	fixture.run(t)
	fixture.now = fixture.now.Add(time.Minute)
	findings := fixture.run(t)
	seriesHealthy, rateHealthy := false, false
	for _, finding := range findings {
		if finding.class == "cannot-observe" {
			t.Fatal("unchanged upstream input became unknown")
		}
		seriesHealthy = seriesHealthy || finding.class == "mimir-series-limit" && finding.healthy
		rateHealthy = rateHealthy || finding.class == "mimir-ingestion-rate-limit" && finding.healthy
	}
	if !seriesHealthy || !rateHealthy || fixture.calls.Load() != 2 {
		t.Fatal("unchanged upstream inputs did not establish explicit comparable health")
	}
}
