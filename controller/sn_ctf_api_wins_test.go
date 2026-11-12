package controller

// sn_ctf_api_wins_test.go — GET /sn/ctf/client's wins and whether each is
// claimed (sn_ctf_api_wins.go), at the rule's boundaries: an account claimed
// through an earlier root, exactly through the paying root, a rao short of it;
// a root that covers two epochs; a pot share paid by the first matured epoch
// after its own, or still owed; a pending root; a claim state that could not
// be read. Nothing here reads a database or a chain.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
)

// snCtfWinsTestView: root 1 (epoch 8) and root 2 (epochs 9 and 10) are
// finalized, root 3 (epochs 11 and 12) is pending. Epochs 9, 11 and 12 did not
// mature, so a pot share won in 9 is paid by 10 and one won in 11 is still owed.
func snCtfWinsTestView() *snCtfView {
	index0, index1 := uint64(0), uint64(1)
	root1 := &snCtfRoot{Seq: 1, Finalized: true, RootIndex: &index0, Epoch: 8}
	root2 := &snCtfRoot{Seq: 2, Finalized: true, RootIndex: &index1, Epoch: 10}
	root3 := &snCtfRoot{Seq: 3, Epoch: 12}
	epochs := []*snCtfEpoch{
		{Epoch: 8, Root: root1, Matured: true},
		{Epoch: 9, Root: root2},
		{Epoch: 10, Root: root2, Matured: true},
		{Epoch: 11, Root: root3},
		{Epoch: 12, Root: root3},
	}
	return &snCtfView{Pool: snCtfPoolFacts{FirstEpoch: 8}, Roots: []*snCtfRoot{root1, root2, root3}, Epochs: epochs}
}

func snCtfWinsTestWin(epoch uint64, role int, country string, rank int, clientId server.Id, coldkey [32]byte, slot uint64, pot uint64, deferred bool) *snCtfWin {
	return &snCtfWin{Epoch: epoch, Award: &snCtfAward{Role: role, Country: country, Rank: rank, ClientId: clientId, Coldkey: coldkey, Slot: snCtfTestRao(slot), Pot: snCtfTestRao(pot), Deferred: deferred}}
}

// The claimed flags of an answer's wins, by "epoch role country rank".
func snCtfWinsTestClaimed(wins []*SnCtfClientWin) map[string]string {
	flags := map[string]string{}
	for _, win := range wins {
		flag := "null"
		if win.Claimed != nil {
			flag = fmt.Sprint(*win.Claimed)
		}
		flags[fmt.Sprintf("%d %s %s %d", win.Epoch, win.Role, win.Country, win.Rank)] = flag
	}
	return flags
}

// One account's wins against its claimed total: a claim reaches every win
// whose paying root it reached, whole roots at a time, and no other.
func TestSnCtfWinClaimedRule(t *testing.T) {
	view := snCtfWinsTestView()
	client := server.NewId()
	coldkey := [32]byte{0xa1}
	wins := []*snCtfWin{
		// paid by root 1
		snCtfWinsTestWin(8, snCtfRoleProvider, "is", 1, client, coldkey, 100, 0, false),
		// a pot share pending in 9, paid by matured 10: root 2, as 10's own
		snCtfWinsTestWin(9, snCtfRoleExtender, "is", 1, client, coldkey, 100, 50, true),
		snCtfWinsTestWin(10, snCtfRoleProvider, "de", 2, client, coldkey, 100, 0, false),
		// a pot share pending in 11 that no matured epoch has paid yet
		snCtfWinsTestWin(11, snCtfRoleProvider, "is", 1, client, coldkey, 100, 30, true),
		// root 3 is pending
		snCtfWinsTestWin(12, snCtfRoleProvider, "is", 1, client, coldkey, 100, 0, false),
	}
	if root := view.payingRoot(wins[1]); root != view.Roots[1] {
		t.Fatalf("a pot share pending in 9 is paid by root %+v, want root 2", root)
	}
	if root := view.payingRoot(wins[3]); root != nil {
		t.Fatalf("a pot share still owed has paying root %+v", root)
	}
	needs := snCtfClaimNeeds(view, wins)
	if len(needs) != 1 || needs[0].Coldkey != coldkey || len(needs[0].Roots) != 2 || needs[0].Roots[0] != view.Roots[1] || needs[0].Roots[1] != view.Roots[0] {
		t.Fatalf("needs %+v: roots 2 and 1, newest first, and no pending root", needs)
	}
	// C(root 1) = 100, C(root 2) = 100 + 150 + 100 = 350
	cumulative := map[uint64]uint256.Int{1: snCtfTestRao(100), 2: snCtfTestRao(350)}
	for _, c := range []struct {
		name    string
		claimed uint64
		want    map[string]string
	}{
		{"nothing claimed", 0, map[string]string{"8 provider is 1": "false", "9 extender is 1": "false", "10 provider de 2": "false", "11 provider is 1": "false", "12 provider is 1": "false"}},
		{"claimed through root 1, exactly", 100, map[string]string{"8 provider is 1": "true", "9 extender is 1": "false", "10 provider de 2": "false", "11 provider is 1": "false", "12 provider is 1": "false"}},
		{"a rao short of root 2", 349, map[string]string{"8 provider is 1": "true", "9 extender is 1": "false", "10 provider de 2": "false", "11 provider is 1": "false", "12 provider is 1": "false"}},
		{"claimed through root 2, exactly", 350, map[string]string{"8 provider is 1": "true", "9 extender is 1": "true", "10 provider de 2": "true", "11 provider is 1": "false", "12 provider is 1": "false"}},
	} {
		claims := map[[32]byte]*snCtfAccountClaims{coldkey: {Claimed: snCtfTestRao(c.claimed), Cumulative: cumulative}}
		// limit 5: every epoch is recent, so every win is listed
		got := snCtfWinsTestClaimed(view.winsAnswer(wins, claims, 5))
		requireSnCtfEqual(t, c.name, got, c.want)
	}

	// a claim state that could not be read: null for the wins it decides,
	// never for a win whose paying root is pending or not there yet
	unknown := map[string]string{"8 provider is 1": "null", "9 extender is 1": "null", "10 provider de 2": "null", "11 provider is 1": "false", "12 provider is 1": "false"}
	requireSnCtfEqual(t, "unread", snCtfWinsTestClaimed(view.winsAnswer(wins, nil, 5)), unknown)
	partial := map[[32]byte]*snCtfAccountClaims{coldkey: {Claimed: snCtfTestRao(350), Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(100)}}}
	requireSnCtfEqual(t, "a root's cumulative not read", snCtfWinsTestClaimed(view.winsAnswer(wins, partial, 5)),
		map[string]string{"8 provider is 1": "true", "9 extender is 1": "null", "10 provider de 2": "null", "11 provider is 1": "false", "12 provider is 1": "false"})

	// The state the records give a coldkey whose account they do not find,
	// as a root recorded between the read of the account record and the count
	// of recorded accounts once made them: nothing claimed and nothing paid in
	// any root. Every finalized win of it is null, never claimed, since its
	// paying root pays it whole.
	notFound := map[[32]byte]*snCtfAccountClaims{coldkey: {Cumulative: map[uint64]uint256.Int{1: {}, 2: {}}}}
	requireSnCtfEqual(t, "an account not found", snCtfWinsTestClaimed(view.winsAnswer(wins, notFound, 5)), unknown)
	// the rule is per win: a paying root's cumulative below one win's amount
	// leaves that win null and decides the others it covers
	short := map[[32]byte]*snCtfAccountClaims{coldkey: {Claimed: snCtfTestRao(100), Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(100), 2: snCtfTestRao(149)}}}
	requireSnCtfEqual(t, "a paying root below a win's amount", snCtfWinsTestClaimed(view.winsAnswer(wins, short, 5)),
		map[string]string{"8 provider is 1": "true", "9 extender is 1": "null", "10 provider de 2": "false", "11 provider is 1": "false", "12 provider is 1": "false"})
	// a win of no amount pays nothing, so no account is needed to hold it,
	// and it is claimed
	nothing := snCtfWinsTestWin(8, snCtfRoleExtender, "de", 5, client, [32]byte{0xa2}, 0, 0, false)
	if claimed := snCtfWinClaimed(nothing, view.payingRoot(nothing), &snCtfAccountClaims{Cumulative: map[uint64]uint256.Int{1: {}}}); claimed == nil || !*claimed {
		t.Fatalf("a win of no amount, of a coldkey no root holds: claimed %v", claimed)
	}

	// limit 2 (epochs 11 and 12): the claimed wins of older epochs are left
	// out, the others listed whatever their epoch, newest first
	claims := map[[32]byte]*snCtfAccountClaims{coldkey: {Claimed: snCtfTestRao(100), Cumulative: cumulative}}
	listed := view.winsAnswer(wins, claims, 2)
	order := []string{}
	for _, win := range listed {
		order = append(order, fmt.Sprintf("%d %s %s %d", win.Epoch, win.Role, win.Country, win.Rank))
	}
	requireSnCtfEqual(t, "listed", order, []string{"12 provider is 1", "11 provider is 1", "10 provider de 2", "9 extender is 1"})
	if all := view.winsAnswer(wins, map[[32]byte]*snCtfAccountClaims{coldkey: {Claimed: snCtfTestRao(350), Cumulative: cumulative}}, 2); len(all) != 2 {
		t.Fatalf("claimed through root 2 with limit 2: %d wins listed, want 11 and 12", len(all))
	}
	// the answer's amounts
	win := listed[3]
	if win.Amount.Rao != 150 || win.Slot.Rao != 100 || win.PotShare.Rao != 50 || !win.Pending || win.RootStatus != "finalized" || win.ClientId != client.String() ||
		win.Coldkey != snCtfHex(coldkey[:]) || win.ColdkeySs58 != snCtfColdkeySs58(coldkey) {
		t.Fatalf("win %+v", win)
	}
	if listed[0].RootStatus != "pending" {
		t.Fatalf("epoch 12's root is %s", listed[0].RootStatus)
	}
}

// A network's answer: no client, no epochs, the wins of its clients newest
// first, providers before extenders, then country and rank; each coldkey's
// claim state read once, the newest wins' coldkeys first, however many there
// are.
func TestSnCtfClientResultOfANetwork(t *testing.T) {
	view := snCtfWinsTestView()
	first, second := server.NewId(), server.NewId()
	wins := []*snCtfWin{
		snCtfWinsTestWin(8, snCtfRoleExtender, "de", 1, second, [32]byte{0xb2}, 100, 0, false),
		snCtfWinsTestWin(10, snCtfRoleExtender, "de", 1, second, [32]byte{0xb2}, 100, 0, false),
		snCtfWinsTestWin(10, snCtfRoleProvider, "is", 3, first, [32]byte{0xa1}, 100, 0, false),
		snCtfWinsTestWin(10, snCtfRoleProvider, "de", 4, first, [32]byte{0xa1}, 100, 0, false),
		snCtfWinsTestWin(10, snCtfRoleProvider, "de", 2, second, [32]byte{0xb2}, 100, 0, false),
	}
	claims := map[[32]byte]*snCtfAccountClaims{
		{0xa1}: {Claimed: snCtfTestRao(200), Cumulative: map[uint64]uint256.Int{2: snCtfTestRao(200)}},
		{0xb2}: {Claimed: snCtfTestRao(100), Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(100), 2: snCtfTestRao(300)}},
	}
	facts := map[uint64]*snCtfClientEpochFacts{10: {}}
	result := snCtfClientResultOf(view, snCtfClientScope{NetworkId: server.NewId()}, facts, wins, claims, 8)
	if result.ClientId != nil || len(result.Epochs) != 0 || result.Epochs == nil {
		t.Fatalf("a network's answer names client %v with %d epochs", result.ClientId, len(result.Epochs))
	}
	order := []string{}
	for _, win := range result.Wins {
		order = append(order, fmt.Sprintf("%d %s %s %d %s %v", win.Epoch, win.Role, win.Country, win.Rank, win.ClientId, *win.Claimed))
	}
	requireSnCtfEqual(t, "network wins", order, []string{
		fmt.Sprintf("10 provider de 2 %s false", second),
		fmt.Sprintf("10 provider de 4 %s true", first),
		fmt.Sprintf("10 provider is 3 %s true", first),
		fmt.Sprintf("10 extender de 1 %s false", second),
		fmt.Sprintf("8 extender de 1 %s true", second),
	})
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil || answer["client_id"] != nil || len(answer["epochs"].([]any)) != 0 {
		t.Fatalf("answer %s", raw)
	}

	// 300 coldkeys, one win each in epoch 10 but the last's, in epoch 8: every
	// coldkey's state is asked for, the newest wins' first, and every win is
	// decided, the oldest included
	const coldkeys = 300
	many := []*snCtfWin{}
	for i := range coldkeys {
		epoch := uint64(10)
		if i == coldkeys-1 {
			epoch = 8
		}
		many = append(many, snCtfWinsTestWin(epoch, snCtfRoleProvider, fmt.Sprintf("%c%c", 'a'+i/26, 'a'+i%26), 1, first, [32]byte{0xc0, byte(i >> 8), byte(i)}, 100, 0, false))
	}
	needs := snCtfClaimNeeds(view, many)
	if len(needs) != coldkeys {
		t.Fatalf("%d of %d coldkeys read", len(needs), coldkeys)
	}
	if oldest := needs[len(needs)-1]; oldest.Coldkey != many[coldkeys-1].Award.Coldkey || len(oldest.Roots) != 1 || oldest.Roots[0] != view.Roots[0] {
		t.Fatalf("the coldkey of the oldest win is not read last, at root 1: %+v", oldest)
	}
	readAll := map[[32]byte]*snCtfAccountClaims{}
	for _, win := range many {
		readAll[win.Award.Coldkey] = &snCtfAccountClaims{Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(100), 2: snCtfTestRao(100)}}
	}
	source := &snCtfTestSource{claims: readAll}
	answered := view.winsAnswer(many, source.AccountClaims(context.Background(), view, needs), 8)
	if len(answered) != coldkeys || answered[coldkeys-1].Epoch != 8 {
		t.Fatalf("%d wins answered", len(answered))
	}
	for _, win := range answered {
		if win.Claimed == nil || *win.Claimed {
			t.Fatalf("win %+v", win)
		}
	}
}
