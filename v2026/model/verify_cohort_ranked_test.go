package model

// Tier B's representatives (sn VERIFY-COHORT-V2.md §2.6): one per unclaimed
// network ranked in the top 400 in 3 of the last 4 folds, one per prefix with
// the higher-ranked network winning, never a provider on probation, a counted
// provider first. Pure: no database or Redis.

import (
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func testVerifyCohortRankedId(b byte) server.Id {
	return server.Id{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, b}
}

func testVerifyCohortRankedPrefix(b byte) [32]byte {
	return [32]byte{b}
}

func testVerifyCohortRankedTop(networks ...server.Id) map[server.Id]bool {
	top := map[server.Id]bool{}
	for _, networkId := range networks {
		top[networkId] = true
	}
	return top
}

func TestSelectVerifyCohortRankedOnePerStableUnclaimedNetwork(t *testing.T) {
	a, b, c, d, e, f := testVerifyCohortRankedId(0xa1), testVerifyCohortRankedId(0xa2), testVerifyCohortRankedId(0xa3), testVerifyCohortRankedId(0xa4), testVerifyCohortRankedId(0xa5), testVerifyCohortRankedId(0xa6)
	networks := []VerifyCohortRankedNetwork{
		{NetworkId: b, Position: 2},
		{NetworkId: a, Position: 1},
		// claimed: its members draw through the priority set instead
		{NetworkId: c, Position: 3, Claimed: true},
		// past the paged top 400
		{NetworkId: d, Position: VerifyCohortRankedPositions + 1},
		// new: in the top 400 only in this fold and one other
		{NetworkId: e, Position: 4},
		// unranked
		{NetworkId: f, Position: 0},
	}
	recentTop := []map[server.Id]bool{
		testVerifyCohortRankedTop(a, b, c, e),
		testVerifyCohortRankedTop(a, b, c, e),
		testVerifyCohortRankedTop(a, c),
		testVerifyCohortRankedTop(a, b, c),
	}
	providers := []VerifyCohortRankedProvider{
		{ClientId: testVerifyCohortRankedId(0x11), NetworkId: a, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(1), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x12), NetworkId: a, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(2), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x21), NetworkId: b, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(3), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x31), NetworkId: c, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(4), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x41), NetworkId: d, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(5), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x51), NetworkId: e, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(6), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x61), NetworkId: f, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(7), HasPrefix: true},
	}
	got := SelectVerifyCohortRanked(networks, recentTop, providers, nil, nil)
	want := []server.Id{testVerifyCohortRankedId(0x11), testVerifyCohortRankedId(0x21)}
	if !slices.Equal(got, want) {
		t.Fatalf("representatives %v, want one each of the two stable unclaimed networks in rank order %v", got, want)
	}

	// a network out of this fold's top 400 has none, however stable it was
	recentTop[0] = testVerifyCohortRankedTop(b, c, e)
	if got := SelectVerifyCohortRanked(networks, recentTop, providers, nil, nil); !slices.Equal(got, []server.Id{testVerifyCohortRankedId(0x21)}) {
		t.Fatalf("a network out of this fold's top 400 kept a representative: %v", got)
	}
	if got := SelectVerifyCohortRanked(networks, nil, providers, nil, nil); len(got) != 0 {
		t.Fatalf("no folds chose %v", got)
	}
}

// A higher-ranked network's representative holds its prefix: a lower-ranked
// network sharing it takes a provider on another prefix, or has none.
func TestSelectVerifyCohortRankedOnePerPrefix(t *testing.T) {
	a, b, c := testVerifyCohortRankedId(0xa1), testVerifyCohortRankedId(0xa2), testVerifyCohortRankedId(0xa3)
	networks := []VerifyCohortRankedNetwork{{NetworkId: a, Position: 1}, {NetworkId: b, Position: 2}, {NetworkId: c, Position: 3}}
	top := testVerifyCohortRankedTop(a, b, c)
	recentTop := []map[server.Id]bool{top, top, top, top}
	shared := testVerifyCohortRankedPrefix(9)
	providers := []VerifyCohortRankedProvider{
		{ClientId: testVerifyCohortRankedId(0x11), NetworkId: a, Eligible: true, PrefixHash: shared, HasPrefix: true},
		// b's lowest id shares a's prefix; its other provider is elsewhere
		{ClientId: testVerifyCohortRankedId(0x21), NetworkId: b, Eligible: true, PrefixHash: shared, HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x22), NetworkId: b, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(8), HasPrefix: true},
		// c has only the shared prefix
		{ClientId: testVerifyCohortRankedId(0x31), NetworkId: c, Eligible: true, PrefixHash: shared, HasPrefix: true},
	}
	got := SelectVerifyCohortRanked(networks, recentTop, providers, nil, nil)
	want := []server.Id{testVerifyCohortRankedId(0x11), testVerifyCohortRankedId(0x22)}
	if !slices.Equal(got, want) {
		t.Fatalf("representatives %v, want %v", got, want)
	}
}

// A counted provider costs no room, so it goes first; then one with a prefix
// pin; then the lowest client id. Never an ineligible provider or one on
// probation.
func TestSelectVerifyCohortRankedPrefersCountedEligibleUnbenched(t *testing.T) {
	a, b := testVerifyCohortRankedId(0xa1), testVerifyCohortRankedId(0xa2)
	networks := []VerifyCohortRankedNetwork{{NetworkId: a, Position: 1}, {NetworkId: b, Position: 2}}
	top := testVerifyCohortRankedTop(a, b)
	recentTop := []map[server.Id]bool{top, top, top}
	providers := []VerifyCohortRankedProvider{
		{ClientId: testVerifyCohortRankedId(0x10), NetworkId: a, Eligible: false, PrefixHash: testVerifyCohortRankedPrefix(1), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x11), NetworkId: a, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(2), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x12), NetworkId: a, Eligible: true},
		{ClientId: testVerifyCohortRankedId(0x13), NetworkId: a, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(3), HasPrefix: true},
		{ClientId: testVerifyCohortRankedId(0x21), NetworkId: b, Eligible: true},
		{ClientId: testVerifyCohortRankedId(0x22), NetworkId: b, Eligible: true, PrefixHash: testVerifyCohortRankedPrefix(4), HasPrefix: true},
	}
	// lowest id among pinned eligible providers
	if got := SelectVerifyCohortRanked(networks, recentTop, providers, nil, nil); !slices.Equal(got, []server.Id{testVerifyCohortRankedId(0x11), testVerifyCohortRankedId(0x22)}) {
		t.Fatalf("by pin and id: %v", got)
	}
	// a counted provider first, even unpinned
	counted := map[server.Id]bool{testVerifyCohortRankedId(0x12): true}
	if got := SelectVerifyCohortRanked(networks, recentTop, providers, counted, nil); !slices.Equal(got, []server.Id{testVerifyCohortRankedId(0x12), testVerifyCohortRankedId(0x22)}) {
		t.Fatalf("counted first: %v", got)
	}
	// probation skips a provider; a network with none left has no representative
	probation := map[server.Id]bool{testVerifyCohortRankedId(0x11): true, testVerifyCohortRankedId(0x21): true, testVerifyCohortRankedId(0x22): true}
	if got := SelectVerifyCohortRanked(networks, recentTop, providers, nil, probation); !slices.Equal(got, []server.Id{testVerifyCohortRankedId(0x13)}) {
		t.Fatalf("probation: %v", got)
	}
}
