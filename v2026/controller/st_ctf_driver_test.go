package controller

// st_ctf_driver_test.go — the capture the flag driver (CAPTURETHEFLAG.md
// §4.3 to §4.7, §11.2) against the in-memory chain, carry and records of
// st_ctf_fake_test.go, with no database or network. Every committed root is
// audited with ctf.Verify from the fake chain's facts, as the validator
// audits it.

import (
	"encoding/hex"
	"errors"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/v2026/amendment"
	"github.com/urfoundation/sn/v2026/ctf"
	"github.com/urfoundation/sn/v2026/ledger"

	"github.com/urnetwork/server/v2026/model"
)

func requireCtfLog(t testing.TB, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q\nwant  %q", got, want)
	}
}

func requireCtfAudit(t testing.TB, result *ctf.Result) {
	t.Helper()
	if result.Action() != ledger.ActionApprove || !result.Reproduced || result.Err != nil {
		t.Fatalf("the audit does not approve: check %v: %v (reproduced %t, missing %v)", result.Check, result.Err, result.Reproduced, result.Missing)
	}
}

func requireCtfAlerts(t testing.TB, alerts *[]string, want ...string) {
	t.Helper()
	got := slices.Clone(*alerts)
	slices.Sort(got)
	slices.Sort(want)
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alerts %q\nwant   %q", got, want)
	}
}

// emission funds epoch k of the CTF pool when it is closed, beside any carry
// returned for it.
func (self *ctfFixture) emission(epoch uint64, rao uint64) {
	self.chain.emission[epoch] += rao
}

// §4.3, §4.4, §3.3: per epoch the driver commits the census inside its
// window, closes the pool, waits for the census and ρ_k, builds, publishes and
// commits, and the validator's audit of the root reproduces it.
func TestStCtfDriverRunsAnEpochInOrder(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)

	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "census 8")
	if census := f.chain.census[8]; census.hash != f.census[8].Hash || census.block != f.chain.end(8)+1 {
		t.Fatalf("committed %x at %d", census.hash, census.block)
	}
	// every census object is published before the commitment
	for ref := range f.census[8].Objects {
		if _, err := GetStLedgerV2Object(f.ctx, ref.String()); err != nil {
			t.Fatalf("census object %v: %v", ref, err)
		}
	}

	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "close 8", "commit 8 prev 0")
	result := f.verify(nil)
	requireCtfAudit(t, result)
	e := result.Epochs[0]
	if !e.Drawn || e.Round != 1000 || e.Candidates != 7 || len(e.Awards) != 6 || !e.Matured {
		t.Fatalf("epoch 8: drawn %t round %d candidates %d awards %d matured %t", e.Drawn, e.Round, e.Candidates, len(e.Awards), e.Matured)
	}
	if got := f.records.rows(8); !reflect.DeepEqual(got, []string{"ctfcensus confirmed", "ctfclose confirmed", "ctfcommit confirmed"}) {
		t.Fatalf("rows %q", got)
	}
	// what the API reads: the commitment, the draw, the fixed result and the
	// ledger, committed
	commitment := f.records.commitments[8]
	if commitment == nil || commitment.Status != model.CtfCommitmentStatusCommitted || commitment.CensusHash != f.census[8].Hash ||
		*commitment.CommitBlock != f.chain.end(8)+1 || *commitment.Round != 1000 || *commitment.CommitBlockTime != ctfTestCommitTime(1000) {
		t.Fatalf("commitment %+v", commitment)
	}
	draw := f.records.draws[8]
	if draw == nil || draw.Round != 1000 || draw.Seed != e.Seed || draw.Randomness != e.Randomness || hex.EncodeToString(draw.Signature) != ctfTestPulses[1000] {
		t.Fatalf("draw %+v", draw)
	}
	fixed := f.records.results[8]
	if fixed == nil || ledger.Ref(fixed.AwardsRef) != e.AwardsRef || fixed.LedgerEpoch != 8 || fixed.LedgerHash != f.chain.pending.LedgerHash ||
		!fixed.Drawn || fixed.Awards != 6 || len(f.records.awards[8]) != 6 || fixed.CensusHash == nil || *fixed.CensusHash != f.census[8].Hash {
		t.Fatalf("epoch result %+v", fixed)
	}
	for _, award := range f.records.awards[8] {
		if award.UnitFamily != ctf.FamilyIPv4 || award.Coldkey != ctfTestColdkey([16]byte(award.ClientId)) {
			t.Fatalf("award %+v", award)
		}
	}
	if built := f.records.ledgers[f.chain.pending.LedgerHash]; built == nil || built.Status != model.CtfLedgerStatusCommitted || built.Shadow || built.FirstEpoch != 8 {
		t.Fatalf("ledger %+v", built)
	}
	requireCtfAlerts(t, f.alerts)
}

// Idempotency and restart safety: a step run again does only what is still
// due, and a build repeated after a failed commit reproduces the fixed awards.
func TestStCtfDriverIsIdempotent(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)

	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusConfirmed, false, "")
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusSkipped, false, "is committed at block")
	requireCtfLog(t, f.takeLog(), "census 8")

	// a commit that fails after the publish: the retry builds the same root
	f.chain.failNext("commit", errors.New("connection reset"))
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusFailed, true, "connection reset")
	requireCtfLog(t, f.takeLog(), "close 8", "commit 8 prev 0")
	fixed := f.records.results[8]
	if fixed == nil || f.records.ledgers[fixed.LedgerHash].Status != model.CtfLedgerStatusBuilt {
		t.Fatalf("not fixed before the commit: %+v", fixed)
	}
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "commit 8 prev 0")
	if f.records.results[8] != fixed || f.chain.pending.LedgerHash != fixed.LedgerHash {
		t.Fatal("the retry changed the fixed result")
	}
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusSkipped, false, "already committed")
	requireCtfLog(t, f.takeLog())
	requireCtfAudit(t, f.verify(nil))

	// a census commitment another attempt made is done, not sent again
	f.census[9] = ctfTestCensusOf(t, 9, ctfTestCandidates()[:3]...)
	f.chain.head = f.chain.end(9)
	f.chain.census[9] = fakeCtfCensus{hash: f.census[9].Hash, block: f.chain.end(9) + 3}
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 9), model.StPublishStatusSkipped, false, "is committed at block")
	requireCtfLog(t, f.takeLog())
	requireCtfAlerts(t, f.alerts)
}

// §4.3: the commitment is sent only while the block after the latest is
// inside [end(k), end(k) + w/2]; a quarter of the way in it is late, and past
// the window it is missed: no draw, and everything carries (§4.7).
func TestStCtfDriverCensusWindow(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.emission(8, 1_000_000_000_000)
	end := f.chain.end(8)

	// not built yet, inside the first quarter: wait, no alert
	f.chain.head = end + 10
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusSkipped, true, "is not built yet")
	requireCtfAlerts(t, f.alerts)
	// past a quarter: late
	f.chain.head = end + 301
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusSkipped, true, "is not built yet")
	requireCtfAlerts(t, f.alerts, StCtfAlertCensusLate+" epoch 8")
	// the latest block already at the window's end: held, never sent late
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.chain.latest = end + 600
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusSkipped, true, "the census window ends at block")
	requireCtfLog(t, f.takeLog())
	// past the window: missed
	f.chain.head = end + 601
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusMissed, false, "the census window closed")
	requireCtfLog(t, f.takeLog())
	requireCtfAlerts(t, f.alerts, StCtfAlertCensusLate+" epoch 8", StCtfAlertCensusMissed+" epoch 8")

	// the epoch has no draw, and its funding all carries
	f.chain.latest = 0
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "close 8", "commit 8 prev 0")
	result := f.verify(nil)
	requireCtfAudit(t, result)
	if e := result.Epochs[0]; e.Census != nil || e.Drawn || len(e.Awards) != 0 || !e.Carried.Eq(&e.Fresh) {
		t.Fatalf("no census: %+v", e)
	}
}

// §4.3: the epoch's build waits for its census window to answer, then for
// ρ_k, the first round at least 60 s after the census block; it never draws
// with an earlier round.
func TestStCtfDriverWaitsForTheCensusAndItsRound(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1003)
	f.emission(8, 1_000_000_000_000)
	end := f.chain.end(8)
	f.chain.head = end

	// the census may still come: closed, not built
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusSkipped, true, "may be committed until block")
	requireCtfLog(t, f.takeLog(), "close 8")

	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusConfirmed, false, "")
	f.takeLog()
	now := stCtfServerNow
	t.Cleanup(func() { stCtfServerNow = now })
	roundAt := stCtfRoundTime(1003)
	stCtfServerNow = func() time.Time { return roundAt.Add(-time.Second) }
	outcome := f.run(StCtfDriverStepEpoch, 8)
	requireLedgerV2Outcome(t, outcome, model.StPublishStatusSkipped, true, "drand round 1003")
	if !outcome.RetryAt.After(roundAt) {
		t.Fatalf("retry at %s, before the round at %s", outcome.RetryAt, roundAt)
	}
	requireCtfLog(t, f.takeLog())
	stCtfServerNow = func() time.Time { return roundAt.Add(time.Second) }
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "commit 8 prev 0")
	result := f.verify(nil)
	requireCtfAudit(t, result)
	if result.Epochs[0].Round != 1003 {
		t.Fatalf("round %d", result.Epochs[0].Round)
	}
}

// §4.6: the carry is recycled only once its root is final, and then the next
// epoch is matured. It returns the carry account and keeps its residual; a
// second recycle finds nothing above the residual, and with nothing to move
// it absorbs nothing from vault2's list, even full.
func TestStCtfDriverRecyclesOnlyFinalRoots(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	f.run(StCtfDriverStepCensus, 8)
	f.run(StCtfDriverStepEpoch, 8)
	f.takeLog()

	// pending, not final: nothing is sent
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusSkipped, false, "is not final")
	requireCtfLog(t, f.takeLog())

	// final: the carry account is claimed and returned
	f.chain.approvals = 1
	f.chain.head += f.chain.challenge
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepFinalize, 8), model.StPublishStatusConfirmed, false, "")
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "finalize 8", "recycle root 0")
	returned := f.chain.recycled[9]
	if claimed := ctfTestCarryClaimed(f.chain); returned.IsZero() || !returned.Eq(&claimed) || f.chain.held.Uint64() != ctfTestSeedRao || f.chain.residual.Uint64() != ctfTestSeedRao {
		t.Fatalf("returned %s of the carry account's %s, keeping %s of the residual %s", returned.Dec(), claimed.Dec(), f.chain.held.Dec(), f.chain.residual.Dec())
	}
	full := [][32]byte{ctfTestEscrowHotkey}
	for i := 0; len(full) < stCtfStakingHotkeysMax; i++ {
		full = append(full, [32]byte{0xd1, byte(i)})
	}
	f.chain.staking[ctfTestVaultColdkey] = full
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusSkipped, false, "NothingToRecycle")
	requireCtfLog(t, f.takeLog(), "recycle root 0")
	f.chain.staking[ctfTestVaultColdkey] = nil

	// epoch 9 captures it with its emission and is matured
	f.census[9] = ctfTestCensusOf(t, 9, ctfTestCandidates()...)
	f.commitRound(9, 1001)
	f.emission(9, 1_000_000_000_000)
	f.chain.head = f.chain.end(9)
	f.run(StCtfDriverStepCensus, 9)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 9), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "census 9", "close 9", "commit 9 prev 1")
	previous := f.chain.roots[0]
	result := f.verify(&ledger.Root{Epoch: previous.Epoch, Root: previous.Root, Total: previous.Total, LeafCount: previous.LeafCount, LedgerHash: previous.LedgerHash})
	requireCtfAudit(t, result)
	if e := result.Epochs[0]; !e.Matured || !e.Returned.Eq(&returned) || e.Owed.IsZero() {
		t.Fatalf("epoch 9: matured %t returned %s of %s owed", e.Matured, e.Returned.Dec(), e.Owed.Dec())
	}
	requireCtfAlerts(t, f.alerts)
}

// ctfTestCarryClaimed is what vault2 has paid the carry account of the CTF
// pool: the only account claimed in these tests.
func ctfTestCarryClaimed(chain *fakeCtfChain) uint256.Int {
	var claimed uint256.Int
	for key, amount := range chain.claimed {
		if key[0] == ctfTestPoolId {
			claimed.Add(&claimed, &amount)
		}
	}
	return claimed
}

// ctfTestFinalRoot runs epoch 8 to a final root whose carry account is not
// yet claimed.
func ctfTestFinalRoot(t *testing.T) *ctfFixture {
	t.Helper()
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	f.run(StCtfDriverStepCensus, 8)
	f.run(StCtfDriverStepEpoch, 8)
	f.chain.approvals = 1
	f.chain.head += f.chain.challenge
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepFinalize, 8), model.StPublishStatusConfirmed, false, "")
	f.takeLog()
	return f
}

// §4.6: root's sweep cleared the carry's residual, so vault2 would refuse to
// pay it the carry account and the recycle would revert ResidualMissing. The
// driver sends nothing, absorbs nothing, records nothing that ends the step,
// alerts with the TAO a seed() needs and tries again an hour later; the
// recycle stays planned. Once anyone seeds, it returns the carry and keeps
// the new residual.
func TestStCtfDriverWaitsForASeedWithoutTheResidual(t *testing.T) {
	f := ctfTestFinalRoot(t)
	f.chain.sweep(2_000_000_000)
	full := [][32]byte{ctfTestEscrowHotkey}
	for i := 0; len(full) < stCtfStakingHotkeysMax; i++ {
		full = append(full, [32]byte{0xd2, byte(i)})
	}
	f.chain.staking[ctfTestVaultColdkey] = full
	before := stCtfServerNow()
	outcome := f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0})
	requireLedgerV2Outcome(t, outcome, model.StPublishStatusSkipped, true, "seed() with at least 19000000000 rao")
	if outcome.RetryAt.Before(before.Add(stCtfResidualWait - time.Minute)) {
		t.Fatalf("retried at %s, from %s", outcome.RetryAt, before)
	}
	requireCtfLog(t, f.takeLog())
	if status, _ := f.records.Outcome(f.ctx, 8, model.StPublishKindCtfRecycle); status != "" {
		t.Fatalf("the waiting recycle is recorded %s", status)
	}
	f.records.mirror(f.chain)
	if got := f.sync(9); !slices.Contains(got, "ctf_recycle 8") {
		t.Fatalf("planned %q", got)
	}
	requireCtfAlerts(t, f.alerts, StCtfAlertCarryResidual+" root 0 missing")

	// a seed of the swept TAO alone is refused; with the rest it restores the
	// residual, and the recycle goes on
	if err := f.chain.seed(0); err == nil {
		t.Fatal("a seed below the least")
	}
	if err := f.chain.seed(19_000_000_000); err != nil {
		t.Fatal(err)
	}
	f.chain.staking[ctfTestVaultColdkey] = nil
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "recycle root 0")
	returned, claimed := f.chain.recycled[9], ctfTestCarryClaimed(f.chain)
	if returned.IsZero() || !returned.Eq(&claimed) || f.chain.residual.Uint64() != 20_000_000_000 || f.chain.held.Uint64() != 20_000_000_000 {
		t.Fatalf("returned %s of %s, keeping %s of the residual %s", returned.Dec(), claimed.Dec(), f.chain.held.Dec(), f.chain.residual.Dec())
	}

	// A sweep with no claim due is alerted at once, so the seed can come
	// before the next root's claim; the recycle ends at NothingToRecycle.
	g := ctfTestFinalRoot(t)
	requireLedgerV2Outcome(t, g.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	g.chain.sweep(2_000_000_000)
	g.takeLog()
	requireLedgerV2Outcome(t, g.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusSkipped, false, "NothingToRecycle")
	requireCtfLog(t, g.takeLog(), "recycle root 0")
	requireCtfAlerts(t, g.alerts, StCtfAlertCarryResidual+" root 0 missing")
}

// §4.6: a sweep after the driver read the residual makes the recycle revert
// ResidualMissing. The attempt fails without ending the step, alerted, and is
// retried an hour later; a failed read of the carry is retried as usual.
func TestStCtfDriverRecycleRevertsResidualMissing(t *testing.T) {
	f := ctfTestFinalRoot(t)
	f.chain.failNext("held", errors.New("rpc down"))
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusFailed, true, "rpc down")
	requireCtfLog(t, f.takeLog())

	f.chain.failNext("recycle", fakeLedgerV2Revert("ResidualMissing"))
	before := stCtfServerNow()
	outcome := f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0})
	requireLedgerV2Outcome(t, outcome, model.StPublishStatusFailed, true, "ResidualMissing")
	if outcome.RetryAt.Before(before.Add(stCtfResidualWait - time.Minute)) {
		t.Fatalf("retried at %s, from %s", outcome.RetryAt, before)
	}
	requireCtfLog(t, f.takeLog(), "recycle root 0")
	if status, _ := f.records.Outcome(f.ctx, 8, model.StPublishKindCtfRecycle); status != "" {
		t.Fatalf("a ResidualMissing revert ended the recycle as %s", status)
	}
	requireCtfAlerts(t, f.alerts, StCtfAlertCarryResidual+" root 0 missing")

	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "recycle root 0")
}

// §4.6: with the carry account claimed, a surplus above the residual worth
// less than the runtime's transfer floor cannot move, so the recycle ends
// without sending and the next root's claim moves it along; a claim of the
// carry account that someone else made before the driver is moved at once.
func TestStCtfDriverLeavesDustForTheNextRoot(t *testing.T) {
	f := ctfTestFinalRoot(t)
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	f.takeLog()
	var key [2]uint64
	var cumulative uint256.Int
	for k, amount := range f.chain.claimed {
		if k[0] == ctfTestPoolId {
			key, cumulative = k, amount
		}
	}
	// 99,999 rao at 1 TAO per α: below the 100,000 rao floor
	f.chain.held.AddUint64(&f.chain.held, 99_999)
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusSkipped, false, "transfer floor")
	requireCtfLog(t, f.takeLog())
	if status, _ := f.records.Outcome(f.ctx, 8, model.StPublishKindCtfRecycle); status != model.StPublishStatusSkipped {
		t.Fatalf("dust left the recycle %q", status)
	}
	// the same α is worth the floor at a higher price, and moves
	f.chain.alphaPrice = big.NewInt(2e18)
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "recycle root 0")
	requireCtfAlerts(t, f.alerts)

	g := ctfTestFinalRoot(t)
	g.chain.claimed[key] = cumulative
	g.chain.held.Add(&g.chain.held, &cumulative)
	requireLedgerV2Outcome(t, g.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, g.takeLog(), "recycle root 0")
	if returned := g.chain.recycled[9]; cumulative.IsZero() || !returned.Eq(&cumulative) || g.chain.held.Uint64() != ctfTestSeedRao {
		t.Fatalf("returned %s of the %s someone else claimed, keeping %s", returned.Dec(), cumulative.Dec(), g.chain.held.Dec())
	}
}

// §4.6: a position the runtime lowered below the residual is followed down
// and alerted; the recycle still returns the carry account above it.
func TestStCtfDriverAlertsALoweredResidual(t *testing.T) {
	f := ctfTestFinalRoot(t)
	f.chain.held.SetUint64(ctfTestSeedRao - 1)
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	returned, claimed := f.chain.recycled[9], ctfTestCarryClaimed(f.chain)
	if !returned.Eq(&claimed) || f.chain.residual.Uint64() != ctfTestSeedRao-1 {
		t.Fatalf("returned %s of %s, residual %s", returned.Dec(), claimed.Dec(), f.chain.residual.Dec())
	}
	requireCtfAlerts(t, f.alerts, StCtfAlertCarryResidual+" lowered 999999999")
}

// §4.7: when the carry is late, the epoch's winners get their slots now and
// their pot shares as pending awards, and the late return is alerted.
func TestStCtfDriverPendingAwardsWhenTheCarryIsLate(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.cfg.Ctf.PotCaptureAlertRao = 1
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	f.run(StCtfDriverStepCensus, 8)
	f.run(StCtfDriverStepEpoch, 8)
	f.chain.approvals = 1
	f.chain.head += f.chain.challenge
	f.run(StCtfDriverStepFinalize, 8)
	f.takeLog()

	// no recycle before epoch 9 closes
	f.census[9] = ctfTestCensusOf(t, 9, ctfTestCandidates()...)
	f.commitRound(9, 1001)
	f.emission(9, 1_000_000_000_000)
	f.chain.head = f.chain.end(9)
	f.run(StCtfDriverStepCensus, 9)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 9), model.StPublishStatusConfirmed, false, "")
	previous := f.chain.roots[0]
	result := f.verify(&ledger.Root{Epoch: previous.Epoch, Root: previous.Root, Total: previous.Total, LeafCount: previous.LeafCount, LedgerHash: previous.LedgerHash})
	requireCtfAudit(t, result)
	e := result.Epochs[0]
	deferred := 0
	for _, award := range e.Awards {
		if award.Deferred {
			deferred++
		}
	}
	if e.Matured || deferred == 0 || len(e.Pending) != deferred {
		t.Fatalf("epoch 9: matured %t, %d deferred awards, %d pending", e.Matured, deferred, len(e.Pending))
	}
	got := slices.Clone(*f.alerts)
	if !slices.Contains(got, StCtfAlertCarryLate+" epoch 9") {
		t.Fatalf("alerts %q", got)
	}
	captures := 0
	for _, alert := range got {
		if strings.HasPrefix(alert, StCtfAlertPotCapture+" epoch 9 ") {
			captures++
		}
	}
	if captures != deferred {
		t.Fatalf("%d pot capture alerts for %d pot shares: %q", captures, deferred, got)
	}
}

// §11.2: from E_c only. Before it, and for an epoch the pool does not cover,
// the driver sends nothing; with the terms unknown it waits.
func TestStCtfDriverIsGated(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.cfg.Ctf.AmendmentLines = []string{ctfTestEntryLine(t, 9)}
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusSkipped, false, "not in force for epoch 8")
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusSkipped, false, "not in force for epoch 8")
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 7), model.StPublishStatusSkipped, false, "is not an epoch of capture the flag pool")
	requireCtfLog(t, f.takeLog())

	// unknown terms: wait, never "not in force"
	signer, url, storer := stCtfAmendmentSigner, stCtfAmendmentUrl, stCtfAmendmentStorer
	t.Cleanup(func() { stCtfAmendmentSigner, stCtfAmendmentUrl, stCtfAmendmentStorer = signer, url, storer })
	stCtfAmendmentSigner = func(uint64, uint16) (common.Address, bool) { return common.Address{1}, true }
	stCtfAmendmentUrl = "http://127.0.0.1:1/amendments.yml"
	stCtfAmendmentStorer = func(*StConfig) amendment.Store { return &ctfTestAmendmentStore{} }
	stCtfAmendmentHolders.Clear()
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 9), model.StPublishStatusSkipped, true, "terms for epoch 9 are unknown")
	requireCtfLog(t, f.takeLog())

	// another mode's steps do nothing
	f.cfg.Ctf.Mode = StCtfModeShadow
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 9), model.StPublishStatusSkipped, false, "not live")
	f.cfg.Ctf.Mode = StCtfModeOff
	if steps, err := StCtfDriverSync(f.ctx, &StEpochState{Epoch: 10, HeadBlock: f.chain.head}); err != nil || steps != nil {
		t.Fatalf("off: %v %v", steps, err)
	}
	if _, err := StCtfDriverRunStep(f.ctx, &StCtfDriverStep{Kind: StCtfDriverStepEpoch, Epoch: 9}); !errors.Is(err, errStCtfNotConfigured) {
		t.Fatalf("off: %v", err)
	}
	requireCtfLog(t, f.takeLog())
}

// A committed census whose bytes are not canonical gives no draw, never a
// refusal; a committed census whose objects are lost stalls the epoch's root
// and is alerted.
func TestStCtfDriverInvalidAndLostCensus(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	invalid := []byte(`{"schema":"not a census"}`)
	ref, err := PutStLedgerV2Object(f.ctx, invalid)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := ledger.ParseRef(ref)
	f.chain.census[8] = fakeCtfCensus{hash: hash, block: f.chain.end(8) + 2}
	f.chain.times[f.chain.end(8)+2] = ctfTestCommitTime(1000)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusConfirmed, false, "")
	result := f.verify(nil)
	requireCtfAudit(t, result)
	if e := result.Epochs[0]; e.Census == nil || e.CensusErr == nil || e.Drawn {
		t.Fatalf("invalid census: %+v", e)
	}
	requireCtfAlerts(t, f.alerts, StCtfAlertCensusInvalid+" epoch 8")

	// epoch 9's census is committed, then lost from the store
	g := newCtfFixture(t, StCtfModeLive)
	g.emission(8, 1_000_000_000_000)
	g.chain.head = g.chain.end(8)
	lost := ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	g.chain.census[8] = fakeCtfCensus{hash: lost.Hash, block: g.chain.end(8) + 2}
	g.chain.times[g.chain.end(8)+2] = ctfTestCommitTime(1000)
	requireLedgerV2Outcome(t, g.run(StCtfDriverStepEpoch, 8), model.StPublishStatusFailed, true, "")
	requireCtfLog(t, g.takeLog(), "close 8")
	requireCtfAlerts(t, g.alerts, StCtfAlertCensusUnavailable+" epoch 8")
}

// The sync turn plans each epoch's census and build at its end, the
// finalize, record and recycle of the pool's roots, and alerts a carry not
// returned within 600 blocks of its root's finalization.
func TestStCtfSyncPlansTheSteps(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	if got := f.sync(9); !reflect.DeepEqual(got, []string{"ctf_commit_census 8", "ctf_epoch 8"}) {
		t.Fatalf("at end(8): %q", got)
	}
	f.run(StCtfDriverStepCensus, 8)
	if got := f.sync(9); !reflect.DeepEqual(got, []string{"ctf_epoch 8"}) {
		t.Fatalf("census committed: %q", got)
	}
	f.run(StCtfDriverStepEpoch, 8)
	f.records.mirror(f.chain)
	if got := f.sync(9); !reflect.DeepEqual(got, []string{"ctf_finalize 8"}) {
		t.Fatalf("committed: %q", got)
	}
	ledgerRow := f.records.ledgers[f.chain.pending.LedgerHash]
	if ledgerRow.Status != model.CtfLedgerStatusCommitted || ledgerRow.Seq == nil || *ledgerRow.Seq != f.chain.pending.Seq {
		t.Fatalf("committed ledger %+v", ledgerRow)
	}
	f.chain.approvals = 1
	f.chain.head += f.chain.challenge
	f.run(StCtfDriverStepFinalize, 8)
	f.records.mirror(f.chain)
	if got := f.sync(9); !reflect.DeepEqual(got, []string{"ctf_record 8", "ctf_recycle 8"}) {
		t.Fatalf("final: %q", got)
	}
	requireCtfAlerts(t, f.alerts)
	f.chain.head += stCtfCarryLateBlocks + 1
	f.sync(9)
	requireCtfAlerts(t, f.alerts, StCtfAlertCarryLate+" root 0")

	// recorded and recycled: nothing more
	root := f.records.roots[0]
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecord, Seq: root.Seq}), model.StPublishStatusConfirmed, false, "recorded")
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	if got := f.sync(9); len(got) != 0 {
		t.Fatalf("done: %q", got)
	}
	if ledgerRow.Status != model.CtfLedgerStatusFinalized || ledgerRow.RootIndex == nil || *ledgerRow.RootIndex != 0 {
		t.Fatalf("finalized ledger %+v", ledgerRow)
	}
	recorded := f.records.recorded[root.Seq]
	var sum uint256.Int
	for _, amount := range recorded.amounts {
		if amount.Epoch != 8 || amount.NoId != ctfTestPoolId {
			t.Fatalf("amount %+v", amount)
		}
		sum.Add(&sum, &amount.Amount)
	}
	if recorded.firstIndex != 0 || uint64(len(recorded.newColdkeys)) != root.LeafCount || !sum.Eq(&root.Total) {
		t.Fatalf("recorded %d new accounts from %d, %s of %s", len(recorded.newColdkeys), recorded.firstIndex, sum.Dec(), root.Total.Dec())
	}
}

// §4.7: a missed close defers the epoch, whose emission funds the next
// captured one, and a missed commit leaves the epoch to the next root; that
// root covers both epochs with their committed censuses and draws, and the
// audit reproduces it.
func TestStCtfDriverDeferredAndMissedEpochs(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusConfirmed, false, "")
	// the epoch step does not run until its close window has passed
	f.chain.head = f.chain.end(8) + f.chain.window + 1
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusMissed, false, "close window closed")
	requireCtfLog(t, f.takeLog(), "census 8")
	requireCtfAlerts(t, f.alerts, StCtfAlertMissedClose+" epoch 8")

	// epoch 9: its close defers 8; its root covers 8 and 9
	f.census[9] = ctfTestCensusOf(t, 9, ctfTestCandidates()[3:]...)
	f.commitRound(9, 1001)
	f.emission(9, 2_000_000_000_000)
	f.chain.head = f.chain.end(9)
	f.run(StCtfDriverStepCensus, 9)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 9), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "census 9", "close 9", "defer 8", "commit 9 prev 0")
	result := f.verify(nil)
	requireCtfAudit(t, result)
	if len(result.Epochs) != 2 || !result.Epochs[0].Funding.IsZero() || !result.Epochs[0].Drawn || !result.Epochs[1].Drawn {
		t.Fatalf("epochs %d: %+v", len(result.Epochs), result.Epochs)
	}
	if f.records.results[8] == nil || f.records.results[9] == nil || f.records.results[8].LedgerEpoch != 9 {
		t.Fatalf("results %+v %+v", f.records.results[8], f.records.results[9])
	}

	// epoch 10: its commit is missed; epoch 11's root covers 10 and 11 on root 0
	f.chain.approvals = 1
	f.chain.head += f.chain.challenge
	f.run(StCtfDriverStepFinalize, 9)
	f.census[10] = ctfTestCensusOf(t, 10, ctfTestCandidates()...)
	f.commitRound(10, 1002)
	f.emission(10, 1_000_000_000_000)
	f.chain.head = f.chain.end(10)
	f.run(StCtfDriverStepCensus, 10)
	f.chain.failNext("commit", fakeLedgerV2Revert("InvalidWindow"))
	f.run(StCtfDriverStepEpoch, 10)
	f.chain.head = f.chain.end(10) + f.chain.window + 1
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 10), model.StPublishStatusMissed, false, "commit window closed")
	f.census[11] = ctfTestCensusOf(t, 11, ctfTestCandidates()...)
	f.commitRound(11, 1003)
	f.emission(11, 1_000_000_000_000)
	f.chain.head = f.chain.end(11)
	f.run(StCtfDriverStepCensus, 11)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 11), model.StPublishStatusConfirmed, false, "")
	previous := f.chain.roots[0]
	result = f.verify(&ledger.Root{Epoch: previous.Epoch, Root: previous.Root, Total: previous.Total, LeafCount: previous.LeafCount, LedgerHash: previous.LedgerHash})
	requireCtfAudit(t, result)
	if len(result.Epochs) != 2 || result.Epochs[0].Epoch != 10 || result.Epochs[0].AwardsRef != ledger.Ref(f.records.results[10].AwardsRef) {
		t.Fatalf("epochs %+v", result.Epochs)
	}
}

// §5 fixed inputs: an epoch's result binds once a committed root carries it.
// A build on the latest final root that is never committed binds nothing:
// when the pending root then finalizes, the epoch is built on it, with other
// awards (its carry is not back), and committed.
func TestStCtfDriverRebuildsAResultNoCommittedRootCarries(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	f.run(StCtfDriverStepCensus, 8)
	f.run(StCtfDriverStepEpoch, 8)
	f.chain.approvals = 1
	first := f.chain.pending.LedgerHash

	// epoch 9 closes, then settlement pauses: the root of 8 cannot finalize,
	// epoch 9 is built on no root, covering 8 and 9, and its commit waits
	f.census[9] = ctfTestCensusOf(t, 9, ctfTestCandidates()...)
	f.commitRound(9, 1001)
	f.emission(9, 1_000_000_000_000)
	f.chain.head = f.chain.end(9)
	f.run(StCtfDriverStepCensus, 9)
	f.chain.afterClose = func() { f.chain.paused = true }
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 9), model.StPublishStatusSkipped, true, "settlement is paused")
	provisional := f.records.results[9]
	if provisional == nil || f.records.results[8].LedgerHash != first {
		t.Fatalf("results 8 %+v 9 %+v", f.records.results[8], provisional)
	}

	// unpaused: the root of 8 finalizes, and 9 is built on it
	f.chain.paused, f.chain.afterClose = false, nil
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 9), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "census 8", "close 8", "commit 8 prev 0", "census 9", "close 9", "finalize 8", "commit 9 prev 1")
	previous := f.chain.roots[0]
	result := f.verify(&ledger.Root{Epoch: previous.Epoch, Root: previous.Root, Total: previous.Total, LeafCount: previous.LeafCount, LedgerHash: previous.LedgerHash})
	requireCtfAudit(t, result)
	fixed := f.records.results[9]
	if fixed == nil || fixed.LedgerHash != f.chain.pending.LedgerHash || ledger.Ref(fixed.AwardsRef) != result.Epochs[0].AwardsRef || fixed.AwardsRef == provisional.AwardsRef {
		t.Fatalf("epoch 9: provisional %+v, fixed %+v", provisional, fixed)
	}
	if result.Epochs[0].Matured {
		t.Fatal("epoch 9 matured on the root of 8, whose carry is not back")
	}
	if f.records.results[8].LedgerHash != first {
		t.Fatal("the committed result of epoch 8 changed")
	}
}

// §4.3: a commitment on chain other than the census built is alerted, and
// the chain's commitment is what is recorded.
func TestStCtfDriverRecordsTheChainsCommitment(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	other := ctfTestCensusOf(t, 8, ctfTestCandidates()[:2]...)
	f.chain.head = f.chain.end(8)
	f.chain.census[8] = fakeCtfCensus{hash: other.Hash, block: f.chain.end(8) + 7}
	f.chain.times[f.chain.end(8)+7] = ctfTestCommitTime(1002)
	requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusSkipped, false, "is committed at block")
	requireCtfLog(t, f.takeLog())
	requireCtfAlerts(t, f.alerts, StCtfAlertCensusMismatch+" epoch 8")
	commitment := f.records.commitments[8]
	if commitment == nil || commitment.CensusHash != other.Hash || commitment.Status != model.CtfCommitmentStatusCommitted || *commitment.Round != 1002 {
		t.Fatalf("commitment %+v", commitment)
	}
}

// §4.6 (sn-contracts' open issue 3): while third parties hold vault2's
// staking hotkey list at 128 without the pool hotkey, the recycle would be
// refused, so the driver absorbs a foreign position first, passing over the
// escrow and pool hotkeys, then recycles.
func TestStCtfDriverFreesAStakingHotkeyForTheRecycle(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	f.run(StCtfDriverStepCensus, 8)
	f.run(StCtfDriverStepEpoch, 8)
	f.chain.approvals = 1
	f.chain.head += f.chain.challenge
	f.run(StCtfDriverStepFinalize, 8)
	f.takeLog()

	otherPool := [32]byte{0x91}
	f.chain.poolHotkeys[otherPool] = true
	list := [][32]byte{ctfTestEscrowHotkey, otherPool}
	for i := 0; len(list) < stCtfStakingHotkeysMax; i++ {
		list = append(list, [32]byte{0xd0, byte(i)})
	}
	f.chain.staking[ctfTestVaultColdkey] = list
	outcome := f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0})
	requireLedgerV2Outcome(t, outcome, model.StPublishStatusSkipped, true, "was freed on hotkey 0xd0")
	requireCtfLog(t, f.takeLog(), "absorb 91", "absorb d0")
	if got := len(f.chain.staking[ctfTestVaultColdkey]); got != stCtfStakingHotkeysMax-1 {
		t.Fatalf("%d staking hotkeys", got)
	}
	requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusConfirmed, false, "")
	requireCtfLog(t, f.takeLog(), "recycle root 0")
	returned := f.chain.recycled[9]
	if !slices.Contains(f.chain.staking[ctfTestVaultColdkey], ctfTestPoolHotkey) || returned.IsZero() {
		t.Fatal("the carry did not return")
	}
	requireCtfAlerts(t, f.alerts)

	// nothing foreign left to free: alerted, and the recycle waits
	g := newCtfFixture(t, StCtfModeLive)
	g.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	g.commitRound(8, 1000)
	g.emission(8, 1_000_000_000_000)
	g.chain.head = g.chain.end(8)
	g.run(StCtfDriverStepCensus, 8)
	g.run(StCtfDriverStepEpoch, 8)
	g.chain.approvals = 1
	g.chain.head += g.chain.challenge
	g.run(StCtfDriverStepFinalize, 8)
	g.takeLog()
	full := [][32]byte{ctfTestEscrowHotkey}
	for i := 0; len(full) < stCtfStakingHotkeysMax; i++ {
		hotkey := [32]byte{0x92, byte(i)}
		g.chain.poolHotkeys[hotkey] = true
		full = append(full, hotkey)
	}
	g.chain.staking[ctfTestVaultColdkey] = full
	requireLedgerV2Outcome(t, g.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecycle, RootIndex: 0}), model.StPublishStatusSkipped, true, "list is full")
	if got := len(g.takeLog()); got != stCtfAbsorbTries {
		t.Fatalf("%d absorbs tried", got)
	}
	requireCtfAlerts(t, g.alerts, StCtfAlertCarryLate+" root 0 staking hotkeys")
}
