package model

// st_model_test.go — offline unit tests (no pg/redis) for the pure half of
// the head-tier epoch exclusion: the HeadBound/HeadUnbound interval replay
// behind GetHeadBoundCkeysInEpoch (HF-1), and the event-row ckey parser.

import (
	"fmt"
	"testing"
)

// testStCkey builds a distinct 32-byte ckey from a marker byte.
func testStCkey(marker byte) [32]byte {
	var ckey [32]byte
	ckey[0] = marker
	return ckey
}

// TestStHeadBoundCkeysFromEventsWindowOverlap covers the interval-overlap
// semantics: a ckey is excluded from the pool payout iff its bound interval
// touches ANY block of [startBlock, closeBlock].
func TestStHeadBoundCkeysFromEventsWindowOverlap(t *testing.T) {
	const startBlock, closeBlock = 1000, 2000

	boundAllEpoch := testStCkey(1)    // bound before the window, never unbound
	unboundBefore := testStCkey(2)    // bound + unbound entirely before the window
	unboundAtStart := testStCkey(3)   // unbound exactly at startBlock → bound at startBlock
	unboundJustPrior := testStCkey(4) // unbound at startBlock-1 → never bound in window
	boundMidEpoch := testStCkey(5)    // bound inside the window, still bound at close
	boundAfterClose := testStCkey(6)  // bound only after closeBlock (defense in depth;
	// the SQL read already filters block ≤ closeBlock)

	events := []StHeadEvent{
		{Ckey: boundAllEpoch, Bound: true, Block: 10},
		{Ckey: unboundBefore, Bound: true, Block: 10},
		{Ckey: unboundBefore, Bound: false, Block: 500},
		{Ckey: unboundAtStart, Bound: true, Block: 10},
		{Ckey: unboundAtStart, Bound: false, Block: startBlock},
		{Ckey: unboundJustPrior, Bound: true, Block: 10},
		{Ckey: unboundJustPrior, Bound: false, Block: startBlock - 1},
		{Ckey: boundMidEpoch, Bound: true, Block: 1500},
		{Ckey: boundAfterClose, Bound: true, Block: closeBlock + 1},
	}

	got := StHeadBoundCkeysFromEvents(events, startBlock, closeBlock)

	want := map[[32]byte]bool{
		boundAllEpoch:  true,
		unboundAtStart: true,
		boundMidEpoch:  true,
	}
	for ckey, in := range want {
		if got[ckey] != in {
			t.Fatalf("ckey %x: excluded=%v, want %v", ckey[0], got[ckey], in)
		}
	}
	for _, ckey := range [][32]byte{unboundBefore, unboundJustPrior, boundAfterClose} {
		if got[ckey] {
			t.Fatalf("ckey %x: excluded, want not excluded", ckey[0])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("excluded set size %d, want %d (%v)", len(got), len(want), got)
	}
}

// TestStHeadBoundCkeysFromEventsUnbindDodge is the HF-1 double-pay regression:
// a provider bound for the whole epoch that calls unbindHead one block before
// close (no longer `active` at payout compute time) earned head emission for
// ~every tempo of the epoch and MUST stay excluded from the pool payout.
func TestStHeadBoundCkeysFromEventsUnbindDodge(t *testing.T) {
	const startBlock, closeBlock = 1000, 2000
	dodger := testStCkey(7)
	events := []StHeadEvent{
		{Ckey: dodger, Bound: true, Block: 10},
		{Ckey: dodger, Bound: false, Block: closeBlock - 1},
	}
	if got := StHeadBoundCkeysFromEvents(events, startBlock, closeBlock); !got[dodger] {
		t.Fatal("unbind at close-1 must NOT dodge the pool exclusion (HF-1)")
	}
}

// TestStHeadBoundCkeysFromEventsMultiCycle: multiple bind/unbind cycles —
// exclusion holds iff any cycle's interval overlaps the window, including a
// re-bind inside the window after a pre-window unbind.
func TestStHeadBoundCkeysFromEventsMultiCycle(t *testing.T) {
	const startBlock, closeBlock = 1000, 2000

	rebindInWindow := testStCkey(8)  // cycle before the window + cycle inside it
	cyclesBefore := testStCkey(9)    // two full cycles, both before the window
	sameBlockCycle := testStCkey(10) // bind→unbind within one in-window block (log order)

	events := []StHeadEvent{
		{Ckey: rebindInWindow, Bound: true, Block: 10},
		{Ckey: rebindInWindow, Bound: false, Block: 20},
		{Ckey: rebindInWindow, Bound: true, Block: 1500},
		{Ckey: rebindInWindow, Bound: false, Block: 1600},
		{Ckey: cyclesBefore, Bound: true, Block: 10},
		{Ckey: cyclesBefore, Bound: false, Block: 20},
		{Ckey: cyclesBefore, Bound: true, Block: 30},
		{Ckey: cyclesBefore, Bound: false, Block: 40},
		{Ckey: sameBlockCycle, Bound: true, Block: 1500},
		{Ckey: sameBlockCycle, Bound: false, Block: 1500},
	}

	got := StHeadBoundCkeysFromEvents(events, startBlock, closeBlock)
	if !got[rebindInWindow] {
		t.Fatal("re-bind inside the window must be excluded")
	}
	if got[cyclesBefore] {
		t.Fatal("cycles entirely before the window must not be excluded")
	}
	if !got[sameBlockCycle] {
		t.Fatal("a same-block bind→unbind inside the window was bound at that block — excluded")
	}
	// duplicate Bound without an Unbound between (rebind re-point) keeps the
	// earliest `since` — still one bound interval
	dupBound := testStCkey(11)
	got = StHeadBoundCkeysFromEvents([]StHeadEvent{
		{Ckey: dupBound, Bound: true, Block: 10},
		{Ckey: dupBound, Bound: true, Block: 1500},
	}, startBlock, closeBlock)
	if !got[dupBound] {
		t.Fatal("duplicate Bound must stay one bound-through-close interval")
	}

	if got := StHeadBoundCkeysFromEvents(nil, startBlock, closeBlock); len(got) != 0 {
		t.Fatalf("empty history must exclude nothing, got %v", got)
	}
}

func TestParseHeadEventCkey(t *testing.T) {
	want := testStCkey(3)
	ckey, ok := parseHeadEventCkey(fmt.Sprintf(`{"ckey":"0x%x","hotkey":"0x00","uid":"5"}`, want))
	if !ok || ckey != want {
		t.Fatalf("parse = %x, %v", ckey, ok)
	}
	for _, malformed := range []string{
		``,                  // not json
		`{}`,                // missing ckey
		`{"ckey":"0x1234"}`, // wrong length
		`{"ckey":"0xzz"}`,   // not hex
		`{"ckey":12}`,       // wrong type
	} {
		if _, ok := parseHeadEventCkey(malformed); ok {
			t.Fatalf("parse(%q) must fail", malformed)
		}
	}
}
