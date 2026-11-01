package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The release owner's enrollment with planned sweep epochs, through the real
// monitor.yml projection.
func subnetSettlementPlannedEnrollment(t *testing.T, planned string) *SubnetReplaySettings {
	t.Helper()
	raw := fmt.Sprintf(`subnet_replay:
  schema: %s
  api_origins: ["https://first.replay.example.test", "https://second.replay.example.test"]
  replay_from_block: %d
  planned_v1_sweep_epochs: %s
`, subnetReplayEnrollmentSchema, subnetSettlementTestDeployBlock, planned)
	parsed, err := decodeMonitorYaml([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	loaded := subnetReplaySettingsFromNode(parsed.SubnetReplay)
	if loaded == nil {
		t.Fatal("the enrollment block was dropped")
	}
	return loaded
}

// The review case: a planned sweep that never closes or commits must not be
// silent. The planned post-cutover epoch keeps every ordinary v1 rule: an
// unset epoch warns while its close is at risk, warns once its close grace
// and root window pass, and pages once its finalization is overdue.
func TestSubnetSettlementPlannedSweepUnsetEpochWarns(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, step := range []struct {
		offset   uint64
		frame    string
		severity Severity
		symptom  string
	}{
		{offset: 10, frame: "epoch-2/close", severity: SeverityWarn, symptom: "at risk of missing its grace window"},
		{offset: 200, frame: "epoch-2/close", severity: SeverityWarn, symptom: "missed its epoch close"},
		{offset: 200, frame: "epoch-2/root", severity: SeverityWarn, symptom: "missed its payout root window"},
		{offset: 850, frame: "epoch-2/finalization", severity: SeverityPage, symptom: "finalization is overdue"},
	} {
		t.Run(fmt.Sprintf("%s/offset=%d", step.frame, step.offset), func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}]", first))
			alerts := f.run(t, f.chain.end(first)+step.offset)
			alert := requireSubnetReplayAlert(t, alerts, "subnet-epoch-progress", "subnet-settlement/no-1", step.frame)
			if alert.Severity != step.severity || !strings.Contains(alert.Symptom, step.symptom) {
				t.Fatalf("planned sweep epoch alert=%s %q", alert.Severity, alert.Symptom)
			}
		})
	}
}

// A planned sweep epoch that closed but has no root by its window warns at
// risk and again once the window passes, exactly as a v1 epoch does.
func TestSubnetSettlementPlannedSweepWithoutRootWarns(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, step := range []struct {
		offset  uint64
		symptom string
	}{
		{offset: 100, symptom: "at risk of missing its commit window"},
		{offset: 200, symptom: "missed its payout root window"},
	} {
		t.Run(fmt.Sprintf("offset=%d", step.offset), func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}]", first))
			end := f.chain.end(first)
			f.chain.capture(first, 1, 333, end)
			alerts := f.run(t, end+step.offset)
			alert := requireSubnetReplayAlert(t, alerts, "subnet-epoch-progress", "subnet-settlement/no-1", "epoch-2/root")
			if alert.Severity != SeverityWarn || alert.Sustain != 2 || !strings.Contains(alert.Symptom, step.symptom) {
				t.Fatalf("planned sweep root alert=%s %q", alert.Severity, alert.Symptom)
			}
			for _, other := range alerts {
				if other.Frame == "epoch-2/close" {
					t.Fatalf("a closed planned epoch warned about its close: %+v", other)
				}
			}
		})
	}
}

// Control: with a plan for a later epoch, an unplanned unset post-cutover
// epoch keeps the cutover rule and stays silent through its overdue band.
func TestSubnetSettlementUnplannedPostCutoverEpochStaysSilent(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, offset := range []uint64{10, 200, 850} {
		t.Run(fmt.Sprintf("offset=%d", offset), func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}]", first+1))
			requireSubnetSettlementOnlyV2ReaderGap(t, f.run(t, f.chain.end(first)+offset))
		})
	}
}

// With a plan, only a sweep-leaf root at the planned epoch is sanctioned: the
// planned sweep stays healthy beside a second sweep root, which pages alone,
// and a sweep root at an unplanned epoch pages even when it is unique.
func TestSubnetSettlementPlannedSweepPinsTheSanction(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	t.Run("planned-sweep-is-healthy", func(t *testing.T) {
		f := newSubnetSettlementCutoverFixture(t)
		f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}]", first))
		end := f.chain.end(first)
		f.commitSweep(first, 333, end+5)
		f.chain.finalize(first, 1, end+500)
		f.chain.claim(first, 1, f.selfColdkey, subnetSettlementBps, end+510, true)
		requireSubnetSettlementOnlyV2ReaderGap(t, f.run(t, end+850))
	})
	t.Run("unique-sweep-root-at-an-unplanned-epoch", func(t *testing.T) {
		f := newSubnetSettlementCutoverFixture(t)
		f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}]", first+1))
		end := f.chain.end(first)
		f.commitSweep(first, 333, end+5)
		alert := requireSubnetReplayAlert(t, f.run(t, end+200), "subnet-payout-root", "subnet-settlement/no-1", "epoch-2/post-cutover-root")
		if alert.Severity != SeverityPage || alert.Sustain != 1 || !strings.Contains(alert.Observed, "observed=sweep-leaf-root post_cutover_roots=1 first_v2_epoch=2 planned_sweep_epoch=3") {
			t.Fatalf("an unplanned sweep root was sanctioned: %+v", alert)
		}
	})
	t.Run("second-sweep-root-pages-alone", func(t *testing.T) {
		f := newSubnetSettlementCutoverFixture(t)
		f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}]", first))
		f.chain.currentEpoch = first + 2
		end := f.chain.end(first)
		f.commitSweep(first, 333, end+5)
		f.chain.finalize(first, 1, end+500)
		f.chain.claim(first, 1, f.selfColdkey, subnetSettlementBps, end+510, true)
		f.commitSweep(first+1, 50, f.chain.end(first+1)+5)
		alerts := f.run(t, f.chain.end(first+1)+200)
		alert := requireSubnetReplayAlert(t, alerts, "subnet-payout-root", "subnet-settlement/no-1", "epoch-3/post-cutover-root")
		if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "observed=sweep-leaf-root post_cutover_roots=2 first_v2_epoch=2 planned_sweep_epoch=2") {
			t.Fatalf("the second sweep root was not a page: %+v", alert)
		}
		for _, other := range alerts {
			if strings.HasPrefix(other.Frame, "epoch-2/") {
				t.Fatalf("the planned sweep lost its sanction: %+v", other)
			}
		}
	})
	t.Run("different-root-at-the-planned-epoch", func(t *testing.T) {
		f := newSubnetSettlementCutoverFixture(t)
		f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}]", first))
		end := f.chain.end(first)
		f.chain.capture(first, 1, 333, end)
		f.chain.commit(first, 1, end+5, [32]byte{0xc3})
		alert := requireSubnetReplayAlert(t, f.run(t, end+200), "subnet-payout-root", "subnet-settlement/no-1", "epoch-2/post-cutover-root")
		if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "observed=different-root post_cutover_roots=1 first_v2_epoch=2 planned_sweep_epoch=2") {
			t.Fatalf("a different root at the planned epoch was not a page: %+v", alert)
		}
	})
}

// The plan is strict: one positive epoch per positive operator, known keys
// only, at most sixteen entries. Anything else is the whole block's unknown.
func TestSubnetSettlementPlannedSweepEnrollmentIsStrict(t *testing.T) {
	for _, planned := range []string{"[]", "[{no_id: 1, epoch: 4}]", "[{no_id: 1, epoch: 4}, {no_id: 2, epoch: 5}]"} {
		if reason := subnetSettlementPlannedEnrollment(t, planned).enrollmentReason(); reason != "" {
			t.Errorf("valid plan %s was refused: %s", planned, reason)
		}
	}
	entries := []string{}
	for noId := 1; noId <= 17; noId++ {
		entries = append(entries, fmt.Sprintf("{no_id: %d, epoch: 4}", noId))
	}
	for _, planned := range []string{
		"[{no_id: 0, epoch: 4}]", "[{no_id: 1, epoch: 0}]", "[{no_id: 1}]", "[{epoch: 4}]",
		"[{no_id: 1, epoch: 4}, {no_id: 1, epoch: 5}]", "[{no_id: 1, epoch: 4, extra: 1}]",
		"[{no_id: 1, epoch: four}]", "[{no_id: -1, epoch: 4}]", "{no_id: 1, epoch: 4}", "4",
		"[" + strings.Join(entries, ", ") + "]",
	} {
		if reason := subnetSettlementPlannedEnrollment(t, planned).enrollmentReason(); reason != "enrollment-invalid" {
			t.Errorf("malformed plan %s was accepted: %q", planned, reason)
		}
	}
}

// The parsed plan keeps its values, a clone is independent, and a changed plan
// changes the settings generation.
func TestSubnetSettlementPlannedSweepValuesAndGeneration(t *testing.T) {
	loaded := subnetSettlementPlannedEnrollment(t, "[{no_id: 1, epoch: 4}, {no_id: 2, epoch: 5}]")
	planned := loaded.plannedV1SweepEpochs()
	if len(planned) != 2 || planned[1] != 4 || planned[2] != 5 {
		t.Fatalf("parsed plan=%v", planned)
	}
	clone := cloneSubnetReplaySettings(loaded)
	clone.PlannedV1SweepEpochs[0].Epoch = 9
	if loaded.PlannedV1SweepEpochs[0].Epoch != 4 {
		t.Fatal("a clone shares the plan")
	}
	if subnetSettlementPlannedEnrollment(t, "[{no_id: 1, epoch: 4}, {no_id: 1, epoch: 5}]").plannedV1SweepEpochs() != nil {
		t.Fatal("a malformed plan was applied")
	}
	_, _, settings, _ := newSubnetSettlementFixture(t)
	settings.SubnetReplay = loaded
	current := settings.withDefaults()
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if equal, err := check(context.Background(), settings); err != nil || !equal {
		t.Fatal("equal settings differ")
	}
	current.SubnetReplay.PlannedV1SweepEpochs[1].Epoch = 6
	if equal, err := check(context.Background(), settings); err != nil || equal {
		t.Fatal("a changed plan kept the settings generation")
	}
}

// A malformed block may hide a plan, so with settlement v2 declared the
// enrollment gap also leaves epoch progress unknown, and the unset planned
// epoch is withheld rather than cleared.
func TestSubnetSettlementMalformedPlanLeavesProgressUnknown(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	f := newSubnetSettlementCutoverFixture(t)
	f.enrollment = subnetSettlementPlannedEnrollment(t, fmt.Sprintf("[{no_id: 1, epoch: %d}, {no_id: 1, epoch: %d}]", first, first+1))
	alerts := f.run(t, f.chain.end(first)+200)
	gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "enrollment")
	if !strings.Contains(gap.Observed, "reason=enrollment-invalid") || !strings.Contains(gap.Observed, "subnet-epoch-progress") {
		t.Fatalf("malformed plan left progress decided: %+v", gap)
	}
	for _, alert := range alerts {
		if alert.Class == "subnet-epoch-progress" {
			t.Fatalf("a malformed plan decided a post-cutover row: %+v", alert)
		}
	}
}

// A plan that contradicts the chain cannot be the sweep: an epoch before the
// pool's first v2 epoch, or an operator outside the complete census. It is
// dropped and reported unknown, never applied to another epoch.
func TestSubnetSettlementInconsistentPlanIsUnknown(t *testing.T) {
	first := subnetSettlementCutoverTestFirst
	for _, tc := range []struct {
		name    string
		planned string
	}{
		{name: "before-first-v2-epoch", planned: fmt.Sprintf("[{no_id: 1, epoch: %d}]", first-1)},
		{name: "operator-outside-census", planned: fmt.Sprintf("[{no_id: 2, epoch: %d}]", first)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubnetSettlementCutoverFixture(t)
			f.enrollment = subnetSettlementPlannedEnrollment(t, tc.planned)
			alerts := f.run(t, f.chain.end(first)+850)
			gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-settlement/coverage", "planned-sweep")
			if !gap.unobservable || !strings.Contains(gap.Observed, "state=unknown reason=planned-sweep-inconsistent undecided_classes=subnet-epoch-progress,subnet-payout-root") ||
				!strings.Contains(gap.Context, "completeness=partial:planned-sweep-inconsistent") {
				t.Fatalf("inconsistent plan was not unknown: %+v", gap)
			}
			for _, alert := range alerts {
				if alert.Frame != "settlement-v2" && alert.Frame != "planned-sweep" {
					t.Fatalf("a dropped plan was applied: %s|%s|%s", alert.Class, alert.Target, alert.Frame)
				}
			}
		})
	}
}
