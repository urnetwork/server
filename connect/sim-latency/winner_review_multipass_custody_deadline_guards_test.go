// Deadline inspection exercises the existing executor hook before any archive
// operation. The same regression source compiles with either retained value type.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A direct deadline interval proves the owned allowance without sleeping or
// relying on an I/O timeout. Returning the sentinel prevents any next claim.
func winnerMultipassCustodyDeadlineProbe[T any](t *testing.T, authenticatedAt *time.Time, observed *bool, stop error, original func(context.Context, winnerMultipassQualificationCapture) (T, error)) func(context.Context, winnerMultipassQualificationCapture) (T, error) {
	t.Helper()
	_ = original
	return func(ctx context.Context, _ winnerMultipassQualificationCapture) (T, error) {
		*observed = true
		deadline, bounded := ctx.Deadline()
		observedAt := time.Now()
		want := 120 * time.Second
		if !bounded || authenticatedAt.IsZero() || deadline.Before(authenticatedAt.Add(want)) || deadline.After(observedAt.Add(want)) {
			t.Error("qualification protection inherited the legacy shared cleanup deadline instead of its own 120-second allowance")
		}
		if ctx.Err() != nil {
			t.Error("already-owned original custody lost its independent completion context")
		}
		var retained T
		return retained, stop
	}
}

// Full-local custody needs its own positive budget too; no protocol/call clock
// is changed and the deliberate retaining error must fence the next invocation.
func TestWinnerMultipassFullLocalProtectionHasIndependentCustodyDeadline(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	actor := winnerMultipassQualificationActorFixture(t, plan)
	var authenticatedAt time.Time
	authenticate := actor.Hooks.Authenticate
	actor.Hooks.Authenticate = func(input, output, diagnostics []byte, canceled bool) (*winnerMultipassContainmentReceipt, error) {
		receipt, err := authenticate(input, output, diagnostics, canceled)
		authenticatedAt = time.Now()
		return receipt, err
	}
	observed := false
	stop := errors.New("synthetic stop after protection deadline inspection")
	actor.Hooks.Retain = winnerMultipassCustodyDeadlineProbe(t, &authenticatedAt, &observed, stop, actor.Hooks.Retain)
	content, _ := json.Marshal(plan)
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	report, err := executeWinnerMultipassQualification(context.Background(), plan, currentQgQualificationPin{Path: "/synthetic/independent-custody-plan.json", Sha256: winnerMultipassSha256(content)}, capacity, actor.Hooks)
	if !observed || !errors.Is(err, stop) || report == nil || report.Complete || report.Qualified || len(actor.Invoked) != 1 || len(actor.Claims) != 1 {
		t.Fatal("full-local protection failure escaped its one-shot current invocation", err)
	}
}

// This is the actual spool executor boundary which formerly lent one cleanup
// deadline to archive protection, retirement and a second protected replay.
func TestWinnerMultipassSpoolProtectionHasIndependentCustodyDeadline(t *testing.T) {
	actor := newWinnerMultipassSpoolActor(t)
	planPin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "independent-custody-plan.json"), actor.Plan)
	if err != nil {
		t.Fatal(err)
	}
	hooks := actor.hooks()
	var authenticatedAt time.Time
	authenticate := hooks.Authenticate
	hooks.Authenticate = func(input, output, diagnostics []byte, canceled bool) (*winnerMultipassContainmentReceipt, error) {
		receipt, err := authenticate(input, output, diagnostics, canceled)
		authenticatedAt = time.Now()
		return receipt, err
	}
	observed := false
	stop := errors.New("synthetic stop before protected spool retirement")
	hooks.Retain = winnerMultipassCustodyDeadlineProbe(t, &authenticatedAt, &observed, stop, hooks.Retain)
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spool := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	report, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, capacity, spool, hooks)
	if !observed || !errors.Is(err, stop) || report == nil || report.Complete || report.Qualified || actor.Invocations != 1 || actor.Generations != 0 || len(actor.Claims) != 1 || len(actor.Pending) != 1 {
		t.Fatal("spool protection failure continued or discarded its original local attempt", err)
	}
}
