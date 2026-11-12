package controller

// sn_ctf_api_claims.go — the claim states GET /sn/ctf/client judges its wins by
// (sn_ctf_api_wins.go): each winning coldkey's account in the CTF pool, its
// cumulative in each finalized root that pays one of its wins, and vault2's
// claimed. The answer gives them snCtfClaimsTimeout, about five seconds;
// vault2's claimed not read by then is the account's last value read, and a
// state with none answers claimed null, so a claim state the chain cannot give
// in time never fails the answer, nor holds it past that.
//
//   - The accounts come from the server's records and the ledger objects, each
//     read once for all of the answer's coldkeys: the count of recorded
//     accounts, the latest finalized root and its accounts, the account record
//     and the record of each root's deltas.
//   - vault2's claimed is read at one finalized block per answer, read once,
//     in eth_call batches of stMaximumEVMRPCBatchCalls, the coldkeys of the
//     newest wins first. Each value is answered as read for snCtfClaimedTtl,
//     so an answer reads only the accounts no answer has read lately, and
//     every coldkey is read however many a network has. Past that it is the
//     account's last value read, answered when a fresh read fails or does not
//     finish in time.
//   - Every read runs on the request's own goroutine: nothing it starts
//     outlives the request.

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server/model"
)

// How long vault2's claimed of an account is answered as read before an
// answer reads it again, counted from when the read began. After a claim the
// apps read the wins at its Done and again 30, 60 and 120 s after it, so a
// value kept under 30 s is never the one an app's read before took, whatever
// that read's latency: once the claim is final, the app's next read shows it,
// unless another reader read the account in the meantime. That holds while
// vault2 can be read: when it cannot, the app's reads get the last value read,
// from before the claim, until a read gets through.
const snCtfClaimedTtl = 25 * time.Second

// The most accounts whose claimed is kept, the least recently used dropped
// first. A value is kept past snCtfClaimedTtl as the account's last read, which
// an answer gives when a fresh read of the account fails or does not finish in
// time, so a slow chain does not take a network's won lines away. Tests lower
// it.
var snCtfClaimedCacheLimit = 1 << 18

type snCtfClaimedKey struct {
	deploymentKey model.StDeploymentKey
	vault         common.Address
	poolId        uint64
	index         uint64
}

type snCtfClaimedRead struct {
	key      snCtfClaimedKey
	claimed  uint256.Int
	readTime time.Time
}

var snCtfClaimedCache struct {
	lock sync.Mutex
	// each key's element of recency, whose value is its *snCtfClaimedRead
	reads map[snCtfClaimedKey]*list.Element
	// the most recently used first
	recency *list.List
}

// snCtfClaimedNow is the claimed cache's clock. Tests replace it.
var snCtfClaimedNow = time.Now

func snCtfResetClaimedCache() {
	cache := &snCtfClaimedCache
	cache.lock.Lock()
	defer cache.lock.Unlock()
	cache.reads, cache.recency = nil, nil
}

// snCtfClaimedLast is the last value read of key, nil when none is kept, which
// becomes the most recently used. The caller holds the cache's lock.
func snCtfClaimedLast(key snCtfClaimedKey) *snCtfClaimedRead {
	cache := &snCtfClaimedCache
	element, ok := cache.reads[key]
	if !ok {
		return nil
	}
	cache.recency.MoveToFront(element)
	return element.Value.(*snCtfClaimedRead)
}

// snCtfClaimedKeep keeps a value read of key as the most recently used,
// dropping the least recently used past snCtfClaimedCacheLimit. A key's claimed
// only grows, so of two reads it keeps the larger value and the later time: a
// read that began earlier and finished later, or one at an older finalized
// block, never lowers it. The caller holds the cache's lock.
func snCtfClaimedKeep(key snCtfClaimedKey, claimed uint256.Int, readTime time.Time) {
	cache := &snCtfClaimedCache
	if cache.reads == nil {
		cache.reads, cache.recency = map[snCtfClaimedKey]*list.Element{}, list.New()
	}
	if element, ok := cache.reads[key]; ok {
		read := element.Value.(*snCtfClaimedRead)
		if read.claimed.Lt(&claimed) {
			read.claimed = claimed
		}
		if read.readTime.Before(readTime) {
			read.readTime = readTime
		}
		cache.recency.MoveToFront(element)
		return
	}
	cache.reads[key] = cache.recency.PushFront(&snCtfClaimedRead{key: key, claimed: claimed, readTime: readTime})
	for snCtfClaimedCacheLimit < cache.recency.Len() {
		oldest := cache.recency.Back()
		cache.recency.Remove(oldest)
		delete(cache.reads, oldest.Value.(*snCtfClaimedRead).key)
	}
}

// snCtfClaimedOf is vault2's claimed of each index of the CTF pool, by index.
// A value whose read began less than snCtfClaimedTtl ago is answered as it was
// read. The others are read at one finalized block, which is read once, a
// batch of stMaximumEVMRPCBatchCalls at a time in the indexes' order, and each
// batch is kept as soon as it is read, as of the time this read began. An
// index whose value is not read before ctx ends, or whose read fails, is
// answered with the last value read for it, however old: claimed only grows,
// so that value errs only by reading a claimed win as not claimed a little
// longer. An index this instance has not read, or no longer keeps, is left
// out.
func snCtfClaimedOf(ctx context.Context, cfg *StConfig, poolId uint64, indexes []uint64) map[uint64]uint256.Int {
	claimed := map[uint64]uint256.Int{}
	deploymentKey := cfg.DeploymentKey()
	keyOf := func(index uint64) snCtfClaimedKey {
		return snCtfClaimedKey{deploymentKey: deploymentKey, vault: cfg.VaultV2, poolId: poolId, index: index}
	}
	cache := &snCtfClaimedCache
	unread := []uint64{}
	listed := map[uint64]bool{}
	now := snCtfClaimedNow()
	cache.lock.Lock()
	for _, index := range indexes {
		if listed[index] {
			continue
		}
		listed[index] = true
		if read := snCtfClaimedLast(keyOf(index)); read != nil && now.Sub(read.readTime) < snCtfClaimedTtl {
			claimed[index] = read.claimed
		} else {
			unread = append(unread, index)
		}
	}
	cache.lock.Unlock()
	if len(unread) == 0 {
		return claimed
	}
	snCtfReadClaimed(ctx, poolId, unread, now, keyOf, claimed)
	// what was not read now: the last value read, whatever its age
	stale := 0
	var oldest time.Duration
	cache.lock.Lock()
	for _, index := range unread {
		if _, ok := claimed[index]; ok {
			continue
		}
		if read := snCtfClaimedLast(keyOf(index)); read != nil {
			claimed[index] = read.claimed
			stale += 1
			oldest = max(oldest, now.Sub(read.readTime))
		}
	}
	cache.lock.Unlock()
	if 0 < stale {
		glog.Infof("[sn]ctf claimed of %d accounts of pool %d answered as last read, the oldest read %s ago\n", stale, poolId, oldest.Round(time.Second))
	}
	return claimed
}

// snCtfReadClaimed reads vault2's claimed of the indexes into claimed at one
// finalized block, which it reads once, a batch at a time while ctx lasts,
// keeping each batch as soon as it is read as of readTime. It stops at the
// first read that fails.
func snCtfReadClaimed(ctx context.Context, poolId uint64, indexes []uint64, readTime time.Time, keyOf func(uint64) snCtfClaimedKey, claimed map[uint64]uint256.Int) {
	cache := &snCtfClaimedCache
	client := stLedgerV2Client()
	if client == nil {
		glog.Infof("[sn]ctf claimed of %d accounts of pool %d: %s\n", len(indexes), poolId, errStLedgerV2NotConfigured)
		return
	}
	var block uint64
	if err := stLedgerV2Guard(func() (err error) {
		block, err = client.LedgerV2FinalizedBlock(ctx)
		return
	}); err != nil {
		glog.Infof("[sn]ctf claimed of %d accounts of pool %d: %s\n", len(indexes), poolId, err)
		return
	}
	for start := 0; start < len(indexes); start += stMaximumEVMRPCBatchCalls {
		batch := indexes[start:min(start+stMaximumEVMRPCBatchCalls, len(indexes))]
		var values []uint256.Int
		err := stLedgerV2Guard(func() (err error) {
			values, err = client.LedgerV2ClaimedAt(ctx, poolId, block, batch)
			return
		})
		if err == nil && len(values) != len(batch) {
			err = fmt.Errorf("%d values read for %d accounts", len(values), len(batch))
		}
		if err != nil {
			glog.Infof("[sn]ctf claimed of %d accounts of pool %d at block %d, %d of them read: %s\n", len(indexes), poolId, block, start, err)
			return
		}
		cache.lock.Lock()
		for i, index := range batch {
			claimed[index] = values[i]
			// as of when the read began, never later: a value read slowly
			// expires no later than one read quickly
			snCtfClaimedKeep(keyOf(index), values[i], readTime)
		}
		cache.lock.Unlock()
	}
}

// snCtfAccount is one coldkey's account in the CTF pool as a claim sees it,
// before vault2's claimed is read.
type snCtfAccount struct {
	// The latest finalized root holds the account, at index, with the
	// cumulative latest. Otherwise no finalized root holds an account of the
	// coldkey, and nothing was paid to it to claim.
	held   bool
	index  uint64
	latest uint256.Int
	// the account's cumulative in each finalized root its wins need, by seq
	cumulative map[uint64]uint256.Int
}

// AccountClaims reads the claim state of each coldkey's account in the CTF
// pool at the finalized roots its wins need, while ctx lasts: the accounts
// from the records (accounts), then vault2's claimed of every account a
// finalized root holds (snCtfClaimedOf). A coldkey whose state is not read in
// time, or disagrees with itself (vault2's claimed above the latest finalized
// cumulative, as the ledger route refuses it), is left out, so its wins answer
// claimed null.
func (self *snCtfCoreSource) AccountClaims(ctx context.Context, view *snCtfView, needs []*snCtfClaimNeed) map[[32]byte]*snCtfAccountClaims {
	claims := map[[32]byte]*snCtfAccountClaims{}
	cfg := self.cfg
	poolId, ok := stCtfPoolId(cfg)
	if len(needs) == 0 || !ok || !stCtfPoolOn(cfg) {
		// capture the flag is off: no capture the flag pool is read
		return claims
	}
	var accounts map[[32]byte]*snCtfAccount
	if err := stLedgerV2Guard(func() (err error) {
		accounts, err = self.accounts(ctx, poolId, needs)
		return
	}); err != nil {
		glog.Infof("[sn]ctf claims: %s\n", err)
		return claims
	}
	indexes := []uint64{}
	for _, need := range needs {
		if account := accounts[need.Coldkey]; account != nil && account.held {
			indexes = append(indexes, account.index)
		}
	}
	claimed := snCtfClaimedOf(ctx, cfg, poolId, indexes)
	for _, need := range needs {
		account := accounts[need.Coldkey]
		switch {
		case account == nil:
			// its account could not be read
		case !account.held:
			// nothing was paid to it, so nothing was claimed
			claims[need.Coldkey] = &snCtfAccountClaims{Cumulative: account.cumulative}
		default:
			value, ok := claimed[account.index]
			if !ok {
				continue
			}
			if account.latest.Lt(&value) {
				glog.Infof("[sn]ctf claims of 0x%x: vault2 claimed(%d, %d) is %s, above the finalized cumulative %s\n", need.Coldkey, poolId, account.index, value.Dec(), account.latest.Dec())
				continue
			}
			claims[need.Coldkey] = &snCtfAccountClaims{Claimed: value, Cumulative: account.cumulative}
		}
	}
	return claims
}

// accounts reads each coldkey's account in the CTF pool as a claim sees it,
// from the server's records and the ledger objects, each read once for all of
// the coldkeys. The account's index and its cumulative in the latest finalized
// root are found as the ledger route finds them (stLedgerV2AccountEntry): its
// recorded index, else its position among the accounts the latest finalized
// root appended after the recorded ones. Its cumulative in an earlier root is
// the sum of its recorded deltas through it, or, for a finalized root the
// record has not reached, the root's own published accounts. A coldkey whose
// records disagree with each other is left out.
func (self *snCtfCoreSource) accounts(ctx context.Context, poolId uint64, needs []*snCtfClaimNeed) (map[[32]byte]*snCtfAccount, error) {
	key := self.cfg.DeploymentKey()
	// The recorded accounts are counted before the record of any account is
	// read. An account the record had not reached when it was read is at or
	// after that count, where the search below looks for it, so a root
	// recorded between the reads never hides it.
	count := model.GetStLedgerV2AccountCount(ctx, key, poolId)
	// Below the lowest finalized root not recorded yet, the record of each
	// root's deltas gives the account's cumulative in it. The latest finalized
	// root is read first, so that a root finalized after it is never taken for
	// a recorded one; a root's deltas are recorded with it in one transaction,
	// and they are read after this.
	latest := model.GetLatestStLedgerV2FinalizedRoot(ctx, key, poolId)
	latestIndex, recordedBelow := uint64(0), uint64(0)
	if latest != nil {
		next := model.GetNextUnrecordedStLedgerV2Root(ctx, key, poolId)
		for _, root := range []*model.StLedgerV2Root{latest, next} {
			if root != nil && root.RootIndex == nil {
				return nil, fmt.Errorf("finalized root %d of pool %d has no root index", root.Seq, poolId)
			}
		}
		latestIndex = *latest.RootIndex
		recordedBelow = latestIndex + 1
		if next != nil {
			recordedBelow = min(recordedBelow, *next.RootIndex)
		}
	}
	coldkeys := make([][32]byte, len(needs))
	for i, need := range needs {
		coldkeys[i] = need.Coldkey
	}
	recorded := model.GetStLedgerV2AccountIndexes(ctx, key, poolId, coldkeys)
	deltas := model.GetStLedgerV2PoolAmountsForColdkeys(ctx, key, poolId, coldkeys)
	var latestAccounts []merkle.LedgerAccount
	if latest != nil {
		var err error
		if _, latestAccounts, err = stLedgerV2AccountTree(ctx, latest); err != nil {
			return nil, err
		}
	}

	// the published accounts of each finalized root the record has not
	// reached, read once
	type rootAccounts struct {
		accounts []merkle.LedgerAccount
		err      error
	}
	published := map[uint64]*rootAccounts{}
	publishedOf := func(rootIndex uint64) ([]merkle.LedgerAccount, error) {
		if read, ok := published[rootIndex]; ok {
			return read.accounts, read.err
		}
		read := &rootAccounts{}
		published[rootIndex] = read
		mirrored := model.GetStLedgerV2FinalizedRoot(ctx, key, poolId, rootIndex)
		if mirrored == nil {
			read.err = fmt.Errorf("finalized root %d of pool %d is not mirrored", rootIndex, poolId)
		} else {
			_, read.accounts, read.err = stLedgerV2AccountTree(ctx, mirrored)
		}
		return read.accounts, read.err
	}

	accountOf := func(need *snCtfClaimNeed) (*snCtfAccount, error) {
		coldkeyDeltas := deltas[need.Coldkey]
		index, isRecorded := recorded[need.Coldkey]
		held := false
		if isRecorded {
			if uint64(len(latestAccounts)) <= index || latestAccounts[index].Coldkey != need.Coldkey {
				return nil, fmt.Errorf("the latest finalized root of pool %d does not hold the coldkey at its recorded index %d", poolId, index)
			}
			held = true
		} else {
			index, held = stLedgerV2FindAccount(need.Coldkey, count, latestAccounts)
		}
		account := &snCtfAccount{cumulative: map[uint64]uint256.Int{}}
		if !held {
			// no finalized root holds an account of the coldkey: none of its
			// wins was paid anything to claim
			if 0 < len(coldkeyDeltas) {
				return nil, fmt.Errorf("no finalized root of pool %d holds the account the record has deltas of", poolId)
			}
			for _, root := range need.Roots {
				account.cumulative[root.Seq] = uint256.Int{}
			}
			return account, nil
		}
		account.held, account.index, account.latest = true, index, latestAccounts[index].Cumulative
		recordedThrough := func(rootIndex uint64) uint256.Int {
			var sum uint256.Int
			for _, delta := range coldkeyDeltas {
				if delta.RootIndex <= rootIndex {
					sum.Add(&sum, &delta.Amount)
				}
			}
			return sum
		}
		if sum := recordedThrough(latestIndex); latestIndex < recordedBelow && !sum.Eq(&account.latest) {
			return nil, fmt.Errorf("the recorded deltas of pool %d add up to %s, not the finalized cumulative %s", poolId, sum.Dec(), account.latest.Dec())
		}
		for _, root := range need.Roots {
			if root.RootIndex == nil {
				return nil, fmt.Errorf("finalized root %d of pool %d has no root index", root.Seq, poolId)
			}
			rootIndex := *root.RootIndex
			switch {
			case rootIndex == latestIndex:
				account.cumulative[root.Seq] = account.latest
			case latestIndex < rootIndex:
				return nil, fmt.Errorf("root %d of pool %d is after the latest finalized root %d", rootIndex, poolId, latestIndex)
			case rootIndex < recordedBelow:
				account.cumulative[root.Seq] = recordedThrough(rootIndex)
			default:
				// a finalized root the record has not reached: its own accounts
				rootAccounts, err := publishedOf(rootIndex)
				if err != nil {
					return nil, err
				}
				var cumulative uint256.Int
				if index < uint64(len(rootAccounts)) {
					if rootAccounts[index].Coldkey != need.Coldkey {
						return nil, fmt.Errorf("finalized root %d of pool %d does not hold the coldkey at index %d", rootIndex, poolId, index)
					}
					cumulative = rootAccounts[index].Cumulative
				}
				account.cumulative[root.Seq] = cumulative
			}
		}
		return account, nil
	}

	accounts := map[[32]byte]*snCtfAccount{}
	for _, need := range needs {
		account, err := accountOf(need)
		if err != nil {
			glog.Infof("[sn]ctf claims of 0x%x: %s\n", need.Coldkey, err)
			continue
		}
		accounts[need.Coldkey] = account
	}
	return accounts, nil
}
