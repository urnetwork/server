package controller

// The pure part of the Top 200 claim's server side: which devices are chosen, and what
// a top-up pays. No database, no chain.

import (
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var (
	testingFleetAutoFleetId = [32]byte{0xf1}
	testingFleetAutoHotkey  = [32]byte{0xb1}
	testingFleetAutoNow     = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
)

// A candidate that asked at the base time: id n, prefix p, its own key.
func testingFleetAutoCandidate(n byte, prefix byte) *snFleetAutoCandidate {
	return &snFleetAutoCandidate{
		ClientId:  server.Id{0xc0, n},
		ClientKey: [32]byte{0xa0, n},
		Prefix:    [32]byte{0xd0, prefix},
		LastCheck: testingFleetAutoNow,
	}
}

func testingFleetAutoPlanIds(plan []*snFleetAutoPlanMember) map[server.Id]*snFleetAutoPlanMember {
	out := map[server.Id]*snFleetAutoPlanMember{}
	for _, member := range plan {
		out[member.Candidate.ClientId] = member
	}
	return out
}

// A second device on a prefix adds nothing and would lose its pool share.
func TestSnFleetAutoPlanOnePerPrefix(t *testing.T) {
	plan := snFleetAutoPlan([]*snFleetAutoCandidate{
		testingFleetAutoCandidate(1, 1),
		testingFleetAutoCandidate(2, 1),
		testingFleetAutoCandidate(3, 2),
	}, 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey)
	if len(plan) != 2 {
		t.Fatalf("chose %d", len(plan))
	}
	prefixes := map[[32]byte]bool{}
	for _, member := range plan {
		if prefixes[member.Candidate.Prefix] {
			t.Fatal("two devices on one prefix")
		}
		prefixes[member.Candidate.Prefix] = true
		if !member.Bind || member.ValidFrom != 11 || member.ValidTo != 22 {
			t.Fatalf("window %+v", member)
		}
	}
}

// No more than the per-fleet share, however many prefixes the network shows.
func TestSnFleetAutoPlanStopsAtTheLimit(t *testing.T) {
	candidates := []*snFleetAutoCandidate{}
	for i := byte(1); i <= 12; i += 1 {
		candidates = append(candidates, testingFleetAutoCandidate(i, i))
	}
	if plan := snFleetAutoPlan(candidates, 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey); len(plan) != 7 {
		t.Fatalf("chose %d", len(plan))
	}
	if plan := snFleetAutoPlan(candidates, 3, 10, testingFleetAutoFleetId, testingFleetAutoHotkey); len(plan) != 3 {
		t.Fatalf("chose %d", len(plan))
	}
}

// Slots on one host share a key, and a manifest can't name a key twice.
func TestSnFleetAutoPlanOnePerClientKey(t *testing.T) {
	a := testingFleetAutoCandidate(1, 1)
	b := testingFleetAutoCandidate(2, 2)
	b.ClientKey = a.ClientKey
	c := testingFleetAutoCandidate(3, 3)
	plan := snFleetAutoPlan([]*snFleetAutoCandidate{a, b, c}, 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey)
	ids := testingFleetAutoPlanIds(plan)
	if len(plan) != 2 || ids[c.ClientId] == nil || (ids[a.ClientId] == nil) == (ids[b.ClientId] == nil) {
		t.Fatalf("chose %v", ids)
	}
}

// For one prefix: a bound device first, then one the validator measures, then the
// freshest. The answer doesn't depend on the order they come in.
func TestSnFleetAutoPlanPrefersBoundThenMeasuredThenFresh(t *testing.T) {
	fresh := testingFleetAutoCandidate(1, 1)
	fresh.LastCheck = testingFleetAutoNow.Add(time.Hour)
	measured := testingFleetAutoCandidate(2, 1)
	measured.Measured = true
	bound := testingFleetAutoCandidate(3, 1)
	bound.Binding = &model.SnFleetBindingVersion{ClientId: bound.ClientId, FleetId: testingFleetAutoFleetId, Hotkey: testingFleetAutoHotkey, Generation: 2, ValidFromEpoch: 5, ValidToEpoch: 16}
	for _, order := range [][]*snFleetAutoCandidate{{fresh, measured, bound}, {bound, measured, fresh}, {measured, bound, fresh}} {
		plan := snFleetAutoPlan(order, 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey)
		if len(plan) != 1 || plan[0].Candidate != bound || plan[0].Bind {
			t.Fatalf("chose %+v", plan[0])
		}
		plan = snFleetAutoPlan(order[:0:0], 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey)
		if len(plan) != 0 {
			t.Fatal("chose from nothing")
		}
	}
	for _, order := range [][]*snFleetAutoCandidate{{fresh, measured}, {measured, fresh}} {
		if plan := snFleetAutoPlan(order, 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey); plan[0].Candidate != measured {
			t.Fatalf("chose %+v", plan[0].Candidate)
		}
	}
	older := testingFleetAutoCandidate(4, 1)
	for _, order := range [][]*snFleetAutoCandidate{{fresh, older}, {older, fresh}} {
		if plan := snFleetAutoPlan(order, 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey); plan[0].Candidate != fresh {
			t.Fatalf("chose %+v", plan[0].Candidate)
		}
	}
}

// The coordinator refuses a binding that overlaps the client's last. A device in
// someone's own fleet is left alone until that ends, and its operators' devices never
// sign anything here.
func TestSnFleetAutoPlanLeavesAnotherFleetsDevice(t *testing.T) {
	other := testingFleetAutoCandidate(1, 1)
	other.Binding = &model.SnFleetBindingVersion{ClientId: other.ClientId, FleetId: [32]byte{0xee}, Hotkey: [32]byte{0xef}, Generation: 2, ValidFromEpoch: 5, ValidToEpoch: 16}
	if plan := snFleetAutoPlan([]*snFleetAutoCandidate{other}, 7, 10, testingFleetAutoFleetId, testingFleetAutoHotkey); len(plan) != 0 {
		t.Fatalf("chose a device of another fleet: %+v", plan[0])
	}
	// in its last epoch: a binding can start right after
	if plan := snFleetAutoPlan([]*snFleetAutoCandidate{other}, 7, 16, testingFleetAutoFleetId, testingFleetAutoHotkey); len(plan) != 1 || plan[0].ValidFrom != 17 {
		t.Fatalf("plan %+v", plan)
	}
}

// Renewing starts where the old binding ends, with no gap, and only when it's near.
func TestSnFleetAutoPlanRenews(t *testing.T) {
	bound := testingFleetAutoCandidate(1, 1)
	bound.Binding = &model.SnFleetBindingVersion{ClientId: bound.ClientId, FleetId: testingFleetAutoFleetId, Hotkey: testingFleetAutoHotkey, Generation: 2, ValidFromEpoch: 5, ValidToEpoch: 16}
	for epoch, want := range map[uint64]bool{10: false, 14: false, 15: true, 16: true} {
		plan := snFleetAutoPlan([]*snFleetAutoCandidate{bound}, 7, epoch, testingFleetAutoFleetId, testingFleetAutoHotkey)
		if len(plan) != 1 || plan[0].Bind != want {
			t.Fatalf("epoch %d: %+v", epoch, plan)
		}
		if want && (plan[0].ValidFrom != 17 || plan[0].ValidTo != 28) {
			t.Fatalf("epoch %d: window %d to %d", epoch, plan[0].ValidFrom, plan[0].ValidTo)
		}
	}
}

// The coordinator requires each client's generation to go up, so a new one is past the
// fleet's and past anything a member ever bound, in any fleet.
func TestSnFleetAutoNextGeneration(t *testing.T) {
	if got := snFleetAutoNextGeneration(nil, nil); got != 1 {
		t.Fatalf("%d", got)
	}
	if got := snFleetAutoNextGeneration(&model.SnFleetAutoGeneration{Generation: 4}, nil); got != 5 {
		t.Fatalf("%d", got)
	}
	versions := []*model.SnFleetBindingVersion{{Generation: 9}, {Generation: 3}}
	if got := snFleetAutoNextGeneration(&model.SnFleetAutoGeneration{Generation: 4}, versions); got != 10 {
		t.Fatalf("%d", got)
	}
}

// The cap is the live burn plus its fee, with the account's deposit it can't spend, and
// only what the key is short of. Never into a burn spike, and never when it isn't needed.
func TestSnFleetTopUpAmount(t *testing.T) {
	for name, c := range map[string]struct {
		burn, fee, free, want uint64
		owed                  bool
	}{
		"an empty key":         {500_000, 1_065_869, 0, 1_566_369, true},
		"short of it":          {500_000, 1_065_869, 1_000_000, 566_369, true},
		"short by very little": {500_000, 1_065_869, 1_566_368, 100_000, true},
		"covered":              {500_000, 1_065_869, 1_566_369, 0, false},
		"a spike":              {5_000_001, 1_065_869, 0, 0, false},
		"at the ceiling":       {5_000_000, 0, 0, 5_000_500, true},
	} {
		amount, owed := snFleetTopUpAmount(c.burn, c.fee, c.free)
		if amount != c.want || owed != c.owed {
			t.Errorf("%s: %d %t", name, amount, owed)
		}
	}
}

func TestSnFleetTopUpCalldata(t *testing.T) {
	calldata, err := snFleetTopUpCalldata([32]byte{0xab})
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(calldata[:4]) != "cd6f4eb1" || len(calldata) != 36 || calldata[4] != 0xab {
		t.Fatalf("%x", calldata)
	}
	if _, err := snFleetTopUpCalldata([32]byte{}); err == nil {
		t.Fatal("a top-up to nobody")
	}
}

// A reply read from mainnet for register_limit: ref_time, proof_size, class, fee.
func TestSnFleetParseFeeQuote(t *testing.T) {
	reply, err := hex.DecodeString("07181118200d96d23800008d43100000000000")
	if err != nil {
		t.Fatal(err)
	}
	fee, err := snFleetParseFeeQuote(reply)
	if err != nil || fee != 1_065_869 {
		t.Fatalf("%d %v", fee, err)
	}
	over := append([]byte{1, 1, 0}, 0, 0, 0, 0, 0, 0, 0, 1)
	if _, err := snFleetParseFeeQuote(over); err == nil {
		t.Fatal("believed a fee over the limit")
	}
	if _, err := snFleetParseFeeQuote([]byte{1}); err == nil {
		t.Fatal("read a short reply")
	}
}

// A key counts as funded only if it holds enough and gas isn't over the ceiling, and a
// read that failed is not funded.
func TestSnFleetRelayKeyUsable(t *testing.T) {
	balance := testingBigInt(snFleetRelayMinBalanceWei)
	price := testingBigInt(snFleetRelayMaxGasPriceWei)
	if !snFleetRelayKeyUsable(balance, price) {
		t.Fatal("at the limits")
	}
	if snFleetRelayKeyUsable(testingBigInt(snFleetRelayMinBalanceWei-1), price) {
		t.Fatal("an unfunded key")
	}
	if snFleetRelayKeyUsable(balance, testingBigInt(snFleetRelayMaxGasPriceWei+1)) {
		t.Fatal("gas over the ceiling")
	}
	if snFleetRelayKeyUsable(nil, price) || snFleetRelayKeyUsable(balance, nil) {
		t.Fatal("a failed read")
	}
}

// A registration near the epoch's start has its UID still immune when its binding starts,
// and one that's too late for this epoch waits for the next window.
func TestSnFleetAutoRegisterWindow(t *testing.T) {
	const epochBlocks = 50_400
	state := &StEpochState{Epoch: 4, EpochStartBlock: 1_000_000, TEpochBlocks: epochBlocks}
	nextStart := uint64(1_050_400)

	state.HeadBlock = 1_000_100
	after, before, start := snFleetAutoRegisterWindow(state)
	if start != nextStart || after != nextStart-snFleetAutoImmunityBlocks || before != nextStart-snFleetAutoRegisterRunwayBlocks {
		t.Fatalf("%d %d %d", after, before, start)
	}
	state.HeadBlock = before
	if after2, before2, _ := snFleetAutoRegisterWindow(state); after2 != after || before2 != before {
		t.Fatal("the last block of the window moved it")
	}
	state.HeadBlock = before + 1
	after2, before2, _ := snFleetAutoRegisterWindow(state)
	if after2 != after+epochBlocks || before2 != before+epochBlocks {
		t.Fatalf("too late for this epoch: %d %d", after2, before2)
	}
}

func testingBigInt(value int64) *big.Int {
	return big.NewInt(value)
}
