// The optional freshness witness must remain process-only through the actual
// Mimir guard projection. Its own callback evidence cannot authorize a later
// changed-generation Mimir attempt or erase the independent incident hold.
package monitor

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

// Exercise both reviewed owners together: guard callbacks emit no witness,
// freshness emits its actual result, and real drift still fences recovery.
func TestMimirAdmissionSettingsWitnessCompositionPreservesAuthority(t *testing.T) {
	fixture := newMimirAdmissionUpstreamSettingsFixture(t)
	var output bytes.Buffer
	witness, err := newSettingsGenerationWitness(fixture.settings, &output, settingsWitnessTestIdentity(), DefaultSettingsGenerationWitnessSettings())
	if err != nil {
		t.Fatal("synthetic composed witness initialization failed")
	}
	// Deliberately attach only to startup. Desired current settings have no
	// writer, and its changing sequence must stay outside complete equality.
	fixture.settings.settingsGenerationWitness = witness
	for range 2 {
		findings := fixture.run(t)
		requireMimirAdmissionGenerationClass(t, findings, "mimir-series-limit")
		requireMimirAdmissionGenerationClass(t, findings, "mimir-ingestion-rate-limit")
		for _, finding := range findings {
			if finding.class == "cannot-observe" {
				t.Fatal("process-only witness made actual guard authority unknown")
			}
		}
		fixture.now = fixture.now.Add(time.Minute)
	}
	if fixture.calls.Load() != 2 || output.Len() != 0 {
		t.Fatal("Mimir guard callbacks emitted a freshness witness or refused true pressure")
	}
	freshnessAlerts, err := NewSettingsFreshnessSignal().Run(t.Context(), fixture.settings)
	if err != nil || len(freshnessAlerts) != 0 {
		t.Fatal("unchanged composed freshness callback failed")
	}
	currentRecords := settingsWitnessTestRecords(t, &output)
	if len(currentRecords) != 1 || currentRecords[0].Result != "current" || !currentRecords[0].CompleteSettingsEqual {
		t.Fatal("actual freshness callback did not retain its own affirmative result")
	}
	// The writer's sequence has now changed. The next guard read must remain
	// observable, preserve its incident hold and create no duplicate record.
	findings := fixture.run(t)
	for _, finding := range findings {
		if finding.class == "cannot-observe" {
			t.Fatal("writer state leaked into captured guard generation")
		}
	}
	if fixture.calls.Load() != 3 || len(settingsWitnessTestRecords(t, &output)) != 1 {
		t.Fatal("writer state changed guard contact or witness scheduling")
	}
	prior := fixture.probe.snapshotState()
	changeMimirAdmissionUpstreamCurrentSettings(fixture, "overlay")
	fixture.now = fixture.now.Add(mimirAdmissionQuietWindow)
	blocked := fixture.run(t)
	requireMimirAdmissionGenerationUnknown(t, blocked)
	requireMimirAdmissionGenerationClass(t, blocked, "mimir-series-limit")
	requireMimirAdmissionGenerationClass(t, blocked, "mimir-ingestion-rate-limit")
	after := fixture.probe.snapshotState()
	if fixture.calls.Load() != 3 || !reflect.DeepEqual(after.histories, prior.histories) ||
		!after.incident || !after.rateIncident || !after.quietSince.IsZero() || !after.rateQuietSince.IsZero() ||
		len(settingsWitnessTestRecords(t, &output)) != 1 {
		t.Fatal("an earlier witness authorized stale contact, recovery or another record")
	}
	freshnessAlerts, err = NewSettingsFreshnessSignal().Run(t.Context(), fixture.settings)
	if err != nil {
		t.Fatal("changed composed freshness callback failed")
	}
	requireAlertClass(t, freshnessAlerts, "settings-generation-stale")
	records := settingsWitnessTestRecords(t, &output)
	if len(records) != 2 || records[1].Result != "stale" || records[1].CompleteSettingsEqual ||
		records[1].RunId != records[0].RunId || records[1].SnapshotId != records[0].SnapshotId ||
		records[0].Sequence != 1 || records[1].Sequence != 2 || fixture.calls.Load() != 3 {
		t.Fatal("freshness did not preserve its own distinct same-run stale result")
	}
}
