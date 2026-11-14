// Independent row budgets never turn an incomplete member into evidence.
package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A malformed member cannot interrupt receipt validation into success.
type errorCauseBatchPanic struct{}

func (*errorCauseBatchPanic) Error() string { return "synthetic malformed row" }
func (*errorCauseBatchPanic) Unwrap() error { panic("synthetic malformed unwrap") }

// The exact member list and original typed causes survive both input and
// returned-slice mutation; only explicit inspection gets independent budgets.
func TestErrorCauseBatchPreservesIndependentBoundsAndOriginalCauses(t *testing.T) {
	causes := make([]error, 512)
	for index := range causes {
		causes[index] = fmt.Errorf("synthetic row %d: %w", index, errors.New("synthetic proof failure"))
	}
	first, last := causes[0], causes[len(causes)-1]
	diagnostic := errors.Join(causes...).Error()
	batch := NewErrorCauseBatch(causes)
	if batch == nil || InspectErrorCauses(batch).Complete {
		t.Fatal("batch changed the ordinary global cause budget")
	}
	causes[0] = context.Canceled
	exposed := batch.Unwrap()
	exposed[len(exposed)-1] = context.DeadlineExceeded
	got := InspectErrorCauseBatch(batch)
	if !got.Complete || got.NilBranches != 0 || len(got.Nodes) != 1025 ||
		batch.Error() != diagnostic || !errors.Is(batch, first) || !errors.Is(batch, last) {
		t.Fatal("complete row receipt lost its independent bounds or original causes")
	}
	for index, node := range got.Nodes {
		if index == 0 {
			if node.Parent != -1 || node.Leaf {
				t.Fatal("batch became a diagnostic leaf")
			}
		} else if node.Parent < 0 || index <= node.Parent {
			t.Fatal("member inspection lost original cause ancestry", index, node.Parent)
		}
	}
}

// Even the final member keeps the full preexisting depth, width and malformed
// graph checks. Receipt construction is never a cached completeness claim.
func TestErrorCauseBatchRejectsMalformedLateMembersAndNestedBatches(t *testing.T) {
	leaf := errors.New("synthetic proof failure")
	cycle := &errorCauseTestOne{}
	cycle.cause = cycle
	wide := make([]error, 128)
	for index := range wide {
		wide[index] = leaf
	}
	var typedNil *errorCauseTestOne
	for index, invalid := range []error{nil, typedNil, cycle, errorCauseTestDepth(40, leaf),
		&errorCauseTestMany{causes: wide}, &errorCauseTestOne{}, &errorCauseTestMany{},
		&errorCauseTestMany{causes: []error{leaf, nil}}, &errorCauseBatchPanic{},
		NewErrorCauseBatch([]error{leaf})} {
		causes := make([]error, 512)
		for index := range causes {
			causes[index] = leaf
		}
		causes[len(causes)-1] = invalid
		if got := InspectErrorCauseBatch(NewErrorCauseBatch(causes)); got.Complete || len(got.Nodes) > 128+512*128 {
			t.Fatal("malformed late member acquired complete receipt authority", index)
		}
	}
	if NewErrorCauseBatch(nil) != nil || NewErrorCauseBatch(make([]error, 513)) != nil {
		t.Fatal("unbounded or empty receipt was constructed")
	}
	batch := NewErrorCauseBatch([]error{leaf})
	for _, invalid := range []error{&ErrorCauseBatch{}, errors.Join(batch, batch), errorCauseTestDepth(40, batch)} {
		if InspectErrorCauseBatch(invalid).Complete {
			t.Fatal("extra batch or incomplete outer graph gained authority")
		}
	}
	mutable := &errorCauseTestOne{cause: leaf}
	batch = NewErrorCauseBatch([]error{mutable, &errorCauseTestMatch{}})
	if !InspectErrorCauseBatch(batch).Complete {
		t.Fatal("ordinary complete member was refused")
	}
	mutable.cause = mutable
	if InspectErrorCauseBatch(batch).Complete {
		t.Fatal("changed row retained stale completeness evidence")
	}
}
