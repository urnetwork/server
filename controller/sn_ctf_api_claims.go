package controller

// sn_ctf_api_claims.go — the claim states GET /sn/ctf/client judges its wins by
// (sn_ctf_api_wins.go): each winning coldkey's account in the CTF pool, its
// cumulative in each finalized root that pays one of its wins, and vault2's
// claimed. The answer gives them snCtfClaimsTimeout, about five seconds;
// whatever is not read by then answers claimed null, so a claim state the
// chain cannot give in time never fails the answer, nor holds it past that.
//
//   - The accounts come from the server's records and the ledger objects, each
//     read once for all of the answer's coldkeys: the count of recorded
//     accounts, the latest finalized root and its accounts, the account record
//     and the record of each root's deltas.
//   - vault2's claimed is read at one finalized block per answer, read once,
//     in eth_call batches of stMaximumEVMRPCBatchCalls, the coldkeys of the
//     newest wins first. Each value is kept for snCtfClaimedTtl, so an answer
//     reads only the accounts no answer has read lately, and every coldkey is
//     read however many a network has.
//   - Every read runs on the request's own goroutine: nothing it starts
//     outlives the request.

import (
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
// unless another reader read the account in the meantime.
const snCtfClaimedTtl = 25 * time.Second

// The most claimed values kept. Past it the expired ones are dropped, and all
// of them when that is not enough.
const snCtfClaimedCacheLimit = 1 << 16

type snCtfClaimedKey struct {
	deploymentKey model.StDeploymentKey
	vault         common.Address
	poolId        uint64
	index         uint64
}

type snCtfClaimedRead struct {
	claimed  uint256.Int
	readTime time.Time
}

var snCtfClaimedCache struct {
	lock  sync.Mutex
	reads map[snCtfClaimedKey]snCtfClaimedRead
}

// snCtfClaimedNow is the claimed cache's clock. Tests replace it.
var snCtfClaimedNow = time.Now

func snCtfResetClaimedCache() {
	cache := &snCtfClaimedCache
	cache.lock.Lock()
	defer cache.lock.Unlock()
	cache.reads = nil
}

// snCtfClaimedOf is vault2's claimed of each index of the CTF pool, by index.
// A value whose read began less than snCtfClaimedTtl ago is answered as it was
// read. The others are read at one finalized block, which is read once, a
// batch of stMaximumEVMRPCBatchCalls at a time in the indexes' order, and each
// batch is kept as soon as it is read, as of the time this read began. An
// index whose value is not read before ctx ends, or whose batch the chain
// refuses, is left out.
func snCtfClaimedOf(ctx context.Context, cfg *StConfig, poolId uint64, indexes []uint64) map[uint64]uint256.Int {
	claimed := map[uint64]uint256.Int{}
	keyOf := func(index uint64) snCtfClaimedKey {
		return snCtfClaimedKey{deploymentKey: cfg.DeploymentKey(), vault: cfg.VaultV2, poolId: poolId, index: index}
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
		if read, ok := cache.reads[keyOf(index)]; ok && now.Sub(read.readTime) < snCtfClaimedTtl {
			claimed[index] = read.claimed
		} else {
			unread = append(unread, index)
		}
	}
	cache.lock.Unlock()
	if len(unread) == 0 {
		return claimed
	}

	client := stLedgerV2Client()
	if client == nil {
		glog.Infof("[sn]ctf claimed of %d accounts of pool %d: %s\n", len(unread), poolId, errStLedgerV2NotConfigured)
		return claimed
	}
	var block uint64
	if err := stLedgerV2Guard(func() (err error) {
		block, err = client.LedgerV2FinalizedBlock(ctx)
		return
	}); err != nil {
		glog.Infof("[sn]ctf claimed of %d accounts of pool %d: %s\n", len(unread), poolId, err)
		return claimed
	}
	for start := 0; start < len(unread); start += stMaximumEVMRPCBatchCalls {
		batch := unread[start:min(start+stMaximumEVMRPCBatchCalls, len(unread))]
		var values []uint256.Int
		err := stLedgerV2Guard(func() (err error) {
			values, err = client.LedgerV2ClaimedAt(ctx, poolId, block, batch)
			return
		})
		if err == nil && len(values) != len(batch) {
			err = fmt.Errorf("%d values read for %d accounts", len(values), len(batch))
		}
		if err != nil {
			glog.Infof("[sn]ctf claimed of %d accounts of pool %d at block %d, %d of them read: %s\n", len(unread), poolId, block, start, err)
			break
		}
		cache.lock.Lock()
		if cache.reads == nil {
			cache.reads = map[snCtfClaimedKey]snCtfClaimedRead{}
		}
		if snCtfClaimedCacheLimit < len(cache.reads)+len(batch) {
			for key, read := range cache.reads {
				if snCtfClaimedTtl <= now.Sub(read.readTime) {
					delete(cache.reads, key)
				}
			}
			if snCtfClaimedCacheLimit < len(cache.reads)+len(batch) {
				cache.reads = map[snCtfClaimedKey]snCtfClaimedRead{}
			}
		}
		for i, index := range batch {
			claimed[index] = values[i]
			// as of when the read began, never later: a value read slowly
			// expires no later than one read quickly
			cache.reads[keyOf(index)] = snCtfClaimedRead{claimed: values[i], readTime: now}
		}
		cache.lock.Unlock()
	}
	return claimed
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
