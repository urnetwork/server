// The phase C v1 carry sweep (VERSION2-FINAL.md §§12.2, 16; owner decision
// 2026-10-31). One sanctioned v1 epoch at or after the pool's first v2 epoch
// carries it: its v1 close captures the reveal-lag stake, the root signer
// commits a single-leaf root paying vault2's coldkey 10,000 bps, the vault's
// finalization folds all v1 carry into that entitlement, and the leaf is
// claimed and absorbed into vault2 as surplus for the pool's next epoch.
//
// The sweep shares its epoch boundary with the pool's own v2 close and commit,
// which must never wait on it:
//   - the time-bound v1 close is sent by the deposit signer, an account with
//     its own nonce sequence that no v2 send uses;
//   - every root-key send waits until the latest v2 commit window is closed
//     or holds its root, so the sweep never queues a root-key transaction
//     ahead of a v2 close or commit;
//   - the ordinary v1 driver never runs the sweep epoch, so no second epoch
//     census competes with the v2 census at the boundary.
//
// The chain is the only state: each turn reads one finalized snapshot,
// decides the single next action, and sends it through the operator
// transaction engine, so a repeated or late turn does only what is still due.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The sweep record's schema; artifactHash is the sha256 of its bytes.
const StV1SweepRecordSchema = "urnetwork/st-v1-sweep-record/v1"

// The single leaf's share: the whole entitlement.
const stV1SweepShareBps = 10_000

// vault1 entitlement states (STSettlementVault.EpochStatus).
const (
	stV1SweepUnset      uint8 = 0
	stV1SweepFunded     uint8 = 1
	stV1SweepFinalized  uint8 = 2
	stV1SweepRootMissed uint8 = 3
	stV1SweepCarried    uint8 = 4
)

const (
	// A close is sent only while the block after the latest leaves this many
	// blocks of the close grace: its eth_estimateGas runs at the latest block
	// and the transaction executes later.
	stV1SweepCloseMarginBlocks = 10
	// A commit leaves this much of the root window for inclusion and fee
	// replacement; past it the epoch's sweep is abandoned.
	stV1SweepCommitMarginBlocks = 100
	// Root-key sends stop this many blocks before a boundary, so the pool's v2
	// close finds the root key idle.
	stV1SweepBoundaryQuietBlocks = 25
	// Surplus below this is not assigned.
	stV1SweepMinimumAssignRao = 1_000_000
	// Polls while waiting on another actor.
	stV1SweepPollDelay = time.Minute
)

// Publish kinds of the sweep's own steps (st_publish.kind).
const (
	StPublishKindV1SweepClose    = "v1sweep_close"
	StPublishKindV1SweepDefer    = "v1sweep_defer"
	StPublishKindV1SweepRecord   = "v1sweep_record"
	StPublishKindV1SweepCommit   = "v1sweep_commit"
	StPublishKindV1SweepFinalize = "v1sweep_finalize"
	StPublishKindV1SweepClaim    = "v1sweep_claim"
	StPublishKindV1SweepWithdraw = "v1sweep_withdraw"
	StPublishKindV1SweepAbsorb   = "v1sweep_absorb"
	StPublishKindV1SweepAssign   = "v1sweep_assign"
	StPublishKindV1SweepRefused  = "v1sweep_refused"
	StPublishKindV1SweepAbandon  = "v1sweep_abandoned"
)

var (
	errStV1SweepNotConfigured = errors.New("st: no v1 sweep epoch is configured")
	errStV1SweepClientMissing = errors.New("st: the v1 sweep client is unavailable")
)

// Selects the sweep epoch and recovery epochs of an enabled st.yml profile.
func stV1SweepConfig(cfg *StConfig, epoch uint64, recovery []uint64) error {
	if epoch == 0 {
		if len(recovery) != 0 {
			return errors.New("v1_sweep_recovery_epochs needs v1_sweep_epoch")
		}
		return nil
	}
	if !cfg.LedgerV2Enabled() {
		return errors.New("v1_sweep_epoch needs settlement_v2_address and vault_v2_address")
	}
	for i, source := range recovery {
		if epoch <= source || (0 < i && source <= recovery[i-1]) {
			return fmt.Errorf("v1_sweep_recovery_epochs %v must ascend strictly below the sweep epoch %d", recovery, epoch)
		}
	}
	cfg.V1SweepEpoch = epoch
	cfg.V1SweepRecoveryEpochs = append([]uint64(nil), recovery...)
	return nil
}

// stV1SweepAdmission admits the sweep chain for epoch: the configured sweep
// epoch, with settlement v2 configured and the pool's activation recorded at
// or before it. It returns the recorded activation. This is the only v1
// admission of an epoch at or after the first v2 epoch.
func stV1SweepAdmission(ctx context.Context, cfg *StConfig, epoch uint64) (*model.StLedgerV2Activation, error) {
	sweepEpoch, sweep := stLedgerV1SweepEpoch(ctx, cfg)
	if !sweep {
		return nil, errStV1SweepNotConfigured
	}
	if epoch != sweepEpoch {
		return nil, fmt.Errorf("%w: epoch %d is not the sweep epoch %d", ErrStV1EpochSettledByV2, epoch, sweepEpoch)
	}
	if !cfg.LedgerV2Enabled() {
		return nil, errStLedgerV2NotConfigured
	}
	activation := stLedgerV2RecordedActivation(ctx, cfg)
	if activation == nil {
		return nil, errors.New("st: the sweep waits for the pool's recorded v2 activation")
	}
	if err := stLedgerV2ActivationRefusal(cfg, activation); err != nil {
		return nil, err
	}
	if activation.NoId != cfg.NoId {
		return nil, fmt.Errorf("st: the recorded v2 activation names pool %d, not %d", activation.NoId, cfg.NoId)
	}
	if epoch < activation.FirstEpoch {
		return nil, fmt.Errorf("st: the sweep epoch %d precedes the pool's first v2 epoch %d; v1 rules settle it", epoch, activation.FirstEpoch)
	}
	return activation, nil
}

// StV1SweepEntitlement is one v1 epoch's vault entitlement and coordinator
// root commitment for the pool.
type StV1SweepEntitlement struct {
	Epoch        uint64
	End          uint64
	Status       uint8
	Funded       *big.Int
	Total        *big.Int
	Claimed      *big.Int
	PayoutRoot   [32]byte
	ArtifactHash [32]byte
	ExpiryBlock  uint64
	// Root is rootCommitments(epoch, noId); zero when uncommitted.
	Root         [32]byte
	RootArtifact [32]byte
	RootBlock    uint64
}

// StV1SweepChain is every fact the sweep's next action depends on, read at
// one finalized block (v2 facts from the same finalized head read).
type StV1SweepChain struct {
	HeadBlock uint64
	HeadHash  [32]byte
	HeadTime  time.Time
	Paused    bool
	// Current is the coordinator's currentEpoch() at the head.
	Current uint64
	// The sweep epoch's policy windows.
	End, CloseGrace, RootWindow, FinalizeOffset uint64
	// RecentEnd is end(Current−1), the latest boundary, and NextEnd is
	// end(Current), the next one; the root-key quiet rule watches both.
	RecentEnd, NextEnd uint64
	Sweep              StV1SweepEntitlement
	Carry              *big.Int
	// Prior holds every epoch below the sweep epoch, ascending.
	Prior []StV1SweepEntitlement
	// PostCutover holds every epoch in [first v2 epoch, Current) other than
	// the sweep epoch, ascending.
	PostCutover    []StV1SweepEntitlement
	Vault2Coldkey  [32]byte
	V1EscrowHotkey [32]byte
	// LeafClaimed is vault1.leafClaimed(sweep, keccak(noId, vault2 coldkey)).
	LeafClaimed bool
	// ClaimCredit is vault1.claimCredit(vault2 coldkey).
	ClaimCredit *big.Int
	// Absorbable is vault2's coldkey's stake on the v1 escrow hotkey.
	Absorbable   *big.Int
	V2LiveEscrow *big.Int
	V2Accounted  *big.Int
	// V2RootThrough is the latest epoch a v2 root (pending or finalized)
	// covers, and V2RootPresent whether there is one.
	V2RootThrough uint64
	V2RootPresent bool
	// V2NextEpoch is vault2.lastFundedEpoch(noId) + 1, the epoch whose
	// funding snapshot an assign enters.
	V2NextEpoch uint64
	// AssignSelector reports that STSettlement2's implementation is the phase
	// C upgrade, whose owner-only assignSurplus(noId, epoch, amount) the
	// governance Safe calls.
	AssignSelector bool
	// RecordedArtifact is the upgrade's v1SweepArtifactHash(noId, epoch):
	// zero until the Safe records the committed sweep root for readers.
	RecordedArtifact [32]byte
}

// StV1SweepClient is the chain surface of the sweep chain. The close is the
// deposit signer's; every other send is the root signer's.
type StV1SweepClient interface {
	V1SweepChain(ctx context.Context, epoch uint64, noId uint64, firstEpoch uint64) (*StV1SweepChain, error)
	V1SweepLatestBlock(ctx context.Context) (uint64, error)
	V1SweepClose(ctx context.Context, epoch uint64, noId uint64) (string, error)
	V1SweepDefer(ctx context.Context, epoch uint64, noId uint64) (string, error)
	V1SweepCommit(ctx context.Context, epoch uint64, noId uint64, root [32]byte, artifactHash [32]byte) (string, error)
	// V1SweepCommitted returns the artifact hash of a commit intent already
	// reserved for the epoch, so a retry repeats its exact calldata.
	V1SweepCommitted(ctx context.Context, epoch uint64, noId uint64, root [32]byte) ([32]byte, bool, error)
	V1SweepFinalize(ctx context.Context, epoch uint64, noId uint64) (string, error)
	V1SweepClaim(ctx context.Context, epoch uint64, noId uint64, coldkey [32]byte) (string, error)
	V1SweepWithdraw(ctx context.Context, epoch uint64, noId uint64, attempt uint64, coldkey [32]byte) (string, error)
	V1SweepAbsorb(ctx context.Context, epoch uint64, noId uint64, hotkey [32]byte) (string, error)
}

// A step that failed this many times stops the chain with an alert. The close
// is exempt: its operation key is fixed, so a reverted close cannot be retried,
// and after stV1SweepMaximumCloseAttempts failures the chain defers instead.
const (
	stV1SweepMaximumFailures      = 10
	stV1SweepMaximumCloseAttempts = 3
)

var stV1SweepClientLock sync.Mutex
var stV1SweepClientInstance StV1SweepClient

// SetStV1SweepClient overrides the core sweep client (for tests); nil
// restores it.
func SetStV1SweepClient(client StV1SweepClient) {
	stV1SweepClientLock.Lock()
	defer stV1SweepClientLock.Unlock()
	stV1SweepClientInstance = client
}

func stV1SweepClient() StV1SweepClient {
	stV1SweepClientLock.Lock()
	override := stV1SweepClientInstance
	stV1SweepClientLock.Unlock()
	if override != nil {
		return override
	}
	if core, ok := stClient().(*CoreStClient); ok && core != nil {
		return core
	}
	return nil
}

// stV1SweepPublish stores the sweep record in the public ledger object store
// (GET /sn/ledger/object?hash=sha256:<hex>); tests replace it.
var stV1SweepPublish = PutStLedgerV2Object

// stV1SweepObject reads a stored record back by reference; tests replace it.
var stV1SweepObject = GetStLedgerV2Object

// StV1SweepRoot is the single-leaf v1 payout root paying coldkey 10,000 bps:
// vault1's payoutLeaf, keccak256(keccak256(abi.encode(coldkey, 10000))).
func StV1SweepRoot(coldkey [32]byte) [32]byte {
	encoded := make([]byte, 64)
	copy(encoded[:32], coldkey[:])
	new(big.Int).SetUint64(stV1SweepShareBps).FillBytes(encoded[32:])
	return [32]byte(crypto.Keccak256(crypto.Keccak256(encoded)))
}

// stV1SweepClaimKey is vault1's leafClaimed key, keccak256(abi.encode(noId,
// coldkey)).
func stV1SweepClaimKey(noId uint64, coldkey [32]byte) [32]byte {
	encoded := make([]byte, 64)
	new(big.Int).SetUint64(noId).FillBytes(encoded[:32])
	copy(encoded[32:], coldkey[:])
	return [32]byte(crypto.Keccak256(encoded))
}

// ---------------------------------------------------------------------------
// Record
// ---------------------------------------------------------------------------

type StV1SweepLeaf struct {
	Coldkey  string `json:"coldkey"`
	ShareBps uint64 `json:"share_bps"`
}

type StV1SweepBlock struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

type StV1SweepCarrySource struct {
	Epoch      uint64 `json:"epoch"`
	Status     string `json:"status"`
	FundedRao  string `json:"funded_rao"`
	TotalRao   string `json:"total_rao"`
	ClaimedRao string `json:"claimed_rao"`
}

type StV1SweepExpiredRemainder struct {
	Epoch        uint64 `json:"epoch"`
	Coldkey      string `json:"coldkey"`
	RemainderRao string `json:"remainder_rao"`
}

type StV1SweepRecovery struct {
	SourceEpoch uint64 `json:"source_epoch"`
	AmountRao   string `json:"amount_rao"`
	Rule        string `json:"rule"`
}

type StV1SweepAllocation struct {
	Recovery    []StV1SweepRecovery `json:"recovery"`
	OrdinaryRao string              `json:"ordinary_rao"`
	Decision    string              `json:"decision"`
}

// StV1SweepRecord is the published sweep record. Its JSON is the compact
// encoding of this struct in field order; artifactHash is its sha256.
type StV1SweepRecord struct {
	Schema              string                      `json:"schema"`
	ChainId             uint64                      `json:"chain_id"`
	GenesisHash         string                      `json:"genesis_hash"`
	Netuid              uint64                      `json:"netuid"`
	NoId                uint64                      `json:"no_id"`
	Coordinator         string                      `json:"coordinator"`
	SettlementVault     string                      `json:"settlement_vault"`
	SettlementV2        string                      `json:"settlement_v2"`
	VaultV2             string                      `json:"vault_v2"`
	PolicyHash          string                      `json:"policy_hash"`
	FirstV2Epoch        uint64                      `json:"first_v2_epoch"`
	SweepEpoch          uint64                      `json:"sweep_epoch"`
	Leaf                StV1SweepLeaf               `json:"leaf"`
	PayoutRoot          string                      `json:"payout_root"`
	PinnedBlock         StV1SweepBlock              `json:"pinned_block"`
	CarryRao            string                      `json:"carry_rao"`
	SweepEpochFundedRao string                      `json:"sweep_epoch_funded_rao"`
	SweepTotalRao       string                      `json:"sweep_total_rao"`
	CarrySources        []StV1SweepCarrySource      `json:"carry_sources"`
	ExpiredRemainders   []StV1SweepExpiredRemainder `json:"expired_remainders"`
	Allocation          StV1SweepAllocation         `json:"allocation"`
}

// The owner's allocation decision, recorded verbatim in every record.
const stV1SweepDecision = "owner 2026-10-31: each recovery source epoch's root-missed funding by that epoch's v2 rows; everything else (other carry, the sweep-epoch capture) as ordinary funding of the pool's next v2 epoch"

func stV1SweepStatusName(status uint8) string {
	switch status {
	case stV1SweepUnset:
		return "unset"
	case stV1SweepFunded:
		return "funded"
	case stV1SweepFinalized:
		return "finalized"
	case stV1SweepRootMissed:
		return "root_missed"
	case stV1SweepCarried:
		return "carried"
	}
	return "unsupported"
}

func stV1SweepHex(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

func stV1SweepAddress(address common.Address) string {
	return stV1SweepHex(address.Bytes())
}

func stV1SweepAmount(value *big.Int) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return value
}

// stV1SweepBuildRecord builds the record of the chain snapshot. It refuses a
// census it cannot account for exactly: a prior epoch still funded (its
// finalization would change the carry after the record) and a finalized or
// carried prior entitlement (an expired-remainder record needs its leaves,
// which this record does not carry).
func stV1SweepBuildRecord(cfg *StConfig, epoch uint64, firstEpoch uint64, chain *StV1SweepChain) ([]byte, *StV1SweepRecord, error) {
	if chain.Sweep.Status != stV1SweepFunded {
		return nil, nil, fmt.Errorf("st: the sweep epoch %d is %s, not funded", epoch, stV1SweepStatusName(chain.Sweep.Status))
	}
	sources := []StV1SweepCarrySource{}
	priorFunded := map[uint64]*big.Int{}
	for _, prior := range chain.Prior {
		switch prior.Status {
		case stV1SweepUnset:
			continue
		case stV1SweepFunded:
			return nil, nil, fmt.Errorf("st: prior epoch %d is funded and unfinalized; its finalization would change the carry", prior.Epoch)
		case stV1SweepFinalized, stV1SweepCarried:
			return nil, nil, fmt.Errorf("st: prior epoch %d is %s; an expired-remainder record is not implemented", prior.Epoch, stV1SweepStatusName(prior.Status))
		case stV1SweepRootMissed:
			sources = append(sources, StV1SweepCarrySource{
				Epoch: prior.Epoch, Status: stV1SweepStatusName(prior.Status),
				FundedRao: stV1SweepAmount(prior.Funded).String(), TotalRao: stV1SweepAmount(prior.Total).String(), ClaimedRao: stV1SweepAmount(prior.Claimed).String(),
			})
			priorFunded[prior.Epoch] = stV1SweepAmount(prior.Funded)
		default:
			return nil, nil, fmt.Errorf("st: prior epoch %d has an unsupported status %d", prior.Epoch, prior.Status)
		}
	}
	carry := stV1SweepAmount(chain.Carry)
	carried := new(big.Int)
	for _, funded := range priorFunded {
		carried.Add(carried, funded)
	}
	if carried.Cmp(carry) != 0 {
		return nil, nil, fmt.Errorf("st: the carry %s differs from the root-missed funding %s of its sources", carry, carried)
	}
	funded := stV1SweepAmount(chain.Sweep.Funded)
	total := new(big.Int).Add(carry, funded)
	recovery := []StV1SweepRecovery{}
	recovered := new(big.Int)
	for _, source := range cfg.V1SweepRecoveryEpochs {
		amount, ok := priorFunded[source]
		if !ok || amount.Sign() == 0 {
			return nil, nil, fmt.Errorf("st: recovery epoch %d holds no root-missed funding", source)
		}
		recovery = append(recovery, StV1SweepRecovery{SourceEpoch: source, AmountRao: amount.String(), Rule: "v2-epoch-rows"})
		recovered.Add(recovered, amount)
	}
	root := StV1SweepRoot(chain.Vault2Coldkey)
	record := &StV1SweepRecord{
		Schema:              StV1SweepRecordSchema,
		ChainId:             cfg.ChainId,
		GenesisHash:         stV1SweepHex(cfg.GenesisHash[:]),
		Netuid:              cfg.Netuid,
		NoId:                cfg.NoId,
		Coordinator:         stV1SweepAddress(cfg.ContractAddress),
		SettlementVault:     stV1SweepAddress(cfg.SettlementVault),
		SettlementV2:        stV1SweepAddress(cfg.SettlementV2),
		VaultV2:             stV1SweepAddress(cfg.VaultV2),
		PolicyHash:          stV1SweepHex(cfg.PolicyHash[:]),
		FirstV2Epoch:        firstEpoch,
		SweepEpoch:          epoch,
		Leaf:                StV1SweepLeaf{Coldkey: stV1SweepHex(chain.Vault2Coldkey[:]), ShareBps: stV1SweepShareBps},
		PayoutRoot:          stV1SweepHex(root[:]),
		PinnedBlock:         StV1SweepBlock{Number: chain.HeadBlock, Hash: stV1SweepHex(chain.HeadHash[:])},
		CarryRao:            carry.String(),
		SweepEpochFundedRao: funded.String(),
		SweepTotalRao:       total.String(),
		CarrySources:        sources,
		ExpiredRemainders:   []StV1SweepExpiredRemainder{},
		Allocation: StV1SweepAllocation{
			Recovery:    recovery,
			OrdinaryRao: new(big.Int).Sub(total, recovered).String(),
			Decision:    stV1SweepDecision,
		},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, nil, err
	}
	return encoded, record, nil
}

// ---------------------------------------------------------------------------
// Planner
// ---------------------------------------------------------------------------

const (
	stV1SweepActWait        = "wait"
	stV1SweepActClose       = "close"
	stV1SweepActDefer       = "defer"
	stV1SweepActCommit      = "commit"
	stV1SweepActFinalize    = "finalize"
	stV1SweepActClaim       = "claim"
	stV1SweepActWithdraw    = "withdraw"
	stV1SweepActAbsorb      = "absorb"
	stV1SweepActAwaitAssign = "await-assign"
	stV1SweepActRefuse      = "refuse"
	stV1SweepActAbandon     = "abandon"
	stV1SweepActDone        = "done"
)

// One decided next action.
type stV1SweepAction struct {
	kind   string
	epoch  uint64
	amount *big.Int
	// until is the block a wait ends at; 0 polls.
	until  uint64
	reason string
}

func (self stV1SweepAction) String() string {
	switch {
	case self.amount != nil:
		return fmt.Sprintf("%s epoch %d amount %s: %s", self.kind, self.epoch, self.amount, self.reason)
	case self.until != 0:
		return fmt.Sprintf("%s until block %d: %s", self.kind, self.until, self.reason)
	}
	return fmt.Sprintf("%s epoch %d: %s", self.kind, self.epoch, self.reason)
}

// The planner's inputs.
type stV1SweepInput struct {
	epoch      uint64
	firstEpoch uint64
	chain      *StV1SweepChain
	// latest is the latest (unfinalized) block number.
	latest uint64
	// v2CommitDone reports a terminal v2 commit outcome recorded for an epoch
	// (confirmed, skipped or missed), for a window whose root never comes.
	v2CommitDone func(epoch uint64) bool
	noId         uint64
	// closeFailures counts the failed closes; past the close attempts the
	// chain stops closing and defers once the root key is free.
	closeFailures int
}

// stV1SweepQuiet reports whether the root key may send now: no v2 commit
// window is open without its root, and the next boundary is not imminent.
func stV1SweepQuiet(in *stV1SweepInput) (bool, string) {
	chain := in.chain
	if 0 < chain.Current && in.firstEpoch < chain.Current {
		recent := chain.Current - 1
		windowOpen := chain.HeadBlock <= chain.RecentEnd+chain.RootWindow
		rooted := chain.V2RootPresent && recent <= chain.V2RootThrough
		if windowOpen && !rooted && (in.v2CommitDone == nil || !in.v2CommitDone(recent)) {
			return false, fmt.Sprintf("the v2 commit window of epoch %d is open without its root; the root key stays with v2", recent)
		}
	}
	if chain.NextEnd != 0 && chain.HeadBlock < chain.NextEnd && chain.NextEnd-chain.HeadBlock <= stV1SweepBoundaryQuietBlocks {
		return false, fmt.Sprintf("the boundary at block %d is imminent; the root key stays idle for the v2 close", chain.NextEnd)
	}
	return true, ""
}

// stV1SweepNext decides the single next action of the sweep chain.
func stV1SweepNext(in *stV1SweepInput) stV1SweepAction {
	chain := in.chain
	epoch := in.epoch
	wait := func(until uint64, format string, args ...any) stV1SweepAction {
		return stV1SweepAction{kind: stV1SweepActWait, epoch: epoch, until: until, reason: fmt.Sprintf(format, args...)}
	}
	act := func(kind string, target uint64, format string, args ...any) stV1SweepAction {
		return stV1SweepAction{kind: kind, epoch: target, reason: fmt.Sprintf(format, args...)}
	}
	if epoch < in.firstEpoch {
		return act(stV1SweepActRefuse, epoch, "the sweep epoch precedes the pool's first v2 epoch %d", in.firstEpoch)
	}
	for _, other := range chain.PostCutover {
		if other.Root != ([32]byte{}) {
			return act(stV1SweepActRefuse, epoch, "post-cutover v1 epoch %d holds a root; the sweep root must be the only one, never a second", other.Epoch)
		}
	}
	sweepRoot := StV1SweepRoot(chain.Vault2Coldkey)
	sweep := chain.Sweep
	if sweep.Root != ([32]byte{}) && sweep.Root != sweepRoot {
		return act(stV1SweepActRefuse, epoch, "epoch %d holds a v1 root that is not the single vault2 leaf", epoch)
	}
	if chain.Paused {
		return wait(0, "v1 settlement is paused")
	}
	if chain.HeadBlock < chain.End {
		return wait(chain.End, "epoch %d ends at block %d", epoch, chain.End)
	}
	// The close is the deposit signer's and bound to the close grace, so it
	// does not wait for the root key.
	if sweep.Status == stV1SweepUnset && in.closeFailures < stV1SweepMaximumCloseAttempts && in.latest+1+stV1SweepCloseMarginBlocks <= chain.End+chain.CloseGrace {
		return act(stV1SweepActClose, epoch, "capture the reveal-lag stake inside the close grace ending at block %d", chain.End+chain.CloseGrace)
	}
	if quiet, reason := stV1SweepQuiet(in); !quiet {
		return wait(0, "%s", reason)
	}
	for _, other := range chain.PostCutover {
		if other.Epoch < epoch && other.Status == stV1SweepFunded && other.Root == ([32]byte{}) && other.End+chain.FinalizeOffset <= chain.HeadBlock {
			return act(stV1SweepActFinalize, other.Epoch, "post-cutover epoch %d was funded without a root; finalizing records it root-missed into carry", other.Epoch)
		}
	}
	deadline := chain.End + chain.RootWindow
	switch {
	case sweep.Root == ([32]byte{}):
		if deadline < chain.HeadBlock || deadline < in.latest+1+stV1SweepCommitMarginBlocks {
			if sweep.Status == stV1SweepFunded {
				if chain.End+chain.FinalizeOffset <= chain.HeadBlock {
					return act(stV1SweepActFinalize, epoch, "the sweep root missed its window; epoch %d finalizes root-missed and its capture joins the carry", epoch)
				}
				return wait(chain.End+chain.FinalizeOffset, "the sweep root missed its window; epoch %d finalizes root-missed at block %d", epoch, chain.End+chain.FinalizeOffset)
			}
			return act(stV1SweepActAbandon, epoch, "no sweep root was committed by block %d; re-point v1_sweep_epoch to a later epoch", deadline)
		}
		if sweep.Status == stV1SweepUnset {
			// deferMissedEmission reverts until a block past the close grace.
			if in.latest+1 <= chain.End+chain.CloseGrace {
				return wait(chain.End+chain.CloseGrace+1, "the close grace ends at block %d; the deferral of epoch %d follows it", chain.End+chain.CloseGrace, epoch)
			}
			return act(stV1SweepActDefer, epoch, "the close grace passed without a capture; record zero for epoch %d", epoch)
		}
		if sweep.Status != stV1SweepFunded {
			return act(stV1SweepActRefuse, epoch, "epoch %d is %s before its sweep root", epoch, stV1SweepStatusName(sweep.Status))
		}
		for _, prior := range chain.Prior {
			if prior.Status == stV1SweepFunded {
				return wait(0, "prior epoch %d is funded and unfinalized; its finalization would change the carry", prior.Epoch)
			}
		}
		return act(stV1SweepActCommit, epoch, "commit the single vault2 leaf before block %d", deadline)
	case sweep.Status == stV1SweepFunded:
		if chain.End+chain.FinalizeOffset <= chain.HeadBlock {
			return act(stV1SweepActFinalize, epoch, "finalize the sweep root; the vault folds all carry into it")
		}
		return wait(chain.End+chain.FinalizeOffset, "the sweep root finalizes at block %d", chain.End+chain.FinalizeOffset)
	case sweep.Status == stV1SweepFinalized:
		if !chain.LeafClaimed {
			if sweep.ExpiryBlock < chain.HeadBlock {
				return act(stV1SweepActRefuse, epoch, "the sweep entitlement expired unclaimed at block %d", sweep.ExpiryBlock)
			}
			return act(stV1SweepActClaim, epoch, "claim the single leaf for vault2's coldkey")
		}
		if 0 < stV1SweepAmount(chain.ClaimCredit).Sign() {
			return act(stV1SweepActWithdraw, epoch, "the claim's payment was deferred; withdraw vault2's claim credit")
		}
		if 0 < stV1SweepAmount(chain.Absorbable).Sign() {
			return act(stV1SweepActAbsorb, epoch, "absorb vault2's stake on the v1 escrow hotkey")
		}
		surplus := new(big.Int).Sub(stV1SweepAmount(chain.V2LiveEscrow), stV1SweepAmount(chain.V2Accounted))
		if surplus.Cmp(big.NewInt(stV1SweepMinimumAssignRao)) < 0 {
			return act(stV1SweepActDone, epoch, "the absorbed sweep is assigned: no surplus is left in vault2")
		}
		amount := surplus
		if total := stV1SweepAmount(sweep.Total); total.Cmp(amount) < 0 {
			amount = new(big.Int).Set(total)
		}
		if !chain.AssignSelector {
			return stV1SweepAction{kind: stV1SweepActAwaitAssign, epoch: epoch, amount: amount, reason: "the STSettlement2 phase C upgrade is not live; the owner's Safe upgrades, then assigns"}
		}
		return stV1SweepAction{kind: stV1SweepActAwaitAssign, epoch: epoch, amount: amount, reason: fmt.Sprintf("the owner's Safe assigns: assignSurplus(%d, %d, %s) on STSettlement2", in.noId, chain.V2NextEpoch, amount)}
	}
	return act(stV1SweepActRefuse, epoch, "the sweep entitlement is %s", stV1SweepStatusName(sweep.Status))
}

// ---------------------------------------------------------------------------
// Turn
// ---------------------------------------------------------------------------

// stV1SweepAlertSink receives each refusal or abandonment; tests replace it.
var stV1SweepAlertSink = func(subject string, message string) {
	glog.Errorf("[st]v1 sweep ALERT %s: %s\n", subject, message)
}

// stV1SweepRecordOutcome records one sweep step as a publish row; tests
// replace it.
var stV1SweepRecordOutcome = func(ctx context.Context, cfg *StConfig, epoch uint64, kind string, outcome *StPublishOutcome) {
	stResolvePublish(ctx, model.AddStPublish(ctx, cfg.DeploymentKey(), epoch, kind), outcome)
}

// stV1SweepPublishes reads the epoch's publish rows; tests replace it.
var stV1SweepPublishes = func(ctx context.Context, cfg *StConfig, epoch uint64) []*model.StPublish {
	return model.GetStPublishes(ctx, cfg.DeploymentKey(), epoch)
}

// stV1SweepConcluded returns a terminal status recorded for the sweep epoch:
// a confirmed assign, a refusal or an abandonment end the chain.
func stV1SweepConcluded(publishes []*model.StPublish) (string, bool) {
	for _, publish := range publishes {
		switch {
		case publish.Kind == StPublishKindV1SweepAssign && publish.Status == model.StPublishStatusConfirmed:
			return "assigned", true
		case publish.Kind == StPublishKindV1SweepRefused || publish.Kind == StPublishKindV1SweepAbandon:
			return publish.Kind, true
		}
	}
	return "", false
}

// StV1SweepDue returns the configured sweep epoch while its chain has not
// concluded, for the task scheduler. It reads only the configuration and the
// epoch's publish rows.
func StV1SweepDue(ctx context.Context) (uint64, bool) {
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled {
		return 0, false
	}
	epoch, sweep := stLedgerV1SweepEpoch(ctx, cfg)
	if !sweep {
		return 0, false
	}
	if _, done := stV1SweepConcluded(stV1SweepPublishes(ctx, cfg, epoch)); done {
		return 0, false
	}
	return epoch, true
}

// stV1SweepV2CommitDone reports a terminal v2 commit outcome for an epoch.
func stV1SweepV2CommitDone(ctx context.Context, cfg *StConfig) func(uint64) bool {
	return func(epoch uint64) bool {
		return stLedgerV2Outcome(ctx, cfg.DeploymentKey(), epoch, model.StPublishKindV2Commit) != ""
	}
}

func stV1SweepRetry(err error) *StPublishOutcome {
	return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true, RetryAt: server.NowUtc().Add(stV1SweepPollDelay)}
}

func stV1SweepWaitOutcome(cfg *StConfig, chain *StV1SweepChain, action stV1SweepAction) *StPublishOutcome {
	at := server.NowUtc().Add(stV1SweepPollDelay)
	if action.until != 0 && chain.HeadBlock < action.until {
		at = stEstimateBlockTime(chain.HeadBlock, chain.HeadTime, action.until, cfg.BlockSeconds)
	}
	return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: action.reason, Retry: true, RetryAt: at}
}

// StV1SweepTurn runs one turn of the sweep chain: it reads one finalized
// snapshot, decides the next action and performs it. A returned outcome with
// Retry continues the chain at RetryAt; without it the chain has concluded.
func StV1SweepTurn(ctx context.Context) (*StPublishOutcome, error) {
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled {
		return nil, errStNotConfigured
	}
	epoch, sweep := stLedgerV1SweepEpoch(ctx, cfg)
	if !sweep {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: errStV1SweepNotConfigured.Error()}, nil
	}
	if err := stPayoutAdmission(ctx, cfg); err != nil {
		return nil, err
	}
	activation, err := stV1SweepAdmission(ctx, cfg, epoch)
	if err != nil {
		return stV1SweepRetry(err), nil
	}
	publishes := stV1SweepPublishes(ctx, cfg, epoch)
	if status, done := stV1SweepConcluded(publishes); done {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: "the sweep has concluded: " + status}, nil
	}
	client := stV1SweepClient()
	if client == nil {
		return nil, errStV1SweepClientMissing
	}
	chain, err := client.V1SweepChain(ctx, epoch, cfg.NoId, activation.FirstEpoch)
	if err != nil {
		return stV1SweepRetry(err), nil
	}
	latest, err := client.V1SweepLatestBlock(ctx)
	if err != nil {
		return stV1SweepRetry(err), nil
	}
	_, closeFailures := stV1SweepFailures(publishes, stV1SweepActClose)
	action := stV1SweepNext(&stV1SweepInput{
		epoch: epoch, firstEpoch: activation.FirstEpoch, chain: chain, latest: latest,
		v2CommitDone: stV1SweepV2CommitDone(ctx, cfg), noId: cfg.NoId, closeFailures: closeFailures,
	})
	if kind, failures := stV1SweepFailures(publishes, action.kind); action.kind != stV1SweepActClose && stV1SweepMaximumFailures <= failures {
		action = stV1SweepAction{kind: stV1SweepActRefuse, epoch: epoch, reason: fmt.Sprintf("the %s step failed %d times (last: %s)", action.kind, failures, kind)}
	}
	glog.Infof("[st]v1 sweep epoch %d at block %d: %s\n", epoch, chain.HeadBlock, action)
	return stV1SweepPerform(ctx, cfg, client, epoch, activation.FirstEpoch, chain, stV1SweepAttempts(publishes), action), nil
}

// The publish kind of each sending action.
var stV1SweepActionKinds = map[string]string{
	stV1SweepActClose:    StPublishKindV1SweepClose,
	stV1SweepActDefer:    StPublishKindV1SweepDefer,
	stV1SweepActCommit:   StPublishKindV1SweepCommit,
	stV1SweepActFinalize: StPublishKindV1SweepFinalize,
	stV1SweepActClaim:    StPublishKindV1SweepClaim,
	stV1SweepActWithdraw: StPublishKindV1SweepWithdraw,
	stV1SweepActAbsorb:   StPublishKindV1SweepAbsorb,
}

// stV1SweepFailures counts the failed publishes of an action's kind and
// returns the last failure's reason.
func stV1SweepFailures(publishes []*model.StPublish, action string) (string, int) {
	kind, sends := stV1SweepActionKinds[action]
	if !sends {
		return "", 0
	}
	last, failures := "", 0
	for _, publish := range publishes {
		if publish.Kind == kind && publish.Status == model.StPublishStatusFailed {
			failures++
			if publish.Error != nil {
				last = *publish.Error
			}
		}
	}
	return last, failures
}

// stV1SweepAttempts counts the withdraw attempts so far, the next attempt's
// operation key.
func stV1SweepAttempts(publishes []*model.StPublish) uint64 {
	attempts := uint64(0)
	for _, publish := range publishes {
		if publish.Kind == StPublishKindV1SweepWithdraw {
			attempts++
		}
	}
	return attempts
}

// stV1SweepSend records one chain write as a publish row.
func stV1SweepSend(ctx context.Context, cfg *StConfig, epoch uint64, kind string, send func() (string, error)) *StPublishOutcome {
	txHash, err := send()
	outcome := &StPublishOutcome{Status: model.StPublishStatusConfirmed, TxHash: txHash, Retry: true, RetryAt: server.NowUtc()}
	if err != nil {
		outcome = &StPublishOutcome{Status: model.StPublishStatusFailed, TxHash: txHash, Reason: err.Error(), Retry: true, RetryAt: server.NowUtc().Add(stV1SweepPollDelay)}
	}
	stV1SweepRecordOutcome(ctx, cfg, epoch, kind, outcome)
	return outcome
}

func stV1SweepPerform(ctx context.Context, cfg *StConfig, client StV1SweepClient, epoch uint64, firstEpoch uint64, chain *StV1SweepChain, withdrawAttempt uint64, action stV1SweepAction) *StPublishOutcome {
	noId := cfg.NoId
	switch action.kind {
	case stV1SweepActWait:
		return stV1SweepWaitOutcome(cfg, chain, action)
	case stV1SweepActRefuse, stV1SweepActAbandon:
		kind := StPublishKindV1SweepRefused
		if action.kind == stV1SweepActAbandon {
			kind = StPublishKindV1SweepAbandon
		}
		stV1SweepAlertSink(fmt.Sprintf("epoch %d", epoch), action.reason)
		outcome := &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: action.reason}
		stV1SweepRecordOutcome(ctx, cfg, epoch, kind, outcome)
		return outcome
	case stV1SweepActDone:
		// The chain ends once the absorbed sweep is assigned: recording it
		// concludes the chain, so later surplus is never mistaken for it.
		outcome := &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: action.reason}
		stV1SweepRecordOutcome(ctx, cfg, epoch, StPublishKindV1SweepAssign, outcome)
		return outcome
	case stV1SweepActClose:
		return stV1SweepSend(ctx, cfg, epoch, StPublishKindV1SweepClose, func() (string, error) {
			return client.V1SweepClose(ctx, epoch, noId)
		})
	case stV1SweepActDefer:
		return stV1SweepSend(ctx, cfg, epoch, StPublishKindV1SweepDefer, func() (string, error) {
			return client.V1SweepDefer(ctx, epoch, noId)
		})
	case stV1SweepActCommit:
		return stV1SweepCommit(ctx, cfg, client, epoch, firstEpoch, chain)
	case stV1SweepActFinalize:
		target := action.epoch
		return stV1SweepSend(ctx, cfg, epoch, StPublishKindV1SweepFinalize, func() (string, error) {
			return client.V1SweepFinalize(ctx, target, noId)
		})
	case stV1SweepActClaim:
		return stV1SweepSend(ctx, cfg, epoch, StPublishKindV1SweepClaim, func() (string, error) {
			return client.V1SweepClaim(ctx, epoch, noId, chain.Vault2Coldkey)
		})
	case stV1SweepActWithdraw:
		return stV1SweepSend(ctx, cfg, epoch, StPublishKindV1SweepWithdraw, func() (string, error) {
			return client.V1SweepWithdraw(ctx, epoch, noId, withdrawAttempt, chain.Vault2Coldkey)
		})
	case stV1SweepActAbsorb:
		return stV1SweepSend(ctx, cfg, epoch, StPublishKindV1SweepAbsorb, func() (string, error) {
			return client.V1SweepAbsorb(ctx, epoch, noId, chain.V1EscrowHotkey)
		})
	case stV1SweepActAwaitAssign:
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("%s (amount %s)", action.reason, action.amount), Retry: true, RetryAt: server.NowUtc().Add(10 * stV1SweepPollDelay)}
	}
	return stV1SweepRetry(fmt.Errorf("st: unknown sweep action %q", action.kind))
}

// stV1SweepCommit publishes the record and commits the single leaf. A commit
// intent already reserved for the epoch keeps its exact artifact hash, whose
// record must be in the store; otherwise the record of this snapshot is built,
// stored and committed.
func stV1SweepCommit(ctx context.Context, cfg *StConfig, client StV1SweepClient, epoch uint64, firstEpoch uint64, chain *StV1SweepChain) *StPublishOutcome {
	root := StV1SweepRoot(chain.Vault2Coldkey)
	artifactHash, reserved, err := client.V1SweepCommitted(ctx, epoch, cfg.NoId, root)
	if err != nil {
		return stV1SweepRetry(err)
	}
	if reserved {
		if _, err := stV1SweepObject(ctx, "sha256:"+hex.EncodeToString(artifactHash[:])); err != nil {
			return stV1SweepRetry(fmt.Errorf("st: the reserved sweep commit's record is not in the store: %w", err))
		}
	} else {
		encoded, _, err := stV1SweepBuildRecord(cfg, epoch, firstEpoch, chain)
		if err != nil {
			return stV1SweepRetry(err)
		}
		ref, err := stV1SweepPublish(ctx, encoded)
		if err != nil {
			return stV1SweepRetry(err)
		}
		artifactHash = sha256.Sum256(encoded)
		if ref != "sha256:"+hex.EncodeToString(artifactHash[:]) {
			return stV1SweepRetry(fmt.Errorf("st: the store returned %s for the sweep record", ref))
		}
		stV1SweepRecordOutcome(ctx, cfg, epoch, StPublishKindV1SweepRecord, &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: ref})
	}
	return stV1SweepSend(ctx, cfg, epoch, StPublishKindV1SweepCommit, func() (string, error) {
		return client.V1SweepCommit(ctx, epoch, cfg.NoId, root, artifactHash)
	})
}

// ---------------------------------------------------------------------------
// Readers
// ---------------------------------------------------------------------------

// StV1SweepStatusView is the sweep as readers see it: the configured epoch,
// the chain facts and the next action, and, before the commit, the record
// this snapshot would commit. Amounts are rao.
type StV1SweepStatusView struct {
	Configured    bool                `json:"configured"`
	Epoch         uint64              `json:"epoch,omitempty"`
	FirstV2Epoch  uint64              `json:"first_v2_epoch,omitempty"`
	Refusal       string              `json:"refusal,omitempty"`
	HeadBlock     uint64              `json:"head_block,omitempty"`
	End           uint64              `json:"end_block,omitempty"`
	CloseDeadline uint64              `json:"close_deadline_block,omitempty"`
	RootDeadline  uint64              `json:"root_deadline_block,omitempty"`
	FinalizeAt    uint64              `json:"finalize_block,omitempty"`
	Status        string              `json:"status,omitempty"`
	PayoutRoot    string              `json:"payout_root,omitempty"`
	Committed     bool                `json:"committed"`
	ArtifactHash  string              `json:"artifact_hash,omitempty"`
	CarryRao      string              `json:"carry_rao,omitempty"`
	FundedRao     string              `json:"funded_rao,omitempty"`
	TotalRao      string              `json:"total_rao,omitempty"`
	LeafClaimed   bool                `json:"leaf_claimed"`
	AbsorbableRao string              `json:"absorbable_rao,omitempty"`
	V2SurplusRao  string              `json:"v2_surplus_rao,omitempty"`
	AssignLive    bool                `json:"assign_live"`
	Recorded      bool                `json:"recorded"`
	SafeCalls     []StV1SweepSafeCall `json:"safe_calls,omitempty"`
	Next          string              `json:"next,omitempty"`
	Preview       *StV1SweepRecord    `json:"preview_record,omitempty"`
	PreviewHash   string              `json:"preview_artifact_hash,omitempty"`
	PreviewError  string              `json:"preview_error,omitempty"`
	Publishes     []*model.StPublish  `json:"publishes,omitempty"`
}

// StV1SweepSafeCall is one owner call the governance Safe signs, built from
// the snapshot: the target, the calldata and what it does.
type StV1SweepSafeCall struct {
	Name     string `json:"name"`
	To       string `json:"to"`
	Calldata string `json:"calldata"`
	Note     string `json:"note"`
}

// stV1SweepSafeCalls returns the owner calls due at the snapshot: the
// recordV1Sweep of a committed, unrecorded sweep root, and the assign of an
// absorbed surplus into the pool's next funding snapshot.
func stV1SweepSafeCalls(cfg *StConfig, epoch uint64, chain *StV1SweepChain, surplus *big.Int) []StV1SweepSafeCall {
	calls := []StV1SweepSafeCall{}
	to := stV1SweepAddress(cfg.SettlementV2)
	root := StV1SweepRoot(chain.Vault2Coldkey)
	if chain.AssignSelector && chain.Sweep.Root == root && chain.RecordedArtifact == ([32]byte{}) {
		if calldata, err := StV1SweepPackRecord(cfg.NoId, epoch, chain.Sweep.RootArtifact); err == nil {
			calls = append(calls, StV1SweepSafeCall{Name: "recordV1Sweep", To: to, Calldata: stV1SweepHex(calldata), Note: fmt.Sprintf("record the committed sweep root of epoch %d (artifact %s) for readers", epoch, stV1SweepHex(chain.Sweep.RootArtifact[:]))})
		}
	}
	absorbed := chain.Sweep.Status == stV1SweepFinalized && chain.LeafClaimed && stV1SweepAmount(chain.ClaimCredit).Sign() == 0 && stV1SweepAmount(chain.Absorbable).Sign() == 0
	if chain.AssignSelector && absorbed && big.NewInt(stV1SweepMinimumAssignRao).Cmp(surplus) <= 0 {
		amount := surplus
		if total := stV1SweepAmount(chain.Sweep.Total); total.Cmp(amount) < 0 {
			amount = new(big.Int).Set(total)
		}
		if calldata, err := StV1SweepPackAssign(cfg.NoId, chain.V2NextEpoch, amount); err == nil {
			calls = append(calls, StV1SweepSafeCall{Name: "assignSurplus", To: to, Calldata: stV1SweepHex(calldata), Note: fmt.Sprintf("assign %s rao into epoch %d's funding snapshot; it reverts InvalidEpoch once epoch %d closes, then rebuild for epoch %d", amount, chain.V2NextEpoch, chain.V2NextEpoch, chain.V2NextEpoch+1)})
		}
	}
	return calls
}

// GetStV1SweepStatus reads the sweep's state read-only. It sends nothing.
func GetStV1SweepStatus(ctx context.Context) (*StV1SweepStatusView, error) {
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled {
		return nil, errStNotConfigured
	}
	epoch, sweep := stLedgerV1SweepEpoch(ctx, cfg)
	view := &StV1SweepStatusView{Configured: sweep, Epoch: epoch}
	if !sweep {
		return view, nil
	}
	activation, err := stV1SweepAdmission(ctx, cfg, epoch)
	if err != nil {
		view.Refusal = err.Error()
		return view, nil
	}
	view.FirstV2Epoch = activation.FirstEpoch
	client := stV1SweepClient()
	if client == nil {
		return nil, errStV1SweepClientMissing
	}
	chain, err := client.V1SweepChain(ctx, epoch, cfg.NoId, activation.FirstEpoch)
	if err != nil {
		return nil, err
	}
	latest, err := client.V1SweepLatestBlock(ctx)
	if err != nil {
		return nil, err
	}
	publishes := stV1SweepPublishes(ctx, cfg, epoch)
	_, closeFailures := stV1SweepFailures(publishes, stV1SweepActClose)
	root := StV1SweepRoot(chain.Vault2Coldkey)
	surplus := new(big.Int).Sub(stV1SweepAmount(chain.V2LiveEscrow), stV1SweepAmount(chain.V2Accounted))
	view.HeadBlock, view.End = chain.HeadBlock, chain.End
	view.CloseDeadline, view.RootDeadline, view.FinalizeAt = chain.End+chain.CloseGrace, chain.End+chain.RootWindow, chain.End+chain.FinalizeOffset
	view.Status = stV1SweepStatusName(chain.Sweep.Status)
	view.PayoutRoot = stV1SweepHex(root[:])
	view.Committed = chain.Sweep.Root == root
	if view.Committed {
		view.ArtifactHash = stV1SweepHex(chain.Sweep.RootArtifact[:])
	}
	view.CarryRao, view.FundedRao, view.TotalRao = stV1SweepAmount(chain.Carry).String(), stV1SweepAmount(chain.Sweep.Funded).String(), stV1SweepAmount(chain.Sweep.Total).String()
	view.LeafClaimed, view.AbsorbableRao, view.V2SurplusRao, view.AssignLive = chain.LeafClaimed, stV1SweepAmount(chain.Absorbable).String(), surplus.String(), chain.AssignSelector
	view.Recorded = chain.RecordedArtifact != ([32]byte{}) && chain.RecordedArtifact == chain.Sweep.RootArtifact
	view.SafeCalls = stV1SweepSafeCalls(cfg, epoch, chain, surplus)
	view.Next = stV1SweepNext(&stV1SweepInput{
		epoch: epoch, firstEpoch: activation.FirstEpoch, chain: chain, latest: latest,
		v2CommitDone: stV1SweepV2CommitDone(ctx, cfg), noId: cfg.NoId, closeFailures: closeFailures,
	}).String()
	view.Publishes = publishes
	if !view.Committed {
		// The preview is the record this snapshot would commit, with a
		// funded sweep epoch assumed when it is not captured yet.
		preview := *chain
		if preview.Sweep.Status == stV1SweepUnset {
			preview.Sweep.Status = stV1SweepFunded
		}
		if encoded, record, err := stV1SweepBuildRecord(cfg, epoch, activation.FirstEpoch, &preview); err != nil {
			view.PreviewError = err.Error()
		} else {
			hash := sha256.Sum256(encoded)
			view.Preview, view.PreviewHash = record, stV1SweepHex(hash[:])
		}
	}
	slices.SortFunc(view.Publishes, func(a, b *model.StPublish) int { return a.CreateTime.Compare(b.CreateTime) })
	return view, nil
}
