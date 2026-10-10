// A committed independent output is a retained witness, so observing a later
// hot-key contender must not repeatedly census the full financial frontier.
package model

import "testing"

// Only the first positive independent result is latched. The caller still
// requires contention, held native custody and a second actual owner/key witness.
func parallelPublicCloseObserveHeldFrontier(state parallelPublicCloseState, observe func() parallelPublicCloseState) parallelPublicCloseState {
	if state.Independent > 0 {
		return state
	}
	return observe()
}

// The second observer call is a deterministic tripwire for the pre-fix loop:
// further contention polls must retain the committed witness without SQL.
func TestParallelPublicCloseHeldFrontierStopsAfterIndependentOutput(t *testing.T) {
	want := parallelPublicCloseState{Reports: 12, Settled: 5, Journals: 3, Pending: 7, ProviderDone: 2, Independent: 1}
	calls := 0
	observe := func() parallelPublicCloseState {
		calls++
		if calls > 1 {
			t.Fatal("full frontier queried again after committed independent progress")
		}
		return want
	}
	state := parallelPublicCloseState{}
	for range 64 {
		state = parallelPublicCloseObserveHeldFrontier(state, observe)
		if state != want {
			t.Fatal("retained independent output witness changed", state)
		}
	}
	if calls != 1 {
		t.Fatal("independent witness was not observed exactly once", calls)
	}
}

// Reports, settled outcomes and provider counts alone do not prove a complete
// independent output. Keep observing until the actual snapshot predicate holds.
func TestParallelPublicCloseHeldFrontierKeepsWaitingForIndependentOutput(t *testing.T) {
	states := []parallelPublicCloseState{
		{Reports: 8},
		{Reports: 8, Settled: 4},
		{Reports: 8, Settled: 4, ProviderDone: 4},
		{Reports: 8, Settled: 4, ProviderDone: 4, Independent: 1},
	}
	calls := 0
	observe := func() parallelPublicCloseState {
		if calls == len(states) {
			t.Fatal("full frontier queried after the first independent output")
		}
		state := states[calls]
		calls++
		return state
	}
	state := parallelPublicCloseState{}
	for index, want := range states {
		state = parallelPublicCloseObserveHeldFrontier(state, observe)
		if state != want || calls != index+1 {
			t.Fatal("partial pipeline evidence replaced the independent output predicate", index, state, calls)
		}
	}
	if state = parallelPublicCloseObserveHeldFrontier(state, observe); state != states[len(states)-1] || calls != len(states) {
		t.Fatal("positive independent output was not retained", state, calls)
	}
}
