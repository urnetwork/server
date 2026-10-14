package controller

// st_ledger_v2_client.go — the settlement v2 chain surface of the operator
// server (VERSION2-FINAL.md §7, §8.1): views of vault2, STSettlement2 and the
// v1 epoch clock at the finalized head, the driver's four writes, and the
// decoders that mirror both contracts' events. Every write is a durable
// transaction intent of the existing engine, signed by the root key, under a
// logical key that names its epoch or commit seq.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi/bind/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server/model"
)

// Logical operation keys of the v2 driver's writes (§8.1).
func stLedgerV2CloseOperation(epoch uint64, noId uint64) string {
	return fmt.Sprintf("v2close:%d:%d", epoch, noId)
}

func stLedgerV2CommitOperation(epoch uint64, noId uint64) string {
	return fmt.Sprintf("v2commit:%d:%d", epoch, noId)
}

func stLedgerV2FinalizeOperation(epoch uint64, noId uint64) string {
	return fmt.Sprintf("v2finalize:%d:%d", epoch, noId)
}

func stLedgerV2AnswerOperation(noId uint64, seq uint64, rootIndex uint64, index uint64) string {
	return fmt.Sprintf("v2answer:%d:%d:%d:%d", noId, seq, rootIndex, index)
}

// ErrStLedgerV2CommitSpent reports that an epoch's commit intent already ended
// without a commit, so the epoch is left to the next root.
var ErrStLedgerV2CommitSpent = errors.New("st ledger v2: the epoch's commit intent is spent")

// vault2's DemandState of one index of a pending root (SPEC A1).
const (
	StLedgerV2DemandNone     uint8 = 0
	StLedgerV2DemandOpen     uint8 = 1
	StLedgerV2DemandAnswered uint8 = 2
)

// StLedgerV2FinalizedRoot is an entry of vault2's roots.
type StLedgerV2FinalizedRoot struct {
	Index      uint64
	Root       [32]byte
	Total      uint256.Int
	LeafCount  uint64
	Epoch      uint64
	LedgerHash [32]byte
}

// StLedgerV2Pending is vault2's pending root.
type StLedgerV2Pending struct {
	Seq            uint64
	Root           [32]byte
	Total          uint256.Int
	LeafCount      uint64
	Epoch          uint64
	LedgerHash     [32]byte
	CommittedBlock uint64
	OpenDemands    uint64
}

// StLedgerV2State is one pool's v2 state read at one finalized block.
type StLedgerV2State struct {
	HeadBlock uint64
	// Paused is the v1 coordinator's pause, which stops v2 close, commit and
	// finalize.
	Paused          bool
	Registered      bool
	FirstEpoch      uint64
	LastFundedEpoch uint64
	RootCount       uint64
	// Latest is the latest finalized root, nil when there is none.
	Latest *StLedgerV2FinalizedRoot
	// Pending is nil when the pool has no pending root.
	Pending         *StLedgerV2Pending
	LastCommitBlock uint64
	// Approvals counts the pending seq's approvals at the current auditor set.
	Approvals       uint64
	Quorum          uint64
	ChallengeBlocks uint64
}

// StLedgerV2Commitment is a root as commitDistribution takes it.
type StLedgerV2Commitment struct {
	Root       [32]byte
	Total      uint256.Int
	LeafCount  uint64
	LedgerHash [32]byte
}

// StLedgerV2Answer opens a pending root's path at an index of the finalized
// root RootIndex: that root's account and claim proof, and the pending root's
// top-down path.
type StLedgerV2Answer struct {
	RootIndex uint64
	Index     uint64
	Account   merkle.LedgerAccount
	Proof     []merkle.LedgerNode
	Path      []merkle.LedgerPathNode
}

// StLedgerV2Client is the swappable v2 chain client; CoreStClient implements it.
type StLedgerV2Client interface {
	// LedgerV2Pool reads vault2.pools(noId) at the finalized head.
	LedgerV2Pool(ctx context.Context, noId uint64) (registered bool, firstEpoch uint64, err error)
	// LedgerV2Coordinator reads vault2.coordinator(), the one STSettlement2
	// the vault is bound to, at the finalized head.
	LedgerV2Coordinator(ctx context.Context) (common.Address, error)
	LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error)
	// LedgerV2EpochWindow reads the v1 epochEndBlock(epoch) and the epoch's
	// rootCommitWindowBlocks, which is also v2's close window.
	LedgerV2EpochWindow(ctx context.Context, epoch uint64) (end uint64, window uint64, err error)
	LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error)
	// LedgerV2LatestBlock reads the latest block's number. A send's eth_call
	// and eth_estimateGas run at it; the transaction executes in a later block.
	LedgerV2LatestBlock(ctx context.Context) (uint64, error)
	// LedgerV2Root reads vault2.roots(noId, rootIndex), a finalized root.
	LedgerV2Root(ctx context.Context, noId uint64, rootIndex uint64) (*StLedgerV2FinalizedRoot, error)
	// LedgerV2Demand reads vault2.demanded(noId, seq, rootIndex, index): a
	// DemandState.
	LedgerV2Demand(ctx context.Context, noId uint64, seq uint64, rootIndex uint64, index uint64) (uint8, error)
	// LedgerV2Claimed reads vault2.claimed(noId, index) at the finalized head.
	LedgerV2Claimed(ctx context.Context, noId uint64, index uint64) (uint256.Int, error)
	// LedgerV2Close sends closeOperatorEpoch under v2close:e:no.
	LedgerV2Close(ctx context.Context, epoch uint64, noId uint64) (string, error)
	// LedgerV2Commit sends commitDistribution under v2commit:e:no.
	LedgerV2Commit(ctx context.Context, epoch uint64, noId uint64, prevRootCount uint64, commitment *StLedgerV2Commitment) (string, error)
	// LedgerV2Finalize sends finalizeDistribution under v2finalize:e:no, e
	// being the pending root's epoch.
	LedgerV2Finalize(ctx context.Context, epoch uint64, noId uint64) (string, error)
	// LedgerV2Answer sends checkPath under v2answer:no:seq:rootIndex:index.
	LedgerV2Answer(ctx context.Context, noId uint64, seq uint64, answer *StLedgerV2Answer) (string, error)
}

var stLedgerV2ClientLock sync.Mutex
var stLedgerV2ClientInstance StLedgerV2Client

// SetStLedgerV2Client overrides the core v2 client (for tests); nil restores it.
func SetStLedgerV2Client(client StLedgerV2Client) {
	stLedgerV2ClientLock.Lock()
	defer stLedgerV2ClientLock.Unlock()
	stLedgerV2ClientInstance = client
}

func stLedgerV2Client() StLedgerV2Client {
	stLedgerV2ClientLock.Lock()
	instance := stLedgerV2ClientInstance
	stLedgerV2ClientLock.Unlock()
	if instance != nil {
		return instance
	}
	if core, ok := stClient().(*CoreStClient); ok {
		return core
	}
	return nil
}

var (
	stLedgerV2SettlementAbi = stabi.NewSTSettlement2()
	stLedgerV2VaultAbi      = stabi.NewSTSettlementVault2()
)

func stBig(value uint64) *big.Int {
	return new(big.Int).SetUint64(value)
}

func stUint64(value *big.Int) (uint64, error) {
	if value == nil || !value.IsUint64() {
		return 0, fmt.Errorf("st: chain value %v is not a uint64", value)
	}
	return value.Uint64(), nil
}

func stUint256(value *big.Int) (uint256.Int, error) {
	var out uint256.Int
	if value == nil || value.Sign() < 0 || out.SetFromBig(value) {
		return out, fmt.Errorf("st: chain value %v is not a uint256", value)
	}
	return out, nil
}

func (self *CoreStClient) LedgerV2Pool(ctx context.Context, noId uint64) (bool, uint64, error) {
	pool, err := stViewAt(self, ctx, self.cfg.VaultV2, stLedgerV2VaultAbi.PackPools(stBig(noId)), stLedgerV2VaultAbi.UnpackPools)
	if err != nil {
		return false, 0, fmt.Errorf("vault2 pools(%d): %w", noId, err)
	}
	return pool.Registered, pool.FirstEpoch, nil
}

func (self *CoreStClient) LedgerV2Coordinator(ctx context.Context) (common.Address, error) {
	coordinator, err := stViewAt(self, ctx, self.cfg.VaultV2, stLedgerV2VaultAbi.PackCoordinator(), stLedgerV2VaultAbi.UnpackCoordinator)
	if err != nil {
		return common.Address{}, fmt.Errorf("vault2 coordinator(): %w", err)
	}
	return coordinator, nil
}

func (self *CoreStClient) LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error) {
	ctx, stopRead, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return nil, err
	}
	defer stopRead()
	head, err := self.finalizedBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("finalized head: %w", err)
	}
	block, vault, settlement, no := head.Number, self.cfg.VaultV2, self.cfg.SettlementV2, stBig(noId)
	state := &StLedgerV2State{HeadBlock: block}
	if state.Paused, err = stViewAtBlock(self, ctx, self.cfg.ContractAddress, block, self.coordinator.PackPaused(), self.coordinator.UnpackPaused); err != nil {
		return nil, fmt.Errorf("paused(): %w", err)
	}
	pool, err := stViewAtBlock(self, ctx, vault, block, stLedgerV2VaultAbi.PackPools(no), stLedgerV2VaultAbi.UnpackPools)
	if err != nil {
		return nil, fmt.Errorf("vault2 pools(): %w", err)
	}
	state.Registered, state.FirstEpoch = pool.Registered, pool.FirstEpoch
	if state.ChallengeBlocks, err = stViewAtBlock(self, ctx, vault, block, stLedgerV2VaultAbi.PackCHALLENGEBLOCKS(), stLedgerV2VaultAbi.UnpackCHALLENGEBLOCKS); err != nil {
		return nil, fmt.Errorf("vault2 CHALLENGE_BLOCKS(): %w", err)
	}
	if state.LastCommitBlock, err = stViewAtBlock(self, ctx, vault, block, stLedgerV2VaultAbi.PackLastCommitBlock(no), stLedgerV2VaultAbi.UnpackLastCommitBlock); err != nil {
		return nil, fmt.Errorf("vault2 lastCommitBlock(): %w", err)
	}
	for _, read := range []struct {
		name     string
		address  common.Address
		calldata []byte
		unpack   func([]byte) (*big.Int, error)
		out      *uint64
	}{
		{"vault2 lastFundedEpoch()", vault, stLedgerV2VaultAbi.PackLastFundedEpoch(no), stLedgerV2VaultAbi.UnpackLastFundedEpoch, &state.LastFundedEpoch},
		{"vault2 rootCount()", vault, stLedgerV2VaultAbi.PackRootCount(no), stLedgerV2VaultAbi.UnpackRootCount, &state.RootCount},
		{"approvalCount()", settlement, stLedgerV2SettlementAbi.PackApprovalCount(no), stLedgerV2SettlementAbi.UnpackApprovalCount, &state.Approvals},
		{"quorum()", settlement, stLedgerV2SettlementAbi.PackQuorum(), stLedgerV2SettlementAbi.UnpackQuorum, &state.Quorum},
	} {
		value, err := stViewAtBlock(self, ctx, read.address, block, read.calldata, read.unpack)
		if err == nil {
			*read.out, err = stUint64(value)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", read.name, err)
		}
	}
	if state.RootCount != 0 {
		root, err := stViewAtBlock(self, ctx, vault, block, stLedgerV2VaultAbi.PackRoots(no, stBig(state.RootCount-1)), stLedgerV2VaultAbi.UnpackRoots)
		if err != nil {
			return nil, fmt.Errorf("vault2 roots(): %w", err)
		}
		if state.Latest, err = stLedgerV2FinalizedRootOf(state.RootCount-1, root); err != nil {
			return nil, err
		}
	}
	pending, err := stViewAtBlock(self, ctx, vault, block, stLedgerV2VaultAbi.PackPending(no), stLedgerV2VaultAbi.UnpackPending)
	if err != nil {
		return nil, fmt.Errorf("vault2 pending(): %w", err)
	}
	if pending.Seq != 0 {
		total, err := stUint256(pending.Total)
		if err != nil {
			return nil, err
		}
		state.Pending = &StLedgerV2Pending{Seq: pending.Seq, Root: pending.Root, Total: total, LeafCount: pending.LeafCount, Epoch: pending.Epoch,
			LedgerHash: pending.LedgerHash, CommittedBlock: pending.CommittedBlock, OpenDemands: pending.OpenDemands}
	}
	return state, nil
}

func (self *CoreStClient) LedgerV2EpochWindow(ctx context.Context, epoch uint64) (uint64, uint64, error) {
	ctx, stopRead, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return 0, 0, err
	}
	defer stopRead()
	head, err := self.finalizedBlock(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("finalized head: %w", err)
	}
	end, err := stViewAtBlock(self, ctx, self.cfg.ContractAddress, head.Number, self.coordinator.PackEpochEndBlock(stBig(epoch)), self.coordinator.UnpackEpochEndBlock)
	if err != nil {
		return 0, 0, fmt.Errorf("epochEndBlock(%d): %w", epoch, err)
	}
	policy, err := stViewAtBlock(self, ctx, self.cfg.ContractAddress, head.Number, self.coordinator.PackPolicyAt(stBig(epoch)), self.coordinator.UnpackPolicyAt)
	if err != nil {
		return 0, 0, fmt.Errorf("policyAt(%d): %w", epoch, err)
	}
	endBlock, err := stUint64(end)
	return endBlock, policy.RootCommitWindowBlocks, err
}

func (self *CoreStClient) LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error) {
	funded, err := stViewAt(self, ctx, self.cfg.VaultV2, stLedgerV2VaultAbi.PackFundedThrough(stBig(noId), stBig(epoch)), stLedgerV2VaultAbi.UnpackFundedThrough)
	if err != nil {
		return uint256.Int{}, fmt.Errorf("vault2 fundedThrough(%d, %d): %w", noId, epoch, err)
	}
	return stUint256(funded)
}

func (self *CoreStClient) LedgerV2LatestBlock(ctx context.Context) (uint64, error) {
	ctx, stopRead, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return 0, err
	}
	defer stopRead()
	var latest uint64
	err = self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		block, err := readStRPCBlockIdentity(callCtx, client, rpc.LatestBlockNumber.String(), nil)
		if err != nil {
			return err
		}
		latest = block.Number
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("latest head: %w", err)
	}
	return latest, nil
}

// The finalized root at an index, as vault2.roots returns it.
func stLedgerV2FinalizedRootOf(index uint64, root stabi.ISTSettlementVault2Root) (*StLedgerV2FinalizedRoot, error) {
	total, err := stUint256(root.Total)
	if err != nil {
		return nil, err
	}
	return &StLedgerV2FinalizedRoot{Index: index, Root: root.Root, Total: total, LeafCount: root.LeafCount, Epoch: root.Epoch, LedgerHash: root.LedgerHash}, nil
}

func (self *CoreStClient) LedgerV2Root(ctx context.Context, noId uint64, rootIndex uint64) (*StLedgerV2FinalizedRoot, error) {
	root, err := stViewAt(self, ctx, self.cfg.VaultV2, stLedgerV2VaultAbi.PackRoots(stBig(noId), stBig(rootIndex)), stLedgerV2VaultAbi.UnpackRoots)
	if err != nil {
		return nil, fmt.Errorf("vault2 roots(%d, %d): %w", noId, rootIndex, err)
	}
	return stLedgerV2FinalizedRootOf(rootIndex, root)
}

func (self *CoreStClient) LedgerV2Demand(ctx context.Context, noId uint64, seq uint64, rootIndex uint64, index uint64) (uint8, error) {
	demand, err := stViewAt(self, ctx, self.cfg.VaultV2, stLedgerV2VaultAbi.PackDemanded(stBig(noId), seq, stBig(rootIndex), stBig(index)), stLedgerV2VaultAbi.UnpackDemanded)
	if err != nil {
		return 0, fmt.Errorf("vault2 demanded(%d, %d, %d, %d): %w", noId, seq, rootIndex, index, err)
	}
	return demand, nil
}

// stLedgerV2StakeMoveGasFloor is the least gas limit the engine signs a v2
// transaction that moves stake with. Runtime 478 admits the staking
// precompile's moveStake with 65,517 gas plus 8,750 for each entry of the
// caller's own StakingHotkeys list, and third parties can lengthen vault2's
// list to 128 entries, one cheap transfer each, between a send's estimate and
// its inclusion (VERSION2-FINAL.md §7.3). A limit sized from eth_estimateGas
// can therefore be left short, and a move refused for gas consumes all the gas
// forwarded to it. Through runtime 477 the admission covered a walk of all 256
// entries a list may hold, whatever it held: R-6 measured a capturing close
// that needed a limit of 2,688,632, which the engine signed 20% above, at
// about 3.2M (§8.1). The floor is that size, which the rehearsal ran on
// runtimes 475, 477 and 478. It covers the walk of a full list, so no list
// leaves a capturing close short; each epoch the close also defers takes about
// 19,500 gas of that margin. The engine caps the floor at the gas the operator
// gas policy still accepts (transactionEnvelope), so a policy that does not
// cover it gets a close signed with what it does cover rather than a refusal.
// The capture's moveStake is the only stake move the server sends through
// vault2: commit, finalize and answer move none.
const stLedgerV2StakeMoveGasFloor = 3_200_000

// sendLedgerV2 sends one v2 operation through the durable engine with the
// root key. A new operation is first preflighted with eth_call at the latest
// block, so a call that would revert never takes a nonce. The engine sizes the
// gas limit by eth_estimateGas, and a close's never below
// stLedgerV2StakeMoveGasFloor.
func (self *CoreStClient) sendLedgerV2(ctx context.Context, operation string, to common.Address, calldata []byte) (string, error) {
	return self.sendPrepared(ctx, self.cfg.RootKey, func(ctx context.Context, client *ethclient.Client) (string, common.Address, []byte, error) {
		logicalKey, err := stTransactionLogicalKey(self.cfg, operation)
		if err != nil {
			return "", common.Address{}, nil, err
		}
		if existing := model.GetStTransactionIntent(ctx, logicalKey); existing == nil || existing.Status == model.StTxReverted {
			err = self.readTransactionRpc(ctx, client, func(ctx context.Context) error {
				var result hexutil.Bytes
				return client.Client().CallContext(ctx, &result, "eth_call", map[string]any{"from": crypto.PubkeyToAddress(self.cfg.RootKey.PublicKey), "to": to, "data": hexutil.Bytes(calldata)}, "latest")
			})
			if err != nil {
				return "", common.Address{}, nil, fmt.Errorf("st ledger v2: %s preflight %s: %w", operation, stLedgerV2RevertName(err), err)
			}
		}
		return operation, to, calldata, nil
	})
}

func (self *CoreStClient) LedgerV2Close(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	return self.sendLedgerV2(ctx, stLedgerV2CloseOperation(epoch, noId), self.cfg.SettlementV2, stLedgerV2SettlementAbi.PackCloseOperatorEpoch(stBig(epoch), stBig(noId)))
}

// An epoch has one commit operation. A retry after a stale build re-drives
// the stored intent rather than sign a second commitment for the epoch, and a
// commit that reverted or was retired is not sent again.
func (self *CoreStClient) LedgerV2Commit(ctx context.Context, epoch uint64, noId uint64, prevRootCount uint64, commitment *StLedgerV2Commitment) (string, error) {
	operation := stLedgerV2CommitOperation(epoch, noId)
	calldata := stLedgerV2SettlementAbi.PackCommitDistribution(stBig(epoch), stBig(noId), stBig(prevRootCount), stLedgerV2CommitmentArgument(commitment))
	logicalKey, err := stTransactionLogicalKey(self.cfg, operation)
	if err != nil {
		return "", err
	}
	if existing := model.GetStTransactionIntent(ctx, logicalKey); existing != nil {
		switch existing.Status {
		case model.StTxReverted, model.StTxCanceled, model.StTxSuperseded, model.StTxFailed, model.StTxInvalid:
			return "", fmt.Errorf("%w: epoch %d intent %s is %s", ErrStLedgerV2CommitSpent, epoch, existing.IntentKey, existing.Status)
		}
		calldata = existing.Calldata
	}
	return self.sendLedgerV2(ctx, operation, self.cfg.SettlementV2, calldata)
}

func stLedgerV2CommitmentArgument(commitment *StLedgerV2Commitment) stabi.ISTSettlementVault2Commitment {
	return stabi.ISTSettlementVault2Commitment{Root: commitment.Root, Total: commitment.Total.ToBig(), LeafCount: commitment.LeafCount, LedgerHash: commitment.LedgerHash}
}

func (self *CoreStClient) LedgerV2Finalize(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	return self.sendLedgerV2(ctx, stLedgerV2FinalizeOperation(epoch, noId), self.cfg.SettlementV2, stLedgerV2SettlementAbi.PackFinalizeDistribution(stBig(noId)))
}

func (self *CoreStClient) LedgerV2Answer(ctx context.Context, noId uint64, seq uint64, answer *StLedgerV2Answer) (string, error) {
	return self.sendLedgerV2(ctx, stLedgerV2AnswerOperation(noId, seq, answer.RootIndex, answer.Index), self.cfg.VaultV2, stLedgerV2PackCheckPath(noId, answer))
}

// stLedgerV2ErrorNames names every custom error of the v2 contracts and of
// LedgerTree by selector.
var stLedgerV2ErrorNames = func() map[[4]byte]string {
	names := map[[4]byte]string{}
	for _, meta := range []*bind.MetaData{&stabi.STSettlement2MetaData, &stabi.STSettlementVault2MetaData} {
		parsed, err := meta.ParseABI()
		if err != nil {
			panic(err)
		}
		for name, abiError := range parsed.Errors {
			var selector [4]byte
			copy(selector[:], abiError.ID[:4])
			names[selector] = name
		}
	}
	for _, name := range []string{"MalformedPath", "InvalidLeafCount"} {
		var selector [4]byte
		copy(selector[:], crypto.Keccak256([]byte(name + "()"))[:4])
		names[selector] = name
	}
	return names
}()

// The revert's error name, "reverts" for an unnamed revert, or "failed".
func stLedgerV2RevertName(err error) string {
	selector, reverted := stLedgerV2RevertSelector(err)
	if !reverted {
		return "failed"
	}
	if name, ok := stLedgerV2ErrorNames[selector]; ok {
		return "reverts " + name
	}
	return "reverts"
}

// stLedgerV2SpentErrors are the v2 reverts that can never clear for an
// intent's exact calldata: its window closed, its epoch was closed or its root
// count moved, or someone else already did it (SPEC A6: a demand answered,
// NoFault; a root finalized, replaced or deleted, NoPendingRoot).
var stLedgerV2SpentErrors = func() map[[4]byte]string {
	spent := map[[4]byte]string{}
	for _, name := range []string{
		"InvalidWindow", "InvalidTransition", "NoPendingRoot", "NoFault", "UnknownRoot", "StaleRootCount",
		"EpochOutOfRange", "TotalMismatch", "TotalNotIncreased", "InvalidLeafCount", "InvalidRoot",
		"IndexOutOfRange", "InvalidProof", "MalformedPath",
	} {
		var selector [4]byte
		copy(selector[:], crypto.Keccak256([]byte(name + "()"))[:4])
		spent[selector] = name
	}
	return spent
}()

// The custom error selector of a reverted call, when the RPC returned one.
func stLedgerV2RevertSelector(err error) ([4]byte, bool) {
	var selector [4]byte
	var dataErr rpc.DataError
	if !errors.As(err, &dataErr) {
		return selector, false
	}
	encoded, ok := dataErr.ErrorData().(string)
	if !ok {
		return selector, false
	}
	data, decodeErr := hexutil.Decode(encoded)
	if decodeErr != nil || len(data) < 4 {
		return selector, false
	}
	copy(selector[:], data[:4])
	return selector, true
}

// spentLedgerV2Intent reports a v2 intent whose call reverts at the finalized
// head with a spent error, so account reconciliation retires its nonce with
// the ordinary cancellation instead of retrying a call that cannot succeed.
// Any other answer leaves the intent to execute.
func (self *CoreStClient) spentLedgerV2Intent(ctx context.Context, client *ethclient.Client, intent *model.StTransactionIntent, from common.Address, finalized *stBlockIdentity) bool {
	to := common.HexToAddress(intent.ToAddress)
	if !self.cfg.LedgerV2Enabled() || intent.DeploymentKey != self.cfg.DeploymentKey() || (to != self.cfg.SettlementV2 && to != self.cfg.VaultV2) {
		return false
	}
	err := self.readTransactionRpc(ctx, client, func(ctx context.Context) error {
		var result hexutil.Bytes
		return client.Client().CallContext(ctx, &result, "eth_call", map[string]any{"from": from, "to": to, "data": hexutil.Bytes(intent.Calldata)}, rpc.BlockNumberOrHashWithHash(common.Hash(finalized.Hash), true))
	})
	selector, reverted := stLedgerV2RevertSelector(err)
	name, spent := stLedgerV2SpentErrors[selector]
	if !reverted || !spent {
		return false
	}
	glog.Infof("[st]cancel spent v2 intent %s nonce %d: %s at finalized block %d\n", intent.IntentKey, intent.Nonce, name, finalized.Number)
	return true
}

func stLedgerV2PackCheckPath(noId uint64, answer *StLedgerV2Answer) []byte {
	account := stabi.LedgerTreeAccount{Index: stBig(answer.Index), Coldkey: answer.Account.Coldkey, Cumulative: answer.Account.Cumulative.ToBig()}
	proof := make([]stabi.LedgerTreeProofNode, len(answer.Proof))
	for i, node := range answer.Proof {
		proof[i] = stabi.LedgerTreeProofNode{Hash: node.Hash, Sum: node.Sum.ToBig()}
	}
	path := make([]stabi.LedgerTreePathNode, len(answer.Path))
	for i, node := range answer.Path {
		path[i] = stabi.LedgerTreePathNode{LeftHash: node.LeftHash, LeftSum: node.LeftSum.ToBig(), RightHash: node.RightHash, RightSum: node.RightSum.ToBig()}
	}
	return stLedgerV2VaultAbi.PackCheckPath(stBig(noId), stBig(answer.RootIndex), account, proof, path)
}

// stLedgerV2EventDecoders returns the decoders for a log of a v2 contract,
// and false for any other address. Numbers are decimal strings, bytes32 values
// and addresses 0x-hex, as for v1.
func (self *CoreStClient) stLedgerV2EventDecoders(address common.Address) ([]stEventDecoder, bool) {
	if self.cfg == nil || !self.cfg.LedgerV2Enabled() {
		return nil, false
	}
	v, s := stLedgerV2VaultAbi, stLedgerV2SettlementAbi
	switch address {
	case self.cfg.VaultV2:
		return []stEventDecoder{
			stEvent("V2CoordinatorFixed", v.UnpackCoordinatorFixedEvent, func(e *stabi.STSettlementVault2CoordinatorFixed) map[string]any {
				return map[string]any{"coordinator": e.Coordinator.Hex()}
			}),
			stEvent("V2EscrowRegistered", v.UnpackEscrowRegisteredEvent, func(e *stabi.STSettlementVault2EscrowRegistered) map[string]any {
				return map[string]any{"hotkey": fmt.Sprintf("0x%x", e.Hotkey), "uid": fmt.Sprintf("%d", e.Uid)}
			}),
			stEvent("V2PoolRegistered", v.UnpackPoolRegisteredEvent, func(e *stabi.STSettlementVault2PoolRegistered) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "hotkey": fmt.Sprintf("0x%x", e.Hotkey), "uid": fmt.Sprintf("%d", e.Uid), "first_epoch": fmt.Sprintf("%d", e.FirstEpoch)}
			}),
			stEvent("V2PoolUidRegistered", v.UnpackPoolUidRegisteredEvent, func(e *stabi.STSettlementVault2PoolUidRegistered) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "hotkey": fmt.Sprintf("0x%x", e.Hotkey), "uid": fmt.Sprintf("%d", e.Uid), "payer": e.Payer.Hex()}
			}),
			stEvent("V2EmissionCaptured", v.UnpackEmissionCapturedEvent, func(e *stabi.STSettlementVault2EmissionCaptured) map[string]any {
				return map[string]any{"e": e.Epoch.String(), "no_id": e.NoId.String(), "observed": e.Observed.String(), "amount": e.Amount.String(), "funded_through": e.FundedThrough.String()}
			}),
			stEvent("V2EmissionDeferred", v.UnpackEmissionDeferredEvent, func(e *stabi.STSettlementVault2EmissionDeferred) map[string]any {
				return map[string]any{"e": e.Epoch.String(), "no_id": e.NoId.String(), "funded_through": e.FundedThrough.String()}
			}),
			stEvent("V2DistributionCommitted", v.UnpackDistributionCommittedEvent, func(e *stabi.STSettlementVault2DistributionCommitted) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "seq": fmt.Sprintf("%d", e.Seq), "e": fmt.Sprintf("%d", e.Epoch), "root": fmt.Sprintf("0x%x", e.Root),
					"total": e.Total.String(), "leaf_count": fmt.Sprintf("%d", e.LeafCount), "ledger_hash": fmt.Sprintf("0x%x", e.LedgerHash), "replaced_seq": fmt.Sprintf("%d", e.ReplacedSeq)}
			}),
			stEvent("V2PathDemanded", v.UnpackPathDemandedEvent, func(e *stabi.STSettlementVault2PathDemanded) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "seq": fmt.Sprintf("%d", e.Seq), "root_index": e.RootIndex.String(), "index": e.Index.String(), "demander": e.Demander.Hex()}
			}),
			stEvent("V2PathAnswered", v.UnpackPathAnsweredEvent, func(e *stabi.STSettlementVault2PathAnswered) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "seq": fmt.Sprintf("%d", e.Seq), "root_index": e.RootIndex.String(), "index": e.Index.String(), "answerer": e.Answerer.Hex()}
			}),
			stEvent("V2DistributionDeleted", v.UnpackDistributionDeletedEvent, func(e *stabi.STSettlementVault2DistributionDeleted) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "seq": fmt.Sprintf("%d", e.Seq), "root_index": e.RootIndex.String(), "index": e.Index.String(), "fault": fmt.Sprintf("%d", e.Fault), "level": e.Level.String(), "challenger": e.Challenger.Hex()}
			}),
			stEvent("V2DistributionFinalized", v.UnpackDistributionFinalizedEvent, func(e *stabi.STSettlementVault2DistributionFinalized) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "seq": fmt.Sprintf("%d", e.Seq), "root_index": e.RootIndex.String(), "e": fmt.Sprintf("%d", e.Epoch), "root": fmt.Sprintf("0x%x", e.Root),
					"total": e.Total.String(), "leaf_count": fmt.Sprintf("%d", e.LeafCount), "ledger_hash": fmt.Sprintf("0x%x", e.LedgerHash)}
			}),
			stEvent("V2Claimed", v.UnpackClaimedEvent, func(e *stabi.STSettlementVault2Claimed) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "root_index": e.RootIndex.String(), "index": e.Index.String(), "coldkey": fmt.Sprintf("0x%x", e.Coldkey),
					"cumulative": e.Cumulative.String(), "amount": e.Amount.String(), "caller": e.Caller.Hex()}
			}),
			stEvent("V2StakeAbsorbed", v.UnpackStakeAbsorbedEvent, func(e *stabi.STSettlementVault2StakeAbsorbed) map[string]any {
				return map[string]any{"hotkey": fmt.Sprintf("0x%x", e.Hotkey), "observed": e.Observed.String(), "amount": e.Amount.String(), "caller": e.Caller.Hex()}
			}),
			stEvent("V2SurplusAssigned", v.UnpackSurplusAssignedEvent, func(e *stabi.STSettlementVault2SurplusAssigned) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "e": e.Epoch.String(), "amount": e.Amount.String()}
			}),
		}, true
	case self.cfg.SettlementV2:
		return []stEventDecoder{
			stEvent("V2AuditorsSet", s.UnpackAuditorsSetEvent, func(e *stabi.STSettlement2AuditorsSet) map[string]any {
				auditors := make([]string, len(e.Auditors))
				for i, auditor := range e.Auditors {
					auditors[i] = auditor.Hex()
				}
				return map[string]any{"version": fmt.Sprintf("%d", e.Version), "auditors": auditors, "quorum": e.Quorum.String()}
			}),
			stEvent("V2DistributionApproved", s.UnpackDistributionApprovedEvent, func(e *stabi.STSettlement2DistributionApproved) map[string]any {
				return map[string]any{"no_id": e.NoId.String(), "seq": fmt.Sprintf("%d", e.Seq), "auditor": e.Auditor.Hex(), "auditor_set_version": fmt.Sprintf("%d", e.AuditorSetVersion), "approvals": e.Approvals.String()}
			}),
		}, true
	}
	return nil, false
}
