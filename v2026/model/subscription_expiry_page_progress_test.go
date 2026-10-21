// Completed visits advance each scan lane; unrelated or incomplete failures do not.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A malformed diagnostic cannot replace the original page failure.
type expiryVisitPanicCause struct{}

func (*expiryVisitPanicCause) Error() string { return "synthetic broken cause" }
func (*expiryVisitPanicCause) Unwrap() error { panic("synthetic broken unwrap") }

// Failed rows keep their ordinary custody while every persisted scan lane can
// acknowledge a completed raw page. The caller's retained arguments stay immutable.
func TestExpiryCompletedVisitAdvancesEveryLane(t *testing.T) {
	epoch := time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC)
	cutoff := epoch.Add(72 * time.Hour)
	old := &ContractExpiryCursor{ScanBefore: epoch, Open: &ContractExpiryPosition{CreateTime: epoch.Add(-time.Hour), ContractId: expiryFairTestId(1)}}
	advanced := &ContractExpiryCursor{ScanBefore: epoch, Open: &ContractExpiryPosition{CreateTime: epoch.Add(-time.Minute), ContractId: expiryFairTestId(2)}}
	failure := &ForceCloseVisitError{cause: errors.New("synthetic visited row failure"), attemptedCloseCount: 1, complete: true}
	for _, lane := range []string{"historical", "recent", "fresh", "catchup"} {
		after := &ContractExpirySweepCursor{Historical: old, RecentAfter: epoch, FreshBefore: cutoff}
		switch lane {
		case "historical":
			after.HistoricalNext = true
		case "recent":
			after.Recent = old
		case "fresh":
			after.Fresh, after.FreshNext = old, true
		case "catchup":
			after.Catchup, after.CatchupTurn = old, 2
		}
		before, err := json.Marshal(after)
		server.Raise(err)
		count, next, err := forceCloseContractExpiryFreshPage(cutoff, cutoff.Add(time.Hour), after,
			func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) { return 1, advanced, failure })
		if count != 1 || err != failure || next == nil {
			t.Fatal("completed visit lost its original failure or lane continuation", lane, count, next, err)
		}
		var actual *ContractExpiryCursor
		switch lane {
		case "historical":
			actual = next.Historical
		case "recent":
			actual = next.Recent
		case "fresh":
			actual = next.Fresh
		case "catchup":
			actual = next.Catchup
		}
		retained, marshalErr := json.Marshal(after)
		if actual != advanced || marshalErr != nil || !bytes.Equal(before, retained) {
			t.Fatal("lane checkpoint lost progress or mutated prior task arguments", lane)
		}
	}
}

// A page's returned cursor is insufficient without complete visitation and
// bounded causes. Parent cancellation withdraws even an earlier valid witness.
func TestExpiryVisitCheckpointRejectsIncompleteWork(t *testing.T) {
	cause := errors.New("synthetic visited row failure")
	valid := &ForceCloseVisitError{cause: cause, complete: true}
	cycle := &ForceCloseVisitError{complete: true}
	cycle.cause = cycle
	wide := make([]error, 128)
	for index := range wide {
		wide[index] = cause
	}
	var deep error = cause
	for range 40 {
		deep = fmt.Errorf("synthetic layer: %w", deep)
	}
	for _, err := range []error{
		cause, errors.Join(valid, cause), fmt.Errorf("synthetic outer: %w", valid),
		&ForceCloseVisitError{cause: cause},
		&ForceCloseVisitError{cause: deep, complete: true},
		&ForceCloseVisitError{cause: errors.Join(wide...), complete: true}, cycle,
		&ForceCloseVisitError{complete: true},
		&ForceCloseVisitError{cause: context.Canceled, complete: true},
		&ForceCloseVisitError{cause: context.DeadlineExceeded, complete: true},
		&ForceCloseVisitError{cause: server.DbContextDoneError, complete: true},
		&ForceCloseVisitError{cause: &expiryVisitPanicCause{}, complete: true},
		&ForceCloseVisitError{cause: &expiryVisitRuntimePanic{}, complete: true},
		&ForceCloseVisitError{cause: fmt.Errorf("synthetic returned runtime cause: %w", &expiryVisitRuntimePanic{}), complete: true},
		&ForceCloseVisitError{cause: errors.Join(cause, &expiryVisitRuntimePanic{}), complete: true},
	} {
		if forceClosePageCanAdvance(err) {
			t.Fatal("unattested or incomplete work acquired a raw checkpoint")
		}
	}
	before := &ContractExpiryCursor{ScanBefore: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	after := &ContractExpiryCursor{ScanBefore: before.ScanBefore, OpenDone: true}
	for _, cancelParent := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		_, cursor, err := forceCloseContractPagesBudgeted(ctx, 1, before, time.Minute, 1, time.Now,
			func(int, *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				if cancelParent {
					cancel()
					return 0, after, valid
				}
				return 0, after, errors.New("synthetic selection failure")
			})
		cancel()
		if cursor != before || err == nil {
			t.Fatal("selection failure or parent cancellation advanced an incomplete page")
		}
	}
	_, cursor, err := forceCloseContractPagesBudgeted(t.Context(), 1, before, time.Minute, 1, time.Now,
		func(int, *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			return 0, after, &ForceCloseVisitError{cause: cause, attemptedCloseCount: 1, complete: true}
		})
	if cursor != before || err == nil {
		t.Fatal("inconsistent attempted-close evidence acquired a checkpoint")
	}
}
