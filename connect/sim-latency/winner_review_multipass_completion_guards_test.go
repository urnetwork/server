// The last retained case advances a deterministic clock exactly to the owner
// boundary. No next provider call exists to catch this terminal custody expiry.
package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestWinnerMultipassFinalCaseRetentionAtDeadlineNeverCompletes(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	actor := winnerMultipassQualificationActorFixture(t, plan)
	late := false
	originalNow, originalRetain := actor.Hooks.Now, actor.Hooks.RetainCase
	actor.Hooks.Now = func() time.Time {
		if late {
			return plan.Owner.Deadline
		}
		return originalNow()
	}
	actor.Hooks.RetainCase = func(position int, evidence winnerMultipassEvidence) (currentQgQualificationPin, error) {
		pin, err := originalRetain(position, evidence)
		if position == plan.Cases[len(plan.Cases)-1].Position {
			late = true
		}
		return pin, err
	}
	content, _ := json.Marshal(plan)
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	report, err := executeWinnerMultipassQualification(context.Background(), plan, currentQgQualificationPin{Path: "/synthetic/terminal-clock-plan.json", Sha256: winnerMultipassSha256(content)}, settings, actor.Hooks)
	if !late || len(actor.Invoked) != len(plan.Invocations) {
		t.Fatal("synthetic last-retention boundary was not reached")
	}
	if err == nil || report == nil || report.Complete || report.Qualified {
		t.Fatal("qualification completed after final case retention reached its immutable deadline")
	}
}

func TestWinnerMultipassSpoolFinalCaseRetentionAtDeadlineNeverCompletes(t *testing.T) {
	actor := newWinnerMultipassSpoolActor(t)
	planPin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "terminal-clock-plan.json"), actor.Plan)
	if err != nil {
		t.Fatal(err)
	}
	hooks := actor.hooks()
	late := false
	originalNow, originalRetain := hooks.Now, hooks.RetainCase
	hooks.Now = func() time.Time {
		if late {
			return actor.Plan.Owner.Deadline
		}
		return originalNow()
	}
	hooks.RetainCase = func(position int, evidence winnerMultipassEvidence) (currentQgQualificationPin, error) {
		pin, err := originalRetain(position, evidence)
		if position == actor.Plan.Cases[len(actor.Plan.Cases)-1].Position {
			late = true
		}
		return pin, err
	}
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spool := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	report, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, settings, spool, hooks)
	if !late || actor.Invocations != len(actor.Plan.Invocations) {
		t.Fatal("synthetic final protected retention boundary was not reached")
	}
	if err == nil || report == nil || report.Complete || report.Qualified {
		t.Fatal("protected-spool qualification completed after final custody reached its immutable deadline")
	}
}
