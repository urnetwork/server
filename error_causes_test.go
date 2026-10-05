// Real unwrap graphs exercise finite work without invoking custom matching.
package server

import (
	"context"
	"errors"
	"net"
	"testing"
)

// Constant diagnostics keep cyclic fixture formatting independent of traversal.
type errorCauseTestOne struct {
	cause error
	reads *int
}

func (self *errorCauseTestOne) Error() string { return "synthetic one cause" }
func (self *errorCauseTestOne) Unwrap() error {
	if self.reads != nil {
		*self.reads++
	}
	return self.cause
}

// A joined wrapper deliberately retains nil branches that errors.Join omits.
type errorCauseTestMany struct{ causes []error }

func (self *errorCauseTestMany) Error() string   { return "synthetic joined causes" }
func (self *errorCauseTestMany) Unwrap() []error { return self.causes }

// Match methods must never grant authority or receive arbitrary traversal.
type errorCauseTestMatch struct{}

func (self *errorCauseTestMatch) Error() string { return "synthetic untrusted matcher" }
func (self *errorCauseTestMatch) Is(error) bool { panic("custom Is invoked") }
func (self *errorCauseTestMatch) As(any) bool   { panic("custom As invoked") }

// Only unwraps, never diagnostic text, construct depth.
func errorCauseTestDepth(count int, leaf error) error {
	for range count {
		leaf = &errorCauseTestOne{cause: leaf}
	}
	return leaf
}

// Malformed or truncated graphs remain unknown; observed nodes remain bounded.
func TestErrorCauseInspectionBoundsCyclesDepthWidthAndNil(t *testing.T) {
	reads := 0
	cycle := &errorCauseTestOne{reads: &reads}
	cycle.cause = cycle
	wide := make([]error, 256)
	for index := range wide {
		wide[index] = context.DeadlineExceeded
	}
	var typedNil *errorCauseTestOne
	for index, cause := range []error{nil, typedNil, cycle, errorCauseTestDepth(40, context.DeadlineExceeded),
		&errorCauseTestMany{causes: wide}, &errorCauseTestMany{},
		&errorCauseTestMany{causes: []error{nil, nil}}, &errorCauseTestOne{}} {
		got := InspectErrorCauses(cause)
		if got.Complete || len(got.Nodes) > 128 {
			t.Fatal("incomplete cause graph gained a complete census", index, got.Complete, len(got.Nodes))
		}
	}
	if reads != 32 {
		t.Fatal("cyclic unwrap did not stop at its depth budget", reads)
	}
	for _, count := range []int{1, 32} {
		got := InspectErrorCauses(errorCauseTestDepth(count, context.DeadlineExceeded))
		if !got.Complete || len(got.Nodes) != count+1 || !got.Nodes[count].Leaf {
			t.Fatal("complete graph within exact depth budget was withheld", count)
		}
	}
	got := InspectErrorCauses(&errorCauseTestMany{causes: wide[:127]})
	if !got.Complete || len(got.Nodes) != 128 {
		t.Fatal("complete graph at exact node budget was withheld")
	}
	dns := &net.DNSError{Err: "synthetic timeout", IsTimeout: true}
	got = InspectErrorCauses(dns)
	if !got.Complete || len(got.Nodes) != 1 || !got.Nodes[0].Leaf || got.Nodes[0].Err != dns {
		t.Fatal("standard DNS optional cause lost its typed leaf")
	}
}

// A cyclic sibling cannot hide an already observed hard cause or invoke Is/As.
func TestErrorCauseInspectionRetainsHardSiblingAndOriginalAncestry(t *testing.T) {
	cycle := &errorCauseTestOne{}
	cycle.cause = cycle
	hard := errors.New("synthetic hard authority refusal")
	matcher := &errorCauseTestMatch{}
	joined := &errorCauseTestMany{causes: []error{cycle, hard, matcher}}
	got := InspectErrorCauses(joined)
	if got.Complete || len(got.Nodes) < 4 || got.Nodes[0].Parent != -1 ||
		got.Nodes[2].Err != hard || got.Nodes[2].Parent != 0 || !got.Nodes[2].Leaf ||
		got.Nodes[3].Err != matcher || !got.Nodes[3].Leaf {
		t.Fatal("bounded inspection lost observed hard sibling or original ancestry")
	}
	got = InspectErrorCauses(&errorCauseTestMany{causes: []error{nil, hard}})
	if !got.Complete || got.NilBranches != 1 || len(got.Nodes) != 2 || got.Nodes[1].Err != hard {
		t.Fatal("mixed nil policy was not left explicit for the caller")
	}
}
