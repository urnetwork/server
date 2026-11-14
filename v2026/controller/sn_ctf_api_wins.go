package controller

// sn_ctf_api_wins.go — the wins GET /sn/ctf/client answers (sn/api/operator.yml
// CtfClientWin), and whether each is claimed. Pure: the awards and the claim
// states come from the source (sn_ctf_api_source.go, sn_ctf_api_claims.go), so
// the rule is tested without a database or a chain.
//
// The rule. vault2's claims are cumulative: a claim pays an account up to its
// cumulative in a finalized root and sets the account's claimed to it, so an
// account's claimed is always the cumulative of the root of its latest claim.
//   - A win's paying epoch is its own epoch, or, when its pot share was a
//     pending award, the first epoch after it that matured, which pays every
//     pending award (CAPTURETHEFLAG.md §4.5).
//   - Its paying root is the committed CTF root that covers the paying epoch.
//   - It is claimed when its paying root is finalized and vault2's claimed for
//     the CTF account of its coldkey, read at the finalized head, is at least
//     that account's cumulative in the paying root: the account's latest claim
//     used the paying root or a later one.
//   - Otherwise it is unclaimed: while its paying root is pending, while its
//     pending pot share waits for a matured epoch, and while the account's
//     claimed is below that cumulative.
//   - The paying root pays the whole win: its slot by then, and its pot share
//     in it. So the account's cumulative in the paying root is at least the
//     win's amount. A state that says less is not trusted, and the win is
//     null, never claimed: an account the records do not find, as a root
//     recorded between the read of the account record and the count of
//     recorded accounts once made it, reads nothing paid and nothing claimed,
//     which would otherwise say claimed. The one other way to say less is a
//     shortfall that took part of a pending pot share before the first matured
//     epoch paid it, which needs every pot empty; that win is null too,
//     unknown rather than wrong.

import (
	"cmp"
	"slices"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server/v2026"
)

// snCtfClientScope is whom GET /sn/ctf/client answers for: the client a client
// credential names, or, for a network credential, every client its network
// has had.
type snCtfClientScope struct {
	// nil for a network credential
	ClientId  *server.Id
	NetworkId server.Id
}

// snCtfWin is one award of the scope.
type snCtfWin struct {
	Epoch uint64
	Award *snCtfAward
}

// snCtfAccountClaims is one coldkey's account in the CTF pool as a claim sees
// it: vault2's claimed, read at the finalized head, and the account's
// cumulative in each finalized root the wins need, by root seq (0 in a root
// before the account's first).
type snCtfAccountClaims struct {
	Claimed    uint256.Int
	Cumulative map[uint64]uint256.Int
}

// snCtfClaimNeed is a coldkey whose claim state an answer reads, and the
// finalized paying roots of its wins.
type snCtfClaimNeed struct {
	Coldkey [32]byte
	Roots   []*snCtfRoot
}

// snCtfSortWins orders wins newest first: by epoch descending, then providers
// before extenders, then country, then rank.
func snCtfSortWins(wins []*snCtfWin) {
	slices.SortStableFunc(wins, func(a, b *snCtfWin) int {
		return cmp.Or(cmp.Compare(b.Epoch, a.Epoch), cmp.Compare(a.Award.Role, b.Award.Role), cmp.Compare(a.Award.Country, b.Award.Country), cmp.Compare(a.Award.Rank, b.Award.Rank))
	})
}

// payingRoot is a win's paying root: the committed root that covers its epoch,
// or, for a pot share that was a pending award, the one that covers the first
// matured epoch after it; nil while no covered epoch after it has matured.
func (self *snCtfView) payingRoot(win *snCtfWin) *snCtfRoot {
	for _, epoch := range self.Epochs {
		if epoch.Epoch == win.Epoch && !win.Award.Deferred {
			return epoch.Root
		}
		if win.Epoch < epoch.Epoch && win.Award.Deferred && epoch.Matured {
			return epoch.Root
		}
	}
	return nil
}

// snCtfClaimNeeds lists every coldkey whose claim state the wins need, by its
// newest win, so that the newest are read first, each with its wins' finalized
// paying roots. A win whose paying root is pending, or that has none yet, is
// unclaimed without a read.
func snCtfClaimNeeds(view *snCtfView, wins []*snCtfWin) []*snCtfClaimNeed {
	ordered := slices.Clone(wins)
	snCtfSortWins(ordered)
	needs := []*snCtfClaimNeed{}
	byColdkey := map[[32]byte]*snCtfClaimNeed{}
	for _, win := range ordered {
		root := view.payingRoot(win)
		if root == nil || !root.Finalized {
			continue
		}
		need := byColdkey[win.Award.Coldkey]
		if need == nil {
			need = &snCtfClaimNeed{Coldkey: win.Award.Coldkey}
			byColdkey[win.Award.Coldkey] = need
			needs = append(needs, need)
		}
		// a finalized epoch the view keeps from an earlier read holds its own
		// copy of its root
		if !slices.ContainsFunc(need.Roots, func(listed *snCtfRoot) bool { return listed.Seq == root.Seq }) {
			need.Roots = append(need.Roots, root)
		}
	}
	return needs
}

// snCtfWinClaimed is whether a win is claimed, by its paying root and its
// coldkey's claim state: false while the paying root is pending or there is
// none yet; nil when the claim state could not be read, or gives the account
// less in the paying root than the win's amount, which that root pays whole.
func snCtfWinClaimed(win *snCtfWin, root *snCtfRoot, claims *snCtfAccountClaims) *bool {
	claimed := false
	if root == nil || !root.Finalized {
		return &claimed
	}
	if claims == nil {
		return nil
	}
	cumulative, ok := claims.Cumulative[root.Seq]
	if !ok || cumulative.Lt(win.Award.total()) {
		return nil
	}
	claimed = !claims.Claimed.Lt(&cumulative)
	return &claimed
}

// winsAnswer is the answer's wins, newest first: every win not known to be
// claimed, whatever its epoch, and every win of the recent epochs (the limit
// most recent covered epochs), claimed or not.
func (self *snCtfView) winsAnswer(wins []*snCtfWin, claims map[[32]byte]*snCtfAccountClaims, limit int) []*SnCtfClientWin {
	recent := map[uint64]bool{}
	for i := len(self.Epochs) - 1; 0 <= i && len(recent) < limit; i -= 1 {
		recent[self.Epochs[i].Epoch] = true
	}
	ordered := slices.Clone(wins)
	snCtfSortWins(ordered)
	answer := []*SnCtfClientWin{}
	for _, win := range ordered {
		epoch := self.epoch(win.Epoch)
		if epoch == nil {
			// only covered epochs: a committed root holds the win
			continue
		}
		claimed := snCtfWinClaimed(win, self.payingRoot(win), claims[win.Award.Coldkey])
		if claimed != nil && *claimed && !recent[win.Epoch] {
			continue
		}
		award := win.Award
		answer = append(answer, &SnCtfClientWin{
			Epoch:       win.Epoch,
			Role:        snCtfRoleName(award.Role),
			Country:     award.Country,
			Rank:        award.Rank,
			ClientId:    award.ClientId.String(),
			Amount:      snCtfAmountOf(award.total()),
			Slot:        snCtfAmountOf(&award.Slot),
			PotShare:    snCtfAmountOf(&award.Pot),
			Pending:     award.Deferred,
			RootStatus:  epoch.Root.status(),
			Coldkey:     snCtfHex(award.Coldkey[:]),
			ColdkeySs58: snCtfColdkeySs58(award.Coldkey),
			Claimed:     claimed,
		})
	}
	return answer
}
