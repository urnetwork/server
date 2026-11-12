package controller

// sn_claim_relay_pools_test.go — the claim relay over the operator's two pools,
// its pool and its capture the flag pool (sn CAPTURETHEFLAG.md §10): the unit is
// (coldkey, pool), a due account takes its coldkey's other accounts above the
// floor into the same batch, Claim now claims every pool, and the 30-day limit
// stays per coldkey. Like sn_claim_relay_test.go, nothing here reads a database,
// Redis or a network.

import (
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"
)

// The accounts the world's transactions sent, by pool and index, once each.
func (self *claimRelayWorld) sentKeys() []string {
	keys := []string{}
	for _, claim := range self.claimsSent() {
		keys = append(keys, claim.Account.key())
	}
	return keys
}

func claimRelayCtfKey(index uint64) string {
	return snClaimRelayAccountKey(claimRelayTestCtfPool, index)
}

func requireClaimRelaySent(t *testing.T, world *claimRelayWorld, want ...string) {
	t.Helper()
	got := world.sentKeys()
	if !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

// A due account takes its coldkey's capture the flag account into the same
// batch, though that one is far under the threshold: a batch adds one claim's
// gas. Another coldkey's account under the threshold waits.
func TestClaimRelayDueAccountTakesItsOtherPools(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, over, small)
	world.batching = true
	world.addCtf(0, small)
	world.addCtf(1, small)
	status := world.passes()
	requireClaimRelayState(t, status, SnClaimRelayStateRunning)
	if len(world.txs) != 1 || !world.txs[0].send.Batch {
		t.Fatalf("transactions %d", len(world.txs))
	}
	requireClaimRelaySent(t, world, "1/0", claimRelayCtfKey(0))
	if !world.txs[0].send.Claims[1].Companion || world.txs[0].send.Claims[0].Companion {
		t.Fatal("the companion is not the capture the flag account")
	}
	if !status.Batching {
		t.Fatal("the status does not say the relay batches")
	}
}

// Without a batcher each claim goes alone, and only a due one: a capture the
// flag account under the threshold is not worth a transaction of its own.
func TestClaimRelayNoCompanionsWithoutABatcher(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, over)
	world.addCtf(0, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	world.passes()
	requireClaimRelaySent(t, world, "1/0")
}

// A companion under the floor would revert, so it stays; one over the threshold
// is due in its own right, and either way each account is claimed once.
func TestClaimRelayCompanionsClearTheFloorAndGoOnce(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, over, over)
	world.batching = true
	world.addCtf(0, claimRelayTestAlpha(snClaimRelayFloorTaoRao/2))
	world.addCtf(1, claimRelayTestAlpha(3*snClaimRelayAutomaticTaoRao))
	world.passes()
	sent := world.sentKeys()
	slices.Sort(sent)
	if want := []string{"1/0", "1/1", claimRelayCtfKey(1)}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

// The chain's read decides a companion too: one someone else has paid, or one
// the chain puts under the floor, is not sent, and the due account goes alone.
func TestClaimRelayCompanionOnTheChainsOwnRead(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, over)
	world.batching = true
	index := world.addCtf(0, small)
	world.chainClaimed[claimRelayTestCtfSlot+index] = small
	world.passes()
	requireClaimRelaySent(t, world, "1/0")
}

// A companion goes only with its due account: when the due one is refused at
// preflight, the companion waits too.
func TestClaimRelayCompanionWaitsForItsDueAccount(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, over)
	world.batching = true
	world.addCtf(0, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	world.refusals[0] = errors.New("execution reverted")
	world.pass()
	if len(world.txs) != 0 {
		t.Fatalf("sent %v", world.sentKeys())
	}
}

// A coldkey's accounts go into one transaction or wait together: a group that
// does not fit what is left of a batch goes first in the next pass, rather than
// leaving its companion with no due account to go with.
func TestClaimRelayAGroupIsNotSplitAcrossBatches(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	// 31 due accounts larger than coldkey 31's, which has a companion
	amounts := claimRelayRepeat(new(uint256.Int).Mul(over, uint256.NewInt(2)), snClaimRelayBatchMaximum-1)
	amounts = append(amounts, over)
	world := newClaimRelayWorld(t, amounts...)
	world.batching = true
	// cheap enough that funds never cut a batch
	world.gasPriceWei, world.feeCapWei = big.NewInt(100_000_000), big.NewInt(100_000_000)
	companion := world.addCtf(snClaimRelayBatchMaximum-1, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	status := world.pass()
	if !status.Again || len(world.txs) != 1 || len(world.txs[0].send.Claims) != snClaimRelayBatchMaximum-1 {
		t.Fatalf("again %t, %d transactions, first of %d", status.Again, len(world.txs), len(world.txs[0].send.Claims))
	}
	world.pass()
	second := world.txs[1].send.Claims
	if len(second) != 2 || second[0].Account.key() != snClaimRelayAccountKey(1, snClaimRelayBatchMaximum-1) || second[1].Account.key() != claimRelayCtfKey(companion) {
		t.Fatalf("the second batch is %v", world.sentKeys()[len(world.txs[0].send.Claims):])
	}
}

// Claim now claims every pool's account of the coldkey above the floor, in one
// batch, finishes the request, and starts the coldkey's one limit.
func TestClaimRelayClaimNowClaimsEveryPool(t *testing.T) {
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, small, small)
	world.batching = true
	world.addCtf(0, small)
	world.addCtf(1, small)
	if !world.tap(0) {
		t.Fatal("the tap was refused")
	}
	world.passes()
	requireClaimRelaySent(t, world, "1/0", claimRelayCtfKey(0))
	if _, queued := world.requests[claimRelayTestColdkey(0)]; queued {
		t.Fatal("the request stayed queued")
	}
	if !world.limited(0) || world.limited(1) {
		t.Fatalf("limited %t %t", world.limited(0), world.limited(1))
	}
	if world.tap(0) {
		t.Fatal("tapped again within the limit")
	}
}

// Without a batcher a tap's claims go one a pass. The request stays until the
// last is paid, the limit starts with the first and is not started again, and
// a pass estimates only the claim it sends.
func TestClaimRelayClaimNowAcrossPassesWithoutABatcher(t *testing.T) {
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, small)
	world.addCtf(0, small)
	world.tap(0)
	status := world.pass()
	requireClaimRelaySent(t, world, "1/0")
	if len(world.estimated) != 1 || !status.Again {
		t.Fatalf("estimated %d, again %t", len(world.estimated), status.Again)
	}
	coldkey := claimRelayTestColdkey(0)
	if _, queued := world.requests[coldkey]; !queued {
		t.Fatal("the request ended with an account still to claim")
	}
	started, limited := world.limits[coldkey]
	if !limited {
		t.Fatal("the first paid claim did not start the limit")
	}
	world.nowTime = world.nowTime.Add(time.Minute)
	world.passes()
	requireClaimRelaySent(t, world, "1/0", claimRelayCtfKey(0))
	if _, queued := world.requests[coldkey]; queued {
		t.Fatal("the request stayed queued after its last account was paid")
	}
	if world.limits[coldkey] != started {
		t.Fatal("the limit started again")
	}
}

// A tap on a coldkey one of whose accounts is due moves the batch ahead: the
// relay would have sent both by itself, so nothing is used up.
func TestClaimRelayClaimNowOfADueColdkeyUsesNoLimit(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.batching = true
	world.addCtf(0, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	world.tap(0)
	world.pass()
	requireClaimRelaySent(t, world, "1/0", claimRelayCtfKey(0))
	for _, claim := range world.claimsSent() {
		if !claim.Requested || claim.UnderThreshold {
			t.Fatalf("claim %s requested %t under %t", claim.Account.key(), claim.Requested, claim.UnderThreshold)
		}
	}
	if _, queued := world.requests[claimRelayTestColdkey(0)]; queued || world.limited(0) {
		t.Fatalf("queued %t limited %t", queued, world.limited(0))
	}
}

// A tap with every account under the floor, or paid, is given back.
func TestClaimRelayClaimNowOfNothingAboveTheFloorInAnyPool(t *testing.T) {
	under := claimRelayTestAlpha(snClaimRelayFloorTaoRao / 2)
	world := newClaimRelayWorld(t, under)
	world.addCtf(0, under)
	world.tap(0)
	world.pass()
	if _, queued := world.requests[claimRelayTestColdkey(0)]; queued || world.limited(0) || len(world.txs) != 0 {
		t.Fatalf("queued %t limited %t sent %d", queued, world.limited(0), len(world.txs))
	}
}

// A pool whose ledger cannot be read leaves the other's claims alone, and the
// pass says the ledger is unavailable, naming the pool, so that the stall alert
// fires. A request stays while its coldkey may hold an account there: the
// account index records one, or the index cannot be read either.
func TestClaimRelayOnePoolUnavailable(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, over, small, small)
	world.batching = true
	world.addCtf(0, small)
	world.addCtf(1, small)
	world.ctfLedgerErr = errors.New("the capture the flag ledger cannot be read")
	world.tap(1)
	// coldkey 2 has no account in the capture the flag pool
	world.tap(2)
	status := world.passes()
	requireClaimRelayState(t, status, SnClaimRelayStateLedgerUnavailable)
	if !strings.Contains(status.Detail, "4294967297") {
		t.Fatalf("detail %q", status.Detail)
	}
	sent := world.sentKeys()
	slices.Sort(sent)
	if want := []string{"1/0", "1/1", "1/2"}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
	if !slices.Equal(status.UnavailablePools, []uint64{claimRelayTestCtfPool}) {
		t.Fatalf("unavailable %v", status.UnavailablePools)
	}
	if _, queued := world.requests[claimRelayTestColdkey(1)]; !queued {
		t.Fatal("a request ended while its coldkey's account in a pool could not be read")
	}
	if _, queued := world.requests[claimRelayTestColdkey(2)]; queued {
		t.Fatal("a request with nothing in the unreadable pool stayed")
	}
	if !world.limited(1) || !world.limited(2) {
		t.Fatal("a paid tap did not start its limit")
	}
	// claims move at the busy cadence while the other pool cannot be read
	if delay := snClaimRelayNextDelay(&SnClaimRelayStatus{State: SnClaimRelayStateLedgerUnavailable, UnavailablePools: status.UnavailablePools, Again: true}); delay != snClaimRelayBusyDelay {
		t.Fatalf("a partial pass waits %s", delay)
	}
	if delay := snClaimRelayNextDelay(&SnClaimRelayStatus{State: SnClaimRelayStateLedgerUnavailable, Again: true}); delay != snClaimRelayIdleDelay {
		t.Fatalf("a pass that read no pool waits %s", delay)
	}

	// once it can be read again, the rest of the request is claimed
	world.ctfLedgerErr = nil
	requireClaimRelayState(t, world.passes(), SnClaimRelayStateRunning)
	if sent := world.sentKeys(); sent[len(sent)-1] != claimRelayCtfKey(1) {
		t.Fatalf("sent %v", sent)
	}
	if _, queued := world.requests[claimRelayTestColdkey(1)]; queued {
		t.Fatal("the request stayed queued")
	}

	// with the index unreadable too, every request waits
	world.nowTime = world.nowTime.Add(snClaimRelayClaimNowPeriod + time.Hour)
	world.setLeaves(over, small, small, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao+1))
	world.ctfLedgerErr, world.indexUnreadable = errors.New("the capture the flag ledger cannot be read"), true
	world.tap(3)
	world.passes()
	if _, queued := world.requests[claimRelayTestColdkey(3)]; !queued {
		t.Fatal("a request ended though the unreadable pool's holders are unknown")
	}

	// both pools unavailable is the ledger unavailable, and nothing is read
	world.ledgerErr = errors.New("the ledger cannot be read")
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateLedgerUnavailable)
}

// The operator's pool cannot be read while the capture the flag pool has a
// root: the CTF pool's accounts are still claimed, the pass names the
// operator's pool, and a request of a coldkey that pool may hold waits.
func TestClaimRelayOperatorPoolUnavailable(t *testing.T) {
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, small, small)
	world.addCtf(0, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.poolLedgerErr = errors.New("the pool's ledger cannot be read")
	world.tap(1)
	status := world.passes()
	requireClaimRelayState(t, status, SnClaimRelayStateLedgerUnavailable)
	requireClaimRelaySent(t, world, claimRelayCtfKey(0))
	if !slices.Equal(status.UnavailablePools, []uint64{1}) {
		t.Fatalf("unavailable %v", status.UnavailablePools)
	}
	if _, queued := world.requests[claimRelayTestColdkey(1)]; !queued {
		t.Fatal("a request ended while its coldkey's account in the pool could not be read")
	}
}

// With no capture the flag root, as with capture the flag off, a pass that
// cannot read the operator's pool ends there as it did before the CTF pool:
// the ledger is unavailable whatever else would stop it, here a low balance,
// nothing is sent, and every request waits, a coldkey's the pool does not
// record too.
func TestClaimRelayOperatorPoolUnavailableBeforeTheCtfPoolsFirstRoot(t *testing.T) {
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, small, small)
	world.poolLedgerErr = errors.New("the pool's ledger cannot be read")
	world.balanceWei = big.NewInt(1)
	world.tap(1)
	world.tap(5)
	status := world.pass()
	requireClaimRelayState(t, status, SnClaimRelayStateLedgerUnavailable)
	if len(world.txs) != 0 || len(status.UnavailablePools) != 0 {
		t.Fatalf("sent %d, unavailable %v", len(world.txs), status.UnavailablePools)
	}
	for _, index := range []int{1, 5} {
		if _, queued := world.requests[claimRelayTestColdkey(index)]; !queued {
			t.Fatalf("coldkey %d's request ended while the ledger could not be read", index)
		}
	}
}

// A batch ends where a group ends: when the batcher fits part of a coldkey's
// group, the group waits for the next transaction, whole.
func TestClaimRelayABatchEndsAtAGroupsEnd(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, new(uint256.Int).Mul(over, uint256.NewInt(2)), over)
	world.batching, world.batchMost = true, 2
	companion := world.addCtf(1, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	world.pass()
	if len(world.txs) != 1 || world.txs[0].send.Batch || world.txs[0].send.Claims[0].Account.key() != "1/0" {
		t.Fatalf("first %v", world.sentKeys())
	}
	world.pass()
	second := world.txs[1].send
	if !second.Batch || len(second.Claims) != 2 || second.Claims[0].Account.key() != "1/1" || second.Claims[1].Account.key() != claimRelayCtfKey(companion) {
		t.Fatalf("second %v", world.sentKeys())
	}
}

// A group that cannot be batched sends its first claim alone, its due account,
// and its companion is not sent: it is not due.
func TestClaimRelayACompanionWithoutABatch(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, over)
	world.batching = true
	world.batchErr = errors.New("the batcher is unavailable")
	world.addCtf(0, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	world.passes()
	requireClaimRelaySent(t, world, "1/0")
}

// A tap whose batch cannot go sends the due account alone first, which uses
// nothing up; the account under the threshold then goes alone, which the relay
// would not have sent by itself, so the limit starts when it is paid.
func TestClaimRelayClaimNowWithoutABatchStartsTheLimitAtTheUnderThresholdClaim(t *testing.T) {
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, small)
	world.batching = true
	world.batchErr = errors.New("the batcher is unavailable")
	world.addCtf(0, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.tap(0)
	world.pass()
	requireClaimRelaySent(t, world, claimRelayCtfKey(0))
	if world.limited(0) {
		t.Fatal("a due claim sent alone started the limit")
	}
	if _, queued := world.requests[claimRelayTestColdkey(0)]; !queued {
		t.Fatal("the request ended with an account still to claim")
	}
	world.passes()
	requireClaimRelaySent(t, world, claimRelayCtfKey(0), "1/0")
	if !world.limited(0) {
		t.Fatal("the tapped claim under the threshold did not start the limit")
	}
	if _, queued := world.requests[claimRelayTestColdkey(0)]; queued {
		t.Fatal("the request stayed queued")
	}
}

// The api says why each pool's balance is pending: a companion of a due
// account is queued while the relay batches, a pool the relay could not read
// waits as a paused relay does, and each row names its pool.
func TestClaimRelayAccountViewOfThePools(t *testing.T) {
	now := claimRelayTestMonday
	small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
	zero := new(uint256.Int)
	status := &SnClaimRelayStatus{State: SnClaimRelayStateRunning, PassTime: now, AlphaPriceWei: claimRelayTestAlphaPrice.String(), Batching: true}
	entry := claimRelayLedgerEntry(small, zero, nil)
	entry.NoId = claimRelayTestCtfPool
	view := func(withDue bool) *SnClaimRelayAccount {
		t.Helper()
		account, err := snClaimRelayAccountView(status, "ss58", entry, false, withDue, time.Time{}, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	if account := view(false); account.Reason != SnClaimRelayReasonUnderThreshold || account.Kind != StLedgerV2PoolKindCtf || account.Label != "Capture the flag" {
		t.Fatalf("alone: %+v", account)
	}
	if account := view(true); account.Reason != SnClaimRelayReasonQueued || !account.ClaimNowAvailable {
		t.Fatalf("with a due account: %+v", account)
	}
	status.Batching = false
	if account := view(true); account.Reason != SnClaimRelayReasonUnderThreshold {
		t.Fatalf("with a due account and no batcher: %+v", account)
	}
	// a pass that could not read the capture the flag pool says the ledger is
	// unavailable: that pool waits, the other is claimed
	status.State, status.UnavailablePools = SnClaimRelayStateLedgerUnavailable, []uint64{claimRelayTestCtfPool}
	if account := view(true); account.Reason != SnClaimRelayReasonRelayPaused || account.ClaimNowReason != SnClaimRelayReasonRelayPaused {
		t.Fatalf("an unavailable pool: %+v", account)
	}
	entry.NoId = 1
	if account := view(false); account.Reason != SnClaimRelayReasonUnderThreshold || !account.ClaimNowAvailable || account.Kind != StLedgerV2PoolKindPool || account.Label != "Pool 1" {
		t.Fatalf("the operator's pool: %+v", account)
	}
	// a ledger unavailable with no pool named pauses every pool
	status.UnavailablePools = nil
	if account := view(false); account.Reason != SnClaimRelayReasonRelayPaused {
		t.Fatalf("the ledger unavailable: %+v", account)
	}
}

// A pool's kind and label: an operator's pool, n below 2^32, and its capture
// the flag pool, 2^32 + n.
func TestStLedgerV2PoolKind(t *testing.T) {
	for _, c := range []struct {
		noId  uint64
		kind  string
		label string
	}{
		{1, StLedgerV2PoolKindPool, "Pool 1"},
		{7, StLedgerV2PoolKindPool, "Pool 7"},
		{1<<32 - 1, StLedgerV2PoolKindPool, "Pool 4294967295"},
		{1<<32 + 1, StLedgerV2PoolKindCtf, "Capture the flag"},
		{1<<32 + 7, StLedgerV2PoolKindCtf, "Capture the flag"},
	} {
		if kind, label := StLedgerV2PoolKind(c.noId); kind != c.kind || label != c.label {
			t.Fatalf("%d: %s %q", c.noId, kind, label)
		}
	}
	if pools := stLedgerV2OwnPools(&StConfig{NoId: 1}); !slices.Equal(pools, []uint64{1, 1<<32 + 1}) {
		t.Fatalf("own pools %v", pools)
	}
}
