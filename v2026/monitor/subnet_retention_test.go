// SIGNALS.md §22.10 retention of the §22.1--§22.5 counterexamples on the
// shared latch: a healthy read, an unknown source or a withheld run cannot
// erase or open one, a reproduction keeps its first proof, and warnings are
// never retained.
package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func subnetRetentionPage(target string) finding {
	return finding{probeId: subnetHeadFamily.probeId, tier: tierPage, class: subnetHeadWeightBypassClass, target: target, sustain: subnetProvedSustain, observed: "state=violated invariant=head-weight-only-to-eligible-uids expected=eligible observed=self-weight"}
}

func TestSubnetRetentionHealthyReadCannotEraseAPublishedPage(t *testing.T) {
	latch := newSubnetReplayLatch()
	start := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	output, commit := latch.stage(start, []finding{subnetRetentionPage("subnet-head/validator-1")})
	if len(output) != 1 || strings.Contains(output[0].observed, "retained=true") {
		t.Fatalf("a first proof was rendered as retained: %+v", output)
	}
	commit()
	healthy := subnetHealthy(subnetHeadFamily, subnetHeadWeightBypassClass, "subnet-head/validator-1")
	output, _ = latch.stage(start.Add(5*time.Minute), []finding{healthy})
	replayed := 0
	for _, value := range output {
		if value.tier == tierPage && strings.Contains(value.observed, "retained=true first_proved=2026-10-23T00:00:00Z last_proved=2026-10-23T00:00:00Z current_read=absent") {
			replayed++
		}
	}
	if replayed != 1 {
		t.Fatalf("a healthy read erased the counterexample: %+v", output)
	}
}

func TestSubnetRetentionWithheldRunRecordsNothing(t *testing.T) {
	latch := newSubnetReplayLatch()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	// The commit is dropped, as a withheld run drops it.
	_, _ = latch.stage(now, []finding{subnetRetentionPage("subnet-head/validator-1")})
	output, _ := latch.stage(now.Add(time.Minute), nil)
	if len(output) != 0 {
		t.Fatalf("an unpublished counterexample was retained: %+v", output)
	}
}

func TestSubnetRetentionReproductionKeepsFirstProof(t *testing.T) {
	latch := newSubnetReplayLatch()
	first := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	_, commit := latch.stage(first, []finding{subnetRetentionPage("subnet-head/validator-1")})
	commit()
	output, commit := latch.stage(first.Add(5*time.Minute), []finding{subnetRetentionPage("subnet-head/validator-1")})
	commit()
	if len(output) != 1 || !strings.Contains(output[0].context, "first_proved=2026-10-23T00:00:00Z") || strings.Contains(output[0].observed, "retained=true") {
		t.Fatalf("a reproduction lost its first proof or was duplicated: %+v", output)
	}
	output, _ = latch.stage(first.Add(10*time.Minute), nil)
	if len(output) != 1 || !strings.Contains(output[0].observed, "last_proved=2026-10-23T00:05:00Z") {
		t.Fatalf("the last proof time did not advance: %+v", output)
	}
}

func TestSubnetRetentionWarningsAndUnknownsAreNotRetained(t *testing.T) {
	latch := newSubnetReplayLatch()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	warning := subnetRetentionPage("subnet-head/churn")
	warning.tier = tierWarn
	unknown := subnetUnknown(subnetHeadFamily, subnetHeadWeightBypassClass, "subnet-head/weights", "native-rpc-error", subnetTransientSustain, "the applied rows")
	_, commit := latch.stage(now, []finding{warning, unknown})
	commit()
	output, _ := latch.stage(now.Add(5*time.Minute), nil)
	if len(output) != 0 {
		t.Fatalf("a warning or an unknown state was retained: %+v", output)
	}
}

// An unknown source does not stand in for a reproduction: the retained page
// is still rendered beside the unknown state.
func TestSubnetRetentionUnknownReadKeepsThePage(t *testing.T) {
	latch := newSubnetReplayLatch()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	_, commit := latch.stage(now, []finding{subnetRetentionPage("subnet-head/validator-1")})
	commit()
	unknown := subnetUnknown(subnetHeadFamily, subnetHeadWeightBypassClass, "subnet-head/validator-1", "native-rpc-error", subnetTransientSustain, "the applied rows")
	output, _ := latch.stage(now.Add(5*time.Minute), []finding{unknown})
	pages := 0
	for _, value := range output {
		if value.tier == tierPage && strings.Contains(value.observed, "retained=true") {
			pages++
		}
	}
	if len(output) != 2 || pages != 1 {
		t.Fatalf("an unknown read hid the retained page: %+v", output)
	}
}

// Through a family run: a counterexample proved by a run whose settings
// generation went stale before it closed is never retained, and a published
// one is.
func TestSubnetRetentionFollowsPublication(t *testing.T) {
	world := newSubnetWorld(t)
	original := world.contracts[world.coordinator].views["owner"]
	world.contracts[world.coordinator].views["owner"] = subnetWorldConstant(subnetWorldAddress("attacker"))
	signal := NewSubnetChainSignal()
	stale := world.signalSettings()
	checks := 0
	stale.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		checks++
		// Current when the run opens, stale when it closes.
		return checks == 1, nil
	}
	alerts := runSubnetWorld(t, signal, stale)
	if checks != 2 || len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "settings-generation-stale") {
		t.Fatalf("the run was not withheld at its close: checks=%d %+v", checks, alerts)
	}
	world.contracts[world.coordinator].views["owner"] = original
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, world.signalSettings()))

	world.contracts[world.coordinator].views["owner"] = subnetWorldConstant(subnetWorldAddress("attacker"))
	requireSubnetAlert(t, runSubnetWorld(t, signal, world.signalSettings()), subnetDeploymentGovernanceClass, "subnet-chain/deployment", "", SeverityPage)
	world.contracts[world.coordinator].views["owner"] = original
	alert := requireSubnetAlert(t, runSubnetWorld(t, signal, world.signalSettings()), subnetDeploymentGovernanceClass, "subnet-chain/deployment", "", SeverityPage)
	if !strings.Contains(alert.Observed, "retained=true") {
		t.Fatalf("a published counterexample was not retained: %s", alert.Observed)
	}
}
