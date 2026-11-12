package controller

// sn_ctf_api_source.go — the capture the flag routes' view over the server's
// records of the CTF pool: its roots as the vault2 mirror holds them (no_id
// 2^32 + n), the epoch results, pots, awards and pending awards the driver
// fixed at the first live build covering each epoch, the census, its
// commitment and the draw (model/ctf_model.go, keyed by the operator's n), and
// the epoch clock. Every read raises on a database failure, as the model does;
// the source turns that into an error, which the routes answer as 503.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// snCtfCoreSource reads the records of the configured operator's CTF pool.
type snCtfCoreSource struct {
	cfg *StConfig
}

// newSnCtfCoreSource is the source over the server's records of the CTF pool.
func newSnCtfCoreSource() (snCtfSource, error) {
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled || !cfg.LedgerV2Enabled() || cfg.DeploymentKey() == "" {
		return nil, errSnCtfNotConfigured
	}
	if _, ok := stCtfPoolId(cfg); !ok {
		return nil, errSnCtfNotConfigured
	}
	return &snCtfCoreSource{cfg: cfg}, nil
}

// snCtfCarryOf is the carry contract the answers name: the capture the flag
// driver's (st.yml ctf.carry_address, which live mode requires to equal the
// signed amendment entry's carry). While the driver is off or names none, the
// routes are unavailable. Tests replace it.
var snCtfCarryOf = func(ctx context.Context, cfg *StConfig, epoch uint64) (common.Address, error) {
	if !cfg.Ctf.On() || cfg.Ctf.CarryAddress == (common.Address{}) {
		return common.Address{}, errSnCtfNotConfigured
	}
	return cfg.Ctf.CarryAddress, nil
}

type snCtfPoolRegistration struct {
	registered bool
	firstEpoch uint64
	checked    time.Time
}

var snCtfPoolRegistrations sync.Map

// snCtfFirstEpoch is the CTF pool's first epoch, from vault2.pools(P). A
// registered pool is kept for good; an unregistered answer is read again after
// a minute, and is errSnCtfNotConfigured.
func snCtfFirstEpoch(ctx context.Context, cfg *StConfig, poolId uint64) (uint64, error) {
	key := fmt.Sprintf("%s|%s|%d", cfg.DeploymentKey(), cfg.VaultV2.Hex(), poolId)
	if value, ok := snCtfPoolRegistrations.Load(key); ok {
		pool := value.(snCtfPoolRegistration)
		if pool.registered {
			return pool.firstEpoch, nil
		}
		if time.Since(pool.checked) < time.Minute {
			return 0, errSnCtfNotConfigured
		}
	}
	client := stLedgerV2Client()
	if client == nil {
		return 0, errStLedgerV2NotConfigured
	}
	registered, firstEpoch, err := client.LedgerV2Pool(ctx, poolId)
	if err != nil {
		return 0, err
	}
	snCtfPoolRegistrations.Store(key, snCtfPoolRegistration{registered: registered, firstEpoch: firstEpoch, checked: time.Now()})
	if !registered {
		return 0, errSnCtfNotConfigured
	}
	return firstEpoch, nil
}

// Header times of mined blocks, which do not change.
var snCtfBlockTimes struct {
	lock  sync.Mutex
	times map[uint64]time.Time
}

const snCtfBlockTimesLimit = 4096

func snCtfBlockTime(ctx context.Context, block uint64) (time.Time, error) {
	cache := &snCtfBlockTimes
	cache.lock.Lock()
	if blockTime, ok := cache.times[block]; ok {
		cache.lock.Unlock()
		return blockTime, nil
	}
	cache.lock.Unlock()
	client := stClient()
	if client == nil {
		return time.Time{}, errStLedgerV2NotConfigured
	}
	blockTime, err := client.BlockTime(ctx, block)
	if err != nil {
		return time.Time{}, fmt.Errorf("the time of block %d: %w", block, err)
	}
	cache.lock.Lock()
	defer cache.lock.Unlock()
	if cache.times == nil || snCtfBlockTimesLimit <= len(cache.times) {
		cache.times = map[uint64]time.Time{}
	}
	cache.times[block] = blockTime.UTC()
	return blockTime.UTC(), nil
}

// snCtfClockOf is this epoch, from the synced epoch summary, else the chain.
func snCtfClockOf(ctx context.Context, cfg *StConfig) (*snCtfClock, error) {
	clock := &snCtfClock{}
	if summary := model.GetStEpochSummaryCache(ctx, cfg.DeploymentKey()); summary != nil && 0 < summary.TEpochBlocks {
		clock.Epoch, clock.StartBlock, clock.EndBlock = summary.Epoch, summary.StartBlock, summary.StartBlock+summary.TEpochBlocks-1
	} else {
		client := stClient()
		if client == nil {
			return nil, errStLedgerV2NotConfigured
		}
		state, err := client.Epoch(ctx)
		if err != nil {
			return nil, err
		}
		if state.TEpochBlocks == 0 {
			return nil, errors.New("the epoch clock has no epoch length")
		}
		clock.Epoch, clock.StartBlock, clock.EndBlock = state.Epoch, state.EpochStartBlock, state.EpochStartBlock+state.TEpochBlocks-1
	}
	startTime, err := snCtfBlockTime(ctx, clock.StartBlock)
	if err != nil {
		return nil, err
	}
	clock.StartTime = startTime
	return clock, nil
}

// snCtfRootsOf are the pool's committed roots, pending and finalized, by epoch.
func snCtfRootsOf(ctx context.Context, cfg *StConfig, poolId uint64) ([]*snCtfRoot, error) {
	roots := []*snCtfRoot{}
	for _, mirrored := range model.GetStLedgerV2RootsFromEpoch(ctx, cfg.DeploymentKey(), poolId, 0) {
		finalized := mirrored.Status == model.StLedgerV2RootFinalized
		if !finalized && mirrored.Status != model.StLedgerV2RootPending {
			continue
		}
		root := &snCtfRoot{Seq: mirrored.Seq, Finalized: finalized, RootIndex: mirrored.RootIndex, Epoch: mirrored.Epoch, LedgerHash: mirrored.LedgerHash}
		// a root whose commitment the mirror missed is known by its finalization
		commitBlock := mirrored.StatusBlock
		if mirrored.CommitBlock != nil {
			commitBlock = mirrored.CommitBlock
		}
		if commitBlock == nil {
			return nil, fmt.Errorf("root %d of pool %d has no block", mirrored.Seq, poolId)
		}
		root.CommitBlock = *commitBlock
		commitTime, err := snCtfBlockTime(ctx, root.CommitBlock)
		if err != nil {
			return nil, err
		}
		root.CommitTime = commitTime
		if finalized && mirrored.StatusBlock != nil {
			root.FinalizedBlock = mirrored.StatusBlock
			if root.FinalizedTime, err = snCtfBlockTime(ctx, *mirrored.StatusBlock); err != nil {
				return nil, err
			}
		}
		roots = append(roots, root)
	}
	slices.SortStableFunc(roots, func(a, b *snCtfRoot) int {
		return cmp.Or(cmp.Compare(a.Epoch, b.Epoch), cmp.Compare(a.Seq, b.Seq))
	})
	return roots, nil
}

// The covered epochs of finalized roots, by deployment, epoch and root seq:
// their results, pots, awards and census are written once.
var snCtfFinalizedEpochs sync.Map

// The root that covers an epoch: the first committed root, by epoch, whose
// epochs include it.
func snCtfCoveringRoot(roots []*snCtfRoot, epoch uint64) *snCtfRoot {
	for _, root := range roots {
		if epoch <= root.Epoch {
			return root
		}
	}
	return nil
}

func snCtfRoleOf(role int) (int, bool) {
	return role, role == snCtfRoleProvider || role == snCtfRoleExtender
}

func (self *snCtfCoreSource) View(ctx context.Context) (view *snCtfView, err error) {
	err = stLedgerV2Guard(func() (err error) {
		view, err = self.view(ctx)
		return
	})
	return
}

func (self *snCtfCoreSource) view(ctx context.Context) (*snCtfView, error) {
	cfg := self.cfg
	key := cfg.DeploymentKey()
	poolId, _ := stCtfPoolId(cfg)
	firstEpoch, err := snCtfFirstEpoch(ctx, cfg, poolId)
	if err != nil {
		return nil, err
	}
	clock, err := snCtfClockOf(ctx, cfg)
	if err != nil {
		return nil, err
	}
	carry, err := snCtfCarryOf(ctx, cfg, clock.Epoch)
	if err != nil {
		return nil, err
	}
	roots, err := snCtfRootsOf(ctx, cfg, poolId)
	if err != nil {
		return nil, err
	}
	view := &snCtfView{
		Pool:  snCtfPoolFacts{NoId: cfg.NoId, PoolId: poolId, Vault: cfg.VaultV2, CarryContract: carry, FirstEpoch: firstEpoch},
		Clock: *clock,
		Roots: roots,
		AsOf:  clock.StartTime,
	}
	for _, root := range roots {
		if view.AsOf.Before(root.changeTime()) {
			view.AsOf = root.changeTime()
		}
	}
	lastRootEpoch := uint64(0)
	if 0 < len(roots) {
		lastRootEpoch = roots[len(roots)-1].Epoch
	}
	if 0 < len(roots) && firstEpoch <= lastRootEpoch {
		results := model.GetCtfEpochResults(ctx, key, cfg.NoId, firstEpoch, lastRootEpoch)
		byEpoch := map[uint64]*model.CtfEpochResult{}
		for _, result := range results {
			byEpoch[result.Epoch] = result
		}
		for epoch := firstEpoch; epoch <= lastRootEpoch; epoch += 1 {
			root := snCtfCoveringRoot(roots, epoch)
			if root == nil {
				return nil, fmt.Errorf("no committed root covers epoch %d", epoch)
			}
			// an epoch a finalized root covers never changes
			cacheKey := fmt.Sprintf("%s|%d|%d", key, epoch, root.Seq)
			if cached, ok := snCtfFinalizedEpochs.Load(cacheKey); ok && root.Finalized {
				view.Epochs = append(view.Epochs, cached.(*snCtfEpoch))
				continue
			}
			result := byEpoch[epoch]
			if result == nil {
				return nil, fmt.Errorf("epoch %d, which a committed root covers, has no recorded result", epoch)
			}
			covered, err := self.epochOf(ctx, clock, root, result)
			if err != nil {
				return nil, err
			}
			if root.Finalized {
				snCtfFinalizedEpochs.Store(cacheKey, covered)
			}
			view.Epochs = append(view.Epochs, covered)
		}
	}
	// the ended epoch no committed root covers yet
	if 0 < clock.Epoch {
		ended := clock.Epoch - 1
		if firstEpoch <= ended && (len(roots) == 0 || lastRootEpoch < ended) {
			view.Pending = &snCtfPendingDraw{Epoch: ended}
			commitment := model.GetCtfCensusCommitment(ctx, key, cfg.NoId, ended)
			if commitment != nil && commitment.Status == model.CtfCommitmentStatusCommitted && commitment.CommitBlock != nil && commitment.Round != nil {
				census := &snCtfCensusCommitment{Hash: commitment.CensusHash, Block: *commitment.CommitBlock, Round: *commitment.Round}
				if commitment.CommitBlockTime != nil {
					census.Time = time.Unix(int64(*commitment.CommitBlockTime), 0).UTC()
				} else if census.Time, err = snCtfBlockTime(ctx, census.Block); err != nil {
					return nil, err
				}
				view.Pending.Census = census
				if view.AsOf.Before(census.Time) {
					view.AsOf = census.Time
				}
			}
		}
	}
	return view, nil
}

// One covered epoch from its fixed result, its 498 pots, its awards and its
// census's near misses.
func (self *snCtfCoreSource) epochOf(ctx context.Context, clock *snCtfClock, root *snCtfRoot, result *model.CtfEpochResult) (*snCtfEpoch, error) {
	cfg := self.cfg
	key := cfg.DeploymentKey()
	epoch := result.Epoch
	if root == nil {
		return nil, fmt.Errorf("no committed root covers epoch %d", epoch)
	}
	covered := &snCtfEpoch{
		Epoch:        epoch,
		Root:         root,
		Drawn:        result.Drawn,
		Funding:      result.Funding,
		Returned:     result.Returned,
		Owed:         result.Owed,
		Matured:      result.Matured,
		Fresh:        result.Fresh,
		SlotBase:     result.SlotBase,
		PaidSlots:    result.PaidSlots,
		PaidPots:     result.PaidPots,
		PaidPending:  result.PaidPending,
		PendingTotal: result.PendingTotal,
		Carried:      result.Carried,
		AwardsRef:    result.AwardsRef,
		PotsRef:      result.PotsRef,
		PendingRef:   result.PendingRef,
		Pots:         make([]*snCtfPot, 2*snCtfCountryCount),
	}
	if result.CensusHash != nil {
		census := &snCtfCensusCommitment{Hash: *result.CensusHash, Round: result.Round}
		if result.CensusBlock != nil {
			census.Block = *result.CensusBlock
		}
		covered.Census = census
	}
	// the epoch's blocks: its census's, else the clock's length from its start
	if census := model.GetCtfCensus(ctx, key, cfg.NoId, epoch); census != nil {
		covered.StartBlock, covered.EndBlock = census.StartBlock, census.EndBlock
		// near misses only from the census the chain holds, and only with a draw
		if covered.Census != nil && census.CensusHash == covered.Census.Hash && result.Drawn {
			for _, censusPot := range model.GetCtfCensusPots(ctx, key, cfg.NoId, epoch) {
				index, ok := snCtfCountryIndex()[censusPot.CountryCode]
				role, valid := snCtfRoleOf(censusPot.Role)
				if !ok || !valid {
					continue
				}
				pot := covered.potAt(snCtfPotIndex(index, role))
				pot.NearMisses = int64(censusPot.NearMisses)
			}
		}
	} else if row := model.GetStEpoch(ctx, key, epoch); row != nil {
		covered.StartBlock = row.StartBlock
		covered.EndBlock = row.StartBlock + clock.EndBlock - clock.StartBlock
	}
	for _, potRow := range model.GetCtfPots(ctx, key, cfg.NoId, epoch) {
		index, ok := snCtfCountryIndex()[potRow.CountryCode]
		role, valid := snCtfRoleOf(potRow.Role)
		if !ok || !valid {
			return nil, fmt.Errorf("epoch %d has a pot row of %q role %d", epoch, potRow.CountryCode, potRow.Role)
		}
		pot := covered.potAt(snCtfPotIndex(index, role))
		pot.Units, pot.Winners, pot.In, pot.Out = uint32(potRow.Units), potRow.Winners, potRow.In, potRow.Out
	}
	for _, awardRow := range model.GetCtfAwards(ctx, key, cfg.NoId, epoch) {
		award := &snCtfAward{
			Role:       awardRow.Role,
			Country:    awardRow.CountryCode,
			Rank:       awardRow.Rank,
			Unit:       awardRow.Unit,
			UnitFamily: awardRow.UnitFamily,
			ClientId:   awardRow.ClientId,
			Coldkey:    awardRow.Coldkey,
			Slot:       awardRow.Slot,
			Pot:        awardRow.Pot,
			Deferred:   awardRow.Deferred,
		}
		if award.UnitFamily != 4 && award.UnitFamily != 6 {
			// the census member's unit's family
			for _, member := range model.GetCtfCensusClientMembers(ctx, key, cfg.NoId, award.ClientId, epoch, epoch) {
				if member.Role == award.Role && member.Epoch == epoch {
					award.UnitFamily = member.UnitFamily
				}
			}
		}
		covered.Awards = append(covered.Awards, award)
	}
	return covered, nil
}

func (self *snCtfEpoch) potAt(index int) *snCtfPot {
	if self.Pots[index] == nil {
		self.Pots[index] = &snCtfPot{}
	}
	return self.Pots[index]
}

func (self *snCtfCoreSource) EpochDetail(ctx context.Context, view *snCtfView, epoch *snCtfEpoch) (detail *snCtfEpochDetail, err error) {
	err = stLedgerV2Guard(func() error {
		cfg := self.cfg
		key := cfg.DeploymentKey()
		detail = &snCtfEpochDetail{}
		if census := model.GetCtfCensus(ctx, key, cfg.NoId, epoch.Epoch); census != nil && epoch.Census != nil && census.CensusHash == epoch.Census.Hash {
			detail.Candidates = census.Candidates
			detail.ExcusedSamples = len(census.ExcusedSamples)
			for _, exclusion := range model.GetCtfCensusExclusions(ctx, key, cfg.NoId, epoch.Epoch) {
				detail.Exclusions = append(detail.Exclusions, &snCtfExclusion{Role: exclusion.Role, Reason: exclusion.Reason, Count: int64(exclusion.Count)})
			}
		}
		if draw := model.GetCtfDraw(ctx, key, cfg.NoId, epoch.Epoch); draw != nil {
			detail.Draw = &snCtfDraw{Round: draw.Round, Signature: draw.Signature, Randomness: draw.Randomness, Seed: draw.Seed}
		}
		for _, pending := range model.GetCtfPendingAwards(ctx, key, cfg.NoId, epoch.Epoch) {
			detail.Pending = append(detail.Pending, &snCtfPendingAward{AwardEpoch: pending.AwardEpoch, Role: pending.Role, Country: pending.CountryCode, Rank: pending.Rank, Coldkey: pending.Coldkey, Amount: pending.Amount})
		}
		return nil
	})
	return
}

func snCtfMemberOf(member *model.CtfCensusMember) *snCtfCensusMember {
	return &snCtfCensusMember{
		Role:           member.Role,
		ClientId:       member.ClientId,
		Candidate:      member.Candidate,
		Failed:         member.Failed,
		Country:        member.CountryCode,
		DrawCountry:    member.DrawCountryCode,
		PresentSamples: member.PresentSamples,
		UnitFamily:     member.UnitFamily,
	}
}

func (self *snCtfCoreSource) ClientEpochs(ctx context.Context, view *snCtfView, clientId server.Id, epochs []*snCtfEpoch) (facts map[uint64]*snCtfClientEpochFacts, err error) {
	facts = map[uint64]*snCtfClientEpochFacts{}
	if len(epochs) == 0 {
		return
	}
	err = stLedgerV2Guard(func() error {
		cfg := self.cfg
		key := cfg.DeploymentKey()
		from, to := epochs[len(epochs)-1].Epoch, epochs[0].Epoch
		for _, epoch := range epochs {
			from, to = min(from, epoch.Epoch), max(to, epoch.Epoch)
			facts[epoch.Epoch] = &snCtfClientEpochFacts{Members: map[int]*snCtfCensusMember{}, Awards: map[int]*snCtfAward{}}
		}
		for _, census := range model.GetCtfCensuses(ctx, key, cfg.NoId, from, to) {
			if epochFacts := facts[census.Epoch]; epochFacts != nil {
				epochFacts.RequiredSamples, epochFacts.ExcusedSamples = census.RequiredSamples, len(census.ExcusedSamples)
			}
		}
		for _, member := range model.GetCtfCensusClientMembers(ctx, key, cfg.NoId, clientId, from, to) {
			if epochFacts := facts[member.Epoch]; epochFacts != nil {
				epochFacts.Members[member.Role] = snCtfMemberOf(member)
			}
		}
		for _, award := range model.GetCtfClientAwards(ctx, key, cfg.NoId, clientId, from) {
			if epochFacts := facts[award.Epoch]; epochFacts != nil && award.Epoch <= to {
				epochFacts.Awards[award.Role] = &snCtfAward{Role: award.Role, Country: award.CountryCode, Rank: award.Rank, Unit: award.Unit, UnitFamily: award.UnitFamily, ClientId: award.ClientId, Coldkey: award.Coldkey, Slot: award.Slot, Pot: award.Pot, Deferred: award.Deferred}
			}
		}
		return nil
	})
	return
}

func snCtfAwardOf(award *model.CtfAward) *snCtfAward {
	return &snCtfAward{Role: award.Role, Country: award.CountryCode, Rank: award.Rank, Unit: award.Unit, UnitFamily: award.UnitFamily, ClientId: award.ClientId, Coldkey: award.Coldkey, Slot: award.Slot, Pot: award.Pot, Deferred: award.Deferred}
}

func (self *snCtfCoreSource) Wins(ctx context.Context, view *snCtfView, scope snCtfClientScope) (wins []*snCtfWin, err error) {
	wins = []*snCtfWin{}
	last := view.lastEpoch()
	if last == nil {
		return
	}
	first := view.Epochs[0].Epoch
	err = stLedgerV2Guard(func() error {
		cfg := self.cfg
		key := cfg.DeploymentKey()
		var awards []*model.CtfAward
		if scope.ClientId != nil {
			awards = model.GetCtfClientAwards(ctx, key, cfg.NoId, *scope.ClientId, first)
		} else {
			awards = model.GetCtfNetworkAwards(ctx, key, cfg.NoId, scope.NetworkId, first, last.Epoch)
		}
		for _, award := range awards {
			// the covered epochs only: a committed root holds the award
			if first <= award.Epoch && award.Epoch <= last.Epoch {
				wins = append(wins, &snCtfWin{Epoch: award.Epoch, Award: snCtfAwardOf(award)})
			}
		}
		return nil
	})
	return
}

// How many coldkeys' claim states are read at once.
const snCtfClaimReadsAtOnce = 8

func (self *snCtfCoreSource) AccountClaims(ctx context.Context, view *snCtfView, needs []*snCtfClaimNeed) map[[32]byte]*snCtfAccountClaims {
	claims := map[[32]byte]*snCtfAccountClaims{}
	cfg := self.cfg
	poolId, ok := stCtfPoolId(cfg)
	if len(needs) == 0 || !ok || !stCtfPoolOn(cfg) {
		// capture the flag is off: no capture the flag pool is read
		return claims
	}
	// below the lowest finalized root not recorded yet, the record of each
	// root's deltas gives the account's cumulative in it. The latest finalized
	// root is read first, so that a root finalized after it is never taken for
	// a recorded one; a root's deltas are recorded with it in one transaction,
	// and they are read after this.
	recordedBelow := uint64(0)
	if err := stLedgerV2Guard(func() error {
		latest := model.GetLatestStLedgerV2FinalizedRoot(ctx, cfg.DeploymentKey(), poolId)
		if latest == nil {
			return nil
		}
		next := model.GetNextUnrecordedStLedgerV2Root(ctx, cfg.DeploymentKey(), poolId)
		for _, root := range []*model.StLedgerV2Root{latest, next} {
			if root != nil && root.RootIndex == nil {
				return fmt.Errorf("finalized root %d of pool %d has no root index", root.Seq, poolId)
			}
		}
		recordedBelow = *latest.RootIndex + 1
		if next != nil {
			recordedBelow = min(recordedBelow, *next.RootIndex)
		}
		return nil
	}); err != nil {
		glog.Infof("[sn]ctf claims: %s\n", err)
		return claims
	}
	var lock sync.Mutex
	var reads sync.WaitGroup
	slots := make(chan struct{}, snCtfClaimReadsAtOnce)
	for _, need := range needs {
		reads.Add(1)
		slots <- struct{}{}
		go server.HandleError(func() {
			defer func() {
				<-slots
				reads.Done()
			}()
			var accountClaims *snCtfAccountClaims
			err := stLedgerV2Guard(func() (err error) {
				accountClaims, err = self.accountClaims(ctx, poolId, recordedBelow, need)
				return
			})
			if err != nil {
				glog.Infof("[sn]ctf claims of 0x%x: %s\n", need.Coldkey, err)
				return
			}
			lock.Lock()
			defer lock.Unlock()
			claims[need.Coldkey] = accountClaims
		})
	}
	reads.Wait()
	return claims
}

// accountClaims reads one coldkey's account in the CTF pool as a claim sees it.
// vault2's claimed and the latest finalized root's cumulative are the ledger
// route's own read (stLedgerV2AccountEntry). An earlier root's cumulative is
// the sum of the account's recorded deltas through it, or, for a finalized
// root not recorded yet, the root's own published accounts.
func (self *snCtfCoreSource) accountClaims(ctx context.Context, poolId uint64, recordedBelow uint64, need *snCtfClaimNeed) (*snCtfAccountClaims, error) {
	cfg := self.cfg
	key := cfg.DeploymentKey()
	index, recorded := uint64(0), false
	for _, account := range model.GetStLedgerV2AccountsForColdkey(ctx, key, need.Coldkey) {
		if account.NoId == poolId {
			index, recorded = account.Index, true
		}
	}
	deltas := []*model.StLedgerV2Amount{}
	for _, amount := range model.GetStLedgerV2AmountsForColdkey(ctx, key, need.Coldkey) {
		if amount.NoId == poolId {
			deltas = append(deltas, amount)
		}
	}
	recordedThrough := func(rootIndex uint64) uint256.Int {
		var sum uint256.Int
		for _, delta := range deltas {
			if delta.RootIndex <= rootIndex {
				sum.Add(&sum, &delta.Amount)
			}
		}
		return sum
	}
	entry, err := stLedgerV2AccountEntry(ctx, cfg, poolId, need.Coldkey, index, recorded)
	if err != nil {
		return nil, err
	}
	claims := &snCtfAccountClaims{Cumulative: map[uint64]uint256.Int{}}
	if entry == nil || entry.RootIndex == nil {
		// no finalized root holds an account of the coldkey: none of its
		// wins was paid anything to claim
		if 0 < len(deltas) {
			return nil, fmt.Errorf("no finalized root of pool %d holds the account the record has deltas of", poolId)
		}
		for _, root := range need.Roots {
			claims.Cumulative[root.Seq] = uint256.Int{}
		}
		return claims, nil
	}
	latestIndex := *entry.RootIndex
	var latest uint256.Int
	if err := latest.SetFromDecimal(entry.Cumulative); err != nil {
		return nil, err
	}
	if err := claims.Claimed.SetFromDecimal(entry.Claimed); err != nil {
		return nil, err
	}
	if sum := recordedThrough(latestIndex); latestIndex < recordedBelow && !sum.Eq(&latest) {
		return nil, fmt.Errorf("the recorded deltas of pool %d add up to %s, not the finalized cumulative %s", poolId, sum.Dec(), latest.Dec())
	}
	for _, root := range need.Roots {
		if root.RootIndex == nil {
			return nil, fmt.Errorf("finalized root %d of pool %d has no root index", root.Seq, poolId)
		}
		rootIndex := *root.RootIndex
		switch {
		case rootIndex == latestIndex:
			claims.Cumulative[root.Seq] = latest
		case latestIndex < rootIndex:
			return nil, fmt.Errorf("root %d of pool %d is after the latest finalized root %d", rootIndex, poolId, latestIndex)
		case rootIndex < recordedBelow:
			claims.Cumulative[root.Seq] = recordedThrough(rootIndex)
		default:
			// a finalized root the record has not reached: its own accounts
			mirrored := model.GetStLedgerV2FinalizedRoot(ctx, key, poolId, rootIndex)
			if mirrored == nil {
				return nil, fmt.Errorf("finalized root %d of pool %d is not mirrored", rootIndex, poolId)
			}
			_, accounts, err := stLedgerV2AccountTree(ctx, mirrored)
			if err != nil {
				return nil, err
			}
			var cumulative uint256.Int
			if entry.Index < uint64(len(accounts)) {
				if accounts[entry.Index].Coldkey != need.Coldkey {
					return nil, fmt.Errorf("finalized root %d of pool %d does not hold the coldkey at index %d", rootIndex, poolId, entry.Index)
				}
				cumulative = accounts[entry.Index].Cumulative
			}
			claims.Cumulative[root.Seq] = cumulative
		}
	}
	return claims, nil
}
