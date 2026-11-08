// Independent phase deadlines are inspected directly. Archive and retirement
// are synthetic exact-version operations, never production network requests.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A replay-only timeout occurs after verified protection and local retirement.
// Its durable compact pin remains visible and no successor may consume a claim.
func TestWinnerMultipassSpoolPostCommitReplayHasFreshDeadlineAndRetainsFailurePin(t *testing.T) {
	actor := newWinnerMultipassSpoolActor(t)
	planPin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "phase-plan.json"), actor.Plan)
	if err != nil {
		t.Fatal(err)
	}
	hooks := actor.hooks()
	retainer, resolver := hooks.Retain, hooks.Resolve
	var protectionCtx context.Context
	var retiredAt time.Time
	var committed currentQgQualificationPin
	hooks.Retain = func(ctx context.Context, capture winnerMultipassQualificationCapture) (currentQgQualificationPin, error) {
		protectionCtx = ctx
		pin, err := retainer(ctx, capture)
		if err == nil {
			committed = pin
			retiredAt = time.Now()
		}
		return pin, err
	}
	replays := 0
	hooks.Resolve = func(ctx context.Context, pin currentQgQualificationPin) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
		replays++
		deadline, bounded := ctx.Deadline()
		observedAt := time.Now()
		want := actor.Plan.Settings.Custody.PostCommitReplayTimeout
		if protectionCtx == nil || protectionCtx.Err() != context.Canceled || ctx.Err() != nil || !bounded || deadline.Before(retiredAt.Add(want)) || deadline.After(observedAt.Add(want)) {
			t.Fatal("postcommit replay borrowed the canceled protection context or its spent deadline")
		}
		if pin != committed || len(actor.Pending) != 0 || actor.Commits[1] != pin {
			t.Fatal("replay started before durable custody and local retirement")
		}
		return nil, context.DeadlineExceeded
	}
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spool := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	report, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, capacity, spool, hooks)
	if !errors.Is(err, context.DeadlineExceeded) || report == nil || report.Complete || report.Qualified || replays != 1 || len(report.CaptureCommits) != 1 || report.CaptureCommits[0] != committed || actor.Invocations != 1 || actor.Generations != 0 || len(actor.Claims) != 1 || len(actor.Pending) != 0 {
		t.Fatal("postcommit-only timeout lost durable evidence or admitted a successor", err)
	}
	original, err := resolver(context.Background(), committed)
	if err != nil || original == nil || original.PhysicalAuthentication == nil || len(original.Originals) == 0 {
		t.Fatal("failed replay erased an already protected original", err)
	}
}

// Only protection is allowed to finish already-owned originals after parent
// cancellation. The subsequent replay never detaches from the original owner.
func TestWinnerMultipassSpoolPostCommitReplayPreservesCanceledOwner(t *testing.T) {
	actor := newWinnerMultipassSpoolActor(t)
	planPin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "cancel-owner-plan.json"), actor.Plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks := actor.hooks()
	retainer := hooks.Retain
	hooks.Retain = func(protectionCtx context.Context, capture winnerMultipassQualificationCapture) (currentQgQualificationPin, error) {
		cancel()
		if protectionCtx.Err() != nil {
			t.Fatal("parent cancellation abandoned already-owned original protection")
		}
		return retainer(protectionCtx, capture)
	}
	replays := 0
	hooks.Resolve = func(context.Context, currentQgQualificationPin) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
		replays++
		return nil, errors.New("unexpected replay after owner cancellation")
	}
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spool := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	report, err := executeWinnerMultipassSpoolQualification(ctx, actor.Plan, planPin, capacity, spool, hooks)
	if !errors.Is(err, context.Canceled) || report == nil || report.Complete || report.Qualified || replays != 0 || len(report.CaptureCommits) != 1 || actor.Invocations != 1 || len(actor.Claims) != 1 || len(actor.Pending) != 0 {
		t.Fatal("canceled owner lost protected custody or authorized new replay/work", err)
	}
}

// Both phases must be explicit positive settings; omitted old settings cannot
// acquire an implicit new allowance, and each prospective change affects hashes.
func TestWinnerMultipassCustodySettingsAreExplicitIndependentAndPinned(t *testing.T) {
	defaults := defaultWinnerMultipassSettings()
	if defaults.Custody.ProtectionTimeout != 120*time.Second || defaults.Custody.PostCommitReplayTimeout != 120*time.Second || validateWinnerMultipassSettings(defaults) != nil {
		t.Fatal("independent custody defaults changed")
	}
	original, _ := json.Marshal(defaults)
	for _, custody := range []winnerMultipassCustodySettings{
		{ProtectionTimeout: 0, PostCommitReplayTimeout: time.Minute},
		{ProtectionTimeout: time.Minute, PostCommitReplayTimeout: 0},
		{ProtectionTimeout: -time.Second, PostCommitReplayTimeout: time.Minute},
		{ProtectionTimeout: time.Minute, PostCommitReplayTimeout: -time.Second},
		{ProtectionTimeout: 121 * time.Second, PostCommitReplayTimeout: time.Minute},
		{ProtectionTimeout: time.Minute, PostCommitReplayTimeout: 121 * time.Second},
	} {
		changed := defaults
		changed.Custody = custody
		if validateWinnerMultipassSettings(changed) == nil {
			t.Fatal("missing, negative or unbounded custody phase accepted", custody)
		}
	}
	for _, custody := range []winnerMultipassCustodySettings{
		{ProtectionTimeout: time.Minute, PostCommitReplayTimeout: 120 * time.Second},
		{ProtectionTimeout: 120 * time.Second, PostCommitReplayTimeout: time.Minute},
	} {
		changed := defaults
		changed.Custody = custody
		content, _ := json.Marshal(changed)
		if validateWinnerMultipassSettings(changed) != nil || winnerMultipassSha256(content) == winnerMultipassSha256(original) {
			t.Fatal("one custody phase changed without an explicit new settings identity")
		}
	}
}
