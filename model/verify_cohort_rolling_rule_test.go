package model

// The rolling census's pure rules (sn VERIFY-COHORT-V2.md §2.4-2.5): when a
// member has left the validator's census, when the rolling census may start,
// and how the admission state reads. No database or Redis.

import (
	"testing"
)

func testVerifyCohortAssigned(epochs ...uint64) func(uint64) bool {
	set := map[uint64]bool{}
	for _, epoch := range epochs {
		set[epoch] = true
	}
	return func(epoch uint64) bool { return set[epoch] }
}

// A member last assigned in server epoch s is in validator windows s-1 to s+1
// at most, and leaves at the fold of the first assigned window w >= s+2: the
// validator's epoch must be past w, and the retirement rule must have been
// running for w (w >= firstRetiring).
func TestVerifyCohortRollingExpiresTwoFoldsAfterTheLastAssignment(t *testing.T) {
	every := testVerifyCohortAssigned(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)
	for _, test := range []struct {
		name          string
		last          uint64
		known         bool
		vstar         uint64
		firstRetiring uint64
		assigned      func(uint64) bool
		expires       bool
	}{
		{name: "the fold of s+2 has not happened", last: 5, known: true, vstar: 7, firstRetiring: 3, assigned: every},
		{name: "the validator folded s+2", last: 5, known: true, vstar: 8, firstRetiring: 3, assigned: every, expires: true},
		{name: "assigned this epoch", last: 8, known: true, vstar: 8, firstRetiring: 3, assigned: every},
		{name: "an empty s+2 never counts", last: 5, known: true, vstar: 8, firstRetiring: 3, assigned: testVerifyCohortAssigned(5, 6, 8)},
		{name: "the next assigned window counts once folded", last: 5, known: true, vstar: 9, firstRetiring: 3, assigned: testVerifyCohortAssigned(5, 6, 8), expires: true},
		{name: "a window folded before the rule ran never counts", last: 1, known: true, vstar: 4, firstRetiring: 4, assigned: every},
		{name: "the first retiring window counts", last: 1, known: true, vstar: 5, firstRetiring: 4, assigned: every, expires: true},
		{name: "a legacy member leaves at the first retiring fold", known: false, vstar: 4, firstRetiring: 3, assigned: every, expires: true},
		{name: "a legacy member waits for the first retiring fold", known: false, vstar: 3, firstRetiring: 3, assigned: every},
		{name: "a legacy member waits for an assigned retiring window", known: false, vstar: 4, firstRetiring: 3, assigned: testVerifyCohortAssigned(1, 2, 4)},
		{name: "no validator epoch frees nothing", last: 0, known: true, vstar: 0, firstRetiring: 0, assigned: every},
		{name: "nothing past the lookback counts", last: 0, known: true, vstar: 12, firstRetiring: 0, assigned: testVerifyCohortAssigned(2, 3)},
	} {
		if expires := verifyCohortRollingExpires(test.last, test.known, test.vstar, test.firstRetiring, test.assigned); expires != test.expires {
			t.Fatalf("%s: expires %t, want %t", test.name, expires, test.expires)
		}
	}
}

// Boundary skew, exhaustively: whatever epoch the validator recorded an
// assignment in (the server's label s, one either side), the server counts the
// member at every validator epoch the validator can still hold it.
func TestVerifyCohortRollingNeverFreesAMemberTheValidatorHolds(t *testing.T) {
	every := testVerifyCohortAssigned(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)
	const firstRetiring = 2
	for last := uint64(2); last <= 10; last++ {
		for window := last - 1; window <= last+1; window++ {
			// the validator holds the member until it folds the first assigned
			// window after the one it recorded it in, from firstRetiring on
			retiredAt := max(window+1, firstRetiring) + 1
			for vstar := uint64(1); vstar < retiredAt; vstar++ {
				if verifyCohortRollingExpires(last, true, vstar, firstRetiring, every) {
					t.Fatalf("assigned in server epoch %d, held in validator window %d: freed at validator epoch %d before its retirement at %d", last, window, vstar, retiredAt)
				}
			}
		}
	}
}

func TestVerifyCohortRollingStartsAfterTheFirstRetiringFold(t *testing.T) {
	census := uint64(150)
	first := uint64(7)
	for _, test := range []struct {
		name   string
		report *VerifyCohortValidatorReport
		start  uint64
		ok     bool
	}{
		{name: "no report"},
		{name: "no census", report: &VerifyCohortValidatorReport{SettlementEpoch: 9, FirstCensusEpoch: &first}},
		{name: "census, not folded since", report: &VerifyCohortValidatorReport{SettlementEpoch: 7, Census: &census, FirstCensusEpoch: &first}},
		{name: "census, folded since", report: &VerifyCohortValidatorReport{SettlementEpoch: 8, Census: &census, FirstCensusEpoch: &first}, start: 8, ok: true},
		{name: "census, folded twice since", report: &VerifyCohortValidatorReport{SettlementEpoch: 9, Census: &census, FirstCensusEpoch: &first}, start: 8, ok: true},
	} {
		start, ok := VerifyCohortRollingStart(test.report)
		if start != test.start || ok != test.ok {
			t.Fatalf("%s: start %d %t, want %d %t", test.name, start, ok, test.start, test.ok)
		}
	}
}

func TestVerifyCohortRollingFieldsReadOnlyAnExactOn(t *testing.T) {
	for _, test := range []struct {
		values   []any
		rolling  bool
		headroom int
	}{
		{values: []any{"1", "14"}, rolling: true, headroom: 14},
		{values: []any{nil, nil}},
		{values: []any{"0", "3"}, headroom: 3},
		{values: []any{"true", "-1"}},
		{values: []any{"1", "x"}, rolling: true},
		{values: []any{"1"}},
	} {
		rolling, headroom := verifyCohortRollingFields(test.values)
		if rolling != test.rolling || headroom != test.headroom {
			t.Fatalf("%v: %t %d, want %t %d", test.values, rolling, headroom, test.rolling, test.headroom)
		}
	}
}

func TestVerifyCohortRefusalLabels(t *testing.T) {
	want := map[verifyCohortRefusal]string{
		verifyCohortRefusedEpochFull:   "epoch_full",
		verifyCohortRefusedRollingFull: "rolling_full",
		verifyCohortRefusedInterlock:   "interlock",
		verifyCohortRefusedCensusFull:  "census_full",
	}
	for refusal, label := range want {
		if got, counted := refusal.label(); !counted || got != label {
			t.Fatalf("refusal %d labels %q %t", refusal, got, counted)
		}
	}
	for _, refusal := range []verifyCohortRefusal{verifyCohortAdmitted, verifyCohortRefusedGrowthGate} {
		if _, counted := refusal.label(); counted {
			t.Fatalf("refusal %d is counted", refusal)
		}
	}
	if verifyCohortInterlockLimit != 4096*7/8 {
		t.Fatal("the interlock is not 7/8 of the validator's 4,096", verifyCohortInterlockLimit)
	}
}
